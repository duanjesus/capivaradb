# The query planner

A SQL query says what rows are wanted, not how to get them. The planner
decides how: where each condition is checked, how each table is read, and
in what order tables are joined. Getting these right is the difference
between a query that takes microseconds and one that never finishes.

The code is `internal/engine/plan.go` and `stats.go`.

## What it decides

### 1. Where each condition is checked

The `WHERE` clause is split at its `AND`s, and each piece is applied at the
earliest point where the tables it mentions are available.

```sql
select * from product p, customer c
where p.price > 100 and c.city = 'city3' and p.id = c.id
```

```
Nested Loop
  Join Filter: (p.id = c.id)
  ->  Seq Scan on product p
        Filter: (p.price > 100)
  ->  Seq Scan on customer c
        Filter: (c.city = 'city3')
```

The two conditions that mention one table filter that table's scan; the
one that mentions both is checked in the join. Before the planner all
three were checked after the join, on every pair of rows. (The plan shown
is with `enable_indexscan`, `enable_hashjoin` and `enable_mergejoin` off,
to show where conditions go and nothing else.)

`INNER JOIN ... ON`, `CROSS JOIN` and a comma are the same thing to the
planner: a set of tables and a pool of conditions.

**Outer joins restrict what may move.** In a left join, a condition in
`WHERE` on the left side is pushed into the left side. A condition in `ON`
on the right side is pushed into the right side. The other two combinations
must stay where they are: a `WHERE` condition on the right side has to see
the NULLs the join produces (`WHERE o.id IS NULL` is how you ask for
customers *without* orders), and an `ON` condition on the left side does
not remove left rows, it only stops them matching. A right join is the
mirror image, and a full join keeps the rows of both sides, so nothing can
be pushed into either.

**Conditions with subqueries are applied last.** A correlated subquery can
refer to any table of the query without that being visible in the condition
around it, so it is only evaluated once every table is joined.

### 2. How each table is read

A table can be read in full (`Seq Scan`) or through its primary key or an
index (`Index Scan`). An index is usable for equalities on its leading
columns, followed by at most one range:

```sql
explain select * from orders where product_id = 3 and qty between 2 and 4;
--  Index Scan using orders_product_qty on orders
--    Index Cond: (product_id = 3) AND (qty between 2 and 4)
```

The value compared with may be a literal, a parameter, a column of an outer
query, or — in a join — a column of a table joined earlier.

Using an index is not always better. Each row found through a secondary
index costs a lookup in the table, so an index that would return most of
the table loses to a scan. The planner estimates both and takes the
cheaper.

`UPDATE` and `DELETE` find their rows the same way, so changing one row by
its key does not read the table.

**Order counts too.** A scan returns rows in the order of the key it reads
through, and if that is the order the query's `ORDER BY` asks for, the
sort can be skipped — and with a `LIMIT`, most of the reading. So when a
query has an `ORDER BY` on plain columns, each way of reading the table is
costed with what would have to follow it: a sort, if its order is wrong;
only the first rows, if its order is right and there is a `LIMIT`. An
index nobody would use to filter can win on those terms:

```sql
explain select * from orders order by customer_id limit 5;
--  Limit
--    ->  Index Scan using orders_customer on orders
```

[executor.md](executor.md#sorting-and-not-sorting) has the details,
including why a column that may be NULL does not qualify.

With MVCC an index entry points at a row *version*; whether a reader may
see it is decided in the table, as for a scan.

### 3. In what order tables are joined, and how

For each join the planner chooses between four methods
([executor.md](executor.md) has how each works):

- **Nested loop with an index lookup**: for each outer row, look the
  matching rows up through an index of the inner table. Proportional to
  the outer side; unbeatable when that is small.
- **Hash join**: hash the inner side on the join key, look each outer row
  up in the table. One pass over each side.
- **Merge join**: walk two inputs sorted on the join key in step. The
  cheapest of all when both are already in order, which a table scan is,
  by primary key; otherwise the inputs must be sorted first.
- **Nested loop**: compare every pair. What is left when the join has no
  equality.

and for the whole query, an order. Orders are *left-deep*: a chain in which
one table at a time is added to what has been joined so far.

- Up to ten tables, the cheapest order is found exactly, by dynamic
  programming over sets of tables: the best way to join a set is the best
  way to join all but one of its members, extended by that one. Every
  order is considered while only 2ⁿ sub-plans are computed.
- Beyond that, greedily: start from the smallest table and keep adding
  whichever is cheapest to add.

```sql
select * from orders o, customer c, product p
where o.customer_id = c.id and o.product_id = p.id and c.name = 'c7'
```

```
Merge Join
  Merge Cond: (o.product_id = p.id)
  ->  Sort
        Sort Key: o.product_id
        ->  Nested Loop
              ->  Index Scan using customer_name on customer c
                    Index Cond: (c.name = 'c7')
              ->  Index Scan using orders_customer on orders o
                    Index Cond: (o.customer_id = c.id)
  ->  Seq Scan on product p
```

Written largest table first; executed starting from the single customer the
query asks for, whose ten orders are reached through an index. The last
step is less obvious and shows the cost model at work: the products are
twenty rows, stored in key order, and reading them all once to merge with
ten sorted orders is estimated cheaper than ten separate lookups by key.
With a larger product table the lookups win.

## Estimates

Choices are made by estimated cost, in units where reading one row of a
scan costs 1.

| | Cost |
|---|---:|
| A row read by a sequential scan | 1 |
| A row read through the primary key | 1.2 |
| A row read through a secondary index | 4 |
| Finding the starting point in a B+tree | 3 |
| Comparing one pair of rows in a nested loop | 0.25 |
| Putting a row in a hash table | 1.5 |
| Looking a row up in a hash table | 0.5 |
| Advancing one row in a merge | 0.3 |
| One comparison of a sort | 0.2 |
| Producing a row, for whatever comes next | 1 |

The last one matters more than it looks. Without it, an order that builds a
cross product and filters it later costs the same as one that follows the
join conditions, and a greedy search happily picks the cross product. With
that mistake `select5.test`, whose queries join up to fifteen tables, still
timed out; without it, it passes in full.

Row counts come from **statistics**: per table, the number of rows; per
column, the number of distinct values, the fraction of NULLs, and for
numeric columns the minimum and maximum.

- `ANALYZE` (or `ANALYZE table`) gathers them.
- A table that has changed by a fifth, and by at least 50 rows, is analysed
  automatically after a commit.
- They are stored in the catalog and survive a restart.

From them, selectivities: `col = value` keeps 1/distinct of the rows; a
range keeps the fraction of the column's span it covers, assuming values
spread evenly; a join on equality keeps 1/max(distinct) of the pairs; and
where nothing is known, PostgreSQL's defaults (a third, a tenth for
`LIKE`).

These estimates are crude. The planner assumes columns are independent and
values evenly spread, and both are often false:

```
->  Seq Scan on customer c  (cost=128.00 rows=43) (actual rows=2 loops=1)
      Filter: (c.city = 'Recife')
```

Three distinct cities among 128 customers, so it guesses a third; in fact
almost everyone is in one city. The estimates only have to rank plans, and
`EXPLAIN ANALYZE` exists to show when they did not.

## EXPLAIN

```sql
explain select ...;                              -- the plan and its estimates
explain analyze select ...;                      -- run it; add what happened
explain (analyze, costs off, timing off) ...;    -- reproducible output
```

`EXPLAIN ANALYZE` really executes the statement, including an `UPDATE` or
`DELETE`. Each node reports the rows it produced and how many times it ran.

Switches turn the planner's choices off, to see the plan it rejected:

```sql
set enable_indexscan = off;     -- no index scans, no index lookups in joins
set enable_hashjoin = off;
set enable_mergejoin = off;
set enable_nestloop = off;      -- plain nested loops only where nothing else can do
set join_collapse_limit = 1;    -- join in the order written
```

With the first three off and the last at 1, a query runs exactly as it is
written: the reference every other plan is compared with.

They are named after PostgreSQL's and exist for comparing plans, in tests
and benchmarks.

## What it changed

Two sqllogictest scripts join up to fifteen tables at once. They are the
before-and-after of this milestone:

| Script | Before | After |
|---|---|---|
| `select4.test` | 39.0% in 441 s; 1351 queries timed out | 74.1% in 2 s; none time out |
| `select5.test` | 51.5% in 217 s; 697 queries timed out | **100%** in 1 s |

(What `select4.test` still failed used `UNION`, `EXCEPT` or `INTERSECT`;
with those, in the next milestone, it reached 100%.)

And on a table of 20 000 orders, the chosen plan against the plan without
indexes ([benchmarks.md](benchmarks.md)):

| Query | Chosen plan | Without indexes | |
|---|---:|---:|---:|
| One order by primary key | 6 µs | 4.4 ms | 770× |
| A customer's orders, by secondary index | 28 µs | 5.2 ms | 190× |
| Update one order by primary key | 32 µs | 3.8 ms | 120× |
| Three-table join for one customer | 129 µs | 5.8 ms | 45× |

## How it is tested

A planner has a property that makes it unusually testable: **whatever it
decides, the answer must not change.**

- **`TestPlansAgree`** generates 400 random queries — joins written in
  different orders and styles, left, right and full joins, a subquery in
  `FROM`, random conditions including `OR`, `IN`, `BETWEEN` and correlated
  subqueries — and runs each under ten combinations of the switches,
  ending with the one where nothing is left to choose. The rows must be
  identical. A wrong pushdown, a join condition applied at the
  wrong step, a range scan that misses a boundary: each shows up as a
  difference.
- **sqllogictest** is the outside oracle: 109 414 records whose expected
  results were not produced by this planner. CI fails on any regression.
- **Plan tests** assert the exact `EXPLAIN` output for representative
  queries, so that a change of plan is a visible change in a test.
- **Index scans against scans**: every range shape (open, closed,
  half-open, empty, NULL, beyond either end) must return what a sequential
  scan returns.
- **Isolation through indexes**: a transcript checks that index scans obey
  snapshots, after first checking that its queries really are index scans.
- **Mutation testing**: six ways of making the planner wrong, each caught
  — pushing a `WHERE` condition below a left join's nullable side, dropping
  unmatched rows of a left join, evaluating a subquery condition too
  early, ignoring visibility in an index scan, mishandling an inclusive
  bound, and skipping a join condition after an index lookup.

One limit of the first test is worth stating: it compares the planner with
itself. A mistake made the same way under every setting — pushing a
condition somewhere it must not go, for instance — is invisible to it,
which is why the mutation run includes tests with known answers and why
sqllogictest matters.

## Limitations

- **Left-deep plans only**, and no reordering across an outer join.
- **Order is used where it is found, not sought across joins.** A scan's
  order lets a merge join or an `ORDER BY` skip a sort, and for a single
  table the planner will pick a path for its order. For a join it will
  not: the join order is chosen without regard to the `ORDER BY`. Only
  ascending order is used.
- **Indexes are not used** for `IN` lists, for `OR`, or to answer a query
  from the index alone.
- **Statistics are simple**: no histograms, no most-common values, no
  correlation between columns.
- **Subqueries are not flattened**: a correlated subquery runs once per
  outer row. It benefits from indexes, since each run is planned like any
  query, but it is never turned into a join.
- **Plans are made when a statement is prepared** and not revisited if the
  data changes afterwards.
- **The planner does not know `work_mem`.** A hash join that will spill is
  costed like one that will not.

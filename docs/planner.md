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
one that mentions both is checked in the join. Before this milestone all
three were checked after the join, on every pair of rows.

`INNER JOIN ... ON`, `CROSS JOIN` and a comma are the same thing to the
planner: a set of tables and a pool of conditions.

**Left joins restrict what may move.** A condition in `WHERE` on the left
side is pushed into the left side. A condition in `ON` on the right side is
pushed into the right side. The other two combinations must stay where they
are: a `WHERE` condition on the right side has to see the NULLs the join
produces (`WHERE o.id IS NULL` is how you ask for customers *without*
orders), and an `ON` condition on the left side does not remove left rows,
it only stops them matching.

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

With MVCC an index entry points at a row *version*; whether a reader may
see it is decided in the table, as for a scan.

### 3. In what order tables are joined, and how

For each join the planner chooses between two methods:

- **Nested loop**: read the inner table once, compare every pair.
- **Nested loop with an index lookup**: for each outer row, look the
  matching rows up through an index of the inner table. This is what makes
  a join proportional to its result rather than to the product of its
  inputs.

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
Nested Loop
  ->  Nested Loop
        ->  Index Scan using customer_name on customer c
              Index Cond: (c.name = 'c7')
        ->  Index Scan using orders_customer on orders o
              Index Cond: (o.customer_id = c.id)
  ->  Index Scan using product_pkey on product p
        Index Cond: (o.product_id = p.id)
```

Written largest table first; executed starting from the single customer the
query asks for.

## Estimates

Choices are made by estimated cost, in units where reading one row of a
scan costs 1.

| | Cost |
|---|---:|
| A row read by a sequential scan | 1 |
| A row read through the primary key | 1.2 |
| A row read through a secondary index | 4 |
| Finding the starting point in a B+tree | 3 |
| Comparing one pair of rows in a join | 0.25 |
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

Two switches turn the planner's choices off, to see the plan it rejected:

```sql
set enable_indexscan = off;     -- sequential scans and plain nested loops only
set join_collapse_limit = 1;    -- join in the order written
```

They are named after PostgreSQL's and exist for comparing plans, in tests
and benchmarks.

## What it changed

Two sqllogictest scripts join up to fifteen tables at once. They are the
before-and-after of this milestone:

| Script | Before | After |
|---|---|---|
| `select4.test` | 39.0% in 441 s; 1351 queries timed out | 74.1% in 2 s; none time out |
| `select5.test` | 51.5% in 217 s; 697 queries timed out | **100%** in 1 s |

(Everything `select4.test` still fails uses `UNION`, `EXCEPT` or
`INTERSECT`.)

And on a table of 20 000 orders, the chosen plan against the rejected one
([benchmarks.md](benchmarks.md)):

| Query | Chosen plan | Rejected plan | |
|---|---:|---:|---:|
| One order by primary key | 6 µs | 4.8 ms | 790× |
| A customer's orders, by secondary index | 29 µs | 4.1 ms | 140× |
| Update one order by primary key | 31 µs | 4.0 ms | 130× |
| Three-table join for one customer | 144 µs | 5.4 ms | 38× |

## How it is tested

A planner has a property that makes it unusually testable: **whatever it
decides, the answer must not change.**

- **`TestPlansAgree`** generates 400 random queries — joins written in
  different orders and styles, left joins, a subquery in `FROM`, random
  conditions including `OR`, `IN`, `BETWEEN` and correlated subqueries —
  and runs each under all four combinations of the two switches. The rows
  must be identical. A wrong pushdown, a join condition applied at the
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
itself. A mistake made the same way under all four settings — pushing a
condition somewhere it must not go, for instance — is invisible to it,
which is why the mutation run includes tests with known answers and why
sqllogictest matters.

## Limitations

- **Only nested-loop joins.** With an index on the inner side they are
  fast; without one a join of two large tables compares every pair. Hash
  and merge joins belong to the executor milestone.
- **Left-deep plans only**, and no reordering across `LEFT JOIN`.
- **Indexes are not used for `ORDER BY`**, for `IN` lists, for `OR`, or to
  answer a query from the index alone.
- **Statistics are simple**: no histograms, no most-common values, no
  correlation between columns.
- **Subqueries are not flattened**: a correlated subquery runs once per
  outer row. It benefits from indexes, since each run is planned like any
  query, but it is never turned into a join.
- **Plans are made when a statement is prepared** and not revisited if the
  data changes afterwards.
- **Every step still materialises its result.** The planner chooses a good
  plan; the executor that runs it without holding everything in memory is
  the last milestone.

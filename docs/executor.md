# The executor

The planner decides what to do; the executor does it. Until this milestone
it did it the simplest way that is correct: every step of a plan computed
its whole result before the next one started. A join of two tables held
both in memory, then their join, then the sorted join, and only then sent
the first row to the client.

Now a plan is a tree of **iterators**. Each has one operation that matters,
"give me your next row", and gets its own input by asking the same of the
step below. This is the Volcano model, and it is what almost every
relational database does.

The code is `internal/engine/iter.go` (the machinery, temporary files,
sorting), `join.go` (the four joins) and `select.go` (grouping, the select
list, `DISTINCT`, limits, set operations).

## What it buys

**1. Memory is bounded by `work_mem`, not by the data.**

A scan holds one batch of rows. A sort or a hash join that has to remember
more than `work_mem` moves the excess to temporary files. Measured on a
table of 40 000 rows, as live heap while the rows are being read
(`TestMemoryIsBoundedByWorkMem`):

| Query | `work_mem = 1GB` | `work_mem = 256kB` |
|---|---:|---:|
| Scan | 64 kB | 64 kB |
| Sort (`ORDER BY`) | 6.4 MB | 0.9 MB |
| Hash join of the table with itself | 13.6 MB | 2.1 MB |

The scan uses 64 kB whatever the table's size. The other two, with room,
hold everything; without it they hold a few times `work_mem` — the
allowance itself plus the read and write buffers of the files in use —
and that does not grow with the table.

**2. A query that needs few rows reads few rows.**

```
explain (analyze, costs off, timing off) select id from orders limit 3;

 Limit (actual rows=3 loops=1)
   ->  Seq Scan on orders (actual rows=3 loops=1)
```

Three rows left the scan, of 6 144 in the table. `LIMIT` stops whatever is
below it; `EXISTS` is settled by the first row; the second half of a
`UNION ALL` is not started if the first half was enough. On 20 000 rows,
`LIMIT 10` takes 12 µs where reading everything takes 5 ms.

**3. Rows reach the client while the query is running**, and a client that
fetches a few at a time holds a cursor, not a result. With JDBC,
`setFetchSize(50)` makes the driver ask for fifty rows per round trip, and
the server keeps the query suspended in between.

## Cursors and snapshots

A query left open has to keep answering from the state it started in,
however long it is left and whatever happens meanwhile. Two things make
that work.

The query **holds its snapshot** until its cursor is closed, which keeps
vacuum away from the row versions it can still see.

And a scan **holds nothing else**. Between batches it keeps no lock and no
position inside a B+tree that other transactions are changing — only the
last key it read, from which it finds its place again. That is sound
because of MVCC: the versions a snapshot sees are not removed while it is
held, and versions added later are invisible to it, so every batch reads
from the same frozen state.

The JDBC smoke test does it for real: it opens a cursor over 640 rows,
reads 120, then from another connection deletes every row and runs
`VACUUM`. The cursor returns the other 520.

Batches start at 16 rows and double up to 256, so a query that wants ten
rows does not decode hundreds, and one that wants them all does not take
the lock for each handful.

## The joins

Four ways of joining, chosen by the planner by cost
([planner.md](planner.md)):

| Method | When it is chosen | What it costs |
|---|---|---|
| **Index nested loop** | The inner table has an index on the join column and few outer rows need looking up | One index lookup per outer row |
| **Hash join** | The join has an equality and no useful index | One pass over each side; a hash table of the inner side |
| **Merge join** | Both inputs are already sorted on the join key | One pass over each side; nothing else |
| **Nested loop** | The join has no equality (`a.x < b.y`), or one side is tiny | The product of the two sides |

On 20 000 orders joined to 1 000 customers with no index
([benchmarks.md](benchmarks.md)): hash join 8 ms, merge join 15 ms (it has
to sort the orders first), nested loop 494 ms.

**Hash join.** The inner side is read into a hash table keyed on the join
columns; each outer row is looked up in it. If the inner side does not fit
in `work_mem`, both sides are first split into 16 partitions on disk by a
hash of the key — rows that can match are then in partitions of the same
number — and the partitions are joined one pair at a time: a *Grace* hash
join. A partition that is still too large is split again with a different
hash, up to three times.

```
 Hash Join (actual rows=5120 loops=1)
   Hash Cond: (c.id = o.customer_id)
   ->  Seq Scan on orders o (actual rows=6144 loops=1)
   ->  Hash (actual rows=3075 loops=1)
         Batches: 16  Memory Usage: 42kB
         ->  Seq Scan on customer c (actual rows=3075 loops=1)
```

**Merge join.** Two inputs sorted on the join key are walked in step, like
merging two sorted lists. A table is stored in primary key order, so a scan
of it is sorted for free — the planner knows this — and two tables joined
on their primary keys merge without a hash table or a sort:

```
 Merge Join
   Merge Cond: (c.id = o.id)
   ->  Seq Scan on orders o
   ->  Seq Scan on customer c
```

An input that is not in order is sorted first, by the external sort below.
That usually makes a hash join cheaper, which is why merge joins with a
`Sort` under them are rare in the plans.

**Outer joins.** `LEFT`, `RIGHT` and `FULL JOIN` run as nested loops or
hash joins that also return the rows that matched nothing. A `RIGHT JOIN`
is a left join read from the other side. A `FULL JOIN` remembers which
inner rows found a partner and returns the others at the end.

**What "equal" means** has to be the same for every method, and each has
its own way of getting it wrong. A hash join compares keys by an encoding
of their values, so an integer and the double precision of the same value
must encode alike (`1 = 1.0`), `-0.0` like `0.0`, and a NULL must never be
looked up at all. `TestJoinKeysByEveryMethod` runs the same joins through
each method and expects the same answer.

## Sorting

`ORDER BY` uses one of three methods, shown by `EXPLAIN ANALYZE`:

- **`quicksort`**: the rows fit in `work_mem` and are sorted in memory.
- **`external merge`**: they do not. Each memory-full is sorted and written
  out as a *run*; at the end the runs are merged through a heap. The memory
  needed is one run, however much is sorted.
- **`top-N heapsort`**: there is a `LIMIT`. Only the first *N* rows are
  wanted, so a heap of *N* rows is kept and every other row is compared
  with the worst of them and dropped.

```
 Sort (actual rows=5120 loops=1)
   Sort Key: o.total DESC, c.name
   Sort Method: external merge  Runs: 6
```

All three are **stable** and give the same order row for row, which is
tested: among rows with equal keys, the order of arrival is kept. For the
external sort that takes a rule in the merge — between equal rows the one
from the earlier run wins — and for the heap, an arrival number as the
last sort key.

## Set operations

`UNION`, `INTERSECT` and `EXCEPT`, each with `ALL`, with the standard's
precedence (`INTERSECT` binds tighter) and with `ORDER BY` / `LIMIT`
applying to the whole.

| | Returns a row… |
|---|---|
| `UNION ALL` | every time it occurs on either side |
| `UNION` | once if it occurs on either side |
| `INTERSECT` | once if it occurs on both sides |
| `INTERSECT ALL` | as many times as it occurs on *both* sides |
| `EXCEPT` | once if it occurs on the left and not on the right |
| `EXCEPT ALL` | as many times as it occurs more on the left than on the right |

Rows are compared as `DISTINCT` compares them: two NULLs are the same row.
Columns are matched by position; the result takes its names from the left
side and, for each column, a type wide enough for both (`integer` and
`double precision` give `double precision`).

`UNION ALL` is an `Append`: the rows of one input, then the rows of the
other. `UNION` is an `Append` with duplicates removed. `INTERSECT` and
`EXCEPT` count the rows of the right input in a hash table and then go
through the left input once.

They closed the last gap in the test corpus: the 1 000 records of
`select4.test` that failed all used them, and it now passes in full.

## Settings

| Setting | Default | |
|---|---|---|
| `work_mem` | `4MB` | Memory one sort or hash table may use before going to disk. Minimum `64kB`. |
| `enable_hashjoin` | `on` | |
| `enable_mergejoin` | `on` | |
| `enable_nestloop` | `on` | Off makes a plain nested loop the last resort instead of forbidding it: a join with no equality has no other method. |

As with the planner's switches, the last three exist to compare plans, in
tests and benchmarks.

## How it is tested

- **Known answers** for every set operation and every kind of join,
  including the cases that are easy to get wrong: NULL keys, duplicate
  keys on both sides, an empty side, conditions in `ON` against the same
  conditions in `WHERE`.
- **Every method, same rows.** The 400 random queries of the planner's
  test now run under ten settings — each join method alone, joins as
  written, with and without indexes, with a `work_mem` small enough to
  send sorts and hash tables to disk — and all ten must agree.
- **On disk against in memory.** The same sorts and joins with
  `work_mem = 64kB` and `64MB` must return the same rows, and the test
  checks that the small one really used temporary files.
- **Cursors against a moving table**: a cursor reads a few rows, another
  session deletes, updates, inserts and vacuums under it, and the cursor
  must still return exactly the rows that existed when it was opened, in
  order, once each — through a table scan and through an index.
- **Nothing left behind.** A cursor abandoned half-way must release its
  snapshot and its files. After every test in the engine, the count of
  temporary files still open must be zero.
- **Memory**, measured, as in the table at the top.
- **The pieces alone**: the external sort against the standard library's
  stable sort; the encoding of rows in temporary files, including that a
  truncated file is an error and not a short result.
- **sqllogictest**: 109 414 records, 99.99%.
- **Mutation testing**: seventeen ways of making the executor almost
  right — a scan that returns again the row it stopped at, a cursor that
  reads the session's newer snapshot, a hash join that takes two NULLs for
  equal, an external sort that is not stable, a merge join that matches
  only the first of several equal rows, `UNION` keeping duplicates — each
  caught.

That last run also found a flaw in itself. A mutant that does not compile
fails every test without any test having looked at it, and one of the
planner mutants of the previous milestone had been "killed" exactly that
way. The script now builds each mutant before testing it and treats a
build failure as its own error; the mutant was rewritten and is killed by
tests.

## Limitations

- **Grouping and duplicate removal are in memory.** `GROUP BY`, `DISTINCT`,
  `UNION`, `INTERSECT` and `EXCEPT` keep a hash table of the distinct
  groups or rows, which does not spill. A query with millions of *groups*
  needs memory for them; one with millions of *rows* in few groups does
  not.
- **A merge join keeps the rows of one key in memory**, and a hash join
  whose build side is mostly one key cannot be partitioned: after three
  attempts it joins that partition in memory.
- **`ORDER BY` always sorts.** A scan in primary key order is known to be
  sorted and a merge join uses that, but `ORDER BY id LIMIT 10` still
  reads the table and keeps a heap of ten rows, where it could read ten.
- **One row at a time.** There is no vectorised execution and no parallel
  query; every row passes through an interface call per plan step.
- **Subquery plans are not shown by `EXPLAIN`**, and a correlated subquery
  is still run once per outer row (it stops early where it can, as
  `EXISTS` does, and uses indexes).
- **`FULL JOIN ... USING` and `NATURAL JOIN` are not supported.**
- **`UPDATE` and `DELETE` collect the rows they will change before
  changing any.** They must: a row that an update moves would otherwise be
  met again. It means a statement that changes a million rows holds a
  million rows.
- **Temporary files go to the system's temporary directory**, are not
  limited in size, and are removed when the query ends; a server that is
  killed leaves them for the operating system to clean up.

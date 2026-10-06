# 9. Executor: row-at-a-time iterators, scans that resume by key, spilling by work_mem

Status: accepted. Replaces the executor of decision 4, whose semantics it
keeps.

## Context

Since milestone 2 a query had been executed by materialising every step:
correct, simple, and the reference everything else was tested against
(decision 4). It had three costs that the planner of milestone 6 could not
remove: memory proportional to the data, no way to stop early, and no way
to join but nested loops. A join that would hold more than two million
intermediate rows was refused outright.

## Decisions

**Volcano iterators, one row at a time.** Each plan step is an object with
`next()` and `close()`.

*Alternative considered:* vectorised execution, passing batches of a
thousand rows in columns. It is several times faster on analytical queries
and is where the field has gone. It is also a different way of writing
every operator and every expression, and the expression evaluator here
works on rows. The roadmap had it as "if time allows"; what was needed was
bounded memory and early termination, which rows give, not throughput.

**A scan keeps a key between batches, not a lock or a cursor.** A table is
read a batch at a time under the database's read lock; between batches the
scan holds only the last key it saw and seeks past it.

*Why not hold the lock for the whole scan,* as before: the scan now lasts
as long as the client takes to read the result, and a writer would wait
for a client.

*Why not keep a position in the tree:* the tree changes between batches.
Pages split and are freed; a saved position would need to be validated or
pinned, which is what latch coupling and page LSN checks are for in
databases that do this. Seeking again by key costs a tree descent per 256
rows and needs none of it.

*Why it is correct:* MVCC. A snapshot's versions are not vacuumed while
the snapshot is held, and versions written later are not visible to it.
So the set of visible keys is fixed for the life of the query, and "the
next visible key after K" has one answer no matter when it is asked. The
query therefore has to hold its snapshot until its cursor is closed, and
does.

**The snapshot is captured by the execution, not read from the session.**
A suspended query and the statements the session runs meanwhile each need
their own. This was a real bug waiting: every scan used to read "the
session's current snapshot" at the moment it ran.

**Memory is limited per operation by `work_mem`, by spilling to temporary
files.** One mechanism — rows encoded to a file, read back in order — and
three users: the external sort's runs, the hash join's partitions, the
nested loop's inner side.

*Not spilled:* hash aggregation and duplicate removal. Their memory is
proportional to the number of groups, not rows, which covers the common
case; spilling them needs the same partitioning as the hash join and is
the first thing to add.

**Hash join with Grace partitioning; no hybrid.** When the build side
exceeds `work_mem`, everything goes to 16 partitions on disk, and each is
joined in turn; a partition still too large is split again, at most three
times. A hybrid hash join would keep the first partition in memory and
save a pass over it. The gain is real and bounded; the code is not small.

**Merge join only when it is free or forced.** It is implemented in full,
with an external sort for inputs that are not in order, and the planner
costs it honestly — which means it is chosen when both inputs are already
sorted (tables joined on their clustered keys) or when a small sort beats
building a hash table, and otherwise loses to the hash join. The planner
tracks one "interesting order" per plan node, the leading primary key
column, and nothing more elaborate.

**Stable sorts everywhere.** In-memory, external and top-N sorts return
equal rows in arrival order. SQL does not require it, but the three would
otherwise give visibly different results for the same query depending on
`work_mem` and on whether there is a `LIMIT`, and tests that compare them
would have to ignore order.

**`UPDATE` and `DELETE` still collect their rows first.** Streaming them
would let an `UPDATE` meet a row it has just moved (the Halloween
problem). The standard cure is exactly this: read everything, then write.

**Set operations as fields of `Select` rather than a new node type.** A
`Select` with `Op` set is a set operation whose own `ORDER BY` and `LIMIT`
apply to the whole. Every place that holds a `*Select` — subqueries,
`IN`, `EXISTS`, derived tables, `INSERT ... SELECT` — gets set operations
without changing.

## How it is kept honest

The old executor is gone, so it can no longer be the oracle. In its place:
the executor with every choice removed (`enable_hashjoin`,
`enable_mergejoin` and `enable_indexscan` off, joins as written) *is* the
old executor's algorithm, and every other configuration must return the
same rows as it does; sqllogictest supplies answers from outside; and each
operator has tests with answers worked out by hand. See
[executor.md](../executor.md).

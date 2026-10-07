# 10. Order as a hint from the query; one overflow scheme for every hash table

Status: accepted

## Context

Milestone 7 left two limitations that anyone trying the database would
meet in the first minutes. `ORDER BY id LIMIT 10` read the whole table,
although the table is stored in `id` order. And `GROUP BY`, `DISTINCT` and
the set operations kept their hash tables in memory, so the claim that a
query's memory is bounded by `work_mem` had an asterisk.

## Decisions

**The query tells the planner the order it would like; the planner is not
taught about ORDER BY.** Planning the `FROM` clause was split in two: its
columns are resolved first, then the rest of the query is analysed far
enough to know whether its `ORDER BY` is a list of plain columns in
ascending order, and that list is handed to the planner as a hint together
with the number of rows the query will read. The planner costs each access
path with what would follow it — a sort, or reading only the first rows —
and reports the order of the plan it chose. If the order is the one asked
for, the sort is left out.

*Alternative considered:* "interesting orders" in the join search, as
System R did it: keep, for every set of relations, the best plan per
useful order, so that a join order can be chosen because it avoids a sort.
That is the complete answer and it multiplies the states of the dynamic
programming. What was built covers one table chosen for its order, and
any join that happens to preserve the order of its first table.

*Cost accepted:* `SELECT ... FROM a JOIN b ... ORDER BY a.id LIMIT 10` is
sorted in full unless the join order chosen for other reasons starts with
`a` and uses nested loops.

**Only ascending, and only where NULLs cannot be in the wrong place.**
The B+tree cursor moves forward only; a backward scan is a storage change
of its own. And since keys put NULL first while `ORDER BY` puts it last,
an index counts as sorted only for `NOT NULL` columns or under
`NULLS FIRST`. The alternative for nullable columns — scan the non-NULL
part, then the NULLs — is two range scans glued together, and was left
out.

**An index's order includes the primary key.** Index entries are the
indexed values followed by the key of the row, so entries with equal
values are in primary key order. This is what makes an ordered index scan
return ties in the same order as a stable sort of the table would, and so
what lets a test compare the two exactly.

**One way of overflowing for every hash table.** Grouping, duplicate
removal, `INTERSECT` and `EXCEPT` share a rule: when the table is full it
takes no new keys, rows of keys already in it are handled in place, and
the rest are set aside in files chosen by a hash of the key, to be handled
in later passes. It needs no change to what each operator does with a row
it accepts, only a decision, per row, between "now" and "later".

*Alternative considered:* sort-based grouping when memory is short — sort
the input with the external sort, then group adjacent rows. It reuses the
sorter and gives ordered output for free. It also sorts every row, where
the scheme chosen sorts none and re-reads only the rows that overflowed;
and the set operations would have needed their own version.

*Alternative considered:* spilling the groups themselves (partial
aggregate states) instead of raw rows. It saves re-reading wide rows, and
requires every aggregate to be able to write its state out and merge two
states, including `count(DISTINCT)`. Raw rows need nothing from the
aggregates.

**Fan-out chosen from size.** A split now uses as many files as the size
of what is being split calls for, between two and sixteen, instead of
always sixteen. With a fixed sixteen, a partition slightly too large was
cut into sixteen slivers, each a pass of its own: the first version
grouped 8 192 rows in 273 passes where 49 do.

## Consequences

Output order of a `GROUP BY` or `DISTINCT` without `ORDER BY` changes when
it overflows: first the groups that fitted, then the others, pass by pass.
SQL promises no order there, and within memory the order of first
appearance is kept, which the sqllogictest baseline was recorded with.

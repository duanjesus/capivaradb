# 8. Planner: cost-based, left-deep, exact up to ten tables, nested loops only

Status: accepted

## Context

Until milestone 6 a query ran exactly as written: tables joined in the
order of the `FROM` clause, the `WHERE` clause applied after the joins,
every table read in full. Indexes existed only to enforce uniqueness. Two
sqllogictest scripts that join up to fifteen tables mostly timed out.

The executor still materialises every step and knows one join method, the
nested loop. Replacing it is milestone 7. The planner had to be worth
having with the executor as it is.

## Decisions

**Cost-based, with statistics, rather than rules.** "Use an index when
there is one" is wrong as soon as the index returns most of the table, and
no rule orders a join. Costs need row counts, so there is an `ANALYZE`, and
it runs by itself after a table has changed by a fifth: a planner that is
only good when someone remembers to analyse is a planner that is usually
bad.

*Cost accepted:* statistics are a number of rows and, per column, distinct
values, NULL fraction and numeric range. No histograms. Estimates on skewed
data are wrong, sometimes by a lot, and `EXPLAIN ANALYZE` shows it.

**Join order: exact by dynamic programming up to ten tables, greedy
beyond.** The exact search costs 2ⁿ sub-plans, which is a thousand at ten
tables and thirty thousand at fifteen — per query, at prepare time. Ten is
where it stops being unnoticeable.

*Alternative considered:* greedy only. It is a few lines, and it is what
the first version did. It also chose a cross product on a five-table query
of the test suite, because the cost of a step did not include the rows it
handed to the next one. The fix was to the cost model, not the search, but
the episode is the argument for the exact search wherever it is affordable:
it is not misled by one bad local choice.

**Left-deep plans only.** One table is added at a time to what has been
joined so far. With nested loops as the only join method, a bushy plan
would have to materialise its inner side to be useful, and the executor
has nowhere to keep that but memory. This is to be revisited with hash
joins.

**Index nested loop as the one fast join.** The inner table is looked up
through an index once per outer row. It needs no executor work beyond the
index scan that single-table queries need anyway, and it is what turns a
join from the product of its inputs into something proportional to its
result. Hash and merge joins stay in milestone 7, with the executor that
can run them without materialising.

**`LEFT JOIN` is not reordered.** A left join and its two sides are planned
as one opaque item. Conditions are pushed into it only where that is
plainly valid: `WHERE` conditions on the preserved side, `ON` conditions on
the nullable side.

*Cost accepted:* queries that mix many inner and left joins get worse
plans than they could. The identities that allow reordering outer joins
are easy to get subtly wrong, and a wrong answer is not a price worth
paying for a faster one.

**Conditions containing subqueries are applied after all joins.** A
correlated subquery's references to the outer query are not visible in the
condition that contains it, so its earliest safe position is not known
without analysing the subquery. Applying it last is always correct.

**Plans are closures, with a tree beside them for `EXPLAIN`.** Each plan
node carries the function that executes it and counters for rows and
loops. `EXPLAIN` prints the tree; `EXPLAIN ANALYZE` runs it and prints the
counters. What is explained is by construction what runs.

**PostgreSQL's `EXPLAIN` format and switch names, not its numbers.** The
shape of the output and the names `enable_indexscan` and
`join_collapse_limit` are PostgreSQL's, because people can already read
them. The cost numbers are this planner's own units and are not comparable
with PostgreSQL's.

## How it is kept honest

A planner may choose any plan but must never change an answer, and that is
directly testable: the same query under every combination of the planner's
switches must return the same rows (`TestPlansAgree`, random queries), and
sqllogictest supplies answers this planner had no part in. Mutation
testing covers the mistakes those two would be expected to catch; see
[planner.md](../planner.md).

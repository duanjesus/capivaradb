# 4. Execute the new SQL now, naively, and pin its answers

Status: accepted. The executor it describes was replaced in milestone 7 (decision 9); its role as the reference is now played by the new executor with every optimisation switched off.

## Context

Milestone 2 is "the parser". A parser can be finished without anything
being able to run what it accepts: the plan puts the planner at milestone 6
and the real executor at milestone 7. But a parser tested only against
itself proves little — whether `a NOT IN (SELECT ...)` was *understood*
shows in the rows that come back, not in the tree.

## Decision

Everything the parser accepts is also bound and executed by the in-memory
engine in this milestone, with the least clever implementation that is
correct: materialise every step, nested-loop joins in the order written,
filters after joins, subqueries re-run per outer row.

Its answers are then pinned by sqllogictest: `tools/slt` runs 104 121
records, and CI fails if the number passing drops.

## Why

- **It makes the front end testable by running it.** The binder — scopes,
  grouping rules, type inference, NULL semantics — is where SQL's subtleties
  live, and none of it is throwaway. It needs an executor underneath to be
  checked against known-good results.
- **It gives the later milestones an oracle.** When the planner starts
  reordering joins and the executor starts streaming, "the result changed"
  is detectable immediately, on a hundred thousand queries. Optimising
  without that is guessing.
- **It keeps the clever code honest.** A cost-based plan is only worth
  something if it beats the naive one; the naive one has to exist to be
  beaten. `select4.test` and `select5.test` currently time out on many-table
  joins — that is the benchmark for milestone 6.

## Consequences

- The executor is slow in ways that are documented rather than patched: no
  predicate pushdown, no hash join, no index use. A join that would build
  more than two million intermediate rows is refused, because exhausting the
  machine's memory is worse than an error.
- Milestone 7 replaces `engine/select.go`'s execution, not its semantics.
  The binder and the `selectPlan` boundary stay.
- The roadmap's milestone 2 grew: it delivered a binder and a reference
  executor as well as a parser. The split between milestones 6/7 and this
  one is now "make it fast" versus "make it right".

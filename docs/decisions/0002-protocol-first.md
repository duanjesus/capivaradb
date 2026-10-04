# 2. Build the wire protocol first, in front of a throwaway engine

Status: accepted

## Context

A database can be built bottom-up (pages, then B+tree, then everything
above) or top-down. Bottom-up produces months of code that can only be
exercised by unit tests. Top-down needs something to stand in for the
layers that do not exist yet.

## Decision

Milestone 1 implements the complete PostgreSQL wire protocol and, behind
it, a deliberately naive in-memory engine: slices of rows, one lock, undo
closures for rollback.

## Why

- From the first milestone the project can be driven by `psql` and by real
  drivers. Every later milestone is tested end to end with tools that were
  not written here, which is a much stronger check than our own tests.
- The protocol dictates the shape of the engine's API — statements must be
  describable before they are executed, results must be resumable, a
  running statement must be cancellable. Discovering that after writing an
  executor would mean rewriting it.
- The compatibility tests written now act as a regression suite while the
  storage engine, WAL and MVCC are swapped in underneath.

## Consequences

- The in-memory engine is thrown away piece by piece. It is kept small and
  its shortcomings are documented rather than patched.
- Until milestones 3–5 land, the database makes no durability or isolation
  promises, and says so: the README lists it, and
  `SHOW transaction_isolation` reports `read uncommitted`.
- The lexer, parser, binder and type system written for milestone 1 are
  not throwaway; milestone 2 extends them.

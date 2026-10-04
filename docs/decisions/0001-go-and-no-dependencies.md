# 1. Go, and nothing but the standard library in the core

Status: accepted

## Context

The project exists to demonstrate building database internals. Every piece
taken from a library is a piece not demonstrated: a parser generator hides
the parser, an embedded key-value store hides the storage engine, a
protocol library hides the protocol.

## Decision

The database is written in Go. The root module may only import the Go
standard library. Parser, wire protocol, storage, WAL, MVCC, planner and
executor are written by hand.

Dependencies are allowed in tests that check compatibility with other
people's software (drivers), and those tests live in separate modules under
`compat/` so they cannot become dependencies of the database.

CI enforces the rule by checking that `go list -m all` lists exactly one
module for the root.

## Why Go

- Goroutines and the `net` package fit a connection-per-goroutine server
  with no framework.
- Direct control over files (`os.File`, `Sync`, `ReadAt`/`WriteAt`) without
  a runtime in the way, which milestones 3 and 4 depend on.
- A race detector, a fuzzer and benchmarks ship with the toolchain; they
  are the tools the concurrency and crash-safety milestones need.
- Garbage collection is a real cost for a buffer pool. The plan is to keep
  pages in a few large byte slices the collector does not need to scan,
  and to measure.

## Consequences

- More code to write and to get right, which is the point.
- No help from battle-tested libraries: correctness has to come from tests
  — raw-protocol tests, driver tests, crash tests, isolation tests.
- Checksums, hashing and randomness come from the standard library
  (`hash/crc32`, `crypto/rand`); that is allowed.

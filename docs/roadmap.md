# Roadmap

Each milestone ends with working software, tests, updated documentation and
screenshots under `docs/screenshots`. A milestone is not started until the
previous one is green.

## 1. Wire protocol — done

Startup, simple query, extended query, cancellation, text and binary
formats. Acceptance: psql, pgx and pgjdbc connect and run queries, checked
by automated tests. See [wire-protocol.md](wire-protocol.md).

## 2. SQL parser

Grow the hand-written parser to the SQL the later milestones need:

- `SELECT` with joins (`INNER`, `LEFT`, `CROSS`), `GROUP BY`, `HAVING`,
  `ORDER BY`, `LIMIT`/`OFFSET`, `DISTINCT`, subqueries in `FROM`
- Aggregates, `CASE`, `IN`, `BETWEEN`, `LIKE`
- `CREATE INDEX`, table-level constraints, `DEFAULT`
- Transaction statements with isolation levels

Acceptance: a parser test corpus, a round-trip property (parse → print →
parse gives the same tree), and fuzzing with `go test -fuzz` that never
panics.

## 3. Storage engine

- 8 kB slotted pages with a checksum
- Buffer pool with pin counts and clock eviction
- B+tree keyed by primary key for tables; secondary indexes as B+trees
  pointing at primary keys
- Persistent catalog stored in its own tables

Acceptance: data survives a clean restart; B+tree invariants checked by a
verifier after randomised insert/delete workloads; the buffer pool works
with a pool far smaller than the data.

## 4. Write-ahead log and recovery

- Physiological log records, page LSNs, group commit
- Checkpoints; recovery in analysis, redo and undo passes
- `fsync` discipline, including the directory after file creation

Acceptance — the centrepiece of the project: a harness that runs a
workload, kills the server process at random points (including in the
middle of page writes, using an injected faulty file layer), restarts it,
and verifies that every acknowledged commit is present and no uncommitted
change is.

## 5. MVCC

- Row versions with creating/deleting transaction IDs
- Snapshot isolation; first-committer-wins on write conflicts
- Vacuum of dead versions

Acceptance: concurrent tests for each anomaly — dirty read, non-repeatable
read, phantom, lost update must not happen; write skew can, and is
documented as the known gap between snapshot isolation and serializable.

## 6. Planner

- Table statistics
- Access path choice between sequential scan and index scan
- Join ordering by dynamic programming for small join counts
- `EXPLAIN` and `EXPLAIN ANALYZE`

Acceptance: plan tests asserting the chosen plan, and benchmarks showing
the chosen plan beating the alternative.

## 7. Executor

- Volcano iterators behind the existing `Rows` interface
- Hash join, merge join, hash aggregation, external merge sort
- Vectorised execution if time allows

## Throughout

- **sqllogictest**: a runner for a subset of SQLite's sqllogictest corpus,
  with the pass rate published in the README and tracked per milestone.
- **Benchmarks**: reproducible scripts, results reported with the hardware
  and commit they were measured on.
- **No dependencies in the core**, enforced by CI.

# Roadmap

Each milestone ends with working software, tests, updated documentation and
screenshots under `docs/screenshots`. A milestone is not started until the
previous one is green.

## 1. Wire protocol — done

Startup, simple query, extended query, cancellation, text and binary
formats. Acceptance: psql, pgx and pgjdbc connect and run queries, checked
by automated tests. See [wire-protocol.md](wire-protocol.md).

## 2. SQL parser and binder — done

The hand-written parser now covers `SELECT` with `INNER`/`LEFT`/`CROSS`
joins, subqueries in `FROM` and in expressions, `GROUP BY`, `HAVING`,
`DISTINCT`, `ORDER BY`, `LIMIT`/`OFFSET`; `CASE`, `IN`, `BETWEEN`, `LIKE`,
aggregates; `CREATE INDEX`, table-level constraints, `DEFAULT`;
`INSERT ... SELECT`; transaction statements with isolation levels.

So that the SQL could be checked by running it, the in-memory engine learned
to execute all of it, in the simplest way that is correct.

Acceptance, all automated:

- a parser corpus pinned to canonical forms, and the round-trip property
  (parse → print → parse gives the same tree);
- a fuzzer that checks "never panics" and the round trip on arbitrary input;
- 104 121 sqllogictest records at 99.99%, with a baseline CI enforces
  ([sqllogictest.md](sqllogictest.md));
- psql transcripts and pgx tests for the new queries.

Left for later milestones: `UNION`/`INTERSECT`/`EXCEPT`, `RIGHT`/`FULL`
joins and `USING` (milestone 7), `EXPLAIN` (milestone 6).

## 3. Storage engine — done

- 8 kB slotted pages with a CRC-32C checksum
- Buffer pool with pin counts and clock eviction
- B+trees with variable-length keys, overflow pages and linked leaves;
  tables clustered by primary key, secondary and unique indexes as B+trees
- Persistent catalog stored in a B+tree of its own
- A consistency checker for trees, indexes and page ownership

Acceptance, all automated:

- data, constraints, defaults and indexes survive a restart, at the engine
  level and through psql against a real file;
- B+tree invariants verified during randomised and fuzzed workloads, with
  a map as the oracle and no page allowed to leak;
- a 9 MB table worked through a 256 kB buffer pool;
- every earlier test and all of sqllogictest run on the new storage with
  no regression, with the consistency checker run after each.

Details in [storage.md](storage.md) and
[decisions/0005](decisions/0005-storage-layout.md). Not crash-safe yet:
that is the next milestone.

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
- Set operations (`UNION`, `INTERSECT`, `EXCEPT`), `RIGHT`/`FULL` joins
- Vectorised execution if time allows

## Throughout

- **sqllogictest**: `tools/slt` runs a slice of SQLite's sqllogictest corpus,
  with the pass rate published in the README and tracked per milestone.
- **Benchmarks**: `scripts/bench.sh`, results in [benchmarks.md](benchmarks.md) with the hardware
  they were measured on.
- **No dependencies in the core**, enforced by CI.

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
[decisions/0005](decisions/0005-storage-layout.md).

## 4. Write-ahead log and recovery — done

- A write-ahead log with physical redo (page images and byte-range deltas)
  and logical undo, one record per B+tree operation
- Recovery in analysis, redo and undo passes; undo steps and post-commit
  work marked in the log so that neither is ever repeated
- Full-page images after each checkpoint, so torn pages are repairable
- Checkpoints: on demand, at shutdown, and automatic by log size
- `fsync` at commit, before any page write, at checkpoint, and of the
  directory when the files are created

Acceptance — the centrepiece of the project:

- a simulated disk that loses any subset of unsynced writes and tears the
  rest; about 800 crashes per run under a random workload, including
  crashes during recovery, each checked against a shadow database and by
  the consistency checker;
- a real process killed with `SIGKILL` a dozen times while writing;
- mutation testing of the crash tests: nine ways of breaking the
  durability rules, all caught.

Details in [recovery.md](recovery.md) and
[decisions/0006](decisions/0006-logging-scheme.md).

## 5. MVCC — done

- Row versions with creating and deleting transaction IDs, stored side by
  side in the table's B+tree
- Snapshots; read committed (a snapshot per statement) and repeatable read
  (a snapshot per transaction, i.e. snapshot isolation)
- Writers that conflict wait for each other, with deadlock detection;
  under repeatable read a transaction that would overwrite a change it
  cannot see fails with a serialization error
- Uniqueness enforced against what is alive, not against what is visible
- Vacuum of dead versions, on request and automatic

Acceptance:

- a transcript per anomaly — dirty read, non-repeatable read, phantom, lost
  update — showing it does not happen at the level that forbids it, and
  one showing that write skew does, which is the documented gap between
  snapshot isolation and serializable;
- deadlocks between two and three transactions detected; waits cancellable;
- concurrent transfers from eight goroutines with readers checking, at
  every moment, that the total is unchanged;
- the crash tests of milestone 4 still passing on versioned rows;
- mutation testing extended with ten ways of breaking the isolation rules,
  all caught.

Details in [mvcc.md](mvcc.md) and
[decisions/0007](decisions/0007-mvcc-design.md).

## 6. Planner — done

- `WHERE` split into conditions, each checked at the earliest point where
  its tables are available; the rules for what may move across a
  `LEFT JOIN`
- Access path choice between a sequential scan and a scan of the primary
  key or an index (equalities on leading columns, then one range); used by
  `UPDATE` and `DELETE` too
- Join ordering: exact, by dynamic programming over sets of tables, up to
  ten tables; greedy beyond. Nested loops, with an index lookup on the
  inner side where there is one
- Table statistics: `ANALYZE`, automatic re-analysis, persisted in the
  catalog
- `EXPLAIN` and `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF)`
- `enable_indexscan` and `join_collapse_limit`, to see the rejected plan

Acceptance, as delivered:

- plan tests asserting the `EXPLAIN` output of representative queries;
- 400 random queries run under every combination of the planner's
  switches, which must agree row for row;
- sqllogictest's two many-table join scripts added to the regular run:
  `select5.test` from 51.5% (most failures being timeouts) to 100%,
  `select4.test` from 39.0% to 74.1%, the rest needing set operations;
- benchmarks of the chosen plan against the rejected one: 38 to 790 times
  faster on 20 000 rows;
- mutation testing extended with six ways of making the planner return
  wrong rows, all caught.

Details in [planner.md](planner.md) and
[decisions/0008](decisions/0008-planner-design.md).

## 7. Executor — done

- Volcano iterators behind the existing `Rows` interface: results are
  cursors, and rows are computed as the client asks for them
- Scans in batches that find their place again by key, holding no lock
  between batches; a query keeps its own snapshot until it is closed
- Hash join (inner, left, full) with Grace partitioning to disk; merge
  join, with sorts skipped for inputs in primary key order; nested loops
  that keep their inner side in a spillable buffer
- External merge sort, and a heap for `ORDER BY ... LIMIT`; all stable
- `work_mem`, and `enable_hashjoin` / `enable_mergejoin` /
  `enable_nestloop`
- `UNION`, `INTERSECT`, `EXCEPT`, each with `ALL`; `RIGHT` and `FULL`
  joins; `JOIN ... USING`
- The limit of two million intermediate rows per join is gone

Acceptance, as delivered:

- every set operation and join kind tested against answers worked out by
  hand;
- the planner's 400 random queries now run under ten settings — each join
  method alone, with and without indexes, with sorts and hash tables
  forced to disk — and all must agree;
- memory measured: sorting 40 000 rows holds 6.4 MB with room and 0.9 MB
  with `work_mem = 256kB`; a scan holds 64 kB whatever the table's size;
- a cursor read through the JDBC driver, fifty rows at a time, while
  another connection deletes the table and vacuums it;
- sqllogictest at 109 399 of 109 414 records: `select4.test`, the last
  script with real failures, from 74.1% to 100%;
- mutation testing extended with seventeen ways of making the executor
  almost right, all caught; the script now refuses mutants that do not
  compile, which exposed one from milestone 6 that had been passing for
  that reason.

Not done: vectorised execution, which the plan had as "if time allows".
Details in [executor.md](executor.md) and
[decisions/0009](decisions/0009-executor-design.md).

## After the seven

The plan is complete. What a next round would take on, roughly in order of
what it would buy:

- spilling for `GROUP BY` and `DISTINCT`, the one place a query's memory
  still follows its data;
- using index order for `ORDER BY`, so that `ORDER BY id LIMIT 10` reads
  ten rows;
- writers in parallel: latches per page instead of one lock per database;
- group commit, to get past one `fsync` per transaction;
- system catalogs, so that `\d` and GUI tools work;
- more types: `numeric`, dates and times.

## Throughout

- **sqllogictest**: `tools/slt` runs a slice of SQLite's sqllogictest corpus,
  with the pass rate published in the README and tracked per milestone.
- **Benchmarks**: `scripts/bench.sh`, results in [benchmarks.md](benchmarks.md) with the hardware
  they were measured on.
- **No dependencies in the core**, enforced by CI.

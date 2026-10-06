# 7. MVCC: versions in the tree, no commit log, wait by retrying the statement

Status: accepted

## Context

Until milestone 5 a transaction's changes were visible to everyone the
moment a statement ran. Isolation needs readers to see a consistent,
committed state while writers are in progress, and it needs a rule for what
happens when two writers want the same row.

## Decisions

**PostgreSQL's model: `xmin`/`xmax` per version, snapshots of the set of
committed transactions.**

*Alternative considered:* commit timestamps, as in most key-value MVCC
stores. A version is then stamped with its commit time, which is not known
when it is written, so uncommitted versions are "intents" that have to be
found and rewritten at commit. Stamping with the *transaction ID* instead
means commit touches no data at all: the transaction simply stops being in
progress.

**Versions live in the clustered tree, keyed by row key + inverted `xmin`.**
Decision 5 left this open. The versions of a row are adjacent and newest
first, so finding the visible one is a short scan, and no second structure
(a heap, an undo segment) is needed.

*Cost accepted:* dead versions sit among live ones and slow scans until
vacuum removes them.

**No commit log.** A rollback physically removes what the transaction
wrote, using the undo records the write-ahead log already has. Every
transaction ID in the data therefore belongs to a transaction in progress
or to a committed one, and visibility needs only the in-memory registry of
transactions in progress.

*Why it is safe across a crash:* recovery undoes every unfinished
transaction before the database accepts work, so afterwards everything on
disk is committed. This leans directly on milestone 4, which is the reason
undo was made logical and exactly-once there.

*Cost accepted:* rollback is proportional to the work done, where
PostgreSQL's is instant. Rollbacks are rare; commits are not.

**Transaction IDs come from the log sequence number.** IDs must grow across
restarts. The LSN already does and is already durable, so no new counter
has to be persisted.

**Writers wait by undoing and retrying the statement.** A statement that
meets another transaction's uncommitted change is rolled back (statements
were already atomic through the log), releases the database lock, sleeps
until that transaction ends, and runs again from the start.

*Alternative considered:* row locks with a lock manager, and resuming the
statement where it stopped. That is what a mature system does; it needs
lock queues, lock escalation rules, and an executor that can be suspended
mid-row. Retrying needs none of that and gives the same observable
behaviour, including PostgreSQL's read-committed rule that the waiter sees
the new row version.

*Cost accepted:* a statement that waits does its work twice.

**Deadlock detection at the moment of waiting.** Each transaction waits for
at most one other, so the wait-for graph is a set of chains and following
one is enough. The statement that would close a cycle fails.

**A failed transaction is undone immediately.** Any error inside a
transaction block rolls its work back on the spot, although the block stays
open until the client says `ROLLBACK`. Otherwise the victim of a deadlock
would keep the others waiting until its client reacted.

**`SERIALIZABLE` is accepted and reported as `repeatable read`.** Refusing
it would break clients that ask for it by default; pretending to provide
it would be a lie. Answering truthfully when asked is the middle course.

**The same lock as before.** Writers are still serialised by one
database-wide lock, held for the duration of a statement. MVCC was added
for isolation and non-blocking reads, not for parallel writes; page latches
and finer-grained locking were left out deliberately, to keep this
milestone about getting the semantics right.

## Consequences

- Isolation holds at two levels, with the anomalies each allows stated and
  tested, write skew included.
- Throughput for writers is unchanged: one at a time.
- The catalog is not versioned; schema changes are visible before commit
  and some are refused while others have uncommitted work.
- Vacuum is required, and runs under the database lock.
- The crash tests needed no new mechanism: versioned rows go through the
  same undo and redo as before.

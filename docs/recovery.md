# Write-ahead log and crash recovery

A transaction that was acknowledged as committed survives the server being
killed, and the machine losing power. A transaction that was not leaves no
trace. This page explains how, and — more importantly — how that claim is
tested.

The code is `internal/storage/wal.go`, `pager.go` and `recovery.go`.

## The guarantee, precisely

- **Durability.** When `COMMIT` (or a statement outside a transaction block)
  returns successfully, its effects are on disk in a form recovery will
  restore, having been `fsync`ed.
- **Atomicity.** After a crash, each transaction is either entirely present
  or entirely absent, across all the tables and indexes it touched. A
  statement that fails is likewise undone entirely.
- **Integrity.** After recovery every B+tree is structurally valid, every
  index agrees with its table, and every page of the file is accounted for.
- **Recovery is restartable.** A crash during recovery is recovered from
  like any other.

What is *not* guaranteed yet is isolation: see the end of this page.

## The rule

A change is described in the log, and the log is made durable, **before**
the changed page may be written to the data file.

The data file is therefore never ahead of the log. After a crash, whatever
state the data file is in, the log knows how to bring every page forward to
the latest change (redo) and how to take back the changes of transactions
that never committed (undo).

Two places enforce it, and the mutation tests below break each to show the
tests notice:

- `writeFrame`, the only function that writes a page, first flushes the log
  up to that page's LSN.
- `Commit` flushes the log before returning.

## What is logged

The log is an append-only file of checksummed records, each with a *log
sequence number* (LSN) that grows with every record.

| Record | Written | Contents |
|--------|---------|----------|
| `PAGES` | at the end of every B+tree operation | The pages that operation changed, plus the file's meta values. One record per operation, so a page split is replayed whole or not at all. |
| `UNDO` | before each change a transaction makes | How to reverse it, in terms of keys: "delete this key", or "put this key back with this value". |
| `COMMIT` | at commit | The transaction ID, and the trees it dropped. Syncing this record *is* the commit. |
| `END` | when nothing remains to do | The transaction has committed and freed what it dropped, or has been rolled back completely. |

**Redo is physical.** A `PAGES` record holds each changed page either as a
full image or as the byte ranges that differ from before. It says nothing
about what the change meant. Replaying it is copying bytes.

**Undo is logical.** An `UNDO` record does not say "restore these bytes" but
"delete key K from the tree rooted at page R". By the time a transaction is
rolled back, the page its row was on may have been split, or filled by
other rows; reversing the change through the tree is what still works.

**Full-page images.** The first time a page changes after a checkpoint, it
is logged whole. If the crash then catches that page half-written to the
data file — a *torn page*, detected by its checksum — recovery does not
need the damaged contents: it starts from the image. Later changes to the
same page are logged as small deltas.

**Undoing is logged too.** Rolling back a change modifies pages like any
other operation, and its `PAGES` record carries a note: "this carried out
undo record N". The note is in the same record as the page changes, so the
two are durable together. An undo is therefore never applied twice, however
many times a rollback or a recovery is interrupted. (ARIES calls these
compensation log records.) The same mechanism marks trees freed after a
commit, so a `DROP TABLE` is neither lost nor done twice.

## Recovery

On open, if the log has records, the last run did not end cleanly. Recovery
is the three passes of ARIES:

1. **Analysis.** Read the log once. For each transaction, collect its undo
   records, which of them were already carried out, and whether it reached
   `COMMIT` and `END`. A record that fails its checksum is the torn end of
   the log; everything from there on is discarded.
2. **Redo.** Replay every `PAGES` record in order, for all transactions,
   committed or not — "repeating history". A change is skipped if the page
   on disk already has it, which its LSN tells. A page that cannot be read
   is rebuilt from its full image.
3. **Undo.** For transactions with no `COMMIT`, carry out their remaining
   undo records, newest first across all of them. For transactions that
   committed but did not finish, free the trees they dropped. Write `END`
   for each.

Recovery then takes a checkpoint, leaving a data file that needs no log.

If the meta page itself was torn, its values (page count, free list,
catalog root) are taken from the last `PAGES` record, which carries them.

## Checkpoints

A checkpoint makes the log durable, writes every modified page, writes the
meta page and `fsync`s the data file. If no transaction is in progress the
log is then discarded; otherwise it is kept, because it holds the undo
records of the open transactions.

Checkpoints happen at clean shutdown, on `CHECKPOINT`, and automatically
once 16 MB of log has accumulated. They bound recovery time.

## fsync discipline

| When | What is synced | Why |
|------|----------------|-----|
| `COMMIT` | the log | The commit must survive |
| Before any page write | the log, up to that page's LSN | The write-ahead rule |
| Checkpoint | the log, then the data file | Only then may the log be discarded |
| After discarding the log | the log file | So that the truncation itself is durable |
| Creating the files | the directory | Otherwise a crash can lose a file's directory entry |

`-nosync` turns all of it off. The database then still survives the server
being killed — the operating system has the data — but not a power failure.

## How it is tested

### A simulated disk that loses power

`storage.SimDisk` behaves the way a database must assume a disk behaves: a
write lands in a volatile cache and is only guaranteed after a sync. On a
simulated power failure, each unsynced write independently either reached
the medium, did not, or reached it torn — only its first sectors. Treating
unsynced writes as an arbitrary subset is harsher than real hardware, which
tends to lose a suffix.

This is what tests fsync placement. Killing a process cannot: the operating
system keeps what it was handed.

### The crash test

`TestCrashRecovery` runs a random workload — inserts, updates that move
rows, deletes, values large enough for overflow pages, transactions that
commit and that roll back, two transactions interleaved, tables and indexes
created and dropped inside transactions — on a simulated disk scheduled to
fail after a random number of operations. The buffer pool is twelve pages,
so uncommitted changes are constantly being written to the data file; the
checkpoint threshold is small, so checkpoints get interrupted too.

After each crash the database is reopened — and one time in three the disk
fails again *during recovery*, up to three times in a row. Then:

- `DB.Verify` must pass: trees valid, indexes matching, no page leaked.
- The contents must equal those of a **shadow database**: a second,
  in-memory database given the same statements, which never crashes. The
  only latitude is the transaction in flight at the moment of the crash,
  which may be on either side.

A full run is about 800 crashes; CI runs it under the race detector. The
workload and the disk are both seeded and nothing depends on map order or
on time, so a failure is reported with its seed and replays exactly.

### Killing a real process

`TestKillProcess` starts a child process that writes transactions to a real
file, printing a line after each commit returns, and kills it with
`SIGKILL` (`TerminateProcess` on Windows) at a random moment, a dozen times.
After each kill the parent opens the file and checks that every transaction
reported as committed is there in full, that no partial transaction is, and
that running totals updated in the same transactions agree with the rows.

`scripts/restart-smoke.sh` does the same through psql, once, for show.

### Testing the tests

A crash test that passes proves little unless it would fail if the code
were wrong. `scripts/mutation-test.sh` breaks the durability machinery in
one way at a time and requires the crash tests to fail each time:

| Mutation | Caught |
|----------|:------:|
| `COMMIT` returns without syncing the log | yes |
| A page is written before its log record is durable | yes |
| Recovery skips the undo pass | yes |
| Recovery skips the redo pass | yes |
| First change to a page after a checkpoint is not logged in full | yes |
| A checkpoint does not sync the data file | yes |
| A checkpoint discards the log while a transaction is open | yes |
| Undo steps are not marked as done in the log | yes |
| Trees dropped by a committed transaction are not freed after a crash | yes |

It earned its keep while it was being written, three times:

- The first version of the crash test passed with the write-ahead rule
  removed. With a pool of 48 pages nothing was ever evicted, so the rule
  was never exercised. The pool is now twelve pages.
- The "undo not marked as done" mutant survived until the workload gained
  schema changes inside transactions.
- That same mutant then survived on CI while being caught locally. The
  crash test was not deterministic: the simulated disk, recovery and the
  consistency checker each iterated over a Go map, so the same seed lost
  different writes from run to run, and whether the mutant was caught was
  luck. All three now iterate in a fixed order — a given seed replays
  exactly — and the case the mutant breaks has a test of its own,
  `TestStatementRollbackIsNotRepeatedByRecovery`, that does not depend on
  chance.

None of these would have been noticed by looking at a green test run.

One mutation is deliberately not on the list: making redo ignore page LSNs
and apply every record. That changes nothing observable, because replaying
all changes to a page in order converges on the same contents wherever the
page started. The LSN check is an optimisation, not a correctness condition
— which is itself worth knowing.

## Limitations

- **No isolation between transactions**, still. Two consequences here: a
  rollback restores the values the transaction saw, which overwrites
  anything another transaction wrote to the same rows in between; and
  `DROP TABLE`, `DROP INDEX` and `CREATE INDEX` are refused while another
  transaction has uncommitted changes, because nothing else stops that
  transaction's rollback from touching what was dropped. MVCC and locks
  (milestone 5) replace both.
- **One fsync per commit.** There is no group commit: with a single writer
  at a time there is nobody to group with. Throughput for single-row
  transactions is therefore bounded by the disk's sync latency.
- **A long transaction pins the log.** The log cannot be discarded while
  any transaction is open, so it grows until that transaction ends.
- **A value's overflow pages must fit in half the buffer pool**, because
  pages modified by an operation in progress cannot be evicted. With the
  default 32 MB pool that is about 16 MB per value.
- **Checkpoints stop the world.** They run under the database lock.
- **Media failure is detected, not repaired.** A page that fails its
  checksum and has no image in the current log is reported as corruption.
  There are no backups, no archiving of the log, no replication.

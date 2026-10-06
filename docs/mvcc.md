# Transactions and isolation (MVCC)

Transactions are isolated from each other by *multi-version concurrency
control*: a row is never overwritten, a reader picks the version its
snapshot can see, and readers and writers do not block each other.

The code is `internal/engine/mvcc.go` (snapshots, the transaction registry,
how versions are keyed), `store.go` (the rules for writing) and `db.go`
(statements, waiting, commit).

## What you get

| | Read committed (default) | Repeatable read |
|---|:---:|:---:|
| Dirty read — seeing uncommitted work | never | never |
| Non-repeatable read — a row changing between two reads | possible | never |
| Phantom — rows appearing between two reads | possible | never |
| Lost update — overwriting a change you did not see | never¹ | never² |
| Write skew | possible | **possible** |

¹ The second writer waits for the first, then applies its change to the new
value. ² The second writer fails with `40001` and is expected to retry.

- **Read committed** takes a new snapshot for every statement: each
  statement sees everything committed before it began.
- **Repeatable read** takes one snapshot, at the transaction's first
  statement, and keeps it. This is *snapshot isolation*.
- `SERIALIZABLE` is accepted and gives repeatable read. That is weaker than
  the name promises, so `SHOW transaction_isolation` answers
  `repeatable read`: the server does not claim what it does not do.
  (PostgreSQL behaved the same way until version 9.1.)
- `READ UNCOMMITTED` gives read committed, as in PostgreSQL: nothing here
  can read uncommitted data.

```sql
begin isolation level repeatable read;
set transaction isolation level repeatable read;   -- before the first query
set session characteristics as transaction isolation level repeatable read;
show transaction isolation level;
```

## Versions

Every version of a row records two transaction IDs: `xmin`, the transaction
that created it, and `xmax`, the one that deleted or superseded it (zero if
none).

- `INSERT` writes a version with `xmin` = the inserting transaction.
- `DELETE` sets `xmax` on the version it deletes. Nothing is removed.
- `UPDATE` does both: sets `xmax` on the old version and writes a new one.

In the table's B+tree a version's key is the row's key followed by eight
bytes holding the complement of `xmin`, so the versions of a row are
adjacent, newest first; the value is `xmax` followed by the tuple. Index
entries point at versions, one entry per version.

Two shortcuts follow from a transaction's own versions being invisible to
everyone else: a transaction that deletes a version it created itself
removes it outright, and one that updates it replaces it in place.

## Snapshots

A snapshot is what the set of committed transactions looked like at one
moment:

```go
type snapshot struct {
    next   uint64          // IDs from here on started later
    active map[uint64]bool // IDs below next that were in progress
    self   uint64          // the reader's own transaction
}
```

It regards a transaction as committed if it is the reader's own, or if its
ID is below `next` and it was not in progress when the snapshot was taken.
A version is visible if the snapshot regards its `xmin` as committed and
does not regard its `xmax` as committed.

A transaction that was in progress when a snapshot was taken stays
invisible to that snapshot forever, even after it commits. That single rule
is what makes the view stable.

**There is no commit log.** PostgreSQL records in `pg_xact` whether each
transaction committed or aborted, because an aborted transaction's versions
stay in the table. Here a rollback removes what the transaction wrote,
through the undo records of the write-ahead log — at run time and in crash
recovery alike. So every ID found in the data belongs to a transaction that
is either still in progress (and listed in the registry) or committed.

**IDs across restarts.** Versions on disk carry the IDs of their
transactions, so IDs must never go backwards. They are derived from the
log's sequence number, which is already durable and only grows.

**A transaction gets an ID when it first writes.** One that only reads
never appears in anyone's snapshot and costs nothing.

## When writers meet

A statement that wants to delete or replace a version another transaction
has already marked (`xmax` set by someone else):

| The other transaction… | Read committed | Repeatable read |
|---|---|---|
| is still in progress | wait for it, then run the statement again | wait for it, then run the statement again |
| committed | *(cannot happen: the statement's snapshot is newer)* | fail with `40001` |
| rolled back | *(its mark is gone)* proceed | proceed |

"Run the statement again" is literal. The statement's changes so far are
undone through the log — statements were already atomic — the database lock
is released until the other transaction ends, and the statement starts over
with a fresh snapshot (under read committed) or the same one (under
repeatable read, where it then fails if the other committed).

**Uniqueness is not a matter of snapshots.** A row committed after my
snapshot is invisible to me and still makes my insert a duplicate. So an
insert looks at whether a version with the same key is *alive*: created by
a transaction in progress → wait; not deleted → duplicate key; deleted by a
transaction in progress → wait; deleted by a committed one → free.

**Deadlocks.** Each waiting transaction waits for exactly one other. Before
a statement starts to wait, the chain of who-waits-for-whom is followed; if
it leads back, the statement that would close the cycle fails with `40P01`
and its transaction is rolled back by the client, which releases the others.

**Cancellation.** A statement that is waiting can be cancelled like any
other and fails with `57014`.

## Write skew: what snapshot isolation does not prevent

Two doctors are on call; each may go off only if the other stays.

```
a: begin isolation level serializable         b: begin isolation level serializable
a: select count(*) … where on_call   → 2      b: select count(*) … where on_call   → 2
a: update … set on_call = false … 'ana'       b: update … set on_call = false … 'bia'
a: commit                                     b: commit
```

Both commit, and nobody is on call. Neither transaction overwrote the
other's change — they wrote different rows — so nothing conflicted; each
made a decision based on a snapshot the other was invalidating. No serial
order of the two transactions could produce this.

Preventing it takes tracking what transactions *read* (PostgreSQL's
serializable snapshot isolation), which is out of scope.
`TestWriteSkewIsPossible` asserts the anomaly, so that the limitation is
on record rather than assumed away.

## Vacuum

Dead versions accumulate: every update leaves one behind. A version can be
removed once its deleter has committed and no snapshot in use can see it —
snapshots taken later will all see the deletion.

- `VACUUM` or `VACUUM table` does it on request (not inside a transaction
  block).
- After a commit, a table that has accumulated 2000 dead versions is
  vacuumed automatically.
- A long-running repeatable-read transaction holds its snapshot and thereby
  keeps the versions *it can see* from being removed. Versions created and
  deleted entirely after it began are not among them and go anyway.

## How it is tested

**Transcripts.** `isolation_test.go` scripts several sessions one statement
at a time, in the manner of PostgreSQL's isolation tester, and checks what
each statement returns — including that it *waits* when it must. A session
reports when it is blocked, so "waits" is observed, not inferred from a
timeout. Each anomaly in the table at the top has a transcript, as do
deadlocks between two and three transactions, unique-key conflicts,
cancellation of a wait, and vacuum under an old snapshot.

**Concurrency.** `TestConcurrentTransfers` moves money between accounts
from eight goroutines, retrying on `40001` and `40P01`, while three readers
add up the balances. The total must be unchanged at the end — no update
lost — and every reader must see that same total at every moment — no
snapshot ever shows half a transfer. It runs at both isolation levels, and
in CI under the race detector.

**Real connections.** `compat/pgx` has two pgx connections contend for a
row: the second `UPDATE` blocks on the wire until the first commits.

**Crashes.** The crash tests of milestone 4 now run on versioned rows: the
recovered database is compared with the committed state of a shadow
database, and the rollback of interleaved transactions goes through the
same undo records as before.

**Mutation testing.** `scripts/mutation-test.sh` now also breaks the
isolation rules, one at a time, and the tests above must fail:

| Mutation | Caught |
|----------|:------:|
| A snapshot treats transactions in progress as committed | yes |
| A version marked deleted is hidden before the deleter commits | yes |
| Repeatable read takes a new snapshot for every statement | yes |
| A transaction overwrites a change it cannot see instead of failing | yes |
| A write does not wait for another transaction's uncommitted change | yes |
| An insert does not wait for an uncommitted row with the same key | yes |
| An insert ignores a key being deleted by a transaction in progress | yes |
| Deadlocks are not detected | yes |
| Vacuum removes versions an open snapshot can still see | yes |
| A transaction's versions become visible to others before it commits | yes |

## Limitations

- **Writes do not run in parallel.** One database-wide lock still
  serialises statements that write. MVCC gives isolation and non-blocking
  reads; it does not give concurrent writers. Readers hold the lock shared
  only while scanning.
- **No serializable isolation**, as explained above.
- **The catalog is not versioned.** A table created by a transaction that
  has not committed is already visible to other sessions, as an empty
  table. `DROP TABLE`, `DROP INDEX` and `CREATE INDEX` are refused while
  another transaction has uncommitted changes.
- **No command IDs.** A statement's target rows are read before any is
  changed, so an `UPDATE` cannot chase its own output; but a subquery
  evaluated later in the same statement can see what the statement has
  already written.
- **No `SELECT ... FOR UPDATE`**, no lock timeout, no explicit locks.
- **Every `UPDATE` writes a full new version** and an entry in every index,
  even if only an unindexed column changed.
- **Vacuum stops the world** while it runs, and a table is scanned in full
  to find what to remove.

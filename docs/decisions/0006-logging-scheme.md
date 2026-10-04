# 6. Physical redo, logical undo, and tests that are themselves tested

Status: accepted

## Context

Milestone 4 has to make committed transactions survive a crash and
uncommitted ones disappear. The pages of an uncommitted transaction can
reach the data file before it commits (the buffer pool evicts whatever it
must), so recovery needs both directions: redo what is missing, undo what
should not be there.

## Decisions

**Redo is physical and generic.** The pager copies a page when a write
operation first touches it and, when the operation ends, logs the byte
ranges that differ. The B+tree code does not describe its changes; it does
not know the log exists.

*Alternative considered:* logical redo records per B+tree operation
("insert cell at slot 3"), as PostgreSQL has per access method. They are
smaller and need no copy, but every kind of page change needs its own
record and its own replay code, and each is a place for redo to diverge
from what the original operation did. Diffing bytes cannot diverge. The
cost is measured in [benchmarks.md](../benchmarks.md).

**One record per tree operation.** All pages an operation changes go into a
single log record, so recovery replays a split entirely or not at all. No
tree is ever seen half-modified.

**Undo is logical.** A transaction logs how to reverse each change in terms
of keys, and rolling back goes through the tree.

*Why not physical undo:* the page a row was inserted into may have been
split by another insert before the rollback. Restoring its old bytes would
destroy the other row.

**Rollback uses the same path at run time and at recovery.** `ROLLBACK`, a
failed statement, and recovery's undo pass all call the same function with
the same undo records, each step marked in the log as done. There is not a
"fast path" for the common case that the crash path does not share.

**Full-page images instead of a double-write buffer.** The first change to
a page after a checkpoint is logged whole, which makes torn pages
repairable. InnoDB instead writes pages twice; PostgreSQL does what is done
here. It bloats the log right after a checkpoint and needs no second file.

**Checkpoints are sharp.** A checkpoint writes everything under the
database lock. A fuzzy checkpoint, which lets work continue, needs a
dirty-page table in the log and is not worth it while there is one writer.

**The tests are mutation-tested.** For each rule the design depends on
there is a mutant that breaks it, and the crash tests must fail on every
one (`scripts/mutation-test.sh`).

*Why:* a durability bug is silent until the power fails. A passing crash
test and a crash test with a blind spot look the same. While this was
being built the suite passed with the write-ahead rule removed, because
the test's buffer pool was large enough that nothing was ever evicted; only
the mutant revealed it.

## Consequences

- Writes through the B+tree are two to four times slower than without
  logging. Acceptable for now, and a known place to optimise.
- The log grows quickly after a checkpoint and cannot be discarded while a
  transaction is open.
- A value's overflow pages must fit in half the buffer pool.
- Without isolation, undo restores what the transaction saw, not what is
  right in the presence of concurrent writers. That is milestone 5's
  problem, and it is stated in [recovery.md](../recovery.md).

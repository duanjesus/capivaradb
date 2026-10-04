# 5. One file, clustered B+trees, one code path for memory and disk

Status: accepted

## Context

Milestone 3 replaces "tables are Go slices" with real storage. The choices
below shape the next two milestones (write-ahead logging and MVCC), so they
are written down with their reasons.

## Decisions

**Tables are clustered B+trees, keyed by primary key.** Rows live in the
leaves, in key order. A table without a primary key gets a hidden row ID.
Secondary indexes store the indexed columns followed by the table key.

*Alternative considered:* a heap of rows addressed by (page, slot), with
every index — the primary key's included — pointing into it, as PostgreSQL
does. That makes secondary indexes cheaper to follow and updates cheaper
when the key does not change. Clustering was chosen because it needs one
structure instead of two (no heap, no free-space map), because the B+tree
then carries the whole storage test burden, and because primary-key access,
the commonest kind, is a single descent.

*Cost accepted:* a secondary index lookup is two descents, and a row moves
if its primary key changes.

**Order-preserving key encoding; the tree compares bytes.** The storage
layer has no notion of types or collations. The engine encodes keys so that
`bytes.Compare` orders them correctly.

*Why:* it keeps `internal/storage` small and testable against a plain map,
makes composite keys trivial (concatenate), and lets a fuzzer drive the
tree with arbitrary bytes.

**The same engine in memory and on disk.** `engine.New()` opens the storage
engine on an in-memory file rather than switching to a different
implementation.

*Why:* every existing test, and all 104 000 sqllogictest records, now run
through the pager and the B+tree. Storage bugs surface as wrong query
results in suites that were already there, and a consistency check runs
after each of them.

**The root page of a tree never changes.** A root split moves the contents
out and turns the root into their parent.

*Why:* the catalog stores root pages. If they moved, every root split would
be a catalog update — a write to another tree in the middle of a split,
which is exactly the kind of multi-page change that is hard to make atomic
in milestone 4.

**No rebalancing on delete.** Pages are freed when they become empty and
not merged before that.

*Why:* merging is the most intricate part of a B+tree and the least
valuable. An emptied page is reclaimed; a half-empty one is refilled by the
next inserts in its range.

**Space for the LSN is in the page header now.** Eight bytes are reserved
in every page for the log sequence number.

*Why:* recovery decides whether to replay a change by comparing the log
record's LSN with the page's. Reserving it now avoids a format change one
milestone later.

## Consequences

- Durability is not there yet, and the documentation says so wherever it
  matters: until the WAL exists, only a checkpointed file is trustworthy.
- MVCC (milestone 5) has to version rows inside a clustered tree. The
  likely shape is a version suffix in the key or an undo chain, to be
  decided then.
- The format has a version number in the meta page; a build refuses files
  written with another.

# Storage engine

Everything the database stores lives in one file made of 8 kB pages, with a
write-ahead log beside it ([recovery.md](recovery.md)).
`internal/storage` manages that file and knows nothing about SQL: it deals
in pages and in B+trees of byte strings. `internal/engine` decides what the
bytes mean.

```
   engine     rows, keys, catalog records        store.go, codec.go
  ─────────────────────────────────────────────────────────────────
              B+tree: Get / Put / Delete / Seek  btree.go, node.go
   storage    buffer pool, allocation, free list pager.go
              File: a real file, or memory       file.go
```

## The file

| Page | Contents |
|------|----------|
| 0 | Meta page: magic `CAPIVARA`, format version, page size, page count, head of the free list, root page of the catalog, LSN of the last checkpoint |
| 1… | B+tree pages (leaf, internal), overflow pages, free pages |

Every page begins with a 16-byte header:

| Offset | Size | Field |
|-------:|-----:|-------|
| 0 | 4 | CRC-32C of the rest of the page |
| 4 | 1 | Page type: meta, leaf, internal, overflow, free |
| 6 | 2 | Number of cells (B+tree pages) |
| 8 | 8 | LSN of the last log record that changed the page (see [recovery.md](recovery.md)) |

The checksum is computed when a page is written and verified when it is
read. A mismatch is reported as corruption instead of being interpreted as
data; it catches a torn write or a failing disk, it does not repair it.

The free list is a chain of *trunk pages*, each holding the IDs of up to
2042 free pages. Freeing a page appends its ID to the first trunk and does
not touch the page itself, so dropping a large table dirties a handful of
trunk pages rather than every page of the table. Allocation takes from the
list before growing the file. The file never shrinks.

## The buffer pool

The pager keeps a fixed number of pages in memory (`-cache`, 32 MB by
default) and reads the rest on demand.

- **Pinning.** A page in use is pinned and cannot be evicted; every `Fetch`
  is paired with an `Unpin` that says whether the page was modified. Tests
  assert that no page stays pinned after an operation.
- **Eviction** uses the clock algorithm: frames are swept in a circle, a
  page referenced since the last sweep gets a second chance, an
  unreferenced, unpinned one is evicted (written first if dirty). It
  approximates LRU without reordering a list on every access.
- **Memory.** The pool is a single `[]byte`. The garbage collector sees one
  pointer-free object regardless of the cache size, which was the concern
  recorded in [decision 1](decisions/0001-go-and-no-dependencies.md).
- **Counters** (hits, misses, evictions, reads, writes) are kept and
  reported by the tests.

`TestBufferPoolSmallerThanData` loads a 9 MB table through a 256 kB pool —
57 000 evictions — and checks every query result, across a restart.

## B+tree pages

Leaf and internal pages are *slotted pages*:

```
 0        16      26                                              8192
 ┌────────┬───────┬──────────────┬─────────────────┬───────────────┐
 │ header │ links │ slot array → │   free space    │ ← cell area   │
 └────────┴───────┴──────────────┴─────────────────┴───────────────┘
```

The slot array holds a two-byte offset per cell, in key order. Keeping the
page sorted therefore moves two-byte slots, never the cells themselves.
Deleting a cell leaves a hole that is reclaimed by compacting the page when
the space is needed.

- A **leaf cell** is `key length | value length | key | value`. Leaves are
  doubly linked in key order, so a range scan walks sideways and never
  climbs back up.
- An **internal cell** is `key length | child page | key`. The child of
  cell *i* holds keys smaller than that cell's key; a separate *rightmost
  child* holds the rest.
- A value too large to keep four cells on a page (about 2 kB) moves to a
  chain of **overflow pages**; the cell keeps the length and the first page.
- A key may be at most 1024 bytes, so that an internal page always has room
  to branch. A longer one is refused with `54000`.

## B+tree operations

**Lookup** descends from the root, binary-searching the slot array at each
level.

**Insert** descends to the leaf. If the cell fits, done. Otherwise the leaf
is split at the byte midpoint, the smallest key of the right half becomes
the separator, and the parent gets a new cell — which may split the parent
in turn, up to the root.

**The root page never moves.** When the root splits, its contents go to two
new pages and the root becomes their parent. Whoever stores a tree's root
page (the catalog) never has to update it.

**Delete** removes the cell. A leaf is taken out of the tree only when it
becomes empty: it is unlinked from its siblings, freed, and its pointer
removed from the parent, recursively if that empties the parent. A root
left with a single child is replaced by that child, which is how the tree
gets shorter. There is no merging of half-empty pages; PostgreSQL makes the
same choice, because merging costs writes and tables that shrink usually
grow again.

**Scan** (`Seek` + `Next`) copies one leaf at a time and holds no page
pinned between calls.

## How rows are stored

A **table** is one B+tree, holding every *version* of every row (see
[mvcc.md](mvcc.md) for why rows have versions).

- The row's key is the primary key, in an *order-preserving encoding*:
  comparing two encoded keys byte by byte gives the same result as
  comparing the values. That is what lets the tree stay ignorant of types.
- A table without a primary key is keyed by a hidden, ever-increasing row
  ID.
- A version's key is the row's key followed by eight bytes: the complement
  of the ID of the transaction that created it. The versions of a row are
  therefore adjacent, newest first.
- The value is the ID of the transaction that deleted the version (zero if
  none), then the row as a *tuple*: a null bitmap followed by the non-NULL
  columns in a compact, fixed layout per type.

The table is therefore *clustered*: rows live in the leaves in key order,
and a primary-key lookup is a single descent.
| Type | Key encoding (after a 0x01 "not NULL" byte; NULL is 0x00) |
|------|-----------------------------------------------------------|
| `integer`, `bigint` | 8 bytes big-endian with the sign bit flipped, so negatives sort first |
| `double precision` | IEEE bits; sign bit flipped for positives, all bits flipped for negatives |
| `boolean` | 1 byte |
| `text` | the bytes, `0x00` escaped as `0x00 0xFF`, terminated by `0x00 0x01` — so `'ab'` sorts before `'abc'` and an embedded NUL is harmless |

A **secondary index** is another B+tree, with one entry per row version,
whose key is *the indexed columns
followed by the row's table key*, with an empty value. Appending the table
key makes every entry distinct even when indexed values repeat, and looking
a value up is a seek to the prefix. `UNIQUE` constraints are unique indexes;
uniqueness is checked with that seek instead of the full scan the in-memory
engine did. NULLs never conflict, as in PostgreSQL.

The **catalog** is a B+tree too, rooted at a page recorded in the meta
page. It maps `t/<name>` to a table definition (columns, types, defaults as
SQL text, primary key, root page) and `i/<name>` to an index definition.
Defaults are stored as the canonical text of their expression and compiled
again on open.

## Consistency checking

`Tree.Verify` checks every structural invariant of a tree: well-formed
slotted pages, keys in order, keys within the bounds their ancestors
promise, all leaves at the same depth, no empty leaf, a sibling chain that
visits exactly the leaves in order, overflow chains of the right length, no
page reachable twice.

`DB.Verify` adds the cross-tree checks: every row decodes and has exactly
one entry in each index of its table, and **every page of the file belongs
to exactly one tree or to the free list** — nothing leaked, nothing shared.

It runs:

- after every engine test, on whatever the test left behind;
- after each sqllogictest script — 104 000 statements — in CI;
- during the randomised B+tree tests and the B+tree fuzzer, against a
  plain map as the oracle;
- on demand: `capivaradb -data file.cdb -check`.

## What is not there yet

- **Parallel writes.** One database-wide lock still serialises
  statements that write; MVCC (milestone 5) added isolation, not this.
- An `UPDATE` rewrites the row and all its index entries even when only an
  unindexed column changed.
- Ascending inserts leave pages half full, because every split is at the
  midpoint.

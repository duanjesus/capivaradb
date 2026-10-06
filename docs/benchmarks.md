# Benchmarks

Numbers are only worth something with the conditions they were taken under,
so every table here says what was run, on what, and how to run it again.

```bash
bash scripts/bench.sh
```

All measurements: Intel Core i7-11370H (4 cores, 8 threads, 3.3 GHz), NVMe
SSD, Windows 11, Go 1.27, single-threaded, 2 seconds per benchmark. One run
on a laptop: repeat runs differ by up to a quarter, so read these as orders
of magnitude.

## What a commit costs (milestone 4)

`internal/engine`: a prepared `INSERT` of one small row into a table with a
primary key, through the binder, the B+tree and the write-ahead log.

| | One row per transaction | 100 rows per transaction |
|---|---:|---:|
| In memory | 4.7 µs/row | 4.4 µs/row (229 000 rows/s) |
| File, `-nosync` | 9.8 µs/row | 4.6 µs/row (217 000 rows/s) |
| File, fsync at commit | **614 µs/row** | 11.9 µs/row (84 000 rows/s) |

With row versions (milestone 5) the same benchmark gives 6.0, 10.6 and
639 µs per row for single-row transactions, and 5.1, 5.5 and 13.5 µs with
100 rows per transaction. An insert now also checks whether any version of
the key is alive and writes a slightly larger entry; that costs about a
microsecond. The fsync still decides everything that matters.

How to read them:

- **The fsync is the commit.** A single-row transaction costs about 0.6 ms,
  of which the database's own work is under 10 µs. The other 98% is waiting
  for the disk to say the log is safe. That is roughly 1 600 commits per
  second on this disk, and it is the price of the durability guarantee, not
  an inefficiency to tune away. `-nosync` shows what is left without it.
- **Batching amortises it.** With 100 rows per transaction the same fsync
  is shared by 100 rows, and throughput rises fifty-fold.
- There is no group commit. With one writer at a time there is nobody to
  share a sync with, and writers are still serialised after milestone 5.

## B+tree

`internal/storage`, through the buffer pool and the log, on in-memory files
— so this times the data structure, the cache and the logging, not the
disk. Keys are 8 bytes, values 100 bytes. A checkpoint is taken every
20 000 operations, as the engine would.

| Benchmark | Milestone 3 | Milestone 4 | What it does |
|-----------|------------:|------------:|--------------|
| `PutSequential` | 1.0 µs | 4.0 µs | Insert keys in ascending order |
| `PutRandom` | 2.8 µs | 5.4 µs | Insert random keys |
| `Get`, in pool | 1.8 µs | 1.3 µs | Random lookups among 1 000 000 keys, all pages cached |
| `Get`, 2 MB pool | 3.0 µs | 2.5 µs | The same with a pool 100 times smaller than the data |
| `Scan` | 176 ns/row | 77 ns/row | Full scan of 1 000 000 rows |

How to read them:

- **Logging made writes two to four times slower.** Each operation now
  copies the pages it is about to modify, compares them afterwards to find
  what changed, and builds a log record; the first change to a page after a
  checkpoint logs all 8 kB of it. This is the straightforward
  implementation and has obvious room: the before-image copy could be
  avoided by having the B+tree report what it changed instead of having the
  pager discover it.
- **Reads did not change in this milestone**; the differences in the read
  rows are run-to-run variation (and a fair illustration of how much to
  trust a single run on a laptop).
- The small-pool lookup is less than twice as slow as the cached one
  because the "disk" is memory. On a real disk each miss is a read, and the
  device dominates.

What none of this shows: concurrency (readers do not block, but there is no
benchmark of it yet), or SQL queries (the executor materialises whole
tables and would dominate).

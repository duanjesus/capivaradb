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

## What the planner buys (milestone 6)

`internal/engine`: prepared statements on 20 000 orders, 1 000 customers
and 100 products, in memory, with a different parameter on every
execution. Each query is timed under the plan the planner chooses and
under the one it rejected, obtained by switching the planner off
(`enable_indexscan = off`, `join_collapse_limit = 1`).

| Query | Chosen plan | Without indexes | |
|---|---:|---:|---:|
| `select * from orders where id = $1` | 6.0 µs | 4.8 ms | 790× |
| `select * from orders where customer_id = $1` (20 rows) | 28.7 µs | 4.1 ms | 140× |
| `update orders set qty = qty + 1 where id = $1` | 31.3 µs | 4.0 ms | 130× |

One customer's orders with their products — three tables, written with the
largest first:

| Plan | Time |
|---|---:|
| Chosen: customer by name, its orders by index, each product by key | 144 µs |
| Joins reordered, no indexes | 6.7 ms |
| As written, no indexes | 5.4 ms |

How to read them:

- **Without an index, every query costs a scan of the table**: about 4 ms
  for 20 000 rows, 0.2 µs per row, whatever is asked. With one, the cost
  follows the number of rows wanted.
- **The update shows it is not only reads.** Finding the row was 99% of
  the work of changing it.
- **Reordering alone bought nothing in the join**, and the table says so:
  with no index, both orders compare the same 20 000 pairs of order and
  customer, and the difference between the two rows is noise. It is the
  index lookups that make the join 38 times faster, and reordering that
  makes them possible, by putting the one customer first.
- The ratios grow with the table: a scan is linear in its size and a
  lookup logarithmic. On 20 000 rows these are modest numbers; they are
  here to show that the planner chooses the right side, not to impress.
- These timings moved more between runs than the others on this page. A
  second run gave 5.5 µs and 8.2 ms for the first row, and 123 µs, 5.8 ms
  and 5.2 ms for the join. The conclusions do not depend on which run is
  read.

sqllogictest gives the other measure: its two scripts of many-table joins
went from minutes, with most queries timing out, to seconds
([sqllogictest.md](sqllogictest.md)).

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
benchmark of it yet), or queries over data larger than memory (the executor
materialises every step).

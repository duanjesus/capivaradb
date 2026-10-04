# Benchmarks

Numbers are only worth something with the conditions they were taken under,
so every table here says what was run, on what, and how to run it again.

```bash
bash scripts/bench.sh
```

## B+tree (milestone 3)

`internal/storage`, through the buffer pool, on an in-memory file — so this
times the data structure and the cache, not the disk. Keys are 8 bytes,
values 100 bytes.

Measured on an Intel Core i7-11370H (4 cores, 8 threads, 3.3 GHz), Windows
11, Go 1.27, single-threaded, 2 seconds per benchmark. One run, on a laptop:
repeat runs differ by up to a quarter, so read these as orders of magnitude.

| Benchmark | Result | What it does |
|-----------|-------:|--------------|
| `PutSequential` | 1.01 µs/op | Insert keys in ascending order: every insert lands in the rightmost leaf |
| `PutRandom` | 2.77 µs/op | Insert random keys: a binary search per level, splits all over the tree |
| `Get`, in pool | 1.82 µs/op | Random lookups among 1 000 000 keys, all pages cached |
| `Get`, 2 MB pool | 3.03 µs/op | The same with a pool 100 times smaller than the data: most lookups evict a page and read another from the (in-memory) file |
| `Scan` | 176 ns/row | Full scan of 1 000 000 rows, copying each key and value |

How to read them:

- The gap between sequential and random inserts is locality: ascending keys
  keep landing on the same few pages, random ones touch a different leaf
  each time.
- The small-pool lookup is less than twice as slow because the "disk" is
  memory. On a real disk each miss is a read, and that number is dominated
  by the device. It shows the pool's own overhead: evict, read, verify the
  checksum.
- These are single-threaded. The pager has one mutex; contention is a
  subject for milestone 5.

What they do not show: SQL-level throughput (the executor materialises
whole tables and would dominate), durability costs (there is no `fsync` on
the write path until the WAL), or concurrency.

package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
)

func newPager(t testing.TB, poolPages int) *Pager {
	t.Helper()
	p, err := Open(NewMemFile(), poolPages)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// model is the oracle: a plain map that must always agree with the tree.
type model struct {
	t    testing.TB
	tree *Tree
	want map[string][]byte
}

func (m *model) put(key, val []byte) {
	m.t.Helper()
	if err := m.tree.Put(key, val); err != nil {
		m.t.Fatalf("Put(%q): %v", key, err)
	}
	m.want[string(key)] = append([]byte(nil), val...)
}

func (m *model) delete(key []byte) {
	m.t.Helper()
	found, err := m.tree.Delete(key)
	if err != nil {
		m.t.Fatalf("Delete(%q): %v", key, err)
	}
	if _, was := m.want[string(key)]; was != found {
		m.t.Fatalf("Delete(%q) reported %v, the model says %v", key, found, was)
	}
	delete(m.want, string(key))
}

func (m *model) get(key []byte) {
	m.t.Helper()
	got, found, err := m.tree.Get(key)
	if err != nil {
		m.t.Fatalf("Get(%q): %v", key, err)
	}
	want, was := m.want[string(key)]
	if found != was || !bytes.Equal(got, want) {
		m.t.Fatalf("Get(%q): got %d bytes (found=%v), want %d bytes (found=%v)", key, len(got), found, len(want), was)
	}
}

// check verifies the tree's invariants and that a full scan returns exactly
// the model's contents in key order.
func (m *model) check() TreeInfo {
	m.t.Helper()
	info, err := m.tree.Verify()
	if err != nil {
		m.t.Fatalf("Verify: %v", err)
	}
	keys := make([]string, 0, len(m.want))
	for k := range m.want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := m.tree.Seek(nil)
	for i, want := range keys {
		k, v, ok := c.Next()
		if !ok {
			m.t.Fatalf("scan ended after %d of %d entries (err: %v)", i, len(keys), c.Err())
		}
		if string(k) != want || !bytes.Equal(v, m.want[want]) {
			m.t.Fatalf("scan entry %d: got key %q, want %q", i, k, want)
		}
	}
	if _, _, ok := c.Next(); ok {
		m.t.Fatalf("scan returned more than the %d expected entries", len(keys))
	}
	if c.Err() != nil {
		m.t.Fatal(c.Err())
	}
	if info.Entries != len(keys) {
		m.t.Fatalf("Verify counted %d entries, expected %d", info.Entries, len(keys))
	}
	if pinned := m.tree.p.Pinned(); pinned != 0 {
		m.t.Fatalf("%d pages left pinned", pinned)
	}
	return info
}

// checkNoLeaks verifies that every page of the file is accounted for: it is
// the meta page, belongs to one of the trees, or is on the free list.
func checkNoLeaks(t testing.TB, p *Pager, trees ...*Tree) {
	t.Helper()
	owner := map[uint32]string{0: "meta"}
	claim := func(id uint32, who string) {
		if prev, taken := owner[id]; taken {
			t.Fatalf("page %d belongs to both %s and %s", id, prev, who)
		}
		owner[id] = who
	}
	for i, tree := range trees {
		info, err := tree.Verify()
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range info.Pages {
			claim(id, fmt.Sprintf("tree %d", i))
		}
	}
	free, err := p.FreePages()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range free {
		claim(id, "the free list")
	}
	if len(owner) != p.PageCount() {
		t.Fatalf("%d of %d pages are accounted for: pages were leaked", len(owner), p.PageCount())
	}
}

func randomKey(rng *rand.Rand, space int) []byte {
	// A few shapes of key: short, long, and sharing long prefixes.
	n := rng.Intn(space)
	switch rng.Intn(4) {
	case 0:
		return []byte(fmt.Sprintf("k%d", n))
	case 1:
		return []byte(fmt.Sprintf("%s/%08d", bytes.Repeat([]byte("prefix"), 20), n))
	case 2:
		return binary.BigEndian.AppendUint64(nil, uint64(n))
	}
	return append(bytes.Repeat([]byte{byte(n)}, 1+n%300), byte(n>>8))
}

func randomValue(rng *rand.Rand) []byte {
	var size int
	switch rng.Intn(10) {
	case 0:
		size = 0
	case 1:
		size = 2000 + rng.Intn(100) // around the inline limit
	case 2:
		size = 3000 + rng.Intn(40000) // one to five overflow pages
	default:
		size = rng.Intn(200)
	}
	val := make([]byte, size)
	rng.Read(val)
	return val
}

// TestRandomOperations is the main B+tree test: long random sequences of
// inserts, overwrites, deletes and lookups, checked against a map, with the
// invariants verified along the way. The buffer pool is kept tiny so that
// pages are constantly evicted and re-read.
func TestRandomOperations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seed      int64
		pool      int
		ops       int
		keySpace  int
		deleteOdd int // one delete every this many operations
	}{
		{"growing", 1, 16, 20000, 100000, 10},
		{"churn", 2, 16, 30000, 500, 2},
		{"shrinking", 3, 32, 20000, 2000, 1},
		{"big pool", 4, 4096, 20000, 5000, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPager(t, tc.pool)
			tree, err := CreateTree(p)
			if err != nil {
				t.Fatal(err)
			}
			m := &model{t: t, tree: tree, want: map[string][]byte{}}
			rng := rand.New(rand.NewSource(tc.seed))
			for op := 0; op < tc.ops; op++ {
				key := randomKey(rng, tc.keySpace)
				switch {
				case rng.Intn(tc.deleteOdd+1) >= tc.deleteOdd:
					m.delete(key)
				case rng.Intn(4) == 0:
					m.get(key)
				default:
					m.put(key, randomValue(rng))
				}
				if op%2500 == 0 {
					m.check()
				}
			}
			info := m.check()
			checkNoLeaks(t, p, tree)
			stats := p.Stats()
			t.Logf("%d entries, depth %d, %d leaves, %d pages; pool of %d: %d hits, %d misses, %d evictions",
				info.Entries, info.Depth, info.Leaves, p.PageCount(), tc.pool, stats.Hits, stats.Misses, stats.Evictions)
			if tc.pool < 100 && stats.Evictions == 0 {
				t.Error("a pool this small should have had to evict pages")
			}

			// Emptying the tree gives every page but the root back.
			for k := range m.want {
				m.delete([]byte(k))
			}
			if info := m.check(); info.Depth != 1 || len(info.Pages) != 1 {
				t.Errorf("an emptied tree should be a single leaf, got depth %d and %d pages", info.Depth, len(info.Pages))
			}
			checkNoLeaks(t, p, tree)
		})
	}
}

func TestSequentialInsertsAndScans(t *testing.T) {
	p := newPager(t, 64)
	tree, _ := CreateTree(p)
	m := &model{t: t, tree: tree, want: map[string][]byte{}}
	key := func(i int) []byte { return binary.BigEndian.AppendUint64(nil, uint64(i)) }

	const n = 50000
	for i := 0; i < n; i++ { // ascending: every split is at the right edge
		m.put(key(i*2), []byte(fmt.Sprint("value-", i)))
	}
	info := m.check()
	if info.Depth < 2 {
		t.Errorf("expected the tree to have grown past a single leaf for %d entries, got depth %d", n, info.Depth)
	}

	// Seek lands on the first key >= the argument, present or not.
	for _, start := range []int{0, 1, 2, 999, 1000, n*2 - 2, n*2 - 1, n * 2} {
		c := tree.Seek(key(start))
		want := (start + 1) / 2 * 2
		k, _, ok := c.Next()
		if want >= n*2 {
			if ok {
				t.Errorf("Seek(%d): expected the end, got a key", start)
			}
			continue
		}
		if !ok || binary.BigEndian.Uint64(k) != uint64(want) {
			t.Errorf("Seek(%d): got %v (ok=%v), want %d", start, k, ok, want)
		}
	}
	last, err := tree.LastKey()
	if err != nil || binary.BigEndian.Uint64(last) != uint64(n*2-2) {
		t.Errorf("LastKey: %v, %v", last, err)
	}

	// Descending deletes empty the leaves from the right; ascending from
	// the left. Both must leave a valid tree at every step.
	for i := n - 1; i >= n/2; i-- {
		m.delete(key(i * 2))
	}
	m.check()
	for i := 0; i < n/2; i++ {
		m.delete(key(i * 2))
		if i%5000 == 0 {
			m.check()
		}
	}
	m.check()
	if last, _ := tree.LastKey(); last != nil {
		t.Errorf("LastKey of an empty tree: %v", last)
	}
	checkNoLeaks(t, p, tree)
}

func TestOverflowValues(t *testing.T) {
	p := newPager(t, 16)
	tree, _ := CreateTree(p)
	m := &model{t: t, tree: tree, want: map[string][]byte{}}
	rng := rand.New(rand.NewSource(7))
	big := func(n int) []byte {
		b := make([]byte, n)
		rng.Read(b)
		return b
	}
	// Sizes around every boundary: the inline limit and whole pages.
	capacity := PageSize - slotBase
	for i, size := range []int{maxInlineCell - 20, maxInlineCell, maxInlineCell + 1, capacity - 1, capacity, capacity + 1, 3 * capacity, 1 << 20} {
		m.put([]byte(fmt.Sprint("key", i)), big(size))
	}
	m.check()
	before := p.PageCount()

	// Overwriting a large value with a small one, and deleting, must free
	// the overflow chain; writing again must reuse those pages.
	m.put([]byte("key7"), []byte("small now"))
	m.delete([]byte("key6"))
	m.check()
	checkNoLeaks(t, p, tree)
	m.put([]byte("key7"), big(1<<20))
	m.check()
	if p.PageCount() != before {
		t.Errorf("file grew from %d to %d pages instead of reusing freed ones", before, p.PageCount())
	}
	for k := range m.want {
		m.get([]byte(k))
	}
}

func TestKeyLimits(t *testing.T) {
	p := newPager(t, 16)
	tree, _ := CreateTree(p)
	if err := tree.Put(make([]byte, MaxKeySize+1), nil); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("expected ErrKeyTooLarge, got %v", err)
	}
	// Keys of the maximum size still leave room for a tree to branch.
	m := &model{t: t, tree: tree, want: map[string][]byte{}}
	for i := 0; i < 300; i++ {
		key := bytes.Repeat([]byte{byte(i), byte(i >> 8)}, MaxKeySize/2)
		m.put(key, []byte{byte(i)})
	}
	m.put([]byte{}, []byte("the empty key is a key too"))
	m.check()
	m.get([]byte{})
}

func TestDropFreesEverything(t *testing.T) {
	p := newPager(t, 32)
	keep, _ := CreateTree(p)
	doomed, _ := CreateTree(p)
	rng := rand.New(rand.NewSource(9))
	for i := 0; i < 5000; i++ {
		key := []byte(fmt.Sprint("key", i))
		if err := doomed.Put(key, randomValue(rng)); err != nil {
			t.Fatal(err)
		}
		if err := keep.Put(key, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := doomed.Drop(); err != nil {
		t.Fatal(err)
	}
	checkNoLeaks(t, p, keep)
	free, _ := p.FreePages()
	if len(free) < 100 {
		t.Errorf("only %d pages were freed", len(free))
	}
	// New trees are built out of the recycled pages.
	before := p.PageCount()
	again, _ := CreateTree(p)
	for i := 0; i < 2000; i++ {
		again.Put([]byte(fmt.Sprint("key", i)), []byte("recycled"))
	}
	if p.PageCount() != before {
		t.Errorf("file grew from %d to %d pages", before, p.PageCount())
	}
	checkNoLeaks(t, p, keep, again)
}

// TestPersistence writes through a real file, reopens it and reads back.
func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.cdb")
	open := func() *Pager {
		f, err := OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Open(f, 16)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	p := open()
	tree, _ := CreateTree(p)
	m := &model{t: t, tree: tree, want: map[string][]byte{}}
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 5000; i++ {
		m.put(randomKey(rng, 3000), randomValue(rng))
	}
	for i := 0; i < 1000; i++ {
		m.delete(randomKey(rng, 3000))
	}
	p.SetCatalogRoot(tree.Root())
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p = open()
	defer p.Close()
	m.tree = OpenTree(p, p.CatalogRoot())
	m.check()
	checkNoLeaks(t, p, m.tree)
	if stats := p.Stats(); stats.Reads == 0 {
		t.Error("nothing was read from the file after reopening")
	}
}

func TestChecksumDetectsCorruption(t *testing.T) {
	file := NewMemFile()
	p, _ := Open(file, 16)
	tree, _ := CreateTree(p)
	for i := 0; i < 2000; i++ {
		tree.Put([]byte(fmt.Sprint("key", i)), []byte("value"))
	}
	root := tree.Root()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Flip one bit in the middle of a page, as a failing disk would.
	file.data[3*PageSize+1234] ^= 0x10

	p, err := Open(file, 16)
	if err != nil {
		t.Fatal(err)
	}
	c := OpenTree(p, root).Seek(nil)
	for {
		if _, _, ok := c.Next(); !ok {
			break
		}
	}
	if !errors.Is(c.Err(), ErrCorrupt) {
		t.Errorf("expected the scan to stop with ErrCorrupt, got %v", c.Err())
	}

	// The meta page is protected too.
	file.data[offPageCount] ^= 0x01
	if _, err := Open(file, 16); !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt for a damaged meta page, got %v", err)
	}
	if _, err := Open(&MemFile{data: bytes.Repeat([]byte("not a database "), 1000)}, 16); !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt for a foreign file, got %v", err)
	}
}

func TestPoolExhaustion(t *testing.T) {
	p := newPager(t, 8)
	var held []*Page
	for {
		pg, err := p.Alloc(pageLeaf)
		if err != nil {
			if !errors.Is(err, ErrPoolExhausted) {
				t.Fatalf("expected ErrPoolExhausted, got %v", err)
			}
			break
		}
		held = append(held, pg)
		if len(held) > 8 {
			t.Fatal("more pages pinned than the pool has frames")
		}
	}
	// Releasing one pin is enough to carry on.
	held[0].Unpin(true)
	pg, err := p.Alloc(pageLeaf)
	if err != nil {
		t.Fatalf("after unpinning: %v", err)
	}
	pg.Unpin(true)
	for _, h := range held[1:] {
		h.Unpin(true)
	}
	if p.Pinned() != 0 {
		t.Errorf("%d pages still pinned", p.Pinned())
	}
}

// FuzzTree turns arbitrary bytes into a sequence of tree operations and
// checks the result against a map. Run with
//
//	go test ./internal/storage -run XXX -fuzz FuzzTree -fuzztime 1m
func FuzzTree(f *testing.F) {
	f.Add([]byte("\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09"))
	f.Add(bytes.Repeat([]byte{0, 7, 200, 1, 7, 0, 2, 9, 30}, 50))
	f.Fuzz(func(t *testing.T, script []byte) {
		p := newPager(t, 8)
		tree, err := CreateTree(p)
		if err != nil {
			t.Fatal(err)
		}
		m := &model{t: t, tree: tree, want: map[string][]byte{}}
		// Each operation is three bytes: what to do, which key, how big.
		for len(script) >= 3 {
			op, k, size := script[0], script[1], int(script[2])
			script = script[3:]
			// Few distinct keys, of very different lengths, so that
			// overwrites and deletes actually hit.
			key := bytes.Repeat([]byte{k}, 1+int(k)*4)
			switch op % 4 {
			case 0, 1:
				m.put(key, bytes.Repeat([]byte{op}, size*size/4))
			case 2:
				m.delete(key)
			case 3:
				m.get(key)
			}
		}
		m.check()
		checkNoLeaks(t, p, tree)
	})
}

// The benchmarks below measure the tree through the buffer pool, on an
// in-memory file so that they time the data structure and not the disk.
// docs/benchmarks.md explains how to read them.

func benchTree(b *testing.B, pool int) *Tree {
	p, err := Open(NewMemFile(), pool)
	if err != nil {
		b.Fatal(err)
	}
	tree, err := CreateTree(p)
	if err != nil {
		b.Fatal(err)
	}
	return tree
}

func benchKey(i int) []byte { return binary.BigEndian.AppendUint64(nil, uint64(i)) }

var benchValue = bytes.Repeat([]byte("v"), 100)

func BenchmarkPutSequential(b *testing.B) {
	tree := benchTree(b, 1<<16)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tree.Put(benchKey(i), benchValue); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutRandom(b *testing.B) {
	tree := benchTree(b, 1<<16)
	rng := rand.New(rand.NewSource(1))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := tree.Put(benchKey(rng.Int()), benchValue); err != nil {
			b.Fatal(err)
		}
	}
}

// filled returns a tree holding n sequential keys.
func filled(b *testing.B, n, pool int) *Tree {
	tree := benchTree(b, pool)
	for i := 0; i < n; i++ {
		if err := tree.Put(benchKey(i), benchValue); err != nil {
			b.Fatal(err)
		}
	}
	return tree
}

func BenchmarkGet(b *testing.B) {
	const n = 1_000_000
	for _, pool := range []int{1 << 16, 256} {
		name := "in pool"
		if pool == 256 {
			name = "pool of 2 MB for 200 MB of data"
		}
		b.Run(name, func(b *testing.B) {
			tree := filled(b, n, pool)
			rng := rand.New(rand.NewSource(1))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, found, err := tree.Get(benchKey(rng.Intn(n))); err != nil || !found {
					b.Fatal(found, err)
				}
			}
		})
	}
}

func BenchmarkScan(b *testing.B) {
	const n = 1_000_000
	tree := filled(b, n, 1<<16)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := tree.Seek(nil)
		count := 0
		for {
			if _, _, ok := c.Next(); !ok {
				break
			}
			count++
		}
		if count != n {
			b.Fatal(count)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/n, "ns/row")
}

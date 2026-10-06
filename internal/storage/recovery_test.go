package storage

import (
	"bytes"
	"fmt"
	"testing"
)

// These tests stage specific situations for recovery. The randomised crash
// tests in internal/engine cover the space; these pin down the cases worth
// naming, deterministically.

type walRig struct {
	t    *testing.T
	disk *SimDisk
	p    *Pager
}

func newWALRig(t *testing.T) *walRig {
	r := &walRig{t: t, disk: NewSimDisk(1)}
	r.open()
	return r
}

func (r *walRig) open() {
	r.t.Helper()
	p, err := Open(r.disk.Open("data"), r.disk.Open("wal"), Options{PoolPages: 64})
	if err != nil {
		r.t.Fatalf("open: %v", err)
	}
	r.p = p
}

// crash loses power and reopens, which recovers.
func (r *walRig) crash() RecoveryInfo {
	r.t.Helper()
	r.disk.Crash()
	r.open()
	return r.p.Recovery()
}

// put stores a key as part of a transaction, logging its undo first, the
// way the engine does.
func (r *walRig) put(tx uint64, tree *Tree, key, val string) {
	r.t.Helper()
	r.p.LogUndo(UndoRec{Tx: tx, Root: tree.Root(), Kind: UndoDelete, Key: []byte(key)})
	if err := tree.Put([]byte(key), []byte(val)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *walRig) del(tx uint64, tree *Tree, key string) {
	r.t.Helper()
	old, found, err := tree.Get([]byte(key))
	if err != nil || !found {
		r.t.Fatalf("del %q: found=%v err=%v", key, found, err)
	}
	r.p.LogUndo(UndoRec{Tx: tx, Root: tree.Root(), Kind: UndoPut, Key: []byte(key), Val: old})
	if _, err := tree.Delete([]byte(key)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *walRig) commit(tx uint64, drops ...uint32) {
	r.t.Helper()
	if err := r.p.Commit(tx, drops); err != nil {
		r.t.Fatal(err)
	}
}

func contents(t *testing.T, tree *Tree) string {
	t.Helper()
	var sb bytes.Buffer
	c := tree.Seek(nil)
	for {
		k, v, ok := c.Next()
		if !ok {
			break
		}
		fmt.Fprintf(&sb, "%s=%s ", k, v)
	}
	if c.Err() != nil {
		t.Fatal(c.Err())
	}
	return sb.String()
}

// setup creates a tree in a committed transaction and returns its root.
func (r *walRig) setup() *Tree {
	tree, _, err := CreateTreeLogged(r.p, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	r.commit(1)
	return tree
}

func TestCommittedSurvivesUncommittedVanishes(t *testing.T) {
	r := newWALRig(t)
	tree := r.setup()

	r.put(2, tree, "a", "committed")
	r.put(2, tree, "b", "committed")
	r.commit(2)
	// Transaction 3 never commits. Its pages are forced to the data file
	// by a checkpoint, the worst case for recovery: the uncommitted
	// changes are on disk and have to be taken out again.
	r.put(3, tree, "c", "uncommitted")
	r.del(3, tree, "a")
	if err := r.p.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	r.put(3, tree, "d", "uncommitted")

	info := r.crash()
	if info.RolledBack != 1 || info.UndoApplied == 0 {
		t.Errorf("recovery: %+v", info)
	}
	tree = OpenTree(r.p, tree.Root())
	if got := contents(t, tree); got != "a=committed b=committed " {
		t.Errorf("after recovery: %q", got)
	}
	if _, err := tree.Verify(); err != nil {
		t.Fatal(err)
	}
	checkNoLeaks(t, r.p, tree)

	// Recovery ends with a checkpoint: a second crash finds nothing to do.
	if info := r.crash(); info.Records != 0 {
		t.Errorf("second recovery still found %d log records", info.Records)
	}
}

func TestUncommittedTreeIsFreed(t *testing.T) {
	r := newWALRig(t)
	keep := r.setup()
	r.put(2, keep, "k", "v")
	r.commit(2)

	// A transaction creates a tree, fills it over many pages, and dies.
	doomed, _, err := CreateTreeLogged(r.p, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := doomed.Put([]byte(fmt.Sprint("key", i)), bytes.Repeat([]byte("x"), 100)); err != nil {
			t.Fatal(err)
		}
	}
	r.p.Checkpoint()

	r.crash()
	keep = OpenTree(r.p, keep.Root())
	if got := contents(t, keep); got != "k=v " {
		t.Errorf("after recovery: %q", got)
	}
	// Every page of the abandoned tree is back on the free list.
	checkNoLeaks(t, r.p, keep)
}

func TestCommittedDropIsFinished(t *testing.T) {
	r := newWALRig(t)
	keep := r.setup()
	victim, _, _ := CreateTreeLogged(r.p, 2)
	for i := 0; i < 2000; i++ {
		victim.Put([]byte(fmt.Sprint("key", i)), bytes.Repeat([]byte("x"), 100))
	}
	r.commit(2)
	r.p.Checkpoint()

	// The commit record that drops the tree becomes durable, and the power
	// goes before the tree is actually freed.
	r.p.mu.Lock()
	r.p.active[3] = true
	r.p.appendLog(recCommit, append([]byte{0, 0, 0, 0, 0, 0, 0, 3}, byte(victim.Root()>>24), byte(victim.Root()>>16), byte(victim.Root()>>8), byte(victim.Root())))
	r.p.wal.flush()
	r.p.mu.Unlock()

	info := r.crash()
	if info.Completed != 1 {
		t.Errorf("recovery: %+v", info)
	}
	checkNoLeaks(t, r.p, OpenTree(r.p, keep.Root()))
	if free, _ := r.p.FreePages(); len(free) < 50 {
		t.Errorf("the dropped tree was not freed: %d free pages", len(free))
	}
}

func TestTornLogTailIsIgnored(t *testing.T) {
	r := newWALRig(t)
	tree := r.setup()
	r.put(2, tree, "a", "1")
	r.commit(2)

	// The end of the log is whatever a half-finished write left behind.
	r.disk.Crash()
	r.disk.Append("wal", bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 300))
	r.open()

	tree = OpenTree(r.p, tree.Root())
	if got := contents(t, tree); got != "a=1 " {
		t.Errorf("after recovery: %q", got)
	}
	// The log is usable again: new work goes after the valid part.
	r.put(3, tree, "b", "2")
	r.commit(3)
	r.crash()
	if got := contents(t, OpenTree(r.p, tree.Root())); got != "a=1 b=2 " {
		t.Errorf("after second recovery: %q", got)
	}
}

func TestTornPageIsRebuiltFromTheLog(t *testing.T) {
	r := newWALRig(t)
	tree := r.setup()
	r.p.Checkpoint()
	for i := 0; i < 500; i++ {
		r.put(2, tree, fmt.Sprint("key", i), "value")
	}
	r.commit(2)
	want := contents(t, tree)

	// The pages reach the data file, but one of them only in part: its
	// checksum will not match.
	r.p.mu.Lock()
	for i := range r.p.frames {
		if r.p.frames[i].loaded && r.p.frames[i].dirty {
			r.p.writeFrame(i)
		}
	}
	r.p.mu.Unlock()
	r.disk.Open("data").Sync()
	r.disk.Crash()
	r.disk.Damage("data", int(tree.Root())*PageSize+3000, 2000)
	r.open()

	if info := r.p.Recovery(); info.TornPages == 0 {
		t.Errorf("recovery did not notice the torn page: %+v", info)
	}
	tree = OpenTree(r.p, tree.Root())
	if got := contents(t, tree); got != want {
		t.Error("the tree was not restored from the page image in the log")
	}
	if _, err := tree.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestTornMetaPageIsRebuiltFromTheLog(t *testing.T) {
	r := newWALRig(t)
	tree := r.setup()
	if err := r.p.SetCatalogRoot(tree.Root()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ { // enough to grow the file well past what the meta page says
		r.put(2, tree, fmt.Sprint("key", i), "value")
	}
	r.commit(2)
	want := contents(t, tree)

	r.disk.Crash()
	r.disk.Damage("data", 100, 500)
	r.open()

	if !r.p.Recovery().MetaRebuilt {
		t.Error("recovery did not report rebuilding the meta page")
	}
	if r.p.CatalogRoot() != tree.Root() {
		t.Errorf("catalog root: %d, want %d", r.p.CatalogRoot(), tree.Root())
	}
	tree = OpenTree(r.p, tree.Root())
	if got := contents(t, tree); got != want {
		t.Error("contents differ after rebuilding the meta page")
	}
	checkNoLeaks(t, r.p, tree)
}

func TestLogIsDiscardedOnlyWhenNoTransactionIsOpen(t *testing.T) {
	r := newWALRig(t)
	tree := r.setup()
	r.put(2, tree, "a", "1")
	// Transaction 2 is still open: a checkpoint must keep its undo records.
	r.p.Checkpoint()
	if r.p.LogSize() <= int64(len(walMagic)) {
		t.Fatal("the log was discarded while a transaction was open")
	}
	r.commit(2)
	r.p.Checkpoint()
	if size := r.p.LogSize(); size != int64(len(walMagic)) {
		t.Errorf("log size after a checkpoint with nothing open: %d", size)
	}
	// LSNs keep growing across the reset; a page's LSN never goes back.
	before := r.p.wal.next
	r.put(3, tree, "b", "2")
	if r.p.wal.next <= before {
		t.Error("LSNs did not advance")
	}
	r.commit(3)
	r.crash()
	if got := contents(t, OpenTree(r.p, tree.Root())); got != "a=1 b=2 " {
		t.Errorf("after recovery: %q", got)
	}
}

func TestBrokenAfterFailedWrite(t *testing.T) {
	r := newWALRig(t)
	tree := r.setup()
	r.put(2, tree, "a", "1")
	r.commit(2)

	// The disk dies. Whatever fails first, the pager must end up refusing
	// further writes rather than carry on with memory and log out of step.
	r.disk.CrashAfter(0)
	var err error
	for i := 0; i < 5000 && err == nil; i++ {
		err = tree.Put([]byte(fmt.Sprint("key", i)), bytes.Repeat([]byte("v"), 500))
		if err == nil {
			err = r.p.Commit(uint64(10+i), nil)
		}
	}
	if err == nil {
		t.Fatal("writes kept succeeding on a dead disk")
	}
	if cerr := r.p.Checkpoint(); cerr == nil {
		t.Error("a checkpoint succeeded on a dead disk")
	}
	r.crash()
	if got := contents(t, OpenTree(r.p, tree.Root())); len(got) < 4 || got[:4] != "a=1 " {
		t.Errorf("after recovery: %.40q", got)
	}
}

// TestUndoIsNotRepeatedByRecovery pins down the case that makes "this undo
// has been carried out" worth writing in the log.
//
// A transaction creates a tree and then takes the creation back — a
// statement being rolled back, or a recovery that got this far before
// being interrupted. The tree's pages are freed, but the transaction has
// no END record yet. Someone else's committed work then reuses the freed
// pages. If the server crashes now, the transaction is a loser and
// recovery walks its undo records, including "free the tree it created".
// Were that not marked as already done, recovery would free pages that by
// now belong to another, committed, tree.
func TestUndoIsNotRepeatedByRecovery(t *testing.T) {
	r := newWALRig(t)
	keep := r.setup()

	doomed, lsn, err := CreateTreeLogged(r.p, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if err := doomed.Put([]byte(fmt.Sprint("key", i)), bytes.Repeat([]byte("x"), 100)); err != nil {
			t.Fatal(err)
		}
	}
	// The creation is undone, and nothing says the transaction is over.
	if err := r.p.ApplyUndo(UndoRec{Tx: 2, Root: doomed.Root(), Kind: UndoDropTree}, lsn); err != nil {
		t.Fatal(err)
	}

	// A committed transaction fills the freed pages with its own data.
	for i := 0; i < 500; i++ {
		r.put(3, keep, fmt.Sprint("kept", i), "value")
	}
	r.commit(3)
	want := contents(t, keep)

	if info := r.crash(); info.RolledBack != 1 {
		t.Errorf("recovery: %+v", info)
	}
	keep = OpenTree(r.p, keep.Root())
	if _, err := keep.Verify(); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if got := contents(t, keep); got != want {
		t.Error("committed data changed during recovery")
	}
	checkNoLeaks(t, r.p, keep)
}

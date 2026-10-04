package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrKeyTooLarge is returned by Put for a key longer than MaxKeySize.
var ErrKeyTooLarge = errors.New("storage: key exceeds the maximum size")

// ErrValueTooLarge is returned by Put for a value whose overflow pages would
// not fit in half the buffer pool.
var ErrValueTooLarge = errors.New("storage: value too large for the buffer pool")

// maxDepth is far more than any tree can reach (a tree of depth 8 already
// addresses more pages than a file can have); exceeding it means the page
// links form a cycle.
const maxTreeDepth = 32

// Tree is a B+tree mapping byte-string keys to byte-string values, ordered
// by bytes.Compare. All values live in the leaves, which are chained in key
// order so that a range scan never has to climb back up.
//
// The root page never changes: when the root splits, its contents move to a
// new page and the root becomes their parent. Whoever stores the root's page
// ID (the catalog) therefore never has to update it.
//
// Deletion does not rebalance. A leaf is removed only once it is empty,
// which is also what PostgreSQL does: merging half-empty pages costs
// writes and rarely pays off, since tables that shrink usually grow again.
//
// A Tree is not safe for concurrent modification; see Pager.
type Tree struct {
	p    *Pager
	root uint32
}

// CreateTree allocates an empty tree. If tx is not zero the creation belongs
// to that transaction: the log notes it, and recovery frees the tree again
// should the transaction not commit.
func CreateTree(p *Pager, tx uint64) (t *Tree, err error) {
	if err = p.beginWrite(); err != nil {
		return nil, err
	}
	defer p.endWrite(&err)
	pg, err := p.Alloc(pageLeaf)
	if err != nil {
		return nil, err
	}
	initNode(pg.Data, pageLeaf)
	pg.Unpin(true)
	if tx != 0 {
		p.mu.Lock()
		p.batch.note = note{kind: noteCreated, tx: tx, arg: uint64(pg.ID)}
		p.mu.Unlock()
	}
	return &Tree{p: p, root: pg.ID}, nil
}

// OpenTree returns the tree rooted at the given page.
func OpenTree(p *Pager, root uint32) *Tree { return &Tree{p: p, root: root} }

// Root returns the tree's root page, which identifies it for OpenTree.
func (t *Tree) Root() uint32 { return t.root }

// step records which child of an internal page a descent took.
type step struct {
	id  uint32
	idx int
}

func (t *Tree) fetchNode(id uint32) (*Page, node, error) {
	pg, err := t.p.Fetch(id)
	if err != nil {
		return nil, nil, err
	}
	if typ := pg.Data[offType]; typ != pageLeaf && typ != pageInternal {
		pg.Unpin(false)
		return nil, nil, fmt.Errorf("%w: page %d has type %d where a B+tree page was expected", ErrCorrupt, id, typ)
	}
	return pg, node(pg.Data), nil
}

// descend walks from the root to the leaf that holds, or would hold, key.
// The leaf is returned pinned; the internal pages on the way are not.
func (t *Tree) descend(key []byte) ([]step, *Page, error) {
	var path []step
	id := t.root
	for {
		// Internal pages are only read on the way down, so they are
		// fetched without the bookkeeping a modification would need;
		// the leaf, which the caller may change, gets it.
		pg, err := t.p.FetchUntracked(id)
		if err != nil {
			return nil, nil, err
		}
		n := node(pg.Data)
		if typ := pg.Data[offType]; typ != pageLeaf && typ != pageInternal {
			pg.Unpin(false)
			return nil, nil, fmt.Errorf("%w: page %d has type %d where a B+tree page was expected", ErrCorrupt, id, typ)
		}
		if n.isLeaf() {
			pg.Track()
			return path, pg, nil
		}
		if len(path) >= maxTreeDepth {
			pg.Unpin(false)
			return nil, nil, fmt.Errorf("%w: B+tree rooted at page %d is deeper than %d levels", ErrCorrupt, t.root, maxTreeDepth)
		}
		i := n.childIndex(key)
		path = append(path, step{pg.ID, i})
		id = n.child(i)
		pg.Unpin(false)
	}
}

// Get returns a copy of the value stored under key.
func (t *Tree) Get(key []byte) ([]byte, bool, error) {
	_, leaf, err := t.descend(key)
	if err != nil {
		return nil, false, err
	}
	defer leaf.Unpin(false)
	n := node(leaf.Data)
	i, found := n.search(key)
	if !found {
		return nil, false, nil
	}
	val, err := t.value(n, i)
	return val, err == nil, err
}

// value returns a copy of the value of leaf cell i, following its overflow
// chain if it has one.
func (t *Tree) value(n node, i int) ([]byte, error) {
	off := n.slot(i)
	klen := int(binary.BigEndian.Uint16(n[off:]))
	vlen := binary.BigEndian.Uint32(n[off+2:])
	body := n[off+cellHdrSize+klen:]
	if vlen&overflowFlag == 0 {
		return append([]byte(nil), body[:vlen]...), nil
	}
	size := int(vlen &^ overflowFlag)
	out := make([]byte, 0, size)
	next := binary.BigEndian.Uint32(body)
	for next != 0 {
		pg, err := t.p.Fetch(next)
		if err != nil {
			return nil, err
		}
		chunk := int(binary.BigEndian.Uint16(pg.Data[offUpper:]))
		if pg.Data[offType] != pageOverflow || chunk > PageSize-slotBase || len(out)+chunk > size {
			pg.Unpin(false)
			return nil, fmt.Errorf("%w: bad overflow page %d", ErrCorrupt, next)
		}
		out = append(out, pg.Data[slotBase:slotBase+chunk]...)
		next = binary.BigEndian.Uint32(pg.Data[offLinkA:])
		pg.Unpin(false)
	}
	if len(out) != size {
		return nil, fmt.Errorf("%w: overflow chain holds %d bytes, expected %d", ErrCorrupt, len(out), size)
	}
	return out, nil
}

// overflowOf returns the first overflow page of leaf cell i, or 0.
func overflowOf(n node, i int) uint32 {
	off := n.slot(i)
	klen := int(binary.BigEndian.Uint16(n[off:]))
	if binary.BigEndian.Uint32(n[off+2:])&overflowFlag == 0 {
		return 0
	}
	return binary.BigEndian.Uint32(n[off+cellHdrSize+klen:])
}

// writeOverflow stores val in a chain of overflow pages and returns the
// first one.
func (t *Tree) writeOverflow(val []byte) (uint32, error) {
	const capacity = PageSize - slotBase
	var first uint32
	var prev *Page
	for len(val) > 0 {
		pg, err := t.p.Alloc(pageOverflow)
		if err != nil {
			if prev != nil {
				prev.Unpin(true)
			}
			return 0, err
		}
		chunk := min(len(val), capacity)
		binary.BigEndian.PutUint16(pg.Data[offUpper:], uint16(chunk))
		copy(pg.Data[slotBase:], val[:chunk])
		val = val[chunk:]
		if prev == nil {
			first = pg.ID
		} else {
			binary.BigEndian.PutUint32(prev.Data[offLinkA:], pg.ID)
			prev.Unpin(true)
		}
		prev = pg
	}
	if prev != nil {
		prev.Unpin(true)
	}
	return first, nil
}

func (t *Tree) freeOverflow(first uint32) error {
	for steps := 0; first != 0; steps++ {
		if steps > t.p.PageCount() {
			return fmt.Errorf("%w: overflow chain has a cycle", ErrCorrupt)
		}
		pg, err := t.p.Fetch(first)
		if err != nil {
			return err
		}
		id := first
		first = binary.BigEndian.Uint32(pg.Data[offLinkA:])
		pg.Unpin(false)
		if err := t.p.Free(id); err != nil {
			return err
		}
	}
	return nil
}

// leafCell builds the cell for key and val, moving val to overflow pages if
// the cell would be too large to keep four of them on a page.
func (t *Tree) leafCell(key, val []byte) ([]byte, error) {
	inline := cellHdrSize+len(key)+len(val) <= maxInlineCell
	size := cellHdrSize + len(key) + 4
	if inline {
		size = cellHdrSize + len(key) + len(val)
	}
	cell := make([]byte, size)
	binary.BigEndian.PutUint16(cell, uint16(len(key)))
	copy(cell[cellHdrSize:], key)
	if inline {
		binary.BigEndian.PutUint32(cell[2:], uint32(len(val)))
		copy(cell[cellHdrSize+len(key):], val)
		return cell, nil
	}
	first, err := t.writeOverflow(val)
	if err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint32(cell[2:], uint32(len(val))|overflowFlag)
	binary.BigEndian.PutUint32(cell[cellHdrSize+len(key):], first)
	return cell, nil
}

// Put stores val under key, replacing any existing value.
func (t *Tree) Put(key, val []byte) (err error) {
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}
	if len(val) >= overflowFlag {
		return fmt.Errorf("storage: value of %d bytes is too large", len(val))
	}
	// A large value's overflow pages are all modified by this one
	// operation, and pages modified by an operation in progress cannot
	// leave the buffer pool.
	if pages := len(val)/(PageSize-slotBase) + 1; pages > t.p.PoolPages()/2 {
		return ErrValueTooLarge
	}
	if err = t.p.beginWrite(); err != nil {
		return err
	}
	defer t.p.endWrite(&err)
	path, leaf, err := t.descend(key)
	if err != nil {
		return err
	}
	n := node(leaf.Data)
	i, found := n.search(key)
	if found {
		if err := t.freeOverflow(overflowOf(n, i)); err != nil {
			leaf.Unpin(true)
			return err
		}
		n.remove(i)
	}
	cell, err := t.leafCell(key, val)
	if err != nil {
		leaf.Unpin(true)
		return err
	}
	if n.insert(i, cell) {
		leaf.Unpin(true)
		return nil
	}

	// The leaf is full: split it. The new cell takes its place among the
	// existing ones first, so that the split point accounts for it.
	cells := n.cells()
	cells = append(cells, nil)
	copy(cells[i+1:], cells[i:])
	cells[i] = cell
	return t.splitLeaf(path, leaf, cells)
}

// splitLeaf distributes cells over the (pinned) leaf and a new right
// sibling, then tells the parent.
func (t *Tree) splitLeaf(path []step, leaf *Page, cells [][]byte) error {
	m := splitPoint(cells)
	// The separator is the smallest key of the right half: everything in
	// the left page sorts before it.
	sep := append([]byte(nil), cellKey(cells[m])...)

	if leaf.ID == t.root {
		// The root keeps its page. Its cells move to two new leaves and it
		// becomes an internal page above them.
		left, err := t.p.Alloc(pageLeaf)
		if err != nil {
			leaf.Unpin(true)
			return err
		}
		right, err := t.p.Alloc(pageLeaf)
		if err != nil {
			left.Unpin(true)
			leaf.Unpin(true)
			return err
		}
		ln, rn := initNode(left.Data, pageLeaf), initNode(right.Data, pageLeaf)
		ln.setCells(cells[:m])
		rn.setCells(cells[m:])
		ln.setLinkA(right.ID)
		rn.setLinkB(left.ID)
		root := initNode(leaf.Data, pageInternal)
		root.setLinkA(right.ID)
		root.insert(0, internalCell(sep, left.ID))
		left.Unpin(true)
		right.Unpin(true)
		leaf.Unpin(true)
		return nil
	}

	right, err := t.p.Alloc(pageLeaf)
	if err != nil {
		leaf.Unpin(true)
		return err
	}
	ln, rn := node(leaf.Data), initNode(right.Data, pageLeaf)
	oldNext := ln.linkA()
	ln.setCells(cells[:m])
	rn.setCells(cells[m:])
	// Splice the new leaf into the sibling chain.
	rn.setLinkA(oldNext)
	rn.setLinkB(leaf.ID)
	ln.setLinkA(right.ID)
	leftID, rightID := leaf.ID, right.ID
	leaf.Unpin(true)
	right.Unpin(true)
	if oldNext != 0 {
		pg, err := t.p.Fetch(oldNext)
		if err != nil {
			return err
		}
		node(pg.Data).setLinkB(rightID)
		pg.Unpin(true)
	}
	return t.insertIntoParent(path, leftID, sep, rightID)
}

// insertIntoParent records that the page left has been split: keys below
// sep stay in left, the rest are in right. The parent is the last step of
// path; if it is full it splits in turn, possibly all the way to the root.
func (t *Tree) insertIntoParent(path []step, left uint32, sep []byte, right uint32) error {
	last := path[len(path)-1]
	path = path[:len(path)-1]
	pg, n, err := t.fetchNode(last.id)
	if err != nil {
		return err
	}
	// The pointer that led to left now leads to right, and a new cell
	// (sep -> left) goes in front of it.
	n.setChild(last.idx, right)
	cell := internalCell(sep, left)
	if n.insert(last.idx, cell) {
		pg.Unpin(true)
		return nil
	}

	cells := n.cells()
	cells = append(cells, nil)
	copy(cells[last.idx+1:], cells[last.idx:])
	cells[last.idx] = cell
	rightmost := n.linkA()

	// The middle cell is promoted rather than copied: its key becomes the
	// separator in the parent and its child the rightmost of the left half.
	m := splitPoint(cells)
	promoted := append([]byte(nil), cellKey(cells[m])...)
	midChild := binary.BigEndian.Uint32(cells[m][2:])

	if pg.ID == t.root {
		l, err := t.p.Alloc(pageInternal)
		if err != nil {
			pg.Unpin(true)
			return err
		}
		r, err := t.p.Alloc(pageInternal)
		if err != nil {
			l.Unpin(true)
			pg.Unpin(true)
			return err
		}
		ln, rn := initNode(l.Data, pageInternal), initNode(r.Data, pageInternal)
		ln.setCells(cells[:m])
		ln.setLinkA(midChild)
		rn.setCells(cells[m+1:])
		rn.setLinkA(rightmost)
		root := initNode(pg.Data, pageInternal)
		root.setLinkA(r.ID)
		root.insert(0, internalCell(promoted, l.ID))
		l.Unpin(true)
		r.Unpin(true)
		pg.Unpin(true)
		return nil
	}

	r, err := t.p.Alloc(pageInternal)
	if err != nil {
		pg.Unpin(true)
		return err
	}
	rn := initNode(r.Data, pageInternal)
	n.setCells(cells[:m])
	n.setLinkA(midChild)
	rn.setCells(cells[m+1:])
	rn.setLinkA(rightmost)
	leftID, rightID := pg.ID, r.ID
	pg.Unpin(true)
	r.Unpin(true)
	return t.insertIntoParent(path, leftID, promoted, rightID)
}

// Delete removes key and reports whether it was present.
func (t *Tree) Delete(key []byte) (found bool, err error) {
	if err = t.p.beginWrite(); err != nil {
		return false, err
	}
	defer t.p.endWrite(&err)
	path, leaf, err := t.descend(key)
	if err != nil {
		return false, err
	}
	n := node(leaf.Data)
	i, found := n.search(key)
	if !found {
		leaf.Unpin(false)
		return false, nil
	}
	if err := t.freeOverflow(overflowOf(n, i)); err != nil {
		leaf.Unpin(true)
		return false, err
	}
	n.remove(i)
	if n.count() > 0 || leaf.ID == t.root {
		leaf.Unpin(true)
		return true, nil
	}

	// The leaf is empty: take it out of the sibling chain, free it and
	// remove the pointer to it from its parent.
	next, prev, id := n.linkA(), n.linkB(), leaf.ID
	leaf.Unpin(true)
	if prev != 0 {
		pg, err := t.p.Fetch(prev)
		if err != nil {
			return true, err
		}
		node(pg.Data).setLinkA(next)
		pg.Unpin(true)
	}
	if next != 0 {
		pg, err := t.p.Fetch(next)
		if err != nil {
			return true, err
		}
		node(pg.Data).setLinkB(prev)
		pg.Unpin(true)
	}
	if err := t.p.Free(id); err != nil {
		return true, err
	}
	return true, t.removeChild(path)
}

// removeChild deletes from the last page of path the pointer the descent
// took, the page it led to having been freed.
func (t *Tree) removeChild(path []step) error {
	for {
		last := path[len(path)-1]
		path = path[:len(path)-1]
		pg, n, err := t.fetchNode(last.id)
		if err != nil {
			return err
		}
		count := n.count()
		if count == 0 {
			// That was this page's only child, so it is now empty too.
			if pg.ID == t.root {
				initNode(pg.Data, pageLeaf)
				pg.Unpin(true)
				return nil
			}
			id := pg.ID
			pg.Unpin(false)
			if err := t.p.Free(id); err != nil {
				return err
			}
			continue // and remove it from its own parent
		}
		if last.idx < count {
			// Dropping cell idx drops its separator with it: the keys it
			// used to route now fall through to the next child, which is
			// right, since there are none left.
			n.remove(last.idx)
		} else {
			n.setLinkA(n.child(count - 1))
			n.remove(count - 1)
		}
		// A root left with a single child is replaced by that child, which
		// is how the tree gets shorter.
		for pg.ID == t.root && !n.isLeaf() && n.count() == 0 {
			childID := n.linkA()
			child, err := t.p.Fetch(childID)
			if err != nil {
				pg.Unpin(true)
				return err
			}
			copy(pg.Data, child.Data)
			child.Unpin(false)
			if err := t.p.Free(childID); err != nil {
				pg.Unpin(true)
				return err
			}
		}
		pg.Unpin(true)
		return nil
	}
}

// LastKey returns a copy of the greatest key, or nil if the tree is empty.
func (t *Tree) LastKey() ([]byte, error) {
	id := t.root
	for depth := 0; ; depth++ {
		pg, n, err := t.fetchNode(id)
		if err != nil {
			return nil, err
		}
		if n.isLeaf() {
			var key []byte
			if c := n.count(); c > 0 {
				key = append([]byte(nil), n.key(c-1)...)
			}
			pg.Unpin(false)
			return key, nil
		}
		id = n.linkA()
		pg.Unpin(false)
		if depth >= maxTreeDepth {
			return nil, fmt.Errorf("%w: B+tree rooted at page %d is deeper than %d levels", ErrCorrupt, t.root, maxTreeDepth)
		}
	}
}

// Cursor iterates over entries in key order. It reads one leaf at a time
// into memory and holds no page pinned between calls, so it stays valid
// (though not necessarily current) if the tree changes underneath.
type Cursor struct {
	t    *Tree
	keys [][]byte
	vals [][]byte
	i    int
	next uint32
	err  error
}

// Seek returns a cursor positioned at the first key >= key. A nil key
// starts from the beginning.
func (t *Tree) Seek(key []byte) *Cursor {
	c := &Cursor{t: t}
	_, leaf, err := t.descend(key)
	if err != nil {
		c.err = err
		return c
	}
	n := node(leaf.Data)
	i, _ := n.search(key)
	c.load(n, i)
	leaf.Unpin(false)
	return c
}

func (c *Cursor) load(n node, from int) {
	c.keys, c.vals, c.i = c.keys[:0], c.vals[:0], 0
	c.next = n.linkA()
	for i := from; i < n.count(); i++ {
		val, err := c.t.value(n, i)
		if err != nil {
			c.err = err
			return
		}
		c.keys = append(c.keys, append([]byte(nil), n.key(i)...))
		c.vals = append(c.vals, val)
	}
}

// Next returns the next entry, or ok == false at the end or on error (check
// Err). The returned slices are the caller's to keep.
func (c *Cursor) Next() (key, val []byte, ok bool) {
	for c.err == nil && c.i >= len(c.keys) {
		if c.next == 0 {
			return nil, nil, false
		}
		pg, n, err := c.t.fetchNode(c.next)
		if err != nil {
			c.err = err
			break
		}
		c.load(n, 0)
		pg.Unpin(false)
	}
	if c.err != nil {
		return nil, nil, false
	}
	c.i++
	return c.keys[c.i-1], c.vals[c.i-1], true
}

// Err returns the error that stopped the iteration, if any.
func (c *Cursor) Err() error { return c.err }

// Drop frees every page of the tree. The Tree must not be used afterwards.
func (t *Tree) Drop() (err error) {
	if err = t.p.beginWrite(); err != nil {
		return err
	}
	defer t.p.endWrite(&err)
	return t.dropPage(t.root, 0)
}

func (t *Tree) dropPage(id uint32, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%w: B+tree rooted at page %d is deeper than %d levels", ErrCorrupt, t.root, maxTreeDepth)
	}
	pg, n, err := t.fetchNode(id)
	if err != nil {
		return err
	}
	var children, overflows []uint32
	if n.isLeaf() {
		for i := 0; i < n.count(); i++ {
			if first := overflowOf(n, i); first != 0 {
				overflows = append(overflows, first)
			}
		}
	} else {
		for i := 0; i <= n.count(); i++ {
			children = append(children, n.child(i))
		}
	}
	pg.Unpin(false)
	for _, first := range overflows {
		if err := t.freeOverflow(first); err != nil {
			return err
		}
	}
	for _, child := range children {
		if err := t.dropPage(child, depth+1); err != nil {
			return err
		}
	}
	return t.p.Free(id)
}

// CreateTreeLogged is CreateTree for a transaction, also returning the LSN
// of the log record that notes the creation. Undoing it is an UndoDropTree
// applied at that LSN.
func CreateTreeLogged(p *Pager, tx uint64) (*Tree, uint64, error) {
	t, err := CreateTree(p, tx)
	if err != nil {
		return nil, 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return t, p.lastLSN, nil
}

package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// TreeInfo is what Verify learned about a tree.
type TreeInfo struct {
	Entries int
	Depth   int // 1 for a tree that is a single leaf
	Leaves  int
	// Pages lists every page the tree owns: internal, leaf and overflow.
	Pages []uint32
}

// Verify checks every structural invariant of the tree and returns an error
// describing the first violation:
//
//   - each page is a well-formed slotted page: slots and cells inside the
//     page, cells not overlapping the slot array;
//   - keys within a page are strictly increasing;
//   - every key lies within the bounds its ancestors' separators promise;
//   - all leaves are at the same depth;
//   - no leaf other than a lone root is empty;
//   - the sibling chain visits exactly the leaves, in key order, and the
//     backward links mirror the forward ones;
//   - overflow chains hold exactly the bytes their cell claims;
//   - no page is reachable twice.
//
// It is what the randomised tests and the fuzzer run after every batch of
// operations, and is cheap enough to run on a real database as a check.
func (t *Tree) Verify() (TreeInfo, error) {
	v := &verifier{t: t, seen: make(map[uint32]bool)}
	if err := v.walk(t.root, nil, nil, 1); err != nil {
		return TreeInfo{}, err
	}
	// Follow the sibling chain from the leftmost leaf.
	var prev uint32
	id := v.leaves[0]
	for i := 0; ; i++ {
		if i >= len(v.leaves) || v.leaves[i] != id {
			return TreeInfo{}, fmt.Errorf("%w: leaf chain reaches page %d out of order", ErrCorrupt, id)
		}
		pg, n, err := t.fetchNode(id)
		if err != nil {
			return TreeInfo{}, err
		}
		next, back := n.linkA(), n.linkB()
		pg.Unpin(false)
		if back != prev {
			return TreeInfo{}, fmt.Errorf("%w: leaf %d links back to %d, expected %d", ErrCorrupt, id, back, prev)
		}
		if next == 0 {
			if i != len(v.leaves)-1 {
				return TreeInfo{}, fmt.Errorf("%w: leaf chain ends at page %d before the last leaf", ErrCorrupt, id)
			}
			break
		}
		prev, id = id, next
	}
	return v.info, nil
}

type verifier struct {
	t      *Tree
	seen   map[uint32]bool
	leaves []uint32
	info   TreeInfo
}

func (v *verifier) claim(id uint32) error {
	if v.seen[id] {
		return fmt.Errorf("%w: page %d is reachable twice", ErrCorrupt, id)
	}
	v.seen[id] = true
	v.info.Pages = append(v.info.Pages, id)
	return nil
}

func (v *verifier) walk(id uint32, lo, hi []byte, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%w: tree deeper than %d levels", ErrCorrupt, maxTreeDepth)
	}
	if err := v.claim(id); err != nil {
		return err
	}
	pg, n, err := v.t.fetchNode(id)
	if err != nil {
		return err
	}
	defer func() {
		if pg != nil {
			pg.Unpin(false)
		}
	}()
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: page %d: %s", ErrCorrupt, id, fmt.Sprintf(format, args...))
	}

	count := n.count()
	slotsEnd := slotBase + 2*count
	if slotsEnd > PageSize || n.upper() < slotsEnd || n.upper() > PageSize {
		return bad("%d cells and cell area at %d do not fit the page", count, n.upper())
	}
	for i := 0; i < count; i++ {
		off := n.slot(i)
		if off < n.upper() || off+cellHdrSize > PageSize || off+n.cellSizeAt(off) > PageSize {
			return bad("cell %d at offset %d is outside the cell area", i, off)
		}
		key := n.key(i)
		if i > 0 && bytes.Compare(n.key(i-1), key) >= 0 {
			return bad("keys %d and %d are not in increasing order", i-1, i)
		}
		if lo != nil && bytes.Compare(key, lo) < 0 {
			return bad("key %d is below the lower bound set by its ancestors", i)
		}
		if hi != nil && bytes.Compare(key, hi) >= 0 {
			return bad("key %d is not below the upper bound set by its ancestors", i)
		}
	}

	if n.isLeaf() {
		if v.info.Depth == 0 {
			v.info.Depth = depth
		} else if v.info.Depth != depth {
			return bad("leaf at depth %d, others at %d", depth, v.info.Depth)
		}
		if count == 0 && id != v.t.root {
			return bad("empty leaf that is not the root")
		}
		v.leaves = append(v.leaves, id)
		v.info.Leaves++
		v.info.Entries += count
		for i := 0; i < count; i++ {
			if err := v.overflow(n, i); err != nil {
				return err
			}
		}
		return nil
	}

	// Copy what is needed and release the page before recursing, so that
	// verifying a deep tree does not need a pin per level.
	type edge struct {
		child  uint32
		lo, hi []byte
	}
	edges := make([]edge, count+1)
	for i := 0; i <= count; i++ {
		e := edge{child: n.child(i), lo: lo, hi: hi}
		if i > 0 {
			e.lo = append([]byte(nil), n.key(i-1)...)
		}
		if i < count {
			e.hi = append([]byte(nil), n.key(i)...)
		}
		edges[i] = e
	}
	pg.Unpin(false)
	pg = nil
	for _, e := range edges {
		if err := v.walk(e.child, e.lo, e.hi, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (v *verifier) overflow(n node, i int) error {
	next := overflowOf(n, i)
	if next == 0 {
		return nil
	}
	off := n.slot(i)
	want := int(binary.BigEndian.Uint32(n[off+2:]) &^ overflowFlag)
	got := 0
	for next != 0 {
		if err := v.claim(next); err != nil {
			return err
		}
		pg, err := v.t.p.Fetch(next)
		if err != nil {
			return err
		}
		typ := pg.Data[offType]
		chunk := int(binary.BigEndian.Uint16(pg.Data[offUpper:]))
		id := next
		next = binary.BigEndian.Uint32(pg.Data[offLinkA:])
		pg.Unpin(false)
		if typ != pageOverflow || chunk == 0 || chunk > PageSize-slotBase {
			return fmt.Errorf("%w: page %d is not a valid overflow page", ErrCorrupt, id)
		}
		got += chunk
	}
	if got != want {
		return fmt.Errorf("%w: overflow chain holds %d bytes, its cell says %d", ErrCorrupt, got, want)
	}
	return nil
}

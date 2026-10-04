package storage

import (
	"bytes"
	"encoding/binary"
	"sort"
)

// B+tree pages are slotted pages. After the common header:
//
//	16  link A   uint32  leaf: next leaf        internal: rightmost child
//	20  link B   uint32  leaf: previous leaf    internal: unused
//	24  upper    uint16  offset where the cell area begins
//	26  slots    uint16 each: the offset of cell i, in key order
//	    ...free space...
//	    cells, growing down from the end of the page
//
// The slot array is what makes the page "slotted": cells are never moved to
// keep them sorted, only their two-byte offsets are. Deleting a cell leaves
// a hole in the cell area that is reclaimed by compacting the page when the
// space is needed.
//
// A leaf cell is      klen uint16 | vlen uint32 | key | value
// An internal cell is klen uint16 | child uint32 | key
//
// In an internal page, the child of cell i holds the keys smaller than that
// cell's key (and not smaller than the previous cell's); the rightmost child
// holds the keys greater than or equal to the last key.
const (
	offLinkA    = 16
	offLinkB    = 20
	offUpper    = 24
	slotBase    = 26
	cellHdrSize = 6

	// overflowFlag in a leaf cell's vlen says the value did not fit: the
	// cell then holds the first page of an overflow chain, and the low 31
	// bits are the value's real length.
	overflowFlag = 1 << 31

	// MaxKeySize bounds a key so that an internal page always fits several.
	MaxKeySize = 1024
	// maxInlineCell bounds a leaf cell so that a page always fits at least
	// four, which is what guarantees that a split leaves both halves with
	// room. Larger values go to overflow pages.
	maxInlineCell = (PageSize-slotBase)/4 - 2
)

// node wraps the bytes of a B+tree page.
type node []byte

func initNode(data []byte, typ byte) node {
	clear(data)
	data[offType] = typ
	binary.BigEndian.PutUint16(data[offUpper:], PageSize)
	return node(data)
}

func (n node) isLeaf() bool      { return n[offType] == pageLeaf }
func (n node) count() int        { return int(binary.BigEndian.Uint16(n[offCells:])) }
func (n node) setCount(c int)    { binary.BigEndian.PutUint16(n[offCells:], uint16(c)) }
func (n node) upper() int        { return int(binary.BigEndian.Uint16(n[offUpper:])) }
func (n node) setUpper(u int)    { binary.BigEndian.PutUint16(n[offUpper:], uint16(u)) }
func (n node) linkA() uint32     { return binary.BigEndian.Uint32(n[offLinkA:]) }
func (n node) setLinkA(v uint32) { binary.BigEndian.PutUint32(n[offLinkA:], v) }
func (n node) linkB() uint32     { return binary.BigEndian.Uint32(n[offLinkB:]) }
func (n node) setLinkB(v uint32) { binary.BigEndian.PutUint32(n[offLinkB:], v) }

func (n node) slot(i int) int {
	return int(binary.BigEndian.Uint16(n[slotBase+2*i:]))
}

// cell returns the bytes of cell i.
func (n node) cell(i int) []byte {
	off := n.slot(i)
	return n[off : off+n.cellSizeAt(off)]
}

func (n node) cellSizeAt(off int) int {
	klen := int(binary.BigEndian.Uint16(n[off:]))
	if !n.isLeaf() {
		return cellHdrSize + klen
	}
	vlen := binary.BigEndian.Uint32(n[off+2:])
	if vlen&overflowFlag != 0 {
		return cellHdrSize + klen + 4
	}
	return cellHdrSize + klen + int(vlen)
}

func (n node) key(i int) []byte {
	off := n.slot(i)
	klen := int(binary.BigEndian.Uint16(n[off:]))
	return n[off+cellHdrSize : off+cellHdrSize+klen]
}

// child returns the page a search for a key that sorts before cell i's key
// descends into. Index count() means the rightmost child.
func (n node) child(i int) uint32 {
	if i == n.count() {
		return n.linkA()
	}
	return binary.BigEndian.Uint32(n[n.slot(i)+2:])
}

func (n node) setChild(i int, id uint32) {
	if i == n.count() {
		n.setLinkA(id)
		return
	}
	binary.BigEndian.PutUint32(n[n.slot(i)+2:], id)
}

// search returns the index of the first cell whose key is >= key, and
// whether that cell's key equals it.
func (n node) search(key []byte) (int, bool) {
	i := sort.Search(n.count(), func(i int) bool { return bytes.Compare(n.key(i), key) >= 0 })
	return i, i < n.count() && bytes.Equal(n.key(i), key)
}

// childIndex returns which child a search for key descends into.
func (n node) childIndex(key []byte) int {
	return sort.Search(n.count(), func(i int) bool { return bytes.Compare(key, n.key(i)) < 0 })
}

// liveBytes is the space the cells and their slots occupy.
func (n node) liveBytes() int {
	total := 0
	for i := 0; i < n.count(); i++ {
		total += 2 + n.cellSizeAt(n.slot(i))
	}
	return total
}

// insert places a cell at index i, compacting the page if the free space is
// fragmented. It returns false if the cell does not fit at all.
func (n node) insert(i int, cell []byte) bool {
	count := n.count()
	need := len(cell) + 2
	slotsEnd := slotBase + 2*count
	if n.upper()-slotsEnd < need {
		if PageSize-slotBase-n.liveBytes() < need {
			return false
		}
		n.compact()
	}
	upper := n.upper() - len(cell)
	copy(n[upper:], cell)
	n.setUpper(upper)
	// Open a gap in the slot array.
	copy(n[slotBase+2*(i+1):slotBase+2*(count+1)], n[slotBase+2*i:slotBase+2*count])
	binary.BigEndian.PutUint16(n[slotBase+2*i:], uint16(upper))
	n.setCount(count + 1)
	return true
}

// remove deletes cell i. Its bytes become a hole reclaimed by compact.
func (n node) remove(i int) {
	count := n.count()
	copy(n[slotBase+2*i:slotBase+2*(count-1)], n[slotBase+2*(i+1):slotBase+2*count])
	n.setCount(count - 1)
}

// compact rewrites the cell area without holes.
func (n node) compact() {
	cells := n.cells()
	n.setCells(cells)
}

// cells returns a copy of every cell, in order.
func (n node) cells() [][]byte {
	out := make([][]byte, n.count())
	for i := range out {
		out[i] = append([]byte(nil), n.cell(i)...)
	}
	return out
}

// setCells replaces the page's cells, keeping its header links.
func (n node) setCells(cells [][]byte) {
	upper := PageSize
	for i, c := range cells {
		upper -= len(c)
		copy(n[upper:], c)
		binary.BigEndian.PutUint16(n[slotBase+2*i:], uint16(upper))
	}
	n.setUpper(upper)
	n.setCount(len(cells))
}

func cellsSize(cells [][]byte) int {
	total := 0
	for _, c := range cells {
		total += len(c) + 2
	}
	return total
}

func cellKey(cell []byte) []byte {
	klen := int(binary.BigEndian.Uint16(cell))
	return cell[cellHdrSize : cellHdrSize+klen]
}

func internalCell(key []byte, child uint32) []byte {
	cell := make([]byte, cellHdrSize+len(key))
	binary.BigEndian.PutUint16(cell, uint16(len(key)))
	binary.BigEndian.PutUint32(cell[2:], child)
	copy(cell[cellHdrSize:], key)
	return cell
}

// splitPoint picks where to cut a sorted run of cells in two so that both
// halves are as close to equal in bytes as possible, with at least one cell
// on each side.
func splitPoint(cells [][]byte) int {
	total := cellsSize(cells)
	acc := 0
	for i, c := range cells {
		acc += len(c) + 2
		if acc >= total/2 {
			return min(max(i+1, 1), len(cells)-1)
		}
	}
	return len(cells) - 1
}

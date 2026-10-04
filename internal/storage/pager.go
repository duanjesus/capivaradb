package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
)

// PageSize is the unit of I/O and of caching. 8 kB is PostgreSQL's choice
// too: large enough for a B+tree node to hold hundreds of keys, small
// enough that reading one for a single row is not wasteful.
const PageSize = 8192

// Every page starts with the same header.
//
//	0   checksum   uint32  CRC-32C of bytes 4..PageSize
//	4   type       uint8
//	5   (unused)
//	6   cells      uint16  number of cells, for B+tree pages
//	8   lsn        uint64  reserved for the write-ahead log (milestone 4)
const (
	offChecksum = 0
	offType     = 4
	offCells    = 6
	offLSN      = 8
	headerSize  = 16
)

// Page types.
const (
	pageMeta     = 1
	pageLeaf     = 2
	pageInternal = 3
	pageOverflow = 4
	pageFree     = 5
)

// The meta page is page 0. After the common header:
//
//	16  magic         8 bytes
//	24  version       uint32
//	28  page size     uint32
//	32  page count    uint32
//	36  free list     uint32  first free page, 0 if none
//	40  catalog root  uint32  root page of the catalog B+tree, 0 if none
const (
	metaMagic      = "CAPIVARA"
	formatVersion  = 1
	offMagic       = 16
	offVersion     = 24
	offPageSize    = 28
	offPageCount   = 32
	offFreeList    = 36
	offCatalogRoot = 40
	// A free page keeps the ID of the next free page after its header.
	offNextFree = 16
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt is returned (wrapped) when a page read from disk fails its
// checksum or does not look like what it should be.
var ErrCorrupt = errors.New("storage: corrupt page")

// ErrPoolExhausted is returned when every page in the buffer pool is pinned
// and a new one is needed.
var ErrPoolExhausted = errors.New("storage: all buffer pool pages are pinned")

// Pager owns the database file. It hands out pages through a buffer pool of
// a fixed number of frames, allocates new pages and recycles freed ones.
//
// All methods are safe for concurrent use. The contents of a page are not
// protected by the pager: callers must not modify a page while another
// goroutine may read it (the engine's database lock guarantees that until
// page latches arrive with MVCC).
type Pager struct {
	mu   sync.Mutex
	file File

	pageCount   uint32
	freeList    uint32
	catalogRoot uint32
	metaDirty   bool

	// The pool's memory is one allocation, so the garbage collector sees a
	// single pointer-free object however large the cache is.
	buf    []byte
	frames []frame
	lookup map[uint32]int // page ID -> frame index
	hand   int            // clock hand

	stats Stats
}

type frame struct {
	id     uint32
	pins   int
	dirty  bool
	ref    bool // referenced since the clock hand last passed
	loaded bool
}

// Stats counts what the buffer pool did.
type Stats struct {
	Hits      uint64 // page found in the pool
	Misses    uint64 // page had to be read from the file
	Evictions uint64 // page thrown out to make room
	Reads     uint64 // pages read from the file
	Writes    uint64 // pages written to the file
}

// Page is a pinned page. Data is valid until Unpin.
type Page struct {
	ID    uint32
	Data  []byte
	pager *Pager
	frame int
}

// Open opens the database in file, initialising it if the file is empty.
// poolPages is the number of pages the buffer pool may hold.
func Open(file File, poolPages int) (*Pager, error) {
	if poolPages < 8 {
		return nil, fmt.Errorf("storage: a buffer pool needs at least 8 pages, got %d", poolPages)
	}
	p := &Pager{
		file:   file,
		buf:    make([]byte, poolPages*PageSize),
		frames: make([]frame, poolPages),
		lookup: make(map[uint32]int, poolPages),
	}
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	if size == 0 {
		p.pageCount = 1
		p.metaDirty = true
		return p, p.Flush()
	}

	meta := make([]byte, PageSize)
	if _, err := file.ReadAt(meta, 0); err != nil && err != io.EOF {
		return nil, err
	}
	if string(meta[offMagic:offMagic+8]) != metaMagic {
		return nil, fmt.Errorf("%w: not a CapivaraDB file", ErrCorrupt)
	}
	if err := verifyChecksum(meta, 0); err != nil {
		return nil, err
	}
	if v := binary.BigEndian.Uint32(meta[offVersion:]); v != formatVersion {
		return nil, fmt.Errorf("storage: file format version %d is not supported (this build reads %d)", v, formatVersion)
	}
	if s := binary.BigEndian.Uint32(meta[offPageSize:]); s != PageSize {
		return nil, fmt.Errorf("storage: file has page size %d, this build uses %d", s, PageSize)
	}
	p.pageCount = binary.BigEndian.Uint32(meta[offPageCount:])
	p.freeList = binary.BigEndian.Uint32(meta[offFreeList:])
	p.catalogRoot = binary.BigEndian.Uint32(meta[offCatalogRoot:])
	return p, nil
}

func verifyChecksum(data []byte, id uint32) error {
	want := binary.BigEndian.Uint32(data[offChecksum:])
	if got := crc32.Checksum(data[4:], castagnoli); got != want {
		return fmt.Errorf("%w: page %d has checksum %08x, expected %08x", ErrCorrupt, id, got, want)
	}
	return nil
}

func (p *Pager) frameData(i int) []byte {
	return p.buf[i*PageSize : (i+1)*PageSize : (i+1)*PageSize]
}

// Fetch pins a page and returns it, reading it from the file if it is not
// in the pool.
func (p *Pager) Fetch(id uint32) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetchLocked(id, true)
}

func (p *Pager) fetchLocked(id uint32, read bool) (*Page, error) {
	if id == 0 || id >= p.pageCount {
		return nil, fmt.Errorf("%w: reference to page %d, but the file has %d pages", ErrCorrupt, id, p.pageCount)
	}
	if i, ok := p.lookup[id]; ok {
		f := &p.frames[i]
		f.pins++
		f.ref = true
		p.stats.Hits++
		return &Page{ID: id, Data: p.frameData(i), pager: p, frame: i}, nil
	}

	i, err := p.victim()
	if err != nil {
		return nil, err
	}
	data := p.frameData(i)
	if read {
		p.stats.Misses++
		p.stats.Reads++
		if _, err := p.file.ReadAt(data, int64(id)*PageSize); err != nil {
			return nil, fmt.Errorf("storage: reading page %d: %w", id, err)
		}
		if err := verifyChecksum(data, id); err != nil {
			return nil, err
		}
	} else {
		clear(data)
	}
	p.frames[i] = frame{id: id, pins: 1, ref: true, loaded: true}
	p.lookup[id] = i
	return &Page{ID: id, Data: data, pager: p, frame: i}, nil
}

// victim finds a frame to (re)use, with the clock algorithm: sweep the
// frames in a circle; a frame referenced since the last sweep gets a second
// chance, an unreferenced and unpinned one is taken. It approximates
// least-recently-used without having to reorder a list on every access.
func (p *Pager) victim() (int, error) {
	// Two full sweeps are enough: the first clears every reference bit.
	for n := 0; n < 2*len(p.frames); n++ {
		i := p.hand
		p.hand = (p.hand + 1) % len(p.frames)
		f := &p.frames[i]
		if !f.loaded {
			return i, nil
		}
		if f.pins > 0 {
			continue
		}
		if f.ref {
			f.ref = false
			continue
		}
		if f.dirty {
			if err := p.writeFrame(i); err != nil {
				return 0, err
			}
		}
		delete(p.lookup, f.id)
		f.loaded = false
		p.stats.Evictions++
		return i, nil
	}
	return 0, ErrPoolExhausted
}

func (p *Pager) writeFrame(i int) error {
	f := &p.frames[i]
	data := p.frameData(i)
	binary.BigEndian.PutUint32(data[offChecksum:], crc32.Checksum(data[4:], castagnoli))
	if _, err := p.file.WriteAt(data, int64(f.id)*PageSize); err != nil {
		return fmt.Errorf("storage: writing page %d: %w", f.id, err)
	}
	f.dirty = false
	p.stats.Writes++
	return nil
}

// Unpin releases the page. dirty must be true if the caller modified it.
func (pg *Page) Unpin(dirty bool) {
	p := pg.pager
	p.mu.Lock()
	f := &p.frames[pg.frame]
	if f.pins <= 0 || f.id != pg.ID {
		p.mu.Unlock()
		panic(fmt.Sprintf("storage: page %d unpinned more often than pinned", pg.ID))
	}
	f.pins--
	f.dirty = f.dirty || dirty
	p.mu.Unlock()
}

// Alloc returns a new zeroed page of the given type, pinned. It reuses a
// freed page if there is one and extends the file otherwise.
func (p *Pager) Alloc(typ byte) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var pg *Page
	var err error
	if p.freeList != 0 {
		if pg, err = p.fetchLocked(p.freeList, true); err != nil {
			return nil, err
		}
		if pg.Data[offType] != pageFree {
			p.frames[pg.frame].pins--
			return nil, fmt.Errorf("%w: page %d is on the free list but has type %d", ErrCorrupt, pg.ID, pg.Data[offType])
		}
		p.freeList = binary.BigEndian.Uint32(pg.Data[offNextFree:])
		clear(pg.Data)
	} else {
		id := p.pageCount
		p.pageCount++
		if pg, err = p.fetchLocked(id, false); err != nil {
			p.pageCount--
			return nil, err
		}
	}
	p.metaDirty = true
	pg.Data[offType] = typ
	p.frames[pg.frame].dirty = true
	return pg, nil
}

// Free puts a page on the free list. The caller must not hold it pinned.
func (p *Pager) Free(id uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pg, err := p.fetchLocked(id, true)
	if err != nil {
		return err
	}
	f := &p.frames[pg.frame]
	if f.pins != 1 {
		f.pins--
		return fmt.Errorf("storage: page %d freed while pinned", id)
	}
	clear(pg.Data)
	pg.Data[offType] = pageFree
	binary.BigEndian.PutUint32(pg.Data[offNextFree:], p.freeList)
	p.freeList = id
	p.metaDirty = true
	f.dirty = true
	f.pins--
	return nil
}

// CatalogRoot returns the root page of the catalog tree, 0 if there is none.
func (p *Pager) CatalogRoot() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.catalogRoot
}

// SetCatalogRoot records the root page of the catalog tree.
func (p *Pager) SetCatalogRoot(id uint32) {
	p.mu.Lock()
	p.catalogRoot = id
	p.metaDirty = true
	p.mu.Unlock()
}

// Flush writes every dirty page and the meta page, then syncs the file.
//
// Until the write-ahead log exists this is the only point at which the file
// is guaranteed to be consistent: a crash between two flushes can leave a
// B+tree half-updated. Checksums will detect a torn page, not repair it.
func (p *Pager) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.frames {
		if p.frames[i].loaded && p.frames[i].dirty {
			if err := p.writeFrame(i); err != nil {
				return err
			}
		}
	}
	if p.metaDirty {
		meta := make([]byte, PageSize)
		meta[offType] = pageMeta
		copy(meta[offMagic:], metaMagic)
		binary.BigEndian.PutUint32(meta[offVersion:], formatVersion)
		binary.BigEndian.PutUint32(meta[offPageSize:], PageSize)
		binary.BigEndian.PutUint32(meta[offPageCount:], p.pageCount)
		binary.BigEndian.PutUint32(meta[offFreeList:], p.freeList)
		binary.BigEndian.PutUint32(meta[offCatalogRoot:], p.catalogRoot)
		binary.BigEndian.PutUint32(meta[offChecksum:], crc32.Checksum(meta[4:], castagnoli))
		if _, err := p.file.WriteAt(meta, 0); err != nil {
			return fmt.Errorf("storage: writing the meta page: %w", err)
		}
		p.stats.Writes++
		p.metaDirty = false
	}
	return p.file.Sync()
}

// Close flushes and closes the file.
func (p *Pager) Close() error {
	if err := p.Flush(); err != nil {
		p.file.Close()
		return err
	}
	return p.file.Close()
}

// Stats returns a snapshot of the buffer pool counters.
func (p *Pager) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// PageCount returns the number of pages in the file, the meta page included.
func (p *Pager) PageCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int(p.pageCount)
}

// PoolPages returns the capacity of the buffer pool.
func (p *Pager) PoolPages() int { return len(p.frames) }

// Pinned returns how many pages are currently pinned. It is zero whenever no
// operation is in progress; tests use it to catch a missing Unpin.
func (p *Pager) Pinned() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.frames {
		if p.frames[i].loaded && p.frames[i].pins > 0 {
			n++
		}
	}
	return n
}

// FreePages walks the free list and returns the IDs on it.
func (p *Pager) FreePages() ([]uint32, error) {
	p.mu.Lock()
	next := p.freeList
	limit := p.pageCount
	p.mu.Unlock()

	var ids []uint32
	for next != 0 {
		if uint32(len(ids)) > limit {
			return nil, fmt.Errorf("%w: the free list has a cycle", ErrCorrupt)
		}
		pg, err := p.Fetch(next)
		if err != nil {
			return nil, err
		}
		if pg.Data[offType] != pageFree {
			pg.Unpin(false)
			return nil, fmt.Errorf("%w: page %d is on the free list but has type %d", ErrCorrupt, next, pg.Data[offType])
		}
		ids = append(ids, next)
		next = binary.BigEndian.Uint32(pg.Data[offNextFree:])
		pg.Unpin(false)
	}
	return ids, nil
}

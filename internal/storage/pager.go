package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
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
//	8   lsn        uint64  LSN of the last log record that changed the page
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
//	36  free list     uint32  first trunk page of the free list, 0 if none
//	40  catalog root  uint32  root page of the catalog B+tree, 0 if none
//	44  lsn           uint64  where the log stood at the last checkpoint
const (
	metaMagic      = "CAPIVARA"
	formatVersion  = 2
	offMagic       = 16
	offVersion     = 24
	offPageSize    = 28
	offPageCount   = 32
	offFreeList    = 36
	offCatalogRoot = 40
	offMetaLSN     = 44
)

// The free list is a chain of trunk pages, each holding the IDs of free
// pages. Freeing a page appends its ID to the first trunk; the freed page
// itself is not touched, so dropping a large table dirties a handful of
// trunk pages rather than every page of the table.
//
//	16  next trunk  uint32
//	20  count       uint32
//	24  page IDs    uint32 each
const (
	offTrunkNext  = 16
	offTrunkCount = 20
	offTrunkIDs   = 24
	trunkCapacity = (PageSize - offTrunkIDs) / 4
)

// logBufferSize is how much log is held in memory before it is handed to
// the operating system.
const logBufferSize = 32 << 10

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt is returned (wrapped) when a page read from disk fails its
// checksum or does not look like what it should be.
var ErrCorrupt = errors.New("storage: corrupt page")

// ErrPoolExhausted is returned when every page in the buffer pool is in use
// and a new one is needed.
var ErrPoolExhausted = errors.New("storage: all buffer pool pages are in use")

// Options configures Open.
type Options struct {
	// PoolPages is the number of pages the buffer pool may hold.
	PoolPages int
	// NoSync skips every fsync. The database then survives a crash of the
	// process but not of the machine. It exists for tests and benchmarks.
	NoSync bool
}

// Pager owns the database file and its write-ahead log. It hands out pages
// through a buffer pool of a fixed number of frames, allocates new pages
// and recycles freed ones, and logs every change before it can reach the
// data file.
//
// All methods are safe for concurrent use, but writes must not overlap: the
// engine's database lock guarantees that, and also keeps readers away from
// pages while they are being modified, until page latches arrive with MVCC.
type Pager struct {
	mu     sync.Mutex
	file   File
	wal    *wal
	noSync bool

	pageCount   uint32
	freeList    uint32
	catalogRoot uint32
	metaLSN     uint64 // the LSN recorded in the meta page on disk

	// The pool's memory is one allocation, so the garbage collector sees a
	// single pointer-free object however large the cache is.
	buf    []byte
	frames []frame
	lookup map[uint32]int // page ID -> frame index
	hand   int            // clock hand

	// batch is the write in progress, if any. See beginWrite.
	batch *batch
	// nextNote is attached to the next batch that is logged.
	nextNote note
	// recovering is set while the log is being replayed: pages are then
	// modified directly, without being logged again.
	recovering bool
	// broken is set when a write failed after modifying pages. Memory no
	// longer matches the log, so nothing more may be written.
	broken error
	// imaged holds the pages whose full image is in the log since the last
	// checkpoint. Other pages are logged in full the next time they
	// change, which is what makes a torn page write repairable.
	imaged map[uint32]bool
	// active holds the transactions that have log records and no END.
	// The log cannot be discarded while there are any.
	active map[uint64]bool

	spare [][]byte // page-sized buffers for reuse

	stats    Stats
	lastLSN  uint64 // LSN of the last batch logged
	recovery RecoveryInfo
}

type frame struct {
	id     uint32
	pins   int
	dirty  bool
	ref    bool // referenced since the clock hand last passed
	loaded bool
	// inBatch marks a page modified by the batch in progress. It must not
	// be evicted: its change is not in the log yet.
	inBatch bool
}

// Stats counts what the buffer pool and the log did.
type Stats struct {
	Hits        uint64 // page found in the pool
	Misses      uint64 // page had to be read from the file
	Evictions   uint64 // page thrown out to make room
	Reads       uint64 // pages read from the file
	Writes      uint64 // pages written to the file
	LogRecords  uint64 // records appended to the log
	LogBytes    uint64 // bytes appended to the log
	FullImages  uint64 // pages logged as a full image
	Deltas      uint64 // pages logged as byte-range deltas
	Checkpoints uint64
}

// Page is a pinned page. Data is valid until Unpin.
type Page struct {
	ID    uint32
	Data  []byte
	pager *Pager
	frame int
}

// batch is a mini-transaction: the page changes of one B+tree operation.
// They are logged as a single record, so that recovery replays either all
// of them or none — a page split is never half done.
type batch struct {
	pages map[uint32]*batchPage
	note  note
	meta  [3]uint32 // page count, free list, catalog root when it began
}

type batchPage struct {
	// before is a copy of the page as the batch found it, to compute what
	// changed. It is nil for a page that is logged in full anyway.
	before []byte
	full   bool
	dirty  bool
}

// A note ties a batch to a transaction. It travels in the same log record
// as the page changes, so the two are durable together or not at all.
type note struct {
	kind byte
	tx   uint64
	arg  uint64
}

const (
	noteNone    = 0
	noteUndone  = 1 // arg: LSN of the undo record this batch carried out
	noteDropped = 2 // arg: root of the tree this batch freed after commit
	noteCreated = 3 // arg: root of the tree this batch created
)

// Open opens the database in file with its log in logFile, creating both
// if they are empty, and recovers from the log if the last run did not
// shut down cleanly.
func Open(file, logFile File, opts Options) (*Pager, error) {
	if opts.PoolPages < 8 {
		return nil, fmt.Errorf("storage: a buffer pool needs at least 8 pages, got %d", opts.PoolPages)
	}
	p := &Pager{
		file:   file,
		noSync: opts.NoSync,
		buf:    make([]byte, opts.PoolPages*PageSize),
		frames: make([]frame, opts.PoolPages),
		lookup: make(map[uint32]int, opts.PoolPages),
		imaged: make(map[uint32]bool),
		active: make(map[uint64]bool),
	}
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	// The meta page may be unreadable if the crash tore it. The log then
	// holds the values it should have.
	metaOK := false
	if size == 0 {
		p.pageCount = 1
		metaOK = true
	} else if err := p.readMeta(); err == nil {
		metaOK = true
	} else if !errors.Is(err, ErrCorrupt) || isForeign(file) {
		return nil, err
	}

	w, recs, err := openWAL(logFile, opts.NoSync)
	if err != nil {
		return nil, err
	}
	p.wal = w
	if p.metaLSN > w.next {
		w.next, w.durable = p.metaLSN, p.metaLSN
	}
	if err := p.recover(recs, metaOK); err != nil {
		return nil, err
	}
	// Start from a clean slate: everything in the data file, nothing in
	// the log.
	return p, p.checkpoint(true)
}

// isForeign reports whether the file clearly is not one of ours, as opposed
// to being ours with a damaged first page.
func isForeign(file File) bool {
	magic := make([]byte, len(metaMagic))
	if _, err := file.ReadAt(magic, offMagic); err != nil {
		return true
	}
	return string(magic) != metaMagic
}

func (p *Pager) readMeta() error {
	meta := make([]byte, PageSize)
	if _, err := p.file.ReadAt(meta, 0); err != nil && err != io.EOF {
		return err
	}
	if string(meta[offMagic:offMagic+8]) != metaMagic {
		return fmt.Errorf("%w: not a CapivaraDB file", ErrCorrupt)
	}
	if err := verifyChecksum(meta, 0); err != nil {
		return err
	}
	if v := binary.BigEndian.Uint32(meta[offVersion:]); v != formatVersion {
		return fmt.Errorf("storage: file format version %d is not supported (this build reads %d)", v, formatVersion)
	}
	if s := binary.BigEndian.Uint32(meta[offPageSize:]); s != PageSize {
		return fmt.Errorf("storage: file has page size %d, this build uses %d", s, PageSize)
	}
	p.pageCount = binary.BigEndian.Uint32(meta[offPageCount:])
	p.freeList = binary.BigEndian.Uint32(meta[offFreeList:])
	p.catalogRoot = binary.BigEndian.Uint32(meta[offCatalogRoot:])
	p.metaLSN = binary.BigEndian.Uint64(meta[offMetaLSN:])
	return nil
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

func (p *Pager) getBuf() []byte {
	if n := len(p.spare); n > 0 {
		b := p.spare[n-1]
		p.spare = p.spare[:n-1]
		return b
	}
	return make([]byte, PageSize)
}

// Fetch pins a page and returns it, reading it from the file if it is not
// in the pool.
func (p *Pager) Fetch(id uint32) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetchLocked(id, true)
}

// fetchLocked pins a page. With read false the page's current contents are
// irrelevant (it is about to be initialised) and it is not read from disk.
func (p *Pager) fetchLocked(id uint32, read bool) (*Page, error) {
	return p.fetch(id, read, true)
}

// FetchUntracked pins a page that the caller will only read. Inside a
// write batch it spares the copy that Fetch makes in case the page is
// modified; Track can still be called if it turns out it will be.
func (p *Pager) FetchUntracked(id uint32) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetch(id, true, false)
}

// Track prepares a page obtained with FetchUntracked for modification.
func (pg *Page) Track() {
	p := pg.pager
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trackLocked(pg, true)
}

func (p *Pager) fetch(id uint32, read, track bool) (*Page, error) {
	if id == 0 || id >= p.pageCount {
		return nil, fmt.Errorf("%w: reference to page %d, but the file has %d pages", ErrCorrupt, id, p.pageCount)
	}
	i, ok := p.lookup[id]
	if ok {
		f := &p.frames[i]
		f.pins++
		f.ref = true
		p.stats.Hits++
	} else {
		var err error
		if i, err = p.victim(); err != nil {
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
	}
	data := p.frameData(i)
	pg := &Page{ID: id, Data: data, pager: p, frame: i}
	if track {
		p.trackLocked(pg, read)
	}
	return pg, nil
}

// trackLocked makes the batch in progress remember the page as it is now,
// so that what the batch changes in it can be worked out and logged. A
// page must be tracked before it is modified.
func (p *Pager) trackLocked(pg *Page, read bool) {
	id, data := pg.ID, pg.Data
	if b := p.batch; b != nil && b.pages[id] == nil {
		// Remember the page as the batch found it, unless it will be
		// logged whole: a page being initialised, or one not yet imaged
		// since the last checkpoint.
		bp := &batchPage{full: !read || !p.imaged[id]}
		if !bp.full {
			bp.before = p.getBuf()
			copy(bp.before, data)
		}
		b.pages[id] = bp
	}
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
		if f.pins > 0 || f.inBatch {
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

// writeFrame writes a page to the data file. This is where the write-ahead
// rule is enforced: the log is made durable up to the page's LSN first, so
// the file never holds a change the log could not redo or undo.
func (p *Pager) writeFrame(i int) error {
	if p.broken != nil {
		return p.broken
	}
	f := &p.frames[i]
	data := p.frameData(i)
	if err := p.wal.flushTo(binary.BigEndian.Uint64(data[offLSN:])); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(data[offChecksum:], crc32.Checksum(data[4:], castagnoli))
	if _, err := p.file.WriteAt(data, int64(f.id)*PageSize); err != nil {
		return fmt.Errorf("storage: writing page %d: %w", f.id, err)
	}
	f.dirty = false
	p.stats.Writes++
	return nil
}

// Unpin releases the page. dirty must be true if the caller modified it,
// which is only allowed inside a write batch.
func (pg *Page) Unpin(dirty bool) {
	p := pg.pager
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unpinLocked(pg, dirty)
}

func (p *Pager) unpinLocked(pg *Page, dirty bool) {
	f := &p.frames[pg.frame]
	if f.pins <= 0 || f.id != pg.ID {
		panic(fmt.Sprintf("storage: page %d unpinned more often than pinned", pg.ID))
	}
	f.pins--
	if dirty {
		f.dirty = true
		if p.recovering {
			return
		}
		if p.batch == nil {
			panic(fmt.Sprintf("storage: page %d modified outside a write batch", pg.ID))
		}
		bp := p.batch.pages[pg.ID]
		if bp == nil {
			panic(fmt.Sprintf("storage: page %d modified without being tracked", pg.ID))
		}
		bp.dirty = true
		f.inBatch = true
		return
	}
	// A page the batch only read needs no before-image any more.
	if b := p.batch; b != nil && f.pins == 0 {
		if bp := b.pages[pg.ID]; bp != nil && !bp.dirty {
			if bp.before != nil {
				p.spare = append(p.spare, bp.before)
			}
			delete(b.pages, pg.ID)
		}
	}
}

// beginWrite starts a batch. Every modification of a page happens inside
// one; the tree operations that modify pages open it themselves.
func (p *Pager) beginWrite() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken != nil {
		return p.broken
	}
	if p.batch != nil {
		panic("storage: nested write batch")
	}
	p.batch = &batch{pages: make(map[uint32]*batchPage), note: p.nextNote, meta: [3]uint32{p.pageCount, p.freeList, p.catalogRoot}}
	p.nextNote = note{}
	return nil
}

// endWrite finishes the batch: it works out what changed in each page the
// batch modified and appends one log record describing all of it. Nothing
// is forced to disk here; durability is the business of Commit and of the
// write-ahead rule.
func (p *Pager) endWrite(errp *error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.batch
	p.batch = nil
	if b == nil {
		return
	}
	ids := make([]uint32, 0, len(b.pages))
	for id, bp := range b.pages {
		if bp.dirty {
			ids = append(ids, id)
		}
	}
	defer func() {
		for id, bp := range b.pages {
			if bp.before != nil {
				p.spare = append(p.spare, bp.before)
			}
			if i, ok := p.lookup[id]; ok {
				p.frames[i].inBatch = false
			}
		}
	}()
	metaChanged := b.meta != [3]uint32{p.pageCount, p.freeList, p.catalogRoot}

	if *errp != nil {
		// Pages already modified cannot be put back: memory and log now
		// disagree. Refuse further writes; a restart recovers from the
		// log, which never saw this batch.
		if len(ids) > 0 || metaChanged {
			p.broken = fmt.Errorf("storage: a write failed half-way (%w); restart to recover", *errp)
		}
		return
	}
	if len(ids) == 0 && !metaChanged && b.note.kind == noteNone {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	rec := make([]byte, 0, 256)
	rec = append(rec, b.note.kind)
	rec = binary.BigEndian.AppendUint64(rec, b.note.tx)
	rec = binary.BigEndian.AppendUint64(rec, b.note.arg)
	rec = binary.BigEndian.AppendUint32(rec, p.pageCount)
	rec = binary.BigEndian.AppendUint32(rec, p.freeList)
	rec = binary.BigEndian.AppendUint32(rec, p.catalogRoot)
	rec = binary.BigEndian.AppendUint32(rec, uint32(len(ids)))
	for _, id := range ids {
		bp := b.pages[id]
		after := p.frameData(p.lookup[id])
		rec = binary.BigEndian.AppendUint32(rec, id)
		var ranges [][2]int
		size := 0
		if !bp.full {
			ranges, size = diffPage(bp.before, after)
		}
		if bp.full || size > PageSize/2 {
			rec = append(rec, 1)
			rec = append(rec, after...)
			p.stats.FullImages++
			continue
		}
		rec = append(rec, 2)
		rec = binary.BigEndian.AppendUint16(rec, uint16(len(ranges)))
		for _, r := range ranges {
			rec = binary.BigEndian.AppendUint16(rec, uint16(r[0]))
			rec = binary.BigEndian.AppendUint16(rec, uint16(r[1]-r[0]))
			rec = append(rec, after[r[0]:r[1]]...)
		}
		p.stats.Deltas++
	}
	lsn := p.appendLog(recPages, rec)
	p.lastLSN = lsn
	for _, id := range ids {
		binary.BigEndian.PutUint64(p.frameData(p.lookup[id])[offLSN:], lsn)
		p.imaged[id] = true
	}
	if b.note.kind != noteNone {
		p.active[b.note.tx] = true
	}
}

func (p *Pager) appendLog(typ byte, payload []byte) uint64 {
	p.stats.LogRecords++
	p.stats.LogBytes += uint64(walHeaderSize + len(payload))
	lsn := p.wal.append(typ, payload)
	// Keep the in-memory log buffer small. Failing to write it is as
	// serious as any other failed write: nothing more may be changed.
	if len(p.wal.buf) >= logBufferSize {
		if err := p.wal.write(); err != nil && p.broken == nil {
			p.broken = err
		}
	}
	return lsn
}

// diffPage returns the byte ranges in which after differs from before,
// ignoring the checksum and the LSN, and the number of bytes they cover.
// Differences closer than eight bytes are merged into one range: a range
// costs four bytes of bookkeeping.
func diffPage(before, after []byte) (ranges [][2]int, size int) {
	i := 4
	for i < PageSize {
		if i == offLSN {
			i = headerSize
		}
		if i >= headerSize && i+8 <= PageSize &&
			binary.LittleEndian.Uint64(before[i:]) == binary.LittleEndian.Uint64(after[i:]) {
			i += 8
			continue
		}
		if before[i] == after[i] {
			i++
			continue
		}
		start, last := i, i
		for j := i + 1; j < PageSize && j-last <= 8; j++ {
			if start < offLSN && j >= offLSN {
				break
			}
			if before[j] != after[j] {
				last = j
			}
		}
		ranges = append(ranges, [2]int{start, last + 1})
		size += last + 1 - start + 4
		i = last + 1
	}
	return ranges, size
}

// Alloc returns a new zeroed page of the given type, pinned. It reuses a
// freed page if there is one and extends the file otherwise.
func (p *Pager) Alloc(typ byte) (*Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.batch == nil {
		panic("storage: Alloc outside a write batch")
	}

	var pg *Page
	if p.freeList != 0 {
		trunk, err := p.fetchLocked(p.freeList, true)
		if err != nil {
			return nil, err
		}
		if trunk.Data[offType] != pageFree {
			p.unpinLocked(trunk, false)
			return nil, fmt.Errorf("%w: page %d is on the free list but has type %d", ErrCorrupt, trunk.ID, trunk.Data[offType])
		}
		if n := binary.BigEndian.Uint32(trunk.Data[offTrunkCount:]); n > 0 {
			if n > trunkCapacity {
				p.unpinLocked(trunk, false)
				return nil, fmt.Errorf("%w: free list page %d claims %d entries", ErrCorrupt, trunk.ID, n)
			}
			id := binary.BigEndian.Uint32(trunk.Data[offTrunkIDs+4*(n-1):])
			binary.BigEndian.PutUint32(trunk.Data[offTrunkCount:], n-1)
			p.unpinLocked(trunk, true)
			if pg, err = p.fetchLocked(id, false); err != nil {
				return nil, err
			}
		} else {
			// An empty trunk is itself the page handed out.
			p.freeList = binary.BigEndian.Uint32(trunk.Data[offTrunkNext:])
			pg = trunk
		}
	} else {
		id := p.pageCount
		p.pageCount++
		var err error
		if pg, err = p.fetchLocked(id, false); err != nil {
			p.pageCount--
			return nil, err
		}
	}
	// Whatever the frame held, the page starts from zero and is logged in
	// full.
	clear(pg.Data)
	pg.Data[offType] = typ
	bp := p.batch.pages[pg.ID]
	bp.full, bp.dirty = true, true
	p.frames[pg.frame].dirty = true
	p.frames[pg.frame].inBatch = true
	return pg, nil
}

// Free puts a page on the free list. The caller must not hold it pinned.
func (p *Pager) Free(id uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.batch == nil {
		panic("storage: Free outside a write batch")
	}
	if id == 0 || id >= p.pageCount {
		return fmt.Errorf("%w: freeing page %d, but the file has %d pages", ErrCorrupt, id, p.pageCount)
	}
	if i, ok := p.lookup[id]; ok && p.frames[i].pins > 0 {
		return fmt.Errorf("storage: page %d freed while pinned", id)
	}
	if p.freeList != 0 {
		trunk, err := p.fetchLocked(p.freeList, true)
		if err != nil {
			return err
		}
		if n := binary.BigEndian.Uint32(trunk.Data[offTrunkCount:]); n < trunkCapacity {
			binary.BigEndian.PutUint32(trunk.Data[offTrunkIDs+4*n:], id)
			binary.BigEndian.PutUint32(trunk.Data[offTrunkCount:], n+1)
			p.unpinLocked(trunk, true)
			return nil
		}
		p.unpinLocked(trunk, false)
	}
	// No trunk with room: the freed page becomes one.
	pg, err := p.fetchLocked(id, false)
	if err != nil {
		return err
	}
	clear(pg.Data)
	pg.Data[offType] = pageFree
	binary.BigEndian.PutUint32(pg.Data[offTrunkNext:], p.freeList)
	p.batch.pages[id].full = true
	p.freeList = id
	p.unpinLocked(pg, true)
	return nil
}

// CatalogRoot returns the root page of the catalog tree, 0 if there is none.
func (p *Pager) CatalogRoot() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.catalogRoot
}

// SetCatalogRoot records the root page of the catalog tree.
func (p *Pager) SetCatalogRoot(id uint32) (err error) {
	if err = p.beginWrite(); err != nil {
		return err
	}
	defer p.endWrite(&err)
	p.mu.Lock()
	p.catalogRoot = id
	p.mu.Unlock()
	return nil
}

// Checkpoint brings the data file up to date with the log: it makes the log
// durable, writes every modified page and the meta page, and syncs the
// file. If no transaction is in progress the log is then empty of anything
// still needed, and is discarded.
func (p *Pager) Checkpoint() error {
	return p.checkpoint(false)
}

func (p *Pager) checkpoint(force bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.batch != nil {
		panic("storage: checkpoint during a write batch")
	}
	if p.broken != nil {
		return p.broken
	}
	if err := p.wal.flush(); err != nil {
		return err
	}
	dirty := false
	for i := range p.frames {
		if p.frames[i].loaded && p.frames[i].dirty {
			dirty = true
			if err := p.writeFrame(i); err != nil {
				return err
			}
		}
	}
	if !dirty && !force && p.metaLSN == p.wal.next {
		return nil
	}

	meta := make([]byte, PageSize)
	meta[offType] = pageMeta
	copy(meta[offMagic:], metaMagic)
	binary.BigEndian.PutUint32(meta[offVersion:], formatVersion)
	binary.BigEndian.PutUint32(meta[offPageSize:], PageSize)
	binary.BigEndian.PutUint32(meta[offPageCount:], p.pageCount)
	binary.BigEndian.PutUint32(meta[offFreeList:], p.freeList)
	binary.BigEndian.PutUint32(meta[offCatalogRoot:], p.catalogRoot)
	binary.BigEndian.PutUint64(meta[offMetaLSN:], p.wal.next)
	binary.BigEndian.PutUint32(meta[offChecksum:], crc32.Checksum(meta[4:], castagnoli))
	if _, err := p.file.WriteAt(meta, 0); err != nil {
		return fmt.Errorf("storage: writing the meta page: %w", err)
	}
	p.stats.Writes++
	if !p.noSync {
		if err := p.file.Sync(); err != nil {
			return fmt.Errorf("storage: syncing the data file: %w", err)
		}
	}
	p.metaLSN = p.wal.next
	p.stats.Checkpoints++
	// From here on, a page's first change is logged in full again.
	clear(p.imaged)

	// The log still holds the undo information of transactions in
	// progress; it can only go once there are none.
	if len(p.active) == 0 {
		return p.wal.reset()
	}
	return nil
}

// Close checkpoints and closes the files.
func (p *Pager) Close() error {
	err := p.Checkpoint()
	if cerr := p.file.Close(); err == nil {
		err = cerr
	}
	if cerr := p.wal.file.Close(); err == nil {
		err = cerr
	}
	return err
}

// Stats returns a snapshot of the counters.
func (p *Pager) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// LogSize returns how many bytes of log are being kept.
func (p *Pager) LogSize() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wal.size()
}

// ActiveTransactions returns how many transactions have log records and
// have not ended.
func (p *Pager) ActiveTransactions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
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

// FreePages walks the free list and returns every page on it, the trunk
// pages included.
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
		n := binary.BigEndian.Uint32(pg.Data[offTrunkCount:])
		if pg.Data[offType] != pageFree || n > trunkCapacity {
			pg.Unpin(false)
			return nil, fmt.Errorf("%w: page %d is not a valid free list page", ErrCorrupt, next)
		}
		ids = append(ids, next)
		for i := uint32(0); i < n; i++ {
			ids = append(ids, binary.BigEndian.Uint32(pg.Data[offTrunkIDs+4*i:]))
		}
		next = binary.BigEndian.Uint32(pg.Data[offTrunkNext:])
		pg.Unpin(false)
	}
	return ids, nil
}

// LSN returns the position the log has reached: the sequence number the
// next record will get. It only ever grows, across restarts too.
func (p *Pager) LSN() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wal.next
}

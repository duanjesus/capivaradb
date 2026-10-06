package engine

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duanjesus/capivaradb/internal/pgerr"
)

// The executor is a tree of iterators, one per step of the plan, in the
// style of Volcano: each asks the one below for its next row, does its part,
// and hands a row up. Nothing is computed until somebody asks, and nothing
// is kept that is not needed for the rows still to come. That has three
// consequences worth the trouble:
//
//   - A query's memory no longer grows with the tables it reads. A scan
//     holds one batch of rows; a join or a sort that has to remember more
//     than work_mem allows moves the excess to temporary files.
//   - A query that needs only its first rows reads only those. LIMIT stops
//     the scan below it; EXISTS stops at the first row.
//   - Rows reach the client while the query is still running, and a client
//     that fetches a few rows at a time holds a cursor, not a result.

// iter produces the rows of one plan step.
type iter interface {
	// next returns the next row, or nil when there are no more.
	next() ([]any, error)
	// close releases what the iterator holds. It may be called more than
	// once, and at any point.
	close()
}

// query is the state of one execution of a statement, shared by every env
// created for it.
type query struct {
	sess *Session
	// snap is the snapshot the statement reads through. It is fixed when
	// the execution starts: a cursor left open keeps reading the same
	// state while the session runs other statements.
	snap *snapshot
	// workMem is the memory, in bytes, one sort or hash table may use
	// before spilling to disk.
	workMem int
	// timing is set under EXPLAIN ANALYZE with timing on.
	timing bool
	// temps are the temporary files still open.
	temps map[*spillFile]struct{}
}

// newEnv creates the environment for one execution of a statement.
func (s *Session) newEnv(ctx context.Context, params []any, locked bool) *env {
	mem, _ := parseMemory(s.vars["work_mem"])
	return &env{ctx: ctx, params: params, locked: locked,
		q: &query{sess: s, snap: s.snap, workMem: mem, timing: s.timing}}
}

// child returns the environment for something evaluated inside cx's query
// level: the rows of a FROM clause, a subquery.
func (cx *env) child() *env {
	return &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked, q: cx.q}
}

// cleanup removes whatever temporary files the execution left open.
func (q *query) cleanup() {
	for sf := range q.temps {
		sf.remove()
	}
}

// parseMemory reads a memory size the way PostgreSQL does: a number of
// kilobytes, or a number with a unit.
func parseMemory(v string) (bytes int, ok bool) {
	v = strings.TrimSpace(v)
	digits := strings.TrimRight(v, "kKmMgGbB ")
	n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(v[len(digits):])) {
	case "", "kb":
		n *= 1 << 10
	case "mb":
		n *= 1 << 20
	case "gb":
		n *= 1 << 30
	default:
		return 0, false
	}
	if n < minWorkMem || n > 1<<40 {
		return 0, false
	}
	return int(n), true
}

const (
	minWorkMem     = 64 << 10
	defaultWorkMem = "4MB"
)

func drain(it iter) ([][]any, error) {
	defer it.close()
	var rows [][]any
	for {
		row, err := it.next()
		if err != nil || row == nil {
			return rows, err
		}
		rows = append(rows, row)
	}
}

// start opens the node and counts what it does, for EXPLAIN ANALYZE.
func (n *planNode) start(cx *env) (iter, error) {
	var began time.Time
	if cx.q.timing {
		began = time.Now()
	}
	it, err := n.open(cx)
	if cx.q.timing {
		n.elapsed += time.Since(began)
	}
	if err != nil {
		return nil, err
	}
	n.loops++
	return &counted{n: n, it: it, timing: cx.q.timing}, nil
}

type counted struct {
	n      *planNode
	it     iter
	timing bool
}

func (c *counted) next() ([]any, error) {
	if !c.timing {
		row, err := c.it.next()
		if row != nil {
			c.n.actual++
		}
		return row, err
	}
	began := time.Now()
	row, err := c.it.next()
	c.n.elapsed += time.Since(began)
	if row != nil {
		c.n.actual++
	}
	return row, err
}

func (c *counted) close() { c.it.close() }

// sliceIter iterates over rows already in memory.
type sliceIter struct {
	rows [][]any
	pos  int
}

func (s *sliceIter) next() ([]any, error) {
	if s.pos >= len(s.rows) {
		return nil, nil
	}
	s.pos++
	return s.rows[s.pos-1], nil
}

func (s *sliceIter) close() { s.rows = nil }

// filterIter passes on the rows that satisfy every condition.
type filterIter struct {
	src   iter
	en    *env
	conds []evalFn
}

func (f *filterIter) next() ([]any, error) {
	for {
		row, err := f.src.next()
		if err != nil || row == nil {
			return nil, err
		}
		f.en.row = row
		if ok, err := allMatch(f.conds, f.en); err != nil {
			return nil, err
		} else if ok {
			return row, nil
		}
	}
}

func (f *filterIter) close() { f.src.close() }

// ---- rows on disk ----

// rowBytes estimates the memory a row occupies. It only has to be
// proportionate: it decides when to stop keeping rows in memory.
func rowBytes(row []any) int {
	n := 24 + 16*len(row)
	for _, v := range row {
		if s, ok := v.(string); ok {
			n += len(s)
		}
	}
	return n
}

// appendRow encodes a row for a temporary file: the number of values, then
// each value behind a one-byte tag.
func appendRow(dst []byte, row []any) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(row)))
	for _, v := range row {
		switch v := v.(type) {
		case nil:
			dst = append(dst, 0)
		case bool:
			if v {
				dst = append(dst, 2)
			} else {
				dst = append(dst, 1)
			}
		case int64:
			dst = binary.AppendVarint(append(dst, 3), v)
		case float64:
			dst = binary.BigEndian.AppendUint64(append(dst, 4), math.Float64bits(v))
		case string:
			dst = binary.AppendUvarint(append(dst, 5), uint64(len(v)))
			dst = append(dst, v...)
		default:
			panic(fmt.Sprintf("engine: value of type %T in a row", v))
		}
	}
	return dst
}

func readRow(r *bufio.Reader) ([]any, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err // io.EOF here is the clean end of the file
	}
	row := make([]any, n)
	for i := range row {
		tag, err := r.ReadByte()
		if err != nil {
			return nil, corruptSpill(err)
		}
		switch tag {
		case 0:
		case 1:
			row[i] = false
		case 2:
			row[i] = true
		case 3:
			v, err := binary.ReadVarint(r)
			if err != nil {
				return nil, corruptSpill(err)
			}
			row[i] = v
		case 4:
			var b [8]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return nil, corruptSpill(err)
			}
			row[i] = math.Float64frombits(binary.BigEndian.Uint64(b[:]))
		case 5:
			size, err := binary.ReadUvarint(r)
			if err != nil {
				return nil, corruptSpill(err)
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, corruptSpill(err)
			}
			row[i] = string(b)
		default:
			return nil, corruptSpill(fmt.Errorf("unknown tag %d", tag))
		}
	}
	return row, nil
}

func corruptSpill(err error) error {
	return pgerr.New(pgerr.InternalError, "temporary file is damaged: %v", err)
}

// spillFile is a temporary file of rows: written from start to end, then
// read from start to end as many times as needed.
type spillFile struct {
	q    *query
	f    *os.File
	w    *bufio.Writer
	buf  []byte
	rows int
	size int64
}

func (q *query) newSpill() (*spillFile, error) {
	f, err := os.CreateTemp("", "capivaradb-*.tmp")
	if err != nil {
		return nil, pgerr.New(pgerr.InternalError, "could not create a temporary file: %v", err)
	}
	sf := &spillFile{q: q, f: f, w: bufio.NewWriterSize(f, 32<<10)}
	if q.temps == nil {
		q.temps = make(map[*spillFile]struct{})
	}
	q.temps[sf] = struct{}{}
	q.sess.db.spills.Add(1)
	q.sess.db.openSpills.Add(1)
	return sf, nil
}

func (sf *spillFile) write(row []any) error {
	sf.buf = appendRow(sf.buf[:0], row)
	sf.rows++
	sf.size += int64(len(sf.buf))
	if _, err := sf.w.Write(sf.buf); err != nil {
		return pgerr.New(pgerr.DiskFull, "could not write to a temporary file: %v", err)
	}
	return nil
}

// reader returns an iterator over the rows written so far.
func (sf *spillFile) reader() (iter, error) {
	if err := sf.w.Flush(); err != nil {
		return nil, pgerr.New(pgerr.DiskFull, "could not write to a temporary file: %v", err)
	}
	return &spillReader{r: bufio.NewReaderSize(io.NewSectionReader(sf.f, 0, sf.size), 16<<10)}, nil
}

func (sf *spillFile) remove() {
	if sf.f == nil {
		return
	}
	name := sf.f.Name()
	sf.f.Close()
	os.Remove(name)
	sf.f = nil
	delete(sf.q.temps, sf)
	sf.q.sess.db.openSpills.Add(-1)
}

type spillReader struct {
	r *bufio.Reader
}

func (r *spillReader) next() ([]any, error) {
	row, err := readRow(r.r)
	if err == io.EOF {
		return nil, nil
	}
	return row, err
}

func (r *spillReader) close() {}

// spool keeps rows to be read back, possibly several times: in memory while
// they fit in work_mem, in a temporary file once they do not.
type spool struct {
	q     *query
	mem   [][]any
	bytes int
	file  *spillFile
	count int
}

func (sp *spool) add(row []any) error {
	sp.count++
	if sp.file == nil {
		sp.mem = append(sp.mem, row)
		sp.bytes += rowBytes(row)
		if sp.bytes <= sp.q.workMem {
			return nil
		}
		f, err := sp.q.newSpill()
		if err != nil {
			return err
		}
		sp.file = f
		for _, r := range sp.mem {
			if err := f.write(r); err != nil {
				return err
			}
		}
		sp.mem = nil
		return nil
	}
	return sp.file.write(row)
}

func (sp *spool) fill(src iter) error {
	defer src.close()
	for {
		row, err := src.next()
		if err != nil || row == nil {
			return err
		}
		if err := sp.add(row); err != nil {
			return err
		}
	}
}

func (sp *spool) reader() (iter, error) {
	if sp.file != nil {
		return sp.file.reader()
	}
	return &sliceIter{rows: sp.mem}, nil
}

func (sp *spool) release() {
	if sp.file != nil {
		sp.file.remove()
	}
	sp.mem = nil
}

// ---- sorting ----

// sorter sorts rows by cmp, stably: rows that compare equal come out in the
// order they went in. While the rows fit in work_mem they are sorted in
// memory. Past that, each memory-full is sorted and written out as a run,
// and the runs are merged at the end — an external merge sort, whose memory
// is the size of one run however much is sorted.
//
// With a limit, only the first limit rows are wanted, and a heap of that
// many is all that is kept.
type sorter struct {
	q     *query
	cmp   func(a, b []any) int
	limit int // 0: no limit

	mem   [][]any
	bytes int
	runs  []*spillFile
	top   topHeap
	seq   int

	// What happened, for EXPLAIN ANALYZE.
	method string
	peak   int
}

// topNMax is the largest LIMIT for which a heap is used instead of a sort.
const topNMax = 10000

func (so *sorter) add(row []any) error {
	if so.limit > 0 && so.limit <= topNMax {
		so.top.cmp = so.cmp
		so.seq++
		e := topEntry{row: row, seq: so.seq}
		if len(so.top.e) < so.limit {
			heap.Push(&so.top, e)
		} else if so.top.less(e, so.top.e[0]) {
			// Better than the worst row kept: it takes that row's place.
			so.top.e[0] = e
			heap.Fix(&so.top, 0)
		}
		return nil
	}
	so.mem = append(so.mem, row)
	so.bytes += rowBytes(row)
	so.peak = max(so.peak, so.bytes)
	if so.bytes > so.q.workMem {
		return so.flush()
	}
	return nil
}

// flush sorts what is in memory and writes it out as a run.
func (so *sorter) flush() error {
	slices.SortStableFunc(so.mem, so.cmp)
	f, err := so.q.newSpill()
	if err != nil {
		return err
	}
	for _, r := range so.mem {
		if err := f.write(r); err != nil {
			return err
		}
	}
	so.runs = append(so.runs, f)
	so.mem, so.bytes = nil, 0
	return nil
}

// finish returns the rows in order.
func (so *sorter) finish() (iter, error) {
	if so.limit > 0 && so.limit <= topNMax {
		so.method = "top-N heapsort"
		entries := so.top.e
		slices.SortFunc(entries, func(a, b topEntry) int {
			if c := so.cmp(a.row, b.row); c != 0 {
				return c
			}
			return a.seq - b.seq
		})
		rows := make([][]any, len(entries))
		for i, e := range entries {
			rows[i] = e.row
			so.peak += rowBytes(e.row)
		}
		so.top.e = nil
		return &sliceIter{rows: rows}, nil
	}
	if len(so.runs) == 0 {
		so.method = "quicksort"
		slices.SortStableFunc(so.mem, so.cmp)
		rows := so.mem
		so.mem = nil
		return &sliceIter{rows: rows}, nil
	}
	so.method = "external merge"
	if len(so.mem) > 0 {
		if err := so.flush(); err != nil {
			return nil, err
		}
	}
	m := &mergeIter{so: so}
	for i, run := range so.runs {
		r, err := run.reader()
		if err != nil {
			return nil, err
		}
		row, err := r.next()
		if err != nil {
			return nil, err
		}
		if row != nil {
			m.h.e = append(m.h.e, mergeEntry{row: row, run: i, src: r})
		}
	}
	m.h.cmp = so.cmp
	heap.Init(&m.h)
	return m, nil
}

func (so *sorter) release() {
	for _, run := range so.runs {
		run.remove()
	}
	so.runs, so.mem, so.top.e = nil, nil, nil
}

// info describes the sort for EXPLAIN ANALYZE.
func (so *sorter) info() string {
	if so.method == "external merge" {
		return fmt.Sprintf("Sort Method: external merge  Runs: %d", len(so.runs))
	}
	return fmt.Sprintf("Sort Method: %s  Memory: %dkB", so.method, (so.peak+1023)/1024)
}

// topHeap keeps the best limit rows seen, worst on top.
type topEntry struct {
	row []any
	seq int
}

type topHeap struct {
	e   []topEntry
	cmp func(a, b []any) int
}

// less orders entries as the sort does, earlier arrivals first among equals.
func (h *topHeap) less(a, b topEntry) bool {
	if c := h.cmp(a.row, b.row); c != 0 {
		return c < 0
	}
	return a.seq < b.seq
}

func (h *topHeap) Len() int           { return len(h.e) }
func (h *topHeap) Less(i, j int) bool { return h.less(h.e[j], h.e[i]) }
func (h *topHeap) Swap(i, j int)      { h.e[i], h.e[j] = h.e[j], h.e[i] }
func (h *topHeap) Push(x any)         { h.e = append(h.e, x.(topEntry)) }
func (h *topHeap) Pop() any {
	x := h.e[len(h.e)-1]
	h.e = h.e[:len(h.e)-1]
	return x
}

// mergeIter merges sorted runs. Among equal rows the one from the earlier
// run wins, which is what keeps the whole sort stable: runs were cut from
// the input in order.
type mergeEntry struct {
	row []any
	run int
	src iter
}

type mergeHeap struct {
	e   []mergeEntry
	cmp func(a, b []any) int
}

func (h *mergeHeap) Len() int { return len(h.e) }
func (h *mergeHeap) Less(i, j int) bool {
	if c := h.cmp(h.e[i].row, h.e[j].row); c != 0 {
		return c < 0
	}
	return h.e[i].run < h.e[j].run
}
func (h *mergeHeap) Swap(i, j int) { h.e[i], h.e[j] = h.e[j], h.e[i] }
func (h *mergeHeap) Push(x any)    { h.e = append(h.e, x.(mergeEntry)) }
func (h *mergeHeap) Pop() any {
	x := h.e[len(h.e)-1]
	h.e = h.e[:len(h.e)-1]
	return x
}

type mergeIter struct {
	so *sorter
	h  mergeHeap
}

func (m *mergeIter) next() ([]any, error) {
	if len(m.h.e) == 0 {
		return nil, nil
	}
	top := m.h.e[0]
	next, err := top.src.next()
	if err != nil {
		return nil, err
	}
	if next == nil {
		heap.Pop(&m.h)
	} else {
		m.h.e[0].row = next
		heap.Fix(&m.h, 0)
	}
	return top.row, nil
}

func (m *mergeIter) close() { m.so.release() }

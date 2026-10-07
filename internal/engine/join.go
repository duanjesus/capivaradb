package engine

import (
	"fmt"
	"hash/fnv"
)

// Four ways of joining, all behind the same iterator interface. Which one
// runs is the planner's choice (plan.go); what each costs is in the
// comments below, because that is what the choice is made from.
//
// Every row here has the full width of the FROM clause it belongs to, with
// only the segments of the relations joined so far filled in. Joining two
// rows is copying the inner relation's segment into a copy of the outer
// row; a row with nothing to match keeps that segment NULL, which is
// exactly what an outer join wants.

type joinKind uint8

const (
	joinInner joinKind = iota
	// joinLeft keeps the outer rows that match nothing.
	joinLeft
	// joinFull keeps the unmatched rows of both sides.
	joinFull
)

func (k joinKind) label() string {
	switch k {
	case joinLeft:
		return " Left"
	case joinFull:
		return " Full"
	}
	return ""
}

// joinSpec is what every join method needs to know.
type joinSpec struct {
	kind         joinKind
	outer, inner *planNode
	// off and end delimit the inner side's segment of the row.
	off, end int
	width    int
	// conds are checked on each candidate pair.
	conds []evalFn
}

func (j *joinSpec) combine(buf, outer, inner []any) []any {
	if buf == nil {
		buf = make([]any, j.width)
	}
	copy(buf, outer[:j.width])
	copy(buf[j.off:j.end], inner[j.off:j.end])
	return buf
}

// ---- nested loop ----

// nlJoinIter compares every outer row with every inner row. The inner side
// is read once and kept — in memory if it fits in work_mem, in a temporary
// file otherwise — and gone through again for each outer row. Its cost is
// the product of the two sides, which is why it is the last resort: it is
// what remains when a join has no equality to hash or sort on.
type nlJoinIter struct {
	j      *joinSpec
	cx, en *env
	outer  iter

	inner   *spool
	reading iter // the pass over the inner side for cur
	cur     []any
	matched bool
	buf     []any

	// For a full join: which inner rows have matched something.
	innerMatched []bool
	idx          int
	tail         iter
	tailIdx      int

	steps int
}

func (it *nlJoinIter) materialise() error {
	src, err := it.j.inner.start(it.cx)
	if err != nil {
		return err
	}
	it.inner = &spool{q: it.cx.q}
	if err := it.inner.fill(src); err != nil {
		return err
	}
	if it.j.kind == joinFull {
		it.innerMatched = make([]bool, it.inner.count)
	}
	return nil
}

func (it *nlJoinIter) next() ([]any, error) {
	for {
		if it.tail != nil {
			// The outer side is exhausted: what is left of a full join is
			// the inner rows nothing matched.
			row, err := it.tail.next()
			if err != nil || row == nil {
				return nil, err
			}
			it.tailIdx++
			if !it.innerMatched[it.tailIdx-1] {
				return row, nil
			}
			continue
		}
		if it.cur == nil {
			row, err := it.outer.next()
			if err != nil {
				return nil, err
			}
			if row == nil {
				if it.j.kind != joinFull {
					return nil, nil
				}
				if it.inner == nil {
					if err := it.materialise(); err != nil {
						return nil, err
					}
				}
				if it.tail, err = it.inner.reader(); err != nil {
					return nil, err
				}
				continue
			}
			// The inner side is only read once there is an outer row to
			// join it with.
			if it.inner == nil {
				if err := it.materialise(); err != nil {
					return nil, err
				}
			}
			if it.reading, err = it.inner.reader(); err != nil {
				return nil, err
			}
			it.cur, it.matched, it.idx = row, false, 0
		}
		in, err := it.reading.next()
		if err != nil {
			return nil, err
		}
		if in == nil {
			row, matched := it.cur, it.matched
			it.cur = nil
			if !matched && it.j.kind != joinInner {
				return row, nil
			}
			continue
		}
		i := it.idx
		it.idx++
		if it.steps++; it.steps&0xfff == 0 {
			if err := it.cx.ctx.Err(); err != nil {
				return nil, err
			}
		}
		it.buf = it.j.combine(it.buf, it.cur, in)
		it.en.row = it.buf
		ok, err := allMatch(it.j.conds, it.en)
		if err != nil {
			return nil, err
		}
		if ok {
			it.matched = true
			if it.innerMatched != nil {
				it.innerMatched[i] = true
			}
			row := it.buf
			it.buf = nil
			return row, nil
		}
	}
}

func (it *nlJoinIter) close() {
	it.outer.close()
	if it.inner != nil {
		it.inner.release()
	}
}

// ---- nested loop with an index lookup ----

// indexJoinIter looks the inner rows up through an index, once per outer
// row. Its cost is proportional to the outer side and to the rows found,
// not to the size of the inner table.
type indexJoinIter struct {
	j      *joinSpec
	s      *Session
	acc    *access
	probe  *planNode // the lookup, for EXPLAIN ANALYZE
	local  []evalFn  // the inner relation's own conditions
	cx, en *env
	outer  iter

	cur     []any
	refs    []rowRef
	pos     int
	matched bool
}

func (it *indexJoinIter) next() ([]any, error) {
	for {
		if it.cur == nil {
			row, err := it.outer.next()
			if err != nil || row == nil {
				return nil, err
			}
			if err := it.cx.ctx.Err(); err != nil {
				return nil, err
			}
			it.en.row = row
			if it.refs, err = it.acc.fetch(it.s, it.en); err != nil {
				return nil, err
			}
			it.probe.loops++
			it.cur, it.pos, it.matched = row, 0, false
		}
		if it.pos >= len(it.refs) {
			row, matched := it.cur, it.matched
			it.cur = nil
			if !matched && it.j.kind != joinInner {
				return row, nil
			}
			continue
		}
		r := it.refs[it.pos]
		it.pos++
		row := make([]any, it.j.width)
		copy(row, it.cur)
		copy(row[it.j.off:], r.vals)
		it.en.row = row
		ok, err := allMatch(it.local, it.en)
		if err == nil && ok {
			ok, err = allMatch(it.j.conds, it.en)
		}
		if err != nil {
			return nil, err
		}
		if ok {
			it.probe.actual++
			it.matched = true
			return row, nil
		}
	}
}

func (it *indexJoinIter) close() { it.outer.close() }

// ---- hash join ----

// hashKey computes one column of a join key.
type hashKey struct {
	eval evalFn
	// float is set when the two sides are compared as double precision and
	// this side may produce an integer.
	float bool
}

// encodeKeys appends the key of en's row. It reports false if any part is
// NULL: such a row equals nothing.
func encodeKeys(dst []byte, keys []hashKey, en *env) ([]byte, bool, error) {
	for _, k := range keys {
		v, err := k.eval(en)
		if err != nil || v == nil {
			return dst, false, err
		}
		if i, ok := v.(int64); ok && k.float {
			v = float64(i)
		}
		dst = appendKey(dst, v)
	}
	return dst, true, nil
}

const (
	// hashPartitions is the number of files each side is split into when
	// the build side does not fit in memory.
	hashPartitions = 16
	// hashMaxDepth bounds how many times a partition that is still too
	// large is split again. Past it — which takes a key shared by a large
	// part of the table — the partition is joined in memory regardless.
	hashMaxDepth = 3
	// hashEntryOverhead is the memory charged per build row beyond the row.
	hashEntryOverhead = 64
)

// fanoutFor chooses into how many files to split something that needs need
// bytes of memory when workMem is what there is: enough for each part to
// fit with room to spare, and no more — every file costs a buffer, and a
// part that fits is done in one pass however small it is. A power of two,
// so that taking a hash modulo it stays uniform.
func fanoutFor(need int64, workMem int) int {
	n := 2
	for n < hashPartitions && int64(n)*int64(workMem) < 2*need {
		n *= 2
	}
	return n
}

// partitionOf assigns a key to one of hashPartitions partitions; users with
// fewer files take it modulo their number. depth changes the assignment,
// so that splitting a partition again actually spreads it.
func partitionOf(key []byte, depth int) int {
	h := fnv.New64a()
	h.Write(key)
	x := h.Sum64() + uint64(depth)*0x9e3779b97f4a7c15
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	return int(x % hashPartitions)
}

// hashJoinIter builds a hash table on the inner side's join key and probes
// it with each outer row: one pass over each side instead of one pass over
// the inner side per outer row.
//
// If the inner side does not fit in work_mem, both sides are first split
// into partitions on disk by a hash of the key — rows that can match are
// then in partitions of the same number — and the partitions are joined
// one pair at a time (a Grace hash join). A partition that is still too
// large is split again with a different hash.
type hashJoinIter struct {
	j          *joinSpec
	probeKeys  []hashKey
	buildKeys  []hashKey
	hashNode   *planNode
	cx, en     *env
	started    bool
	probe      iter
	first      []any // an outer row read ahead of the build
	rows       [][]any
	matched    []bool
	table      map[string][]int32
	cur        []any
	pending    []int32
	pi         int
	curMatched bool
	tail       int
	work       []partPair
	files      []*spillFile
	keyBuf     []byte
	buf        []any
	steps      int

	batches, peak int
}

type partPair struct {
	build, probe *spillFile
	depth        int
}

func (h *hashJoinIter) init() error {
	h.started, h.tail = true, -1
	outer, err := h.j.outer.start(h.cx)
	if err != nil {
		return err
	}
	h.probe = outer
	if h.first, err = outer.next(); err != nil {
		return err
	}
	if h.first == nil && h.j.kind != joinFull {
		// Nothing to probe with: the inner side need not even be read.
		outer.close()
		h.probe = nil
		return nil
	}

	inner, err := h.j.inner.start(h.cx)
	if err != nil {
		return err
	}
	defer inner.close()
	mem := 0
	var parts []*spillFile
	for {
		row, err := inner.next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		h.hashNode.actual++
		if parts != nil {
			if err := h.route(parts, row, h.buildKeys, 0, h.j.kind == joinFull); err != nil {
				return err
			}
			continue
		}
		h.rows = append(h.rows, row)
		mem += rowBytes(row) + hashEntryOverhead
		if mem <= h.cx.q.workMem {
			continue
		}
		// Too much for memory: from here on the build side goes to disk.
		if parts, err = h.newParts(hashPartitions); err != nil {
			return err
		}
		for _, r := range h.rows {
			if err := h.route(parts, r, h.buildKeys, 0, h.j.kind == joinFull); err != nil {
				return err
			}
		}
		h.rows = nil
	}
	h.hashNode.loops++
	if parts == nil {
		h.peak = mem
		h.batches = 1
		return h.buildTable()
	}

	// The probe side has to be split the same way before any of it can be
	// joined.
	probeParts, err := h.newParts(len(parts))
	if err != nil {
		return err
	}
	keep := h.j.kind != joinInner
	for row := h.first; row != nil; {
		if err := h.route(probeParts, row, h.probeKeys, 0, keep); err != nil {
			return err
		}
		if row, err = outer.next(); err != nil {
			return err
		}
	}
	outer.close()
	h.first, h.probe = nil, nil
	for i := range parts {
		h.work = append(h.work, partPair{build: parts[i], probe: probeParts[i], depth: 1})
	}
	return nil
}

func (h *hashJoinIter) newParts(n int) ([]*spillFile, error) {
	parts := make([]*spillFile, n)
	for i := range parts {
		f, err := h.cx.q.newSpill()
		if err != nil {
			return nil, err
		}
		parts[i] = f
	}
	return parts, nil
}

// route writes a row to the partition its key belongs to. A row with a NULL
// in its key matches nothing; it is dropped unless the join has to return
// unmatched rows of its side, in which case any partition will do.
func (h *hashJoinIter) route(parts []*spillFile, row []any, keys []hashKey, depth int, keepNull bool) error {
	h.en.row = row
	key, ok, err := encodeKeys(h.keyBuf[:0], keys, h.en)
	h.keyBuf = key
	if err != nil {
		return err
	}
	if !ok {
		if !keepNull {
			return nil
		}
		return parts[0].write(row)
	}
	return parts[partitionOf(key, depth)%len(parts)].write(row)
}

func (h *hashJoinIter) buildTable() error {
	h.table = make(map[string][]int32, len(h.rows))
	for i, row := range h.rows {
		h.en.row = row
		key, ok, err := encodeKeys(h.keyBuf[:0], h.buildKeys, h.en)
		h.keyBuf = key
		if err != nil {
			return err
		}
		if ok {
			h.table[string(key)] = append(h.table[string(key)], int32(i))
		}
	}
	h.matched = nil
	if h.j.kind == joinFull {
		h.matched = make([]bool, len(h.rows))
	}
	return nil
}

// load prepares the next pair of partitions, splitting it further if its
// build side is still too large.
func (h *hashJoinIter) load(pp partPair) error {
	h.files = append(h.files, pp.build, pp.probe)
	src, err := pp.build.reader()
	if err != nil {
		return err
	}
	rows, err := drain(src)
	if err != nil {
		return err
	}
	mem := 0
	for _, r := range rows {
		mem += rowBytes(r) + hashEntryOverhead
	}
	if mem > h.cx.q.workMem && pp.depth < hashMaxDepth && len(rows) > 1 {
		build, err := h.newParts(fanoutFor(int64(mem), h.cx.q.workMem))
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := h.route(build, r, h.buildKeys, pp.depth, h.j.kind == joinFull); err != nil {
				return err
			}
		}
		probe, err := h.newParts(len(build))
		if err != nil {
			return err
		}
		src, err := pp.probe.reader()
		if err != nil {
			return err
		}
		for {
			row, err := src.next()
			if err != nil {
				return err
			}
			if row == nil {
				break
			}
			if err := h.route(probe, row, h.probeKeys, pp.depth, h.j.kind != joinInner); err != nil {
				return err
			}
		}
		h.dropFiles()
		for i := range build {
			h.work = append(h.work, partPair{build: build[i], probe: probe[i], depth: pp.depth + 1})
		}
		return nil
	}
	h.rows = rows
	h.peak = max(h.peak, mem)
	h.batches++
	if err := h.buildTable(); err != nil {
		return err
	}
	h.probe, err = pp.probe.reader()
	return err
}

func (h *hashJoinIter) dropFiles() {
	for _, f := range h.files {
		f.remove()
	}
	h.files = nil
}

func (h *hashJoinIter) next() ([]any, error) {
	if !h.started {
		if err := h.init(); err != nil {
			return nil, err
		}
	}
	for {
		// The build rows that share the current probe row's key.
		for h.pi < len(h.pending) {
			idx := h.pending[h.pi]
			h.pi++
			h.buf = h.j.combine(h.buf, h.cur, h.rows[idx])
			h.en.row = h.buf
			ok, err := allMatch(h.j.conds, h.en)
			if err != nil {
				return nil, err
			}
			if ok {
				h.curMatched = true
				if h.matched != nil {
					h.matched[idx] = true
				}
				row := h.buf
				h.buf = nil
				return row, nil
			}
		}
		if h.cur != nil {
			row := h.cur
			h.cur = nil
			if !h.curMatched && h.j.kind != joinInner {
				return row, nil
			}
		}
		if h.probe != nil {
			row := h.first
			h.first = nil
			if row == nil {
				var err error
				if row, err = h.probe.next(); err != nil {
					return nil, err
				}
			}
			if row != nil {
				if h.steps++; h.steps&0xfff == 0 {
					if err := h.cx.ctx.Err(); err != nil {
						return nil, err
					}
				}
				h.cur, h.curMatched, h.pending, h.pi = row, false, nil, 0
				h.en.row = row
				key, ok, err := encodeKeys(h.keyBuf[:0], h.probeKeys, h.en)
				h.keyBuf = key
				if err != nil {
					return nil, err
				}
				if ok {
					h.pending = h.table[string(key)]
				}
				continue
			}
			h.probe.close()
			h.probe = nil
			if h.j.kind == joinFull {
				h.tail = 0
			}
		}
		if h.tail >= 0 {
			// A full join also returns the build rows nothing matched.
			for h.tail < len(h.rows) {
				i := h.tail
				h.tail++
				if !h.matched[i] {
					return h.rows[i], nil
				}
			}
			h.tail = -1
		}
		h.rows, h.table, h.matched = nil, nil, nil
		h.dropFiles()
		if len(h.work) == 0 {
			h.report()
			return nil, nil
		}
		pp := h.work[len(h.work)-1]
		h.work = h.work[:len(h.work)-1]
		if err := h.load(pp); err != nil {
			return nil, err
		}
	}
}

func (h *hashJoinIter) report() {
	if h.batches > 0 {
		h.hashNode.info = []string{fmt.Sprintf("Batches: %d  Memory Usage: %dkB", h.batches, (h.peak+1023)/1024)}
	}
}

func (h *hashJoinIter) close() {
	if h.probe != nil {
		h.probe.close()
		h.probe = nil
	}
	h.dropFiles()
	for _, pp := range h.work {
		pp.build.remove()
		pp.probe.remove()
	}
	h.work, h.rows, h.table = nil, nil, nil
}

// ---- merge join ----

// mergeJoinIter joins two inputs that are both sorted on the join key by
// walking them in step, the way two sorted lists are merged. It needs no
// hash table and no second pass, so when the inputs are already in key
// order — two tables joined on their primary keys — it is the cheapest
// join there is. When they are not, they are sorted first, which usually
// makes a hash join the better choice.
//
// Each input row carries its key as an extra value after the row proper.
type mergeJoinIter struct {
	j *joinSpec
	// cmp compares a left key with a right key; same compares two left
	// keys.
	cmp, same   func(a, b any) int
	left, right iter
	en          *env

	cur      []any
	group    [][]any // the right rows whose key equals groupKey
	groupKey any
	gi       int
	ahead    []any // a right row read but not yet used
	rdone    bool
	buf      []any
}

func (m *mergeJoinIter) nextKeyed(src iter) ([]any, error) {
	for {
		row, err := src.next()
		if err != nil || row == nil || row[m.j.width] != nil {
			return row, err
		}
		// A NULL key equals nothing.
	}
}

func (m *mergeJoinIter) next() ([]any, error) {
	for {
		for m.cur != nil && m.gi < len(m.group) {
			in := m.group[m.gi]
			m.gi++
			m.buf = m.j.combine(m.buf, m.cur, in)
			m.en.row = m.buf
			ok, err := allMatch(m.j.conds, m.en)
			if err != nil {
				return nil, err
			}
			if ok {
				row := m.buf
				m.buf = nil
				return row, nil
			}
		}
		var err error
		if m.cur, err = m.nextKeyed(m.left); err != nil || m.cur == nil {
			return nil, err
		}
		key := m.cur[m.j.width]
		m.gi = 0
		if m.group != nil && m.same(key, m.groupKey) == 0 {
			continue // same key as the row before: same group again
		}
		m.group = nil
		for !m.rdone {
			if m.ahead == nil {
				if m.ahead, err = m.nextKeyed(m.right); err != nil {
					return nil, err
				}
				if m.ahead == nil {
					m.rdone = true
					break
				}
			}
			c := m.cmp(key, m.ahead[m.j.width])
			if c < 0 {
				break // the right side is past this key
			}
			if c == 0 {
				m.group = append(m.group, m.ahead)
			}
			m.ahead = nil
		}
		m.groupKey = key
		if m.group == nil && m.rdone {
			return nil, nil // nothing left on the right to match anything
		}
	}
}

func (m *mergeJoinIter) close() {
	m.left.close()
	m.right.close()
}

// keyedIter appends a key to each row, for a merge join and the sort that
// may precede it.
type keyedIter struct {
	src  iter
	en   *env
	eval evalFn
}

func (k *keyedIter) next() ([]any, error) {
	row, err := k.src.next()
	if err != nil || row == nil {
		return nil, err
	}
	k.en.row = row
	v, err := k.eval(k.en)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(row)+1)
	copy(out, row)
	out[len(row)] = v
	return out, nil
}

func (k *keyedIter) close() { k.src.close() }

package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
	"github.com/duanjesus/capivaradb/internal/storage"
)

// This file is where rows meet B+trees.
//
// A table is one tree, holding every version of every row (see mvcc.go for
// how a version is keyed). The row's own key is the primary key, encoded so
// that byte order is key order; a table without a primary key gets a
// hidden, ever-increasing row ID instead. The table is therefore
// "clustered": rows live in the leaves, in key order, with the versions of
// a row side by side.
//
// A secondary index is another tree. Its key is the indexed columns followed
// by the table key of a version, and its value is empty: an index entry is
// a pointer to one version. Looking a value up is a seek to the prefix made
// of the indexed columns.

// rowRef is one version of a row, with the key it is stored under.
type rowRef struct {
	key  []byte // the row's key followed by the version tag
	xmin uint64
	xmax uint64
	vals []any
}

// must turns a storage failure while undoing a change into a panic: there
// is no way to continue with a transaction half rolled back. The connection
// handler recovers and reports it.
func must(err error) {
	if err != nil {
		panic(fmt.Sprintf("engine: storage error while rolling back: %v", err))
	}
}

// storageError converts an error from the storage layer for the client.
func storageError(err error) error {
	if errors.Is(err, storage.ErrValueTooLarge) {
		return pgerr.New(pgerr.ProgramLimitExceeded, "value is too large for the buffer pool; start the server with a larger -cache")
	}
	if errors.Is(err, storage.ErrKeyTooLarge) {
		return pgerr.New(pgerr.ProgramLimitExceeded,
			"index row size exceeds the maximum of %d bytes", storage.MaxKeySize)
	}
	return err
}

func (t *table) decodeVersion(key, val []byte) (rowRef, error) {
	if len(key) < versionTagSize || len(val) < 8 {
		return rowRef{}, fmt.Errorf("corrupt row version in table %s", t.name)
	}
	_, xmin := splitVersionKey(key)
	vals, err := decodeTuple(t.cols, val[8:])
	return rowRef{key: key, xmin: xmin, xmax: binary.BigEndian.Uint64(val), vals: vals}, err
}

// scan returns the versions of t that the snapshot can see, in key order. A
// nil snapshot returns every version. The caller must hold db.mu.
func (db *DB) scan(t *table, sn *snapshot) ([]rowRef, error) {
	var rows []rowRef
	c := t.tree.Seek(nil)
	for {
		key, val, ok := c.Next()
		if !ok {
			break
		}
		// Visibility is decided from the eight bytes at either end before
		// the tuple is decoded: most of the cost of skipping a dead
		// version is avoided.
		if sn != nil && len(key) >= versionTagSize && len(val) >= 8 {
			_, xmin := splitVersionKey(key)
			if !sn.visible(xmin, binary.BigEndian.Uint64(val)) {
				continue
			}
		}
		ref, err := t.decodeVersion(key, val)
		if err != nil {
			return nil, err
		}
		rows = append(rows, ref)
	}
	return rows, c.Err()
}

// rows returns the rows of t as the snapshot sees them.
func (db *DB) rows(t *table, sn *snapshot, locked bool) ([][]any, error) {
	if !locked {
		db.mu.RLock()
		defer db.mu.RUnlock()
	}
	if err := db.stillCurrent(t); err != nil {
		return nil, err
	}
	refs, err := db.scan(t, sn)
	if err != nil {
		return nil, err
	}
	rows := make([][]any, len(refs))
	for i, r := range refs {
		rows[i] = r.vals
	}
	return rows, nil
}

// entryKey is the key of the index entry for a version.
func (ix *index) entryKey(vals []any, rowKey []byte) []byte {
	return append(encodeKey(ix.cols, vals), rowKey...)
}

// storeVersion writes a new version of the row with the given key, created
// by the transaction, and its index entries.
func (t *table) storeVersion(base []byte, vals []any, ch *changes) error {
	key := versionKey(base, ch.begin())
	if err := ch.put(t.tree, key, versionValue(0, encodeTuple(t.cols, vals))); err != nil {
		return err
	}
	for _, ix := range t.indexes {
		if err := ch.put(ix.tree, ix.entryKey(vals, key), nil); err != nil {
			return err
		}
	}
	return nil
}

// removeVersion takes a version and its index entries out of the trees.
func (t *table) removeVersion(v rowRef, ch *changes) error {
	if err := ch.del(t.tree, v.key, versionValue(v.xmax, encodeTuple(t.cols, v.vals))); err != nil {
		return err
	}
	for _, ix := range t.indexes {
		if err := ch.del(ix.tree, ix.entryKey(v.vals, v.key), nil); err != nil {
			return err
		}
	}
	return nil
}

func hasNull(cols []int, vals []any) bool {
	for _, c := range cols {
		if vals[c] == nil {
			return true
		}
	}
	return false
}

func (t *table) uniqueViolation(constraint string, cols []int, vals []any) error {
	names, key := make([]string, len(cols)), make([]string, len(cols))
	for i, c := range cols {
		names[i], key[i] = t.cols[c].name, pgwire.TextValue(vals[c])
	}
	return pgerr.New(pgerr.UniqueViolation, "duplicate key value violates unique constraint %q", constraint).
		WithDetail("Key (%s)=(%s) already exists.", strings.Join(names, ", "), strings.Join(key, ", "))
}

// occupies decides whether an existing version stands in the way of a new
// row with the same unique key, for the transaction me.
//
// Uniqueness is not a matter of snapshots: a row committed after my
// snapshot was taken is invisible to me and still makes my insert a
// duplicate. What matters is whether the version is, or may yet turn out
// to be, alive:
//
//   - created by a transaction still in progress: unknown, wait for it;
//   - not deleted: alive, a duplicate;
//   - deleted by me: gone, as far as I am concerned;
//   - deleted by a transaction still in progress: unknown, wait for it;
//   - deleted by a committed transaction: gone.
func (db *DB) occupies(xmin, xmax, me uint64) (alive bool, err error) {
	switch {
	case xmin != me && db.isActive(xmin):
		return false, &waitError{xmin}
	case xmax == 0:
		return true, nil
	case xmax == me:
		return false, nil
	case db.isActive(xmax):
		return false, &waitError{xmax}
	}
	return false, nil
}

// checkPrimaryKey verifies that no live version has the row key base.
func (t *table) checkPrimaryKey(db *DB, base []byte, vals []any, me uint64) error {
	c := t.tree.Seek(base)
	for {
		key, val, ok := c.Next()
		if !ok || len(key) != len(base)+versionTagSize || !bytes.HasPrefix(key, base) {
			break
		}
		_, xmin := splitVersionKey(key)
		alive, err := db.occupies(xmin, binary.BigEndian.Uint64(val), me)
		if err != nil {
			return err
		}
		if alive {
			return t.uniqueViolation(t.name+"_pkey", t.pk, vals)
		}
	}
	return c.Err()
}

// checkUnique verifies that no live version has the same values in a unique
// index. As in PostgreSQL, NULLs are distinct from each other, so a key
// containing one never conflicts.
func (t *table) checkUnique(db *DB, vals []any, me uint64) error {
	for _, ix := range t.indexes {
		if !ix.unique || hasNull(ix.cols, vals) {
			continue
		}
		prefix := encodeKey(ix.cols, vals)
		c := ix.tree.Seek(prefix)
		for {
			entry, _, ok := c.Next()
			if !ok || !bytes.HasPrefix(entry, prefix) {
				break
			}
			// The entry points at a version; whether that version is
			// alive is written in the table.
			rowKey := entry[len(prefix):]
			val, found, err := t.tree.Get(rowKey)
			if err != nil {
				return err
			}
			if !found || len(val) < 8 || len(rowKey) < versionTagSize {
				return fmt.Errorf("index %s has an entry for a row version that does not exist", ix.name)
			}
			_, xmin := splitVersionKey(rowKey)
			alive, err := db.occupies(xmin, binary.BigEndian.Uint64(val), me)
			if err != nil {
				return err
			}
			if alive {
				return t.uniqueViolation(ix.name, ix.cols, vals)
			}
		}
		if err := c.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (t *table) checkNotNull(vals []any) error {
	for i, c := range t.cols {
		if c.notNull && vals[i] == nil {
			return pgerr.New(pgerr.NotNullViolation,
				"null value in column %q of relation %q violates not-null constraint", c.name, t.name)
		}
	}
	return nil
}

// insertRow validates and stores a new row. The caller must hold db.mu.
func (t *table) insertRow(vals []any, ch *changes) error {
	if err := t.checkNotNull(vals); err != nil {
		return err
	}
	me := ch.begin()
	var base []byte
	if t.pk != nil {
		base = encodeKey(t.pk, vals)
		if err := t.checkPrimaryKey(ch.db, base, vals, me); err != nil {
			return err
		}
	} else {
		base = rowIDKey(t.nextRowID)
		t.nextRowID++
	}
	if err := t.checkUnique(ch.db, vals, me); err != nil {
		return err
	}
	t.delta++
	t.mods++
	return t.storeVersion(base, vals, ch)
}

// claim makes sure the transaction may delete or replace a version it can
// see. If someone else already has, the two transactions are in conflict:
//
//   - the other is still in progress: wait for it. If it rolls back, the
//     version is free again; if it commits, the statement runs again and
//     decides anew.
//   - the other committed: only possible when this transaction's snapshot
//     predates that commit, that is, under repeatable read. Carrying on
//     would overwrite a change this transaction never saw — a lost update
//     — so it fails instead and the application retries.
func (t *table) claim(v rowRef, ch *changes) error {
	if v.xmax == 0 || v.xmax == ch.tx {
		return nil
	}
	if ch.db.isActive(v.xmax) {
		return &waitError{v.xmax}
	}
	return errSerialization()
}

// retire ends a version's life on behalf of the transaction: it marks it as
// deleted by it, or, if the transaction created the version itself, removes
// it outright — nobody else has ever been able to see it.
func (t *table) retire(v rowRef, ch *changes) error {
	if err := t.claim(v, ch); err != nil {
		return err
	}
	me := ch.begin()
	if v.xmin == me {
		return t.removeVersion(v, ch)
	}
	tuple := encodeTuple(t.cols, v.vals)
	t.dead++
	return ch.replace(t.tree, v.key, versionValue(v.xmax, tuple), versionValue(me, tuple))
}

// updateRow replaces the version old with a new one holding vals.
func (t *table) updateRow(old rowRef, vals []any, ch *changes) error {
	t.mods++
	if err := t.checkNotNull(vals); err != nil {
		return err
	}
	// Retire the old version first: the uniqueness checks below then see
	// it as deleted by this transaction and do not count it.
	if err := t.retire(old, ch); err != nil {
		return err
	}
	base, _ := splitVersionKey(old.key)
	if t.pk != nil {
		if newBase := encodeKey(t.pk, vals); !bytes.Equal(newBase, base) {
			base = newBase
			if err := t.checkPrimaryKey(ch.db, base, vals, ch.tx); err != nil {
				return err
			}
		}
	}
	if err := t.checkUnique(ch.db, vals, ch.tx); err != nil {
		return err
	}
	return t.storeVersion(base, vals, ch)
}

// deleteRow deletes the version old.
func (t *table) deleteRow(old rowRef, ch *changes) error {
	t.delta--
	t.mods++
	return t.retire(old, ch)
}

// vacuum removes the versions of t that no snapshot, present or future,
// can see: those deleted by a transaction whose commit every snapshot in
// use already takes into account. It returns how many it removed. The
// caller must hold db.mu.
func (db *DB) vacuum(t *table, ch *changes) (int, error) {
	versions, err := db.scan(t, nil)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, v := range versions {
		if v.xmax == 0 || db.isActive(v.xmax) || !db.goneForEveryone(v.xmin, v.xmax) {
			continue
		}
		if err := t.removeVersion(v, ch); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// ---- catalog ----
//
// The catalog is itself a B+tree, whose root page is recorded in the file's
// meta page. It maps "t/<name>" to a table definition and "i/<name>" to an
// index definition; everything else is found from there.

func catalogKey(kind byte, name string) []byte {
	return append([]byte{kind, '/'}, name...)
}

type recWriter struct{ b []byte }

func (w *recWriter) uint(v uint64) { w.b = binary.AppendUvarint(w.b, v) }
func (w *recWriter) str(s string)  { w.uint(uint64(len(s))); w.b = append(w.b, s...) }
func (w *recWriter) bool(v bool) {
	if v {
		w.b = append(w.b, 1)
	} else {
		w.b = append(w.b, 0)
	}
}
func (w *recWriter) ints(v []int) {
	w.uint(uint64(len(v)))
	for _, x := range v {
		w.uint(uint64(x))
	}
}

type recReader struct {
	b   []byte
	bad bool
}

func (r *recReader) uint() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.bad = true
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *recReader) str() string {
	n := r.uint()
	if r.bad || uint64(len(r.b)) < n {
		r.bad = true
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

func (r *recReader) bool() bool {
	if len(r.b) == 0 {
		r.bad = true
		return false
	}
	v := r.b[0] != 0
	r.b = r.b[1:]
	return v
}

func (r *recReader) ints() []int {
	n := r.uint()
	if r.bad || n > uint64(len(r.b)) {
		r.bad = true
		return nil
	}
	if n == 0 {
		return nil
	}
	out := make([]int, n)
	for i := range out {
		out[i] = int(r.uint())
	}
	return out
}

// saveTable writes the table's definition to the catalog.
func (db *DB) saveTable(t *table, ch *changes) error {
	var w recWriter
	w.uint(uint64(t.tree.Root()))
	w.uint(uint64(len(t.cols)))
	for _, c := range t.cols {
		w.str(c.name)
		w.uint(uint64(c.typ))
		w.bool(c.notNull)
		// A default is stored as SQL text and compiled again on load:
		// the canonical printer guarantees it reads back as the same
		// expression.
		w.str(c.defSQL)
	}
	w.bool(t.pk != nil)
	w.ints(t.pk)
	return ch.put(db.catalog, catalogKey('t', t.name), w.b)
}

func (db *DB) saveIndex(ix *index, ch *changes) error {
	var w recWriter
	w.str(ix.table.name)
	w.uint(uint64(ix.tree.Root()))
	w.bool(ix.unique)
	w.ints(ix.cols)
	return ch.put(db.catalog, catalogKey('i', ix.name), w.b)
}

// forget removes an entry from the catalog.
func (db *DB) forget(kind byte, name string, ch *changes) error {
	key := catalogKey(kind, name)
	old, found, err := db.catalog.Get(key)
	if err != nil || !found {
		return err
	}
	return ch.del(db.catalog, key, old)
}

// loadCatalog rebuilds the in-memory catalog from the catalog tree.
func (db *DB) loadCatalog() error {
	type entry struct {
		name string
		rec  []byte
	}
	var tables, indexes, stats []entry
	c := db.catalog.Seek(nil)
	for {
		key, rec, ok := c.Next()
		if !ok {
			break
		}
		if len(key) < 2 || key[1] != '/' {
			return fmt.Errorf("corrupt catalog: unexpected key %q", key)
		}
		e := entry{string(key[2:]), rec}
		switch key[0] {
		case 't':
			tables = append(tables, e)
		case 's':
			stats = append(stats, e)
		default:
			indexes = append(indexes, e)
		}
	}
	if err := c.Err(); err != nil {
		return err
	}

	// Defaults are bound outside any connection.
	b := &binder{sess: &Session{db: db}, scope: &scope{}}
	for _, e := range tables {
		r := recReader{b: e.rec}
		t := &table{name: e.name, tree: storage.OpenTree(db.pager, uint32(r.uint()))}
		ncols := r.uint()
		for i := uint64(0); i < ncols && !r.bad; i++ {
			col := column{name: r.str(), typ: sql.Type(r.uint()), notNull: r.bool(), defSQL: r.str()}
			if col.defSQL != "" && !r.bad {
				stmts, err := sql.Parse("select " + col.defSQL)
				if err != nil {
					return fmt.Errorf("corrupt catalog: default of %s.%s: %w", t.name, col.name, err)
				}
				be, err := b.bind(stmts[0].Node.(*sql.Select).Items[0].Expr, col.typ)
				if err != nil {
					return fmt.Errorf("corrupt catalog: default of %s.%s: %w", t.name, col.name, err)
				}
				if col.def, err = storeAs(be, col, 0); err != nil {
					return fmt.Errorf("corrupt catalog: default of %s.%s: %w", t.name, col.name, err)
				}
			}
			t.cols = append(t.cols, col)
		}
		hasPK := r.bool()
		t.pk = r.ints()
		if r.bad || (hasPK && t.pk == nil) {
			return fmt.Errorf("corrupt catalog: table %q", e.name)
		}
		if t.pk == nil {
			// Row IDs continue after the largest one in use.
			last, err := t.tree.LastKey()
			if err != nil {
				return err
			}
			if len(last) == 8+versionTagSize {
				t.nextRowID = int64(binary.BigEndian.Uint64(last)) + 1
			}
		}
		db.tables[t.name] = t
	}
	for _, e := range indexes {
		r := recReader{b: e.rec}
		t := db.tables[r.str()]
		ix := &index{name: e.name, table: t, tree: storage.OpenTree(db.pager, uint32(r.uint())), unique: r.bool(), cols: r.ints()}
		if r.bad || t == nil {
			return fmt.Errorf("corrupt catalog: index %q", e.name)
		}
		t.indexes = append(t.indexes, ix)
		db.indexes[ix.name] = ix
	}
	for _, e := range stats {
		// Statistics that do not fit the table are ignored rather than
		// trusted: the planner can do without, and ANALYZE replaces them.
		if t := db.tables[e.name]; t != nil {
			t.stats = decodeStats(e.rec, len(t.cols))
		}
	}
	return nil
}

// CheckReport summarises what Verify found.
type CheckReport struct {
	Tables, Indexes int
	Rows            int // rows as a new transaction would see them, give or take uncommitted work
	Versions        int // row versions stored, dead ones awaiting vacuum included
	Pages           int // pages in the file
	FreePages       int // of which on the free list
}

// Verify checks the whole database file for consistency:
//
//   - every B+tree (catalog, tables, indexes) satisfies its invariants;
//   - every row version decodes, no row has two current versions, and each
//     version has exactly one entry in each index of its
//     table, with no entries left over;
//   - every page of the file belongs to exactly one tree or to the free
//     list: nothing is leaked, nothing is shared.
//
// It reads everything, so it is for tests and for an explicit check, not
// for the normal path.
func (db *DB) Verify() (CheckReport, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	report := CheckReport{Pages: db.pager.PageCount()}
	owner := map[uint32]string{0: "the meta page"}
	claim := func(who string, ids []uint32) error {
		for _, id := range ids {
			if prev, taken := owner[id]; taken {
				return fmt.Errorf("page %d belongs to both %s and %s", id, prev, who)
			}
			owner[id] = who
		}
		return nil
	}
	check := func(who string, tree *storage.Tree) (storage.TreeInfo, error) {
		info, err := tree.Verify()
		if err != nil {
			return info, fmt.Errorf("%s: %w", who, err)
		}
		return info, claim(who, info.Pages)
	}

	if _, err := check("the catalog", db.catalog); err != nil {
		return report, err
	}
	// In name order: the check reads pages, and which pages are cached
	// afterwards should not depend on Go's map iteration.
	for _, name := range db.tableNames() {
		t := db.tables[name]
		report.Tables++
		if _, err := check("table "+t.name, t.tree); err != nil {
			return report, err
		}
		// Every version, whatever any snapshot thinks of it.
		rows, err := db.scan(t, nil)
		if err != nil {
			return report, fmt.Errorf("table %s: %w", t.name, err)
		}
		report.Versions += len(rows)
		var current []byte
		for _, r := range rows {
			if r.xmax != 0 && !db.isActive(r.xmax) {
				continue // dead, awaiting vacuum
			}
			if r.xmax == 0 {
				report.Rows++
			}
			// A row has at most one version that nobody has deleted:
			// two would be two rows with the same key.
			base, _ := splitVersionKey(r.key)
			if r.xmax == 0 && !db.isActive(r.xmin) {
				if bytes.Equal(base, current) {
					return report, fmt.Errorf("table %s has two current versions of one row", t.name)
				}
				current = base
			}
		}
		for _, ix := range t.indexes {
			report.Indexes++
			info, err := check("index "+ix.name, ix.tree)
			if err != nil {
				return report, err
			}
			if info.Entries != len(rows) {
				return report, fmt.Errorf("index %s has %d entries for %d row versions", ix.name, info.Entries, len(rows))
			}
			for _, r := range rows {
				if _, found, err := ix.tree.Get(ix.entryKey(r.vals, r.key)); err != nil || !found {
					return report, fmt.Errorf("index %s is missing the entry of a row (err: %v)", ix.name, err)
				}
			}
		}
	}
	free, err := db.pager.FreePages()
	if err != nil {
		return report, err
	}
	report.FreePages = len(free)
	if err := claim("the free list", free); err != nil {
		return report, err
	}
	if len(owner) != report.Pages {
		return report, fmt.Errorf("%d of %d pages are accounted for: the rest are leaked", len(owner), report.Pages)
	}
	return report, nil
}

// tableNames returns the names of all tables, sorted. The caller must hold
// db.mu.
func (db *DB) tableNames() []string {
	names := make([]string, 0, len(db.tables))
	for name := range db.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (w *recWriter) float(v float64) {
	w.b = binary.BigEndian.AppendUint64(w.b, math.Float64bits(v))
}

func (r *recReader) float() float64 {
	if len(r.b) < 8 {
		r.bad = true
		return 0
	}
	v := math.Float64frombits(binary.BigEndian.Uint64(r.b))
	r.b = r.b[8:]
	return v
}

// keyBounds describes a range of an index (or of a table's primary key) to
// read: every entry whose leading columns encode to prefix and whose next
// column lies between lo and hi. A nil bound is open.
//
// The bounds only have to be loose enough: whoever asks for a range checks
// the rows it gets against the full condition anyway.
type keyBounds struct {
	prefix         []byte
	lo, hi         []byte
	loIncl, hiIncl bool
}

// rangeScan returns the versions of t the snapshot sees whose key in ix (or
// in the table itself, if ix is nil) falls within the bounds, in key order.
func (db *DB) rangeScan(t *table, ix *index, sn *snapshot, kb keyBounds, locked bool) ([]rowRef, error) {
	if !locked {
		db.mu.RLock()
		defer db.mu.RUnlock()
	}
	if err := db.stillCurrent(t); err != nil {
		return nil, err
	}
	tree := t.tree
	if ix != nil {
		tree = ix.tree
	}
	var rows []rowRef
	start := append(append([]byte(nil), kb.prefix...), kb.lo...)
	if kb.lo == nil && kb.hi != nil {
		// NULL sorts before every value and satisfies no comparison:
		// a range with only an upper bound starts after the NULLs.
		start = append(start, 1)
	}
	c := tree.Seek(start)
	for {
		key, val, ok := c.Next()
		if !ok || !bytes.HasPrefix(key, kb.prefix) {
			break
		}
		rest := key[len(kb.prefix):]
		if kb.lo != nil && !kb.loIncl && bytes.HasPrefix(rest, kb.lo) {
			continue
		}
		if kb.hi != nil {
			// Encoded values are self-delimiting, so "starts with the
			// bound" means "equals the bound".
			if bytes.HasPrefix(rest, kb.hi) {
				if !kb.hiIncl {
					break
				}
			} else if bytes.Compare(rest, kb.hi) > 0 {
				break
			}
		}

		rowKey := key
		if ix != nil {
			// An index entry is the indexed columns followed by the key
			// of the version it points at, which holds the rest.
			n, err := keyPartsLen(key, ix.cols, t)
			if err != nil {
				return nil, err
			}
			rowKey = key[n:]
			var found bool
			if val, found, err = t.tree.Get(rowKey); err != nil {
				return nil, err
			} else if !found {
				return nil, fmt.Errorf("index %s has an entry for a row version that does not exist", ix.name)
			}
		}
		if len(rowKey) < versionTagSize || len(val) < 8 {
			return nil, fmt.Errorf("corrupt row version in table %s", t.name)
		}
		_, xmin := splitVersionKey(rowKey)
		if sn != nil && !sn.visible(xmin, binary.BigEndian.Uint64(val)) {
			continue
		}
		ref, err := t.decodeVersion(rowKey, val)
		if err != nil {
			return nil, err
		}
		rows = append(rows, ref)
	}
	return rows, c.Err()
}

// keyPartsLen returns how many bytes of key the encodings of the given
// columns take up.
func keyPartsLen(key []byte, cols []int, t *table) (int, error) {
	n := 0
	for _, c := range cols {
		if n >= len(key) {
			return 0, fmt.Errorf("corrupt index key in table %s", t.name)
		}
		if key[n] == 0 { // NULL
			n++
			continue
		}
		n++
		switch t.cols[c].typ {
		case sql.Bool:
			n++
		case sql.Text:
			// Up to the 0x00 0x01 terminator; 0x00 0xFF is an escaped
			// zero byte inside the text.
			for {
				i := bytes.IndexByte(key[n:], 0)
				if i < 0 || n+i+1 >= len(key) {
					return 0, fmt.Errorf("corrupt index key in table %s", t.name)
				}
				n += i + 2
				if key[n-1] == 1 {
					break
				}
			}
		default:
			n += 8
		}
	}
	if n > len(key) {
		return 0, fmt.Errorf("corrupt index key in table %s", t.name)
	}
	return n, nil
}

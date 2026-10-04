package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
	"github.com/duanjesus/capivaradb/internal/storage"
)

// This file is where rows meet B+trees.
//
// A table is one tree. Its key is the primary key, encoded so that byte
// order is key order; a table without a primary key gets a hidden,
// ever-increasing row ID instead. Its value is the row as a tuple. The table
// is therefore "clustered": the rows live in the leaves, in key order, and
// a lookup by primary key is a single descent.
//
// A secondary index is another tree. Its key is the indexed columns followed
// by the table key of the row, and its value is empty: an index entry is
// just a pointer, and appending the table key makes every entry distinct
// even when the indexed values repeat. Looking a value up is a seek to the
// prefix made of the indexed columns.

// rowRef is a row together with the key it is stored under.
type rowRef struct {
	key  []byte
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

// scan returns every row of t in key order. The caller must hold db.mu.
func (db *DB) scan(t *table) ([]rowRef, error) {
	var rows []rowRef
	c := t.tree.Seek(nil)
	for {
		key, tuple, ok := c.Next()
		if !ok {
			break
		}
		vals, err := decodeTuple(t.cols, tuple)
		if err != nil {
			return nil, err
		}
		rows = append(rows, rowRef{key, vals})
	}
	return rows, c.Err()
}

// snapshot returns the current rows of t.
func (db *DB) snapshot(t *table, locked bool) ([][]any, error) {
	if !locked {
		db.mu.RLock()
		defer db.mu.RUnlock()
	}
	if err := db.stillCurrent(t); err != nil {
		return nil, err
	}
	refs, err := db.scan(t)
	if err != nil {
		return nil, err
	}
	rows := make([][]any, len(refs))
	for i, r := range refs {
		rows[i] = r.vals
	}
	return rows, nil
}

// entryKey is the key of the index entry for a row.
func (ix *index) entryKey(vals []any, rowKey []byte) []byte {
	return append(encodeKey(ix.cols, vals), rowKey...)
}

// storeRow writes a row and its index entries, without checking anything.
func (t *table) storeRow(key []byte, vals []any, ch *changes) error {
	if err := ch.put(t.tree, key, encodeTuple(t.cols, vals)); err != nil {
		return err
	}
	for _, ix := range t.indexes {
		if err := ch.put(ix.tree, ix.entryKey(vals, key), nil); err != nil {
			return err
		}
	}
	return nil
}

// removeRow deletes a row and its index entries.
func (t *table) removeRow(key []byte, vals []any, ch *changes) error {
	if err := ch.del(t.tree, key, encodeTuple(t.cols, vals)); err != nil {
		return err
	}
	for _, ix := range t.indexes {
		if err := ch.del(ix.tree, ix.entryKey(vals, key), nil); err != nil {
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

// checkUnique verifies that no row other than the one stored under self has
// the same values in a unique index. As in PostgreSQL, NULLs are distinct
// from each other, so a key containing one never conflicts.
func (t *table) checkUnique(vals []any, self []byte) error {
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
			if !bytes.Equal(entry[len(prefix):], self) {
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
	var key []byte
	if t.pk != nil {
		key = encodeKey(t.pk, vals)
		_, exists, err := t.tree.Get(key)
		if err != nil {
			return err
		}
		if exists {
			return t.uniqueViolation(t.name+"_pkey", t.pk, vals)
		}
	} else {
		key = rowIDKey(t.nextRowID)
		t.nextRowID++
	}
	if err := t.checkUnique(vals, nil); err != nil {
		return err
	}
	return t.storeRow(key, vals, ch)
}

// updateRow replaces the row old with vals.
func (t *table) updateRow(old rowRef, vals []any, ch *changes) error {
	if err := t.checkNotNull(vals); err != nil {
		return err
	}
	key := old.key
	if t.pk != nil {
		key = encodeKey(t.pk, vals)
		if !bytes.Equal(key, old.key) {
			_, exists, err := t.tree.Get(key)
			if err != nil {
				return err
			}
			if exists {
				return t.uniqueViolation(t.name+"_pkey", t.pk, vals)
			}
		}
	}
	if err := t.checkUnique(vals, old.key); err != nil {
		return err
	}
	// The row moves if its key changed, and its index entries embed the
	// key, so the general case is delete-then-insert. When only non-key,
	// non-indexed columns change this does more work than needed; the
	// planner milestone can afford to be smarter.
	if err := t.removeRow(old.key, old.vals, ch); err != nil {
		return err
	}
	return t.storeRow(key, vals, ch)
}

// deleteRow removes a row.
func (t *table) deleteRow(old rowRef, ch *changes) error {
	return t.removeRow(old.key, old.vals, ch)
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
	var tables, indexes []entry
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
		if key[0] == 't' {
			tables = append(tables, e)
		} else {
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
			if len(last) == 8 {
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
	return nil
}

// CheckReport summarises what Verify found.
type CheckReport struct {
	Tables, Indexes int
	Rows            int
	Pages           int // pages in the file
	FreePages       int // of which on the free list
}

// Verify checks the whole database file for consistency:
//
//   - every B+tree (catalog, tables, indexes) satisfies its invariants;
//   - every row decodes, and has exactly one entry in each index of its
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
	for _, t := range db.tables {
		report.Tables++
		if _, err := check("table "+t.name, t.tree); err != nil {
			return report, err
		}
		rows, err := db.scan(t)
		if err != nil {
			return report, fmt.Errorf("table %s: %w", t.name, err)
		}
		report.Rows += len(rows)
		for _, ix := range t.indexes {
			report.Indexes++
			info, err := check("index "+ix.name, ix.tree)
			if err != nil {
				return report, err
			}
			if info.Entries != len(rows) {
				return report, fmt.Errorf("index %s has %d entries for %d rows", ix.name, info.Entries, len(rows))
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

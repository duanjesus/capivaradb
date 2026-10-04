// Package engine binds and executes SQL on top of the storage layer.
//
// Rows live in B+trees (see store.go); the catalog is persistent. What is
// still deliberately naive is everything about concurrency and planning: one
// database-wide lock, no isolation between concurrent transactions, full
// scans and nested-loop joins only. Those are the subjects of the later
// milestones. Everything the protocol layer needs goes through
// pgwire.Session.
package engine

import (
	"fmt"
	"strings"
	"sync"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
	"github.com/duanjesus/capivaradb/internal/storage"
	"github.com/duanjesus/capivaradb/internal/version"
)

// DefaultPoolPages is the default size of the buffer pool: 4096 pages of
// 8 kB, or 32 MB.
const DefaultPoolPages = 4096

// DB is a database. It implements pgwire.Handler.
type DB struct {
	// mu guards the catalog maps and, for now, every page: writers hold it
	// for a whole statement, readers while they scan.
	mu      sync.RWMutex
	pager   *storage.Pager
	closed  bool
	catalog *storage.Tree
	tables  map[string]*table
	indexes map[string]*index
}

// New returns an empty database held in memory. It uses the same storage
// engine as a database on disk, on top of an in-memory file.
func New() *DB {
	db, err := open(storage.NewMemFile(), DefaultPoolPages)
	if err != nil {
		panic(fmt.Sprintf("engine: opening an in-memory database: %v", err))
	}
	return db
}

// Open opens the database stored in the file at path, creating it if it
// does not exist. poolPages is the size of the buffer pool in pages.
func Open(path string, poolPages int) (*DB, error) {
	file, err := storage.OpenFile(path)
	if err != nil {
		return nil, err
	}
	db, err := open(file, poolPages)
	if err != nil {
		file.Close()
		return nil, err
	}
	return db, nil
}

func open(file storage.File, poolPages int) (*DB, error) {
	pager, err := storage.Open(file, poolPages)
	if err != nil {
		return nil, err
	}
	db := &DB{pager: pager, tables: make(map[string]*table), indexes: make(map[string]*index)}
	if root := pager.CatalogRoot(); root != 0 {
		db.catalog = storage.OpenTree(pager, root)
		return db, db.loadCatalog()
	}
	if db.catalog, err = storage.CreateTree(pager); err != nil {
		return nil, err
	}
	pager.SetCatalogRoot(db.catalog.Root())
	return db, pager.Flush()
}

// Checkpoint writes every modified page to the file and syncs it.
func (db *DB) Checkpoint() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.pager.Flush()
}

// Close checkpoints and closes the database file.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.closed = true
	return db.pager.Close()
}

// Stats reports the buffer pool counters and the size of the file in pages.
func (db *DB) Stats() (stats storage.Stats, pages, poolPages int) {
	return db.pager.Stats(), db.pager.PageCount(), db.pager.PoolPages()
}

type table struct {
	name string
	cols []column
	// pk lists the primary key columns; nil means rows are keyed by a
	// hidden row ID, handed out from nextRowID.
	pk        []int
	tree      *storage.Tree
	nextRowID int64
	// indexes are the secondary indexes, including the ones that back
	// UNIQUE constraints. The slice is replaced, never modified in place.
	indexes []*index
}

type column struct {
	name    string
	typ     sql.Type
	notNull bool
	// def computes the column's DEFAULT, already converted to the column
	// type; nil means the default is NULL. defSQL is its source text, which
	// is what the catalog stores.
	def    evalFn
	defSQL string
}

// index is a secondary index: a B+tree whose keys are the indexed columns
// followed by the key of the row in the table.
type index struct {
	name   string
	table  *table
	cols   []int
	unique bool
	tree   *storage.Tree
}

func (t *table) colIndex(name string) int {
	for i, c := range t.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// colNames joins the names of the given columns.
func (t *table) colNames(cols []int, sep string) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = t.cols[c].name
	}
	return strings.Join(names, sep)
}

// changes collects what a statement did, so that it can be undone or, at
// commit, finished.
type changes struct {
	// undo holds one function per change; rolling back runs them newest
	// first.
	undo []func()
	// onCommit holds work that must wait until the change is final, such
	// as freeing the pages of a dropped table: until then a rollback has
	// to be able to bring the table back.
	onCommit []func() error
}

// Session is the state of one client connection. It implements
// pgwire.Session.
type Session struct {
	db       *DB
	user     string
	database string
	vars     map[string]string

	inTx   bool
	failed bool
	// tx accumulates the changes of the open transaction block.
	tx changes
}

// NewSession implements pgwire.Handler.
func (db *DB) NewSession(params map[string]string) (pgwire.Session, error) {
	s := &Session{
		db:       db,
		user:     params["user"],
		database: params["database"],
		vars: map[string]string{
			"server_version":              version.PGCompat,
			"server_encoding":             "UTF8",
			"client_encoding":             "UTF8",
			"datestyle":                   "ISO, MDY",
			"timezone":                    "UTC",
			"integer_datetimes":           "on",
			"standard_conforming_strings": "on",
			"search_path":                 "public",
			"application_name":            params["application_name"],
			// Honest answer until MVCC lands: statements from other
			// sessions are visible as soon as they run, committed or not.
			// Requesting another level is accepted and changes nothing.
			"transaction_isolation": "read uncommitted",
		},
	}
	return s, nil
}

// Parse implements pgwire.Session.
func (s *Session) Parse(query string) ([]pgwire.Stmt, error) {
	stmts, err := sql.Parse(query)
	if err != nil {
		return nil, err
	}
	out := make([]pgwire.Stmt, len(stmts))
	for i, st := range stmts {
		out[i] = st
	}
	return out, nil
}

// TxStatus implements pgwire.Session.
func (s *Session) TxStatus() byte {
	switch {
	case s.failed:
		return 'E'
	case s.inTx:
		return 'T'
	}
	return 'I'
}

// OnError implements pgwire.Session: an error inside a transaction block
// poisons it until the client issues ROLLBACK.
func (s *Session) OnError() {
	if s.inTx {
		s.failed = true
	}
}

// Close implements pgwire.Session.
func (s *Session) Close() {
	if s.inTx {
		s.rollback()
	}
}

func (s *Session) begin() {
	s.inTx = true
}

// finish runs the work that was waiting for the changes to become final.
// The caller must hold db.mu.
func (ch *changes) finish() error {
	for _, fn := range ch.onCommit {
		if err := fn(); err != nil {
			return err
		}
	}
	return nil
}

// revert undoes the changes, newest first. The caller must hold db.mu.
func (ch *changes) revert() {
	for i := len(ch.undo) - 1; i >= 0; i-- {
		ch.undo[i]()
	}
}

func (s *Session) commit() error {
	s.db.mu.Lock()
	err := s.tx.finish()
	s.db.mu.Unlock()
	s.inTx, s.failed, s.tx = false, false, changes{}
	return err
}

func (s *Session) rollback() {
	s.db.mu.Lock()
	s.tx.revert()
	s.db.mu.Unlock()
	s.inTx, s.failed, s.tx = false, false, changes{}
}

// write runs a data-modifying statement under the write lock. fn records
// every change it makes. If fn fails, its changes are undone on the spot,
// so a statement either happens entirely or not at all; if it succeeds
// inside a transaction block, the record is kept for COMMIT or ROLLBACK.
func (s *Session) write(fn func(ch *changes) (string, error)) (*result, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var ch changes
	tag, err := fn(&ch)
	if err != nil {
		ch.revert()
		return nil, err
	}
	if s.inTx {
		s.tx.undo = append(s.tx.undo, ch.undo...)
		s.tx.onCommit = append(s.tx.onCommit, ch.onCommit...)
	} else if err := ch.finish(); err != nil {
		return nil, err
	}
	return &result{tag: tag}, nil
}

// lookup returns the named table. The caller must hold db.mu.
func (db *DB) lookup(ref sql.TableRef) (*table, error) {
	t := db.tables[ref.Name]
	if t == nil {
		return nil, pgerr.New(pgerr.UndefinedTable, "relation %q does not exist", ref.Name).At(ref.Pos)
	}
	return t, nil
}

// stillCurrent verifies that a table captured when a statement was prepared
// is the one the name refers to now. The caller must hold db.mu.
func (db *DB) stillCurrent(t *table) error {
	if db.tables[t.name] != t {
		return pgerr.New(pgerr.UndefinedTable,
			"relation %q was dropped or replaced after the statement was prepared", t.name)
	}
	return nil
}

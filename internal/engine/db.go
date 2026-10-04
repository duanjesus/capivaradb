// Package engine is the milestone-1 execution engine: tables are slices of
// rows held in memory behind one database-wide lock.
//
// It exists so that the wire protocol can be exercised end to end by real
// clients. It is deliberately naive — no persistence, no isolation between
// concurrent transactions, full scans only — and is replaced piece by piece
// by the storage engine, WAL, MVCC and planner of the later milestones. The
// part that is meant to last is the boundary: everything the protocol layer
// needs goes through the pgwire.Session interface.
package engine

import (
	"sync"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
	"github.com/duanjesus/capivaradb/internal/version"
)

// DB is an in-memory database. It implements pgwire.Handler.
type DB struct {
	// mu guards tables and everything reachable from it. Writers hold it
	// for a whole statement; readers only while copying the row slice.
	mu     sync.RWMutex
	tables map[string]*table
}

// New returns an empty database.
func New() *DB {
	return &DB{tables: make(map[string]*table)}
}

type table struct {
	name string
	cols []column
	rows []*row
}

type column struct {
	name    string
	typ     sql.Type
	notNull bool
	pk      bool
}

// row is a pointer so that a row keeps its identity across updates, which is
// what the undo log refers to. The vals slice itself is never modified in
// place: an update swaps in a new slice. Readers can therefore keep using a
// slice they copied out under the lock after releasing it.
type row struct {
	vals []any
}

func (t *table) colIndex(name string) int {
	for i, c := range t.cols {
		if c.name == name {
			return i
		}
	}
	return -1
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
	// undo holds one function per change made by the open transaction;
	// ROLLBACK runs them newest first.
	undo []func()
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
			// Honest answer for this milestone: statements from other
			// sessions are visible as soon as they run, committed or not.
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

func (s *Session) commit() {
	s.inTx, s.failed, s.undo = false, false, nil
}

func (s *Session) rollback() {
	s.db.mu.Lock()
	for i := len(s.undo) - 1; i >= 0; i-- {
		s.undo[i]()
	}
	s.db.mu.Unlock()
	s.inTx, s.failed, s.undo = false, false, nil
}

// write runs a data-modifying statement under the write lock. fn appends an
// undo function for every change it makes. If fn fails, its changes are
// undone on the spot, so a statement either happens entirely or not at all;
// if it succeeds inside a transaction block, the undo functions are kept for
// a possible ROLLBACK.
func (s *Session) write(fn func(undo *[]func()) (string, error)) (*result, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	var undo []func()
	tag, err := fn(&undo)
	if err != nil {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return nil, err
	}
	if s.inTx {
		s.undo = append(s.undo, undo...)
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

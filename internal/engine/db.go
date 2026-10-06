// Package engine binds and executes SQL on top of the storage layer.
//
// Rows live in B+trees (see store.go), the catalog is persistent, and every
// change goes through the write-ahead log, so a committed transaction
// survives a crash and an unfinished one leaves no trace. What is still
// deliberately naive is everything about concurrency and planning: one
// database-wide lock, no isolation between concurrent transactions, full
// scans and nested-loop joins only. Those are the subjects of the later
// milestones. Everything the protocol layer needs goes through
// pgwire.Session.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
	"github.com/duanjesus/capivaradb/internal/storage"
	"github.com/duanjesus/capivaradb/internal/version"
)

// DefaultPoolPages is the default size of the buffer pool: 4096 pages of
// 8 kB, or 32 MB.
const DefaultPoolPages = 4096

// DefaultCheckpointBytes is how much log accumulates before a checkpoint is
// taken on its own. A larger value means fewer checkpoints and a longer
// recovery.
const DefaultCheckpointBytes = 16 << 20

// autoVacuumThreshold is how many dead row versions a table accumulates
// before it is vacuumed after a commit.
const autoVacuumThreshold = 2000

// Options configures a database.
type Options struct {
	// PoolPages is the size of the buffer pool, in pages.
	PoolPages int
	// NoSync turns off every fsync. Commits then survive a crash of the
	// server but not of the machine. For tests and benchmarks.
	NoSync bool
	// CheckpointBytes is the amount of log that triggers a checkpoint.
	CheckpointBytes int64
}

func (o Options) withDefaults() Options {
	if o.PoolPages == 0 {
		o.PoolPages = DefaultPoolPages
	}
	if o.CheckpointBytes == 0 {
		o.CheckpointBytes = DefaultCheckpointBytes
	}
	return o
}

// DB is a database. It implements pgwire.Handler.
type DB struct {
	// mu guards the catalog maps and, for now, every page: writers hold it
	// for a whole statement, readers while they scan.
	mu      sync.RWMutex
	pager   *storage.Pager
	opts    Options
	closed  bool
	catalog *storage.Tree
	tables  map[string]*table
	indexes map[string]*index

	// txs knows the transactions in progress and the snapshots in use.
	txs txRegistry
	// writers holds the sessions with an open transaction that has changed
	// something.
	writers map[*Session]bool
}

// New returns an empty database held in memory. It uses the same storage
// engine and the same log as a database on disk, on top of in-memory files.
func New() *DB {
	db, err := OpenFiles(storage.NewMemFile(), storage.NewMemFile(), Options{})
	if err != nil {
		panic(fmt.Sprintf("engine: opening an in-memory database: %v", err))
	}
	return db
}

// Open opens the database stored in the file at path, creating it if it
// does not exist, and recovers it if it was not shut down cleanly. The
// write-ahead log is kept next to it, in path + ".wal".
func Open(path string, opts Options) (*DB, error) {
	file, err := storage.OpenFile(path)
	if err != nil {
		return nil, err
	}
	logFile, err := storage.OpenFile(path + ".wal")
	if err != nil {
		file.Close()
		return nil, err
	}
	// A new file's directory entry is not durable until the directory is
	// synced. Without this a crash could leave a database whose log
	// exists and whose data file does not, or the other way round.
	if !opts.NoSync {
		syncDir(filepath.Dir(path))
	}
	db, err := OpenFiles(file, logFile, opts)
	if err != nil {
		file.Close()
		logFile.Close()
		return nil, err
	}
	return db, nil
}

// syncDir flushes a directory's entries to disk. It is best-effort: Windows
// has no such operation and returns an error that is safe to ignore.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// OpenFiles opens a database on the given data and log files. It is what
// Open and New are built on, and what tests use to interpose their own
// files.
func OpenFiles(file, logFile storage.File, opts Options) (*DB, error) {
	opts = opts.withDefaults()
	pager, err := storage.Open(file, logFile, storage.Options{PoolPages: opts.PoolPages, NoSync: opts.NoSync})
	if err != nil {
		return nil, err
	}
	db := &DB{
		pager:   pager,
		opts:    opts,
		tables:  make(map[string]*table),
		indexes: make(map[string]*index),
		writers: make(map[*Session]bool),
		txs:     newTxRegistry(),
	}
	// Everything in the file was written by transactions that are over,
	// so every ID it contains must count as past: IDs carry on from the
	// log position, which is at least as large as any of them.
	db.txs.last = pager.LSN()
	if root := pager.CatalogRoot(); root != 0 {
		db.catalog = storage.OpenTree(pager, root)
		return db, db.loadCatalog()
	}
	if db.catalog, err = storage.CreateTree(pager, 0); err != nil {
		return nil, err
	}
	if err := pager.SetCatalogRoot(db.catalog.Root()); err != nil {
		return nil, err
	}
	return db, pager.Checkpoint()
}

// Checkpoint brings the data file up to date with the log, after which the
// log can be discarded if no transaction is in progress.
func (db *DB) Checkpoint() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.pager.Checkpoint()
}

// maybeCheckpoint takes a checkpoint if enough log has accumulated. The
// caller must hold db.mu.
func (db *DB) maybeCheckpoint() error {
	if db.pager.LogSize() < db.opts.CheckpointBytes || db.pager.ActiveTransactions() > 0 {
		return nil
	}
	return db.pager.Checkpoint()
}

// Close checkpoints and closes the database files.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.closed = true
	return db.pager.Close()
}

// Stats reports the storage counters and the size of the file in pages.
func (db *DB) Stats() (stats storage.Stats, pages, poolPages int) {
	return db.pager.Stats(), db.pager.PageCount(), db.pager.PoolPages()
}

// Recovery reports what recovery did when the database was opened.
func (db *DB) Recovery() storage.RecoveryInfo { return db.pager.Recovery() }

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
	// dead counts the versions deleted since the table was last vacuumed;
	// vacuumAt is the count at which it is vacuumed again.
	dead, vacuumAt int
	// stats are the statistics gathered by the last ANALYZE, nil if there
	// has been none. delta is the net number of rows added since, and mods
	// the number of rows changed in any way; neither is corrected for
	// rollbacks, which is good enough for an estimate.
	stats       *tableStats
	delta, mods int
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

// changes collects what a statement or a transaction did, so that it can be
// undone or, at commit, finished.
type changes struct {
	db   *DB
	sess *Session
	// tx is the transaction's ID; zero until it first changes something.
	tx uint64
	// undo lists how to reverse each change; rolling back runs it newest
	// first.
	undo []undoEntry
	// drops lists the trees to free once the transaction has committed.
	// Until then a rollback has to be able to bring them back.
	drops []uint32
}

// undoEntry reverses one change. A change to a tree is reversed through the
// log (rec, logged at lsn), so that a crash in the middle of a rollback
// neither repeats nor skips it. A change to the in-memory catalog is
// reversed by mem; after a crash the catalog is simply read again.
type undoEntry struct {
	lsn uint64
	rec storage.UndoRec
	mem func()
}

// begin gives the transaction an ID the first time it needs one, which is
// when it first changes something: a transaction that only reads never
// appears in anyone's snapshot.
func (ch *changes) begin() uint64 {
	if ch.tx == 0 {
		ch.tx = ch.db.startTx()
		// From now on the session's snapshots must recognise the
		// transaction's own versions.
		if s := ch.sess; s != nil {
			if s.snap != nil {
				s.snap.self = ch.tx
			}
			if s.txSnap != nil {
				s.txSnap.self = ch.tx
			}
		}
	}
	return ch.tx
}

// put stores key in tree, where it must not exist yet, logging how to take
// it out again.
func (ch *changes) put(tree *storage.Tree, key, val []byte) error {
	rec := storage.UndoRec{Tx: ch.begin(), Root: tree.Root(), Kind: storage.UndoDelete, Key: key}
	ch.undo = append(ch.undo, undoEntry{lsn: ch.db.pager.LogUndo(rec), rec: rec})
	return storageError(tree.Put(key, val))
}

// del removes key from tree, logging how to put it back with its old value.
func (ch *changes) del(tree *storage.Tree, key, old []byte) error {
	rec := storage.UndoRec{Tx: ch.begin(), Root: tree.Root(), Kind: storage.UndoPut, Key: key, Val: old}
	ch.undo = append(ch.undo, undoEntry{lsn: ch.db.pager.LogUndo(rec), rec: rec})
	_, err := tree.Delete(key)
	return err
}

// replace overwrites the value of key in tree, logging how to restore the
// old one.
func (ch *changes) replace(tree *storage.Tree, key, old, val []byte) error {
	rec := storage.UndoRec{Tx: ch.begin(), Root: tree.Root(), Kind: storage.UndoPut, Key: key, Val: old}
	ch.undo = append(ch.undo, undoEntry{lsn: ch.db.pager.LogUndo(rec), rec: rec})
	return storageError(tree.Put(key, val))
}

// createTree allocates a tree that is freed again if the changes are undone.
func (ch *changes) createTree() (*storage.Tree, error) {
	tree, lsn, err := storage.CreateTreeLogged(ch.db.pager, ch.begin())
	if err != nil {
		return nil, err
	}
	rec := storage.UndoRec{Tx: ch.tx, Root: tree.Root(), Kind: storage.UndoDropTree}
	ch.undo = append(ch.undo, undoEntry{lsn: lsn, rec: rec})
	return tree, nil
}

// onUndo registers a change to in-memory state to reverse on rollback.
func (ch *changes) onUndo(fn func()) {
	ch.undo = append(ch.undo, undoEntry{mem: fn})
}

// revert undoes the changes, newest first. The caller must hold db.mu.
func (ch *changes) revert() {
	for i := len(ch.undo) - 1; i >= 0; i-- {
		if e := ch.undo[i]; e.mem != nil {
			e.mem()
		} else {
			must(ch.db.pager.ApplyUndo(e.rec, e.lsn))
		}
	}
	ch.undo = nil
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

	// defaultLevel applies to transactions that do not choose a level;
	// level is that of the open transaction block.
	defaultLevel isoLevel
	level        isoLevel
	// snap is the snapshot of the statement being executed. Under read
	// committed it is new for every statement; under repeatable read it
	// is txSnap, taken by the transaction's first statement and kept
	// (and held against vacuum) until the transaction ends.
	snap        *snapshot
	txSnap      *snapshot
	releaseSnap func()
	// started is set once a statement has run in the transaction block,
	// after which its isolation level can no longer be changed.
	started bool
	// blocked is set while a statement waits for another transaction to
	// end. Nothing depends on it but tests, which need to tell "still
	// running" from "waiting".
	blocked atomic.Bool
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
			// Planner switches, named after PostgreSQL's. They exist to
			// compare plans: turning them off gives the plan the planner
			// would otherwise have rejected.
			"enable_indexscan":    "on",
			"join_collapse_limit": "8",
		},
	}
	s.tx = changes{db: db, sess: s}
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
	if s.inTx && !s.failed {
		s.failed = true
		// The transaction is over in all but name: its work is undone
		// now, not when the client gets round to saying ROLLBACK, so
		// that nobody is left waiting for a transaction that can no
		// longer commit.
		s.db.mu.Lock()
		s.abandon()
		s.db.mu.Unlock()
	}
}

// Close implements pgwire.Session.
func (s *Session) Close() {
	if s.inTx {
		s.rollback()
	}
}

func (s *Session) begin(level string) {
	s.inTx = true
	s.level = s.defaultLevel
	if level != "" {
		s.level = parseLevel(level)
	}
}

// isolation is the level that applies to the next statement.
func (s *Session) isolation() isoLevel {
	if s.inTx {
		return s.level
	}
	return s.defaultLevel
}

// takeSnapshot sets s.snap for the statement about to run and returns the
// function to call when the statement is over. The caller must hold db.mu,
// shared or exclusive.
func (s *Session) takeSnapshot() (release func()) {
	s.started = s.inTx
	if s.inTx && s.level == repeatableRead {
		if s.txSnap == nil {
			s.txSnap = s.db.newSnapshot(s.tx.tx)
			s.releaseSnap = s.db.hold(s.txSnap)
		}
		s.snap = s.txSnap
		return func() {}
	}
	s.snap = s.db.newSnapshot(s.tx.tx)
	return s.db.hold(s.snap)
}

// read runs a query under a snapshot of its own (or the transaction's).
func (s *Session) read(fn func() ([][]any, error)) ([][]any, error) {
	s.db.mu.RLock()
	release := s.takeSnapshot()
	s.db.mu.RUnlock()
	defer release()
	return fn()
}

// endTx forgets the transaction block. The caller must hold db.mu.
func (s *Session) endTx() {
	if s.tx.tx != 0 {
		s.db.finishTx(s.tx.tx)
	}
	if s.releaseSnap != nil {
		s.releaseSnap()
	}
	delete(s.db.writers, s)
	s.inTx, s.failed = false, false
	s.tx = changes{db: s.db, sess: s}
	s.snap, s.txSnap, s.releaseSnap, s.started = nil, nil, nil, false
}

// commit makes the transaction block durable. When it returns without
// error, the transaction survives a crash, and from that moment its
// versions are visible to every snapshot taken afterwards.
func (s *Session) commit() error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	defer s.endTx()
	if s.tx.tx == 0 {
		return nil // it changed nothing
	}
	if err := s.db.pager.Commit(s.tx.tx, s.tx.drops); err != nil {
		return err
	}
	// The transaction is over as far as everyone else is concerned before
	// the housekeeping below, which may be slow, begins.
	s.db.finishTx(s.tx.tx)
	s.tx.tx = 0
	return s.db.housekeeping()
}

// abandon undoes the transaction's work and lets go of everything it
// held, without ending the transaction block. The caller must hold db.mu.
func (s *Session) abandon() {
	s.tx.revert()
	if s.tx.tx != 0 {
		s.db.pager.End(s.tx.tx)
		s.db.finishTx(s.tx.tx)
	}
	if s.releaseSnap != nil {
		s.releaseSnap()
	}
	delete(s.db.writers, s)
	s.tx = changes{db: s.db, sess: s}
	s.snap, s.txSnap, s.releaseSnap = nil, nil, nil
}

func (s *Session) rollback() {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.abandon()
	s.endTx()
}

// write runs a data-modifying statement. fn records every change it makes.
// If fn fails, its changes are undone on the spot, so a statement either
// happens entirely or not at all. Outside a transaction block the statement
// is a transaction of its own and is committed before write returns; inside
// one, the record is kept for COMMIT or ROLLBACK.
//
// If fn runs into the uncommitted work of another transaction, the
// statement is undone, the database lock is released until that
// transaction ends, and the statement starts over.
func (s *Session) write(ctx context.Context, fn func(ch *changes) (string, error)) (*result, error) {
	for {
		tag, wait, err := s.attempt(fn)
		if wait == nil {
			if err != nil {
				return nil, err
			}
			return &result{tag: tag}, nil
		}
		s.blocked.Store(true)
		select {
		case <-wait:
		case <-ctx.Done():
			err = ctx.Err()
		}
		s.blocked.Store(false)
		s.db.mu.Lock()
		delete(s.db.txs.waiting, s.tx.tx)
		s.db.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

// attempt runs fn once under the database lock. If the statement has to
// wait for another transaction, it returns the channel that is closed when
// that transaction ends.
func (s *Session) attempt(fn func(ch *changes) (string, error)) (tag string, wait <-chan struct{}, err error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	release := s.takeSnapshot()
	defer release()

	ch := changes{db: s.db, sess: s}
	if s.inTx {
		ch.tx = s.tx.tx
	}
	tag, err = fn(&ch)
	if err != nil {
		ch.revert()
		if s.inTx {
			s.tx.tx = ch.tx
			if ch.tx != 0 {
				s.db.writers[s] = true
			}
		} else if ch.tx != 0 {
			// The statement was a transaction of its own, now undone.
			s.db.pager.End(ch.tx)
			s.db.finishTx(ch.tx)
			s.snap.self = 0
		}
		var w *waitError
		if !errors.As(err, &w) {
			return "", nil, err
		}
		done, running := s.db.txs.active[w.tx]
		if !running {
			// It ended between the check and here; just go again.
			closed := make(chan struct{})
			close(closed)
			return "", closed, nil
		}
		// Only a transaction that has changed something can be waited
		// for, so only then can waiting close a cycle.
		if me := s.tx.tx; me != 0 {
			if s.db.deadlocked(me, w.tx) {
				return "", nil, errDeadlock()
			}
			s.db.txs.waiting[me] = w.tx
		}
		return "", done, nil
	}

	if s.inTx {
		s.tx.tx = ch.tx
		s.tx.undo = append(s.tx.undo, ch.undo...)
		s.tx.drops = append(s.tx.drops, ch.drops...)
		if ch.tx != 0 {
			s.db.writers[s] = true
		}
		return tag, nil, nil
	}
	if ch.tx != 0 {
		if err := s.db.pager.Commit(ch.tx, ch.drops); err != nil {
			return "", nil, err
		}
		s.db.finishTx(ch.tx)
		if err := s.db.housekeeping(); err != nil {
			return "", nil, err
		}
	}
	return tag, nil, nil
}

// housekeeping does the periodic work that follows a commit: vacuuming
// tables that have accumulated dead versions, refreshing the statistics of
// tables that have changed a lot, and checkpointing if enough log has built
// up. The caller must hold db.mu.
func (db *DB) housekeeping() error {
	for _, name := range db.tableNames() {
		t := db.tables[name]
		if t.dead >= max(t.vacuumAt, autoVacuumThreshold) {
			err := db.internalTx(func(ch *changes) error {
				removed, err := db.vacuum(t, ch)
				// What could not be removed is still wanted by some
				// snapshot; wait for as many new dead versions again
				// before retrying.
				t.dead -= removed
				t.vacuumAt = t.dead + autoVacuumThreshold
				return err
			})
			if err != nil {
				return err
			}
		}
		if t.needsAnalyze() {
			err := db.internalTx(func(ch *changes) error {
				st, err := db.analyze(t, db.newSnapshot(0))
				if err != nil {
					return err
				}
				return db.storeStats(t, st, ch)
			})
			if err != nil {
				return err
			}
		}
	}
	return db.maybeCheckpoint()
}

// internalTx runs maintenance work as a transaction of its own. The caller
// must hold db.mu.
func (db *DB) internalTx(fn func(ch *changes) error) error {
	ch := changes{db: db}
	err := fn(&ch)
	if err != nil {
		ch.revert()
	}
	if ch.tx != 0 {
		if err == nil {
			err = db.pager.Commit(ch.tx, nil)
		} else {
			db.pager.End(ch.tx)
		}
		db.finishTx(ch.tx)
	}
	return err
}

// othersWriting reports whether a session other than s has uncommitted
// changes. The caller must hold db.mu.
func (db *DB) othersWriting(s *Session) bool {
	for other := range db.writers {
		if other != s {
			return true
		}
	}
	return false
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

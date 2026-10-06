package engine

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/duanjesus/capivaradb/internal/pgerr"
)

// Multi-version concurrency control.
//
// A row is not overwritten: an UPDATE writes a new version next to the old
// one, and a DELETE only marks the version as deleted. Every version records
// the transaction that created it (xmin) and, once it has been deleted or
// superseded, the transaction that did that (xmax). A reader does not look
// at "the row" but at the versions, and picks the one its snapshot can see.
//
// That is what lets a reader ignore writers entirely: a transaction that is
// half-way through changing a table cannot disturb a query, because the
// query simply does not see its versions. And it is what gives a transaction
// a stable view: its snapshot decides what it sees, however much is
// committed by others in the meantime.
//
// What follows is PostgreSQL's design. Two things differ. The versions live
// in the table's B+tree, clustered by primary key, rather than in a heap.
// And there is no commit log recording which transactions aborted: a
// rollback physically removes what the transaction wrote (through the undo
// records of the write-ahead log), so every transaction ID found in the
// data belongs either to a transaction still in progress or to one that
// committed.

// A snapshot answers one question: was this transaction's work committed
// when the snapshot was taken?
type snapshot struct {
	// next is the first transaction ID not yet handed out when the
	// snapshot was taken. Anything from next on started later.
	next uint64
	// active holds the transactions that were in progress at that moment.
	// They may have committed since; to this snapshot they never did.
	active map[uint64]bool
	// self is the reader's own transaction, whose changes it does see.
	// It is zero until the transaction makes its first change.
	self uint64
}

// sees reports whether the snapshot treats tx as committed.
func (sn *snapshot) sees(tx uint64) bool {
	return tx == sn.self || (tx < sn.next && !sn.active[tx])
}

// visible reports whether a version created by xmin and deleted by xmax
// (zero if not deleted) exists as far as the snapshot is concerned.
func (sn *snapshot) visible(xmin, xmax uint64) bool {
	return sn.sees(xmin) && (xmax == 0 || !sn.sees(xmax))
}

// isoLevel is a transaction isolation level.
type isoLevel uint8

const (
	// readCommitted takes a new snapshot for every statement: each
	// statement sees everything committed before it began.
	readCommitted isoLevel = iota
	// repeatableRead takes one snapshot for the whole transaction. This
	// is snapshot isolation; it is also what SERIALIZABLE gets, which is
	// weaker than the name promises (see docs/mvcc.md on write skew).
	repeatableRead
)

func (l isoLevel) String() string {
	if l == repeatableRead {
		return "repeatable read"
	}
	return "read committed"
}

// parseLevel maps the SQL names to the two levels that exist. READ
// UNCOMMITTED is read committed, as in PostgreSQL: nothing here can read
// uncommitted data.
func parseLevel(name string) isoLevel {
	switch name {
	case "repeatable read", "serializable":
		return repeatableRead
	}
	return readCommitted
}

// txRegistry is the part of the database that knows which transactions are
// in progress. Everything but the snapshot set is guarded by db.mu.
type txRegistry struct {
	// last is the most recent transaction ID handed out.
	last uint64
	// active maps each transaction in progress to a channel that is
	// closed when it ends, which is how others wait for it.
	active map[uint64]chan struct{}
	// waiting records who waits for whom, to detect deadlocks.
	waiting map[uint64]uint64

	// snaps holds the snapshots in use. Vacuum may only remove a version
	// that none of them can see. It has its own lock because readers
	// register theirs without holding db.mu exclusively.
	snapMu sync.Mutex
	snaps  map[*snapshot]int
}

func newTxRegistry() txRegistry {
	return txRegistry{
		active:  make(map[uint64]chan struct{}),
		waiting: make(map[uint64]uint64),
		snaps:   make(map[*snapshot]int),
	}
}

// startTx hands out a transaction ID. The caller must hold db.mu.
//
// IDs must keep growing across restarts, because versions written before a
// restart carry the IDs of their transactions. The log's sequence number
// does exactly that and is already durable, so an ID is never smaller than
// the log position at which it was issued.
func (db *DB) startTx() uint64 {
	id := max(db.txs.last+1, db.pager.LSN())
	db.txs.last = id
	db.txs.active[id] = make(chan struct{})
	return id
}

// finishTx marks a transaction as over, committed or rolled back, and wakes
// whoever was waiting for it. The caller must hold db.mu.
func (db *DB) finishTx(id uint64) {
	if done, ok := db.txs.active[id]; ok {
		delete(db.txs.active, id)
		close(done)
	}
}

func (db *DB) isActive(tx uint64) bool {
	_, ok := db.txs.active[tx]
	return ok
}

// newSnapshot captures which transactions are in progress right now. The
// caller must hold db.mu, shared or exclusive.
func (db *DB) newSnapshot(self uint64) *snapshot {
	sn := &snapshot{next: db.txs.last + 1, self: self}
	if len(db.txs.active) > 0 {
		sn.active = make(map[uint64]bool, len(db.txs.active))
		for tx := range db.txs.active {
			if tx != self {
				sn.active[tx] = true
			}
		}
	}
	return sn
}

// hold registers a snapshot as in use and returns the function that
// releases it.
func (db *DB) hold(sn *snapshot) func() {
	db.txs.snapMu.Lock()
	db.txs.snaps[sn]++
	db.txs.snapMu.Unlock()
	return func() {
		db.txs.snapMu.Lock()
		if db.txs.snaps[sn]--; db.txs.snaps[sn] <= 0 {
			delete(db.txs.snaps, sn)
		}
		db.txs.snapMu.Unlock()
	}
}

// goneForEveryone reports whether a version whose deleter has committed is
// invisible to every snapshot in use. Snapshots taken from now on will see
// the deletion, so such a version can never be wanted again. Note that an
// old snapshot protects only the versions it can actually see: a version
// created and deleted entirely after it was taken is not one of them.
func (db *DB) goneForEveryone(xmin, xmax uint64) bool {
	db.txs.snapMu.Lock()
	defer db.txs.snapMu.Unlock()
	for sn := range db.txs.snaps {
		if sn.visible(xmin, xmax) {
			return false
		}
	}
	return true
}

// waitError is returned by a write that ran into the uncommitted work of
// another transaction. Whether that work will stand is not known yet, so
// the statement is undone, waits for the other transaction to end, and
// runs again.
type waitError struct {
	tx uint64
}

func (w *waitError) Error() string {
	return fmt.Sprintf("waiting for transaction %d", w.tx)
}

// deadlocked reports whether making waiter wait for holder would close a
// cycle: holder (or whoever it waits for, and so on) is already waiting for
// waiter. The caller must hold db.mu.
func (db *DB) deadlocked(waiter, holder uint64) bool {
	for tx, steps := holder, 0; steps <= len(db.txs.waiting); steps++ {
		next, waits := db.txs.waiting[tx]
		if !waits {
			return false
		}
		if next == waiter {
			return true
		}
		tx = next
	}
	return true
}

var (
	errSerialization = func() error {
		return pgerr.New(pgerr.SerializationFailure, "could not serialize access due to concurrent update")
	}
	errDeadlock = func() error {
		return pgerr.New(pgerr.DeadlockDetected, "deadlock detected")
	}
)

// ---- how versions are stored ----
//
// A version's key in the table's tree is the row's key (primary key, or
// row ID) followed by eight bytes: the bitwise complement of xmin. The
// versions of one row are therefore adjacent, newest first, and a row's
// history is a short range scan. The value is xmax, eight bytes, followed
// by the tuple.

const versionTagSize = 8

func versionKey(base []byte, xmin uint64) []byte {
	key := make([]byte, len(base), len(base)+versionTagSize)
	copy(key, base)
	return binary.BigEndian.AppendUint64(key, ^xmin)
}

// splitVersionKey separates a version's key into the row's key and xmin.
func splitVersionKey(key []byte) (base []byte, xmin uint64) {
	n := len(key) - versionTagSize
	return key[:n], ^binary.BigEndian.Uint64(key[n:])
}

func versionValue(xmax uint64, tuple []byte) []byte {
	val := make([]byte, 0, 8+len(tuple))
	val = binary.BigEndian.AppendUint64(val, xmax)
	return append(val, tuple...)
}

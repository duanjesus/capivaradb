package storage

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// This file is the transactional side of the log: what a transaction
// records so that it can be undone, how it commits, and how the database is
// put back together after a crash.
//
// The scheme is ARIES, reduced to what a single-writer engine needs:
//
//   - Redo is physical. Page changes are logged as images or byte ranges
//     (pager.go) and replayed blindly, in order, whatever transaction they
//     belonged to: recovery first "repeats history".
//   - Undo is logical. A transaction logs, before each change to a tree,
//     how to reverse it in terms of keys: "delete this key", "put this key
//     back with this value". Reversing through the tree rather than through
//     page images is what makes undo work after the page has since been
//     split or its neighbours changed.
//   - Undoing is itself a logged change, and the log record of the page
//     changes says which undo record it carried out. An undo is therefore
//     never applied twice, even if recovery itself is interrupted.

// Kinds of undo.
const (
	// UndoDelete removes Key from the tree: the reverse of an insert.
	UndoDelete = 1
	// UndoPut stores Val under Key: the reverse of a delete.
	UndoPut = 2
	// UndoDropTree frees the tree: the reverse of creating it.
	UndoDropTree = 3
)

// UndoRec says how to reverse one change made by a transaction.
type UndoRec struct {
	Tx   uint64
	Root uint32 // root page of the tree concerned
	Kind byte
	Key  []byte
	Val  []byte
}

// RecoveryInfo reports what recovery did when the database was opened.
type RecoveryInfo struct {
	// Records is the number of log records found. Zero means the last
	// shutdown was clean.
	Records int
	// PagesRedone counts page changes replayed into the data file.
	PagesRedone int
	// TornPages counts pages that were unreadable and were rebuilt from
	// their image in the log.
	TornPages int
	// RolledBack is the number of unfinished transactions undone.
	RolledBack int
	// UndoApplied counts the individual changes reversed.
	UndoApplied int
	// Completed is the number of committed transactions whose remaining
	// work (freeing dropped trees) had to be finished.
	Completed int
	// MetaRebuilt is set if the meta page was unreadable and its contents
	// were taken from the log.
	MetaRebuilt bool
}

// Recovery returns what happened when this database was opened.
func (p *Pager) Recovery() RecoveryInfo { return p.recovery }

// LogUndo records how to reverse a change the transaction is about to make.
// It must be called before the change, so that the log holds the way back
// before it holds the change itself. It returns the record's LSN, which
// ApplyUndo needs.
func (p *Pager) LogUndo(u UndoRec) uint64 {
	rec := make([]byte, 0, 24+len(u.Key)+len(u.Val))
	rec = binary.BigEndian.AppendUint64(rec, u.Tx)
	rec = binary.BigEndian.AppendUint32(rec, u.Root)
	rec = append(rec, u.Kind)
	rec = binary.BigEndian.AppendUint32(rec, uint32(len(u.Key)))
	rec = append(rec, u.Key...)
	rec = append(rec, u.Val...)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[u.Tx] = true
	return p.appendLog(recUndo, rec)
}

func decodeUndo(payload []byte) (UndoRec, bool) {
	if len(payload) < 17 {
		return UndoRec{}, false
	}
	u := UndoRec{
		Tx:   binary.BigEndian.Uint64(payload),
		Root: binary.BigEndian.Uint32(payload[8:]),
		Kind: payload[12],
	}
	klen := int(binary.BigEndian.Uint32(payload[13:]))
	if klen > len(payload)-17 {
		return UndoRec{}, false
	}
	u.Key = payload[17 : 17+klen]
	u.Val = payload[17+klen:]
	return u, true
}

// Commit makes the transaction durable: once it returns, the transaction's
// changes survive a crash. drops lists the trees to free now that the
// transaction can no longer be undone; they are part of the commit record
// so that recovery can finish the job if the crash comes before they are
// all freed.
func (p *Pager) Commit(tx uint64, drops []uint32) error {
	rec := binary.BigEndian.AppendUint64(nil, tx)
	for _, root := range drops {
		rec = binary.BigEndian.AppendUint32(rec, root)
	}
	p.mu.Lock()
	if p.broken != nil {
		p.mu.Unlock()
		return p.broken
	}
	p.active[tx] = true
	p.appendLog(recCommit, rec)
	// This sync is the moment of commit.
	err := p.wal.flush()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	for _, root := range drops {
		if err := p.dropCommitted(tx, root); err != nil {
			return err
		}
	}
	p.End(tx)
	return nil
}

// dropCommitted frees a tree dropped by a committed transaction, noting in
// the same log record that it has been done.
func (p *Pager) dropCommitted(tx uint64, root uint32) error {
	p.mu.Lock()
	p.nextNote = note{kind: noteDropped, tx: tx, arg: uint64(root)}
	p.mu.Unlock()
	return OpenTree(p, root).Drop()
}

// End records that nothing remains to be done for the transaction: it
// committed and its dropped trees are freed, or it was rolled back and
// every change is reversed.
func (p *Pager) End(tx uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active[tx] {
		p.appendLog(recEnd, binary.BigEndian.AppendUint64(nil, tx))
		delete(p.active, tx)
	}
}

// ApplyUndo carries out the undo record that was logged at lsn. The page
// changes and the fact that this record is now undone go into the log
// together.
func (p *Pager) ApplyUndo(u UndoRec, lsn uint64) error {
	p.mu.Lock()
	p.nextNote = note{kind: noteUndone, tx: u.Tx, arg: lsn}
	p.mu.Unlock()
	t := OpenTree(p, u.Root)
	switch u.Kind {
	case UndoDelete:
		_, err := t.Delete(u.Key)
		return err
	case UndoPut:
		return t.Put(u.Key, u.Val)
	case UndoDropTree:
		return t.Drop()
	}
	return fmt.Errorf("%w: unknown undo kind %d", ErrCorrupt, u.Kind)
}

// txState is what recovery learns about one transaction from the log.
type txState struct {
	undo      []undoItem
	undone    map[uint64]bool // LSNs of undo records already carried out
	committed bool
	drops     []uint32
	dropped   map[uint32]bool
	ended     bool
}

type undoItem struct {
	lsn uint64
	rec UndoRec
}

// recover brings the database to a consistent state from the log records
// that survived. It has the three passes of ARIES.
func (p *Pager) recover(recs []walRecord, metaOK bool) error {
	p.recovery.Records = len(recs)

	// Analysis: one pass over the log to find out, for every transaction,
	// what it did and how far it got.
	txs := make(map[uint64]*txState)
	state := func(tx uint64) *txState {
		st := txs[tx]
		if st == nil {
			st = &txState{undone: make(map[uint64]bool), dropped: make(map[uint32]bool)}
			txs[tx] = st
		}
		return st
	}
	sawPages := false
	for _, r := range recs {
		switch r.typ {
		case recPages:
			sawPages = true
			if len(r.payload) < 33 {
				return fmt.Errorf("%w: malformed log record at LSN %d", ErrCorrupt, r.lsn)
			}
			kind, tx, arg := r.payload[0], binary.BigEndian.Uint64(r.payload[1:]), binary.BigEndian.Uint64(r.payload[9:])
			switch kind {
			case noteUndone:
				state(tx).undone[arg] = true
			case noteDropped:
				state(tx).dropped[uint32(arg)] = true
			case noteCreated:
				st := state(tx)
				st.undo = append(st.undo, undoItem{r.lsn, UndoRec{Tx: tx, Root: uint32(arg), Kind: UndoDropTree}})
			}
		case recUndo:
			u, ok := decodeUndo(r.payload)
			if !ok {
				return fmt.Errorf("%w: malformed undo record at LSN %d", ErrCorrupt, r.lsn)
			}
			st := state(u.Tx)
			st.undo = append(st.undo, undoItem{r.lsn, u})
		case recCommit:
			st := state(binary.BigEndian.Uint64(r.payload))
			st.committed = true
			for b := r.payload[8:]; len(b) >= 4; b = b[4:] {
				st.drops = append(st.drops, binary.BigEndian.Uint32(b))
			}
		case recEnd:
			state(binary.BigEndian.Uint64(r.payload)).ended = true
		}
	}
	if !metaOK {
		if !sawPages {
			return fmt.Errorf("%w: the meta page is unreadable and the log cannot rebuild it", ErrCorrupt)
		}
		p.recovery.MetaRebuilt = true
	}

	// Redo: repeat history. Every page change in the log is applied again
	// unless the page on disk already has it, which its LSN tells.
	p.recovering = true
	for _, r := range recs {
		if r.typ == recPages {
			if err := p.redo(r); err != nil {
				return err
			}
		}
	}
	p.recovering = false

	// Undo: reverse what unfinished transactions did, newest change first
	// across all of them, skipping what was already reversed before the
	// crash.
	var pending []undoItem
	for tx, st := range txs {
		if st.ended {
			continue
		}
		p.active[tx] = true
		if st.committed {
			continue
		}
		p.recovery.RolledBack++
		for _, item := range st.undo {
			if !st.undone[item.lsn] {
				pending = append(pending, item)
			}
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].lsn > pending[j].lsn })
	for _, item := range pending {
		if err := p.ApplyUndo(item.rec, item.lsn); err != nil {
			return fmt.Errorf("storage: undoing transaction %d during recovery: %w", item.rec.Tx, err)
		}
		p.recovery.UndoApplied++
	}

	// Finish committed transactions that were interrupted while freeing
	// the trees they dropped, then close every transaction.
	for tx, st := range txs {
		if st.ended {
			continue
		}
		if st.committed {
			p.recovery.Completed++
			for _, root := range st.drops {
				if !st.dropped[root] {
					if err := p.dropCommitted(tx, root); err != nil {
						return fmt.Errorf("storage: finishing transaction %d during recovery: %w", tx, err)
					}
				}
			}
		}
		p.End(tx)
	}
	return nil
}

// redo applies one page-change record.
func (p *Pager) redo(r walRecord) error {
	b := r.payload
	bad := func() error {
		return fmt.Errorf("%w: malformed log record at LSN %d", ErrCorrupt, r.lsn)
	}
	p.mu.Lock()
	p.pageCount = binary.BigEndian.Uint32(b[17:])
	p.freeList = binary.BigEndian.Uint32(b[21:])
	p.catalogRoot = binary.BigEndian.Uint32(b[25:])
	p.mu.Unlock()
	count := int(binary.BigEndian.Uint32(b[29:]))
	b = b[33:]

	for n := 0; n < count; n++ {
		if len(b) < 5 {
			return bad()
		}
		id, kind := binary.BigEndian.Uint32(b), b[4]
		b = b[5:]

		// A page that cannot be read — beyond the end of the file, or
		// failing its checksum because the crash tore it — is treated as
		// having no contents. Its first record since the checkpoint is a
		// full image, which restores it.
		pg, readable, err := p.fetchForRedo(id)
		if err != nil {
			return err
		}
		current := binary.BigEndian.Uint64(pg.Data[offLSN:])
		apply := !readable || current < r.lsn

		switch kind {
		case 1:
			if len(b) < PageSize {
				pg.Unpin(false)
				return bad()
			}
			if apply {
				copy(pg.Data, b[:PageSize])
			}
			b = b[PageSize:]
		case 2:
			if len(b) < 2 {
				pg.Unpin(false)
				return bad()
			}
			ranges := int(binary.BigEndian.Uint16(b))
			b = b[2:]
			// Without a base to apply them to, byte ranges mean nothing;
			// the image that follows later in the log will set the page.
			apply = apply && readable
			for ; ranges > 0; ranges-- {
				if len(b) < 4 {
					pg.Unpin(false)
					return bad()
				}
				off, size := int(binary.BigEndian.Uint16(b)), int(binary.BigEndian.Uint16(b[2:]))
				if off+size > PageSize || len(b) < 4+size {
					pg.Unpin(false)
					return bad()
				}
				if apply {
					copy(pg.Data[off:], b[4:4+size])
				}
				b = b[4+size:]
			}
		default:
			pg.Unpin(false)
			return bad()
		}
		if apply {
			binary.BigEndian.PutUint64(pg.Data[offLSN:], r.lsn)
			p.recovery.PagesRedone++
		}
		pg.Unpin(apply)
	}
	return nil
}

// fetchForRedo pins a page for recovery. Unlike Fetch it tolerates a page
// that cannot be read, returning it zeroed with readable false.
func (p *Pager) fetchForRedo(id uint32) (pg *Page, readable bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, cached := p.lookup[id]; cached {
		pg, err = p.fetchLocked(id, true)
		// A cached page was either read successfully or already rebuilt.
		return pg, err == nil && pg.Data[offType] != 0, err
	}
	if pg, err = p.fetchLocked(id, true); err == nil {
		return pg, true, nil
	}
	p.recovery.TornPages++
	pg, err = p.fetchLocked(id, false)
	return pg, false, err
}

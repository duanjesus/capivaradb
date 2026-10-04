package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// The write-ahead log is an append-only file of records. Its rule is in its
// name: a change is described in the log, and the log made durable, before
// the changed page may be written to the data file. After a crash the log is
// therefore always at least as new as the data file, and replaying it
// brings every page up to date.
//
// A record on disk:
//
//	length   uint32   of the payload
//	crc      uint32   CRC-32C of everything after this field
//	lsn      uint64   log sequence number: the record's logical position
//	type     uint8
//	payload
//
// The file starts with an eight-byte magic. A record whose CRC does not
// match marks the end of the log: it is the torn tail of a write that was
// interrupted, and nothing after it can be trusted.
const (
	walMagic      = "CAPIWAL1"
	walHeaderSize = 17

	recPages  = 1 // page changes of one B+tree operation, applied atomically
	recUndo   = 2 // how to undo a logical change, should its transaction fail
	recCommit = 3 // the transaction is committed once this is durable
	recEnd    = 4 // nothing remains to be done for the transaction
)

type wal struct {
	file   File
	noSync bool
	// buf holds records appended but not yet written to the file.
	buf []byte
	// written is the size of the file.
	written int64
	// next is the LSN the next record will get. LSNs grow by the size of
	// each record, so they also measure how much log has been produced.
	next uint64
	// durable is the LSN up to which the log is known to be on disk:
	// every record with a smaller LSN survives a crash.
	durable uint64
}

// walRecord is a record read back during recovery.
type walRecord struct {
	lsn     uint64
	typ     byte
	payload []byte
}

// openWAL reads the log file and returns its valid records. Anything after
// the last valid record is cut off, so that new records follow directly.
func openWAL(file File, noSync bool) (*wal, []walRecord, error) {
	w := &wal{file: file, noSync: noSync, next: 1, durable: 1}
	size, err := file.Size()
	if err != nil {
		return nil, nil, err
	}
	if size < int64(len(walMagic)) {
		// New, or cut short while being created: start it afresh.
		if err := w.reset(); err != nil {
			return nil, nil, err
		}
		return w, nil, nil
	}
	data := make([]byte, size)
	if _, err := file.ReadAt(data, 0); err != nil && err != io.EOF {
		return nil, nil, err
	}
	if string(data[:len(walMagic)]) != walMagic {
		return nil, nil, fmt.Errorf("%w: the log file is not a CapivaraDB log", ErrCorrupt)
	}

	var recs []walRecord
	off := len(walMagic)
	for off+walHeaderSize <= len(data) {
		length := int(binary.BigEndian.Uint32(data[off:]))
		end := off + walHeaderSize + length
		if length < 0 || end > len(data) {
			break
		}
		if crc32.Checksum(data[off+8:end], castagnoli) != binary.BigEndian.Uint32(data[off+4:]) {
			break
		}
		lsn := binary.BigEndian.Uint64(data[off+8:])
		// LSNs only grow; a smaller one is debris from an older log that
		// happened to be left behind a truncation.
		if lsn < w.next {
			break
		}
		recs = append(recs, walRecord{lsn: lsn, typ: data[off+16], payload: data[off+walHeaderSize : end]})
		w.next = lsn + uint64(walHeaderSize+length)
		off = end
	}
	w.written = int64(off)
	w.durable = w.next
	if int64(off) < size {
		if err := file.Truncate(int64(off)); err != nil {
			return nil, nil, err
		}
		if err := w.sync(); err != nil {
			return nil, nil, err
		}
	}
	return w, recs, nil
}

func (w *wal) sync() error {
	if w.noSync {
		return nil
	}
	return w.file.Sync()
}

// append adds a record to the log buffer and returns its LSN. The record is
// not durable until flush.
func (w *wal) append(typ byte, payload []byte) uint64 {
	lsn := w.next
	start := len(w.buf)
	w.buf = binary.BigEndian.AppendUint32(w.buf, uint32(len(payload)))
	w.buf = append(w.buf, 0, 0, 0, 0)
	w.buf = binary.BigEndian.AppendUint64(w.buf, lsn)
	w.buf = append(w.buf, typ)
	w.buf = append(w.buf, payload...)
	binary.BigEndian.PutUint32(w.buf[start+4:], crc32.Checksum(w.buf[start+8:], castagnoli))
	w.next += uint64(walHeaderSize + len(payload))
	return lsn
}

// write hands the buffered records to the operating system without
// syncing. It bounds the memory a long transaction's log takes; the
// records are no more durable for it.
func (w *wal) write() error {
	if len(w.buf) == 0 {
		return nil
	}
	if _, err := w.file.WriteAt(w.buf, w.written); err != nil {
		return fmt.Errorf("storage: writing the log: %w", err)
	}
	w.written += int64(len(w.buf))
	w.buf = w.buf[:0]
	return nil
}

// flush writes the buffered records and syncs the file. When it returns,
// everything appended so far survives a crash.
func (w *wal) flush() error {
	if len(w.buf) > 0 {
		if _, err := w.file.WriteAt(w.buf, w.written); err != nil {
			return fmt.Errorf("storage: writing the log: %w", err)
		}
		w.written += int64(len(w.buf))
		w.buf = w.buf[:0]
	}
	if w.durable == w.next {
		return nil
	}
	if err := w.sync(); err != nil {
		return fmt.Errorf("storage: syncing the log: %w", err)
	}
	w.durable = w.next
	return nil
}

// flushTo makes the record with the given LSN durable, if it is not yet.
func (w *wal) flushTo(lsn uint64) error {
	if lsn < w.durable {
		return nil
	}
	return w.flush()
}

// reset empties the log. It is only called when every change it describes
// is safely in the data file.
func (w *wal) reset() error {
	w.buf = w.buf[:0]
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	if _, err := w.file.WriteAt([]byte(walMagic), 0); err != nil {
		return err
	}
	w.written = int64(len(walMagic))
	w.durable = w.next
	return w.sync()
}

// size is the amount of log not yet discarded, in bytes.
func (w *wal) size() int64 { return w.written + int64(len(w.buf)) }

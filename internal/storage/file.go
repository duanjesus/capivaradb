// Package storage is the on-disk layer: a file of fixed-size pages, a buffer
// pool that caches them, and B+trees built on top.
//
// It deals only in bytes. Keys and values are opaque byte strings ordered by
// bytes.Compare; what they mean (rows, index entries, catalog records) is
// decided by the engine.
package storage

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// File is the little that the pager needs from a file. Besides the real
// thing there is an in-memory implementation, and tests wrap either to
// inject faults.
type File interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Size() (int64, error)
	Close() error
}

// OpenFile opens, creating it if necessary, a database file on disk.
func OpenFile(path string) (File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

type osFile struct{ *os.File }

func (f osFile) Size() (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// MemFile is a File held in memory. A database opened on one behaves
// exactly like one on disk, minus the persistence, which is what lets the
// whole test suite exercise the storage engine without touching the disk.
type MemFile struct {
	mu   sync.RWMutex
	data []byte
}

// NewMemFile returns an empty in-memory file.
func NewMemFile() *MemFile { return &MemFile{} }

func (m *MemFile) ReadAt(p []byte, off int64) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if off < 0 || off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *MemFile) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off < 0 {
		return 0, fmt.Errorf("storage: negative offset %d", off)
	}
	if end := off + int64(len(p)); end > int64(len(m.data)) {
		if end > int64(cap(m.data)) {
			grown := make([]byte, end, max(end, 2*int64(cap(m.data))))
			copy(grown, m.data)
			m.data = grown
		} else {
			m.data = m.data[:end]
		}
	}
	return copy(m.data[off:], p), nil
}

func (m *MemFile) Sync() error { return nil }

func (m *MemFile) Size() (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.data)), nil
}

func (m *MemFile) Close() error { return nil }

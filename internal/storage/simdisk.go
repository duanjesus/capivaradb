package storage

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
)

// ErrSimCrash is returned by every operation on a SimDisk once it has
// crashed, until Crash is called to "restart the machine".
var ErrSimCrash = errors.New("simulated crash")

// SimDisk simulates a disk the way a database has to think about one: a
// write goes to a volatile cache and is only guaranteed to be on the medium
// after a sync. When the power fails, each unsynced write may have made it,
// may not have, or may have made it in part.
//
// It exists for the crash tests. Killing a real process does not lose the
// operating system's cache, so it cannot tell whether fsync is called in the
// right places; this can. A test schedules a crash after some number of
// operations, runs a workload until everything starts failing, calls Crash,
// and reopens the files to see what recovery makes of the remains.
type SimDisk struct {
	mu    sync.Mutex
	rng   *rand.Rand
	files map[string]*simData
	// budget is the number of write, truncate and sync operations left
	// before the crash; negative means no crash is scheduled.
	budget  int
	crashed bool
	// Ops counts the mutating operations performed since the last Crash.
	ops int
}

type simData struct {
	durable []byte     // what is safely on the medium
	current []byte     // what a reader sees: durable plus unsynced changes
	pending []simWrite // the unsynced changes, in the order they were made
}

type simWrite struct {
	truncate bool
	off      int64 // for a truncate, the new size
	data     []byte
}

// NewSimDisk returns an empty disk. seed determines what survives each
// crash, so that a failing test can be replayed.
func NewSimDisk(seed int64) *SimDisk {
	return &SimDisk{rng: rand.New(rand.NewSource(seed)), files: make(map[string]*simData), budget: -1}
}

// Open returns a handle on the named file, creating it empty if needed.
func (d *SimDisk) Open(name string) File {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.files[name] == nil {
		d.files[name] = &simData{}
	}
	return &simFile{disk: d, data: d.files[name]}
}

// CrashAfter schedules a crash: the n-th mutating operation from now, and
// every operation after it, fails.
func (d *SimDisk) CrashAfter(n int) {
	d.mu.Lock()
	d.budget = n
	d.mu.Unlock()
}

// Crashed reports whether the scheduled crash has happened.
func (d *SimDisk) Crashed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.crashed
}

// Ops returns the number of mutating operations since the last Crash.
func (d *SimDisk) Ops() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ops
}

// Crash cuts the power. For every file, each unsynced write independently
// either reached the medium or did not, and one that did may be torn: only
// its first sectors written. Handles opened before the crash must not be
// used afterwards; Open returns fresh ones on what survived.
//
// Treating unsynced writes as an arbitrary subset is harsher than most real
// hardware, which tends to lose a suffix. A recovery that survives this
// does not depend on the order in which a disk empties its cache.
func (d *SimDisk) Crash() {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Vary how much is lost from one crash to the next: sometimes nothing
	// unsynced survives, sometimes everything does.
	keep := []float64{0, 0.5, 0.5, 0.9, 1}[d.rng.Intn(5)]
	for _, f := range d.files {
		for _, w := range f.pending {
			if d.rng.Float64() >= keep {
				continue
			}
			if w.truncate {
				f.durable = resize(f.durable, w.off)
				continue
			}
			data := w.data
			if d.rng.Intn(4) == 0 {
				const sector = 512
				data = data[:d.rng.Intn(len(data)/sector+1)*sector]
			}
			if len(data) == 0 {
				continue
			}
			if end := w.off + int64(len(data)); end > int64(len(f.durable)) {
				f.durable = resize(f.durable, end)
			}
			copy(f.durable[w.off:], data)
		}
		f.pending = nil
		f.current = append([]byte(nil), f.durable...)
	}
	d.crashed, d.budget, d.ops = false, -1, 0
}

func resize(b []byte, size int64) []byte {
	if size <= int64(len(b)) {
		return b[:size]
	}
	return append(b, make([]byte, size-int64(len(b)))...)
}

// tick accounts for one mutating operation. The caller must hold d.mu.
func (d *SimDisk) tick() error {
	if d.crashed {
		return ErrSimCrash
	}
	if d.budget == 0 {
		d.crashed = true
		return ErrSimCrash
	}
	if d.budget > 0 {
		d.budget--
	}
	d.ops++
	return nil
}

type simFile struct {
	disk *SimDisk
	data *simData
}

func (f *simFile) ReadAt(p []byte, off int64) (int, error) {
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if f.disk.crashed {
		return 0, ErrSimCrash
	}
	if off < 0 || off >= int64(len(f.data.current)) {
		return 0, io.EOF
	}
	n := copy(p, f.data.current[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *simFile) WriteAt(p []byte, off int64) (int, error) {
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if off < 0 {
		return 0, fmt.Errorf("storage: negative offset %d", off)
	}
	if err := f.disk.tick(); err != nil {
		return 0, err
	}
	if end := off + int64(len(p)); end > int64(len(f.data.current)) {
		f.data.current = resize(f.data.current, end)
	}
	copy(f.data.current[off:], p)
	f.data.pending = append(f.data.pending, simWrite{off: off, data: append([]byte(nil), p...)})
	return len(p), nil
}

func (f *simFile) Truncate(size int64) error {
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.disk.tick(); err != nil {
		return err
	}
	f.data.current = resize(f.data.current, size)
	f.data.pending = append(f.data.pending, simWrite{truncate: true, off: size})
	return nil
}

func (f *simFile) Sync() error {
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.disk.tick(); err != nil {
		return err
	}
	f.data.durable = append(f.data.durable[:0], f.data.current...)
	f.data.pending = nil
	return nil
}

func (f *simFile) Size() (int64, error) {
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if f.disk.crashed {
		return 0, ErrSimCrash
	}
	return int64(len(f.data.current)), nil
}

func (f *simFile) Close() error { return nil }

// Damage overwrites n bytes of what is on the medium, at the given offset
// of the named file, with garbage. Tests use it to stage damage that a
// random crash would only produce once in a long while.
func (d *SimDisk) Damage(name string, off, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.files[name]
	for i := off; i < off+n && i < len(f.durable); i++ {
		f.durable[i] ^= 0xA5
	}
	f.current = append(f.current[:0], f.durable...)
}

// Append adds bytes to the end of the named file on the medium.
func (d *SimDisk) Append(name string, data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.files[name]
	f.durable = append(f.durable, data...)
	f.current = append(f.current[:0], f.durable...)
}

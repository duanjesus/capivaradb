package pgwire

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
)

// Server accepts PostgreSQL client connections and serves each one on its own
// goroutine.
type Server struct {
	Handler Handler
	// Logger receives connection-level events. Nil discards them.
	Logger *slog.Logger

	mu sync.Mutex
	ln net.Listener
	// active holds every accepted connection; conns only those that got
	// past startup and therefore have a cancellation key.
	active  map[*conn]struct{}
	conns   map[int32]*conn
	nextPID int32
	closed  bool
	wg      sync.WaitGroup
}

// Serve accepts connections on ln until Close is called, then returns nil.
func (s *Server) Serve(ln net.Listener) error {
	if s.Logger == nil {
		s.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.ln = ln
	if s.conns == nil {
		s.conns = make(map[int32]*conn)
		s.active = make(map[*conn]struct{})
	}
	s.mu.Unlock()

	for {
		nc, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		c := newConn(s, nc)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			nc.Close()
			return nil
		}
		s.active[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			c.serve()
			s.mu.Lock()
			delete(s.active, c)
			s.mu.Unlock()
		}()
	}
}

// Close stops accepting connections, closes the open ones and waits for
// their goroutines to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	ln := s.ln
	conns := make([]*conn, 0, len(s.active))
	for c := range s.active {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	var err error
	if ln != nil {
		err = ln.Close()
	}
	for _, c := range conns {
		// Closing the socket only unblocks a connection that is waiting
		// for input; one that is busy executing has to be interrupted.
		c.cancelRunning()
		c.nc.Close()
	}
	s.wg.Wait()
	return err
}

// register assigns the connection its cancellation key. The "process ID" is
// just a counter: there is no process per connection, but clients treat the
// pair (pid, secret) as an opaque token and send it back in CancelRequest.
func (s *Server) register(c *conn) {
	var b [4]byte
	rand.Read(b[:])
	c.secret = int32(binary.BigEndian.Uint32(b[:]))

	s.mu.Lock()
	s.nextPID++
	c.pid = s.nextPID
	s.conns[c.pid] = c
	s.mu.Unlock()
}

func (s *Server) unregister(c *conn) {
	s.mu.Lock()
	delete(s.conns, c.pid)
	s.mu.Unlock()
}

// cancel handles a CancelRequest. As in PostgreSQL, nothing is reported back
// to the requester whether or not the key matched.
func (s *Server) cancel(pid, secret int32) {
	s.mu.Lock()
	c := s.conns[pid]
	s.mu.Unlock()
	if c == nil {
		return
	}
	var want, got [4]byte
	binary.BigEndian.PutUint32(want[:], uint32(c.secret))
	binary.BigEndian.PutUint32(got[:], uint32(secret))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return
	}
	c.cancelRunning()
}

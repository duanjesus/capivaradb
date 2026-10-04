// Package pgwire implements the server side of the PostgreSQL frontend/backend
// protocol, version 3.0.
//
// The package knows nothing about SQL. It turns bytes into calls on the
// Handler/Session/Prepared/Rows interfaces below and turns their results back
// into bytes, which is what allows the storage and execution layers to be
// replaced milestone by milestone without touching protocol code.
package pgwire

import "context"

// Handler creates one Session per client connection.
type Handler interface {
	// NewSession is called once the startup packet has been read. params
	// holds the startup parameters ("user", "database", ...).
	NewSession(params map[string]string) (Session, error)
}

// Stmt is an opaque parsed statement, produced and consumed by the Session.
type Stmt any

// Session is the per-connection state of the database engine. A Session is
// only ever used by one goroutine at a time.
type Session interface {
	// Parse splits a query string into statements. An empty or
	// comment-only string yields zero statements.
	Parse(query string) ([]Stmt, error)

	// Prepare analyses a statement. paramOIDs are the parameter types
	// declared by the client; it may be shorter than the number of
	// parameters and may contain zeros, both meaning "infer it".
	Prepare(stmt Stmt, paramOIDs []uint32) (Prepared, error)

	// TxStatus is the transaction status byte sent in ReadyForQuery:
	// 'I' idle, 'T' in a transaction block, 'E' in a failed one.
	TxStatus() byte

	// OnError is called whenever an error is reported to the client, so
	// that an open transaction block can be marked as failed.
	OnError()

	// Close releases the session, rolling back any open transaction.
	Close()
}

// Prepared is an analysed statement, ready to be executed any number of times.
type Prepared interface {
	// ParamOIDs returns the type of every parameter. Parameter values
	// passed to Execute are decoded according to these.
	ParamOIDs() []uint32

	// Columns describes the result set, or returns nil for statements
	// that do not return rows.
	Columns() []Column

	// Execute runs the statement. Parameter values are nil, bool, int64,
	// float64 or string.
	Execute(ctx context.Context, params []any) (Rows, error)
}

// Rows iterates over the result of one execution.
type Rows interface {
	// Next returns the next row, or io.EOF when there are no more. Values
	// use the same Go types as parameters.
	Next(ctx context.Context) ([]any, error)

	// Tag is the command tag ("SELECT 3", "INSERT 0 1"). It is only valid
	// after Next has returned io.EOF.
	Tag() string

	Close()
}

// Column describes one result column.
type Column struct {
	Name string
	OID  uint32
}

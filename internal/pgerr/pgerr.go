// Package pgerr defines the error type shared by every layer of the database.
//
// PostgreSQL clients rely on the five-character SQLSTATE code far more than on
// the message text (drivers map codes to exception classes, retry logic keys
// off them), so errors carry their code from the point where they are raised
// instead of being classified later at the protocol boundary.
package pgerr

import (
	"context"
	"errors"
	"fmt"
)

// Severities defined by the protocol. FATAL terminates the connection.
const (
	SeverityError = "ERROR"
	SeverityFatal = "FATAL"
)

// SQLSTATE codes used by CapivaraDB. Names follow Appendix A of the
// PostgreSQL manual.
const (
	ProtocolViolation           = "08P01"
	FeatureNotSupported         = "0A000"
	NumericValueOutOfRange      = "22003"
	DivisionByZero              = "22012"
	InvalidParameterValue       = "22023"
	InvalidTextRepresentation   = "22P02"
	InvalidBinaryRepresentation = "22P03"
	NotNullViolation            = "23502"
	UniqueViolation             = "23505"
	InFailedSQLTransaction      = "25P02"
	InvalidSQLStatementName     = "26000"
	InvalidCursorName           = "34000"
	SyntaxError                 = "42601"
	DuplicateColumn             = "42701"
	UndefinedColumn             = "42703"
	UndefinedObject             = "42704"
	DatatypeMismatch            = "42804"
	CannotCoerce                = "42846"
	UndefinedFunction           = "42883"
	UndefinedTable              = "42P01"
	UndefinedParameter          = "42P02"
	DuplicateCursor             = "42P03"
	DuplicatePreparedStatement  = "42P05"
	DuplicateTable              = "42P07"
	AmbiguousColumn             = "42702"
	GroupingError               = "42803"
	InvalidTableDefinition      = "42P16"
	CardinalityViolation        = "21000"
	InvalidRowCountInLimit      = "2201W"
	InvalidRowCountInOffset     = "2201X"
	ProgramLimitExceeded        = "54000"
	ObjectInUse                 = "55006"
	StatementTooComplex         = "54001"
	QueryCanceled               = "57014"
	AdminShutdown               = "57P01"
	InternalError               = "XX000"
)

// Error is a PostgreSQL-style error. It maps one-to-one onto the fields of an
// ErrorResponse message.
type Error struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	Hint     string
	// Position is the 1-based character (not byte) offset into the query
	// text that the error refers to, or 0 if there is none. psql uses it to
	// draw the caret under the offending token.
	Position int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s (SQLSTATE %s)", e.Severity, e.Message, e.Code)
}

// New builds an ERROR-severity error.
func New(code, format string, args ...any) *Error {
	return &Error{Severity: SeverityError, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Fatal builds a FATAL-severity error; the connection is closed after it is sent.
func Fatal(code, format string, args ...any) *Error {
	return &Error{Severity: SeverityFatal, Code: code, Message: fmt.Sprintf(format, args...)}
}

// At sets the query position and returns the error for chaining.
func (e *Error) At(pos int) *Error {
	e.Position = pos
	return e
}

// WithDetail sets the detail field and returns the error for chaining.
func (e *Error) WithDetail(format string, args ...any) *Error {
	e.Detail = fmt.Sprintf(format, args...)
	return e
}

// From converts any error into an *Error. Context cancellation becomes
// query_canceled; anything unrecognised becomes an internal error so that a
// bug never reaches the client as a malformed response.
func From(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return New(QueryCanceled, "canceling statement due to user request")
	}
	return New(InternalError, "internal error: %v", err)
}

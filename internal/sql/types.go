// Package sql contains the hand-written SQL front end: lexer, AST and parser.
// It has no dependency on the rest of the database besides the error type.
package sql

// Type is a SQL data type.
type Type uint8

const (
	// Unknown is the type of an untyped NULL or of a parameter whose type
	// has not been inferred yet.
	Unknown Type = iota
	Bool
	Int4
	Int8
	Float8
	Text
)

func (t Type) String() string {
	switch t {
	case Bool:
		return "boolean"
	case Int4:
		return "integer"
	case Int8:
		return "bigint"
	case Float8:
		return "double precision"
	case Text:
		return "text"
	}
	return "unknown"
}

// IsInt reports whether t is an integer type.
func (t Type) IsInt() bool { return t == Int4 || t == Int8 }

// IsNumeric reports whether t is a numeric type.
func (t Type) IsNumeric() bool { return t == Int4 || t == Int8 || t == Float8 }

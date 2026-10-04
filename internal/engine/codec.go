package engine

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/duanjesus/capivaradb/internal/sql"
)

// This file defines the two byte formats rows take on disk.
//
// A *tuple* is the value side of a table's B+tree: compact, not comparable.
//
//	null bitmap (one bit per column), then each non-NULL column:
//	  boolean            1 byte
//	  integer            4 bytes, big-endian
//	  bigint             8 bytes, big-endian
//	  double precision   8 bytes, IEEE 754
//	  text               length as a uvarint, then the bytes
//
// A *key* is the key side, of tables and of indexes. It is order-preserving:
// comparing two encoded keys with bytes.Compare gives the same answer as
// comparing the values they were made from, column by column. That is what
// lets the B+tree stay ignorant of types.
//
//	each column: 0x00 for NULL, or 0x01 followed by
//	  integer, bigint    8 bytes, big-endian, with the sign bit flipped
//	  double precision   8 bytes: sign bit flipped if positive, all bits
//	                     flipped if negative
//	  boolean            1 byte
//	  text               the bytes, 0x00 escaped as 0x00 0xFF, ended by
//	                     0x00 0x01

func encodeTuple(cols []column, vals []any) []byte {
	out := make([]byte, (len(cols)+7)/8, 16+8*len(cols))
	for i, v := range vals {
		if v == nil {
			out[i/8] |= 1 << (i % 8)
			continue
		}
		switch cols[i].typ {
		case sql.Bool:
			if v.(bool) {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		case sql.Int4:
			out = binary.BigEndian.AppendUint32(out, uint32(v.(int64)))
		case sql.Int8:
			out = binary.BigEndian.AppendUint64(out, uint64(v.(int64)))
		case sql.Float8:
			out = binary.BigEndian.AppendUint64(out, math.Float64bits(v.(float64)))
		default:
			s := v.(string)
			out = binary.AppendUvarint(out, uint64(len(s)))
			out = append(out, s...)
		}
	}
	return out
}

func decodeTuple(cols []column, b []byte) ([]any, error) {
	bad := func() ([]any, error) {
		return nil, fmt.Errorf("corrupt tuple: %d bytes do not decode as %d columns", len(b), len(cols))
	}
	nulls := (len(cols) + 7) / 8
	if len(b) < nulls {
		return bad()
	}
	vals := make([]any, len(cols))
	rest := b[nulls:]
	for i, c := range cols {
		if b[i/8]&(1<<(i%8)) != 0 {
			continue
		}
		need := 8
		switch c.typ {
		case sql.Bool:
			need = 1
		case sql.Int4:
			need = 4
		case sql.Text:
			need = 0
		}
		if len(rest) < need {
			return bad()
		}
		switch c.typ {
		case sql.Bool:
			vals[i] = rest[0] != 0
		case sql.Int4:
			vals[i] = int64(int32(binary.BigEndian.Uint32(rest)))
		case sql.Int8:
			vals[i] = int64(binary.BigEndian.Uint64(rest))
		case sql.Float8:
			vals[i] = math.Float64frombits(binary.BigEndian.Uint64(rest))
		default:
			size, n := binary.Uvarint(rest)
			if n <= 0 || uint64(len(rest)-n) < size {
				return bad()
			}
			vals[i] = string(rest[n : n+int(size)])
			need = n + int(size)
		}
		rest = rest[need:]
	}
	return vals, nil
}

const signBit = 1 << 63

// appendKeyPart appends the order-preserving encoding of one value.
func appendKeyPart(dst []byte, v any) []byte {
	switch v := v.(type) {
	case nil:
		return append(dst, 0)
	case int64:
		// Flipping the sign bit makes negative numbers, which have it
		// set, sort before positive ones as unsigned bytes.
		return binary.BigEndian.AppendUint64(append(dst, 1), uint64(v)^signBit)
	case float64:
		if v == 0 {
			v = 0 // -0 and +0 are equal and must encode alike
		}
		bits := math.Float64bits(v)
		if bits&signBit == 0 {
			bits ^= signBit
		} else {
			// Negative floats sort backwards as bit patterns (a larger
			// magnitude is a smaller number), so all bits are inverted.
			bits = ^bits
		}
		return binary.BigEndian.AppendUint64(append(dst, 1), bits)
	case bool:
		if v {
			return append(dst, 1, 1)
		}
		return append(dst, 1, 0)
	case string:
		dst = append(dst, 1)
		for i := 0; i < len(v); i++ {
			if v[i] == 0 {
				dst = append(dst, 0, 0xFF)
			} else {
				dst = append(dst, v[i])
			}
		}
		// The terminator sorts below any continuation, so that "ab" comes
		// before "abc", and it cannot occur inside the escaped text.
		return append(dst, 0, 1)
	}
	panic(fmt.Sprintf("engine: cannot encode %T in a key", v))
}

// encodeKey encodes the given columns of a row as a key.
func encodeKey(cols []int, vals []any) []byte {
	key := make([]byte, 0, 9*len(cols))
	for _, c := range cols {
		key = appendKeyPart(key, vals[c])
	}
	return key
}

// rowIDKey is the key of a row in a table without a primary key.
func rowIDKey(id int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(id))
}

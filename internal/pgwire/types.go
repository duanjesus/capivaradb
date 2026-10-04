package pgwire

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"

	"github.com/duanjesus/capivaradb/internal/pgerr"
)

// Type OIDs from pg_type. They are part of the protocol: clients pick their
// decoders by OID, so these numbers must match PostgreSQL's.
const (
	OIDBool    uint32 = 16
	OIDName    uint32 = 19
	OIDInt8    uint32 = 20
	OIDInt2    uint32 = 21
	OIDInt4    uint32 = 23
	OIDText    uint32 = 25
	OIDFloat4  uint32 = 700
	OIDFloat8  uint32 = 701
	OIDUnknown uint32 = 705
	OIDBPChar  uint32 = 1042
	OIDVarchar uint32 = 1043
)

// Format codes used by Bind and RowDescription.
const (
	FormatText   int16 = 0
	FormatBinary int16 = 1
)

// typeLen is the pg_type.typlen reported in RowDescription: the fixed size in
// bytes, or -1 for variable-length types.
func typeLen(oid uint32) int16 {
	switch oid {
	case OIDBool:
		return 1
	case OIDInt2:
		return 2
	case OIDInt4, OIDFloat4:
		return 4
	case OIDInt8, OIDFloat8:
		return 8
	}
	return -1
}

// TextValue renders a value in PostgreSQL's text output format.
func TextValue(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case bool:
		if v {
			return "t"
		}
		return "f"
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return formatFloat(v)
	case string:
		return v
	}
	return ""
}

func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// appendValue appends the wire representation of a non-NULL value.
func appendValue(dst []byte, oid uint32, format int16, v any) ([]byte, error) {
	if format == FormatText {
		switch v := v.(type) {
		case bool:
			if v {
				return append(dst, 't'), nil
			}
			return append(dst, 'f'), nil
		case int64:
			return strconv.AppendInt(dst, v, 10), nil
		case float64:
			return append(dst, formatFloat(v)...), nil
		case string:
			return append(dst, v...), nil
		}
		return nil, pgerr.New(pgerr.InternalError, "cannot encode value of type %T", v)
	}
	switch v := v.(type) {
	case bool:
		if v {
			return append(dst, 1), nil
		}
		return append(dst, 0), nil
	case int64:
		switch oid {
		case OIDInt2:
			return binary.BigEndian.AppendUint16(dst, uint16(v)), nil
		case OIDInt4:
			return binary.BigEndian.AppendUint32(dst, uint32(v)), nil
		case OIDInt8:
			return binary.BigEndian.AppendUint64(dst, uint64(v)), nil
		}
	case float64:
		switch oid {
		case OIDFloat4:
			return binary.BigEndian.AppendUint32(dst, math.Float32bits(float32(v))), nil
		case OIDFloat8:
			return binary.BigEndian.AppendUint64(dst, math.Float64bits(v)), nil
		}
	case string:
		return append(dst, v...), nil
	}
	return nil, pgerr.New(pgerr.InternalError, "cannot encode value of type %T as OID %d in binary", v, oid)
}

// decodeParam converts a parameter value sent by the client into a Go value.
func decodeParam(oid uint32, format int16, b []byte) (any, error) {
	if format == FormatBinary {
		return decodeBinary(oid, b)
	}
	s := string(b)
	switch oid {
	case OIDBool:
		v, ok := ParseBool(s)
		if !ok {
			return nil, pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type boolean: %q", s)
		}
		return v, nil
	case OIDInt2:
		return ParseInt(s, 16, "smallint")
	case OIDInt4:
		return ParseInt(s, 32, "integer")
	case OIDInt8:
		return ParseInt(s, 64, "bigint")
	case OIDFloat4, OIDFloat8:
		return ParseFloat(s)
	case OIDText, OIDVarchar, OIDBPChar, OIDName, OIDUnknown:
		return s, nil
	}
	return nil, pgerr.New(pgerr.FeatureNotSupported, "parameter type with OID %d is not supported", oid)
}

func decodeBinary(oid uint32, b []byte) (any, error) {
	want := -1
	switch oid {
	case OIDBool:
		want = 1
	case OIDInt2:
		want = 2
	case OIDInt4, OIDFloat4:
		want = 4
	case OIDInt8, OIDFloat8:
		want = 8
	case OIDText, OIDVarchar, OIDBPChar, OIDName, OIDUnknown:
		return string(b), nil
	default:
		return nil, pgerr.New(pgerr.FeatureNotSupported, "parameter type with OID %d is not supported", oid)
	}
	if len(b) != want {
		return nil, pgerr.New(pgerr.InvalidBinaryRepresentation,
			"incorrect binary data format: expected %d bytes for OID %d, got %d", want, oid, len(b))
	}
	switch oid {
	case OIDBool:
		return b[0] != 0, nil
	case OIDInt2:
		return int64(int16(binary.BigEndian.Uint16(b))), nil
	case OIDInt4:
		return int64(int32(binary.BigEndian.Uint32(b))), nil
	case OIDInt8:
		return int64(binary.BigEndian.Uint64(b)), nil
	case OIDFloat4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
}

// ParseBool parses PostgreSQL's boolean input syntax.
func ParseBool(s string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "t", "true", "y", "yes", "on", "1":
		return true, true
	case "f", "false", "n", "no", "off", "0":
		return false, true
	}
	return false, false
}

// ParseInt parses an integer of the given bit size, reporting errors with
// the SQLSTATEs PostgreSQL uses for bad syntax and for overflow.
func ParseInt(s string, bits int, typeName string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, bits)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return 0, pgerr.New(pgerr.NumericValueOutOfRange, "value %q is out of range for type %s", s, typeName)
		}
		return 0, pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type %s: %q", typeName, s)
	}
	return v, nil
}

// ParseFloat parses PostgreSQL's double precision input syntax.
func ParseFloat(s string) (float64, error) {
	t := strings.TrimSpace(s)
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return 0, pgerr.New(pgerr.NumericValueOutOfRange, "%q is out of range for type double precision", s)
		}
		return 0, pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type double precision: %q", s)
	}
	return v, nil
}

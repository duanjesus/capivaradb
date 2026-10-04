package pgwire

import (
	"bytes"
	"encoding/binary"
)

// Message type bytes. Frontend and backend reuse some letters with different
// meanings, hence the two groups.
const (
	// Frontend.
	msgQuery     = 'Q'
	msgParse     = 'P'
	msgBind      = 'B'
	msgDescribe  = 'D'
	msgExecute   = 'E'
	msgClose     = 'C'
	msgSync      = 'S'
	msgFlush     = 'H'
	msgTerminate = 'X'
	msgFunction  = 'F'
	msgCopyData  = 'd'
	msgCopyDone  = 'c'
	msgCopyFail  = 'f'
	msgPassword  = 'p'

	// Backend.
	msgAuthentication       = 'R'
	msgParameterStatus      = 'S'
	msgBackendKeyData       = 'K'
	msgReadyForQuery        = 'Z'
	msgRowDescription       = 'T'
	msgDataRow              = 'D'
	msgCommandComplete      = 'C'
	msgEmptyQueryResponse   = 'I'
	msgErrorResponse        = 'E'
	msgParseComplete        = '1'
	msgBindComplete         = '2'
	msgCloseComplete        = '3'
	msgParameterDescription = 't'
	msgNoData               = 'n'
	msgPortalSuspended      = 's'
	msgNegotiateProtocol    = 'v'
)

// msgReader decodes the body of one message. Reads past the end set bad
// instead of panicking; callers check it once after decoding all fields.
type msgReader struct {
	b   []byte
	bad bool
}

func (r *msgReader) byte() byte {
	if len(r.b) < 1 {
		r.bad = true
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *msgReader) int16() int {
	if len(r.b) < 2 {
		r.bad = true
		return 0
	}
	v := int16(binary.BigEndian.Uint16(r.b))
	r.b = r.b[2:]
	return int(v)
}

func (r *msgReader) int32() int32 {
	if len(r.b) < 4 {
		r.bad = true
		return 0
	}
	v := int32(binary.BigEndian.Uint32(r.b))
	r.b = r.b[4:]
	return v
}

// cstring reads a NUL-terminated string.
func (r *msgReader) cstring() string {
	i := bytes.IndexByte(r.b, 0)
	if i < 0 {
		r.bad = true
		return ""
	}
	s := string(r.b[:i])
	r.b = r.b[i+1:]
	return s
}

// bytes returns the next n bytes without copying them.
func (r *msgReader) bytes(n int) []byte {
	if n < 0 || len(r.b) < n {
		r.bad = true
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// msgWriter builds one message at a time into a reusable buffer.
type msgWriter struct {
	b []byte
}

// begin starts a message of the given type, leaving room for its length.
func (w *msgWriter) begin(typ byte) {
	w.b = append(w.b[:0], typ, 0, 0, 0, 0)
}

// finish fills in the length (which counts itself but not the type byte) and
// returns the complete message.
func (w *msgWriter) finish() []byte {
	binary.BigEndian.PutUint32(w.b[1:], uint32(len(w.b)-1))
	return w.b
}

func (w *msgWriter) byte(v byte)     { w.b = append(w.b, v) }
func (w *msgWriter) int16(v int)     { w.b = binary.BigEndian.AppendUint16(w.b, uint16(v)) }
func (w *msgWriter) int32(v int32)   { w.b = binary.BigEndian.AppendUint32(w.b, uint32(v)) }
func (w *msgWriter) uint32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }

func (w *msgWriter) cstring(s string) {
	w.b = append(w.b, s...)
	w.b = append(w.b, 0)
}

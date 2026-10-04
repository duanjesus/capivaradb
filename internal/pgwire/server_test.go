package pgwire_test

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duanjesus/capivaradb/internal/engine"
	"github.com/duanjesus/capivaradb/internal/pgwire"
)

// These tests speak the protocol byte by byte, with a client written here
// from the specification rather than with a driver. That keeps them free of
// dependencies and lets them send things no well-behaved driver would.
// Compatibility with real drivers is covered separately under compat/.

func startServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &pgwire.Server{Handler: engine.New()}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Close()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String()
}

type message struct {
	typ  byte
	body []byte
}

func (m message) String() string { return fmt.Sprintf("%c%q", m.typ, m.body) }

type client struct {
	t      *testing.T
	nc     net.Conn
	r      *bufio.Reader
	pid    uint32
	secret uint32
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	t.Cleanup(func() { nc.Close() })
	return &client{t: t, nc: nc, r: bufio.NewReader(nc)}
}

// connect dials and completes the startup handshake.
func connect(t *testing.T, addr string) *client {
	t.Helper()
	c := dial(t, addr)
	c.startup(196608, "user", "tester", "database", "capi")
	msgs := c.readUntil('Z')
	if msgs[0].typ != 'R' || binary.BigEndian.Uint32(msgs[0].body) != 0 {
		t.Fatalf("expected AuthenticationOk first, got %v", msgs[0])
	}
	for _, m := range msgs {
		if m.typ == 'K' {
			c.pid = binary.BigEndian.Uint32(m.body)
			c.secret = binary.BigEndian.Uint32(m.body[4:])
		}
	}
	return c
}

func (c *client) startup(code uint32, kv ...string) {
	var body []byte
	body = binary.BigEndian.AppendUint32(body, code)
	for _, s := range kv {
		body = append(append(body, s...), 0)
	}
	if len(kv) > 0 {
		body = append(body, 0)
	}
	c.raw(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body)
}

func (c *client) raw(parts ...[]byte) {
	c.t.Helper()
	for _, p := range parts {
		if _, err := c.nc.Write(p); err != nil {
			c.t.Fatalf("write: %v", err)
		}
	}
}

// send writes one message. Arguments are appended in order: string as a
// C string, int16/int32/uint32 big-endian, []byte verbatim.
func (c *client) send(typ byte, fields ...any) {
	c.t.Helper()
	var body []byte
	for _, f := range fields {
		switch f := f.(type) {
		case string:
			body = append(append(body, f...), 0)
		case int16:
			body = binary.BigEndian.AppendUint16(body, uint16(f))
		case int32:
			body = binary.BigEndian.AppendUint32(body, uint32(f))
		case uint32:
			body = binary.BigEndian.AppendUint32(body, f)
		case byte:
			body = append(body, f)
		case []byte:
			body = append(body, f...)
		default:
			c.t.Fatalf("send: unsupported field type %T", f)
		}
	}
	c.raw([]byte{typ}, binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body)
}

func (c *client) read() message {
	c.t.Helper()
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(hdr[1:])-4)
	if _, err := io.ReadFull(c.r, body); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return message{hdr[0], body}
}

func (c *client) readUntil(typ byte) []message {
	c.t.Helper()
	var msgs []message
	for {
		m := c.read()
		msgs = append(msgs, m)
		if m.typ == typ {
			return msgs
		}
	}
}

// query runs a simple query and returns the responses up to ReadyForQuery.
func (c *client) query(sql string) []message {
	c.t.Helper()
	c.send('Q', sql)
	return c.readUntil('Z')
}

// types returns the message types as a string, e.g. "TDDCZ".
func types(msgs []message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteByte(m.typ)
	}
	return sb.String()
}

func expectTypes(t *testing.T, msgs []message, want string) {
	t.Helper()
	if got := types(msgs); got != want {
		t.Fatalf("message sequence: got %s, want %s\n%v", got, want, msgs)
	}
}

// errorFields decodes an ErrorResponse into its fields.
func errorFields(t *testing.T, m message) map[byte]string {
	t.Helper()
	if m.typ != 'E' {
		t.Fatalf("expected ErrorResponse, got %v", m)
	}
	fields := make(map[byte]string)
	b := m.body
	for len(b) > 0 && b[0] != 0 {
		end := bytes.IndexByte(b[1:], 0)
		fields[b[0]] = string(b[1 : 1+end])
		b = b[end+2:]
	}
	return fields
}

// dataRow decodes a DataRow; NULL is rendered as "<null>".
func dataRow(m message) []string {
	n := int(binary.BigEndian.Uint16(m.body))
	b := m.body[2:]
	out := make([]string, n)
	for i := range out {
		size := int32(binary.BigEndian.Uint32(b))
		b = b[4:]
		if size < 0 {
			out[i] = "<null>"
			continue
		}
		out[i] = string(b[:size])
		b = b[size:]
	}
	return out
}

func cstr(b []byte) string { return string(bytes.TrimRight(b, "\x00")) }

func TestStartup(t *testing.T) {
	addr := startServer(t)
	c := dial(t, addr)

	// psql and most drivers open with SSLRequest. The answer is a single
	// byte, and the same connection then carries the real startup packet.
	c.startup(80877103)
	b, err := c.r.ReadByte()
	if err != nil || b != 'N' {
		t.Fatalf("SSLRequest: got %q, %v", b, err)
	}
	c.startup(196608, "user", "ana", "database", "capi", "application_name", "tests")

	msgs := c.readUntil('Z')
	expectTypes(t, msgs, "RSSSSSSSSSSKZ")
	status := make(map[string]string)
	for _, m := range msgs {
		if m.typ == 'S' {
			kv := bytes.SplitN(m.body, []byte{0}, 2)
			status[string(kv[0])] = cstr(kv[1])
		}
	}
	for k, want := range map[string]string{
		"server_version": "16.0", "client_encoding": "UTF8", "integer_datetimes": "on",
		"standard_conforming_strings": "on", "application_name": "tests",
	} {
		if status[k] != want {
			t.Errorf("ParameterStatus %s = %q, want %q", k, status[k], want)
		}
	}
	if last := msgs[len(msgs)-1]; string(last.body) != "I" {
		t.Errorf("ReadyForQuery status: %q", last.body)
	}
}

func TestStartupRejections(t *testing.T) {
	addr := startServer(t)

	c := dial(t, addr)
	c.startup(2 << 16) // protocol 2.0
	if f := errorFields(t, c.read()); f['S'] != "FATAL" || f['C'] != "0A000" {
		t.Errorf("protocol 2.0: %v", f)
	}

	c = dial(t, addr)
	c.startup(196608, "database", "capi")
	if f := errorFields(t, c.read()); f['S'] != "FATAL" || f['C'] != "28000" {
		t.Errorf("missing user: %v", f)
	}
}

func TestProtocolVersionNegotiation(t *testing.T) {
	c := dial(t, startServer(t))
	// Protocol 3.2 with an extension option the server does not know.
	c.startup(196608+2, "user", "ana", "_pq_.fancy", "1")
	m := c.read()
	if m.typ != 'v' {
		t.Fatalf("expected NegotiateProtocolVersion, got %v", m)
	}
	if minor, n := binary.BigEndian.Uint32(m.body), binary.BigEndian.Uint32(m.body[4:]); minor != 0 || n != 1 {
		t.Errorf("minor=%d unrecognized=%d", minor, n)
	}
	if got := cstr(m.body[8:]); got != "_pq_.fancy" {
		t.Errorf("unrecognized option: %q", got)
	}
	c.readUntil('Z')
}

func TestSimpleQuery(t *testing.T) {
	c := connect(t, startServer(t))

	msgs := c.query("select 1 as n, 'hi' as s, null, 2.5, true")
	expectTypes(t, msgs, "TDCZ")
	if got := dataRow(msgs[1]); strings.Join(got, ",") != "1,hi,<null>,2.5,t" {
		t.Errorf("row: %v", got)
	}
	if tag := cstr(msgs[2].body); tag != "SELECT 1" {
		t.Errorf("tag: %q", tag)
	}

	// RowDescription: field count, then per field name\0, table OID,
	// attribute number, type OID, type length, type modifier, format.
	rd := msgs[0].body
	if n := binary.BigEndian.Uint16(rd); n != 5 {
		t.Fatalf("field count: %d", n)
	}
	rd = rd[2:]
	if name := cstr(rd[:2]); name != "n" {
		t.Errorf("first column name: %q", name)
	}
	rd = rd[2:]
	if oid, size := binary.BigEndian.Uint32(rd[6:]), int16(binary.BigEndian.Uint16(rd[10:])); oid != 23 || size != 4 {
		t.Errorf("first column: oid=%d typlen=%d", oid, size)
	}
}

func TestMultiStatementQuery(t *testing.T) {
	c := connect(t, startServer(t))
	query := `
		create table t (id int primary key, name text);
		insert into t values (1, 'ana'), (2, 'bia');
		select * from t;
		select nope from t;
		select 'never runs'`
	msgs := c.query(query)
	// Each statement gets its own completion; the error stops the rest of
	// the string and there is still exactly one ReadyForQuery.
	expectTypes(t, msgs, "CCTDDCEZ")
	if tag := cstr(msgs[1].body); tag != "INSERT 0 2" {
		t.Errorf("insert tag: %q", tag)
	}
	f := errorFields(t, msgs[6])
	if f['C'] != "42703" || f['M'] != `column "nope" does not exist` || f['S'] != "ERROR" || f['V'] != "ERROR" {
		t.Errorf("error fields: %v", f)
	}
	// The position points at "nope" in the whole query string, which is
	// what lets psql draw a caret under it.
	if want := fmt.Sprint(strings.Index(query, "nope") + 1); f['P'] != want {
		t.Errorf("error position: got %s, want %s", f['P'], want)
	}
}

func TestEmptyQuery(t *testing.T) {
	c := connect(t, startServer(t))
	for _, q := range []string{"", "   ", ";", "-- ping"} {
		expectTypes(t, c.query(q), "IZ")
	}
}

func TestTransactionStatus(t *testing.T) {
	c := connect(t, startServer(t))
	status := func(msgs []message) string { return string(msgs[len(msgs)-1].body) }

	c.query("create table t (id int)")
	if s := status(c.query("begin")); s != "T" {
		t.Errorf("after BEGIN: %s", s)
	}
	c.query("insert into t values (1)")
	if s := status(c.query("select 1/0")); s != "E" {
		t.Errorf("after an error in a transaction: %s", s)
	}
	msgs := c.query("select 1")
	if f := errorFields(t, msgs[0]); f['C'] != "25P02" {
		t.Errorf("statement in a failed transaction: %v", f)
	}
	msgs = c.query("commit")
	if tag := cstr(msgs[0].body); tag != "ROLLBACK" || status(msgs) != "I" {
		t.Errorf("commit of a failed transaction: tag=%q status=%s", tag, status(msgs))
	}
	expectTypes(t, c.query("select * from t"), "TCZ")
}

func TestExtendedQuery(t *testing.T) {
	c := connect(t, startServer(t))
	c.query("create table t (id int primary key, name text, score float8); insert into t values (1, 'ana', 9.5), (2, 'bia', null)")

	// Parse a named statement without declaring the parameter type, then
	// ask the server what it inferred.
	c.send('P', "by_id", "select id, name, score from t where id = $1", int16(0))
	c.send('D', byte('S'), "by_id")
	c.send('S')
	msgs := c.readUntil('Z')
	expectTypes(t, msgs, "1tTZ")
	if n, oid := binary.BigEndian.Uint16(msgs[1].body), binary.BigEndian.Uint32(msgs[1].body[2:]); n != 1 || oid != 23 {
		t.Errorf("ParameterDescription: n=%d oid=%d", n, oid)
	}

	// Bind with a binary int4 parameter and ask for binary results.
	c.send('B', "", "by_id",
		int16(1), int16(1), // one parameter format: binary
		int16(1), int32(4), int32(1), // one parameter, 4 bytes, value 1
		int16(1), int16(1)) // one result format: binary
	c.send('D', byte('P'), "")
	c.send('E', "", int32(0))
	c.send('S')
	msgs = c.readUntil('Z')
	expectTypes(t, msgs, "2TDCZ")
	row := dataRow(msgs[2])
	if id := binary.BigEndian.Uint32([]byte(row[0])); id != 1 || row[1] != "ana" || len(row[2]) != 8 {
		t.Errorf("binary row: %q", row)
	}
	// The portal's RowDescription carries the format chosen at Bind.
	rd := msgs[1].body
	if format := binary.BigEndian.Uint16(rd[len(rd)-2:]); format != 1 {
		t.Errorf("last column format code: %d", format)
	}

	// The same statement again, text parameter and text results, NULL column.
	c.send('B', "", "by_id", int16(0), int16(1), int32(1), []byte("2"), int16(0))
	c.send('E', "", int32(0))
	c.send('S')
	msgs = c.readUntil('Z')
	expectTypes(t, msgs, "2DCZ")
	if got := strings.Join(dataRow(msgs[1]), ","); got != "2,bia,<null>" {
		t.Errorf("text row: %s", got)
	}

	// A statement that returns no rows is described with NoData.
	c.send('P', "", "insert into t values ($1, $2, $3)", int16(0))
	c.send('D', byte('S'), "")
	c.send('B', "", "", int16(0), int16(3), int32(1), []byte("3"), int32(4), []byte("caio"), int32(-1), int16(0))
	c.send('E', "", int32(0))
	c.send('S')
	msgs = c.readUntil('Z')
	expectTypes(t, msgs, "1tn2CZ")
	if tag := cstr(msgs[4].body); tag != "INSERT 0 1" {
		t.Errorf("tag: %q", tag)
	}

	// Close the named statement; using it afterwards is an error.
	c.send('C', byte('S'), "by_id")
	c.send('B', "", "by_id", int16(0), int16(0), int16(0))
	c.send('S')
	msgs = c.readUntil('Z')
	expectTypes(t, msgs, "3EZ")
	if f := errorFields(t, msgs[1]); f['C'] != "26000" {
		t.Errorf("bind to a closed statement: %v", f)
	}
}

func TestPortalSuspension(t *testing.T) {
	c := connect(t, startServer(t))
	c.query("create table t (id int); insert into t values (1), (2), (3), (4), (5)")

	// Portals only outlive Sync inside a transaction block.
	c.query("begin")
	c.send('P', "", "select id from t", int16(0))
	c.send('B', "cur", "", int16(0), int16(0), int16(0))
	c.send('E', "cur", int32(2))
	c.send('S')
	expectTypes(t, c.readUntil('Z'), "12DDsZ")

	c.send('E', "cur", int32(2))
	c.send('S')
	msgs := c.readUntil('Z')
	expectTypes(t, msgs, "DDsZ")
	if got := dataRow(msgs[0])[0]; got != "3" {
		t.Errorf("the portal should resume at row 3, got %s", got)
	}

	c.send('E', "cur", int32(2))
	c.send('S')
	msgs = c.readUntil('Z')
	expectTypes(t, msgs, "DCZ")
	if tag := cstr(msgs[1].body); tag != "SELECT 5" {
		t.Errorf("tag: %q", tag)
	}
	c.query("commit")

	// After the transaction ends the portal is gone.
	c.send('E', "cur", int32(0))
	c.send('S')
	msgs = c.readUntil('Z')
	if f := errorFields(t, msgs[0]); f['C'] != "34000" {
		t.Errorf("execute of a closed portal: %v", f)
	}
}

func TestExtendedErrorSkipsToSync(t *testing.T) {
	c := connect(t, startServer(t))
	// A pipelined batch whose first message fails: everything up to Sync
	// is discarded, so the client sees one error and one ReadyForQuery.
	c.send('P', "", "select * from missing", int16(0))
	c.send('B', "", "", int16(0), int16(0), int16(0))
	c.send('D', byte('P'), "")
	c.send('E', "", int32(0))
	c.send('S')
	msgs := c.readUntil('Z')
	expectTypes(t, msgs, "EZ")
	if f := errorFields(t, msgs[0]); f['C'] != "42P01" {
		t.Errorf("error: %v", f)
	}
	// The connection is usable again after the Sync.
	expectTypes(t, c.query("select 1"), "TDCZ")
}

func TestBindErrors(t *testing.T) {
	c := connect(t, startServer(t))
	c.send('P', "s", "select $1::int", int16(0))
	c.send('S')
	expectTypes(t, c.readUntil('Z'), "1Z")

	check := func(what, code string, bind ...any) {
		t.Helper()
		c.send('B', append([]any{"", "s"}, bind...)...)
		c.send('S')
		msgs := c.readUntil('Z')
		if f := errorFields(t, msgs[0]); f['C'] != code {
			t.Errorf("%s: got %v, want SQLSTATE %s", what, f, code)
		}
	}
	check("too few parameters", "08P01", int16(0), int16(0), int16(0))
	check("bad text integer", "22P02", int16(0), int16(1), int32(3), []byte("abc"), int16(0))
	check("integer overflow", "22003", int16(0), int16(1), int32(10), []byte("9999999999"), int16(0))
	check("short binary integer", "22P03", int16(1), int16(1), int16(1), int32(2), []byte{0, 1}, int16(0))
	check("bad format code", "08P01", int16(1), int16(7), int16(1), int32(1), []byte("1"), int16(0))
	check("wrong number of result formats", "08P01", int16(0), int16(1), int32(1), []byte("1"), int16(2), int16(0), int16(0))

	c.send('P', "s", "select 1", int16(0))
	c.send('S')
	if f := errorFields(t, c.readUntil('Z')[0]); f['C'] != "42P05" {
		t.Errorf("duplicate statement name: %v", f)
	}
	c.send('P', "", "select 1; select 2", int16(0))
	c.send('S')
	if f := errorFields(t, c.readUntil('Z')[0]); f['C'] != "42601" {
		t.Errorf("multiple statements in Parse: %v", f)
	}
	// Parameters cannot be used in the simple protocol.
	if f := errorFields(t, c.query("select $1")[0]); f['C'] != "42P02" {
		t.Errorf("parameter in a simple query: %v", f)
	}
}

func TestEmptyStatementExtended(t *testing.T) {
	c := connect(t, startServer(t))
	c.send('P', "", "", int16(0))
	c.send('B', "", "", int16(0), int16(0), int16(0))
	c.send('D', byte('P'), "")
	c.send('E', "", int32(0))
	c.send('S')
	expectTypes(t, c.readUntil('Z'), "12nIZ")
}

func TestFlush(t *testing.T) {
	c := connect(t, startServer(t))
	// Without Sync nothing is flushed until the client asks for it.
	c.send('P', "", "select 1", int16(0))
	c.send('H')
	if m := c.read(); m.typ != '1' {
		t.Fatalf("expected ParseComplete after Flush, got %v", m)
	}
	c.send('S')
	expectTypes(t, c.readUntil('Z'), "Z")
}

func TestCancelRequest(t *testing.T) {
	addr := startServer(t)
	c := connect(t, addr)

	start := time.Now()
	c.send('Q', "select pg_sleep(30)")
	time.Sleep(100 * time.Millisecond)

	// A cancel request with the wrong secret must be ignored...
	bad := dial(t, addr)
	bad.raw(binary.BigEndian.AppendUint32(nil, 16), binary.BigEndian.AppendUint32(nil, 80877102),
		binary.BigEndian.AppendUint32(nil, c.pid), binary.BigEndian.AppendUint32(nil, c.secret+1))
	time.Sleep(100 * time.Millisecond)

	// ...and one with the right secret interrupts the running statement.
	// It travels on its own connection, which the server then closes.
	good := dial(t, addr)
	good.raw(binary.BigEndian.AppendUint32(nil, 16), binary.BigEndian.AppendUint32(nil, 80877102),
		binary.BigEndian.AppendUint32(nil, c.pid), binary.BigEndian.AppendUint32(nil, c.secret))
	if _, err := good.r.ReadByte(); err != io.EOF {
		t.Errorf("the cancel connection should be closed without a reply, got %v", err)
	}

	msgs := c.readUntil('Z')
	if f := errorFields(t, msgs[len(msgs)-2]); f['C'] != "57014" {
		t.Errorf("cancelled query: %v", f)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("query ended after %v", elapsed)
	}
	expectTypes(t, c.query("select 1"), "TDCZ")
}

func TestProtocolViolation(t *testing.T) {
	c := connect(t, startServer(t))
	c.send('!', "nonsense")
	if f := errorFields(t, c.read()); f['S'] != "FATAL" || f['C'] != "08P01" {
		t.Errorf("error: %v", f)
	}
	if _, err := c.r.ReadByte(); err != io.EOF {
		t.Errorf("the server should close the connection after a FATAL error, got %v", err)
	}
}

func TestOversizedMessageIsRejected(t *testing.T) {
	c := connect(t, startServer(t))
	c.raw([]byte{'Q'}, binary.BigEndian.AppendUint32(nil, 1<<30))
	if f := errorFields(t, c.read()); f['S'] != "FATAL" || f['C'] != "08P01" {
		t.Errorf("error: %v", f)
	}
}

func TestTerminate(t *testing.T) {
	c := connect(t, startServer(t))
	c.send('X')
	if _, err := c.r.ReadByte(); err != io.EOF {
		t.Errorf("expected EOF after Terminate, got %v", err)
	}
}

func TestDisconnectRollsBack(t *testing.T) {
	addr := startServer(t)
	a := connect(t, addr)
	a.query("create table t (id int)")

	b := connect(t, addr)
	b.query("begin; insert into t values (1)")
	b.nc.Close()

	// The rollback happens when the server notices the disconnect.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if types(a.query("select * from t")) == "TCZ" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the dropped connection's transaction was not rolled back")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConcurrentClients(t *testing.T) {
	addr := startServer(t)
	connect(t, addr).query("create table t (id int primary key, client int)")

	const clients, perClient = 8, 100
	var wg sync.WaitGroup
	for n := 0; n < clients; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := connect(t, addr)
			c.send('P', "ins", "insert into t values ($1, $2)", int16(0))
			c.send('S')
			c.readUntil('Z')
			for i := 0; i < perClient; i++ {
				id := fmt.Sprint(n*perClient + i)
				c.send('B', "", "ins", int16(0), int16(2), int32(len(id)), []byte(id), int32(1), []byte(fmt.Sprint(n)), int16(0))
				c.send('E', "", int32(0))
				c.send('S')
				if got := types(c.readUntil('Z')); got != "2CZ" {
					t.Errorf("client %d: %s", n, got)
					return
				}
			}
		}()
	}
	wg.Wait()

	msgs := connect(t, addr).query("select id from t")
	if tag := cstr(msgs[len(msgs)-2].body); tag != fmt.Sprintf("SELECT %d", clients*perClient) {
		t.Errorf("tag: %q", tag)
	}
}

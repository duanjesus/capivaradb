package pgwire

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/version"
)

const (
	// Request codes that take the place of a protocol version in the first
	// packet of a connection.
	codeSSLRequest    = 80877103
	codeGSSENCRequest = 80877104
	codeCancelRequest = 80877102

	protocolMajor = 3

	maxStartupLen = 10_000
	// maxMessageLen bounds a single frontend message so that a corrupt or
	// hostile length prefix cannot make the server allocate gigabytes.
	maxMessageLen = 64 << 20
)

// errCancelHandled signals that the connection only carried a CancelRequest.
var errCancelHandled = errors.New("cancel request handled")

type conn struct {
	srv *Server
	nc  net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
	out msgWriter
	in  []byte // reusable buffer for message bodies

	sess   Session
	pid    int32
	secret int32

	stmts   map[string]*preparedStmt
	portals map[string]*portal

	// skipToSync is set after an error in the extended protocol: every
	// message up to the next Sync is discarded, which is how a client that
	// pipelines Parse/Bind/Execute/Sync gets exactly one error and one
	// ReadyForQuery back.
	skipToSync bool
	// fatal is set once a FATAL error has been sent; the connection closes.
	fatal bool

	mu      sync.Mutex
	running context.CancelFunc // cancels the statement being executed, if any
}

// preparedStmt is the target of Parse. p is nil for an empty query string.
type preparedStmt struct {
	p Prepared
}

// portal is the target of Bind: a statement plus parameter values, possibly
// part-way through returning its rows.
type portal struct {
	stmt    *preparedStmt
	params  []any
	cols    []Column
	formats []int16 // per result column; nil means all text
	rows    Rows
	done    bool
	tag     string
}

func newConn(s *Server, nc net.Conn) *conn {
	return &conn{
		srv:     s,
		nc:      nc,
		r:       bufio.NewReader(nc),
		w:       bufio.NewWriter(nc),
		stmts:   make(map[string]*preparedStmt),
		portals: make(map[string]*portal),
	}
}

func (c *conn) serve() {
	defer c.nc.Close()
	defer func() {
		if r := recover(); r != nil {
			c.srv.Logger.Error("panic serving connection", "remote", c.nc.RemoteAddr(), "panic", r, "stack", string(debug.Stack()))
			c.sendError(pgerr.Fatal(pgerr.InternalError, "internal error: %v", r))
			c.w.Flush()
		}
	}()

	params, err := c.startup()
	if err != nil {
		if !errors.Is(err, errCancelHandled) && !errors.Is(err, io.EOF) {
			c.srv.Logger.Debug("startup failed", "remote", c.nc.RemoteAddr(), "err", err)
		}
		c.w.Flush()
		return
	}

	sess, err := c.srv.Handler.NewSession(params)
	if err != nil {
		pe := pgerr.From(err)
		pe.Severity = pgerr.SeverityFatal
		c.sendError(pe)
		c.w.Flush()
		return
	}
	c.sess = sess
	defer c.closeSession()

	c.srv.register(c)
	defer c.srv.unregister(c)
	c.srv.Logger.Info("connection opened", "pid", c.pid, "remote", c.nc.RemoteAddr(), "user", params["user"], "database", params["database"])
	defer c.srv.Logger.Info("connection closed", "pid", c.pid)

	c.sendGreeting(params)
	c.sendReady()

	for !c.fatal {
		typ, body, err := c.readMessage()
		if err != nil {
			var pe *pgerr.Error
			if errors.As(err, &pe) {
				c.sendError(pe)
				c.w.Flush()
			}
			return
		}
		if c.skipToSync && typ != msgSync && typ != msgTerminate {
			continue
		}
		switch typ {
		case msgQuery:
			c.handleQuery(body)
		case msgParse:
			err = c.handleParse(body)
		case msgBind:
			err = c.handleBind(body)
		case msgDescribe:
			err = c.handleDescribe(body)
		case msgExecute:
			err = c.handleExecute(body)
		case msgClose:
			err = c.handleClose(body)
		case msgSync:
			c.handleSync()
		case msgFlush:
			c.w.Flush()
		case msgTerminate:
			return
		case msgCopyData, msgCopyDone, msgCopyFail:
			// Stray COPY messages are ignored, as PostgreSQL does.
		case msgFunction:
			c.sendError(pgerr.New(pgerr.FeatureNotSupported, "the function call sub-protocol is not supported"))
			c.sendReady()
		default:
			err = pgerr.Fatal(pgerr.ProtocolViolation, "invalid frontend message type %d", typ)
		}
		if err != nil {
			c.sendError(pgerr.From(err))
			c.skipToSync = true
			c.w.Flush()
		}
	}
	c.w.Flush()
}

func (c *conn) closeSession() {
	for name := range c.portals {
		c.closePortal(name)
	}
	c.sess.Close()
}

// startup reads startup packets until it gets a real StartupMessage and
// returns its parameters. The first packets of a connection have no type
// byte: just a length and a 32-bit code.
func (c *conn) startup() (map[string]string, error) {
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
			return nil, io.EOF
		}
		n := int(binary.BigEndian.Uint32(hdr[:4]))
		code := binary.BigEndian.Uint32(hdr[4:])
		if n < 8 || n > maxStartupLen {
			err := pgerr.Fatal(pgerr.ProtocolViolation, "invalid length of startup packet")
			c.sendError(err)
			return nil, err
		}
		body := make([]byte, n-8)
		if _, err := io.ReadFull(c.r, body); err != nil {
			return nil, io.EOF
		}

		switch code {
		case codeSSLRequest, codeGSSENCRequest:
			// A single byte, outside the normal message framing: 'N' means
			// "not supported, carry on in plaintext".
			c.w.WriteByte('N')
			if err := c.w.Flush(); err != nil {
				return nil, io.EOF
			}
			continue
		case codeCancelRequest:
			r := msgReader{b: body}
			pid, secret := r.int32(), r.int32()
			if !r.bad {
				c.srv.cancel(pid, secret)
			}
			return nil, errCancelHandled
		}

		major, minor := code>>16, code&0xffff
		if major != protocolMajor {
			err := pgerr.Fatal(pgerr.FeatureNotSupported,
				"unsupported frontend protocol %d.%d: server supports %d.0", major, minor, protocolMajor)
			c.sendError(err)
			return nil, err
		}

		params := make(map[string]string)
		var unrecognized []string
		r := msgReader{b: body}
		for {
			key := r.cstring()
			if key == "" || r.bad {
				break
			}
			val := r.cstring()
			// Options prefixed with "_pq_." are protocol extensions.
			// None is implemented, so all of them are reported back.
			if strings.HasPrefix(key, "_pq_.") {
				unrecognized = append(unrecognized, key)
				continue
			}
			params[key] = val
		}
		if r.bad {
			err := pgerr.Fatal(pgerr.ProtocolViolation, "invalid startup packet layout: expected terminator as last byte")
			c.sendError(err)
			return nil, err
		}
		if params["user"] == "" {
			err := pgerr.Fatal("28000", "no PostgreSQL user name specified in startup packet")
			c.sendError(err)
			return nil, err
		}
		if params["database"] == "" {
			params["database"] = params["user"]
		}
		// A client asking for a newer 3.x minor version, or for protocol
		// extensions, is told what the server can do and may then decide
		// to proceed or hang up.
		if minor > 0 || len(unrecognized) > 0 {
			c.out.begin(msgNegotiateProtocol)
			c.out.int32(0) // newest minor version supported
			c.out.int32(int32(len(unrecognized)))
			for _, name := range unrecognized {
				c.out.cstring(name)
			}
			c.w.Write(c.out.finish())
		}
		return params, nil
	}
}

// sendGreeting completes the startup phase. Authentication is "trust": any
// user is accepted without a password.
func (c *conn) sendGreeting(params map[string]string) {
	c.out.begin(msgAuthentication)
	c.out.int32(0) // AuthenticationOk
	c.w.Write(c.out.finish())

	// Drivers read these to configure themselves. pgjdbc, for instance,
	// refuses to continue unless client_encoding is UTF8 and switches its
	// timestamp decoding on integer_datetimes.
	status := [][2]string{
		{"server_version", version.PGCompat},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
		{"TimeZone", "UTC"},
		{"integer_datetimes", "on"},
		{"standard_conforming_strings", "on"},
		{"application_name", params["application_name"]},
		{"session_authorization", params["user"]},
		{"is_superuser", "on"},
	}
	for _, kv := range status {
		c.out.begin(msgParameterStatus)
		c.out.cstring(kv[0])
		c.out.cstring(kv[1])
		c.w.Write(c.out.finish())
	}

	c.out.begin(msgBackendKeyData)
	c.out.int32(c.pid)
	c.out.int32(c.secret)
	c.w.Write(c.out.finish())
}

// readMessage reads one regular message: a type byte, a length that counts
// itself, and the body. The returned body is only valid until the next call.
func (c *conn) readMessage() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[1:]))
	if n < 4 || n > maxMessageLen {
		return 0, nil, pgerr.Fatal(pgerr.ProtocolViolation, "invalid message length")
	}
	n -= 4
	if cap(c.in) < n {
		c.in = make([]byte, n)
	}
	body := c.in[:n]
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body, nil
}

func malformed(what string) error {
	return pgerr.Fatal(pgerr.ProtocolViolation, "invalid %s message", what)
}

// ---- simple query protocol ----

func (c *conn) handleQuery(body []byte) {
	r := msgReader{b: body}
	query := r.cstring()
	if r.bad {
		c.sendError(pgerr.From(malformed("Query")))
		return
	}
	// A simple query implicitly discards the unnamed statement and portal.
	c.closePortal("")
	delete(c.stmts, "")

	if err := c.simpleQuery(query); err != nil {
		c.sendError(pgerr.From(err))
	}
	c.sendReady()
}

func (c *conn) simpleQuery(query string) error {
	stmts, err := c.sess.Parse(query)
	if err != nil {
		return err
	}
	if len(stmts) == 0 {
		c.out.begin(msgEmptyQueryResponse)
		c.w.Write(c.out.finish())
		return nil
	}
	for _, st := range stmts {
		p, err := c.sess.Prepare(st, nil)
		if err != nil {
			return err
		}
		if len(p.ParamOIDs()) > 0 {
			return pgerr.New(pgerr.UndefinedParameter, "there is no parameter $1")
		}
		pt := &portal{stmt: &preparedStmt{p: p}, cols: p.Columns()}
		if pt.cols != nil {
			c.writeRowDescription(pt.cols, nil)
		}
		if err := c.runPortal(pt, 0); err != nil {
			return err
		}
	}
	return nil
}

// ---- extended query protocol ----

func (c *conn) handleParse(body []byte) error {
	r := msgReader{b: body}
	name, query := r.cstring(), r.cstring()
	n := r.int16()
	if r.bad || n < 0 {
		return malformed("Parse")
	}
	oids := make([]uint32, n)
	for i := range oids {
		oids[i] = uint32(r.int32())
	}
	if r.bad {
		return malformed("Parse")
	}
	if name != "" {
		if _, dup := c.stmts[name]; dup {
			return pgerr.New(pgerr.DuplicatePreparedStatement, "prepared statement %q already exists", name)
		}
	}

	stmts, err := c.sess.Parse(query)
	if err != nil {
		return err
	}
	if len(stmts) > 1 {
		return pgerr.New(pgerr.SyntaxError, "cannot insert multiple commands into a prepared statement")
	}
	ps := &preparedStmt{}
	if len(stmts) == 1 {
		if ps.p, err = c.sess.Prepare(stmts[0], oids); err != nil {
			return err
		}
	}
	c.stmts[name] = ps

	c.out.begin(msgParseComplete)
	c.w.Write(c.out.finish())
	return nil
}

func (c *conn) lookupStmt(name string) (*preparedStmt, error) {
	if ps, ok := c.stmts[name]; ok {
		return ps, nil
	}
	if name == "" {
		return nil, pgerr.New(pgerr.InvalidSQLStatementName, "unnamed prepared statement does not exist")
	}
	return nil, pgerr.New(pgerr.InvalidSQLStatementName, "prepared statement %q does not exist", name)
}

func (c *conn) lookupPortal(name string) (*portal, error) {
	if pt, ok := c.portals[name]; ok {
		return pt, nil
	}
	return nil, pgerr.New(pgerr.InvalidCursorName, "portal %q does not exist", name)
}

func (c *conn) handleBind(body []byte) error {
	r := msgReader{b: body}
	portalName, stmtName := r.cstring(), r.cstring()
	nFormats := r.int16()
	if r.bad || nFormats < 0 {
		return malformed("Bind")
	}
	paramFormats := make([]int16, nFormats)
	for i := range paramFormats {
		paramFormats[i] = int16(r.int16())
	}
	nParams := r.int16()
	if r.bad || nParams < 0 {
		return malformed("Bind")
	}

	ps, err := c.lookupStmt(stmtName)
	if err != nil {
		return err
	}
	var oids []uint32
	var cols []Column
	if ps.p != nil {
		oids, cols = ps.p.ParamOIDs(), ps.p.Columns()
	}
	if nParams != len(oids) {
		return pgerr.New(pgerr.ProtocolViolation,
			"bind message supplies %d parameters, but prepared statement %q requires %d", nParams, stmtName, len(oids))
	}
	if nFormats > 1 && nFormats != nParams {
		return pgerr.New(pgerr.ProtocolViolation,
			"bind message has %d parameter formats but %d parameters", nFormats, nParams)
	}

	params := make([]any, nParams)
	for i := range params {
		size := r.int32()
		if size == -1 {
			continue // NULL
		}
		raw := r.bytes(int(size))
		if r.bad {
			return malformed("Bind")
		}
		// Zero format codes means all text; one applies to every
		// parameter; otherwise there is one per parameter.
		format := FormatText
		switch {
		case nFormats == 1:
			format = paramFormats[0]
		case nFormats > 1:
			format = paramFormats[i]
		}
		if format != FormatText && format != FormatBinary {
			return pgerr.New(pgerr.ProtocolViolation, "invalid format code: %d", format)
		}
		if params[i], err = decodeParam(oids[i], format, raw); err != nil {
			return err
		}
	}

	nResults := r.int16()
	if r.bad || nResults < 0 {
		return malformed("Bind")
	}
	resultFormats := make([]int16, nResults)
	for i := range resultFormats {
		resultFormats[i] = int16(r.int16())
		if resultFormats[i] != FormatText && resultFormats[i] != FormatBinary {
			return pgerr.New(pgerr.ProtocolViolation, "invalid format code: %d", resultFormats[i])
		}
	}
	if r.bad {
		return malformed("Bind")
	}
	var formats []int16
	switch {
	case nResults == 1:
		formats = make([]int16, len(cols))
		for i := range formats {
			formats[i] = resultFormats[0]
		}
	case nResults > 1:
		if nResults != len(cols) {
			return pgerr.New(pgerr.ProtocolViolation,
				"bind message has %d result formats but query has %d columns", nResults, len(cols))
		}
		formats = resultFormats
	}

	if portalName != "" {
		if _, dup := c.portals[portalName]; dup {
			return pgerr.New(pgerr.DuplicateCursor, "cursor %q already exists", portalName)
		}
	}
	c.closePortal(portalName)
	c.portals[portalName] = &portal{stmt: ps, params: params, cols: cols, formats: formats}

	c.out.begin(msgBindComplete)
	c.w.Write(c.out.finish())
	return nil
}

func (c *conn) handleDescribe(body []byte) error {
	r := msgReader{b: body}
	kind, name := r.byte(), r.cstring()
	if r.bad {
		return malformed("Describe")
	}
	switch kind {
	case 'S':
		ps, err := c.lookupStmt(name)
		if err != nil {
			return err
		}
		var oids []uint32
		var cols []Column
		if ps.p != nil {
			oids, cols = ps.p.ParamOIDs(), ps.p.Columns()
		}
		c.out.begin(msgParameterDescription)
		c.out.int16(len(oids))
		for _, oid := range oids {
			c.out.uint32(oid)
		}
		c.w.Write(c.out.finish())
		// The result format is not known until Bind, so a statement is
		// always described with text format codes.
		c.writeRowDescriptionOrNoData(cols, nil)
	case 'P':
		pt, err := c.lookupPortal(name)
		if err != nil {
			return err
		}
		c.writeRowDescriptionOrNoData(pt.cols, pt.formats)
	default:
		return pgerr.New(pgerr.ProtocolViolation, "invalid DESCRIBE message subtype %d", kind)
	}
	return nil
}

func (c *conn) handleExecute(body []byte) error {
	r := msgReader{b: body}
	name, maxRows := r.cstring(), r.int32()
	if r.bad {
		return malformed("Execute")
	}
	pt, err := c.lookupPortal(name)
	if err != nil {
		return err
	}
	return c.runPortal(pt, int(maxRows))
}

func (c *conn) handleClose(body []byte) error {
	r := msgReader{b: body}
	kind, name := r.byte(), r.cstring()
	if r.bad {
		return malformed("Close")
	}
	// Closing something that does not exist is not an error.
	switch kind {
	case 'S':
		if ps, ok := c.stmts[name]; ok {
			for pname, pt := range c.portals {
				if pt.stmt == ps {
					c.closePortal(pname)
				}
			}
			delete(c.stmts, name)
		}
	case 'P':
		c.closePortal(name)
	default:
		return pgerr.New(pgerr.ProtocolViolation, "invalid CLOSE message subtype %d", kind)
	}
	c.out.begin(msgCloseComplete)
	c.w.Write(c.out.finish())
	return nil
}

func (c *conn) handleSync() {
	c.skipToSync = false
	c.sendReady()
}

func (c *conn) closePortal(name string) {
	if pt, ok := c.portals[name]; ok {
		if pt.rows != nil {
			pt.rows.Close()
		}
		delete(c.portals, name)
	}
}

// runPortal sends up to maxRows rows of the portal (all of them if maxRows
// is zero), followed by CommandComplete, or by PortalSuspended if the limit
// was reached first.
func (c *conn) runPortal(pt *portal, maxRows int) error {
	if pt.stmt.p == nil {
		c.out.begin(msgEmptyQueryResponse)
		c.w.Write(c.out.finish())
		return nil
	}
	if pt.done {
		c.writeCommandComplete(pt.tag)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.running = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = nil
		c.mu.Unlock()
		cancel()
	}()

	if pt.rows == nil {
		rows, err := pt.stmt.p.Execute(ctx, pt.params)
		if err != nil {
			return err
		}
		pt.rows = rows
	}
	for sent := 0; ; sent++ {
		if maxRows > 0 && sent >= maxRows {
			c.out.begin(msgPortalSuspended)
			c.w.Write(c.out.finish())
			return nil
		}
		row, err := pt.rows.Next(ctx)
		if err == io.EOF {
			pt.done, pt.tag = true, pt.rows.Tag()
			pt.rows.Close()
			pt.rows = nil
			c.writeCommandComplete(pt.tag)
			return nil
		}
		if err != nil {
			return err
		}
		if err := c.writeDataRow(pt, row); err != nil {
			return err
		}
	}
}

// cancelRunning aborts the statement currently executing on this connection,
// if any. It is called from the goroutine that received the CancelRequest.
func (c *conn) cancelRunning() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running != nil {
		c.running()
	}
}

// ---- backend messages ----

func (c *conn) writeRowDescriptionOrNoData(cols []Column, formats []int16) {
	if cols == nil {
		c.out.begin(msgNoData)
		c.w.Write(c.out.finish())
		return
	}
	c.writeRowDescription(cols, formats)
}

func (c *conn) writeRowDescription(cols []Column, formats []int16) {
	c.out.begin(msgRowDescription)
	c.out.int16(len(cols))
	for i, col := range cols {
		c.out.cstring(col.Name)
		c.out.int32(0) // table OID: not a column of a catalogued table
		c.out.int16(0) // attribute number
		c.out.uint32(col.OID)
		c.out.int16(int(typeLen(col.OID)))
		c.out.int32(-1) // type modifier
		if formats != nil {
			c.out.int16(int(formats[i]))
		} else {
			c.out.int16(int(FormatText))
		}
	}
	c.w.Write(c.out.finish())
}

func (c *conn) writeDataRow(pt *portal, row []any) error {
	if len(row) != len(pt.cols) {
		return fmt.Errorf("row has %d values but %d columns were described", len(row), len(pt.cols))
	}
	c.out.begin(msgDataRow)
	c.out.int16(len(row))
	for i, v := range row {
		if v == nil {
			c.out.int32(-1)
			continue
		}
		format := FormatText
		if pt.formats != nil {
			format = pt.formats[i]
		}
		lenAt := len(c.out.b)
		c.out.int32(0)
		b, err := appendValue(c.out.b, pt.cols[i].OID, format, v)
		if err != nil {
			return err
		}
		c.out.b = b
		binary.BigEndian.PutUint32(c.out.b[lenAt:], uint32(len(c.out.b)-lenAt-4))
	}
	c.w.Write(c.out.finish())
	return nil
}

func (c *conn) writeCommandComplete(tag string) {
	c.out.begin(msgCommandComplete)
	c.out.cstring(tag)
	c.w.Write(c.out.finish())
}

// sendError writes an ErrorResponse and tells the session about it.
func (c *conn) sendError(e *pgerr.Error) {
	if c.sess != nil {
		c.sess.OnError()
	}
	if e.Severity == pgerr.SeverityFatal {
		c.fatal = true
	}
	c.out.begin(msgErrorResponse)
	field := func(code byte, value string) {
		c.out.byte(code)
		c.out.cstring(value)
	}
	field('S', e.Severity)
	field('V', e.Severity) // non-localised severity, same thing here
	field('C', e.Code)
	field('M', e.Message)
	if e.Detail != "" {
		field('D', e.Detail)
	}
	if e.Hint != "" {
		field('H', e.Hint)
	}
	if e.Position > 0 {
		field('P', fmt.Sprint(e.Position))
	}
	c.out.byte(0)
	c.w.Write(c.out.finish())
}

// sendReady writes ReadyForQuery and flushes: this is the point where the
// client gets control back.
func (c *conn) sendReady() {
	status := c.sess.TxStatus()
	// Portals do not outlive their transaction. Outside a transaction
	// block that means the implicit transaction which ends right here.
	if status == 'I' {
		for name := range c.portals {
			c.closePortal(name)
		}
	}
	c.out.begin(msgReadyForQuery)
	c.out.byte(status)
	c.w.Write(c.out.finish())
	c.w.Flush()
}

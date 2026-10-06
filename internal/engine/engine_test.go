package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
)

// harness drives a Session the way the protocol layer does, without a socket.
type harness struct {
	t    *testing.T
	sess pgwire.Session
}

func newHarness(t *testing.T, db *DB) *harness {
	t.Helper()
	sess, err := db.NewSession(map[string]string{"user": "ana", "database": "capi"})
	if err != nil {
		t.Fatal(err)
	}
	// Registered first so that it runs last, after the session has closed:
	// whatever the test did, the file must still be consistent and every
	// page accounted for.
	t.Cleanup(func() {
		if db.closed {
			return // the test closed it, and checked it before doing so
		}
		if _, err := db.Verify(); err != nil {
			t.Errorf("database is inconsistent after the test: %v", err)
		}
	})
	t.Cleanup(sess.Close)
	return &harness{t: t, sess: sess}
}

// run executes every statement in query and returns the result of the last
// one, rendered as "a|b;c|d" plus the command tag.
func (h *harness) run(query string, params ...any) (rows string, tag string, err error) {
	defer func() {
		if err != nil {
			h.sess.OnError()
		}
	}()
	stmts, err := h.sess.Parse(query)
	if err != nil {
		return "", "", err
	}
	for _, st := range stmts {
		p, err := h.sess.Prepare(st, nil)
		if err != nil {
			return "", "", err
		}
		res, err := p.Execute(context.Background(), params)
		if err != nil {
			return "", "", err
		}
		var lines []string
		for {
			row, err := res.Next(context.Background())
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", "", err
			}
			cells := make([]string, len(row))
			for i, v := range row {
				if v == nil {
					cells[i] = "NULL"
				} else {
					cells[i] = pgwire.TextValue(v)
				}
			}
			lines = append(lines, strings.Join(cells, "|"))
		}
		rows, tag = strings.Join(lines, ";"), res.Tag()
	}
	return rows, tag, nil
}

func (h *harness) mustRun(query string, params ...any) string {
	h.t.Helper()
	rows, _, err := h.run(query, params...)
	if err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
	return rows
}

func (h *harness) expect(query, want string, params ...any) {
	h.t.Helper()
	if got := h.mustRun(query, params...); got != want {
		h.t.Errorf("%s:\n got %s\nwant %s", query, got, want)
	}
}

func (h *harness) expectError(query, code string, params ...any) *pgerr.Error {
	h.t.Helper()
	_, _, err := h.run(query, params...)
	var pe *pgerr.Error
	if !errors.As(err, &pe) {
		h.t.Fatalf("%s: expected SQLSTATE %s, got %v", query, code, err)
	}
	if pe.Code != code {
		h.t.Errorf("%s: got SQLSTATE %s (%s), want %s", query, pe.Code, pe.Message, code)
	}
	return pe
}

func TestExpressions(t *testing.T) {
	h := newHarness(t, New())
	cases := []struct{ expr, want string }{
		{"1 + 2 * 3", "7"},
		{"7 / 2", "3"},
		{"-7 / 2", "-3"},
		{"7 % 3", "1"},
		{"7 / 2.0", "3.5"},
		{"1 + 2147483648", "2147483649"},
		{"2.5::int", "2"}, // ties round to even
		{"3.5::int", "4"},
		{"'42'::int + 1", "43"},
		{"42::text || '!'", "42!"},
		{"'a' || 1", "a1"},
		{"1 || 'a'", "1a"},
		// A quoted literal takes the type of what it is compared with.
		{"'42' = 42", "t"},
		{"41 + '1'", "42"},
		{"true = 'yes'", "t"},
		{"true::text", "true"},
		{"1 = 1", "t"},
		{"1 = 1.0", "t"},
		{"'b' > 'a'", "t"},
		{"1 < 2 and 2 < 1", "f"},
		{"not true", "f"},
		{"null is null", "t"},
		{"1 is not null", "t"},
		{"null = null", "NULL"},
		{"1 + null", "NULL"},
		{"null and false", "f"},
		{"null and true", "NULL"},
		{"null or true", "t"},
		{"null or false", "NULL"},
		{"length('capivará')", "8"},
		{"upper('abc') || lower('DEF')", "ABCdef"},
		{"current_database()", "capi"},
		{"current_user", "ana"},
	}
	for _, tc := range cases {
		h.expect("select "+tc.expr, tc.want)
	}
}

func TestExpressionErrors(t *testing.T) {
	h := newHarness(t, New())
	cases := []struct{ expr, code string }{
		{"1 / 0", pgerr.DivisionByZero},
		{"1.0 / 0", pgerr.DivisionByZero},
		{"5 % 0", pgerr.DivisionByZero},
		{"2147483647 + 1", pgerr.NumericValueOutOfRange},
		{"9223372036854775807 + 1", pgerr.NumericValueOutOfRange},
		{"9223372036854775807 * 2", pgerr.NumericValueOutOfRange},
		{"3000000000::int", pgerr.NumericValueOutOfRange},
		{"'abc'::int", pgerr.InvalidTextRepresentation},
		{"1 + 'a'", pgerr.InvalidTextRepresentation},
		{"1 + upper('a')", pgerr.UndefinedFunction},
		{"1 = 'a'", pgerr.InvalidTextRepresentation},
		{"1 = lower('a')", pgerr.UndefinedFunction},
		{"1 and true", pgerr.DatatypeMismatch},
		{"not 1", pgerr.DatatypeMismatch},
		{"nope", pgerr.UndefinedColumn},
		{"nope(1)", pgerr.UndefinedFunction},
		{"t.x", pgerr.UndefinedTable},
		{"true::double precision", pgerr.CannotCoerce},
	}
	for _, tc := range cases {
		h.expectError("select "+tc.expr, tc.code)
	}
}

func TestResultTypes(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun("create table t (i int, b bigint, f float8, s text, k bool)")
	cases := []struct {
		query string
		want  []uint32
	}{
		{"select 1, 2147483648, 1.5, 'x', true, null", []uint32{23, 20, 701, 25, 16, 25}},
		{"select i + i, i + b, i + f, i = b, s || i from t", []uint32{23, 20, 701, 16, 25}},
		{"select * from t", []uint32{23, 20, 701, 25, 16}},
		{"select i::bigint, f::int, i::text from t", []uint32{20, 23, 25}},
	}
	for _, tc := range cases {
		stmts, err := h.sess.Parse(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		p, err := h.sess.Prepare(stmts[0], nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		var got []uint32
		for _, c := range p.Columns() {
			got = append(got, c.OID)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestParameterTypeInference(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun("create table t (id int, big bigint, name text, ok bool, score float8)")
	cases := []struct {
		query    string
		declared []uint32
		want     []uint32
	}{
		{"select * from t where id = $1", nil, []uint32{23}},
		{"select * from t where $1 = id", nil, []uint32{23}},
		{"select * from t where big > $1 and name = $2", nil, []uint32{20, 25}},
		{"select * from t where ok = $1 or score < $2", nil, []uint32{16, 701}},
		{"select * from t where $1", nil, []uint32{16}},
		{"insert into t values ($1, $2, $3, $4, $5)", nil, []uint32{23, 20, 25, 16, 701}},
		{"insert into t (name, id) values ($1, $2)", nil, []uint32{25, 23}},
		{"update t set name = $1, score = $2 where id = $3", nil, []uint32{25, 701, 23}},
		{"delete from t where id = $2 and name = $1", nil, []uint32{25, 23}},
		{"select $1", nil, []uint32{25}},
		{"select $1::bigint", nil, []uint32{20}},
		// A use later in the statement types an earlier one.
		{"select $1, id from t where id = $1", nil, []uint32{23}},
		{"select $1 || 'x'", nil, []uint32{25}},
		{"select id + $1 from t", nil, []uint32{23}},
		// A type declared by the client wins and is echoed back verbatim.
		{"select * from t where id = $1", []uint32{20}, []uint32{20}},
		{"select * from t where id = $1", []uint32{21}, []uint32{21}},
		{"select * from t where name = $1", []uint32{1043}, []uint32{1043}},
		{"select * from t where id = $1", []uint32{0}, []uint32{23}},
	}
	for _, tc := range cases {
		stmts, err := h.sess.Parse(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		p, err := h.sess.Prepare(stmts[0], tc.declared)
		if err != nil {
			t.Errorf("%s: %v", tc.query, err)
			continue
		}
		if got := p.ParamOIDs(); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s (declared %v): got %v, want %v", tc.query, tc.declared, got, tc.want)
		}
	}

	h.mustRun("insert into t values ($1, $2, $3, $4, $5)", int64(1), int64(10), "ana", true, 1.5)
	h.expect("select name, $1 from t where id = $1", "ana|1", int64(1))
	h.expectError("select $1 + $2", pgerr.UndefinedFunction)
}

func TestDML(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun("create table users (id int primary key, name text not null, age bigint, score float8)")

	if _, tag, _ := h.run("insert into users values (1, 'ana', 30, 1.5), (2, 'bia', 25, 2), (3, 'caio')"); tag != "INSERT 0 3" {
		t.Errorf("insert tag: %s", tag)
	}
	h.expect("select * from users", "1|ana|30|1.5;2|bia|25|2;3|caio|NULL|NULL")
	h.expect("select u.name, age + 1 from users u where u.age > 26", "ana|31")
	h.expect("select name from users where age is null", "caio")
	// A comparison with NULL is unknown, which WHERE treats as false.
	h.expect("select name from users where age <> 30", "bia")

	if _, tag, _ := h.run("update users set age = age + 1, name = upper(name) where id <= 2"); tag != "UPDATE 2" {
		t.Errorf("update tag: %s", tag)
	}
	h.expect("select name, age from users where id < 3", "ANA|31;BIA|26")

	h.mustRun("create table swap (a int, b int); insert into swap values (1, 2); update swap set a = b, b = a")
	h.expect("select * from swap", "2|1")

	if _, tag, _ := h.run("delete from users where age is null"); tag != "DELETE 1" {
		t.Errorf("delete tag: %s", tag)
	}
	h.expect("select id from users", "1;2")
	if _, tag, _ := h.run("select id from users"); tag != "SELECT 2" {
		t.Errorf("select tag: %s", tag)
	}

	h.mustRun("drop table swap")
	h.expectError("select * from swap", pgerr.UndefinedTable)
	h.mustRun("drop table if exists swap; create table if not exists users (x int)")
	h.expect("select id from users", "1;2")
}

func TestConstraints(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun("create table t (id int primary key, name text not null, n int)")
	h.mustRun("insert into t values (1, 'a', 1), (2, 'b', 2)")

	pe := h.expectError("insert into t values (1, 'dup', 0)", pgerr.UniqueViolation)
	if pe.Detail != "Key (id)=(1) already exists." {
		t.Errorf("detail: %q", pe.Detail)
	}
	h.expectError("insert into t values (3, null, 0)", pgerr.NotNullViolation)
	h.expectError("insert into t (name) values ('no id')", pgerr.NotNullViolation)
	h.expectError("update t set id = 1 where id = 2", pgerr.UniqueViolation)
	h.expectError("update t set name = null", pgerr.NotNullViolation)
	h.mustRun("update t set id = id where id = 1") // a row does not conflict with itself

	h.expectError("insert into t values (4, 5, 6)", pgerr.DatatypeMismatch)
	h.expectError("insert into t values (4, 'x', 'y')", pgerr.InvalidTextRepresentation)
	h.expectError("insert into t values (4, 'x', upper('y'))", pgerr.DatatypeMismatch)
	h.expectError("insert into t values (4, 'x', 3000000000)", pgerr.NumericValueOutOfRange)
	h.expectError("insert into t values (4, 'x', 1, 2)", pgerr.SyntaxError)
	h.expectError("insert into t (id, name) values (4)", pgerr.SyntaxError)
	h.expectError("insert into t (id, nope) values (4, 'x')", pgerr.UndefinedColumn)
	h.expectError("insert into t (id, id) values (4, 5)", pgerr.DuplicateColumn)
	h.expectError("insert into t values (4, name, 1)", pgerr.UndefinedColumn)
	h.expectError("create table t (a int)", pgerr.DuplicateTable)
	h.expectError("create table u (a int, a text)", pgerr.DuplicateColumn)
	h.expectError("select * from t where n", pgerr.DatatypeMismatch)

	// A multi-row statement that fails half-way leaves nothing behind.
	h.expectError("insert into t values (10, 'x', 0), (11, 'y', 0), (1, 'dup', 0)", pgerr.UniqueViolation)
	h.expectError("update t set n = 10 / (n - 2)", pgerr.DivisionByZero)
	h.expect("select * from t", "1|a|1;2|b|2")
}

func TestTransactions(t *testing.T) {
	h := newHarness(t, New())
	status := func(want byte) {
		t.Helper()
		if got := h.sess.TxStatus(); got != want {
			t.Errorf("transaction status: got %c, want %c", got, want)
		}
	}
	h.mustRun("create table t (id int primary key, v text); insert into t values (1, 'a'), (2, 'b'), (3, 'c')")
	status('I')

	h.mustRun("begin")
	status('T')
	h.mustRun("insert into t values (4, 'd'); update t set v = 'B' where id = 2; delete from t where id = 1 or id = 3")
	h.mustRun("create table extra (x int); drop table extra; create table kept (x int)")
	h.expect("select * from t", "2|B;4|d")
	if _, tag, _ := h.run("rollback"); tag != "ROLLBACK" {
		t.Errorf("rollback tag: %s", tag)
	}
	status('I')
	// Everything is undone, and deleted rows are back in their old places.
	h.expect("select * from t", "1|a;2|b;3|c")
	h.expectError("select * from kept", pgerr.UndefinedTable)

	h.mustRun("begin; insert into t values (4, 'd'); commit")
	h.expect("select * from t", "1|a;2|b;3|c;4|d")

	// An error poisons the transaction: only ROLLBACK/COMMIT are accepted,
	// and COMMIT then reports that it rolled back.
	h.mustRun("begin; insert into t values (5, 'e')")
	h.expectError("select 1 / 0", pgerr.DivisionByZero)
	status('E')
	h.expectError("select 1", pgerr.InFailedSQLTransaction)
	if _, tag, _ := h.run("commit"); tag != "ROLLBACK" {
		t.Errorf("commit of a failed transaction: %s", tag)
	}
	status('I')
	h.expect("select * from t", "1|a;2|b;3|c;4|d")
}

func TestSessionCloseRollsBack(t *testing.T) {
	db := New()
	a := newHarness(t, db)
	a.mustRun("create table t (id int)")

	sess, _ := db.NewSession(map[string]string{"user": "bia"})
	b := &harness{t: t, sess: sess}
	b.mustRun("begin; insert into t values (1)")
	sess.Close()

	a.expect("select * from t", "")
}

func TestSetShow(t *testing.T) {
	h := newHarness(t, New())
	h.expect("show server_version", "16.0")
	h.expect("show transaction isolation level", "read committed")
	h.mustRun("set application_name = 'tests'; set extra_float_digits to 3")
	h.expect("show application_name", "tests")
	h.expect("show extra_float_digits", "3")
	h.expectError("show nope", pgerr.UndefinedObject)
}

func TestStalePreparedStatement(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun("create table t (id int)")
	stmts, _ := h.sess.Parse("select * from t")
	p, err := h.sess.Prepare(stmts[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	h.mustRun("drop table t; create table t (name text)")
	if _, err := p.Execute(context.Background(), nil); pgerr.From(err).Code != pgerr.UndefinedTable {
		t.Errorf("expected the stale statement to be rejected, got %v", err)
	}
}

func TestConcurrentSessions(t *testing.T) {
	db := New()
	newHarness(t, db).mustRun("create table t (id int primary key, worker int)")

	const workers, perWorker = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, _ := db.NewSession(map[string]string{"user": "w"})
			defer sess.Close()
			h := &harness{t: t, sess: sess}
			for i := 0; i < perWorker; i++ {
				// Half the inserts are rolled back; readers run alongside.
				id := int64(w*perWorker + i)
				if i%2 == 0 {
					h.mustRun("insert into t values ($1, $2)", id, int64(w))
				} else {
					h.mustRun("begin")
					h.mustRun("insert into t values ($1, $2)", id, int64(w))
					h.mustRun("rollback")
				}
				h.mustRun("select * from t where worker = $1", int64(w))
			}
		}()
	}
	wg.Wait()

	h := newHarness(t, db)
	_, tag, err := h.run("select id from t")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("SELECT %d", workers*perWorker/2); tag != want {
		t.Errorf("got %s, want %s", tag, want)
	}
}

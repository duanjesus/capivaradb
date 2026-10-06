package sql

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/duanjesus/capivaradb/internal/pgerr"
)

func TestLex(t *testing.T) {
	toks, err := Lex(`SELECT "Weird ""Name""", 'it''s', 1.5e3, $12 -- trailing
		/* nested /* comment */ still comment */ <> ::`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tok := range toks {
		got = append(got, fmt.Sprintf("%d:%s", tok.Kind, tok.Text))
	}
	want := []string{
		"1:select", `2:Weird "Name"`, "7:,", "5:it's", "7:,", "4:1.5e3", "7:,", "6:12", "7:<>", "7:::", "0:",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("tokens:\n got %v\nwant %v", got, want)
	}
}

func TestLexNumbers(t *testing.T) {
	cases := map[string]TokenKind{
		"42": TInt, "4.2": TFloat, ".5": TFloat, "1.": TFloat, "1e10": TFloat, "1E-3": TFloat,
	}
	for src, kind := range cases {
		toks, err := Lex(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if len(toks) != 2 || toks[0].Kind != kind || toks[0].Text != src {
			t.Errorf("%s: got %+v", src, toks)
		}
	}
	// "1e" is the integer 1 followed by the identifier e, not a broken float.
	toks, _ := Lex("1e")
	if len(toks) != 3 || toks[0].Kind != TInt || toks[1].Kind != TIdent {
		t.Errorf("1e: got %+v", toks)
	}
}

// FormatExpr parenthesises every operator application, which makes
// precedence and associativity visible in the expected strings.
func TestExpressionPrecedence(t *testing.T) {
	cases := []struct{ src, want string }{
		{"1 + 2 * 3", "(1 + (2 * 3))"},
		{"(1 + 2) * 3", "((1 + 2) * 3)"},
		{"1 - 2 - 3", "((1 - 2) - 3)"},
		{"8 / 4 / 2", "((8 / 4) / 2)"},
		{"a or b and c", "(a or (b and c))"},
		{"not a = b", "(not (a = b))"},
		{"not a and b", "((not a) and b)"},
		{"a = 1 or b = 2 and c <> 3", "((a = 1) or ((b = 2) and (c <> 3)))"},
		{"a != 1", "(a <> 1)"},
		{"-a * b", "((- a) * b)"},
		{"-a::int", "(- (a::integer))"},
		{"a + 1 is null", "((a + 1) is null)"},
		{"a is not null and b", "((a is not null) and b)"},
		{"a || b || 'x' = c", "(((a || b) || 'x') = c)"},
		{"1 + 2 < 3 * 4", "((1 + 2) < (3 * 4))"},
		{"$1::bigint + $2", "(($1::bigint) + $2)"},
		{"t.id % 2", "(t.id % 2)"},
		{"upper(name || '!')", "upper((name || '!'))"},
		{"- 5", "(-5)"},
		{"-9223372036854775808", "(-9223372036854775808)"},
		{"+ 5", "5"},
		{"1 - -5", "(1 - (-5))"},
		{"-2147483648", "(-2147483648)"},
		{"'a''b'", "'a''b'"},
		{"x::double precision", "(x::double precision)"},
		{"cast(x as bigint) + 1", "((x::bigint) + 1)"},
		// IN, BETWEEN and LIKE bind tighter than comparison, looser than ||.
		{"a in (1, 2) and b", "((a in (1, 2)) and b)"},
		{"a not in (1, 2)", "(a not in (1, 2))"},
		{"a between 1 and 2 and b", "((a between 1 and 2) and b)"},
		{"a not between 1 + 1 and 2 * 3", "(a not between (1 + 1) and (2 * 3))"},
		{"a || 'x' like 'b%'", "((a || 'x') like 'b%')"},
		{"a not ilike b or c", "((a not ilike b) or c)"},
		{"not a between 1 and 2", "(not (a between 1 and 2))"},
		{"a = b in (true)", "(a = (b in (true)))"},
		{"case when a then 1 when b then 2 else 3 end", "case when a then 1 when b then 2 else 3 end"},
		{"case a when 1 then 'x' end || 'y'", "(case a when 1 then 'x' end || 'y')"},
		{"count(*) + count(distinct a)", "(count(*) + count(distinct a))"},
		{"sum(all a)", "sum(a)"},
		{"(select 1) + 1", "((select 1) + 1)"},
		{"exists (select 1) and a in (select b from t)", "(exists (select 1) and (a in (select b from t)))"},
		{`"Select" + "a b"`, `("Select" + "a b")`},
		{"1.0 + 5e0 + -1.5", "((1.0 + 5.0) + (-1.5))"},
	}
	for _, tc := range cases {
		stmts, err := Parse("select " + tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
			continue
		}
		got := FormatExpr(stmts[0].Node.(*Select).Items[0].Expr)
		if got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.src, got, tc.want)
		}
	}
}

func TestIntegerLiteralTypes(t *testing.T) {
	cases := map[string]Type{
		"2147483647": Int4, "2147483648": Int8, "-2147483648": Int4, "-2147483649": Int8, "1.0": Float8,
	}
	for src, want := range cases {
		stmts, err := Parse("select " + src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := stmts[0].Node.(*Select).Items[0].Expr.(*Literal).Type; got != want {
			t.Errorf("%s: got %s, want %s", src, got, want)
		}
	}
}

func TestParseStatements(t *testing.T) {
	stmts, err := Parse(`
		create table if not exists users (id int primary key, name text not null, score double precision);
		insert into users (id, name) values (1, 'ana'), ($1, $2);
		select u.id, name as n, 1 one from users as u where id = $1;
		update users set name = 'x', score = score + 1 where id = 2;
		delete from users where name is null;
		begin; commit work; rollback transaction; start transaction; end;
		set extra_float_digits = 3; set application_name to 'app'; set time zone 'UTC';
		show transaction isolation level;;
	`)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, s := range stmts {
		kinds = append(kinds, strings.TrimPrefix(fmt.Sprintf("%T", s.Node), "*sql."))
	}
	want := "CreateTable Insert Select Update Delete Begin Commit Rollback Begin Commit Set Set Set Show"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("statements:\n got %s\nwant %s", got, want)
	}

	ct := stmts[0].Node.(*CreateTable)
	if !ct.IfNotExists || len(ct.Cols) != 3 || !ct.Cols[0].PrimaryKey || !ct.Cols[1].NotNull || ct.Cols[2].Type != Float8 {
		t.Errorf("create table: %+v", ct)
	}
	ins := stmts[1].Node.(*Insert)
	if len(ins.Cols) != 2 || len(ins.Rows) != 2 || stmts[1].NumParams != 2 {
		t.Errorf("insert: %+v params=%d", ins, stmts[1].NumParams)
	}
	sel := stmts[2].Node.(*Select)
	from := sel.From.(*TableRef)
	if from.Name != "users" || from.Alias != "u" || sel.Items[1].Alias != "n" || sel.Items[2].Alias != "one" {
		t.Errorf("select: %+v", sel)
	}
	if stmts[2].NumParams != 1 || stmts[3].NumParams != 0 {
		t.Errorf("parameter counts are per statement: %d, %d", stmts[2].NumParams, stmts[3].NumParams)
	}
	if set := stmts[12].Node.(*Set); set.Name != "timezone" || set.Value != "UTC" {
		t.Errorf("set time zone: %+v", set)
	}
	if show := stmts[13].Node.(*Show); show.Name != "transaction_isolation" {
		t.Errorf("show: %+v", show)
	}
}

// corpus is SQL that must parse. Each entry is paired with its canonical
// form, which documents how the parser read it: join associativity, where
// clauses attach, what is optional noise.
var corpus = []struct{ src, canonical string }{
	// FROM and joins
	{"select * from a, b where a.x = b.x",
		"select * from (a cross join b) where (a.x = b.x)"},
	{"select a.*, b.y from a join b on a.x = b.x",
		"select a.*, b.y from (a inner join b on (a.x = b.x))"},
	{"select * from a inner join b on a.x = b.x left outer join c on b.y = c.y",
		"select * from ((a inner join b on (a.x = b.x)) left join c on (b.y = c.y))"},
	{"select * from a cross join b cross join c",
		"select * from ((a cross join b) cross join c)"},
	{"select * from a join (b join c on b.y = c.y) on a.x = b.x",
		"select * from (a inner join (b inner join c on (b.y = c.y)) on (a.x = b.x))"},
	{"select * from a x, a AS y",
		"select * from (a as x cross join a as y)"},
	{"select s.n from (select count(*) as n from t) s",
		"select s.n from (select count(*) as n from t) as s"},
	{"select * from (select 1) as \"Sub Query\"",
		"select * from (select 1) as \"Sub Query\""},

	// Grouping, ordering, limiting
	{"select distinct a, b from t",
		"select distinct a, b from t"},
	{"select all a from t",
		"select a from t"},
	{"select dept, count(*), avg(salary) from emp group by dept having count(*) > 1 order by 2 desc, dept",
		"select dept, count(*), avg(salary) from emp group by dept having (count(*) > 1) order by 2 desc, dept"},
	{"select a from t order by a asc nulls first, b desc nulls last, c nulls first",
		"select a from t order by a nulls first, b desc nulls last, c nulls first"},
	{"select a from t limit 10 offset 5",
		"select a from t limit 10 offset 5"},
	{"select a from t offset 5 rows limit all",
		"select a from t offset 5"},
	{"select a from t offset $1 limit $2",
		"select a from t limit $2 offset $1"},
	{"select a + 1 as b from t group by a + 1 order by b",
		"select (a + 1) as b from t group by (a + 1) order by b"},

	// Subqueries in expressions
	{"select (select max(x) from u where u.k = t.k) from t where exists (select 1 from u) and a not in (select b from u)",
		"select (select max(x) from u where (u.k = t.k)) from t where (exists (select 1 from u) and (a not in (select b from u)))"},

	// DDL
	{"create table t (a int default 0 not null, b text default 'x' || 'y' unique, c real, primary key (a, b), unique (c))",
		"create table t (a integer default 0 not null, b text default ('x' || 'y') unique, c double precision, primary key (a, b), unique (c))"},
	{"create table \"Order\" (\"select\" varchar(10) null)",
		"create table \"Order\" (\"select\" text)"},
	{"create unique index if not exists i on t (a desc, b)",
		"create unique index if not exists i on t (a, b)"},
	{"create index i on t (a)", "create index i on t (a)"},
	{"drop index if exists i", "drop index if exists i"},
	{"drop table if exists t", "drop table if exists t"},

	// DML
	{"insert into t values (default, 1), (2, default)",
		"insert into t values (default, 1), (2, default)"},
	{"insert into t (a, b) select x, y from u where x > 0",
		"insert into t (a, b) select x, y from u where (x > 0)"},
	{"insert into t select * from u",
		"insert into t select * from u"},
	{"update t as x set a = default_value, b = b + 1 where x.a in (1, 2)",
		"update t as x set a = default_value, b = (b + 1) where (x.a in (1, 2))"},
	{"update t set a = default, b = 1", "update t set a = default, b = 1"},
	{"delete from t where a between 1 and 10 or b like 'x%'",
		"delete from t where ((a between 1 and 10) or (b like 'x%'))"},

	// Transactions and settings
	{"begin isolation level serializable", "begin isolation level serializable"},
	{"start transaction isolation level repeatable read, read only", "begin isolation level repeatable read"},
	{"begin transaction read write", "begin"},
	{"set transaction isolation level read committed", "set transaction_isolation = 'read committed'"},
	{"set session characteristics as transaction isolation level read uncommitted",
		"set default_transaction_isolation = 'read uncommitted'"},
	{"set search_path to public, extra", "set search_path = 'public, extra'"},
	{"show time zone", "show timezone"},
	{"CHECKPOINT", "checkpoint"},
	{"VACUUM", "vacuum"},
	{"vacuum \"My Table\"", "vacuum \"My Table\""},
}

func TestCorpus(t *testing.T) {
	for _, tc := range corpus {
		stmts, err := Parse(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
			continue
		}
		if got := Format(stmts[0].Node); got != tc.canonical {
			t.Errorf("%s:\n got %s\nwant %s", tc.src, got, tc.canonical)
		}
	}
}

// roundTrip checks that the canonical form of src is a fixed point: parsing
// it gives a tree that formats to the very same text. Together with
// TestCorpus, which pins what the canonical form is, this shows that the
// printer loses nothing the parser produced.
func roundTrip(t *testing.T, src string) {
	t.Helper()
	stmts, err := Parse(src)
	if err != nil {
		return // not valid SQL; nothing to check
	}
	for _, st := range stmts {
		first := Format(st.Node)
		again, err := Parse(first)
		if err != nil {
			t.Fatalf("canonical form does not parse\n   input: %q\n printed: %s\n   error: %v", src, first, err)
		}
		if len(again) != 1 {
			t.Fatalf("canonical form parsed into %d statements\n   input: %q\n printed: %s", len(again), src, first)
		}
		if second := Format(again[0].Node); second != first {
			t.Fatalf("canonical form is not stable\n  input: %q\n  first: %s\n second: %s", src, first, second)
		}
		if again[0].NumParams != st.NumParams {
			t.Fatalf("parameter count changed from %d to %d\n input: %q", st.NumParams, again[0].NumParams, src)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, tc := range corpus {
		roundTrip(t, tc.src)
	}
}

// FuzzParse feeds the parser arbitrary input. It must never panic or hang,
// and whatever it accepts must survive the round trip. Run it with
//
//	go test ./internal/sql -fuzz FuzzParse -fuzztime 1m
//
// Without -fuzz it replays the seeds and any saved crashers as a unit test.
func FuzzParse(f *testing.F) {
	for _, tc := range corpus {
		f.Add(tc.src)
	}
	for _, seed := range []string{
		"select 1; select 2", "select $1 + $2::int", "select -(-(-1))", "select 'a''b' || \"c\"\"d\"",
		"select 1e309", "select ((((1))))", "select case when 1 then 2 end from t t2",
		"select * from ((a join b on true) cross join c)", "/* c */ select -- x\n 1",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		roundTrip(t, src)
	})
}

func TestDeepNestingIsRejected(t *testing.T) {
	for _, src := range []string{
		"select " + strings.Repeat("(", 100000) + "1" + strings.Repeat(")", 100000),
		"select " + strings.Repeat("- ", 100000) + "a",
		"select " + strings.Repeat("not ", 100000) + "a",
		"select * from " + strings.Repeat("(", 100000) + "t" + strings.Repeat(")", 100000),
		"select " + strings.Repeat("(select ", 100000) + "1" + strings.Repeat(")", 100000),
	} {
		_, err := Parse(src)
		var pe *pgerr.Error
		if !errors.As(err, &pe) || pe.Code != pgerr.StatementTooComplex {
			t.Errorf("expected statement_too_complex, got %v", err)
		}
	}
	// A long flat expression is not nesting and must be fine.
	if _, err := Parse("select 1" + strings.Repeat(" + 1", 100000)); err != nil {
		t.Errorf("flat expression: %v", err)
	}
}

func TestParseEmpty(t *testing.T) {
	for _, src := range []string{"", "  ", ";", " ; ;; ", "-- just a comment", "/* x */ ;"} {
		stmts, err := Parse(src)
		if err != nil || len(stmts) != 0 {
			t.Errorf("%q: got %d statements, err %v", src, len(stmts), err)
		}
	}
}

func TestSyntaxErrors(t *testing.T) {
	cases := []struct {
		src  string
		msg  string
		pos  int
		code string
	}{
		{"selec 1", `syntax error at or near "selec"`, 1, pgerr.SyntaxError},
		{"select 1 +", "syntax error at end of input", 11, pgerr.SyntaxError},
		{"select (1", "syntax error at end of input", 10, pgerr.SyntaxError},
		{"select 1 2", `syntax error at or near "2"`, 10, pgerr.SyntaxError},
		{"select * from", "syntax error at end of input", 14, pgerr.SyntaxError},
		{"select 'abc", "unterminated quoted string", 8, pgerr.SyntaxError},
		{"select 1 /* oops", "unterminated /* comment", 10, pgerr.SyntaxError},
		{"select 1 ? 2", `syntax error at or near "?"`, 10, pgerr.SyntaxError},
		{"create table t (a blob)", `type "blob" does not exist`, 19, pgerr.UndefinedObject},
		{"insert into t values", "syntax error at end of input", 21, pgerr.SyntaxError},
		{"select 99999999999999999999", "integer literal 99999999999999999999 is out of range for type bigint", 8, pgerr.NumericValueOutOfRange},
		// Positions count characters, not bytes: "é" is two bytes in UTF-8.
		{"select 'é' from from", `syntax error at or near "from"`, 17, pgerr.SyntaxError},
		{"select a from t join u", "syntax error at end of input", 23, pgerr.SyntaxError},
		{"select a from t group a", `syntax error at or near "a"`, 23, pgerr.SyntaxError},
		{"select a between 1 or 2", `syntax error at or near "or"`, 20, pgerr.SyntaxError},
		{"select case end", `syntax error at or near "end"`, 13, pgerr.SyntaxError},
		{"select * from (select 1)", "subquery in FROM must have an alias", 15, pgerr.SyntaxError},
		{"select a from t limit 1 limit 2", `syntax error at or near "limit"`, 25, pgerr.SyntaxError},
		{"select 1 union select 2", "UNION is not supported", 10, pgerr.FeatureNotSupported},
		{"select * from a right join b on true", "RIGHT JOIN is not supported", 17, pgerr.FeatureNotSupported},
		{"select * from a join b using (x)", "JOIN ... USING is not supported", 24, pgerr.FeatureNotSupported},
		{"begin isolation level chaos", `syntax error at or near "chaos"`, 23, pgerr.SyntaxError},
	}
	for _, tc := range cases {
		_, err := Parse(tc.src)
		var pe *pgerr.Error
		if !errors.As(err, &pe) {
			t.Errorf("%s: expected a pgerr.Error, got %v", tc.src, err)
			continue
		}
		if pe.Message != tc.msg || pe.Position != tc.pos || pe.Code != tc.code {
			t.Errorf("%s:\n got %q at %d (%s)\nwant %q at %d (%s)", tc.src, pe.Message, pe.Position, pe.Code, tc.msg, tc.pos, tc.code)
		}
	}
}

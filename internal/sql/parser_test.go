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

// format renders an expression fully parenthesised, which makes precedence
// and associativity visible.
func format(e Expr) string {
	switch e := e.(type) {
	case *Literal:
		switch v := e.Val.(type) {
		case nil:
			return "null"
		case string:
			return "'" + v + "'"
		default:
			return fmt.Sprint(v)
		}
	case *Param:
		return fmt.Sprintf("$%d", e.Index)
	case *ColumnRef:
		if e.Table != "" {
			return e.Table + "." + e.Name
		}
		return e.Name
	case *Unary:
		return fmt.Sprintf("(%s %s)", e.Op, format(e.X))
	case *Binary:
		return fmt.Sprintf("(%s %s %s)", format(e.L), e.Op, format(e.R))
	case *IsNull:
		if e.Not {
			return fmt.Sprintf("(%s is not null)", format(e.X))
		}
		return fmt.Sprintf("(%s is null)", format(e.X))
	case *Cast:
		return fmt.Sprintf("(%s::%s)", format(e.X), e.To)
	case *FuncCall:
		args := make([]string, len(e.Args))
		for i, a := range e.Args {
			args[i] = format(a)
		}
		return e.Name + "(" + strings.Join(args, ", ") + ")"
	}
	return "?"
}

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
		{"- 5", "-5"},
		{"1 - -5", "(1 - -5)"},
		{"-2147483648", "-2147483648"},
		{"'a''b'", "'a'b'"},
		{"x::double precision", "(x::double precision)"},
	}
	for _, tc := range cases {
		stmts, err := Parse("select " + tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
			continue
		}
		got := format(stmts[0].Node.(*Select).Items[0].Expr)
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
	if sel.From.Name != "users" || sel.From.Alias != "u" || sel.Items[1].Alias != "n" || sel.Items[2].Alias != "one" {
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

package sql

import (
	"strconv"
	"strings"
)

// Format renders a statement back to SQL.
//
// The output is canonical rather than pretty: keywords in lower case, every
// operator application parenthesised, identifiers quoted only when needed.
// Parsing the output yields the same tree, which is the property the
// round-trip tests and the fuzzer check, and which makes the text usable as
// a key for comparing expressions (GROUP BY matching relies on it).
func Format(node Node) string {
	var f formatter
	f.stmt(node)
	return f.sb.String()
}

// FormatExpr renders a single expression.
func FormatExpr(e Expr) string {
	var f formatter
	f.expr(e)
	return f.sb.String()
}

type formatter struct {
	sb strings.Builder
}

func (f *formatter) w(parts ...string) {
	for _, p := range parts {
		f.sb.WriteString(p)
	}
}

// QuoteIdent returns name as it must be written to be read back as the same
// identifier: bare if it is a plain lower-case word that is not reserved,
// double-quoted otherwise.
func QuoteIdent(name string) string {
	plain := name != "" && !reserved[name]
	for i := 0; plain && i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_' || (c >= 'a' && c <= 'z'):
		case i > 0 && (isDigit(c) || c == '$'):
		default:
			plain = false
		}
	}
	if plain {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (f *formatter) idents(ids []Ident) {
	f.w("(")
	for i, id := range ids {
		if i > 0 {
			f.w(", ")
		}
		f.w(QuoteIdent(id.Name))
	}
	f.w(")")
}

func (f *formatter) tableRef(t TableRef) {
	f.w(QuoteIdent(t.Name))
	if t.Alias != "" && t.Alias != t.Name {
		f.w(" as ", QuoteIdent(t.Alias))
	}
}

func (f *formatter) stmt(node Node) {
	switch n := node.(type) {
	case *Select:
		f.selectStmt(n)
	case *Insert:
		f.w("insert into ", QuoteIdent(n.Table.Name))
		if n.Cols != nil {
			f.w(" ")
			f.idents(n.Cols)
		}
		if n.Select != nil {
			f.w(" ")
			f.selectStmt(n.Select)
			return
		}
		f.w(" values ")
		for i, row := range n.Rows {
			if i > 0 {
				f.w(", ")
			}
			f.w("(")
			f.exprs(row)
			f.w(")")
		}
	case *Update:
		f.w("update ")
		f.tableRef(n.Table)
		f.w(" set ")
		for i, a := range n.Sets {
			if i > 0 {
				f.w(", ")
			}
			f.w(QuoteIdent(a.Col.Name), " = ")
			f.expr(a.Value)
		}
		f.where(n.Where)
	case *Delete:
		f.w("delete from ")
		f.tableRef(n.Table)
		f.where(n.Where)
	case *CreateTable:
		f.w("create table ")
		if n.IfNotExists {
			f.w("if not exists ")
		}
		f.w(QuoteIdent(n.Name.Name), " (")
		for i, c := range n.Cols {
			if i > 0 {
				f.w(", ")
			}
			f.w(QuoteIdent(c.Name.Name), " ", c.Type.String())
			if c.Default != nil {
				f.w(" default ")
				f.expr(c.Default)
			}
			if c.NotNull {
				f.w(" not null")
			}
			if c.PrimaryKey {
				f.w(" primary key")
			}
			if c.Unique {
				f.w(" unique")
			}
		}
		for i, tc := range n.Constraints {
			// A table may consist of constraints only, as far as the
			// grammar is concerned, so the separator cannot be assumed.
			if i > 0 || len(n.Cols) > 0 {
				f.w(", ")
			}
			if tc.PrimaryKey {
				f.w("primary key ")
			} else {
				f.w("unique ")
			}
			f.idents(tc.Cols)
		}
		f.w(")")
	case *DropTable:
		f.w("drop table ")
		if n.IfExists {
			f.w("if exists ")
		}
		f.w(QuoteIdent(n.Name.Name))
	case *CreateIndex:
		f.w("create ")
		if n.Unique {
			f.w("unique ")
		}
		f.w("index ")
		if n.IfNotExists {
			f.w("if not exists ")
		}
		f.w(QuoteIdent(n.Name.Name), " on ", QuoteIdent(n.Table.Name), " ")
		f.idents(n.Cols)
	case *DropIndex:
		f.w("drop index ")
		if n.IfExists {
			f.w("if exists ")
		}
		f.w(QuoteIdent(n.Name.Name))
	case *Begin:
		f.w("begin")
		if n.Isolation != "" {
			f.w(" isolation level ", n.Isolation)
		}
	case *Commit:
		f.w("commit")
	case *Explain:
		f.w("explain ")
		var opts []string
		if n.Analyze {
			opts = append(opts, "analyze")
		}
		if n.NoCosts {
			opts = append(opts, "costs off")
		}
		if n.NoTiming {
			opts = append(opts, "timing off")
		}
		if len(opts) > 0 {
			f.w("(", strings.Join(opts, ", "), ") ")
		}
		f.stmt(n.Stmt)
	case *Analyze:
		f.w("analyze")
		if n.Table != "" {
			f.w(" ", QuoteIdent(n.Table))
		}
	case *Vacuum:
		f.w("vacuum")
		if n.Table != "" {
			f.w(" ", QuoteIdent(n.Table))
		}
	case *Checkpoint:
		f.w("checkpoint")
	case *Rollback:
		f.w("rollback")
	case *Set:
		f.w("set ", n.Name, " = ", quoteString(n.Value))
	case *Show:
		f.w("show ", n.Name)
	}
}

func (f *formatter) where(e Expr) {
	if e != nil {
		f.w(" where ")
		f.expr(e)
	}
}

func (f *formatter) selectStmt(s *Select) {
	f.selectBody(s)
	for i, o := range s.OrderBy {
		if i == 0 {
			f.w(" order by ")
		} else {
			f.w(", ")
		}
		f.expr(o.Expr)
		if o.Desc {
			f.w(" desc")
		}
		if o.NullsFirst != nil {
			if *o.NullsFirst {
				f.w(" nulls first")
			} else {
				f.w(" nulls last")
			}
		}
	}
	if s.Limit != nil {
		f.w(" limit ")
		f.expr(s.Limit)
	}
	if s.Offset != nil {
		f.w(" offset ")
		f.expr(s.Offset)
	}
}

// setOperand prints one side of a set operation. Anything but a plain
// SELECT goes in parentheses, which makes the printed form independent of
// precedence and keeps an operand's own ORDER BY and LIMIT with it.
func (f *formatter) setOperand(s *Select) {
	if s.Op == "" && len(s.OrderBy) == 0 && s.Limit == nil && s.Offset == nil {
		f.selectStmt(s)
		return
	}
	f.w("(")
	f.selectStmt(s)
	f.w(")")
}

// selectBody prints a query without its ORDER BY, LIMIT and OFFSET.
func (f *formatter) selectBody(s *Select) {
	if s.Op != "" {
		f.setOperand(s.Left)
		f.w(" ", s.Op, " ")
		if s.All {
			f.w("all ")
		}
		f.setOperand(s.Right)
		return
	}
	f.w("select ")
	if s.Distinct {
		f.w("distinct ")
	}
	for i, item := range s.Items {
		if i > 0 {
			f.w(", ")
		}
		switch {
		case item.Star && item.Table != "":
			f.w(QuoteIdent(item.Table), ".*")
		case item.Star:
			f.w("*")
		default:
			f.expr(item.Expr)
			if item.Alias != "" {
				f.w(" as ", QuoteIdent(item.Alias))
			}
		}
	}
	if s.From != nil {
		f.w(" from ")
		f.tableExpr(s.From)
	}
	f.where(s.Where)
	if len(s.GroupBy) > 0 {
		f.w(" group by ")
		f.exprs(s.GroupBy)
	}
	if s.Having != nil {
		f.w(" having ")
		f.expr(s.Having)
	}
}

func (f *formatter) tableExpr(t TableExpr) {
	switch t := t.(type) {
	case *TableRef:
		f.tableRef(*t)
	case *DerivedTable:
		f.w("(")
		f.selectStmt(t.Select)
		f.w(") as ", QuoteIdent(t.Alias))
	case *Join:
		f.w("(")
		f.tableExpr(t.Left)
		f.w(" ", t.Kind.String(), " ")
		f.tableExpr(t.Right)
		if t.On != nil {
			f.w(" on ")
			f.expr(t.On)
		}
		if t.Using != nil {
			f.w(" using ")
			f.idents(t.Using)
		}
		f.w(")")
	}
}

func (f *formatter) exprs(list []Expr) {
	for i, e := range list {
		if i > 0 {
			f.w(", ")
		}
		f.expr(e)
	}
}

func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (f *formatter) expr(e Expr) {
	switch e := e.(type) {
	case *Literal:
		switch v := e.Val.(type) {
		case nil:
			f.w("null")
		case bool:
			f.w(strconv.FormatBool(v))
		case int64:
			// A negative literal is parenthesised: "-5::int" would read
			// back as the negation of a cast rather than a cast of -5.
			if v < 0 {
				f.w("(", strconv.FormatInt(v, 10), ")")
			} else {
				f.w(strconv.FormatInt(v, 10))
			}
		case float64:
			s := strconv.FormatFloat(v, 'g', -1, 64)
			// Keep it a floating-point token when read back.
			if !strings.ContainsAny(s, ".e") {
				s += ".0"
			}
			if v < 0 || (v == 0 && strings.HasPrefix(s, "-")) {
				s = "(" + s + ")"
			}
			f.w(s)
		case string:
			f.w(quoteString(v))
		}
	case *Param:
		f.w("$", strconv.Itoa(e.Index))
	case *ColumnRef:
		if e.Table != "" {
			f.w(QuoteIdent(e.Table), ".")
		}
		f.w(QuoteIdent(e.Name))
	case *Unary:
		f.w("(", e.Op, " ")
		f.expr(e.X)
		f.w(")")
	case *Binary:
		f.w("(")
		f.expr(e.L)
		f.w(" ", e.Op, " ")
		f.expr(e.R)
		f.w(")")
	case *IsNull:
		f.w("(")
		f.expr(e.X)
		if e.Not {
			f.w(" is not null)")
		} else {
			f.w(" is null)")
		}
	case *Cast:
		f.w("(")
		f.expr(e.X)
		f.w("::", e.To.String(), ")")
	case *FuncCall:
		f.w(e.Name, "(")
		switch {
		case e.Star:
			f.w("*")
		case e.Distinct:
			f.w("distinct ")
			f.exprs(e.Args)
		default:
			f.exprs(e.Args)
		}
		f.w(")")
	case *Case:
		f.w("case")
		if e.Operand != nil {
			f.w(" ")
			f.expr(e.Operand)
		}
		for _, w := range e.Whens {
			f.w(" when ")
			f.expr(w.Cond)
			f.w(" then ")
			f.expr(w.Then)
		}
		if e.Else != nil {
			f.w(" else ")
			f.expr(e.Else)
		}
		f.w(" end")
	case *In:
		f.w("(")
		f.expr(e.X)
		if e.Not {
			f.w(" not")
		}
		f.w(" in (")
		if e.Sub != nil {
			f.selectStmt(e.Sub)
		} else {
			f.exprs(e.List)
		}
		f.w("))")
	case *Between:
		f.w("(")
		f.expr(e.X)
		if e.Not {
			f.w(" not")
		}
		f.w(" between ")
		f.expr(e.Lo)
		f.w(" and ")
		f.expr(e.Hi)
		f.w(")")
	case *Like:
		f.w("(")
		f.expr(e.X)
		if e.Not {
			f.w(" not")
		}
		if e.ILike {
			f.w(" ilike ")
		} else {
			f.w(" like ")
		}
		f.expr(e.Pattern)
		f.w(")")
	case *SubqueryExpr:
		f.w("(")
		f.selectStmt(e.Select)
		f.w(")")
	case *Exists:
		f.w("exists (")
		f.selectStmt(e.Select)
		f.w(")")
	case *DefaultValue:
		f.w("default")
	}
}

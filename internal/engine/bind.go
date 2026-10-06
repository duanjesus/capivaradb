package engine

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
	"github.com/duanjesus/capivaradb/internal/version"
)

// Runtime values are represented by plain Go types:
//
//	NULL              nil
//	boolean           bool
//	integer, bigint   int64
//	double precision  float64
//	text              string

// env is what an expression is evaluated against.
type env struct {
	ctx    context.Context
	params []any
	// row is the current row of the query level this env belongs to.
	row []any
	// aggs holds the aggregate results of the current group.
	aggs []any
	// outer is the env of the enclosing query, for correlated subqueries.
	outer *env
	// locked is set when the goroutine already holds the database lock,
	// so that scans inside a writing statement do not try to take it again.
	locked bool
	// q is the execution the env belongs to.
	q *query
}

type evalFn func(*env) (any, error)

// bound is an expression after name resolution and type checking, compiled
// to a closure so that execution does not walk the AST again.
type bound struct {
	typ  sql.Type
	eval evalFn
}

// scopeCol is a column visible to expressions: a table column, or a column
// of a subquery or join result.
type scopeCol struct {
	table string // the alias it can be qualified with
	name  string
	typ   sql.Type
	// hidden is set on the copy of a JOIN ... USING column that the join
	// does not show: it is found by its qualified name only.
	hidden bool
}

// scope is the set of columns of one query level. parent is the enclosing
// query's scope, which a subquery may refer to.
type scope struct {
	cols   []scopeCol
	parent *scope
}

// find looks the column up in this level only. It returns -1 if there is no
// such column and an error if the reference is ambiguous.
func (sc *scope) find(ref *sql.ColumnRef) (int, error) {
	found := -1
	for i, c := range sc.cols {
		if c.name != ref.Name || (ref.Table != "" && c.table != ref.Table) || (ref.Table == "" && c.hidden) {
			continue
		}
		if found >= 0 {
			return -1, pgerr.New(pgerr.AmbiguousColumn, "column reference %q is ambiguous", ref.Name).At(ref.Pos)
		}
		found = i
	}
	return found, nil
}

func (sc *scope) hasTable(name string) bool {
	for ; sc != nil; sc = sc.parent {
		for _, c := range sc.cols {
			if c.table == name {
				return true
			}
		}
	}
	return false
}

// colIdx is a reference to a scope column by position. The planner uses it
// for the columns that "*" expands to, which cannot always be named
// unambiguously (two joined tables may both have a column "id").
type colIdx struct {
	idx int
}

func (*colIdx) Position() int { return 0 }

// binder resolves names and types for the expressions of one query level.
type binder struct {
	sess  *Session
	scope *scope
	// ptypes holds the type of each parameter, shared by every binder of
	// the statement. Entries that are still Unknown get filled in from
	// context as expressions are bound.
	ptypes []sql.Type

	// aggs collects the aggregate calls found; nil where aggregates are
	// not allowed (WHERE, ON, VALUES ...).
	aggs *[]aggSpec
	// grouped is set while binding expressions that are evaluated once per
	// group: there, a column may only appear inside an aggregate or as
	// (part of) a GROUP BY expression, listed in groupKeys.
	grouped   bool
	groupKeys map[string]bool
	inAgg     bool
}

func null(*env) (any, error) { return nil, nil }

func constant(typ sql.Type, v any) bound {
	return bound{typ, func(*env) (any, error) { return v, nil }}
}

// keyOf identifies an expression for GROUP BY matching: columns by their
// position, so that "a" and "t.a" are the same thing, anything else by its
// canonical text.
func (b *binder) keyOf(e sql.Expr) string {
	switch e := e.(type) {
	case *colIdx:
		return fmt.Sprintf("#%d", e.idx)
	case *sql.ColumnRef:
		if i, err := b.scope.find(e); err == nil && i >= 0 {
			return fmt.Sprintf("#%d", i)
		}
	}
	return sql.FormatExpr(e)
}

// bind compiles e. hint is the type the surrounding context expects; it is
// only used to give a type to things that have none of their own: untyped
// parameters, NULL and quoted literals.
func (b *binder) bind(e sql.Expr, hint sql.Type) (bound, error) {
	// An expression that is itself a GROUP BY key is constant within a
	// group, so the columns inside it need no further justification.
	if b.grouped && !b.inAgg && b.groupKeys[b.keyOf(e)] {
		b.grouped = false
		defer func() { b.grouped = true }()
	}

	switch e := e.(type) {
	case *sql.Literal:
		v, t := e.Val, e.Type
		if t == sql.Unknown {
			t = hint
		}
		// A quoted literal is not really text yet: like PostgreSQL's
		// "unknown" type, it takes the type the context asks for, so
		// that id = '42' compares integers. Drivers depend on this when
		// they inline parameters as quoted strings in the simple protocol.
		if s, ok := v.(string); ok && hint != sql.Unknown && hint != sql.Text {
			conv, err := castFn(sql.Text, hint)
			if err != nil {
				return bound{}, err.(*pgerr.Error).At(e.Pos)
			}
			cv, err := conv(s)
			if err != nil {
				return bound{}, pgerr.From(err).At(e.Pos)
			}
			v, t = cv, hint
		}
		return constant(t, v), nil

	case *sql.Param:
		i := e.Index - 1
		if b.ptypes[i] == sql.Unknown {
			b.ptypes[i] = hint
		}
		return bound{b.ptypes[i], func(en *env) (any, error) { return en.params[i], nil }}, nil

	case *sql.ColumnRef:
		return b.bindColumn(e)

	case *colIdx:
		if err := b.checkGrouped(b.scope.cols[e.idx].name, 0); err != nil {
			return bound{}, err
		}
		i := e.idx
		return bound{b.scope.cols[i].typ, func(en *env) (any, error) { return en.row[i], nil }}, nil

	case *sql.Unary:
		return b.bindUnary(e, hint)

	case *sql.Binary:
		return b.bindBinary(e)

	case *sql.IsNull:
		x, err := b.bind(e.X, sql.Unknown)
		if err != nil {
			return bound{}, err
		}
		not := e.Not
		return bound{sql.Bool, func(en *env) (any, error) {
			v, err := x.eval(en)
			if err != nil {
				return nil, err
			}
			return (v == nil) != not, nil
		}}, nil

	case *sql.Cast:
		x, err := b.bind(e.X, e.To)
		if err != nil {
			return bound{}, err
		}
		conv, err := castFn(x.typ, e.To)
		if err != nil {
			return bound{}, err.(*pgerr.Error).At(e.Pos)
		}
		return bound{e.To, func(en *env) (any, error) {
			v, err := x.eval(en)
			if err != nil || v == nil {
				return nil, err
			}
			return conv(v)
		}}, nil

	case *sql.FuncCall:
		return b.bindFunc(e, hint)

	case *sql.Case:
		return b.bindCase(e, hint)

	case *sql.In:
		return b.bindIn(e)

	case *sql.Between:
		// x BETWEEN lo AND hi is exactly x >= lo AND x <= hi, including
		// how NULLs behave, so it is bound as that.
		var rewritten sql.Expr = &sql.Binary{Op: "and", Pos: e.Pos,
			L: &sql.Binary{Op: ">=", L: e.X, R: e.Lo, Pos: e.Pos},
			R: &sql.Binary{Op: "<=", L: e.X, R: e.Hi, Pos: e.Pos}}
		if e.Not {
			rewritten = &sql.Unary{Op: "not", X: rewritten, Pos: e.Pos}
		}
		return b.bind(rewritten, sql.Bool)

	case *sql.Like:
		return b.bindLike(e)

	case *sql.SubqueryExpr:
		plan, err := b.subquery(e.Select, e.Pos)
		if err != nil {
			return bound{}, err
		}
		return bound{plan.types[0], func(en *env) (any, error) {
			it, err := plan.open(en)
			if err != nil {
				return nil, err
			}
			defer it.close()
			row, err := it.next()
			if err != nil || row == nil {
				return nil, err
			}
			// One row is the answer; a second one is an error, and there
			// is no need to look further than that.
			if more, err := it.next(); err != nil {
				return nil, err
			} else if more != nil {
				return nil, pgerr.New(pgerr.CardinalityViolation,
					"more than one row returned by a subquery used as an expression")
			}
			return row[0], nil
		}}, nil

	case *sql.Exists:
		plan, err := b.sess.planSelect(e.Select, b.scope, b.ptypes)
		if err != nil {
			return bound{}, err
		}
		return bound{sql.Bool, func(en *env) (any, error) {
			// EXISTS is settled by the first row; the subquery is not run
			// any further.
			it, err := plan.open(en)
			if err != nil {
				return nil, err
			}
			defer it.close()
			row, err := it.next()
			return row != nil, err
		}}, nil

	case *sql.DefaultValue:
		return bound{}, pgerr.New(pgerr.SyntaxError, "DEFAULT is not allowed in this context").At(e.Pos)
	}
	return bound{}, pgerr.New(pgerr.InternalError, "unhandled expression node %T", e)
}

// subquery plans a subquery that is used as a value and must therefore
// have exactly one column.
func (b *binder) subquery(sel *sql.Select, pos int) (*selectPlan, error) {
	plan, err := b.sess.planSelect(sel, b.scope, b.ptypes)
	if err != nil {
		return nil, err
	}
	if len(plan.cols) != 1 {
		return nil, pgerr.New(pgerr.SyntaxError, "subquery must return only one column").At(pos)
	}
	return plan, nil
}

// checkGrouped rejects a bare column in an expression that is evaluated per
// group. Columns of an enclosing query (depth > 0) are constants here.
func (b *binder) checkGrouped(name string, depth int) error {
	if depth == 0 && b.grouped && !b.inAgg {
		return pgerr.New(pgerr.GroupingError,
			"column %q must appear in the GROUP BY clause or be used in an aggregate function", name)
	}
	return nil
}

func (b *binder) bindColumn(e *sql.ColumnRef) (bound, error) {
	depth := 0
	for sc := b.scope; sc != nil; sc, depth = sc.parent, depth+1 {
		i, err := sc.find(e)
		if err != nil {
			return bound{}, err
		}
		if i < 0 {
			continue
		}
		if err := b.checkGrouped(e.Name, depth); err != nil {
			return bound{}, err.(*pgerr.Error).At(e.Pos)
		}
		d := depth
		return bound{sc.cols[i].typ, func(en *env) (any, error) {
			for n := 0; n < d; n++ {
				en = en.outer
			}
			return en.row[i], nil
		}}, nil
	}
	if e.Table != "" {
		if !b.scope.hasTable(e.Table) {
			return bound{}, pgerr.New(pgerr.UndefinedTable, "missing FROM-clause entry for table %q", e.Table).At(e.Pos)
		}
		return bound{}, pgerr.New(pgerr.UndefinedColumn, "column %s.%s does not exist", e.Table, e.Name).At(e.Pos)
	}
	// SQL has a few "functions" that are written without parentheses.
	switch e.Name {
	case "current_user", "session_user":
		return constant(sql.Text, b.sess.user), nil
	case "current_schema":
		return constant(sql.Text, "public"), nil
	}
	return bound{}, pgerr.New(pgerr.UndefinedColumn, "column %q does not exist", e.Name).At(e.Pos)
}

func (b *binder) bindUnary(e *sql.Unary, hint sql.Type) (bound, error) {
	if e.Op == "not" {
		x, err := b.bind(e.X, sql.Bool)
		if err != nil {
			return bound{}, err
		}
		if x.typ != sql.Bool && x.typ != sql.Unknown {
			return bound{}, pgerr.New(pgerr.DatatypeMismatch,
				"argument of NOT must be type boolean, not type %s", x.typ).At(e.X.Position())
		}
		return bound{sql.Bool, func(en *env) (any, error) {
			v, err := x.eval(en)
			if err != nil || v == nil {
				return nil, err
			}
			return !v.(bool), nil
		}}, nil
	}
	if !hint.IsNumeric() {
		hint = sql.Unknown
	}
	x, err := b.bind(e.X, hint)
	if err != nil {
		return bound{}, err
	}
	if x.typ == sql.Unknown {
		return bound{sql.Unknown, null}, nil
	}
	if !x.typ.IsNumeric() {
		return bound{}, pgerr.New(pgerr.UndefinedFunction, "operator does not exist: - %s", x.typ).At(e.Pos)
	}
	typ := x.typ
	return bound{typ, func(en *env) (any, error) {
		v, err := x.eval(en)
		if err != nil || v == nil {
			return nil, err
		}
		if f, ok := v.(float64); ok {
			return -f, nil
		}
		return checkIntRange(-v.(int64), v.(int64) == math.MinInt64, typ)
	}}, nil
}

func (b *binder) bindBinary(e *sql.Binary) (bound, error) {
	if e.Op == "and" || e.Op == "or" {
		return b.bindLogic(e)
	}

	// Bind the left side first and let its type flow to the right; if only
	// the right side turned out to have a type, flow it back. This is what
	// types the parameter in both "id = $1" and "$1 = id".
	//
	// For "||" the context is always text: 1 || 'a' must not try to read
	// 'a' as a number.
	first := sql.Unknown
	if e.Op == "||" {
		first = sql.Text
	}
	l, err := b.bind(e.L, first)
	if err != nil {
		return bound{}, err
	}
	rhint := l.typ
	if e.Op == "||" {
		rhint = sql.Text
	}
	r, err := b.bind(e.R, rhint)
	if err != nil {
		return bound{}, err
	}
	if (l.typ == sql.Unknown || isStringLiteral(e.L)) && r.typ != sql.Unknown && e.Op != "||" {
		if l, err = b.bind(e.L, r.typ); err != nil {
			return bound{}, err
		}
	}
	noOperator := func() error {
		return pgerr.New(pgerr.UndefinedFunction, "operator does not exist: %s %s %s", l.typ, e.Op, r.typ).At(e.Pos)
	}

	switch e.Op {
	case "=", "<>", "<", "<=", ">", ">=":
		cmp := comparator(l.typ, r.typ)
		if cmp == nil {
			return bound{}, noOperator()
		}
		test := compareTest(e.Op)
		return bound{sql.Bool, func(en *env) (any, error) {
			lv, err := l.eval(en)
			if err != nil || lv == nil {
				return nil, err
			}
			rv, err := r.eval(en)
			if err != nil || rv == nil {
				return nil, err
			}
			return test(cmp(lv, rv)), nil
		}}, nil

	case "||":
		if l.typ != sql.Text && r.typ != sql.Text && l.typ != sql.Unknown && r.typ != sql.Unknown {
			return bound{}, noOperator()
		}
		return bound{sql.Text, func(en *env) (any, error) {
			lv, err := l.eval(en)
			if err != nil || lv == nil {
				return nil, err
			}
			rv, err := r.eval(en)
			if err != nil || rv == nil {
				return nil, err
			}
			return pgwire.TextValue(lv) + pgwire.TextValue(rv), nil
		}}, nil
	}

	// Arithmetic.
	if l.typ == sql.Unknown || r.typ == sql.Unknown {
		// Only reachable with an untyped NULL operand: the result is NULL.
		return bound{sql.Unknown, null}, nil
	}
	if !l.typ.IsNumeric() || !r.typ.IsNumeric() {
		return bound{}, noOperator()
	}
	typ := promote(l.typ, r.typ)
	if e.Op == "%" && typ == sql.Float8 {
		return bound{}, noOperator()
	}
	op := e.Op
	return bound{typ, func(en *env) (any, error) {
		lv, err := l.eval(en)
		if err != nil || lv == nil {
			return nil, err
		}
		rv, err := r.eval(en)
		if err != nil || rv == nil {
			return nil, err
		}
		if typ == sql.Float8 {
			return floatArith(op, toFloat(lv), toFloat(rv))
		}
		return intArith(op, lv.(int64), rv.(int64), typ)
	}}, nil
}

// promote returns the wider of two numeric types.
func promote(a, b sql.Type) sql.Type {
	switch {
	case a == sql.Float8 || b == sql.Float8:
		return sql.Float8
	case a == sql.Int8 || b == sql.Int8:
		return sql.Int8
	}
	return sql.Int4
}

// bindLogic implements AND / OR with SQL's three-valued logic: NULL is
// "unknown", so FALSE AND NULL is FALSE but TRUE AND NULL is NULL.
func (b *binder) bindLogic(e *sql.Binary) (bound, error) {
	var side [2]bound
	for i, x := range []sql.Expr{e.L, e.R} {
		bx, err := b.bind(x, sql.Bool)
		if err != nil {
			return bound{}, err
		}
		if bx.typ != sql.Bool && bx.typ != sql.Unknown {
			return bound{}, pgerr.New(pgerr.DatatypeMismatch,
				"argument of %s must be type boolean, not type %s", strings.ToUpper(e.Op), bx.typ).At(x.Position())
		}
		side[i] = bx
	}
	// For AND the dominant value is false; for OR it is true.
	dominant := e.Op == "or"
	return bound{sql.Bool, func(en *env) (any, error) {
		lv, err := side[0].eval(en)
		if err != nil {
			return nil, err
		}
		if lv != nil && lv.(bool) == dominant {
			return dominant, nil
		}
		rv, err := side[1].eval(en)
		if err != nil {
			return nil, err
		}
		if rv != nil && rv.(bool) == dominant {
			return dominant, nil
		}
		if lv == nil || rv == nil {
			return nil, nil
		}
		return !dominant, nil
	}}, nil
}

// bindUnified binds expressions that must end up with one common type: the
// branches of a CASE, the arguments of COALESCE, the elements of an IN list.
// Integers widen to bigint and to double precision; untyped operands (NULL,
// parameters, quoted literals) adopt the type of the others.
func (b *binder) bindUnified(exprs []sql.Expr, hint sql.Type, what string) ([]bound, sql.Type, error) {
	out := make([]bound, len(exprs))
	common := sql.Unknown
	var untyped []int
	for i, e := range exprs {
		bx, err := b.bind(e, sql.Unknown)
		if err != nil {
			return nil, 0, err
		}
		out[i] = bx
		if bx.typ == sql.Unknown || isStringLiteral(e) {
			untyped = append(untyped, i)
			continue
		}
		switch {
		case common == sql.Unknown || common == bx.typ:
			common = bx.typ
		case common.IsNumeric() && bx.typ.IsNumeric():
			common = promote(common, bx.typ)
		default:
			return nil, 0, pgerr.New(pgerr.DatatypeMismatch,
				"%s types %s and %s cannot be matched", what, common, bx.typ).At(e.Position())
		}
	}
	if common == sql.Unknown {
		common = hint
		for _, i := range untyped {
			if isStringLiteral(exprs[i]) && hint == sql.Unknown {
				common = sql.Text
			}
		}
	}
	for _, i := range untyped {
		bx, err := b.bind(exprs[i], common)
		if err != nil {
			return nil, 0, err
		}
		out[i] = bx
	}
	for i := range out {
		from := out[i].typ
		if from == common || from == sql.Unknown {
			continue
		}
		conv := numericConv(from, common)
		if conv == nil {
			return nil, 0, pgerr.New(pgerr.DatatypeMismatch,
				"%s types %s and %s cannot be matched", what, common, from).At(exprs[i].Position())
		}
		inner := out[i].eval
		out[i] = bound{common, func(en *env) (any, error) {
			v, err := inner(en)
			if err != nil || v == nil {
				return nil, err
			}
			return conv(v)
		}}
	}
	return out, common, nil
}

func (b *binder) bindCase(e *sql.Case, hint sql.Type) (bound, error) {
	// Conditions: either booleans, or values compared with the operand.
	conds := make([]evalFn, len(e.Whens))
	var operand bound
	var cmps []func(a, b any) int
	if e.Operand != nil {
		all := []sql.Expr{e.Operand}
		for _, w := range e.Whens {
			all = append(all, w.Cond)
		}
		bs, typ, err := b.bindUnified(all, sql.Unknown, "CASE")
		if err != nil {
			return bound{}, err
		}
		operand = bs[0]
		for i := range e.Whens {
			conds[i] = bs[i+1].eval
			cmps = append(cmps, comparator(typ, typ))
		}
	} else {
		for i, w := range e.Whens {
			c, err := b.bindWhere(w.Cond, "CASE/WHEN")
			if err != nil {
				return bound{}, err
			}
			conds[i] = c
		}
	}

	results := make([]sql.Expr, 0, len(e.Whens)+1)
	for _, w := range e.Whens {
		results = append(results, w.Then)
	}
	if e.Else != nil {
		results = append(results, e.Else)
	}
	rs, typ, err := b.bindUnified(results, hint, "CASE")
	if err != nil {
		return bound{}, err
	}
	otherwise := null
	if e.Else != nil {
		otherwise = rs[len(rs)-1].eval
	}

	return bound{typ, func(en *env) (any, error) {
		var subject any
		if operand.eval != nil {
			v, err := operand.eval(en)
			if err != nil {
				return nil, err
			}
			subject = v
		}
		for i, cond := range conds {
			v, err := cond(en)
			if err != nil {
				return nil, err
			}
			var hit bool
			if operand.eval != nil {
				hit = subject != nil && v != nil && cmps[i](subject, v) == 0
			} else {
				hit, _ = v.(bool)
			}
			if hit {
				return rs[i].eval(en)
			}
		}
		return otherwise(en)
	}}, nil
}

// inResult applies the three-valued logic of IN: true if some element
// matched, otherwise NULL if any comparison was unknown, otherwise false.
func inResult(matched, sawNull, not bool) any {
	switch {
	case matched:
		return !not
	case sawNull:
		return nil
	}
	return not
}

func (b *binder) bindIn(e *sql.In) (bound, error) {
	if e.Sub != nil {
		plan, err := b.subquery(e.Sub, e.Pos)
		if err != nil {
			return bound{}, err
		}
		x, err := b.bind(e.X, plan.types[0])
		if err != nil {
			return bound{}, err
		}
		cmp := comparator(x.typ, plan.types[0])
		if cmp == nil {
			return bound{}, pgerr.New(pgerr.UndefinedFunction,
				"operator does not exist: %s = %s", x.typ, plan.types[0]).At(e.Pos)
		}
		not := e.Not
		return bound{sql.Bool, func(en *env) (any, error) {
			v, err := x.eval(en)
			if err != nil {
				return nil, err
			}
			rows, err := plan.run(en)
			if err != nil {
				return nil, err
			}
			// x IN (empty set) is false even when x is NULL.
			if len(rows) == 0 {
				return not, nil
			}
			if v == nil {
				return nil, nil
			}
			matched, sawNull := false, false
			for _, r := range rows {
				switch {
				case r[0] == nil:
					sawNull = true
				case cmp(v, r[0]) == 0:
					matched = true
				}
			}
			return inResult(matched, sawNull, not), nil
		}}, nil
	}

	all := append([]sql.Expr{e.X}, e.List...)
	bs, typ, err := b.bindUnified(all, sql.Unknown, "IN")
	if err != nil {
		return bound{}, err
	}
	cmp := comparator(typ, typ)
	x, list, not := bs[0], bs[1:], e.Not
	return bound{sql.Bool, func(en *env) (any, error) {
		v, err := x.eval(en)
		if err != nil || v == nil {
			return nil, err
		}
		matched, sawNull := false, false
		for _, item := range list {
			iv, err := item.eval(en)
			if err != nil {
				return nil, err
			}
			switch {
			case iv == nil:
				sawNull = true
			case cmp(v, iv) == 0:
				matched = true
			}
		}
		return inResult(matched, sawNull, not), nil
	}}, nil
}

func (b *binder) bindLike(e *sql.Like) (bound, error) {
	var side [2]bound
	for i, x := range []sql.Expr{e.X, e.Pattern} {
		bx, err := b.bind(x, sql.Text)
		if err != nil {
			return bound{}, err
		}
		if bx.typ != sql.Text && bx.typ != sql.Unknown {
			return bound{}, pgerr.New(pgerr.UndefinedFunction,
				"operator does not exist: %s ~~ %s", side[0].typ, bx.typ).At(e.Pos)
		}
		side[i] = bx
	}
	not, fold := e.Not, e.ILike
	return bound{sql.Bool, func(en *env) (any, error) {
		s, err := side[0].eval(en)
		if err != nil || s == nil {
			return nil, err
		}
		p, err := side[1].eval(en)
		if err != nil || p == nil {
			return nil, err
		}
		str, pattern := s.(string), p.(string)
		if fold {
			str, pattern = strings.ToLower(str), strings.ToLower(pattern)
		}
		return likeMatch([]rune(str), []rune(pattern)) != not, nil
	}}, nil
}

// likeMatch implements LIKE: '%' matches any run of characters, '_' exactly
// one, and a backslash makes the next pattern character literal. It runs in
// O(len(s) * len(p)) at worst by remembering only the last '%' to retry
// from, rather than recursing.
func likeMatch(s, p []rune) bool {
	si, pi := 0, 0
	starP, starS := -1, 0
	for si < len(s) {
		if pi < len(p) {
			switch c := p[pi]; {
			case c == '%':
				starP, starS = pi, si
				pi++
				continue
			case c == '\\' && pi+1 < len(p):
				if p[pi+1] == s[si] {
					si++
					pi += 2
					continue
				}
			case c == '_' || c == s[si]:
				si++
				pi++
				continue
			}
		}
		if starP < 0 {
			return false
		}
		// Let the last '%' swallow one more character and try again.
		starS++
		si, pi = starS, starP+1
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}

func (b *binder) bindFunc(e *sql.FuncCall, hint sql.Type) (bound, error) {
	if isAggregate(e.Name) {
		return b.bindAggregate(e)
	}
	if e.Star || e.Distinct {
		return bound{}, pgerr.New(pgerr.SyntaxError, "%s is not an aggregate function", e.Name).At(e.Pos)
	}

	switch e.Name {
	case "coalesce":
		if len(e.Args) == 0 {
			break
		}
		args, typ, err := b.bindUnified(e.Args, hint, "COALESCE")
		if err != nil {
			return bound{}, err
		}
		return bound{typ, func(en *env) (any, error) {
			for _, a := range args {
				if v, err := a.eval(en); err != nil || v != nil {
					return v, err
				}
			}
			return nil, nil
		}}, nil
	case "nullif":
		if len(e.Args) != 2 {
			break
		}
		// The result has the type of the first argument; the second is
		// only compared with it. nullif(53, 2.5) is the integer 53.
		first, err := b.bind(e.Args[0], hint)
		if err != nil {
			return bound{}, err
		}
		second, err := b.bind(e.Args[1], first.typ)
		if err != nil {
			return bound{}, err
		}
		if first.typ == sql.Unknown && second.typ != sql.Unknown {
			if first, err = b.bind(e.Args[0], second.typ); err != nil {
				return bound{}, err
			}
		}
		cmp := comparator(first.typ, second.typ)
		if cmp == nil {
			return bound{}, pgerr.New(pgerr.DatatypeMismatch,
				"NULLIF types %s and %s cannot be matched", first.typ, second.typ).At(e.Pos)
		}
		args := []bound{first, second}
		return bound{first.typ, func(en *env) (any, error) {
			v, err := args[0].eval(en)
			if err != nil || v == nil {
				return nil, err
			}
			w, err := args[1].eval(en)
			if err != nil {
				return nil, err
			}
			if w != nil && cmp(v, w) == 0 {
				return nil, nil
			}
			return v, nil
		}}, nil
	}

	args := make([]bound, len(e.Args))
	argTypes := make([]string, len(e.Args))
	for i, a := range e.Args {
		argHint := sql.Text
		if e.Name == "pg_sleep" || e.Name == "abs" {
			argHint = sql.Unknown
		}
		ba, err := b.bind(a, argHint)
		if err != nil {
			return bound{}, err
		}
		args[i], argTypes[i] = ba, ba.typ.String()
	}
	is := func(types ...sql.Type) bool {
		if len(types) != len(args) {
			return false
		}
		for i, t := range types {
			if args[i].typ != t && args[i].typ != sql.Unknown {
				return false
			}
		}
		return true
	}
	// strFn builds a NULL-propagating function of one text argument.
	strFn := func(typ sql.Type, f func(string) any) (bound, error) {
		arg := args[0]
		return bound{typ, func(en *env) (any, error) {
			v, err := arg.eval(en)
			if err != nil || v == nil {
				return nil, err
			}
			return f(v.(string)), nil
		}}, nil
	}

	switch e.Name {
	case "version":
		if is() {
			return constant(sql.Text, version.Full), nil
		}
	case "current_database":
		if is() {
			return constant(sql.Text, b.sess.database), nil
		}
	case "current_schema":
		if is() {
			return constant(sql.Text, "public"), nil
		}
	case "length":
		if is(sql.Text) {
			return strFn(sql.Int4, func(s string) any { return int64(len([]rune(s))) })
		}
	case "upper":
		if is(sql.Text) {
			return strFn(sql.Text, func(s string) any { return strings.ToUpper(s) })
		}
	case "lower":
		if is(sql.Text) {
			return strFn(sql.Text, func(s string) any { return strings.ToLower(s) })
		}
	case "abs":
		if len(args) == 1 && args[0].typ.IsNumeric() {
			arg, typ := args[0], args[0].typ
			return bound{typ, func(en *env) (any, error) {
				v, err := arg.eval(en)
				if err != nil || v == nil {
					return nil, err
				}
				if f, ok := v.(float64); ok {
					return math.Abs(f), nil
				}
				if i := v.(int64); i < 0 {
					return checkIntRange(-i, i == math.MinInt64, typ)
				}
				return v, nil
			}}, nil
		}
	case "pg_sleep":
		// Besides being handy for demos, pg_sleep is what the tests use to
		// have a statement in flight when a CancelRequest arrives.
		if len(args) == 1 && (args[0].typ.IsNumeric() || args[0].typ == sql.Unknown) {
			arg := args[0]
			return bound{sql.Text, func(en *env) (any, error) {
				v, err := arg.eval(en)
				if err != nil || v == nil {
					return nil, err
				}
				timer := time.NewTimer(time.Duration(toFloat(v) * float64(time.Second)))
				defer timer.Stop()
				select {
				case <-timer.C:
					return "", nil
				case <-en.ctx.Done():
					return nil, en.ctx.Err()
				}
			}}, nil
		}
	}
	return bound{}, pgerr.New(pgerr.UndefinedFunction,
		"function %s(%s) does not exist", e.Name, strings.Join(argTypes, ", ")).At(e.Pos)
}

func isStringLiteral(e sql.Expr) bool {
	lit, ok := e.(*sql.Literal)
	return ok && lit.Type == sql.Text
}

// bindWhere binds a condition (WHERE, ON, HAVING, WHEN), which must be
// boolean. A nil expression yields a nil evalFn.
func (b *binder) bindWhere(e sql.Expr, clause string) (evalFn, error) {
	if e == nil {
		return nil, nil
	}
	w, err := b.bind(e, sql.Bool)
	if err != nil {
		return nil, err
	}
	if w.typ != sql.Bool && w.typ != sql.Unknown {
		return nil, pgerr.New(pgerr.DatatypeMismatch,
			"argument of %s must be type boolean, not type %s", clause, w.typ).At(e.Position())
	}
	return w.eval, nil
}

// matches evaluates a condition; NULL counts as false.
func matches(cond evalFn, en *env) (bool, error) {
	if cond == nil {
		return true, nil
	}
	v, err := cond(en)
	if err != nil {
		return false, err
	}
	ok, _ := v.(bool)
	return ok, nil
}

// ---- operators ----

func toFloat(v any) float64 {
	if i, ok := v.(int64); ok {
		return float64(i)
	}
	return v.(float64)
}

// comparator returns a three-way comparison for two operand types, or nil if
// they cannot be compared. An Unknown side (an untyped NULL) is accepted: the
// comparison is never actually called for it because NULL short-circuits.
func comparator(l, r sql.Type) func(a, b any) int {
	if l == sql.Unknown {
		l = r
	}
	if r == sql.Unknown {
		r = l
	}
	switch {
	case l.IsInt() && r.IsInt():
		return func(a, b any) int { return cmp3(a.(int64), b.(int64)) }
	case l.IsNumeric() && r.IsNumeric():
		return func(a, b any) int { return cmp3(toFloat(a), toFloat(b)) }
	case l == sql.Text && r == sql.Text, l == sql.Unknown:
		return func(a, b any) int { return strings.Compare(a.(string), b.(string)) }
	case l == sql.Bool && r == sql.Bool:
		return func(a, b any) int {
			x, y := a.(bool), b.(bool)
			switch {
			case x == y:
				return 0
			case !x:
				return -1
			}
			return 1
		}
	}
	return nil
}

func cmp3[T int64 | float64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareTest(op string) func(int) bool {
	switch op {
	case "=":
		return func(c int) bool { return c == 0 }
	case "<>":
		return func(c int) bool { return c != 0 }
	case "<":
		return func(c int) bool { return c < 0 }
	case "<=":
		return func(c int) bool { return c <= 0 }
	case ">":
		return func(c int) bool { return c > 0 }
	}
	return func(c int) bool { return c >= 0 }
}

// checkIntRange returns v, or an out-of-range error if the 64-bit operation
// overflowed or the result does not fit the (possibly 32-bit) result type.
func checkIntRange(v int64, overflow bool, typ sql.Type) (any, error) {
	if typ == sql.Int4 && (overflow || v < math.MinInt32 || v > math.MaxInt32) {
		return nil, pgerr.New(pgerr.NumericValueOutOfRange, "integer out of range")
	}
	if overflow {
		return nil, pgerr.New(pgerr.NumericValueOutOfRange, "bigint out of range")
	}
	return v, nil
}

func intArith(op string, a, b int64, typ sql.Type) (any, error) {
	switch op {
	case "+":
		s := a + b
		return checkIntRange(s, (s > a) != (b > 0), typ)
	case "-":
		d := a - b
		return checkIntRange(d, (d < a) != (b > 0), typ)
	case "*":
		if a == 0 || b == 0 {
			return int64(0), nil
		}
		p := a * b
		overflow := p/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64)
		return checkIntRange(p, overflow, typ)
	}
	if b == 0 {
		return nil, pgerr.New(pgerr.DivisionByZero, "division by zero")
	}
	if op == "/" {
		// SQL integer division truncates toward zero, as Go's does.
		return checkIntRange(a/b, a == math.MinInt64 && b == -1, typ)
	}
	if b == -1 {
		return int64(0), nil // avoids the MinInt64 % -1 trap
	}
	return a % b, nil
}

func floatArith(op string, a, b float64) (any, error) {
	switch op {
	case "+":
		return a + b, nil
	case "-":
		return a - b, nil
	case "*":
		return a * b, nil
	}
	if b == 0 {
		return nil, pgerr.New(pgerr.DivisionByZero, "division by zero")
	}
	return a / b, nil
}

// ---- conversions ----

type convFn func(any) (any, error)

func identity(v any) (any, error) { return v, nil }

func intToInt4(v any) (any, error) {
	return checkIntRange(v.(int64), false, sql.Int4)
}

func floatToInt(typ sql.Type) convFn {
	return func(v any) (any, error) {
		// PostgreSQL rounds to nearest, ties to even, when casting to integer.
		f := math.RoundToEven(v.(float64))
		if math.IsNaN(f) || f < math.MinInt64 || f >= math.MaxInt64 {
			return nil, pgerr.New(pgerr.NumericValueOutOfRange, "%s out of range", typ)
		}
		return checkIntRange(int64(f), false, typ)
	}
}

// numericConv converts between numeric types, or returns nil if either type
// is not numeric.
func numericConv(from, to sql.Type) convFn {
	switch {
	case from.IsInt() && to == sql.Int8:
		return identity
	case from.IsInt() && to == sql.Int4:
		return intToInt4
	case from.IsInt() && to == sql.Float8:
		return func(v any) (any, error) { return float64(v.(int64)), nil }
	case from == sql.Float8 && to.IsInt():
		return floatToInt(to)
	}
	return nil
}

// castFn returns the conversion for an explicit CAST / "::".
func castFn(from, to sql.Type) (convFn, error) {
	if from == to || from == sql.Unknown {
		return identity, nil
	}
	if conv := numericConv(from, to); conv != nil {
		return conv, nil
	}
	switch {
	case to == sql.Text:
		return func(v any) (any, error) {
			if b, ok := v.(bool); ok {
				// Casting a boolean to text spells the word out, unlike
				// the "t"/"f" of the output format.
				if b {
					return "true", nil
				}
				return "false", nil
			}
			return pgwire.TextValue(v), nil
		}, nil
	case from == sql.Text && to == sql.Int4:
		return func(v any) (any, error) { return pgwire.ParseInt(v.(string), 32, "integer") }, nil
	case from == sql.Text && to == sql.Int8:
		return func(v any) (any, error) { return pgwire.ParseInt(v.(string), 64, "bigint") }, nil
	case from == sql.Text && to == sql.Float8:
		return func(v any) (any, error) { return pgwire.ParseFloat(v.(string)) }, nil
	case from == sql.Text && to == sql.Bool:
		return func(v any) (any, error) {
			b, ok := pgwire.ParseBool(v.(string))
			if !ok {
				return nil, pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type boolean: %q", v)
			}
			return b, nil
		}, nil
	case from == sql.Bool && to == sql.Int4:
		return func(v any) (any, error) {
			if v.(bool) {
				return int64(1), nil
			}
			return int64(0), nil
		}, nil
	case from == sql.Int4 && to == sql.Bool:
		return func(v any) (any, error) { return v.(int64) != 0, nil }, nil
	}
	return nil, pgerr.New(pgerr.CannotCoerce, "cannot cast type %s to %s", from, to)
}

// assignFn returns the implicit conversion applied when a value of type from
// is stored into a column of type to, or nil if there is none. It is stricter
// than an explicit cast: text is never silently parsed into a number.
func assignFn(from, to sql.Type) convFn {
	if from == to || from == sql.Unknown {
		return identity
	}
	if conv := numericConv(from, to); conv != nil {
		return conv
	}
	return nil
}

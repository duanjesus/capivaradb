package engine

import (
	"context"
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
	row    []any // current row of the FROM table, if any
	params []any
}

type evalFn func(*env) (any, error)

// bound is an expression after name resolution and type checking, compiled
// to a closure so that execution does not walk the AST again.
type bound struct {
	typ  sql.Type
	eval evalFn
}

// binder resolves names and types for the expressions of one statement.
type binder struct {
	sess  *Session
	tbl   *table // the FROM / target table, or nil
	alias string
	// ptypes holds the type of each parameter. Entries that are still
	// Unknown get filled in from context as expressions are bound.
	ptypes []sql.Type
}

func null(*env) (any, error) { return nil, nil }

// bind compiles e. hint is the type the surrounding context expects; it is
// only used to give a type to things that have none of their own: untyped
// parameters and NULL.
func (b *binder) bind(e sql.Expr, hint sql.Type) (bound, error) {
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
		return bound{t, func(*env) (any, error) { return v, nil }}, nil

	case *sql.Param:
		i := e.Index - 1
		if b.ptypes[i] == sql.Unknown {
			b.ptypes[i] = hint
		}
		return bound{b.ptypes[i], func(en *env) (any, error) { return en.params[i], nil }}, nil

	case *sql.ColumnRef:
		return b.bindColumn(e)

	case *sql.Unary:
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
		return b.bindFunc(e)
	}
	return bound{}, pgerr.New(pgerr.InternalError, "unhandled expression node %T", e)
}

func (b *binder) bindColumn(e *sql.ColumnRef) (bound, error) {
	if e.Table != "" && (b.tbl == nil || e.Table != b.alias) {
		return bound{}, pgerr.New(pgerr.UndefinedTable, "missing FROM-clause entry for table %q", e.Table).At(e.Pos)
	}
	if b.tbl != nil {
		if i := b.tbl.colIndex(e.Name); i >= 0 {
			return bound{b.tbl.cols[i].typ, func(en *env) (any, error) { return en.row[i], nil }}, nil
		}
	}
	// SQL has a few "functions" that are written without parentheses.
	if e.Table == "" {
		switch e.Name {
		case "current_user", "session_user":
			user := b.sess.user
			return bound{sql.Text, func(*env) (any, error) { return user, nil }}, nil
		case "current_schema":
			return bound{sql.Text, func(*env) (any, error) { return "public", nil }}, nil
		}
	}
	return bound{}, pgerr.New(pgerr.UndefinedColumn, "column %q does not exist", e.Name).At(e.Pos)
}

func (b *binder) bindBinary(e *sql.Binary) (bound, error) {
	if e.Op == "and" || e.Op == "or" {
		return b.bindLogic(e)
	}

	// Bind the left side first and let its type flow to the right; if only
	// the right side turned out to have a type, flow it back. This is what
	// types the parameter in both "id = $1" and "$1 = id".
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
	typ := sql.Int4
	switch {
	case l.typ == sql.Float8 || r.typ == sql.Float8:
		typ = sql.Float8
	case l.typ == sql.Int8 || r.typ == sql.Int8:
		typ = sql.Int8
	}
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

func (b *binder) bindFunc(e *sql.FuncCall) (bound, error) {
	args := make([]bound, len(e.Args))
	argTypes := make([]string, len(e.Args))
	for i, a := range e.Args {
		hint := sql.Text
		if e.Name == "pg_sleep" {
			hint = sql.Float8
		}
		ba, err := b.bind(a, hint)
		if err != nil {
			return bound{}, err
		}
		args[i], argTypes[i] = ba, ba.typ.String()
	}
	constant := func(s string) (bound, error) {
		return bound{sql.Text, func(*env) (any, error) { return s, nil }}, nil
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
			return constant(version.Full)
		}
	case "current_database":
		if is() {
			return constant(b.sess.database)
		}
	case "current_schema":
		if is() {
			return constant("public")
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

package engine

import (
	"encoding/binary"
	"math"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/sql"
)

func isAggregate(name string) bool {
	switch name {
	case "count", "sum", "avg", "min", "max":
		return true
	}
	return false
}

// hasAggregate reports whether e contains an aggregate call at this query
// level (subqueries are their own level and are not looked into).
func hasAggregate(e sql.Expr) bool {
	found := false
	sql.WalkExpr(e, func(x sql.Expr) bool {
		if call, ok := x.(*sql.FuncCall); ok && isAggregate(call.Name) {
			found = true
		}
		return !found
	})
	return found
}

// aggSpec is one aggregate call of a query, e.g. sum(distinct price).
type aggSpec struct {
	fn       string
	arg      evalFn // nil for count(*)
	argType  sql.Type
	distinct bool
	cmp      func(a, b any) int // for min and max
}

// aggState accumulates one aggregate over the rows of one group.
type aggState struct {
	count int64
	sumI  int64
	sumF  float64
	best  any
	seen  map[string]struct{}
}

func (b *binder) bindAggregate(e *sql.FuncCall) (bound, error) {
	if b.aggs == nil {
		return bound{}, pgerr.New(pgerr.GroupingError, "aggregate functions are not allowed here").At(e.Pos)
	}
	if b.inAgg {
		return bound{}, pgerr.New(pgerr.GroupingError, "aggregate function calls cannot be nested").At(e.Pos)
	}
	spec := aggSpec{fn: e.Name, distinct: e.Distinct}
	typ := sql.Int8

	switch {
	case e.Star:
		if e.Name != "count" {
			return bound{}, pgerr.New(pgerr.SyntaxError, "%s(*) is not valid; only count(*) is", e.Name).At(e.Pos)
		}
	case len(e.Args) != 1:
		return bound{}, pgerr.New(pgerr.UndefinedFunction, "%s takes exactly one argument", e.Name).At(e.Pos)
	default:
		b.inAgg = true
		arg, err := b.bind(e.Args[0], sql.Unknown)
		b.inAgg = false
		if err != nil {
			return bound{}, err
		}
		spec.arg, spec.argType = arg.eval, arg.typ
		numeric := arg.typ.IsNumeric() || arg.typ == sql.Unknown
		switch e.Name {
		case "sum":
			// PostgreSQL widens sum(integer) to bigint and sum(bigint) to
			// numeric. There is no numeric type here, so a bigint sum
			// stays bigint and overflow is an error instead.
			if !numeric {
				return bound{}, pgerr.New(pgerr.UndefinedFunction, "function sum(%s) does not exist", arg.typ).At(e.Pos)
			}
			if arg.typ == sql.Float8 {
				typ = sql.Float8
			}
		case "avg":
			if !numeric {
				return bound{}, pgerr.New(pgerr.UndefinedFunction, "function avg(%s) does not exist", arg.typ).At(e.Pos)
			}
			typ = sql.Float8
		case "min", "max":
			typ = arg.typ
			spec.cmp = comparator(arg.typ, arg.typ)
		}
	}

	i := len(*b.aggs)
	*b.aggs = append(*b.aggs, spec)
	return bound{typ, func(en *env) (any, error) { return en.aggs[i], nil }}, nil
}

// feed adds the current row to the aggregate.
func (st *aggState) feed(spec *aggSpec, en *env) error {
	if spec.arg == nil { // count(*)
		st.count++
		return nil
	}
	v, err := spec.arg(en)
	if err != nil {
		return err
	}
	// Aggregates other than count(*) ignore NULL inputs.
	if v == nil {
		return nil
	}
	if spec.distinct {
		key := string(appendKey(nil, v))
		if _, dup := st.seen[key]; dup {
			return nil
		}
		if st.seen == nil {
			st.seen = make(map[string]struct{})
		}
		st.seen[key] = struct{}{}
	}
	st.count++
	switch spec.fn {
	case "sum", "avg":
		if f, ok := v.(float64); ok {
			st.sumF += f
			break
		}
		i := v.(int64)
		st.sumF += float64(i)
		s := st.sumI + i
		if (s > st.sumI) != (i > 0) && spec.fn == "sum" {
			return pgerr.New(pgerr.NumericValueOutOfRange, "bigint out of range")
		}
		st.sumI = s
	case "min":
		if st.best == nil || spec.cmp(v, st.best) < 0 {
			st.best = v
		}
	case "max":
		if st.best == nil || spec.cmp(v, st.best) > 0 {
			st.best = v
		}
	}
	return nil
}

// result is the aggregate's value for the group. Over no rows, count is 0
// and everything else is NULL.
func (st *aggState) result(spec *aggSpec) any {
	switch spec.fn {
	case "count":
		return st.count
	case "sum":
		if st.count == 0 {
			return nil
		}
		if spec.argType == sql.Float8 {
			return st.sumF
		}
		return st.sumI
	case "avg":
		if st.count == 0 {
			return nil
		}
		return st.sumF / float64(st.count)
	}
	return st.best
}

// appendKey appends an encoding of v under which two values are equal
// exactly when their encodings are. It is the map key for GROUP BY,
// DISTINCT and count(distinct ...).
func appendKey(dst []byte, v any) []byte {
	switch v := v.(type) {
	case nil:
		return append(dst, 0)
	case bool:
		if v {
			return append(dst, 1, 1)
		}
		return append(dst, 1, 0)
	case int64:
		return binary.BigEndian.AppendUint64(append(dst, 2), uint64(v))
	case float64:
		if v == 0 {
			v = 0 // -0 and +0 are equal and must share a key
		}
		return binary.BigEndian.AppendUint64(append(dst, 3), math.Float64bits(v))
	case string:
		// Length-prefixed, so that ("ab","c") and ("a","bc") differ.
		dst = binary.BigEndian.AppendUint32(append(dst, 4), uint32(len(v)))
		return append(dst, v...)
	}
	return dst
}

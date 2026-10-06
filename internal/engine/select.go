package engine

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
)

// This file runs what a SELECT does after its FROM and WHERE clauses, which
// plan.go plans: grouping, the select list, DISTINCT, ordering and limits.
// Every step materialises its whole result. It is what the iterator executor
// (milestone 7) will be measured against, and the semantics settled here —
// scoping, grouping rules, NULL ordering — stay.

// selectPlan is a SELECT ready to run.
type selectPlan struct {
	cols  []pgwire.Column
	types []sql.Type
	// run executes the query. cx is the environment of the enclosing query
	// level (for a top-level query, one with no row): it supplies the
	// context, the parameters and the outer rows a correlated subquery
	// refers to.
	run func(cx *env) ([][]any, error)
	// root is the plan as a tree, for EXPLAIN.
	root *planNode
}

func (t *table) scopeCols(alias string) []scopeCol {
	cols := make([]scopeCol, len(t.cols))
	for i, c := range t.cols {
		cols[i] = scopeCol{table: alias, name: c.name, typ: c.typ}
	}
	return cols
}

// sortKey is one ORDER BY entry.
type sortKey struct {
	// out is the index of the output column to sort by, or -1 if the key is
	// an expression of its own, evaluated by eval.
	out        int
	eval       evalFn
	cmp        func(a, b any) int
	desc       bool
	nullsFirst bool
}

func (s *Session) planSelect(n *sql.Select, parent *scope, ptypes []sql.Type) (*selectPlan, error) {
	// FROM and WHERE are planned together: where each condition is
	// applied, how each table is read and in what order they are joined
	// is the planner's business (plan.go). What comes back is a plan
	// producing the rows that satisfy WHERE, with the columns of the FROM
	// clause in the order they were written.
	var fromCols []scopeCol
	var source *planNode
	if n.From != nil {
		var err error
		if fromCols, source, err = s.planFromWhere(n.From, n.Where, parent, ptypes); err != nil {
			return nil, err
		}
	} else {
		// Without FROM a query produces exactly one row with no columns,
		// or none if WHERE says so.
		cond, err := (&binder{sess: s, scope: &scope{parent: parent}, ptypes: ptypes}).bindWhere(n.Where, "WHERE")
		if err != nil {
			return nil, err
		}
		source = &planNode{op: "Result", rows: 1, cost: 0.01}
		if n.Where != nil {
			source.lines = []string{"One-Time Filter: " + sql.FormatExpr(n.Where)}
		}
		source.run = func(cx *env) ([][]any, error) {
			ok, err := matches(cond, &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked})
			if err != nil || !ok {
				return nil, err
			}
			return [][]any{nil}, nil
		}
	}
	sc := &scope{cols: fromCols, parent: parent}
	b := &binder{sess: s, scope: sc, ptypes: ptypes}
	// The rows arrive already filtered.
	var where evalFn
	var err error

	// Expand "*" and "t.*" into the columns they stand for.
	type item struct {
		expr sql.Expr
		name string
	}
	var items []item
	for _, it := range n.Items {
		if !it.Star {
			name := it.Alias
			if name == "" {
				name = columnName(it.Expr)
			}
			items = append(items, item{it.Expr, name})
			continue
		}
		if n.From == nil {
			return nil, pgerr.New(pgerr.SyntaxError, "SELECT * with no tables specified is not valid").At(it.Pos)
		}
		matched := false
		for i, c := range sc.cols {
			if it.Table == "" || it.Table == c.table {
				items = append(items, item{&colIdx{i}, c.name})
				matched = true
			}
		}
		if !matched {
			return nil, pgerr.New(pgerr.UndefinedTable, "missing FROM-clause entry for table %q", it.Table).At(it.Pos)
		}
	}

	// outputRef resolves the two shorthands GROUP BY and ORDER BY accept
	// for a select-list entry: its 1-based position, or its name.
	outputRef := func(e sql.Expr, clause string, byName bool) (int, error) {
		switch e := e.(type) {
		case *sql.Literal:
			if e.Type.IsInt() {
				pos := e.Val.(int64)
				if pos < 1 || pos > int64(len(items)) {
					return -1, pgerr.New("42P10", "%s position %d is not in select list", clause, pos).At(e.Pos)
				}
				return int(pos - 1), nil
			}
		case *sql.ColumnRef:
			if !byName || e.Table != "" {
				break
			}
			for i, it := range items {
				if it.name == e.Name {
					return i, nil
				}
			}
		}
		return -1, nil
	}

	// GROUP BY. A name is an input column first and an output name second,
	// the opposite of ORDER BY.
	groupExprs := make([]sql.Expr, len(n.GroupBy))
	for i, g := range n.GroupBy {
		groupExprs[i] = g
		ref, isCol := g.(*sql.ColumnRef)
		inputCol := false
		if isCol {
			idx, _ := sc.find(ref)
			inputCol = idx >= 0
		}
		out, err := outputRef(g, "GROUP BY", !inputCol)
		if err != nil {
			return nil, err
		}
		if out >= 0 {
			groupExprs[i] = items[out].expr
		}
	}
	groupEvals := make([]evalFn, len(groupExprs))
	for i, g := range groupExprs {
		if hasAggregate(g) {
			return nil, pgerr.New(pgerr.GroupingError, "aggregate functions are not allowed in GROUP BY").At(g.Position())
		}
		bg, err := b.bind(g, sql.Unknown)
		if err != nil {
			return nil, err
		}
		groupEvals[i] = bg.eval
	}

	grouped := len(groupExprs) > 0 || n.Having != nil
	for _, it := range items {
		grouped = grouped || hasAggregate(it.expr)
	}
	for _, o := range n.OrderBy {
		grouped = grouped || hasAggregate(o.Expr)
	}

	// From here on expressions are evaluated per output row, which in a
	// grouped query means once per group.
	var aggs []aggSpec
	if grouped {
		b.aggs, b.grouped = &aggs, true
		b.groupKeys = make(map[string]bool)
		for _, g := range groupExprs {
			b.groupKeys[b.keyOf(g)] = true
		}
	}

	having, err := b.bindWhere(n.Having, "HAVING")
	if err != nil {
		return nil, err
	}

	plan := &selectPlan{}
	outs := make([]evalFn, len(items))
	for i, it := range items {
		bo, err := b.bind(it.expr, sql.Unknown)
		if err != nil {
			return nil, err
		}
		outs[i] = bo.eval
		typ := bo.typ
		if typ == sql.Unknown {
			typ = sql.Text
		}
		plan.types = append(plan.types, typ)
		plan.cols = append(plan.cols, pgwire.Column{Name: it.name, OID: oidOf(typ)})
	}

	keys := make([]sortKey, len(n.OrderBy))
	for i, o := range n.OrderBy {
		key := sortKey{desc: o.Desc, nullsFirst: o.Desc}
		if o.NullsFirst != nil {
			key.nullsFirst = *o.NullsFirst
		}
		if key.out, err = outputRef(o.Expr, "ORDER BY", true); err != nil {
			return nil, err
		}
		// An expression that is textually a select-list entry sorts by
		// that entry, which is also what makes it legal with DISTINCT.
		for j := 0; key.out < 0 && j < len(items); j++ {
			if _, star := items[j].expr.(*colIdx); !star && sql.FormatExpr(items[j].expr) == sql.FormatExpr(o.Expr) {
				key.out = j
			}
		}
		typ := sql.Unknown
		if key.out >= 0 {
			typ = plan.types[key.out]
		} else {
			if n.Distinct {
				return nil, pgerr.New("42P10",
					"for SELECT DISTINCT, ORDER BY expressions must appear in select list").At(o.Expr.Position())
			}
			bk, err := b.bind(o.Expr, sql.Unknown)
			if err != nil {
				return nil, err
			}
			key.eval, typ = bk.eval, bk.typ
		}
		key.cmp = comparator(typ, typ)
		keys[i] = key
	}

	limit, err := s.bindRowCount(n.Limit, parent, ptypes, "LIMIT")
	if err != nil {
		return nil, err
	}
	offset, err := s.bindRowCount(n.Offset, parent, ptypes, "OFFSET")
	if err != nil {
		return nil, err
	}

	// The steps after the join, as plan nodes. They all run inside the one
	// function below; the nodes exist so that EXPLAIN can show them.
	root := source
	stage := func(op string, rows float64, lines ...string) *planNode {
		root = &planNode{op: op, lines: lines, kids: []*planNode{root}, rows: math.Max(rows, 1), cost: root.cost + root.rows*0.1}
		return root
	}
	var aggNode, distinctNode, sortNode, limitNode *planNode
	if grouped {
		var lines []string
		op, rows := "Aggregate", 1.0
		if len(groupExprs) > 0 {
			op, rows = "HashAggregate", source.rows/10
			lines = append(lines, "Group Key: "+exprCSV(groupExprs))
		}
		if n.Having != nil {
			lines = append(lines, "Filter: "+sql.FormatExpr(n.Having))
		}
		aggNode = stage(op, rows, lines...)
	}
	if n.Distinct {
		distinctNode = stage("Unique", root.rows/2)
	}
	if len(n.OrderBy) > 0 {
		sortExprs := make([]sql.Expr, len(n.OrderBy))
		for i, o := range n.OrderBy {
			sortExprs[i] = o.Expr
		}
		sortNode = stage("Sort", root.rows, "Sort Key: "+exprCSV(sortExprs))
		sortNode.cost += root.rows * math.Log2(root.rows+2) * 0.05
	}
	if n.Limit != nil || n.Offset != nil {
		rows := root.rows
		if lit, ok := n.Limit.(*sql.Literal); ok && lit.Type.IsInt() {
			rows = math.Min(rows, float64(lit.Val.(int64)))
		}
		limitNode = stage("Limit", rows)
	}
	plan.root = root

	nCols, distinct := len(sc.cols), n.Distinct
	plan.run = func(cx *env) ([][]any, error) {
		started := time.Now()
		input, err := source.exec(cx)
		if err != nil {
			return nil, err
		}
		en := &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked}
		// done records, for EXPLAIN ANALYZE, that a step has finished and
		// how many rows it left.
		done := func(node *planNode, rows int) {
			if node != nil {
				node.loops++
				node.actual += rows
				node.elapsed += time.Since(started)
			}
		}

		type outRow struct {
			vals []any
			keys []any
		}
		var out []outRow
		// emit produces the output row for en's current row or group.
		emit := func() error {
			vals := make([]any, len(outs))
			for i, eval := range outs {
				if vals[i], err = eval(en); err != nil {
					return err
				}
			}
			row := outRow{vals: vals}
			if len(keys) > 0 {
				row.keys = make([]any, len(keys))
				for i, k := range keys {
					if k.out >= 0 {
						row.keys[i] = vals[k.out]
					} else if row.keys[i], err = k.eval(en); err != nil {
						return err
					}
				}
			}
			out = append(out, row)
			return nil
		}

		if !grouped {
			for _, row := range input {
				if err := cx.ctx.Err(); err != nil {
					return nil, err
				}
				en.row = row
				ok, err := matches(where, en)
				if err != nil {
					return nil, err
				}
				if ok {
					if err := emit(); err != nil {
						return nil, err
					}
				}
			}
		} else {
			type group struct {
				first  []any // a representative row, for the GROUP BY columns
				states []aggState
			}
			var order []*group
			index := make(map[string]*group)
			var keyBuf []byte
			for _, row := range input {
				if err := cx.ctx.Err(); err != nil {
					return nil, err
				}
				en.row = row
				ok, err := matches(where, en)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				keyBuf = keyBuf[:0]
				for _, g := range groupEvals {
					v, err := g(en)
					if err != nil {
						return nil, err
					}
					keyBuf = appendKey(keyBuf, v)
				}
				grp := index[string(keyBuf)]
				if grp == nil {
					grp = &group{first: row, states: make([]aggState, len(aggs))}
					index[string(keyBuf)] = grp
					order = append(order, grp)
				}
				for i := range aggs {
					if err := grp.states[i].feed(&aggs[i], en); err != nil {
						return nil, err
					}
				}
			}
			// Aggregating without GROUP BY always yields one row, even
			// over no input: count(*) of an empty table is 0, not nothing.
			if len(groupEvals) == 0 && len(order) == 0 {
				order = append(order, &group{first: make([]any, nCols), states: make([]aggState, len(aggs))})
			}
			en.aggs = make([]any, len(aggs))
			for _, grp := range order {
				en.row = grp.first
				for i := range aggs {
					en.aggs[i] = grp.states[i].result(&aggs[i])
				}
				ok, err := matches(having, en)
				if err != nil {
					return nil, err
				}
				if ok {
					if err := emit(); err != nil {
						return nil, err
					}
				}
			}
		}

		done(aggNode, len(out))
		if distinct {
			seen := make(map[string]struct{}, len(out))
			kept := out[:0]
			var keyBuf []byte
			for _, row := range out {
				keyBuf = keyBuf[:0]
				for _, v := range row.vals {
					keyBuf = appendKey(keyBuf, v)
				}
				if _, dup := seen[string(keyBuf)]; !dup {
					seen[string(keyBuf)] = struct{}{}
					kept = append(kept, row)
				}
			}
			out = kept
		}
		done(distinctNode, len(out))

		if len(keys) > 0 {
			sort.SliceStable(out, func(i, j int) bool {
				for k, key := range keys {
					a, b := out[i].keys[k], out[j].keys[k]
					switch {
					case a == nil && b == nil:
						continue
					case a == nil:
						return key.nullsFirst
					case b == nil:
						return !key.nullsFirst
					}
					c := key.cmp(a, b)
					if c == 0 {
						continue
					}
					return (c < 0) != key.desc
				}
				return false
			})
		}

		done(sortNode, len(out))
		skip, err := offset(cx, 0)
		if err != nil {
			return nil, err
		}
		take, err := limit(cx, len(out))
		if err != nil {
			return nil, err
		}
		skip = min(skip, len(out))
		take = min(take, len(out)-skip)

		rows := make([][]any, take)
		for i := range rows {
			rows[i] = out[skip+i].vals
		}
		done(limitNode, len(rows))
		return rows, nil
	}
	return plan, nil
}

// bindRowCount binds a LIMIT or OFFSET expression. The returned function
// evaluates it, giving def when the clause is absent or NULL.
func (s *Session) bindRowCount(e sql.Expr, parent *scope, ptypes []sql.Type, clause string) (func(cx *env, def int) (int, error), error) {
	if e == nil {
		return func(_ *env, def int) (int, error) { return def, nil }, nil
	}
	// It may use parameters and outer columns, but not the query's own.
	b := &binder{sess: s, scope: &scope{parent: parent}, ptypes: ptypes}
	be, err := b.bind(e, sql.Int8)
	if err != nil {
		return nil, err
	}
	if !be.typ.IsInt() && be.typ != sql.Unknown {
		return nil, pgerr.New(pgerr.DatatypeMismatch,
			"argument of %s must be type bigint, not type %s", clause, be.typ).At(e.Position())
	}
	code := pgerr.InvalidRowCountInLimit
	if clause == "OFFSET" {
		code = pgerr.InvalidRowCountInOffset
	}
	return func(cx *env, def int) (int, error) {
		v, err := be.eval(&env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked})
		if err != nil || v == nil {
			return def, err
		}
		n := v.(int64)
		if n < 0 {
			return 0, pgerr.New(code, "%s must not be negative", clause)
		}
		return int(min(n, int64(1)<<40)), nil
	}, nil
}

// columnName picks the name of a result column the way PostgreSQL does.
func columnName(e sql.Expr) string {
	switch e := e.(type) {
	case *sql.ColumnRef:
		return e.Name
	case *sql.FuncCall:
		return e.Name
	case *sql.Case:
		return "case"
	case *sql.Exists:
		return "exists"
	case *sql.SubqueryExpr:
		if len(e.Select.Items) == 1 && !e.Select.Items[0].Star {
			if alias := e.Select.Items[0].Alias; alias != "" {
				return alias
			}
			return columnName(e.Select.Items[0].Expr)
		}
	case *sql.Cast:
		if name := columnName(e.X); name != "?column?" {
			return name
		}
		switch e.To {
		case sql.Bool:
			return "bool"
		case sql.Int4:
			return "int4"
		case sql.Int8:
			return "int8"
		case sql.Float8:
			return "float8"
		}
		return "text"
	}
	return "?column?"
}

func exprCSV(exprs []sql.Expr) string {
	parts := make([]string, len(exprs))
	for i, e := range exprs {
		if _, positional := e.(*colIdx); positional {
			parts[i] = "*"
		} else {
			parts[i] = sql.FormatExpr(e)
		}
	}
	return strings.Join(parts, ", ")
}

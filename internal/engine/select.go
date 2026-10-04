package engine

import (
	"sort"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
)

// This file runs SELECT the simplest way that is correct: every step
// materialises its whole result, joins are nested loops, and nothing is
// reordered. It is the reference the planner (milestone 6) and the iterator
// executor (milestone 7) will be measured against, and the semantics settled
// here — scoping, grouping rules, NULL ordering — stay.

// selectPlan is a SELECT ready to run.
type selectPlan struct {
	cols  []pgwire.Column
	types []sql.Type
	// run executes the query. cx is the environment of the enclosing query
	// level (for a top-level query, one with no row): it supplies the
	// context, the parameters and the outer rows a correlated subquery
	// refers to.
	run func(cx *env) ([][]any, error)
}

// maxJoinRows caps what a join may materialise. Joins run in the order they
// are written and filter only afterwards, so "FROM a, b, c, d, e WHERE ..."
// builds the full cross product first. Until the planner pushes predicates
// down and reorders joins (milestone 6), failing is better than exhausting
// the machine's memory.
const maxJoinRows = 2_000_000

// relation is the result of planning a FROM clause.
type relation struct {
	cols []scopeCol
	rows func(cx *env) ([][]any, error)
}

func (s *Session) planFrom(te sql.TableExpr, parent *scope, ptypes []sql.Type) (*relation, error) {
	switch te := te.(type) {
	case *sql.TableRef:
		s.db.mu.RLock()
		t, err := s.db.lookup(*te)
		s.db.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		return &relation{cols: t.scopeCols(te.Alias), rows: func(cx *env) ([][]any, error) {
			return s.db.snapshot(t, cx.locked)
		}}, nil

	case *sql.DerivedTable:
		// A subquery in FROM sees the enclosing query's outer scope, but
		// not its sibling FROM items.
		plan, err := s.planSelect(te.Select, parent, ptypes)
		if err != nil {
			return nil, err
		}
		cols := make([]scopeCol, len(plan.cols))
		for i, c := range plan.cols {
			cols[i] = scopeCol{table: te.Alias, name: c.Name, typ: plan.types[i]}
		}
		return &relation{cols: cols, rows: plan.run}, nil

	case *sql.Join:
		left, err := s.planFrom(te.Left, parent, ptypes)
		if err != nil {
			return nil, err
		}
		right, err := s.planFrom(te.Right, parent, ptypes)
		if err != nil {
			return nil, err
		}
		cols := append(append([]scopeCol(nil), left.cols...), right.cols...)
		b := &binder{sess: s, scope: &scope{cols: cols, parent: parent}, ptypes: ptypes}
		on, err := b.bindWhere(te.On, "JOIN/ON")
		if err != nil {
			return nil, err
		}
		outer, nLeft := te.Kind == sql.LeftJoin, len(left.cols)
		return &relation{cols: cols, rows: func(cx *env) ([][]any, error) {
			lrows, err := left.rows(cx)
			if err != nil {
				return nil, err
			}
			rrows, err := right.rows(cx)
			if err != nil {
				return nil, err
			}
			en := &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked}
			var out [][]any
			steps := 0
			buf := make([]any, len(cols))
			for _, l := range lrows {
				if err := cx.ctx.Err(); err != nil {
					return nil, err
				}
				copy(buf, l)
				matched := false
				for _, r := range rrows {
					if steps++; steps&0xfff == 0 {
						if err := cx.ctx.Err(); err != nil {
							return nil, err
						}
					}
					if len(out) > maxJoinRows {
						return nil, pgerr.New(pgerr.ProgramLimitExceeded,
							"join produced more than %d intermediate rows", maxJoinRows)
					}
					copy(buf[nLeft:], r)
					en.row = buf
					ok, err := matches(on, en)
					if err != nil {
						return nil, err
					}
					if ok {
						matched = true
						out = append(out, buf)
						buf = make([]any, len(cols))
						copy(buf, l)
					}
				}
				// A left join keeps unmatched left rows, padded with NULLs.
				if outer && !matched {
					padded := make([]any, len(cols))
					copy(padded, l)
					out = append(out, padded)
				}
			}
			return out, nil
		}}, nil
	}
	return nil, pgerr.New(pgerr.InternalError, "unhandled FROM item %T", te)
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
	// FROM. Without one a query produces exactly one row with no columns.
	rel := &relation{rows: func(*env) ([][]any, error) { return [][]any{nil}, nil }}
	if n.From != nil {
		var err error
		if rel, err = s.planFrom(n.From, parent, ptypes); err != nil {
			return nil, err
		}
	}
	sc := &scope{cols: rel.cols, parent: parent}
	b := &binder{sess: s, scope: sc, ptypes: ptypes}

	where, err := b.bindWhere(n.Where, "WHERE")
	if err != nil {
		return nil, err
	}

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

	nCols, distinct := len(sc.cols), n.Distinct
	plan.run = func(cx *env) ([][]any, error) {
		input, err := rel.rows(cx)
		if err != nil {
			return nil, err
		}
		en := &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked}

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

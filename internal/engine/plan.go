package engine

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/sql"
)

// The planner decides how the FROM and WHERE clauses of a query are
// carried out. It makes three kinds of decision, each of which can change
// the cost of a query by orders of magnitude:
//
//   - Where each condition is checked. The WHERE clause is split at its
//     ANDs, and each piece is applied as early as the tables it mentions
//     allow: a condition on one table filters that table's scan, a
//     condition on two filters their join. Nothing is filtered later than
//     it could be.
//   - How each table is read: all of it, or only the part an index (or the
//     primary key) picks out for the conditions at hand.
//   - In what order tables are joined, and whether a join looks each row up
//     through an index of the other table or compares all pairs.
//
// Choices are made by estimated cost. The estimates come from table
// statistics (stats.go) and are crude; they only need to rank plans.
//
// What the planner leaves alone: it does not reorder across LEFT JOIN, it
// builds only left-deep join trees, and the only join algorithm is the
// nested loop, with or without an index on the inner side. Hash and merge
// joins belong to the executor milestone.

// planNode is a step of a plan. It serves both execution, through run, and
// EXPLAIN, through everything else.
type planNode struct {
	op    string   // "Seq Scan on emp e", "Nested Loop", ...
	lines []string // "Filter: ...", "Index Cond: ..."
	kids  []*planNode
	// rows and cost are the planner's estimates.
	rows, cost float64
	run        func(cx *env) ([][]any, error)

	// What actually happened, for EXPLAIN ANALYZE.
	loops, actual int
	elapsed       time.Duration
}

// exec runs the node and records what it did.
func (n *planNode) exec(cx *env) ([][]any, error) {
	start := time.Now()
	rows, err := n.run(cx)
	n.elapsed += time.Since(start)
	n.loops++
	n.actual += len(rows)
	return rows, err
}

func (n *planNode) reset() {
	n.loops, n.actual, n.elapsed = 0, 0, 0
	for _, k := range n.kids {
		k.reset()
	}
}

// explainOptions selects what EXPLAIN prints.
type explainOptions struct {
	analyze, costs, timing bool
}

// explain renders the plan the way PostgreSQL does: one line per node,
// children indented under an arrow.
func (n *planNode) explain(opt explainOptions) []string {
	var out []string
	n.write(&out, "", true, opt)
	return out
}

func (n *planNode) write(out *[]string, indent string, root bool, opt explainOptions) {
	head, body := indent, indent+"  "
	if !root {
		head, body = indent+"->  ", indent+"      "
	}
	line := head + n.op
	if opt.costs {
		line += fmt.Sprintf("  (cost=%.2f rows=%.0f)", n.cost, math.Max(math.Round(n.rows), 1))
	}
	if opt.analyze {
		if n.loops == 0 {
			line += " (never executed)"
		} else {
			line += fmt.Sprintf(" (actual rows=%d loops=%d", n.actual, n.loops)
			if opt.timing {
				line += fmt.Sprintf(" time=%.3f ms", float64(n.elapsed.Microseconds())/1000)
			}
			line += ")"
		}
	}
	*out = append(*out, line)
	for _, l := range n.lines {
		*out = append(*out, body+l)
	}
	for _, k := range n.kids {
		k.write(out, body, false, opt)
	}
}

// Costs, in arbitrary units where reading one row of a scan is 1.
const (
	costSeqRow   = 1.0  // a row read by a sequential scan
	costKeyRow   = 1.2  // a row read through the primary key: adjacent in the tree
	costIndexRow = 4.0  // a row read through a secondary index: a lookup in the table per row
	costDescent  = 3.0  // finding the starting point in a B+tree
	costPair     = 0.25 // comparing one pair of rows in a nested loop
)

// bits is a set of relations of a join group.
type bits [2]uint64

const maxGroupItems = 128

func bit(i int) (b bits)            { b[i/64] = 1 << (i % 64); return b }
func (b bits) or(o bits) bits       { return bits{b[0] | o[0], b[1] | o[1]} }
func (b bits) has(i int) bool       { return b[i/64]&(1<<(i%64)) != 0 }
func (b bits) empty() bool          { return b[0] == 0 && b[1] == 0 }
func (b bits) within(o bits) bool   { return b[0]&^o[0] == 0 && b[1]&^o[1] == 0 }
func (b bits) overlaps(o bits) bool { return b[0]&o[0] != 0 || b[1]&o[1] != 0 }

// relItem is one relation of a FROM clause: a table, a subquery, or a LEFT
// JOIN taken as a whole. Its columns occupy a fixed segment of the row that
// the FROM clause produces, in the order the query was written — which is
// what lets expressions be compiled once, against that layout, and
// evaluated at any stage of any join order: a partial row simply has the
// segments of the relations not joined yet still empty.
type relItem struct {
	off, width int

	table *table
	alias string
	ps    planStats

	derived *selectPlan
	unit    *joinUnit
}

func (it *relItem) covers(col int) bool { return col >= it.off && col < it.off+it.width }

func (it *relItem) label() string {
	if it.table == nil {
		return it.alias
	}
	if it.alias != it.table.name {
		return it.table.name + " " + it.alias
	}
	return it.table.name
}

// joinGroup is a set of relations joined by inner joins — written with
// commas, CROSS JOIN or INNER JOIN, it makes no difference — together with
// the conditions that connect them. Within a group the planner is free to
// choose any order.
type joinGroup struct {
	items []*relItem
	conds []sql.Expr
}

func (g *joinGroup) span() (off, end int) {
	off, end = g.items[0].off, g.items[0].off+g.items[0].width
	for _, it := range g.items[1:] {
		off, end = min(off, it.off), max(end, it.off+it.width)
	}
	return off, end
}

// joinUnit is a LEFT JOIN. Its two sides are planned on their own and it
// takes part in the enclosing group as a single relation: rows cannot be
// moved across an outer join without changing the result.
type joinUnit struct {
	left, right *joinGroup
	on          []sql.Expr
}

// conjuncts splits a condition at its top-level ANDs.
func conjuncts(e sql.Expr) []sql.Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*sql.Binary); ok && b.Op == "and" {
		return append(conjuncts(b.L), conjuncts(b.R)...)
	}
	return []sql.Expr{e}
}

// planner plans one FROM clause.
type planner struct {
	s      *Session
	b      *binder // scope: every column of the FROM clause, in written order
	cols   []scopeCol
	owners []*relItem // for each column, the base table it belongs to, if any
	conjs  map[sql.Expr]*conj
}

// conj is one AND-ed piece of a condition, analysed.
type conj struct {
	expr sql.Expr
	eval evalFn
	// cols lists the columns of this FROM clause the condition mentions.
	cols []int
	// opaque is set if it contains a subquery, which might refer to
	// anything; such a condition is not used to drive an index.
	opaque bool
}

func (p *planner) analyse(e sql.Expr) (*conj, error) {
	if c := p.conjs[e]; c != nil {
		return c, nil
	}
	eval, err := p.b.bindWhere(e, "WHERE")
	if err != nil {
		return nil, err
	}
	c := &conj{expr: e, eval: eval}
	c.cols, c.opaque = p.refs(e)
	p.conjs[e] = c
	return c, nil
}

// refs lists the columns of this FROM clause that e mentions, and reports
// whether e contains a subquery.
func (p *planner) refs(e sql.Expr) (cols []int, opaque bool) {
	sql.WalkExpr(e, func(x sql.Expr) bool {
		switch x := x.(type) {
		case *sql.ColumnRef:
			if i, err := p.b.scope.find(x); err == nil && i >= 0 {
				cols = append(cols, i)
			}
		case *sql.SubqueryExpr, *sql.Exists:
			opaque = true
		case *sql.In:
			opaque = opaque || x.Sub != nil
		}
		return true
	})
	return cols, opaque
}

// maskOf returns which items of the group a set of columns belongs to.
func maskOf(g *joinGroup, cols []int) (m bits) {
	for _, col := range cols {
		for i, it := range g.items {
			if it.covers(col) {
				m = m.or(bit(i))
				break
			}
		}
	}
	return m
}

// ---- building the groups ----

type fromBuilder struct {
	s      *Session
	parent *scope
	ptypes []sql.Type
	cols   []scopeCol
	owners []*relItem
}

func (fb *fromBuilder) add(it *relItem, cols []scopeCol) *joinGroup {
	it.off, it.width = len(fb.cols), len(cols)
	fb.cols = append(fb.cols, cols...)
	for range cols {
		fb.owners = append(fb.owners, it)
	}
	return &joinGroup{items: []*relItem{it}}
}

func (fb *fromBuilder) build(te sql.TableExpr) (*joinGroup, error) {
	switch te := te.(type) {
	case *sql.TableRef:
		fb.s.db.mu.RLock()
		t, err := fb.s.db.lookup(*te)
		var ps planStats
		if err == nil {
			ps = t.forPlanning()
		}
		fb.s.db.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		return fb.add(&relItem{table: t, alias: te.Alias, ps: ps}, t.scopeCols(te.Alias)), nil

	case *sql.DerivedTable:
		// A subquery in FROM sees the enclosing query's outer scope, but
		// not its sibling FROM items.
		plan, err := fb.s.planSelect(te.Select, fb.parent, fb.ptypes)
		if err != nil {
			return nil, err
		}
		cols := make([]scopeCol, len(plan.cols))
		for i, c := range plan.cols {
			cols[i] = scopeCol{table: te.Alias, name: c.Name, typ: plan.types[i]}
		}
		return fb.add(&relItem{derived: plan, alias: te.Alias}, cols), nil

	case *sql.Join:
		left, err := fb.build(te.Left)
		if err != nil {
			return nil, err
		}
		right, err := fb.build(te.Right)
		if err != nil {
			return nil, err
		}
		if te.Kind == sql.LeftJoin {
			off, _ := left.span()
			_, end := right.span()
			unit := &relItem{off: off, width: end - off, alias: "left join",
				unit: &joinUnit{left: left, right: right, on: conjuncts(te.On)}}
			return &joinGroup{items: []*relItem{unit}}, nil
		}
		// An inner join's ON is just more conditions on the merged group.
		left.items = append(left.items, right.items...)
		left.conds = append(append(left.conds, right.conds...), conjuncts(te.On)...)
		return left, nil
	}
	return nil, pgerr.New(pgerr.InternalError, "unhandled FROM item %T", te)
}

// ---- estimating ----

// tableCol maps a column of the FROM clause to the base table column it
// is, if it is one.
func (p *planner) tableCol(col int) (it *relItem, tcol int, ok bool) {
	it = p.owners[col]
	if it == nil || it.table == nil {
		return nil, 0, false
	}
	return it, col - it.off, true
}

// distinct estimates the number of distinct values of a FROM column.
func (p *planner) distinct(col int) float64 {
	if it, tcol, ok := p.tableCol(col); ok {
		return it.ps.distinct(it.table, tcol)
	}
	return 10
}

func (p *planner) columnOf(e sql.Expr) (int, bool) {
	ref, ok := e.(*sql.ColumnRef)
	if !ok {
		return 0, false
	}
	i, err := p.b.scope.find(ref)
	return i, err == nil && i >= 0
}

// selectivity estimates the fraction of rows (or of pairs of rows, for a
// join condition) that satisfy e.
func (p *planner) selectivity(e sql.Expr) float64 {
	switch e := e.(type) {
	case *sql.Binary:
		l, lcol := p.columnOf(e.L)
		r, rcol := p.columnOf(e.R)
		switch e.Op {
		case "and":
			return p.selectivity(e.L) * p.selectivity(e.R)
		case "or":
			a, b := p.selectivity(e.L), p.selectivity(e.R)
			return a + b - a*b
		case "=", "<>":
			eq := 0.1
			switch {
			case lcol && rcol:
				// A join on equal values: each value of the side with
				// fewer distinct values matches its share of the other.
				eq = 1 / math.Max(p.distinct(l), p.distinct(r))
			case lcol:
				eq = 1 / p.distinct(l)
			case rcol:
				eq = 1 / p.distinct(r)
			}
			if e.Op == "<>" {
				return 1 - eq
			}
			return eq
		case "<", "<=", ">", ">=":
			// col op literal, with the column's range known.
			col, lit, op := l, e.R, e.Op
			if !lcol && rcol {
				col, lit, op = r, e.L, flipOp(e.Op)
			} else if !lcol {
				return defaultRangeSel
			}
			if it, tcol, ok := p.tableCol(col); ok && it.ps.stats != nil && tcol < len(it.ps.stats.cols) {
				if l, isLit := lit.(*sql.Literal); isLit && l.Val != nil && l.Type.IsNumeric() {
					return it.ps.stats.cols[tcol].rangeSelectivity(op, toFloat(l.Val))
				}
			}
			return defaultRangeSel
		}
	case *sql.Unary:
		if e.Op == "not" {
			return 1 - p.selectivity(e.X)
		}
	case *sql.IsNull:
		frac := 0.05
		if col, ok := p.columnOf(e.X); ok {
			if it, tcol, ok := p.tableCol(col); ok && it.ps.stats != nil && tcol < len(it.ps.stats.cols) {
				frac = it.ps.stats.cols[tcol].nullFrac
			}
		}
		if e.Not {
			return 1 - frac
		}
		return math.Max(frac, 0.001)
	case *sql.In:
		sel := defaultSel
		if col, ok := p.columnOf(e.X); ok && e.Sub == nil {
			sel = math.Min(float64(len(e.List))/p.distinct(col), 1)
		}
		if e.Not {
			return 1 - sel
		}
		return sel
	case *sql.Between:
		if e.Not {
			return 0.75
		}
		return 0.25
	case *sql.Like:
		if e.Not {
			return 1 - defaultLikeSel
		}
		return defaultLikeSel
	}
	return defaultSel
}

func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

// ---- access paths ----

// sarg is a condition in a form an index can use: a column of a table
// compared with something that does not depend on that table's row.
// ("Search argument", from System R.)
type sarg struct {
	c     *conj
	col   int // column within the table
	op    string
	other sql.Expr
	// needs lists the FROM columns the other side mentions: none for a
	// constant, columns of other tables for a join condition.
	needs []int
}

// sargs extracts the search arguments c offers for the table item it.
func (p *planner) sargs(c *conj, it *relItem) []sarg {
	if c.opaque {
		return nil
	}
	var out []sarg
	try := func(colExpr, other sql.Expr, op string) {
		col, ok := p.columnOf(colExpr)
		if !ok || !it.covers(col) {
			return
		}
		needs, opaque := p.refs(other)
		for _, n := range needs {
			if it.covers(n) {
				return // both sides depend on this table's row
			}
		}
		if !opaque {
			out = append(out, sarg{c: c, col: col - it.off, op: op, other: other, needs: needs})
		}
	}
	switch e := c.expr.(type) {
	case *sql.Binary:
		switch e.Op {
		case "=", "<", "<=", ">", ">=":
			try(e.L, e.R, e.Op)
			try(e.R, e.L, flipOp(e.Op))
		}
	case *sql.Between:
		if !e.Not {
			try(e.X, e.Lo, ">=")
			try(e.X, e.Hi, "<=")
		}
	}
	return out
}

// keyPart computes one value of a lookup key.
type keyPart struct {
	eval    evalFn
	toFloat bool // the column is double precision and the value may be an integer
}

func (k keyPart) encode(en *env) ([]byte, error) {
	v, err := k.eval(en)
	if err != nil || v == nil {
		return nil, err
	}
	if i, ok := v.(int64); ok && k.toFloat {
		v = float64(i)
	}
	return appendKeyPart(nil, v), nil
}

// keyPart compiles the other side of a search argument, if its type can be
// compared with the column through the index's key encoding.
func (p *planner) keyPart(sa sarg, col column) (keyPart, bool) {
	be, err := p.b.bind(sa.other, col.typ)
	if err != nil || !sameKeyFamily(col.typ, be.typ) {
		return keyPart{}, false
	}
	return keyPart{eval: be.eval, toFloat: col.typ == sql.Float8}, true
}

// access is a way of reading rows of a table through its primary key or
// an index: equalities on the leading columns, then at most one range.
type access struct {
	item   *relItem
	index  *index // nil for the primary key
	eq     []keyPart
	lo, hi *keyPart
	loIncl bool
	hiIncl bool
	// used are the conditions the access relies on, for EXPLAIN.
	used []sql.Expr
	// unique is set when the equalities cover a whole unique key.
	unique bool
	// rows estimates how many rows one use of the access returns.
	rows, cost float64
}

func (a *access) name() string {
	if a.index == nil {
		return a.item.table.name + "_pkey"
	}
	return a.index.name
}

// fetch reads the rows the access selects, given the values en supplies.
func (a *access) fetch(s *Session, en *env) ([]rowRef, error) {
	var kb keyBounds
	for _, k := range a.eq {
		part, err := k.encode(en)
		if err != nil {
			return nil, err
		}
		if part == nil {
			return nil, nil // col = NULL is never true
		}
		kb.prefix = append(kb.prefix, part...)
	}
	for _, bound := range []struct {
		k    *keyPart
		dst  *[]byte
		incl bool
	}{{a.lo, &kb.lo, a.loIncl}, {a.hi, &kb.hi, a.hiIncl}} {
		if bound.k == nil {
			continue
		}
		part, err := bound.k.encode(en)
		if err != nil {
			return nil, err
		}
		if part == nil {
			return nil, nil
		}
		*bound.dst = part
	}
	kb.loIncl, kb.hiIncl = a.loIncl, a.hiIncl
	return s.db.rangeScan(a.item.table, a.index, s.snap, kb, en.locked)
}

// bestAccess finds the cheapest index access to a table for the given
// search arguments, or nil if none is usable. available says whether a
// search argument's other side can be computed where the access would be
// used.
func (p *planner) bestAccess(it *relItem, sargs []sarg, available func(sarg) bool) *access {
	if it.table == nil || p.s.vars["enable_indexscan"] == "off" {
		return nil
	}
	t := it.table
	type candidate struct {
		index *index
		cols  []int
		uniq  bool
	}
	var cands []candidate
	if t.pk != nil {
		cands = append(cands, candidate{nil, t.pk, true})
	}
	for _, ix := range it.ps.indexes {
		cands = append(cands, candidate{ix, ix.cols, ix.unique})
	}

	var best *access
	for _, cand := range cands {
		a := &access{item: it, index: cand.index}
		sel := 1.0
		for _, col := range cand.cols {
			found := false
			for _, sa := range sargs {
				if sa.col != col || sa.op != "=" || !available(sa) {
					continue
				}
				if k, ok := p.keyPart(sa, t.cols[col]); ok {
					a.eq = append(a.eq, k)
					a.used = append(a.used, sa.c.expr)
					sel /= it.ps.distinct(t, col)
					found = true
					break
				}
			}
			if !found {
				// No equality on this column: a range on it can still
				// narrow the scan, and that is where the key stops
				// being useful.
				for _, sa := range sargs {
					if sa.col != col || sa.op == "=" || !available(sa) {
						continue
					}
					k, ok := p.keyPart(sa, t.cols[col])
					if !ok {
						continue
					}
					switch {
					case (sa.op == ">" || sa.op == ">=") && a.lo == nil:
						a.lo, a.loIncl = &k, sa.op == ">="
					case (sa.op == "<" || sa.op == "<=") && a.hi == nil:
						a.hi, a.hiIncl = &k, sa.op == "<="
					default:
						continue
					}
					a.used = append(a.used, sa.c.expr)
					sel *= p.selectivity(sa.c.expr)
				}
				break
			}
		}
		if len(a.eq) == 0 && a.lo == nil && a.hi == nil {
			continue
		}
		a.unique = cand.uniq && len(a.eq) == len(cand.cols)
		a.rows = math.Max(it.ps.rows*sel, 1)
		if a.unique {
			a.rows = 1
		}
		perRow := costIndexRow
		if cand.index == nil {
			perRow = costKeyRow
		}
		a.cost = costDescent + a.rows*perRow
		if best == nil || a.cost < best.cost {
			best = a
		}
	}
	return best
}

// without returns the conditions in all that are not among covered: what
// remains to be checked once an index has selected the rows.
func without(all, covered []sql.Expr) []sql.Expr {
	var rest []sql.Expr
	for _, e := range all {
		found := false
		for _, c := range covered {
			found = found || c == e
		}
		if !found {
			rest = append(rest, e)
		}
	}
	return rest
}

// exprList renders conditions for EXPLAIN, each one once.
func exprList(exprs []sql.Expr) string {
	var parts []string
	seen := make(map[sql.Expr]bool)
	for _, e := range exprs {
		if !seen[e] {
			seen[e] = true
			parts = append(parts, sql.FormatExpr(e))
		}
	}
	return strings.Join(parts, " AND ")
}

func condEvals(conds []*conj) ([]evalFn, []sql.Expr) {
	evals, exprs := make([]evalFn, len(conds)), make([]sql.Expr, len(conds))
	for i, c := range conds {
		evals[i], exprs[i] = c.eval, c.expr
	}
	return evals, exprs
}

func allMatch(conds []evalFn, en *env) (bool, error) {
	for _, c := range conds {
		if ok, err := matches(c, en); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// scanPlan is the best way found to read one relation on its own.
type scanPlan struct {
	node *planNode
	// rawRows is the size of the relation before its own conditions.
	rawRows float64
	local   []*conj
	sargs   []sarg
}

// planScan plans reading one relation, applying the conditions that
// mention it alone.
func (p *planner) planScan(it *relItem, local []*conj) (*scanPlan, error) {
	width := len(p.cols)
	filters, filterExprs := condEvals(local)
	sel := 1.0
	for _, c := range local {
		sel *= p.selectivity(c.expr)
	}
	// widen places a relation's own row in its segment of the full row and
	// applies the relation's conditions.
	widen := func(cx *env, rows [][]any) ([][]any, error) {
		en := &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked}
		out := make([][]any, 0, len(rows))
		for _, r := range rows {
			full := make([]any, width)
			copy(full[it.off:], r)
			en.row = full
			if ok, err := allMatch(filters, en); err != nil {
				return nil, err
			} else if ok {
				out = append(out, full)
			}
		}
		return out, nil
	}
	sp := &scanPlan{local: local}
	node := &planNode{}
	filterLine := func(covered []sql.Expr) {
		if rest := without(filterExprs, covered); len(rest) > 0 {
			node.lines = append(node.lines, "Filter: "+exprList(rest))
		}
	}

	switch {
	case it.unit != nil:
		return p.planUnit(it, local)

	case it.derived != nil:
		sub := it.derived
		sp.rawRows = sub.root.rows
		node.op = "Subquery Scan on " + it.alias
		filterLine(nil)
		node.kids = []*planNode{sub.root}
		node.rows, node.cost = math.Max(sp.rawRows*sel, 1), sub.root.cost+sp.rawRows*costSeqRow*0.1
		node.run = func(cx *env) ([][]any, error) {
			rows, err := sub.run(cx)
			if err != nil {
				return nil, err
			}
			return widen(cx, rows)
		}

	default:
		t := it.table
		sp.rawRows = it.ps.rows
		for _, c := range local {
			sp.sargs = append(sp.sargs, p.sargs(c, it)...)
		}
		node.rows = math.Max(sp.rawRows*sel, 1)
		seqCost := sp.rawRows * costSeqRow
		// Only conditions on constants can drive a scan that stands alone.
		acc := p.bestAccess(it, sp.sargs, func(sa sarg) bool { return len(sa.needs) == 0 })
		if acc != nil && acc.cost < seqCost {
			node.op = fmt.Sprintf("Index Scan using %s on %s", acc.name(), it.label())
			node.lines = []string{"Index Cond: " + exprList(acc.used)}
			filterLine(acc.used)
			node.cost = acc.cost
			node.rows = math.Min(node.rows, acc.rows)
			node.run = func(cx *env) ([][]any, error) {
				refs, err := acc.fetch(p.s, &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked})
				if err != nil {
					return nil, err
				}
				rows := make([][]any, len(refs))
				for i, r := range refs {
					rows[i] = r.vals
				}
				return widen(cx, rows)
			}
		} else {
			node.op = "Seq Scan on " + it.label()
			filterLine(nil)
			node.cost = seqCost
			node.run = func(cx *env) ([][]any, error) {
				rows, err := p.s.db.rows(t, p.s.snap, cx.locked)
				if err != nil {
					return nil, err
				}
				return widen(cx, rows)
			}
		}
	}
	sp.node = node
	return sp, nil
}

// planUnit plans a LEFT JOIN. Conditions from outside that mention only
// its left side are pushed into that side; conditions in its ON clause that
// mention only the right side are pushed into the right side. (Not the
// other way round: an ON condition on the left side does not remove left
// rows, it only stops them matching.)
func (p *planner) planUnit(it *relItem, outside []*conj) (*scanPlan, error) {
	u := it.unit
	loff, lend := u.left.span()
	roff, rend := u.right.span()
	inside := func(cols []int, off, end int) bool {
		for _, c := range cols {
			if c < off || c >= end {
				return false
			}
		}
		return true
	}

	var toLeft, above []sql.Expr
	var aboveConds []*conj
	for _, c := range outside {
		if !c.opaque && inside(c.cols, loff, lend) {
			toLeft = append(toLeft, c.expr)
		} else {
			above = append(above, c.expr)
			aboveConds = append(aboveConds, c)
		}
	}
	var toRight []sql.Expr
	var on []*conj
	for _, e := range u.on {
		c, err := p.analyse(e)
		if err != nil {
			return nil, err
		}
		if !c.opaque && len(c.cols) > 0 && inside(c.cols, roff, rend) {
			toRight = append(toRight, e)
		} else {
			on = append(on, c)
		}
	}

	left, err := p.planGroup(u.left, toLeft)
	if err != nil {
		return nil, err
	}
	onEvals, onExprs := condEvals(on)
	aboveEvals, _ := condEvals(aboveConds)
	node := &planNode{op: "Nested Loop Left Join"}
	if len(onExprs) > 0 {
		node.lines = append(node.lines, "Join Filter: "+exprList(onExprs))
	}
	if len(above) > 0 {
		node.lines = append(node.lines, "Filter: "+exprList(above))
	}
	sel := 1.0
	for _, c := range on {
		sel *= p.selectivity(c.expr)
	}
	width := len(p.cols)
	finish := func(en *env, out *[][]any, row []any) error {
		en.row = row
		ok, err := allMatch(aboveEvals, en)
		if err == nil && ok {
			if len(*out) >= maxJoinRows {
				return errJoinTooLarge()
			}
			*out = append(*out, row)
		}
		return err
	}

	// If the right side is a single table that an index can look up from
	// the left row, do that instead of comparing every pair.
	if len(u.right.items) == 1 && u.right.items[0].table != nil {
		inner := u.right.items[0]
		var sargs []sarg
		for _, c := range on {
			sargs = append(sargs, p.sargs(c, inner)...)
		}
		var rightLocal []*conj
		for _, e := range toRight {
			c, err := p.analyse(e)
			if err != nil {
				return nil, err
			}
			rightLocal = append(rightLocal, c)
			sargs = append(sargs, p.sargs(c, inner)...)
		}
		acc := p.bestAccess(inner, sargs, func(sa sarg) bool { return inside(sa.needs, loff, lend) })
		usesLeft := false
		if acc != nil {
			for _, e := range acc.used {
				if cols, _ := p.refs(e); !inside(cols, roff, rend) {
					usesLeft = true
				}
			}
		}
		if acc != nil && usesLeft && costDescent+acc.rows*costIndexRow < inner.ps.rows*costPair {
			localEvals, localExprs := condEvals(rightLocal)
			probe := &planNode{op: fmt.Sprintf("Index Scan using %s on %s", acc.name(), inner.label()),
				lines: []string{"Index Cond: " + exprList(acc.used)}, rows: acc.rows, cost: acc.cost}
			if len(localExprs) > 0 {
				probe.lines = append(probe.lines, "Filter: "+exprList(localExprs))
			}
			node.kids = []*planNode{left, probe}
			node.rows = math.Max(left.rows, left.rows*acc.rows*sel)
			node.cost = left.cost + left.rows*acc.cost
			node.run = func(cx *env) ([][]any, error) {
				lrows, err := left.exec(cx)
				if err != nil {
					return nil, err
				}
				en := &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked}
				var out [][]any
				for _, l := range lrows {
					if err := cx.ctx.Err(); err != nil {
						return nil, err
					}
					en.row = l
					refs, err := acc.fetch(p.s, en)
					if err != nil {
						return nil, err
					}
					probe.loops++
					matched := false
					for _, r := range refs {
						row := make([]any, width)
						copy(row, l)
						copy(row[inner.off:], r.vals)
						en.row = row
						ok, err := allMatch(localEvals, en)
						if err == nil && ok {
							ok, err = allMatch(onEvals, en)
						}
						if err != nil {
							return nil, err
						}
						if !ok {
							continue
						}
						probe.actual++
						matched = true
						if err := finish(en, &out, row); err != nil {
							return nil, err
						}
					}
					if !matched {
						if err := finish(en, &out, l); err != nil {
							return nil, err
						}
					}
				}
				return out, nil
			}
			return &scanPlan{node: node, rawRows: node.rows}, nil
		}
	}

	right, err := p.planGroup(u.right, toRight)
	if err != nil {
		return nil, err
	}
	node.kids = []*planNode{left, right}
	node.rows = math.Max(left.rows, left.rows*right.rows*sel)
	node.cost = left.cost + right.cost + left.rows*right.rows*costPair
	node.run = func(cx *env) ([][]any, error) {
		lrows, err := left.exec(cx)
		if err != nil {
			return nil, err
		}
		rrows, err := right.exec(cx)
		if err != nil {
			return nil, err
		}
		en := &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked}
		var out [][]any
		buf := make([]any, width)
		steps := 0
		for _, l := range lrows {
			matched := false
			for _, r := range rrows {
				if steps++; steps&0xfff == 0 {
					if err := cx.ctx.Err(); err != nil {
						return nil, err
					}
				}
				copy(buf, l)
				copy(buf[roff:rend], r[roff:rend])
				en.row = buf
				ok, err := allMatch(onEvals, en)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				matched = true
				row := buf
				buf = make([]any, width)
				if err := finish(en, &out, row); err != nil {
					return nil, err
				}
			}
			// A left join keeps unmatched left rows; the right side's
			// columns stay NULL.
			if !matched {
				if err := finish(en, &out, l); err != nil {
					return nil, err
				}
			}
		}
		return out, nil
	}
	return &scanPlan{node: node, rawRows: node.rows}, nil
}

func errJoinTooLarge() error {
	return pgerr.New(pgerr.ProgramLimitExceeded, "join produced more than %d intermediate rows", maxJoinRows)
}

// maxJoinRows caps what a join may materialise. Every step of a plan still
// holds its whole result in memory; until the executor streams rows
// (milestone 7), failing is better than exhausting the machine.
const maxJoinRows = 2_000_000

// ---- join ordering ----

// joinStep is one decision of a join order: add relation item to what has
// been joined so far, either by comparing all pairs or by looking it up
// through acc.
type joinStep struct {
	item       int
	acc        *access
	conds      []*conj // conditions that become checkable at this step
	rows, cost float64
}

// planGroup plans an inner-join group: how to read each relation, and in
// what order to join them.
func (p *planner) planGroup(g *joinGroup, extra []sql.Expr) (*planNode, error) {
	n := len(g.items)
	if n > maxGroupItems {
		return nil, pgerr.New(pgerr.ProgramLimitExceeded, "a FROM clause can join at most %d relations", maxGroupItems)
	}
	// Sort the conditions by the relations they involve.
	var conds []*conj
	masks := map[*conj]bits{}
	local := make([][]*conj, n)
	for _, e := range append(append([]sql.Expr(nil), g.conds...), extra...) {
		c, err := p.analyse(e)
		if err != nil {
			return nil, err
		}
		m := maskOf(g, c.cols)
		if c.opaque {
			// A subquery may refer to any column of this FROM clause
			// without that showing in the condition itself, so it is
			// only safe to evaluate once every relation is in place.
			for i := 0; i < n; i++ {
				m = m.or(bit(i))
			}
		}
		single := -1
		for i := 0; i < n; i++ {
			if m.within(bit(i)) && !m.empty() {
				single = i
			}
		}
		switch {
		case m.empty():
			// Mentions no relation at all (a constant, a parameter, an
			// outer column): filter the first scan with it, so that a
			// false one empties the whole join at once.
			local[0] = append(local[0], c)
		case single >= 0 && !c.opaque:
			local[single] = append(local[single], c)
		case single >= 0:
			local[single] = append(local[single], c)
		default:
			conds = append(conds, c)
			masks[c] = m
		}
	}

	scans := make([]*scanPlan, n)
	for i, it := range g.items {
		sp, err := p.planScan(it, local[i])
		if err != nil {
			return nil, err
		}
		scans[i] = sp
	}
	if n == 1 {
		return scans[0].node, nil
	}

	// extend computes the step that adds relation i to the set joined.
	extend := func(joined bits, rows, cost float64, i int) joinStep {
		it := g.items[i]
		now := joined.or(bit(i))
		step := joinStep{item: i}
		sel := 1.0
		var sargs []sarg
		for _, c := range conds {
			m := masks[c]
			if m.within(now) && !m.within(joined) {
				step.conds = append(step.conds, c)
				sel *= p.selectivity(c.expr)
				if it.table != nil {
					sargs = append(sargs, p.sargs(c, it)...)
				}
			}
		}
		sp := scans[i]
		step.rows = math.Max(rows*sp.node.rows*sel, 0.01)
		// Every row a step produces is work for the steps after it, so
		// producing rows has a cost of its own. Without it an order that
		// builds a cross product and filters it later would look as cheap
		// as one that follows the join conditions.
		emit := step.rows * costSeqRow
		// Compare all pairs: the inner relation is read once.
		step.cost = cost + sp.node.cost + rows*sp.node.rows*costPair + emit
		// Or look each outer row up through an index of the inner table,
		// if a join condition gives the key.
		if it.table != nil && len(sargs) > 0 {
			avail := func(sa sarg) bool { return maskOf(g, sa.needs).within(joined) }
			if acc := p.bestAccess(it, append(sargs, sp.sargs...), avail); acc != nil {
				usesOuter := false
				for _, e := range acc.used {
					for _, c := range step.conds {
						usesOuter = usesOuter || c.expr == e
					}
				}
				if lookup := cost + rows*acc.cost + emit; usesOuter && lookup < step.cost {
					step.cost, step.acc = lookup, acc
				}
			}
		}
		return step
	}

	var order []joinStep
	reorder := p.s.vars["join_collapse_limit"] != "1"
	switch {
	case !reorder:
		// As written.
		order = []joinStep{{item: 0, rows: scans[0].node.rows, cost: scans[0].node.cost}}
		joined := bit(0)
		for i := 1; i < n; i++ {
			last := order[len(order)-1]
			order = append(order, extend(joined, last.rows, last.cost, i))
			joined = joined.or(bit(i))
		}
	case n <= 10:
		order = p.orderExhaustive(n, scans, extend)
	default:
		order = p.orderGreedy(n, scans, extend)
	}

	// Build the left-deep tree the order describes.
	width := len(p.cols)
	node := scans[order[0].item].node
	for _, step := range order[1:] {
		it, sp, outer := g.items[step.item], scans[step.item], node
		evals, exprs := condEvals(step.conds)
		join := &planNode{op: "Nested Loop", rows: step.rows, cost: step.cost}

		if acc := step.acc; acc != nil {
			localEvals, localExprs := condEvals(sp.local)
			probe := &planNode{op: fmt.Sprintf("Index Scan using %s on %s", acc.name(), it.label()),
				lines: []string{"Index Cond: " + exprList(acc.used)}, rows: acc.rows, cost: acc.cost}
			// Whatever the index condition does not already guarantee.
			var rest []sql.Expr
			for _, e := range append(append([]sql.Expr(nil), exprs...), localExprs...) {
				covered := false
				for _, u := range acc.used {
					covered = covered || u == e
				}
				if !covered {
					rest = append(rest, e)
				}
			}
			if len(rest) > 0 {
				probe.lines = append(probe.lines, "Filter: "+exprList(rest))
			}
			join.kids = []*planNode{outer, probe}
			join.run = func(cx *env) ([][]any, error) {
				orows, err := outer.exec(cx)
				if err != nil {
					return nil, err
				}
				en := &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked}
				var out [][]any
				for _, o := range orows {
					if err := cx.ctx.Err(); err != nil {
						return nil, err
					}
					en.row = o
					refs, err := acc.fetch(p.s, en)
					if err != nil {
						return nil, err
					}
					probe.loops++
					for _, r := range refs {
						row := make([]any, width)
						copy(row, o)
						copy(row[it.off:], r.vals)
						en.row = row
						ok, err := allMatch(localEvals, en)
						if err == nil && ok {
							ok, err = allMatch(evals, en)
						}
						if err != nil {
							return nil, err
						}
						if ok {
							if len(out) >= maxJoinRows {
								return nil, errJoinTooLarge()
							}
							probe.actual++
							out = append(out, row)
						}
					}
				}
				return out, nil
			}
		} else {
			inner := sp.node
			if len(exprs) > 0 {
				join.lines = []string{"Join Filter: " + exprList(exprs)}
			}
			join.kids = []*planNode{outer, inner}
			join.run = func(cx *env) ([][]any, error) {
				orows, err := outer.exec(cx)
				if err != nil {
					return nil, err
				}
				if len(orows) == 0 {
					return nil, nil // nothing to join with: skip the inner side
				}
				irows, err := inner.exec(cx)
				if err != nil {
					return nil, err
				}
				en := &env{ctx: cx.ctx, params: cx.params, outer: cx, locked: cx.locked}
				var out [][]any
				buf := make([]any, width)
				steps := 0
				for _, o := range orows {
					for _, in := range irows {
						if steps++; steps&0xfff == 0 {
							if err := cx.ctx.Err(); err != nil {
								return nil, err
							}
						}
						copy(buf, o)
						copy(buf[it.off:it.off+it.width], in[it.off:it.off+it.width])
						en.row = buf
						ok, err := allMatch(evals, en)
						if err != nil {
							return nil, err
						}
						if ok {
							if len(out) >= maxJoinRows {
								return nil, errJoinTooLarge()
							}
							out = append(out, buf)
							buf = make([]any, width)
						}
					}
				}
				return out, nil
			}
		}
		node = join
	}
	return node, nil
}

// orderExhaustive finds the cheapest left-deep join order by dynamic
// programming over sets of relations: the best way to join a set is the
// best way to join all but one of its members, extended by that member.
// It considers every order while computing only 2^n subplans.
func (p *planner) orderExhaustive(n int, scans []*scanPlan, extend func(bits, float64, float64, int) joinStep) []joinStep {
	type entry struct {
		ok   bool
		step joinStep
		prev int
	}
	best := make([]entry, 1<<n)
	for i := 0; i < n; i++ {
		best[1<<i] = entry{ok: true, step: joinStep{item: i, rows: scans[i].node.rows, cost: scans[i].node.cost}}
	}
	for set := 1; set < 1<<n; set++ {
		if !best[set].ok {
			continue
		}
		var joined bits
		joined[0] = uint64(set)
		for i := 0; i < n; i++ {
			if set&(1<<i) != 0 {
				continue
			}
			step := extend(joined, best[set].step.rows, best[set].step.cost, i)
			next := set | 1<<i
			if !best[next].ok || step.cost < best[next].step.cost {
				best[next] = entry{ok: true, step: step, prev: set}
			}
		}
	}
	order := make([]joinStep, n)
	for set, i := 1<<n-1, n-1; i >= 0; i-- {
		order[i] = best[set].step
		set = best[set].prev
	}
	return order
}

// orderGreedy handles joins too large for the exhaustive search: start
// from the smallest relation and keep adding whichever is cheapest to add.
func (p *planner) orderGreedy(n int, scans []*scanPlan, extend func(bits, float64, float64, int) joinStep) []joinStep {
	first := 0
	for i := 1; i < n; i++ {
		if scans[i].node.rows < scans[first].node.rows {
			first = i
		}
	}
	order := []joinStep{{item: first, rows: scans[first].node.rows, cost: scans[first].node.cost}}
	joined := bit(first)
	for len(order) < n {
		last := order[len(order)-1]
		var pick *joinStep
		for i := 0; i < n; i++ {
			if joined.has(i) {
				continue
			}
			step := extend(joined, last.rows, last.cost, i)
			if pick == nil || step.cost < pick.cost {
				pick = &step
			}
		}
		order = append(order, *pick)
		joined = joined.or(bit(pick.item))
	}
	return order
}

// planFromWhere plans a FROM clause together with the WHERE clause that
// filters it. It returns the columns the FROM clause produces, in written
// order, and the plan that produces the rows satisfying WHERE.
func (s *Session) planFromWhere(from sql.TableExpr, where sql.Expr, parent *scope, ptypes []sql.Type) ([]scopeCol, *planNode, error) {
	fb := &fromBuilder{s: s, parent: parent, ptypes: ptypes}
	g, err := fb.build(from)
	if err != nil {
		return nil, nil, err
	}
	p := &planner{
		s:      s,
		b:      &binder{sess: s, scope: &scope{cols: fb.cols, parent: parent}, ptypes: ptypes},
		cols:   fb.cols,
		owners: fb.owners,
		conjs:  make(map[sql.Expr]*conj),
	}
	// Bind the whole condition once first, so that a type error is
	// reported about the clause as written rather than about a fragment.
	if _, err := p.b.bindWhere(where, "WHERE"); err != nil {
		return nil, nil, err
	}
	node, err := p.planGroup(g, conjuncts(where))
	return fb.cols, node, err
}

// targetScan is how an UPDATE or DELETE finds the rows it applies to. It is
// the same choice a SELECT from one table faces — scan, or go through an
// index — so that changing one row by its key does not read the table.
type targetScan struct {
	node  *planNode
	fetch func(en *env) ([]rowRef, error)
}

func (s *Session) planTarget(op string, t *table, ref sql.TableRef, where sql.Expr, ptypes []sql.Type) (*targetScan, error) {
	s.db.mu.RLock()
	ps := t.forPlanning()
	s.db.mu.RUnlock()

	it := &relItem{table: t, alias: ref.Alias, ps: ps, width: len(t.cols)}
	cols := t.scopeCols(ref.Alias)
	owners := make([]*relItem, len(cols))
	for i := range owners {
		owners[i] = it
	}
	p := &planner{
		s:      s,
		b:      &binder{sess: s, scope: &scope{cols: cols}, ptypes: ptypes},
		cols:   cols,
		owners: owners,
		conjs:  make(map[sql.Expr]*conj),
	}
	var conds []*conj
	var sargs []sarg
	sel := 1.0
	for _, e := range conjuncts(where) {
		c, err := p.analyse(e)
		if err != nil {
			return nil, err
		}
		conds = append(conds, c)
		sargs = append(sargs, p.sargs(c, it)...)
		sel *= p.selectivity(e)
	}
	filters, filterExprs := condEvals(conds)

	scan := &planNode{op: "Seq Scan on " + it.label(), rows: math.Max(ps.rows*sel, 1), cost: ps.rows * costSeqRow}
	var covered []sql.Expr
	acc := p.bestAccess(it, sargs, func(sa sarg) bool { return len(sa.needs) == 0 })
	if acc != nil && acc.cost < scan.cost {
		scan.op = fmt.Sprintf("Index Scan using %s on %s", acc.name(), it.label())
		scan.lines = []string{"Index Cond: " + exprList(acc.used)}
		covered = acc.used
		scan.rows, scan.cost = math.Min(scan.rows, acc.rows), acc.cost
	} else {
		acc = nil
	}
	if rest := without(filterExprs, covered); len(rest) > 0 {
		scan.lines = append(scan.lines, "Filter: "+exprList(rest))
	}
	top := &planNode{op: op + " on " + t.name, kids: []*planNode{scan}, rows: scan.rows, cost: scan.cost}

	return &targetScan{node: top, fetch: func(en *env) ([]rowRef, error) {
		start := time.Now()
		var refs []rowRef
		var err error
		if acc != nil {
			refs, err = acc.fetch(s, en)
		} else {
			refs, err = s.db.scan(t, s.snap)
		}
		if err != nil {
			return nil, err
		}
		kept := refs[:0]
		for _, r := range refs {
			en.row = r.vals
			ok, err := allMatch(filters, en)
			if err != nil {
				return nil, err
			}
			if ok {
				kept = append(kept, r)
			}
		}
		en.row = nil
		scan.loops++
		scan.actual += len(kept)
		scan.elapsed += time.Since(start)
		top.loops++
		return kept, nil
	}}, nil
}

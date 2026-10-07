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
//   - How each join is carried out: by looking the inner rows up through an
//     index, by hashing, by merging two sorted inputs, or by comparing
//     every pair (join.go has what each costs).
//
// What the planner leaves alone: it does not reorder across an outer join,
// and it builds only left-deep join trees.

// planNode is a step of a plan. It serves both execution, through open, and
// EXPLAIN, through everything else.
type planNode struct {
	op    string   // "Seq Scan on emp e", "Nested Loop", ...
	lines []string // "Filter: ...", "Index Cond: ..."
	kids  []*planNode
	// rows and cost are the planner's estimates.
	rows, cost float64
	// open starts the step and returns the iterator over its rows.
	open func(cx *env) (iter, error)
	// order lists the FROM columns the rows come out sorted by, ascending,
	// most significant first; nil if they come in no useful order. A table
	// is stored in primary key order and an index in the order of its
	// columns, so a scan is sorted for free: a merge join can skip its
	// sort, and an ORDER BY that asks for the same order needs none.
	// orderedBy is 1 + the first of those columns, or 0.
	order     []int
	orderedBy int

	// What actually happened, for EXPLAIN ANALYZE.
	loops, actual int
	elapsed       time.Duration
	// info holds what the step has to say about its own execution ("Sort
	// Method: ...").
	info []string
}

func (n *planNode) reset() {
	n.loops, n.actual, n.elapsed, n.info = 0, 0, 0, nil
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
	if opt.analyze {
		for _, l := range n.info {
			*out = append(*out, body+l)
		}
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
	costHashRow  = 1.5  // putting a row in a hash table
	costProbe    = 0.5  // looking a row up in a hash table
	costMergeRow = 0.3  // advancing one row in a merge
	costSortCmp  = 0.2  // one comparison of a sort
)

// disabledCost is added to the cost of a join method that has been switched
// off but may still be the only one possible, so that it is chosen last
// rather than never. (PostgreSQL does the same.)
const disabledCost = 1e10

// loopPenalty is what a plain nested loop costs extra in this session.
func (p *planner) loopPenalty() float64 {
	if p.s.vars["enable_nestloop"] == "off" {
		return disabledCost
	}
	return 0
}

// sortCost estimates sorting rows rows.
func sortCost(rows float64) float64 { return rows * math.Log2(rows+2) * costSortCmp }

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
	// star lists the group's columns in the order "*" shows them.
	star []int
}

func (g *joinGroup) span() (off, end int) {
	off, end = g.items[0].off, g.items[0].off+g.items[0].width
	for _, it := range g.items[1:] {
		off, end = min(off, it.off), max(end, it.off+it.width)
	}
	return off, end
}

// joinUnit is an outer join. Its two sides are planned on their own and it
// takes part in the enclosing group as a single relation: rows cannot be
// moved across an outer join without changing the result.
//
// left is the side whose rows are all kept; for a RIGHT JOIN that is the
// side written on the right. Which side is which does not affect where
// their columns are in the row, which follows the order written.
type joinUnit struct {
	kind        joinKind // joinLeft or joinFull
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
	equis  map[*conj]*equiCond
	// hint is the order the query would like, and hintItem the one
	// relation whose scan can provide it: the only one of the FROM clause.
	hint     *orderHint
	hintItem *relItem
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
		case *colIdx:
			cols = append(cols, x.idx)
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
	g := &joinGroup{items: []*relItem{it}}
	for i := range cols {
		fb.owners = append(fb.owners, it)
		g.star = append(g.star, it.off+i)
	}
	return g
}

// using turns JOIN ... USING (a, b) into the conditions it stands for and
// works out what "*" shows: each named column once, first, then the rest of
// the left side, then the rest of the right. The copy that is not shown
// stays reachable by its table's name.
func (fb *fromBuilder) using(te *sql.Join, left, right *joinGroup) (conds []sql.Expr, star []int, err error) {
	find := func(g *joinGroup, id sql.Ident, side string) (int, error) {
		found := -1
		for _, i := range g.star {
			if fb.cols[i].name != id.Name {
				continue
			}
			if found >= 0 {
				return 0, pgerr.New(pgerr.AmbiguousColumn,
					"common column name %q appears more than once in %s table", id.Name, side).At(id.Pos)
			}
			found = i
		}
		if found < 0 {
			return 0, pgerr.New(pgerr.UndefinedColumn,
				"column %q specified in USING clause does not exist in %s table", id.Name, side).At(id.Pos)
		}
		return found, nil
	}
	merged := map[int]bool{}
	for _, id := range te.Using {
		l, err := find(left, id, "left")
		if err != nil {
			return nil, nil, err
		}
		r, err := find(right, id, "right")
		if err != nil {
			return nil, nil, err
		}
		lc, rc := fb.cols[l], fb.cols[r]
		conds = append(conds, &sql.Binary{Op: "=", Pos: id.Pos,
			L: &sql.ColumnRef{Table: lc.table, Name: lc.name, Pos: id.Pos},
			R: &sql.ColumnRef{Table: rc.table, Name: rc.name, Pos: id.Pos}})
		// The column shown is the one from the side whose rows are all
		// there: in a RIGHT JOIN the left one may be NULL.
		shown, hidden := l, r
		if te.Kind == sql.RightJoin {
			shown, hidden = r, l
		}
		fb.cols[hidden].hidden = true
		merged[l], merged[r] = true, true
		star = append(star, shown)
	}
	for _, i := range append(append([]int(nil), left.star...), right.star...) {
		if !merged[i] {
			star = append(star, i)
		}
	}
	return conds, star, nil
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
		on := conjuncts(te.On)
		star := append(append([]int(nil), left.star...), right.star...)
		if te.Using != nil {
			if te.Kind == sql.FullJoin {
				// Its column would have to be whichever of the two is
				// not NULL, which is neither side's column.
				return nil, pgerr.New(pgerr.FeatureNotSupported, "FULL JOIN ... USING is not supported").At(te.Pos)
			}
			if on, star, err = fb.using(te, left, right); err != nil {
				return nil, err
			}
		}
		if te.Kind == sql.LeftJoin || te.Kind == sql.RightJoin || te.Kind == sql.FullJoin {
			off, _ := left.span()
			_, end := right.span()
			u := &joinUnit{kind: joinLeft, left: left, right: right, on: on}
			switch te.Kind {
			case sql.RightJoin:
				u.left, u.right = right, left
			case sql.FullJoin:
				u.kind = joinFull
			}
			unit := &relItem{off: off, width: end - off, alias: "outer join", unit: u}
			return &joinGroup{items: []*relItem{unit}, star: star}, nil
		}
		// An inner join's ON is just more conditions on the merged group.
		left.items = append(left.items, right.items...)
		left.conds = append(append(left.conds, right.conds...), on...)
		left.star = star
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
	// bounded is set if a condition narrows the range read; an access that
	// is not bounded reads the whole index, which is only worth it for the
	// order the rows come in.
	bounded bool
	// order lists the FROM columns the rows come out sorted by: the key's
	// columns after the ones fixed by an equality, then, for an index, the
	// primary key, which is how entries with equal indexed values are
	// arranged.
	order []int
}

func (a *access) name() string {
	if a.index == nil {
		return a.item.table.name + "_pkey"
	}
	return a.index.name
}

// fetch reads the rows the access selects, given the values en supplies.
func (a *access) fetch(s *Session, en *env) ([]rowRef, error) {
	kb, ok, err := a.bounds(en)
	if err != nil || !ok {
		return nil, err
	}
	return s.db.rangeScan(a.item.table, a.index, en.q.snap, kb, en.locked)
}

// bounds computes the key range the access reads, given the values en
// supplies. It reports false if the range is empty for certain.
func (a *access) bounds(en *env) (kb keyBounds, ok bool, err error) {
	for _, k := range a.eq {
		part, err := k.encode(en)
		if err != nil {
			return kb, false, err
		}
		if part == nil {
			return kb, false, nil // col = NULL is never true
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
			return kb, false, err
		}
		if part == nil {
			return kb, false, nil
		}
		*bound.dst = part
	}
	kb.loIncl, kb.hiIncl = a.loIncl, a.hiIncl
	return kb, true, nil
}

// bestAccess finds the cheapest index access to a table for the given
// search arguments, or nil if none is usable. available says whether a
// search argument's other side can be computed where the access would be
// used.
func (p *planner) bestAccess(it *relItem, sargs []sarg, available func(sarg) bool) *access {
	var best *access
	for _, a := range p.accesses(it, sargs, available) {
		if a.bounded && (best == nil || a.cost < best.cost) {
			best = a
		}
	}
	return best
}

// accesses lists every way of reading a table through its primary key or
// an index, with the search arguments each can use.
func (p *planner) accesses(it *relItem, sargs []sarg, available func(sarg) bool) []*access {
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

	var all []*access
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
		a.bounded = len(a.eq) > 0 || a.lo != nil || a.hi != nil
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
		for _, col := range cand.cols[len(a.eq):] {
			a.order = append(a.order, it.off+col)
		}
		if cand.index != nil {
			for _, col := range t.pk {
				a.order = append(a.order, it.off+col)
			}
		}
		all = append(all, a)
	}
	return all
}

// orderHint is what the query would like from the plan of its FROM clause:
// rows sorted by these columns, so that its ORDER BY needs no sort, and —
// if it has a LIMIT — only the first rows of them.
type orderHint struct {
	cols []int
	// rows is how many rows the query will read, or 0 for all of them.
	rows float64
}

// inOrder reports whether rows sorted by order are sorted by want.
func inOrder(order, want []int) bool {
	if len(want) == 0 || len(want) > len(order) {
		return false
	}
	for i, col := range want {
		if order[i] != col {
			return false
		}
	}
	return true
}

func (n *planNode) setOrder(order []int) {
	n.order, n.orderedBy = order, 0
	if len(order) > 0 {
		n.orderedBy = 1 + order[0]
	}
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

// scanIter reads a table, or the part of it a key range selects, a batch at
// a time. It returns the table's own rows.
type scanIter struct {
	db     *DB
	t      *table
	ix     *index
	kb     keyBounds
	cx     *env
	batch  []rowRef
	pos    int
	resume []byte
	done   bool
	// size is how many rows the next batch asks for. It starts small and
	// doubles: a query that wants ten rows should not decode hundreds, and
	// one that wants them all should not take the lock for each handful.
	size int
}

func (sc *scanIter) next() ([]any, error) {
	for sc.pos >= len(sc.batch) {
		if sc.done {
			return nil, nil
		}
		if err := sc.cx.ctx.Err(); err != nil {
			return nil, err
		}
		sc.size = min(max(2*sc.size, scanBatchFirst), scanBatchRows)
		batch, resume, err := sc.db.scanBatch(sc.t, sc.ix, sc.cx.q.snap, sc.kb, sc.resume, sc.size, sc.cx.locked)
		if err != nil {
			return nil, err
		}
		sc.batch, sc.pos, sc.resume, sc.done = batch, 0, resume, resume == nil
	}
	sc.pos++
	return sc.batch[sc.pos-1].vals, nil
}

func (sc *scanIter) close() { sc.batch, sc.done = nil, true }

// widenIter places a relation's own rows in its segment of the full row and
// applies the relation's conditions.
type widenIter struct {
	src        iter
	en         *env
	filters    []evalFn
	off, width int
}

func (w *widenIter) next() ([]any, error) {
	for {
		r, err := w.src.next()
		if err != nil || r == nil {
			return nil, err
		}
		full := make([]any, w.width)
		copy(full[w.off:], r)
		w.en.row = full
		if ok, err := allMatch(w.filters, w.en); err != nil {
			return nil, err
		} else if ok {
			return full, nil
		}
	}
}

func (w *widenIter) close() { w.src.close() }

// planScan plans reading one relation, applying the conditions that
// mention it alone.
func (p *planner) planScan(it *relItem, local []*conj) (*scanPlan, error) {
	width := len(p.cols)
	filters, filterExprs := condEvals(local)
	sel := 1.0
	for _, c := range local {
		sel *= p.selectivity(c.expr)
	}
	widen := func(cx *env, src iter) iter {
		return &widenIter{src: src, en: cx.child(), filters: filters, off: it.off, width: width}
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
		node.open = func(cx *env) (iter, error) {
			src, err := sub.open(cx)
			if err != nil {
				return nil, err
			}
			return widen(cx, src), nil
		}

	default:
		t := it.table
		sp.rawRows = it.ps.rows
		for _, c := range local {
			sp.sargs = append(sp.sargs, p.sargs(c, it)...)
		}
		node.rows = math.Max(sp.rawRows*sel, 1)
		seqCost := sp.rawRows * costSeqRow
		// A table is stored in the order of its primary key.
		var seqOrder []int
		for _, col := range t.pk {
			seqOrder = append(seqOrder, it.off+col)
		}
		// Only conditions on constants can drive a scan that stands alone.
		constant := func(sa sarg) bool { return len(sa.needs) == 0 }
		acc := p.bestAccess(it, sp.sargs, constant)
		if acc != nil && acc.cost >= seqCost {
			acc = nil
		}
		// If the query wants the rows of this table in some order, a path
		// that delivers them in it saves the sort — and, with a LIMIT,
		// reading most of the table. Every path is costed with what the
		// query would still have to do after it.
		if hint := p.hint; hint != nil && it == p.hintItem {
			after := func(cost float64, order []int) float64 {
				switch {
				case !inOrder(order, hint.cols):
					return cost + sortCost(node.rows)
				case hint.rows > 0 && hint.rows < node.rows:
					// Only the first rows are read.
					return costDescent + cost*hint.rows/node.rows
				}
				return cost
			}
			best := after(seqCost, seqOrder)
			if acc != nil {
				best = after(acc.cost, acc.order)
			}
			for _, a := range p.accesses(it, sp.sargs, constant) {
				if c := after(a.cost, a.order); inOrder(a.order, hint.cols) && c < best {
					best, acc = c, a
				}
			}
			if acc != nil && after(seqCost, seqOrder) <= best {
				acc = nil
			}
		}
		if acc != nil {
			node.op = fmt.Sprintf("Index Scan using %s on %s", acc.name(), it.label())
			if acc.bounded {
				node.lines = []string{"Index Cond: " + exprList(acc.used)}
			}
			filterLine(acc.used)
			node.cost = acc.cost
			node.rows = math.Min(node.rows, acc.rows)
			node.setOrder(acc.order)
			node.open = func(cx *env) (iter, error) {
				kb, ok, err := acc.bounds(cx.child())
				if err != nil {
					return nil, err
				}
				if !ok {
					return &sliceIter{}, nil
				}
				return widen(cx, &scanIter{db: p.s.db, t: t, ix: acc.index, kb: kb, cx: cx}), nil
			}
		} else {
			node.op = "Seq Scan on " + it.label()
			filterLine(nil)
			node.cost = seqCost
			node.setOrder(seqOrder)
			node.open = func(cx *env) (iter, error) {
				return widen(cx, &scanIter{db: p.s.db, t: t, cx: cx}), nil
			}
		}
	}
	sp.node = node
	return sp, nil
}

// ---- equalities a join can hash or merge on ----

// equiCond is a join condition of the form "a = b" in which the two sides
// can be computed separately and compared by value.
type equiCond struct {
	c            *conj
	l, r         bound
	lcols, rcols []int
	// float is set if the sides are compared as double precision.
	float bool
}

// equi analyses c as an equality between two separately computable sides,
// or returns nil if it is not one.
func (p *planner) equi(c *conj) *equiCond {
	if eq, done := p.equis[c]; done {
		return eq
	}
	p.equis[c] = nil
	e, ok := c.expr.(*sql.Binary)
	if !ok || e.Op != "=" || c.opaque {
		return nil
	}
	l, err := p.b.bind(e.L, sql.Unknown)
	if err != nil {
		return nil
	}
	r, err := p.b.bind(e.R, sql.Unknown)
	if err != nil {
		return nil
	}
	eq := &equiCond{c: c, l: l, r: r}
	switch {
	case l.typ.IsInt() && r.typ.IsInt():
	case l.typ.IsNumeric() && r.typ.IsNumeric():
		eq.float = true
	case l.typ == sql.Text && r.typ == sql.Text, l.typ == sql.Bool && r.typ == sql.Bool:
	default:
		return nil
	}
	eq.lcols, _ = p.refs(e.L)
	eq.rcols, _ = p.refs(e.R)
	if len(eq.lcols) == 0 || len(eq.rcols) == 0 {
		return nil
	}
	p.equis[c] = eq
	return eq
}

// keySide is one side of an equiCond, assigned to a side of a join.
type keySide struct {
	b    bound
	expr sql.Expr
}

func (k keySide) hashKey(eq *equiCond) hashKey { return hashKey{eval: k.b.eval, float: eq.float} }

// split assigns the sides of eq to the outer and the inner side of a join,
// given which columns each side has. It reports false if the equality does
// not separate that way.
func (eq *equiCond) split(isOuter, isInner func(cols []int) bool) (outer, inner keySide, ok bool) {
	e := eq.c.expr.(*sql.Binary)
	l, r := keySide{eq.l, e.L}, keySide{eq.r, e.R}
	switch {
	case isOuter(eq.lcols) && isInner(eq.rcols):
		return l, r, true
	case isOuter(eq.rcols) && isInner(eq.lcols):
		return r, l, true
	}
	return keySide{}, keySide{}, false
}

// keyColumn returns 1 + the FROM column a key is, if it is a plain column.
func (p *planner) keyColumn(k keySide) int {
	if col, ok := p.columnOf(k.expr); ok {
		return 1 + col
	}
	return 0
}

// hashParts builds the pieces of a hash join from the conditions of a join:
// the keys of each side, the conditions the keys stand for, and the
// conditions left to check on each pair.
func (p *planner) hashParts(conds []*conj, isOuter, isInner func(cols []int) bool) (probe, build []hashKey, used []sql.Expr, rest []*conj) {
	for _, c := range conds {
		if eq := p.equi(c); eq != nil {
			if o, i, ok := eq.split(isOuter, isInner); ok {
				probe, build = append(probe, o.hashKey(eq)), append(build, i.hashKey(eq))
				used = append(used, c.expr)
				continue
			}
		}
		rest = append(rest, c)
	}
	return probe, build, used, rest
}

// hashJoinNode builds the plan node of a hash join.
func (p *planner) hashJoinNode(spec *joinSpec, probe, build []hashKey, used []sql.Expr, rest []*conj) *planNode {
	restEvals, restExprs := condEvals(rest)
	spec.conds = restEvals
	hash := &planNode{op: "Hash", kids: []*planNode{spec.inner}, rows: spec.inner.rows,
		cost: spec.inner.cost + spec.inner.rows*costHashRow}
	node := &planNode{op: "Hash" + spec.kind.label() + " Join", kids: []*planNode{spec.outer, hash},
		lines: []string{"Hash Cond: " + exprList(used)}}
	if len(restExprs) > 0 {
		node.lines = append(node.lines, "Join Filter: "+exprList(restExprs))
	}
	node.open = func(cx *env) (iter, error) {
		return &hashJoinIter{j: spec, probeKeys: probe, buildKeys: build, hashNode: hash, cx: cx, en: cx.child()}, nil
	}
	return node
}

// nlJoinNode builds the plan node of a plain nested loop.
func nlJoinNode(spec *joinSpec, conds []*conj) *planNode {
	evals, exprs := condEvals(conds)
	spec.conds = evals
	node := &planNode{op: "Nested Loop" + spec.kind.label() + joinWord(spec.kind), kids: []*planNode{spec.outer, spec.inner}}
	// The outer rows keep their order, unless the rows of the inner side
	// that matched nothing are added at the end.
	if spec.kind != joinFull {
		node.setOrder(spec.outer.order)
	}
	if len(exprs) > 0 {
		node.lines = []string{"Join Filter: " + exprList(exprs)}
	}
	node.open = func(cx *env) (iter, error) {
		outer, err := spec.outer.start(cx)
		if err != nil {
			return nil, err
		}
		return &nlJoinIter{j: spec, cx: cx, en: cx.child(), outer: outer}, nil
	}
	return node
}

// joinWord completes the name of an outer nested loop the way PostgreSQL
// writes it: "Nested Loop", but "Nested Loop Left Join".
func joinWord(k joinKind) string {
	if k == joinInner {
		return ""
	}
	return " Join"
}

// filtered puts conditions on top of a node's rows.
func filtered(node *planNode, conds []evalFn) {
	if len(conds) == 0 {
		return
	}
	inner := node.open
	node.open = func(cx *env) (iter, error) {
		src, err := inner(cx)
		if err != nil {
			return nil, err
		}
		return &filterIter{src: src, en: cx.child(), conds: conds}, nil
	}
}

// planUnit plans an outer join. Conditions from outside that mention only
// its preserved side are pushed into that side; conditions in its ON clause
// that mention only the other side are pushed into the other side. (Not the
// other way round: an ON condition on the preserved side does not remove
// its rows, it only stops them matching.) A full join preserves both sides,
// and nothing can be pushed into either.
//
// In a unit, "left" is the preserved side and "right" the side whose
// columns are NULL in a row that matched nothing. A RIGHT JOIN arrives here
// with its sides already exchanged.
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
	inLeft := func(cols []int) bool { return len(cols) > 0 && inside(cols, loff, lend) }
	inRight := func(cols []int) bool { return len(cols) > 0 && inside(cols, roff, rend) }

	var toLeft, above []sql.Expr
	var aboveConds []*conj
	for _, c := range outside {
		if u.kind != joinFull && !c.opaque && inside(c.cols, loff, lend) {
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
		if u.kind != joinFull && !c.opaque && inRight(c.cols) {
			toRight = append(toRight, e)
		} else {
			on = append(on, c)
		}
	}

	left, err := p.planGroup(u.left, toLeft)
	if err != nil {
		return nil, err
	}
	right, err := p.planGroup(u.right, toRight)
	if err != nil {
		return nil, err
	}
	_, onExprs := condEvals(on)
	aboveEvals, _ := condEvals(aboveConds)
	sel := 1.0
	for _, c := range on {
		sel *= p.selectivity(c.expr)
	}
	width := len(p.cols)
	spec := &joinSpec{kind: u.kind, outer: left, inner: right, off: roff, end: rend, width: width}
	rows := math.Max(left.rows, left.rows*right.rows*sel)
	if u.kind == joinFull {
		rows = math.Max(rows, right.rows)
	}

	// Three ways to do it; the cheapest wins.
	nlCost := left.cost + right.cost + left.rows*right.rows*costPair + p.loopPenalty()
	node := nlJoinNode(spec, on)
	node.cost = nlCost

	if probe, build, used, rest := p.hashParts(on, inLeft, inRight); len(used) > 0 && p.s.vars["enable_hashjoin"] != "off" {
		if cost := left.cost + right.cost + right.rows*costHashRow + left.rows*costProbe; cost < nlCost {
			node = p.hashJoinNode(spec, probe, build, used, rest)
			node.cost = cost
		}
	}

	// If the right side is a single table that an index can look up from
	// the left row, that may beat both: it never reads the table at all.
	if len(u.right.items) == 1 && u.right.items[0].table != nil && u.kind == joinLeft {
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
		if cost := left.cost; acc != nil && usesLeft && cost+left.rows*acc.cost < node.cost {
			localEvals, localExprs := condEvals(rightLocal)
			onEvals, _ := condEvals(on)
			probe := &planNode{op: fmt.Sprintf("Index Scan using %s on %s", acc.name(), inner.label()),
				lines: []string{"Index Cond: " + exprList(acc.used)}, rows: acc.rows, cost: acc.cost}
			if len(localExprs) > 0 {
				probe.lines = append(probe.lines, "Filter: "+exprList(localExprs))
			}
			ispec := &joinSpec{kind: joinLeft, outer: left, off: inner.off, width: width, conds: onEvals}
			node = &planNode{op: "Nested Loop Left Join", kids: []*planNode{left, probe}, cost: cost + left.rows*acc.cost}
			node.setOrder(left.order)
			if len(onExprs) > 0 {
				node.lines = []string{"Join Filter: " + exprList(onExprs)}
			}
			rows = math.Max(left.rows, left.rows*acc.rows*sel)
			node.open = func(cx *env) (iter, error) {
				outer, err := left.start(cx)
				if err != nil {
					return nil, err
				}
				return &indexJoinIter{j: ispec, s: p.s, acc: acc, probe: probe, local: localEvals,
					cx: cx, en: cx.child(), outer: outer}, nil
			}
		}
	}

	node.rows = rows
	if len(above) > 0 {
		node.lines = append(node.lines, "Filter: "+exprList(above))
		filtered(node, aboveEvals)
	}
	return &scanPlan{node: node, rawRows: node.rows}, nil
}

// ---- join ordering ----

type joinMethod uint8

const (
	nestedLoop joinMethod = iota
	indexLoop
	hashJoin
	mergeJoin
)

// joinStep is one decision of a join order: add relation item to what has
// been joined so far, and how.
type joinStep struct {
	item   int
	method joinMethod
	acc    *access // for indexLoop
	// For mergeJoin: the equality merged on, and which inputs arrive
	// already in its order.
	merge                    *equiCond
	outerSorted, innerSorted bool
	conds                    []*conj // conditions that become checkable at this step
	rows, cost               float64
	// orderedBy describes the order of the step's output; see planNode.
	orderedBy int
}

// planGroup plans an inner-join group: how to read each relation, in what
// order to join them, and by which method.
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
	hashOK := p.s.vars["enable_hashjoin"] != "off"
	mergeOK := p.s.vars["enable_mergejoin"] != "off"

	// extend computes the step that adds relation i to the set joined.
	extend := func(joined bits, prev joinStep, i int) joinStep {
		it := g.items[i]
		now := joined.or(bit(i))
		rows, cost := prev.rows, prev.cost
		step := joinStep{item: i, orderedBy: prev.orderedBy}
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
		step.cost = cost + sp.node.cost + rows*sp.node.rows*costPair + emit + p.loopPenalty()
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
					step.cost, step.method, step.acc = lookup, indexLoop, acc
				}
			}
		}
		// Or, if the join has an equality, hash the inner relation or merge
		// the two in key order.
		isOuter := func(cols []int) bool { return maskOf(g, cols).within(joined) }
		isInner := func(cols []int) bool { return maskOf(g, cols).within(bit(i)) }
		for _, c := range step.conds {
			eq := p.equi(c)
			if eq == nil {
				continue
			}
			o, in, ok := eq.split(isOuter, isInner)
			if !ok {
				continue
			}
			if hash := cost + sp.node.cost + sp.node.rows*costHashRow + rows*costProbe + emit; hashOK && hash < step.cost {
				step.cost, step.method, step.acc, step.orderedBy = hash, hashJoin, nil, 0
			}
			if !mergeOK {
				continue
			}
			oCol, iCol := p.keyColumn(o), p.keyColumn(in)
			oSorted := oCol != 0 && prev.orderedBy == oCol
			iSorted := iCol != 0 && sp.node.orderedBy == iCol
			merge := cost + sp.node.cost + (rows+sp.node.rows)*costMergeRow + emit
			if !oSorted {
				merge += sortCost(rows)
			}
			if !iSorted {
				merge += sortCost(sp.node.rows)
			}
			if merge < step.cost {
				step.cost, step.method, step.acc = merge, mergeJoin, nil
				step.merge, step.outerSorted, step.innerSorted, step.orderedBy = eq, oSorted, iSorted, oCol
			}
		}
		return step
	}

	first := func(i int) joinStep {
		return joinStep{item: i, rows: scans[i].node.rows, cost: scans[i].node.cost, orderedBy: scans[i].node.orderedBy}
	}
	var order []joinStep
	reorder := p.s.vars["join_collapse_limit"] != "1"
	switch {
	case !reorder:
		// As written.
		order = []joinStep{first(0)}
		joined := bit(0)
		for i := 1; i < n; i++ {
			order = append(order, extend(joined, order[len(order)-1], i))
			joined = joined.or(bit(i))
		}
	case n <= 10:
		order = p.orderExhaustive(n, first, extend)
	default:
		order = p.orderGreedy(n, first, extend)
	}

	// Build the left-deep tree the order describes.
	width := len(p.cols)
	node := scans[order[0].item].node
	joined := bit(order[0].item)
	sorted := node.order
	for _, step := range order[1:] {
		it, sp, outer := g.items[step.item], scans[step.item], node
		spec := &joinSpec{kind: joinInner, outer: outer, inner: sp.node, off: it.off, end: it.off + it.width, width: width}
		was := joined
		isOuter := func(cols []int) bool { return maskOf(g, cols).within(was) }
		isInner := func(cols []int) bool { return maskOf(g, cols).within(bit(step.item)) }
		var join *planNode

		switch step.method {
		case indexLoop:
			acc := step.acc
			evals, exprs := condEvals(step.conds)
			spec.conds = evals
			localEvals, localExprs := condEvals(sp.local)
			probe := &planNode{op: fmt.Sprintf("Index Scan using %s on %s", acc.name(), it.label()),
				lines: []string{"Index Cond: " + exprList(acc.used)}, rows: acc.rows, cost: acc.cost}
			// Whatever the index condition does not already guarantee.
			if rest := without(append(append([]sql.Expr(nil), exprs...), localExprs...), acc.used); len(rest) > 0 {
				probe.lines = append(probe.lines, "Filter: "+exprList(rest))
			}
			join = &planNode{op: "Nested Loop", kids: []*planNode{outer, probe}}
			join.open = func(cx *env) (iter, error) {
				src, err := outer.start(cx)
				if err != nil {
					return nil, err
				}
				return &indexJoinIter{j: spec, s: p.s, acc: acc, probe: probe, local: localEvals,
					cx: cx, en: cx.child(), outer: src}, nil
			}

		case hashJoin:
			probe, build, used, rest := p.hashParts(step.conds, isOuter, isInner)
			join = p.hashJoinNode(spec, probe, build, used, rest)

		case mergeJoin:
			eq := step.merge
			o, in, _ := eq.split(isOuter, isInner)
			var rest []*conj
			for _, c := range step.conds {
				if c != eq.c {
					rest = append(rest, c)
				}
			}
			restEvals, restExprs := condEvals(rest)
			spec.conds = restEvals
			left, openLeft := p.mergeInput(outer, o, step.outerSorted)
			right, openRight := p.mergeInput(sp.node, in, step.innerSorted)
			join = &planNode{op: "Merge Join", kids: []*planNode{left, right},
				lines: []string{"Merge Cond: " + sql.FormatExpr(eq.c.expr)}}
			if len(restExprs) > 0 {
				join.lines = append(join.lines, "Join Filter: "+exprList(restExprs))
			}
			cmp, same := comparator(o.b.typ, in.b.typ), comparator(o.b.typ, o.b.typ)
			join.open = func(cx *env) (iter, error) {
				l, err := openLeft(cx)
				if err != nil {
					return nil, err
				}
				r, err := openRight(cx)
				if err != nil {
					l.close()
					return nil, err
				}
				return &mergeJoinIter{j: spec, cmp: cmp, same: same, left: l, right: r, en: cx.child()}, nil
			}

		default:
			join = nlJoinNode(spec, step.conds)
		}
		// Nested loops, with or without an index, return the outer rows
		// in the order they came; a merge returns them in key order; a
		// hash join that goes to disk returns them in no order at all.
		switch step.method {
		case hashJoin:
			sorted = nil
		case mergeJoin:
			sorted = nil
			if step.orderedBy != 0 {
				sorted = []int{step.orderedBy - 1}
			}
		}
		join.rows, join.cost = step.rows, step.cost
		join.setOrder(sorted)
		node = join
		joined = joined.or(bit(step.item))
	}
	return node, nil
}

// mergeInput prepares one input of a merge join: each row gets its key
// appended, and unless the rows already arrive in key order they are
// sorted by it — by an external sort, so that the input may be larger than
// memory. It returns the node to show in the plan and how to open it.
func (p *planner) mergeInput(child *planNode, key keySide, sorted bool) (*planNode, func(cx *env) (iter, error)) {
	width := len(p.cols)
	keyed := func(cx *env) (iter, error) {
		src, err := child.start(cx)
		if err != nil {
			return nil, err
		}
		return &keyedIter{src: src, en: cx.child(), eval: key.b.eval}, nil
	}
	if sorted {
		return child, keyed
	}
	cmp := comparator(key.b.typ, key.b.typ)
	node := &planNode{op: "Sort", lines: []string{"Sort Key: " + sql.FormatExpr(key.expr)},
		kids: []*planNode{child}, rows: child.rows, cost: child.cost + sortCost(child.rows)}
	node.open = func(cx *env) (iter, error) {
		src, err := keyed(cx)
		if err != nil {
			return nil, err
		}
		so := &sorter{q: cx.q, cmp: func(a, b []any) int {
			x, y := a[width], b[width]
			switch {
			case x == nil && y == nil:
				return 0
			case x == nil:
				return 1
			case y == nil:
				return -1
			}
			return cmp(x, y)
		}}
		return sortAll(src, so, node)
	}
	return node, node.start
}

// sortAll feeds every row of src to the sorter and returns them in order.
func sortAll(src iter, so *sorter, node *planNode) (iter, error) {
	defer src.close()
	for {
		row, err := src.next()
		if err != nil {
			so.release()
			return nil, err
		}
		if row == nil {
			break
		}
		if err := so.add(row); err != nil {
			so.release()
			return nil, err
		}
	}
	out, err := so.finish()
	if err != nil {
		so.release()
		return nil, err
	}
	node.info = []string{so.info()}
	return out, nil
}

// orderExhaustive finds the cheapest left-deep join order by dynamic
// programming over sets of relations: the best way to join a set is the
// best way to join all but one of its members, extended by that member.
// It considers every order while computing only 2^n subplans.
func (p *planner) orderExhaustive(n int, first func(int) joinStep, extend func(bits, joinStep, int) joinStep) []joinStep {
	type entry struct {
		ok   bool
		step joinStep
		prev int
	}
	best := make([]entry, 1<<n)
	for i := 0; i < n; i++ {
		best[1<<i] = entry{ok: true, step: first(i)}
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
			step := extend(joined, best[set].step, i)
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
func (p *planner) orderGreedy(n int, first func(int) joinStep, extend func(bits, joinStep, int) joinStep) []joinStep {
	start := first(0)
	for i := 1; i < n; i++ {
		if s := first(i); s.rows < start.rows {
			start = s
		}
	}
	order := []joinStep{start}
	joined := bit(start.item)
	for len(order) < n {
		last := order[len(order)-1]
		var pick *joinStep
		for i := 0; i < n; i++ {
			if joined.has(i) {
				continue
			}
			step := extend(joined, last, i)
			if pick == nil || step.cost < pick.cost {
				pick = &step
			}
		}
		order = append(order, *pick)
		joined = joined.or(bit(pick.item))
	}
	return order
}

// fromPlan is a FROM clause being planned. Its columns are known as soon
// as it is analysed; the plan comes second, once the rest of the query
// has said what it would like from it.
type fromPlan struct {
	// cols are the columns the clause produces, in written order.
	cols []scopeCol
	// star lists the columns "*" stands for, in the order it shows them:
	// all of them, except that a JOIN ... USING shows each of its columns
	// once, and first.
	star []int
	node *planNode

	p     *planner
	g     *joinGroup
	where sql.Expr
}

// analyseFrom resolves a FROM clause and checks the WHERE clause that
// filters it.
func (s *Session) analyseFrom(from sql.TableExpr, where sql.Expr, parent *scope, ptypes []sql.Type) (*fromPlan, error) {
	fb := &fromBuilder{s: s, parent: parent, ptypes: ptypes}
	g, err := fb.build(from)
	if err != nil {
		return nil, err
	}
	p := &planner{
		s:      s,
		b:      &binder{sess: s, scope: &scope{cols: fb.cols, parent: parent}, ptypes: ptypes},
		cols:   fb.cols,
		owners: fb.owners,
		conjs:  make(map[sql.Expr]*conj),
		equis:  make(map[*conj]*equiCond),
	}
	// Bind the whole condition once first, so that a type error is
	// reported about the clause as written rather than about a fragment.
	if _, err := p.b.bindWhere(where, "WHERE"); err != nil {
		return nil, err
	}
	return &fromPlan{cols: fb.cols, star: g.star, p: p, g: g, where: where}, nil
}

// sortable reports whether a scan can deliver rows in ascending order of
// FROM column col the way ORDER BY means it. Keys are stored with NULL
// before every value and ORDER BY puts NULLs last unless told otherwise,
// so the column must be one that has no NULLs, or the query must have
// asked for them first.
func (fp *fromPlan) sortable(col int, nullsFirst bool) bool {
	it, tcol, ok := fp.p.tableCol(col)
	return ok && (nullsFirst || it.table.cols[tcol].notNull)
}

// plan plans the clause: the result produces the rows that satisfy WHERE.
// hint, if not nil, is the order the query would like them in.
func (fp *fromPlan) plan(hint *orderHint) error {
	if hint != nil && len(fp.g.items) == 1 && fp.g.items[0].table != nil {
		fp.p.hint, fp.p.hintItem = hint, fp.g.items[0]
	}
	node, err := fp.p.planGroup(fp.g, conjuncts(fp.where))
	fp.node = node
	return err
}

// targetScan is how an UPDATE or DELETE finds the rows it applies to. It is
// the same choice a SELECT from one table faces — scan, or go through an
// index — so that changing one row by its key does not read the table.
//
// Unlike a query it collects all its rows before any is changed: a row
// that an UPDATE moves must not be met again further along the scan.
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
		equis:  make(map[*conj]*equiCond),
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
			refs, err = s.db.scan(t, en.q.snap)
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

package engine

import (
	"fmt"
	"math"
	"strings"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
)

// This file plans what a SELECT does after its FROM and WHERE clauses, which
// plan.go plans: grouping, the select list, DISTINCT, ordering and limits,
// and the set operations that combine whole queries. Each is an iterator
// over the one before; the semantics — scoping, grouping rules, NULL
// ordering — were settled by the first executor and are pinned by
// sqllogictest.

// opener starts a step of a plan.
type opener func(cx *env) (iter, error)

// selectPlan is a query ready to run.
type selectPlan struct {
	cols  []pgwire.Column
	types []sql.Type
	// loose marks the columns whose type was only a default: an untyped
	// NULL or quoted literal, which takes the type of whatever it is
	// combined with in a set operation.
	loose []bool
	// open starts the query. cx is the environment of the enclosing query
	// level (for a top-level query, one with no row): it supplies the
	// context, the parameters and the outer rows a correlated subquery
	// refers to.
	open opener
	// root is the plan as a tree, for EXPLAIN.
	root *planNode
}

// run executes the query to the end and returns its rows.
func (p *selectPlan) run(cx *env) ([][]any, error) {
	it, err := p.open(cx)
	if err != nil {
		return nil, err
	}
	return drain(it)
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

// rowOrder returns the comparison ORDER BY asks for, with key i found in
// column cols[i] of a row.
func rowOrder(keys []sortKey, cols []int) func(a, b []any) int {
	return func(a, b []any) int {
		for k, key := range keys {
			x, y := a[cols[k]], b[cols[k]]
			switch {
			case x == nil && y == nil:
				continue
			case x == nil:
				if key.nullsFirst {
					return -1
				}
				return 1
			case y == nil:
				if key.nullsFirst {
					return 1
				}
				return -1
			}
			c := key.cmp(x, y)
			if c == 0 {
				continue
			}
			if key.desc {
				return -c
			}
			return c
		}
		return 0
	}
}

func (s *Session) planSelect(n *sql.Select, parent *scope, ptypes []sql.Type) (*selectPlan, error) {
	if n.Op != "" {
		return s.planSetOp(n, parent, ptypes)
	}
	// FROM and WHERE are planned together: where each condition is
	// applied, how each table is read, in what order and by what method
	// they are joined is the planner's business (plan.go). What comes back
	// is a plan producing the rows that satisfy WHERE, with the columns of
	// the FROM clause in the order they were written.
	var from *fromPlan
	var source *planNode
	if n.From != nil {
		var err error
		// The plan itself is made further down, once it is known whether
		// the query would like its rows in some order.
		if from, err = s.analyseFrom(n.From, n.Where, parent, ptypes); err != nil {
			return nil, err
		}
	} else {
		// Without FROM a query produces exactly one row with no columns,
		// or none if WHERE says so.
		from = &fromPlan{}
		cond, err := (&binder{sess: s, scope: &scope{parent: parent}, ptypes: ptypes}).bindWhere(n.Where, "WHERE")
		if err != nil {
			return nil, err
		}
		source = &planNode{op: "Result", rows: 1, cost: 0.01}
		if n.Where != nil {
			source.lines = []string{"One-Time Filter: " + sql.FormatExpr(n.Where)}
		}
		source.open = func(cx *env) (iter, error) {
			ok, err := matches(cond, cx.child())
			if err != nil || !ok {
				return &sliceIter{}, err
			}
			return &sliceIter{rows: [][]any{{}}}, nil
		}
	}
	sc := &scope{cols: from.cols, parent: parent}
	b := &binder{sess: s, scope: sc, ptypes: ptypes}
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
		if it.Table == "" {
			for _, i := range from.star {
				items = append(items, item{&colIdx{i}, sc.cols[i].name})
				matched = true
			}
		}
		for i, c := range sc.cols {
			if it.Table != "" && it.Table == c.table {
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
		plan.loose = append(plan.loose, bo.typ == sql.Unknown)
		plan.cols = append(plan.cols, pgwire.Column{Name: it.name, OID: oidOf(typ)})
	}

	// A row travels from the select list onwards as its output values
	// followed by the values of any ORDER BY expressions that are not
	// output columns; those are dropped at the very end.
	keys := make([]sortKey, len(n.OrderBy))
	keyCols := make([]int, len(n.OrderBy))
	var extra []evalFn
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
			keyCols[i] = key.out
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
			keyCols[i] = len(outs) + len(extra)
			extra = append(extra, bk.eval)
		}
		key.cmp = comparator(typ, typ)
		keys[i] = key
	}

	// project computes the row for en's current input row or group.
	project := func(en *env) ([]any, error) {
		vals := make([]any, len(outs)+len(extra))
		for i, eval := range outs {
			v, err := eval(en)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		for i, eval := range extra {
			v, err := eval(en)
			if err != nil {
				return nil, err
			}
			vals[len(outs)+i] = v
		}
		return vals, nil
	}

	// If ORDER BY asks for nothing but columns of the FROM clause, in
	// ascending order, a scan may be able to deliver the rows already
	// sorted: the plan is asked for that, and if it obliges there is no
	// sort. With a LIMIT that is the difference between reading the table
	// and reading the rows wanted.
	ordered := false
	if n.From != nil {
		var hint *orderHint
		if !grouped && !n.Distinct && len(n.OrderBy) > 0 {
			hint = &orderHint{}
			for i, o := range n.OrderBy {
				e := o.Expr
				if keys[i].out >= 0 {
					e = items[keys[i].out].expr
				}
				col := -1
				switch e := e.(type) {
				case *colIdx:
					col = e.idx
				case *sql.ColumnRef:
					col, _ = sc.find(e)
				}
				if col < 0 || o.Desc || !from.sortable(col, o.NullsFirst != nil && *o.NullsFirst) {
					hint = nil
					break
				}
				hint.cols = append(hint.cols, col)
			}
		}
		if hint != nil {
			if lit, ok := n.Limit.(*sql.Literal); ok && lit.Type.IsInt() {
				hint.rows = float64(lit.Val.(int64))
				if off, ok := n.Offset.(*sql.Literal); ok && off.Type.IsInt() {
					hint.rows += float64(off.Val.(int64))
				} else if n.Offset != nil {
					hint.rows = 0
				}
			}
		}
		if err := from.plan(hint); err != nil {
			return nil, err
		}
		source = from.node
		ordered = hint != nil && inOrder(source.order, hint.cols)
	}
	if ordered {
		keys, keyCols = nil, nil
	}

	root, cur := source, opener(source.start)
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
		root = &planNode{op: op, lines: lines, kids: []*planNode{source}, rows: math.Max(rows, 1), cost: source.cost + source.rows*0.1}
		nCols, node := len(sc.cols), root
		root.open = func(cx *env) (iter, error) {
			src, err := source.start(cx)
			if err != nil {
				return nil, err
			}
			return &aggIter{src: src, cx: cx, en: cx.child(), node: node, groupEvals: groupEvals, aggs: aggs,
				having: having, project: project, nCols: nCols}, nil
		}
		cur = root.start
	} else {
		cur = func(cx *env) (iter, error) {
			src, err := source.start(cx)
			if err != nil {
				return nil, err
			}
			return &projectIter{src: src, en: cx.child(), project: project}, nil
		}
	}

	plan.root, plan.open, err = s.planTail(root, cur, tail{
		distinct: n.Distinct, width: len(outs), extra: len(extra),
		keys: keys, keyCols: keyCols, sortKeys: sortKeyList(n.OrderBy),
		limit: n.Limit, offset: n.Offset,
	}, parent, ptypes)
	return plan, err
}

// tail describes what happens to the rows of a query after its select
// list: the same for a plain SELECT and for a set operation.
type tail struct {
	distinct bool
	// width is the number of output columns; extra, the number of sort-only
	// values that follow them in each row.
	width, extra  int
	keys          []sortKey
	keyCols       []int
	sortKeys      string // for EXPLAIN
	limit, offset sql.Expr
}

// sortKeyList renders an ORDER BY clause for EXPLAIN.
func sortKeyList(items []sql.OrderItem) string {
	parts := make([]string, len(items))
	for i, o := range items {
		parts[i] = sql.FormatExpr(o.Expr)
		if o.Desc {
			parts[i] += " DESC"
		}
		if o.NullsFirst != nil && *o.NullsFirst != o.Desc {
			if *o.NullsFirst {
				parts[i] += " NULLS FIRST"
			} else {
				parts[i] += " NULLS LAST"
			}
		}
	}
	return strings.Join(parts, ", ")
}

// planTail adds DISTINCT, ORDER BY, LIMIT and OFFSET on top of a plan.
func (s *Session) planTail(root *planNode, cur opener, t tail, parent *scope, ptypes []sql.Type) (*planNode, opener, error) {
	limit, err := s.bindRowCount(t.limit, parent, ptypes, "LIMIT")
	if err != nil {
		return nil, nil, err
	}
	offset, err := s.bindRowCount(t.offset, parent, ptypes, "OFFSET")
	if err != nil {
		return nil, nil, err
	}
	stage := func(op string, rows float64, lines ...string) *planNode {
		root = &planNode{op: op, lines: lines, kids: []*planNode{root}, rows: math.Max(rows, 1), cost: root.cost + root.rows*0.1}
		return root
	}
	if t.distinct {
		src, width := cur, t.width
		node := stage("Unique", root.rows/2)
		node.open = func(cx *env) (iter, error) {
			in, err := src(cx)
			if err != nil {
				return nil, err
			}
			return &distinctIter{src: in, cx: cx, node: node, width: width}, nil
		}
		cur = node.start
	}
	if len(t.keys) > 0 {
		src, order := cur, rowOrder(t.keys, t.keyCols)
		node := stage("Sort", root.rows, "Sort Key: "+t.sortKeys)
		node.cost += sortCost(root.rows)
		node.open = func(cx *env) (iter, error) {
			so := &sorter{q: cx.q, cmp: order}
			// With a LIMIT only the first rows are wanted, and the sort
			// need not keep the others.
			if t.limit != nil {
				take, err := limit(cx, -1)
				if err != nil {
					return nil, err
				}
				skip, err := offset(cx, 0)
				if err != nil {
					return nil, err
				}
				if take >= 0 {
					so.limit = skip + take
				}
			}
			in, err := src(cx)
			if err != nil {
				return nil, err
			}
			return sortAll(in, so, node)
		}
		cur = node.start
	}
	if t.limit != nil || t.offset != nil {
		src := cur
		rows := root.rows
		if lit, ok := t.limit.(*sql.Literal); ok && lit.Type.IsInt() {
			rows = math.Min(rows, float64(lit.Val.(int64)))
		}
		node := stage("Limit", rows)
		node.open = func(cx *env) (iter, error) {
			skip, err := offset(cx, 0)
			if err != nil {
				return nil, err
			}
			take, err := limit(cx, -1)
			if err != nil {
				return nil, err
			}
			if take == 0 {
				return &sliceIter{}, nil // nothing is wanted: nothing is read
			}
			in, err := src(cx)
			if err != nil {
				return nil, err
			}
			return &limitIter{src: in, skip: skip, take: take}, nil
		}
		cur = node.start
	}
	if t.extra > 0 {
		src, width := cur, t.width
		cur = func(cx *env) (iter, error) {
			in, err := src(cx)
			if err != nil {
				return nil, err
			}
			return &trimIter{src: in, width: width}, nil
		}
	}
	return root, cur, nil
}

// projectIter computes the select list for each input row.
type projectIter struct {
	src     iter
	en      *env
	project func(*env) ([]any, error)
}

func (p *projectIter) next() ([]any, error) {
	row, err := p.src.next()
	if err != nil || row == nil {
		return nil, err
	}
	p.en.row = row
	return p.project(p.en)
}

func (p *projectIter) close() { p.src.close() }

// overflow is where an operator that keeps a hash table puts the rows it
// has no room for: sixteen temporary files, a row going to the one its key
// hashes to. Rows with the same key end up in the same file, so each file
// can be dealt with on its own afterwards, as a smaller instance of the
// same problem — and split again, with a different hash, if it is still
// too large.
type overflow struct {
	depth int
	parts []*spillFile
}

// newOverflow creates the files. from is the file being read when memory
// ran out, if it is one: its size says roughly how much is still to come,
// and so how many files are worth opening.
func newOverflow(q *query, depth int, from *spillFile) (*overflow, error) {
	n := hashPartitions
	if from != nil {
		// A row takes about three times as much memory as it does on disk.
		n = fanoutFor(3*from.size, q.workMem)
	}
	o := &overflow{depth: depth, parts: make([]*spillFile, n)}
	for i := range o.parts {
		f, err := q.newSpill()
		if err != nil {
			o.release()
			return nil, err
		}
		o.parts[i] = f
	}
	return o, nil
}

func (o *overflow) add(key []byte, row []any) error {
	return o.parts[partitionOf(key, o.depth)%len(o.parts)].write(row)
}

func (o *overflow) release() {
	for _, f := range o.parts {
		if f != nil {
			f.remove()
		}
	}
	o.parts = nil
}

// hashReport describes a hash-based step for EXPLAIN ANALYZE.
func hashReport(node *planNode, batches, peak int) {
	if node != nil && batches > 0 {
		node.info = []string{fmt.Sprintf("Batches: %d  Memory Usage: %dkB", batches, (peak+1023)/1024)}
	}
}

// hashEntryBytes is the memory charged for an entry of a hash table beyond
// its key.
const hashEntryBytes = 48

// aggIter groups its input and computes the select list once per group.
//
// The groups are kept in a hash table. If they outgrow work_mem, no new
// group is started: rows of groups already in the table go on being
// aggregated, and the others are set aside in an overflow, to be grouped
// in later passes. A group is therefore always completed within one pass,
// and memory holds the groups of one pass at a time.
type aggIter struct {
	src        iter
	cx, en     *env
	node       *planNode
	groupEvals []evalFn
	aggs       []aggSpec
	having     evalFn
	project    func(*env) ([]any, error)
	nCols      int

	started bool
	groups  []*aggGroup
	pos     int
	pending []spilled

	batches, peak int
}

// spilled is a file of rows waiting for a pass of their own.
type spilled struct {
	file  *spillFile
	depth int
}

type aggGroup struct {
	first  []any // a representative row, for the GROUP BY columns
	states []aggState
}

// pass groups the rows of src, which are those of file from if it is not
// the query's own input.
func (a *aggIter) pass(src iter, depth int, from *spillFile) error {
	defer src.close()
	a.groups, a.pos = nil, 0
	index := make(map[string]*aggGroup)
	perGroup := hashEntryBytes + 64*len(a.aggs)
	var over *overflow
	var keyBuf []byte
	mem := 0
	for n := 0; ; n++ {
		row, err := src.next()
		if err != nil {
			return err
		}
		if row == nil {
			break
		}
		if n&0xfff == 0 {
			if err := a.cx.ctx.Err(); err != nil {
				return err
			}
		}
		a.en.row = row
		keyBuf = keyBuf[:0]
		for _, g := range a.groupEvals {
			v, err := g(a.en)
			if err != nil {
				return err
			}
			keyBuf = appendKey(keyBuf, v)
		}
		grp := index[string(keyBuf)]
		if grp == nil {
			if over == nil && mem > a.cx.q.workMem && depth < hashMaxDepth {
				if over, err = newOverflow(a.cx.q, depth, from); err != nil {
					return err
				}
				for _, f := range over.parts {
					a.pending = append(a.pending, spilled{f, depth + 1})
				}
			}
			if over != nil {
				if err := over.add(keyBuf, row); err != nil {
					return err
				}
				continue
			}
			grp = &aggGroup{first: row, states: make([]aggState, len(a.aggs))}
			index[string(keyBuf)] = grp
			a.groups = append(a.groups, grp)
			mem += rowBytes(row) + len(keyBuf) + perGroup
		}
		for i := range a.aggs {
			if err := grp.states[i].feed(&a.aggs[i], a.en); err != nil {
				return err
			}
		}
	}
	a.batches++
	a.peak = max(a.peak, mem)
	return nil
}

func (a *aggIter) next() ([]any, error) {
	if !a.started {
		a.started = true
		if err := a.pass(a.src, 0, nil); err != nil {
			return nil, err
		}
		// Aggregating without GROUP BY always yields one row, even over
		// no input: count(*) of an empty table is 0, not nothing.
		if len(a.groupEvals) == 0 && len(a.groups) == 0 {
			a.groups = append(a.groups, &aggGroup{first: make([]any, a.nCols), states: make([]aggState, len(a.aggs))})
		}
		a.en.aggs = make([]any, len(a.aggs))
	}
	for {
		for a.pos < len(a.groups) {
			grp := a.groups[a.pos]
			a.groups[a.pos] = nil
			a.pos++
			a.en.row = grp.first
			for i := range a.aggs {
				a.en.aggs[i] = grp.states[i].result(&a.aggs[i])
			}
			ok, err := matches(a.having, a.en)
			if err != nil {
				return nil, err
			}
			if ok {
				return a.project(a.en)
			}
		}
		if len(a.pending) == 0 {
			hashReport(a.node, a.batches, a.peak)
			return nil, nil
		}
		next := a.pending[len(a.pending)-1]
		a.pending = a.pending[:len(a.pending)-1]
		src, err := next.file.reader()
		if err == nil {
			err = a.pass(src, next.depth, next.file)
		}
		next.file.remove()
		if err != nil {
			return nil, err
		}
	}
}

func (a *aggIter) close() {
	if !a.started {
		a.src.close()
	}
	for _, p := range a.pending {
		p.file.remove()
	}
	a.pending, a.groups = nil, nil
}

// distinctIter passes on the first of each set of equal rows. It remembers
// the rows it has passed on in a hash table; when that outgrows work_mem,
// rows it has not seen are set aside in an overflow instead of being
// passed on, and are dealt with in later passes.
type distinctIter struct {
	src   iter
	cx    *env
	node  *planNode
	width int

	seen    map[string]struct{}
	mem     int
	depth   int
	over    *overflow
	pending []spilled
	current *spillFile
	keyBuf  []byte

	batches, peak int
}

func rowKey(dst []byte, row []any) []byte {
	for _, v := range row {
		dst = appendKey(dst, v)
	}
	return dst
}

func (d *distinctIter) next() ([]any, error) {
	if d.seen == nil {
		d.seen = make(map[string]struct{})
	}
	for {
		row, err := d.src.next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			// The pass is over. Go on with a file of rows set aside, if any.
			d.src.close()
			d.src = &sliceIter{}
			if d.current != nil {
				d.current.remove()
				d.current = nil
			}
			d.batches++
			d.peak = max(d.peak, d.mem)
			d.over = nil
			if len(d.pending) == 0 {
				hashReport(d.node, d.batches, d.peak)
				return nil, nil
			}
			next := d.pending[len(d.pending)-1]
			d.pending = d.pending[:len(d.pending)-1]
			if d.src, err = next.file.reader(); err != nil {
				return nil, err
			}
			d.current, d.depth = next.file, next.depth
			d.seen, d.mem = make(map[string]struct{}), 0
			continue
		}
		d.keyBuf = rowKey(d.keyBuf[:0], row[:d.width])
		if _, dup := d.seen[string(d.keyBuf)]; dup {
			continue
		}
		if d.over == nil && d.mem > d.cx.q.workMem && d.depth < hashMaxDepth {
			if d.over, err = newOverflow(d.cx.q, d.depth, d.current); err != nil {
				return nil, err
			}
			for _, f := range d.over.parts {
				d.pending = append(d.pending, spilled{f, d.depth + 1})
			}
		}
		if d.over != nil {
			if err := d.over.add(d.keyBuf, row); err != nil {
				return nil, err
			}
			continue
		}
		d.seen[string(d.keyBuf)] = struct{}{}
		d.mem += len(d.keyBuf) + hashEntryBytes
		return row, nil
	}
}

func (d *distinctIter) close() {
	d.src.close()
	if d.current != nil {
		d.current.remove()
		d.current = nil
	}
	for _, p := range d.pending {
		p.file.remove()
	}
	d.pending, d.seen = nil, nil
}

// limitIter skips rows, passes on at most take (all of them if take is
// negative), and then stops its source: whatever is below does not run
// further than it has to.
type limitIter struct {
	src        iter
	skip, take int
	done       bool
}

func (l *limitIter) next() ([]any, error) {
	if l.done {
		return nil, nil
	}
	for ; l.skip > 0; l.skip-- {
		row, err := l.src.next()
		if err != nil || row == nil {
			l.done = true
			return nil, err
		}
	}
	if l.take == 0 {
		l.close()
		return nil, nil
	}
	row, err := l.src.next()
	if err != nil || row == nil {
		l.done = true
		return nil, err
	}
	if l.take > 0 {
		l.take--
	}
	return row, nil
}

func (l *limitIter) close() {
	l.done = true
	l.src.close()
}

// trimIter drops the sort-only values at the end of each row.
type trimIter struct {
	src   iter
	width int
}

func (t *trimIter) next() ([]any, error) {
	row, err := t.src.next()
	if err != nil || row == nil {
		return nil, err
	}
	return row[:t.width:t.width], nil
}

func (t *trimIter) close() { t.src.close() }

// ---- set operations ----

// planSetOp plans "left UNION | INTERSECT | EXCEPT [ALL] right".
func (s *Session) planSetOp(n *sql.Select, parent *scope, ptypes []sql.Type) (*selectPlan, error) {
	left, err := s.planSelect(n.Left, parent, ptypes)
	if err != nil {
		return nil, err
	}
	right, err := s.planSelect(n.Right, parent, ptypes)
	if err != nil {
		return nil, err
	}
	op := strings.ToUpper(n.Op)
	if len(left.cols) != len(right.cols) {
		return nil, pgerr.New(pgerr.SyntaxError, "each %s query must have the same number of columns", op)
	}
	width := len(left.cols)
	plan := &selectPlan{cols: make([]pgwire.Column, width), types: make([]sql.Type, width), loose: make([]bool, width)}
	convL, convR := make([]convFn, width), make([]convFn, width)
	for i := range left.cols {
		lt, rt := left.types[i], right.types[i]
		typ := lt
		switch {
		case left.loose[i] && !right.loose[i]:
			typ = rt
		case right.loose[i] && !left.loose[i], lt == rt:
		case lt.IsInt() && rt.IsInt():
			typ = sql.Int8
		case lt.IsNumeric() && rt.IsNumeric():
			typ = sql.Float8
		default:
			return nil, pgerr.New(pgerr.DatatypeMismatch, "%s types %s and %s cannot be matched", op, lt, rt)
		}
		plan.types[i], plan.loose[i] = typ, left.loose[i] && right.loose[i]
		plan.cols[i] = pgwire.Column{Name: left.cols[i].Name, OID: oidOf(typ)}
		convL[i], convR[i] = setOpConv(lt, typ, left.loose[i]), setOpConv(rt, typ, right.loose[i])
	}
	openLeft, openRight := converted(left.open, convL), converted(right.open, convR)

	var root *planNode
	kids := []*planNode{left.root, right.root}
	rows, cost := left.root.rows+right.root.rows, left.root.cost+right.root.cost
	switch {
	case n.Op == "union":
		// The rows of one, then the rows of the other.
		root = &planNode{op: "Append", kids: kids, rows: rows, cost: cost}
		root.open = func(cx *env) (iter, error) {
			l, err := openLeft(cx)
			if err != nil {
				return nil, err
			}
			return &appendIter{cur: l, next_: func() (iter, error) { return openRight(cx) }}, nil
		}
	default:
		name := "HashSetOp Intersect"
		if n.Op == "except" {
			name = "HashSetOp Except"
			rows = left.root.rows
		} else {
			rows = math.Min(left.root.rows, right.root.rows)
		}
		if n.All {
			name += " All"
		}
		root = &planNode{op: name, kids: kids, rows: math.Max(rows/2, 1), cost: cost + right.root.rows*costHashRow + left.root.rows*costProbe}
		except, all, node := n.Op == "except", n.All, root
		root.open = func(cx *env) (iter, error) {
			l, err := openLeft(cx)
			if err != nil {
				return nil, err
			}
			return &setOpIter{left: l, openRight: func() (iter, error) { return openRight(cx) }, except: except, all: all,
				cx: cx, node: node}, nil
		}
	}
	cur := opener(root.start)

	// ORDER BY sees only the result's columns, by name or by position.
	keys := make([]sortKey, len(n.OrderBy))
	keyCols := make([]int, len(n.OrderBy))
	for i, o := range n.OrderBy {
		col := -1
		switch e := o.Expr.(type) {
		case *sql.Literal:
			if e.Type.IsInt() {
				pos := e.Val.(int64)
				if pos < 1 || pos > int64(width) {
					return nil, pgerr.New("42P10", "ORDER BY position %d is not in select list", pos).At(e.Pos)
				}
				col = int(pos - 1)
			}
		case *sql.ColumnRef:
			for j, c := range plan.cols {
				if e.Table == "" && c.Name == e.Name && col < 0 {
					col = j
				}
			}
		}
		if col < 0 {
			return nil, pgerr.New(pgerr.FeatureNotSupported, "invalid UNION/INTERSECT/EXCEPT ORDER BY clause").
				At(o.Expr.Position()).WithDetail("Only result column names can be used, not expressions or functions.")
		}
		key := sortKey{out: col, desc: o.Desc, nullsFirst: o.Desc, cmp: comparator(plan.types[col], plan.types[col])}
		if o.NullsFirst != nil {
			key.nullsFirst = *o.NullsFirst
		}
		keys[i], keyCols[i] = key, col
	}
	plan.root, plan.open, err = s.planTail(root, cur, tail{
		distinct: n.Op == "union" && !n.All, width: width,
		keys: keys, keyCols: keyCols, sortKeys: sortKeyList(n.OrderBy),
		limit: n.Limit, offset: n.Offset,
	}, parent, ptypes)
	return plan, err
}

// setOpConv returns how a value of one side of a set operation becomes a
// value of the result's column type, or nil if it already is one.
func setOpConv(from, to sql.Type, loose bool) convFn {
	switch {
	case loose && to != sql.Text:
		// An untyped literal on this side and a typed column on the other:
		// only NULL is acceptable without a cast.
		return func(v any) (any, error) {
			if s, ok := v.(string); ok {
				return nil, pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type %s: %q", to, s)
			}
			return v, nil
		}
	case to == sql.Float8 && from.IsInt():
		return func(v any) (any, error) {
			if i, ok := v.(int64); ok {
				return float64(i), nil
			}
			return v, nil
		}
	}
	return nil
}

// converted applies the conversions to every row an opener produces.
func converted(open opener, convs []convFn) opener {
	needed := false
	for _, c := range convs {
		needed = needed || c != nil
	}
	if !needed {
		return open
	}
	return func(cx *env) (iter, error) {
		src, err := open(cx)
		if err != nil {
			return nil, err
		}
		return &convIter{src: src, convs: convs}, nil
	}
}

type convIter struct {
	src   iter
	convs []convFn
}

func (c *convIter) next() ([]any, error) {
	row, err := c.src.next()
	if err != nil || row == nil {
		return nil, err
	}
	out := make([]any, len(row))
	copy(out, row)
	for i, conv := range c.convs {
		if conv != nil && out[i] != nil {
			if out[i], err = conv(out[i]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (c *convIter) close() { c.src.close() }

// appendIter returns the rows of one input and then those of the next,
// which is not even opened before the first is exhausted.
type appendIter struct {
	cur   iter
	next_ func() (iter, error)
}

func (a *appendIter) next() ([]any, error) {
	for {
		row, err := a.cur.next()
		if err != nil || row != nil {
			return row, err
		}
		if a.next_ == nil {
			return nil, nil
		}
		a.cur.close()
		if a.cur, err = a.next_(); err != nil {
			a.cur, a.next_ = &sliceIter{}, nil
			return nil, err
		}
		a.next_ = nil
	}
}

func (a *appendIter) close() { a.cur.close() }

// setOpIter computes INTERSECT and EXCEPT. It counts the rows of the right
// input in a hash table, then goes through the left input once.
//
//	INTERSECT      a left row is returned once if it occurs on the right
//	INTERSECT ALL  as many times as it occurs on both sides
//	EXCEPT         once if it does not occur on the right
//	EXCEPT ALL     as many times as it occurs more on the left than on the right
//
// Rows are compared as DISTINCT compares them: two NULLs are the same.
//
// If the table outgrows work_mem, it takes no new rows: the rows of both
// inputs that are not in it are set aside, each in the overflow of its
// side, split by the same hash. Equal rows are then in files of the same
// number, and each pair of files is a smaller set operation, done in a
// later pass.
type setOpIter struct {
	left        iter
	openRight   func() (iter, error)
	except, all bool
	cx          *env
	node        *planNode

	started bool
	counts  map[string]int
	mem     int
	depth   int
	// lover and rover hold the left and right rows set aside in this pass;
	// both are nil until the table is full.
	lover, rover *overflow
	pending      []setPair
	files        []*spillFile // of the pass in progress
	keyBuf       []byte

	batches, peak int
}

type setPair struct {
	left, right *spillFile
	depth       int
}

// full reports whether the table can take another row, starting the
// overflow of both sides if it cannot.
func (s *setOpIter) full() (bool, error) {
	if s.lover != nil {
		return true, nil
	}
	if s.mem <= s.cx.q.workMem || s.depth >= hashMaxDepth {
		return false, nil
	}
	// Both sides must be split into the same number of files.
	var larger *spillFile
	for _, f := range s.files {
		if larger == nil || f.size > larger.size {
			larger = f
		}
	}
	var err error
	if s.lover, err = newOverflow(s.cx.q, s.depth, larger); err != nil {
		return false, err
	}
	if s.rover, err = newOverflow(s.cx.q, s.depth, larger); err != nil {
		return false, err
	}
	for i := range s.lover.parts {
		s.pending = append(s.pending, setPair{s.lover.parts[i], s.rover.parts[i], s.depth + 1})
	}
	return true, nil
}

// build counts the rows of the right input.
func (s *setOpIter) build(right iter) error {
	defer right.close()
	s.counts, s.mem, s.lover, s.rover = make(map[string]int), 0, nil, nil
	for {
		row, err := right.next()
		if err != nil || row == nil {
			return err
		}
		s.keyBuf = rowKey(s.keyBuf[:0], row)
		if n, ok := s.counts[string(s.keyBuf)]; ok {
			s.counts[string(s.keyBuf)] = n + 1
			continue
		}
		full, err := s.full()
		if err != nil {
			return err
		}
		if full {
			if err := s.rover.add(s.keyBuf, row); err != nil {
				return err
			}
			continue
		}
		s.counts[string(s.keyBuf)] = 1
		s.mem += len(s.keyBuf) + hashEntryBytes
	}
}

func (s *setOpIter) next() ([]any, error) {
	if !s.started {
		s.started = true
		right, err := s.openRight()
		if err != nil {
			return nil, err
		}
		if err := s.build(right); err != nil {
			return nil, err
		}
	}
	for {
		row, err := s.left.next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			// The pass is over. Go on with a pair of files, if any.
			s.left.close()
			s.left = &sliceIter{}
			s.dropFiles()
			s.batches++
			s.peak = max(s.peak, s.mem)
			if len(s.pending) == 0 {
				hashReport(s.node, s.batches, s.peak)
				return nil, nil
			}
			next := s.pending[len(s.pending)-1]
			s.pending = s.pending[:len(s.pending)-1]
			s.files, s.depth = []*spillFile{next.left, next.right}, next.depth
			right, err := next.right.reader()
			if err != nil {
				return nil, err
			}
			if err := s.build(right); err != nil {
				return nil, err
			}
			if s.left, err = next.left.reader(); err != nil {
				return nil, err
			}
			continue
		}
		s.keyBuf = rowKey(s.keyBuf[:0], row)
		n, known := s.counts[string(s.keyBuf)]
		if !known {
			// Not in the table. If right rows were set aside, this row's
			// equals may be among them: it waits for their pass.
			if s.rover != nil {
				if err := s.lover.add(s.keyBuf, row); err != nil {
					return nil, err
				}
				continue
			}
			switch {
			case !s.except:
				// INTERSECT: not on the right, not in the result.
			case s.all:
				return row, nil
			default:
				// EXCEPT returns it once, so it has to be remembered —
				// which takes room in the table like anything else.
				full, err := s.full()
				if err != nil {
					return nil, err
				}
				if full {
					if err := s.lover.add(s.keyBuf, row); err != nil {
						return nil, err
					}
					continue
				}
				s.counts[string(s.keyBuf)] = 0
				s.mem += len(s.keyBuf) + hashEntryBytes
				return row, nil
			}
			continue
		}
		switch {
		case s.all && n > 0:
			// One occurrence on the right is used up by this one.
			s.counts[string(s.keyBuf)] = n - 1
			if !s.except {
				return row, nil
			}
		case s.all:
			if s.except {
				return row, nil
			}
		case s.except:
			// On the right, or returned already.
		default:
			if n > 0 {
				s.counts[string(s.keyBuf)] = 0
				return row, nil
			}
		}
	}
}

func (s *setOpIter) dropFiles() {
	for _, f := range s.files {
		f.remove()
	}
	s.files = nil
}

func (s *setOpIter) close() {
	s.left.close()
	s.dropFiles()
	for _, p := range s.pending {
		p.left.remove()
		p.right.remove()
	}
	s.pending, s.counts = nil, nil
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
		v, err := be.eval(cx.child())
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

package sql

import (
	"math"
	"strconv"
	"strings"

	"github.com/duanjesus/capivaradb/internal/pgerr"
)

// Parse parses a query string holding zero or more ';'-separated statements.
//
// Statements are parsed by recursive descent; expressions use precedence
// climbing (a Pratt parser) with PostgreSQL's operator precedence.
func Parse(src string) ([]*Stmt, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var out []*Stmt
	for {
		for p.isOp(";") {
			p.i++
		}
		if p.peek().Kind == TEOF {
			return out, nil
		}
		p.maxParam = 0
		node, err := p.statement()
		if err != nil {
			return nil, err
		}
		if !p.isOp(";") && p.peek().Kind != TEOF {
			return nil, p.syntaxError()
		}
		out = append(out, &Stmt{Node: node, NumParams: p.maxParam})
	}
}

// maxDepth bounds the nesting of expressions and subqueries. The parser is
// recursive, so without a limit a query made of a hundred thousand opening
// parentheses would overflow the stack, which in Go kills the whole process.
const maxDepth = 250

type parser struct {
	toks     []Token
	i        int
	maxParam int
	depth    int
}

// reserved lists the keywords that cannot be used as a bare identifier or as
// an alias without AS. It is what lets "SELECT a FROM t" stop reading the
// select item at FROM instead of treating FROM as an alias. The list follows
// PostgreSQL's reserved and type_func_name keywords, limited to the ones this
// grammar knows.
var reserved = map[string]bool{
	"all": true, "and": true, "as": true, "asc": true, "case": true,
	"cast": true, "create": true, "cross": true, "default": true,
	"desc": true, "distinct": true, "else": true, "end": true,
	"except": true, "exists": true, "false": true, "from": true,
	"full": true, "group": true, "having": true, "ilike": true,
	"in": true, "inner": true, "intersect": true, "into": true,
	"is": true, "join": true, "left": true, "like": true, "limit": true,
	"natural": true, "not": true, "null": true, "offset": true, "on": true,
	"or": true, "order": true, "outer": true, "primary": true,
	"right": true, "select": true, "table": true, "then": true,
	"true": true, "union": true, "unique": true, "using": true,
	"when": true, "where": true,
	// Not reserved in PostgreSQL, but reserving it is the simplest way to
	// keep "UPDATE t SET ..." from reading SET as an alias for t.
	"set": true,
}

// IsReserved reports whether word cannot be used as a bare identifier.
func IsReserved(word string) bool { return reserved[word] }

func (p *parser) peek() Token { return p.toks[p.i] }

// peekAt looks n tokens ahead; past the end it returns the EOF token.
func (p *parser) peekAt(n int) Token {
	if p.i+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i+n]
}

func (p *parser) next() Token {
	t := p.toks[p.i]
	if t.Kind != TEOF {
		p.i++
	}
	return t
}

func (p *parser) isOp(op string) bool {
	t := p.toks[p.i]
	return t.Kind == TOp && t.Text == op
}

func (p *parser) isKw(kw string) bool {
	t := p.toks[p.i]
	return t.Kind == TIdent && t.Text == kw
}

// acceptKw consumes the keyword if it is next.
func (p *parser) acceptKw(kw string) bool {
	if p.isKw(kw) {
		p.i++
		return true
	}
	return false
}

func (p *parser) acceptOp(op string) bool {
	if p.isOp(op) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expectKw(kw string) error {
	if !p.acceptKw(kw) {
		return p.syntaxError()
	}
	return nil
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.syntaxError()
	}
	return nil
}

// syntaxError reports the current token the way PostgreSQL does.
func (p *parser) syntaxError() error {
	t := p.peek()
	if t.Kind == TEOF {
		return pgerr.New(pgerr.SyntaxError, "syntax error at end of input").At(t.Pos)
	}
	return pgerr.New(pgerr.SyntaxError, "syntax error at or near %q", t.Raw).At(t.Pos)
}

func (p *parser) unsupported(what string, pos int) error {
	return pgerr.New(pgerr.FeatureNotSupported, "%s is not supported", what).At(pos)
}

// enter and leave bracket every recursive production.
func (p *parser) enter() error {
	p.depth++
	if p.depth > maxDepth {
		return pgerr.New(pgerr.StatementTooComplex, "statement is nested too deeply").At(p.peek().Pos)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) isIdent() bool {
	t := p.peek()
	return t.Kind == TQuotedIdent || (t.Kind == TIdent && !reserved[t.Text])
}

func (p *parser) ident() (Ident, error) {
	if p.isIdent() {
		t := p.next()
		return Ident{Name: t.Text, Pos: t.Pos}, nil
	}
	return Ident{}, p.syntaxError()
}

// identList parses "( a, b, c )".
func (p *parser) identList() ([]Ident, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var ids []Ident
	for {
		id, err := p.ident()
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
		if !p.acceptOp(",") {
			break
		}
	}
	return ids, p.expectOp(")")
}

// optAlias parses "[AS] alias". After AS any word is an alias, even a
// reserved one; without AS a reserved word ends the item instead.
func (p *parser) optAlias() (string, error) {
	if p.acceptKw("as") {
		t := p.peek()
		if t.Kind != TIdent && t.Kind != TQuotedIdent {
			return "", p.syntaxError()
		}
		p.i++
		return t.Text, nil
	}
	if p.isIdent() {
		return p.next().Text, nil
	}
	return "", nil
}

func (p *parser) statement() (Node, error) {
	t := p.peek()
	if t.Kind != TIdent {
		return nil, p.syntaxError()
	}
	switch t.Text {
	case "select":
		return p.selectStmt()
	case "insert":
		return p.insertStmt()
	case "update":
		return p.updateStmt()
	case "delete":
		return p.deleteStmt()
	case "create":
		return p.createStmt()
	case "drop":
		return p.dropStmt()
	case "begin":
		p.i++
		p.txnNoise()
		return p.beginOptions()
	case "start":
		p.i++
		if err := p.expectKw("transaction"); err != nil {
			return nil, err
		}
		return p.beginOptions()
	case "commit", "end":
		p.i++
		p.txnNoise()
		return &Commit{}, nil
	case "rollback", "abort":
		p.i++
		p.txnNoise()
		return &Rollback{}, nil
	case "explain":
		return p.explainStmt()
	case "analyze":
		p.i++
		a := &Analyze{}
		if p.isIdent() {
			t := p.next()
			a.Table, a.Pos = t.Text, t.Pos
		}
		return a, nil
	case "vacuum":
		p.i++
		v := &Vacuum{}
		if p.isIdent() {
			t := p.next()
			v.Table, v.Pos = t.Text, t.Pos
		}
		return v, nil
	case "checkpoint":
		p.i++
		return &Checkpoint{}, nil
	case "set":
		return p.setStmt()
	case "show":
		return p.showStmt()
	}
	return nil, p.syntaxError()
}

// txnNoise skips the optional WORK / TRANSACTION keyword.
func (p *parser) txnNoise() {
	if !p.acceptKw("work") {
		p.acceptKw("transaction")
	}
}

// beginOptions parses the transaction modes that may follow BEGIN.
func (p *parser) beginOptions() (Node, error) {
	b := &Begin{}
	for {
		switch {
		case p.acceptKw("isolation"):
			level, err := p.isolationLevel()
			if err != nil {
				return nil, err
			}
			b.Isolation = level
		case p.acceptKw("read"):
			if !p.acceptKw("write") && !p.acceptKw("only") {
				return nil, p.syntaxError()
			}
		default:
			return b, nil
		}
		p.acceptOp(",")
	}
}

// isolationLevel parses "LEVEL <name>", ISOLATION having been consumed.
func (p *parser) isolationLevel() (string, error) {
	if err := p.expectKw("level"); err != nil {
		return "", err
	}
	switch {
	case p.acceptKw("serializable"):
		return "serializable", nil
	case p.acceptKw("repeatable"):
		return "repeatable read", p.expectKw("read")
	case p.acceptKw("read"):
		if p.acceptKw("committed") {
			return "read committed", nil
		}
		return "read uncommitted", p.expectKw("uncommitted")
	}
	return "", p.syntaxError()
}

// ---- SELECT ----

func (p *parser) selectStmt() (*Select, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	p.i++ // select
	s := &Select{}
	if p.acceptKw("distinct") {
		s.Distinct = true
	} else {
		p.acceptKw("all")
	}
	for {
		item, err := p.selectItem()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, item)
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("from") {
		from, err := p.fromList()
		if err != nil {
			return nil, err
		}
		s.From = from
	}
	var err error
	if s.Where, err = p.optWhere(); err != nil {
		return nil, err
	}
	if p.acceptKw("group") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		if s.GroupBy, err = p.exprList(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("having") {
		if s.Having, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("order") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			item, err := p.orderItem()
			if err != nil {
				return nil, err
			}
			s.OrderBy = append(s.OrderBy, item)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	// PostgreSQL accepts LIMIT and OFFSET in either order.
	for {
		switch {
		case s.Limit == nil && p.acceptKw("limit"):
			if p.acceptKw("all") {
				continue
			}
			if s.Limit, err = p.expr(0); err != nil {
				return nil, err
			}
		case s.Offset == nil && p.acceptKw("offset"):
			if s.Offset, err = p.expr(0); err != nil {
				return nil, err
			}
			if !p.acceptKw("rows") {
				p.acceptKw("row")
			}
		default:
			if t := p.peek(); t.Kind == TIdent && (t.Text == "union" || t.Text == "intersect" || t.Text == "except") {
				return nil, p.unsupported(strings.ToUpper(t.Text), t.Pos)
			}
			return s, nil
		}
	}
}

func (p *parser) selectItem() (SelectItem, error) {
	t := p.peek()
	if p.acceptOp("*") {
		return SelectItem{Star: true, Pos: t.Pos}, nil
	}
	// "t.*"
	if p.isIdent() {
		if dot, star := p.peekAt(1), p.peekAt(2); dot.Kind == TOp && dot.Text == "." && star.Kind == TOp && star.Text == "*" {
			p.i += 3
			return SelectItem{Star: true, Table: t.Text, Pos: t.Pos}, nil
		}
	}
	e, err := p.expr(0)
	if err != nil {
		return SelectItem{}, err
	}
	alias, err := p.optAlias()
	if err != nil {
		return SelectItem{}, err
	}
	return SelectItem{Expr: e, Alias: alias, Pos: t.Pos}, nil
}

func (p *parser) orderItem() (OrderItem, error) {
	e, err := p.expr(0)
	if err != nil {
		return OrderItem{}, err
	}
	item := OrderItem{Expr: e}
	if p.acceptKw("desc") {
		item.Desc = true
	} else {
		p.acceptKw("asc")
	}
	if p.acceptKw("nulls") {
		first := p.acceptKw("first")
		if !first {
			if err := p.expectKw("last"); err != nil {
				return OrderItem{}, err
			}
		}
		item.NullsFirst = &first
	}
	return item, nil
}

// fromList parses the comma-separated FROM list. "a, b" is a cross join.
func (p *parser) fromList() (TableExpr, error) {
	left, err := p.joinedTable()
	if err != nil {
		return nil, err
	}
	for p.isOp(",") {
		pos := p.next().Pos
		right, err := p.joinedTable()
		if err != nil {
			return nil, err
		}
		left = &Join{Kind: CrossJoin, Left: left, Right: right, Pos: pos}
	}
	return left, nil
}

// joinedTable parses a table followed by any number of joins, which
// associate to the left: a JOIN b JOIN c is (a JOIN b) JOIN c.
func (p *parser) joinedTable() (TableExpr, error) {
	left, err := p.tablePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		kind := InnerJoin
		switch {
		case p.acceptKw("join"):
		case p.acceptKw("inner"):
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
		case p.acceptKw("left"):
			p.acceptKw("outer")
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			kind = LeftJoin
		case p.acceptKw("cross"):
			if err := p.expectKw("join"); err != nil {
				return nil, err
			}
			kind = CrossJoin
		case p.isKw("right"), p.isKw("full"), p.isKw("natural"):
			return nil, p.unsupported(strings.ToUpper(t.Text)+" JOIN", t.Pos)
		default:
			return left, nil
		}
		right, err := p.tablePrimary()
		if err != nil {
			return nil, err
		}
		join := &Join{Kind: kind, Left: left, Right: right, Pos: t.Pos}
		if kind != CrossJoin {
			if p.isKw("using") {
				return nil, p.unsupported("JOIN ... USING", p.peek().Pos)
			}
			if err := p.expectKw("on"); err != nil {
				return nil, err
			}
			if join.On, err = p.expr(0); err != nil {
				return nil, err
			}
		}
		left = join
	}
}

// tablePrimary parses a table name, a subquery with its alias, or a
// parenthesised join.
func (p *parser) tablePrimary() (TableExpr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	t := p.peek()
	if !p.acceptOp("(") {
		ref, err := p.tableRef(true)
		if err != nil {
			return nil, err
		}
		return &ref, nil
	}
	if p.isKw("select") {
		sel, err := p.selectStmt()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		alias, err := p.optAlias()
		if err != nil {
			return nil, err
		}
		if alias == "" {
			return nil, pgerr.New(pgerr.SyntaxError, "subquery in FROM must have an alias").At(t.Pos)
		}
		return &DerivedTable{Select: sel, Alias: alias, Pos: t.Pos}, nil
	}
	inner, err := p.fromList()
	if err != nil {
		return nil, err
	}
	return inner, p.expectOp(")")
}

func (p *parser) tableRef(allowAlias bool) (TableRef, error) {
	id, err := p.ident()
	if err != nil {
		return TableRef{}, err
	}
	ref := TableRef{Name: id.Name, Alias: id.Name, Pos: id.Pos}
	if !allowAlias {
		return ref, nil
	}
	alias, err := p.optAlias()
	if err != nil {
		return TableRef{}, err
	}
	if alias != "" {
		ref.Alias = alias
	}
	return ref, nil
}

func (p *parser) optWhere() (Expr, error) {
	if !p.acceptKw("where") {
		return nil, nil
	}
	return p.expr(0)
}

func (p *parser) exprList() ([]Expr, error) {
	var list []Expr
	for {
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if !p.acceptOp(",") {
			return list, nil
		}
	}
}

// ---- data modification ----

func (p *parser) insertStmt() (Node, error) {
	p.i++ // insert
	if err := p.expectKw("into"); err != nil {
		return nil, err
	}
	ref, err := p.tableRef(false)
	if err != nil {
		return nil, err
	}
	ins := &Insert{Table: ref}
	// "(" here starts a column list unless it starts a parenthesised SELECT.
	if p.isOp("(") && !(p.peekAt(1).Kind == TIdent && p.peekAt(1).Text == "select") {
		if ins.Cols, err = p.identList(); err != nil {
			return nil, err
		}
	}
	if p.isKw("select") {
		if ins.Select, err = p.selectStmt(); err != nil {
			return nil, err
		}
		return ins, nil
	}
	if err := p.expectKw("values"); err != nil {
		return nil, err
	}
	for {
		pos := p.peek().Pos
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var row []Expr
		for {
			if t := p.peek(); p.acceptKw("default") {
				row = append(row, &DefaultValue{Pos: t.Pos})
			} else {
				e, err := p.expr(0)
				if err != nil {
					return nil, err
				}
				row = append(row, e)
			}
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		ins.Rows = append(ins.Rows, row)
		ins.RowPos = append(ins.RowPos, pos)
		if !p.acceptOp(",") {
			break
		}
	}
	return ins, nil
}

func (p *parser) updateStmt() (Node, error) {
	p.i++ // update
	ref, err := p.tableRef(true)
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("set"); err != nil {
		return nil, err
	}
	u := &Update{Table: ref}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		var val Expr
		if t := p.peek(); p.acceptKw("default") {
			val = &DefaultValue{Pos: t.Pos}
		} else if val, err = p.expr(0); err != nil {
			return nil, err
		}
		u.Sets = append(u.Sets, Assignment{Col: col, Value: val})
		if !p.acceptOp(",") {
			break
		}
	}
	if u.Where, err = p.optWhere(); err != nil {
		return nil, err
	}
	return u, nil
}

func (p *parser) deleteStmt() (Node, error) {
	p.i++ // delete
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	ref, err := p.tableRef(true)
	if err != nil {
		return nil, err
	}
	d := &Delete{Table: ref}
	if d.Where, err = p.optWhere(); err != nil {
		return nil, err
	}
	return d, nil
}

// ---- data definition ----

func (p *parser) createStmt() (Node, error) {
	p.i++ // create
	switch {
	case p.acceptKw("table"):
		return p.createTable()
	case p.acceptKw("unique"):
		if err := p.expectKw("index"); err != nil {
			return nil, err
		}
		return p.createIndex(true)
	case p.acceptKw("index"):
		return p.createIndex(false)
	}
	return nil, p.syntaxError()
}

func (p *parser) ifNotExists() (bool, error) {
	if !p.acceptKw("if") {
		return false, nil
	}
	if err := p.expectKw("not"); err != nil {
		return false, err
	}
	return true, p.expectKw("exists")
}

func (p *parser) createTable() (Node, error) {
	ct := &CreateTable{}
	var err error
	if ct.IfNotExists, err = p.ifNotExists(); err != nil {
		return nil, err
	}
	if ct.Name, err = p.ident(); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case p.isKw("primary"), p.isKw("unique"):
			p.i++
			tc := TableConstraint{PrimaryKey: t.Text == "primary", Pos: t.Pos}
			if tc.PrimaryKey {
				if err := p.expectKw("key"); err != nil {
					return nil, err
				}
			}
			if tc.Cols, err = p.identList(); err != nil {
				return nil, err
			}
			ct.Constraints = append(ct.Constraints, tc)
		default:
			def, err := p.columnDef()
			if err != nil {
				return nil, err
			}
			ct.Cols = append(ct.Cols, def)
		}
		if !p.acceptOp(",") {
			break
		}
	}
	return ct, p.expectOp(")")
}

func (p *parser) columnDef() (ColumnDef, error) {
	name, err := p.ident()
	if err != nil {
		return ColumnDef{}, err
	}
	typ, err := p.typeName()
	if err != nil {
		return ColumnDef{}, err
	}
	def := ColumnDef{Name: name, Type: typ}
	for {
		switch {
		case p.acceptKw("primary"):
			if err := p.expectKw("key"); err != nil {
				return ColumnDef{}, err
			}
			def.PrimaryKey = true
		case p.acceptKw("unique"):
			def.Unique = true
		case p.acceptKw("not"):
			if err := p.expectKw("null"); err != nil {
				return ColumnDef{}, err
			}
			def.NotNull = true
		case p.acceptKw("null"):
		case p.acceptKw("default"):
			if def.Default, err = p.expr(0); err != nil {
				return ColumnDef{}, err
			}
		default:
			return def, nil
		}
	}
}

func (p *parser) createIndex(unique bool) (Node, error) {
	ci := &CreateIndex{Unique: unique}
	var err error
	if ci.IfNotExists, err = p.ifNotExists(); err != nil {
		return nil, err
	}
	if ci.Name, err = p.ident(); err != nil {
		return nil, err
	}
	if err := p.expectKw("on"); err != nil {
		return nil, err
	}
	if ci.Table, err = p.tableRef(false); err != nil {
		return nil, err
	}
	// Each column may carry a direction, which is accepted and ignored: it
	// only matters for multi-column ordering of scans, decided by the planner.
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		ci.Cols = append(ci.Cols, col)
		if !p.acceptKw("asc") {
			p.acceptKw("desc")
		}
		if !p.acceptOp(",") {
			break
		}
	}
	return ci, p.expectOp(")")
}

func (p *parser) dropStmt() (Node, error) {
	p.i++ // drop
	index := p.acceptKw("index")
	if !index {
		if err := p.expectKw("table"); err != nil {
			return nil, err
		}
	}
	ifExists := false
	if p.acceptKw("if") {
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		ifExists = true
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	if index {
		return &DropIndex{Name: name, IfExists: ifExists}, nil
	}
	return &DropTable{Name: name, IfExists: ifExists}, nil
}

// ---- SET / SHOW ----

// setStmt parses SET [SESSION|LOCAL] name {=|TO} value [, ...], plus the
// special spellings drivers actually send.
func (p *parser) setStmt() (Node, error) {
	p.i++ // set
	session := p.acceptKw("session")
	if !session {
		p.acceptKw("local")
	}
	t := p.peek()
	if t.Kind != TIdent {
		return nil, p.syntaxError()
	}
	p.i++
	name := t.Text
	switch {
	case name == "time" && p.acceptKw("zone"):
		name = "timezone"
	case name == "names":
		name = "client_encoding"
	case name == "transaction" && p.acceptKw("isolation"):
		// SET TRANSACTION ISOLATION LEVEL ...
		level, err := p.isolationLevel()
		return &Set{Name: "transaction_isolation", Value: level}, err
	case session && name == "characteristics":
		// SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL ...,
		// which is what JDBC's setTransactionIsolation sends.
		for _, kw := range []string{"as", "transaction", "isolation"} {
			if err := p.expectKw(kw); err != nil {
				return nil, err
			}
		}
		level, err := p.isolationLevel()
		return &Set{Name: "default_transaction_isolation", Value: level}, err
	default:
		if !p.acceptOp("=") && !p.acceptKw("to") {
			return nil, p.syntaxError()
		}
	}
	var vals []string
	for {
		t := p.peek()
		switch t.Kind {
		case TIdent, TQuotedIdent, TString, TInt, TFloat:
			p.i++
			vals = append(vals, t.Text)
		default:
			return nil, p.syntaxError()
		}
		if !p.acceptOp(",") {
			break
		}
	}
	return &Set{Name: name, Value: strings.Join(vals, ", ")}, nil
}

func (p *parser) showStmt() (Node, error) {
	p.i++ // show
	t := p.peek()
	if t.Kind != TIdent {
		return nil, p.syntaxError()
	}
	p.i++
	name := t.Text
	switch {
	case name == "time" && p.acceptKw("zone"):
		name = "timezone"
	case name == "transaction" && p.acceptKw("isolation"):
		if err := p.expectKw("level"); err != nil {
			return nil, err
		}
		name = "transaction_isolation"
	}
	return &Show{Name: name, Pos: t.Pos}, nil
}

// ---- types ----

func (p *parser) typeName() (Type, error) {
	t := p.peek()
	if t.Kind != TIdent {
		return Unknown, p.syntaxError()
	}
	p.i++
	switch t.Text {
	case "int", "integer", "int4":
		return Int4, nil
	case "bigint", "int8":
		return Int8, nil
	case "bool", "boolean":
		return Bool, nil
	case "text":
		return Text, nil
	case "varchar":
		// The length limit is parsed and ignored: varchar(n) is stored as
		// text. See "Limitations" in the README.
		if p.acceptOp("(") {
			if p.peek().Kind != TInt {
				return Unknown, p.syntaxError()
			}
			p.i++
			if err := p.expectOp(")"); err != nil {
				return Unknown, err
			}
		}
		return Text, nil
	case "float", "float8", "real", "float4":
		// real is widened to double precision; there is no 4-byte float.
		return Float8, nil
	case "double":
		if err := p.expectKw("precision"); err != nil {
			return Unknown, err
		}
		return Float8, nil
	}
	return Unknown, pgerr.New(pgerr.UndefinedObject, "type %q does not exist", t.Raw).At(t.Pos)
}

// ---- expressions ----

// Binding powers, lowest to highest, mirroring PostgreSQL's precedence table.
const (
	bpOr      = 1
	bpAnd     = 2
	bpNot     = 3
	bpIs      = 4
	bpCompare = 5
	bpIn      = 6 // IN, BETWEEN, LIKE
	bpConcat  = 7
	bpAdd     = 8
	bpMul     = 9
	bpUnary   = 10
	bpCast    = 11
)

func isMembershipKw(t Token) bool {
	if t.Kind != TIdent {
		return false
	}
	switch t.Text {
	case "in", "between", "like", "ilike":
		return true
	}
	return false
}

// infixPower returns the binding power of the operator at the current
// position, or 0 if what follows cannot continue an expression.
func (p *parser) infixPower() int {
	t := p.peek()
	switch t.Kind {
	case TIdent:
		switch t.Text {
		case "or":
			return bpOr
		case "and":
			return bpAnd
		case "is":
			return bpIs
		case "not":
			// NOT is only an infix operator as part of NOT IN / NOT
			// BETWEEN / NOT LIKE.
			if isMembershipKw(p.peekAt(1)) {
				return bpIn
			}
		}
		if isMembershipKw(t) {
			return bpIn
		}
	case TOp:
		switch t.Text {
		case "=", "<>", "!=", "<", "<=", ">", ">=":
			return bpCompare
		case "||":
			return bpConcat
		case "+", "-":
			return bpAdd
		case "*", "/", "%":
			return bpMul
		case "::":
			return bpCast
		}
	}
	return 0
}

// expr parses an expression whose operators all bind tighter than minBP.
func (p *parser) expr(minBP int) (Expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	left, err := p.prefix()
	if err != nil {
		return nil, err
	}
	for {
		bp := p.infixPower()
		if bp == 0 || bp <= minBP {
			return left, nil
		}
		t := p.next()
		switch {
		case t.Kind == TIdent && t.Text == "is":
			not := p.acceptKw("not")
			if err := p.expectKw("null"); err != nil {
				return nil, err
			}
			left = &IsNull{X: left, Not: not, Pos: t.Pos}
		case t.Kind == TIdent && bp == bpIn:
			not := t.Text == "not"
			if not {
				t = p.next()
			}
			if left, err = p.membership(left, t, not); err != nil {
				return nil, err
			}
		case t.Text == "::":
			typ, err := p.typeName()
			if err != nil {
				return nil, err
			}
			left = &Cast{X: left, To: typ, Pos: t.Pos}
		default:
			// Every binary operator here is left-associative, so the right
			// operand may only contain operators that bind strictly tighter.
			right, err := p.expr(bp)
			if err != nil {
				return nil, err
			}
			op := t.Text
			if op == "!=" {
				op = "<>"
			}
			left = &Binary{Op: op, L: left, R: right, Pos: t.Pos}
		}
	}
}

// membership parses the rest of IN, BETWEEN or LIKE; kw is that keyword.
func (p *parser) membership(left Expr, kw Token, not bool) (Expr, error) {
	switch kw.Text {
	case "in":
		in := &In{X: left, Not: not, Pos: kw.Pos}
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var err error
		if p.isKw("select") {
			in.Sub, err = p.selectStmt()
		} else {
			in.List, err = p.exprList()
		}
		if err != nil {
			return nil, err
		}
		return in, p.expectOp(")")
	case "between":
		// The bounds are parsed above AND's precedence, so that the AND
		// separating them is not taken for a logical operator.
		lo, err := p.expr(bpIn)
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("and"); err != nil {
			return nil, err
		}
		hi, err := p.expr(bpIn)
		if err != nil {
			return nil, err
		}
		return &Between{X: left, Lo: lo, Hi: hi, Not: not, Pos: kw.Pos}, nil
	}
	pattern, err := p.expr(bpIn)
	if err != nil {
		return nil, err
	}
	return &Like{X: left, Pattern: pattern, Not: not, ILike: kw.Text == "ilike", Pos: kw.Pos}, nil
}

func (p *parser) prefix() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case TOp:
		switch t.Text {
		case "(":
			p.i++
			if p.isKw("select") {
				sel, err := p.selectStmt()
				if err != nil {
					return nil, err
				}
				return &SubqueryExpr{Select: sel, Pos: t.Pos}, p.expectOp(")")
			}
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			return e, p.expectOp(")")
		case "-", "+":
			p.i++
			// The smallest bigint has no positive counterpart, so its digits
			// only make sense together with the sign.
			if next := p.peek(); t.Text == "-" && next.Kind == TInt && next.Text == "9223372036854775808" {
				p.i++
				return &Literal{Val: int64(math.MinInt64), Type: Int8, Pos: t.Pos}, nil
			}
			x, err := p.expr(bpUnary)
			if err != nil {
				return nil, err
			}
			if t.Text == "+" {
				return x, nil
			}
			// Fold the sign into numeric literals so that -2147483648 is an
			// integer rather than the negation of an out-of-range integer.
			if lit, ok := x.(*Literal); ok {
				switch v := lit.Val.(type) {
				case int64:
					if v != math.MinInt64 {
						return intLiteral(-v, t.Pos), nil
					}
				case float64:
					return &Literal{Val: -v, Type: Float8, Pos: t.Pos}, nil
				}
			}
			return &Unary{Op: "-", X: x, Pos: t.Pos}, nil
		}
	case TInt:
		p.i++
		v, err := strconv.ParseInt(t.Text, 10, 64)
		if err != nil {
			return nil, pgerr.New(pgerr.NumericValueOutOfRange,
				"integer literal %s is out of range for type bigint", t.Text).At(t.Pos)
		}
		return intLiteral(v, t.Pos), nil
	case TFloat:
		p.i++
		v, err := strconv.ParseFloat(t.Text, 64)
		if err != nil {
			return nil, pgerr.New(pgerr.NumericValueOutOfRange,
				"%q is out of range for type double precision", t.Text).At(t.Pos)
		}
		return &Literal{Val: v, Type: Float8, Pos: t.Pos}, nil
	case TString:
		p.i++
		return &Literal{Val: t.Text, Type: Text, Pos: t.Pos}, nil
	case TParam:
		p.i++
		n, err := strconv.Atoi(t.Text)
		if err != nil || n < 1 || n > math.MaxUint16 {
			return nil, pgerr.New(pgerr.SyntaxError, "parameter number out of range: $%s", t.Text).At(t.Pos)
		}
		if n > p.maxParam {
			p.maxParam = n
		}
		return &Param{Index: n, Pos: t.Pos}, nil
	case TIdent:
		switch t.Text {
		case "true", "false":
			p.i++
			return &Literal{Val: t.Text == "true", Type: Bool, Pos: t.Pos}, nil
		case "null":
			p.i++
			return &Literal{Val: nil, Type: Unknown, Pos: t.Pos}, nil
		case "not":
			p.i++
			x, err := p.expr(bpNot)
			if err != nil {
				return nil, err
			}
			return &Unary{Op: "not", X: x, Pos: t.Pos}, nil
		case "case":
			return p.caseExpr()
		case "cast":
			return p.castExpr()
		case "exists":
			p.i++
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			if !p.isKw("select") {
				return nil, p.syntaxError()
			}
			sel, err := p.selectStmt()
			if err != nil {
				return nil, err
			}
			return &Exists{Select: sel, Pos: t.Pos}, p.expectOp(")")
		}
		if reserved[t.Text] {
			return nil, p.syntaxError()
		}
		return p.nameExpr()
	case TQuotedIdent:
		return p.nameExpr()
	}
	return nil, p.syntaxError()
}

func (p *parser) caseExpr() (Expr, error) {
	c := &Case{Pos: p.next().Pos}
	var err error
	if !p.isKw("when") {
		if c.Operand, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	for p.acceptKw("when") {
		var w When
		if w.Cond, err = p.expr(0); err != nil {
			return nil, err
		}
		if err := p.expectKw("then"); err != nil {
			return nil, err
		}
		if w.Then, err = p.expr(0); err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, w)
	}
	if len(c.Whens) == 0 {
		return nil, p.syntaxError()
	}
	if p.acceptKw("else") {
		if c.Else, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	return c, p.expectKw("end")
}

// castExpr parses CAST(x AS type), the standard spelling of x::type.
func (p *parser) castExpr() (Expr, error) {
	pos := p.next().Pos
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	x, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("as"); err != nil {
		return nil, err
	}
	typ, err := p.typeName()
	if err != nil {
		return nil, err
	}
	return &Cast{X: x, To: typ, Pos: pos}, p.expectOp(")")
}

// nameExpr parses what follows an identifier in expression position: a
// function call, a qualified column or a plain column.
func (p *parser) nameExpr() (Expr, error) {
	t := p.next()
	if t.Kind == TIdent && p.acceptOp("(") {
		call := &FuncCall{Name: t.Text, Pos: t.Pos}
		switch {
		case p.acceptOp(")"):
			return call, nil
		case p.acceptOp("*"):
			call.Star = true
			return call, p.expectOp(")")
		case p.acceptKw("distinct"):
			call.Distinct = true
		default:
			p.acceptKw("all")
		}
		var err error
		if call.Args, err = p.exprList(); err != nil {
			return nil, err
		}
		return call, p.expectOp(")")
	}
	if p.acceptOp(".") {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &ColumnRef{Table: t.Text, Name: col.Name, Pos: t.Pos}, nil
	}
	return &ColumnRef{Name: t.Text, Pos: t.Pos}, nil
}

// intLiteral types an integer constant the way PostgreSQL does: integer if
// it fits in 32 bits, bigint otherwise.
func intLiteral(v int64, pos int) *Literal {
	typ := Int8
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		typ = Int4
	}
	return &Literal{Val: v, Type: typ, Pos: pos}
}

// explainStmt parses EXPLAIN [ANALYZE] statement and the parenthesised form
// EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) statement.
func (p *parser) explainStmt() (Node, error) {
	ex := &Explain{Pos: p.next().Pos}
	onOff := func() (bool, error) {
		switch {
		case p.acceptKw("off"), p.acceptKw("false"):
			return false, nil
		case p.acceptKw("on"), p.acceptKw("true"):
			return true, nil
		}
		// A bare option name means "on".
		if p.isOp(",") || p.isOp(")") {
			return true, nil
		}
		return false, p.syntaxError()
	}
	if p.acceptOp("(") {
		for {
			var err error
			var on bool
			switch {
			case p.acceptKw("analyze"):
				ex.Analyze, err = onOff()
			case p.acceptKw("costs"):
				on, err = onOff()
				ex.NoCosts = !on
			case p.acceptKw("timing"):
				on, err = onOff()
				ex.NoTiming = !on
			default:
				err = p.syntaxError()
			}
			if err != nil {
				return nil, err
			}
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	} else if p.acceptKw("analyze") {
		ex.Analyze = true
	}
	// Only statements that have a plan can be explained, as in PostgreSQL.
	// Besides being meaningless, "explain (costs on) analyze t" would print
	// as "explain analyze t", which is a different statement.
	start := p.peek()
	stmt, err := p.statement()
	if err != nil {
		return nil, err
	}
	switch stmt.(type) {
	case *Select, *Insert, *Update, *Delete:
	default:
		return nil, pgerr.New(pgerr.SyntaxError, "syntax error at or near %q", start.Raw).At(start.Pos)
	}
	ex.Stmt = stmt
	return ex, nil
}

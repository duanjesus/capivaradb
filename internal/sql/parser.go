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

type parser struct {
	toks     []Token
	i        int
	maxParam int
}

// reserved lists the keywords that cannot be used as a bare identifier or as
// an alias without AS. It is what lets "SELECT a FROM t" stop reading the
// select item at FROM instead of treating FROM as an alias.
var reserved = map[string]bool{
	"select": true, "from": true, "where": true, "and": true, "or": true,
	"not": true, "null": true, "true": true, "false": true, "is": true,
	"as": true, "into": true, "create": true, "table": true, "primary": true,
	"group": true, "order": true, "limit": true, "offset": true, "join": true,
	"on": true, "inner": true, "left": true, "right": true, "having": true,
	"union": true, "distinct": true, "default": true,
	// Not reserved in PostgreSQL, but reserving it is the simplest way to
	// keep "UPDATE t SET ..." from reading SET as an alias for t.
	"set": true,
}

func (p *parser) peek() Token { return p.toks[p.i] }

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

func (p *parser) ident() (Ident, error) {
	t := p.peek()
	if t.Kind == TQuotedIdent || (t.Kind == TIdent && !reserved[t.Text]) {
		p.i++
		return Ident{Name: t.Text, Pos: t.Pos}, nil
	}
	return Ident{}, p.syntaxError()
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
		return p.createTable()
	case "drop":
		return p.dropTable()
	case "begin":
		p.i++
		p.txnNoise()
		return &Begin{}, nil
	case "start":
		p.i++
		if err := p.expectKw("transaction"); err != nil {
			return nil, err
		}
		return &Begin{}, nil
	case "commit", "end":
		p.i++
		p.txnNoise()
		return &Commit{}, nil
	case "rollback", "abort":
		p.i++
		p.txnNoise()
		return &Rollback{}, nil
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

func (p *parser) selectStmt() (Node, error) {
	p.i++ // select
	s := &Select{}
	for {
		if p.acceptOp("*") {
			s.Items = append(s.Items, SelectItem{Star: true})
		} else {
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			item := SelectItem{Expr: e}
			if p.acceptKw("as") {
				// After AS any word is an alias, even a reserved one.
				t := p.peek()
				if t.Kind != TIdent && t.Kind != TQuotedIdent {
					return nil, p.syntaxError()
				}
				p.i++
				item.Alias = t.Text
			} else if t := p.peek(); t.Kind == TQuotedIdent || (t.Kind == TIdent && !reserved[t.Text]) {
				p.i++
				item.Alias = t.Text
			}
			s.Items = append(s.Items, item)
		}
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("from") {
		ref, err := p.tableRef(true)
		if err != nil {
			return nil, err
		}
		s.From = &ref
	}
	where, err := p.optWhere()
	if err != nil {
		return nil, err
	}
	s.Where = where
	return s, nil
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
	if p.acceptKw("as") {
		a, err := p.ident()
		if err != nil {
			return TableRef{}, err
		}
		ref.Alias = a.Name
	} else if t := p.peek(); t.Kind == TQuotedIdent || (t.Kind == TIdent && !reserved[t.Text]) {
		p.i++
		ref.Alias = t.Text
	}
	return ref, nil
}

func (p *parser) optWhere() (Expr, error) {
	if !p.acceptKw("where") {
		return nil, nil
	}
	return p.expr(0)
}

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
	if p.acceptOp("(") {
		for {
			id, err := p.ident()
			if err != nil {
				return nil, err
			}
			ins.Cols = append(ins.Cols, id)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
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
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			row = append(row, e)
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
		val, err := p.expr(0)
		if err != nil {
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

func (p *parser) createTable() (Node, error) {
	p.i++ // create
	if err := p.expectKw("table"); err != nil {
		return nil, err
	}
	ct := &CreateTable{}
	if p.acceptKw("if") {
		if err := p.expectKw("not"); err != nil {
			return nil, err
		}
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		ct.IfNotExists = true
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	ct.Name = name
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		typ, err := p.typeName()
		if err != nil {
			return nil, err
		}
		def := ColumnDef{Name: col, Type: typ}
	constraints:
		for {
			switch {
			case p.acceptKw("primary"):
				if err := p.expectKw("key"); err != nil {
					return nil, err
				}
				def.PrimaryKey = true
			case p.acceptKw("not"):
				if err := p.expectKw("null"); err != nil {
					return nil, err
				}
				def.NotNull = true
			case p.acceptKw("null"):
			default:
				break constraints
			}
		}
		ct.Cols = append(ct.Cols, def)
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return ct, nil
}

func (p *parser) dropTable() (Node, error) {
	p.i++ // drop
	if err := p.expectKw("table"); err != nil {
		return nil, err
	}
	dt := &DropTable{}
	if p.acceptKw("if") {
		if err := p.expectKw("exists"); err != nil {
			return nil, err
		}
		dt.IfExists = true
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	dt.Name = name
	return dt, nil
}

// setStmt parses SET [SESSION|LOCAL] name {=|TO} value [, ...], plus the two
// spellings drivers actually send: SET TIME ZONE and SET NAMES.
func (p *parser) setStmt() (Node, error) {
	p.i++ // set
	if !p.acceptKw("session") {
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
	case "float", "float8":
		return Float8, nil
	case "double":
		if err := p.expectKw("precision"); err != nil {
			return Unknown, err
		}
		return Float8, nil
	}
	return Unknown, pgerr.New(pgerr.UndefinedObject, "type %q does not exist", t.Raw).At(t.Pos)
}

// Binding powers, lowest to highest, mirroring PostgreSQL's precedence table.
const (
	bpOr      = 1
	bpAnd     = 2
	bpNot     = 3
	bpIs      = 4
	bpCompare = 5
	bpConcat  = 6
	bpAdd     = 7
	bpMul     = 8
	bpUnary   = 9
	bpCast    = 10
)

func infixPower(t Token) int {
	switch t.Kind {
	case TIdent:
		switch t.Text {
		case "or":
			return bpOr
		case "and":
			return bpAnd
		case "is":
			return bpIs
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
	left, err := p.prefix()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		bp := infixPower(t)
		if bp == 0 || bp <= minBP {
			return left, nil
		}
		p.i++
		switch {
		case t.Kind == TIdent && t.Text == "is":
			not := p.acceptKw("not")
			if err := p.expectKw("null"); err != nil {
				return nil, err
			}
			left = &IsNull{X: left, Not: not, Pos: t.Pos}
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

func (p *parser) prefix() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case TOp:
		switch t.Text {
		case "(":
			p.i++
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return e, nil
		case "-", "+":
			p.i++
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
					return intLiteral(-v, t.Pos), nil
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

// nameExpr parses what follows an identifier in expression position: a
// function call, a qualified column or a plain column.
func (p *parser) nameExpr() (Expr, error) {
	t := p.next()
	if t.Kind == TIdent && p.acceptOp("(") {
		call := &FuncCall{Name: t.Text, Pos: t.Pos}
		if p.acceptOp(")") {
			return call, nil
		}
		for {
			arg, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			call.Args = append(call.Args, arg)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return call, nil
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

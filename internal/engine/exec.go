package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/sql"
)

// prepared implements pgwire.Prepared.
type prepared struct {
	paramOIDs []uint32
	cols      []pgwire.Column
	run       func(ctx context.Context, params []any) (*result, error)
}

func (p *prepared) ParamOIDs() []uint32      { return p.paramOIDs }
func (p *prepared) Columns() []pgwire.Column { return p.cols }

func (p *prepared) Execute(ctx context.Context, params []any) (pgwire.Rows, error) {
	return p.run(ctx, params)
}

// result implements pgwire.Rows over fully materialised rows.
type result struct {
	rows [][]any
	next int
	tag  string
}

func (r *result) Next(ctx context.Context) ([]any, error) {
	if r.next >= len(r.rows) {
		return nil, io.EOF
	}
	r.next++
	return r.rows[r.next-1], nil
}

func (r *result) Tag() string { return r.tag }
func (r *result) Close()      {}

func typeOfOID(oid uint32) (sql.Type, bool) {
	switch oid {
	case 0, pgwire.OIDUnknown:
		return sql.Unknown, true
	case pgwire.OIDBool:
		return sql.Bool, true
	case pgwire.OIDInt2, pgwire.OIDInt4:
		return sql.Int4, true
	case pgwire.OIDInt8:
		return sql.Int8, true
	case pgwire.OIDFloat4, pgwire.OIDFloat8:
		return sql.Float8, true
	case pgwire.OIDText, pgwire.OIDVarchar, pgwire.OIDBPChar, pgwire.OIDName:
		return sql.Text, true
	}
	return sql.Unknown, false
}

func oidOf(t sql.Type) uint32 {
	switch t {
	case sql.Bool:
		return pgwire.OIDBool
	case sql.Int4:
		return pgwire.OIDInt4
	case sql.Int8:
		return pgwire.OIDInt8
	case sql.Float8:
		return pgwire.OIDFloat8
	}
	return pgwire.OIDText
}

// Prepare implements pgwire.Session.
func (s *Session) Prepare(st pgwire.Stmt, oids []uint32) (pgwire.Prepared, error) {
	stmt := st.(*sql.Stmt)
	if s.failed {
		switch stmt.Node.(type) {
		case *sql.Commit, *sql.Rollback:
		default:
			return nil, pgerr.New(pgerr.InFailedSQLTransaction,
				"current transaction is aborted, commands ignored until end of transaction block")
		}
	}

	n := max(stmt.NumParams, len(oids))
	ptypes := make([]sql.Type, n)
	for i, oid := range oids {
		t, ok := typeOfOID(oid)
		if !ok {
			return nil, pgerr.New(pgerr.FeatureNotSupported, "parameter $%d has unsupported type OID %d", i+1, oid)
		}
		ptypes[i] = t
	}

	// Parameter types are inferred from how each parameter is used, and a
	// use late in the statement can determine the type of an earlier one.
	// So statements with parameters are bound twice: the first pass only
	// collects types, the second compiles with every type known. A
	// parameter no context gives a type to is text, as in PostgreSQL.
	if n > 0 {
		if _, err := s.build(stmt.Node, &binder{sess: s, ptypes: ptypes}); err != nil {
			return nil, err
		}
		for i, t := range ptypes {
			if t == sql.Unknown {
				ptypes[i] = sql.Text
			}
		}
	}
	p, err := s.build(stmt.Node, &binder{sess: s, ptypes: ptypes})
	if err != nil {
		return nil, err
	}

	// The client decodes nothing here, but it encodes parameters according
	// to these OIDs, so a type it declared itself is echoed back verbatim
	// (int2 stays int2) rather than replaced by our internal equivalent.
	p.paramOIDs = make([]uint32, n)
	for i := range p.paramOIDs {
		if i < len(oids) && oids[i] != 0 && oids[i] != pgwire.OIDUnknown {
			p.paramOIDs[i] = oids[i]
		} else {
			p.paramOIDs[i] = oidOf(ptypes[i])
		}
	}
	return p, nil
}

func (s *Session) build(node sql.Node, b *binder) (*prepared, error) {
	switch n := node.(type) {
	case *sql.Select:
		return s.buildSelect(n, b)
	case *sql.Insert:
		return s.buildInsert(n, b)
	case *sql.Update:
		return s.buildUpdate(n, b)
	case *sql.Delete:
		return s.buildDelete(n, b)
	case *sql.CreateTable:
		return s.buildCreateTable(n)
	case *sql.DropTable:
		return s.buildDropTable(n)
	case *sql.Begin:
		return command(func() string { s.begin(); return "BEGIN" }), nil
	case *sql.Commit:
		return command(func() string {
			// COMMIT of a failed transaction rolls it back and says so.
			if s.failed {
				s.rollback()
				return "ROLLBACK"
			}
			s.commit()
			return "COMMIT"
		}), nil
	case *sql.Rollback:
		return command(func() string { s.rollback(); return "ROLLBACK" }), nil
	case *sql.Set:
		return command(func() string { s.vars[n.Name] = n.Value; return "SET" }), nil
	case *sql.Show:
		return s.buildShow(n)
	}
	return nil, pgerr.New(pgerr.InternalError, "unhandled statement node %T", node)
}

// command wraps a statement that cannot fail and returns no rows.
func command(fn func() string) *prepared {
	return &prepared{run: func(context.Context, []any) (*result, error) {
		return &result{tag: fn()}, nil
	}}
}

func (s *Session) buildShow(n *sql.Show) (*prepared, error) {
	if _, ok := s.vars[n.Name]; !ok {
		return nil, pgerr.New(pgerr.UndefinedObject, "unrecognized configuration parameter %q", n.Name).At(n.Pos)
	}
	return &prepared{
		cols: []pgwire.Column{{Name: n.Name, OID: pgwire.OIDText}},
		run: func(context.Context, []any) (*result, error) {
			return &result{rows: [][]any{{s.vars[n.Name]}}, tag: "SHOW"}, nil
		},
	}, nil
}

// bindTable resolves the table a statement works on and points the binder
// at it.
func (s *Session) bindTable(ref sql.TableRef, b *binder) (*table, error) {
	s.db.mu.RLock()
	t, err := s.db.lookup(ref)
	s.db.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	b.tbl, b.alias = t, ref.Alias
	return t, nil
}

// bindWhere binds an optional WHERE clause, which must be boolean.
func (b *binder) bindWhere(e sql.Expr) (evalFn, error) {
	if e == nil {
		return nil, nil
	}
	w, err := b.bind(e, sql.Bool)
	if err != nil {
		return nil, err
	}
	if w.typ != sql.Bool && w.typ != sql.Unknown {
		return nil, pgerr.New(pgerr.DatatypeMismatch,
			"argument of WHERE must be type boolean, not type %s", w.typ).At(e.Position())
	}
	return w.eval, nil
}

// matches evaluates a WHERE clause; NULL counts as false.
func matches(where evalFn, en *env) (bool, error) {
	if where == nil {
		return true, nil
	}
	v, err := where(en)
	if err != nil {
		return false, err
	}
	ok, _ := v.(bool)
	return ok, nil
}

// columnName picks the name of a result column the way PostgreSQL does.
func columnName(e sql.Expr) string {
	switch e := e.(type) {
	case *sql.ColumnRef:
		return e.Name
	case *sql.FuncCall:
		return e.Name
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

func (s *Session) buildSelect(n *sql.Select, b *binder) (*prepared, error) {
	var t *table
	if n.From != nil {
		var err error
		if t, err = s.bindTable(*n.From, b); err != nil {
			return nil, err
		}
	}

	var cols []pgwire.Column
	var exprs []evalFn
	for _, item := range n.Items {
		if item.Star {
			if t == nil {
				return nil, pgerr.New(pgerr.SyntaxError, "SELECT * with no tables specified is not valid")
			}
			for i, c := range t.cols {
				cols = append(cols, pgwire.Column{Name: c.name, OID: oidOf(c.typ)})
				exprs = append(exprs, func(en *env) (any, error) { return en.row[i], nil })
			}
			continue
		}
		be, err := b.bind(item.Expr, sql.Unknown)
		if err != nil {
			return nil, err
		}
		name := item.Alias
		if name == "" {
			name = columnName(item.Expr)
		}
		cols = append(cols, pgwire.Column{Name: name, OID: oidOf(be.typ)})
		exprs = append(exprs, be.eval)
	}
	where, err := b.bindWhere(n.Where)
	if err != nil {
		return nil, err
	}

	run := func(ctx context.Context, params []any) (*result, error) {
		// Without FROM a query produces exactly one row.
		input := [][]any{nil}
		if t != nil {
			s.db.mu.RLock()
			if err := s.db.stillCurrent(t); err != nil {
				s.db.mu.RUnlock()
				return nil, err
			}
			input = make([][]any, len(t.rows))
			for i, r := range t.rows {
				input[i] = r.vals
			}
			s.db.mu.RUnlock()
		}
		en := &env{ctx: ctx, params: params}
		out := make([][]any, 0, len(input))
		for _, vals := range input {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			en.row = vals
			ok, err := matches(where, en)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			rec := make([]any, len(exprs))
			for i, eval := range exprs {
				if rec[i], err = eval(en); err != nil {
					return nil, err
				}
			}
			out = append(out, rec)
		}
		return &result{rows: out, tag: fmt.Sprintf("SELECT %d", len(out))}, nil
	}
	return &prepared{cols: cols, run: run}, nil
}

// assignment is a value expression targeting one column.
type assignment struct {
	col  int
	eval evalFn
	conv convFn
}

// bindValue binds e as a value to be stored in column c, which is at
// position idx of its table.
func (b *binder) bindValue(c column, idx int, e sql.Expr) (assignment, error) {
	be, err := b.bind(e, c.typ)
	if err != nil {
		return assignment{}, err
	}
	conv := assignFn(be.typ, c.typ)
	if conv == nil {
		return assignment{}, pgerr.New(pgerr.DatatypeMismatch,
			"column %q is of type %s but expression is of type %s", c.name, c.typ, be.typ).At(e.Position())
	}
	return assignment{col: idx, eval: be.eval, conv: conv}, nil
}

func (a assignment) value(en *env) (any, error) {
	v, err := a.eval(en)
	if err != nil || v == nil {
		return nil, err
	}
	return a.conv(v)
}

// checkConstraints validates a new row version. self is the row being
// replaced on UPDATE, or nil on INSERT. The caller must hold db.mu.
func (t *table) checkConstraints(vals []any, self *row) error {
	for i, c := range t.cols {
		if (c.notNull || c.pk) && vals[i] == nil {
			return pgerr.New(pgerr.NotNullViolation,
				"null value in column %q of relation %q violates not-null constraint", c.name, t.name)
		}
		if !c.pk {
			continue
		}
		// Linear scan: there are no indexes until the B+tree lands.
		for _, r := range t.rows {
			if r != self && r.vals[i] == vals[i] {
				return pgerr.New(pgerr.UniqueViolation,
					"duplicate key value violates unique constraint %q", t.name+"_pkey").
					WithDetail("Key (%s)=(%s) already exists.", c.name, pgwire.TextValue(vals[i]))
			}
		}
	}
	return nil
}

func (s *Session) buildInsert(n *sql.Insert, b *binder) (*prepared, error) {
	t, err := s.bindTable(n.Table, b)
	if err != nil {
		return nil, err
	}
	// Column references make no sense in VALUES: take the table back out of
	// scope so that they are reported as unknown columns.
	b.tbl = nil

	target := make([]int, 0, len(t.cols))
	if n.Cols == nil {
		for i := range t.cols {
			target = append(target, i)
		}
	} else {
		seen := make(map[int]bool)
		for _, c := range n.Cols {
			i := t.colIndex(c.Name)
			if i < 0 {
				return nil, pgerr.New(pgerr.UndefinedColumn,
					"column %q of relation %q does not exist", c.Name, t.name).At(c.Pos)
			}
			if seen[i] {
				return nil, pgerr.New(pgerr.DuplicateColumn, "column %q specified more than once", c.Name).At(c.Pos)
			}
			seen[i] = true
			target = append(target, i)
		}
	}

	rows := make([][]assignment, len(n.Rows))
	for ri, exprs := range n.Rows {
		if len(exprs) > len(target) {
			return nil, pgerr.New(pgerr.SyntaxError, "INSERT has more expressions than target columns").At(n.RowPos[ri])
		}
		// With an explicit column list every listed column needs a value.
		// Without one, trailing columns may be omitted and are NULL.
		if len(exprs) < len(target) && n.Cols != nil {
			return nil, pgerr.New(pgerr.SyntaxError, "INSERT has more target columns than expressions").At(n.RowPos[ri])
		}
		for i, e := range exprs {
			a, err := b.bindValue(t.cols[target[i]], target[i], e)
			if err != nil {
				return nil, err
			}
			rows[ri] = append(rows[ri], a)
		}
	}

	run := func(ctx context.Context, params []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			if err := s.db.stillCurrent(t); err != nil {
				return "", err
			}
			en := &env{ctx: ctx, params: params}
			for _, assigns := range rows {
				vals := make([]any, len(t.cols))
				for _, a := range assigns {
					v, err := a.value(en)
					if err != nil {
						return "", err
					}
					vals[a.col] = v
				}
				if err := t.checkConstraints(vals, nil); err != nil {
					return "", err
				}
				r := &row{vals: vals}
				t.rows = append(t.rows, r)
				*undo = append(*undo, func() { t.remove(r) })
			}
			return fmt.Sprintf("INSERT 0 %d", len(rows)), nil
		})
	}
	return &prepared{run: run}, nil
}

// remove deletes r from the table. The caller must hold db.mu.
func (t *table) remove(r *row) {
	for i := len(t.rows) - 1; i >= 0; i-- {
		if t.rows[i] == r {
			t.rows = append(t.rows[:i], t.rows[i+1:]...)
			return
		}
	}
}

func (s *Session) buildUpdate(n *sql.Update, b *binder) (*prepared, error) {
	t, err := s.bindTable(n.Table, b)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool)
	assigns := make([]assignment, len(n.Sets))
	for i, set := range n.Sets {
		idx := t.colIndex(set.Col.Name)
		if idx < 0 {
			return nil, pgerr.New(pgerr.UndefinedColumn,
				"column %q of relation %q does not exist", set.Col.Name, t.name).At(set.Col.Pos)
		}
		if seen[idx] {
			return nil, pgerr.New(pgerr.SyntaxError, "multiple assignments to same column %q", set.Col.Name).At(set.Col.Pos)
		}
		seen[idx] = true
		if assigns[i], err = b.bindValue(t.cols[idx], idx, set.Value); err != nil {
			return nil, err
		}
	}
	where, err := b.bindWhere(n.Where)
	if err != nil {
		return nil, err
	}

	run := func(ctx context.Context, params []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			if err := s.db.stillCurrent(t); err != nil {
				return "", err
			}
			en := &env{ctx: ctx, params: params}
			count := 0
			for _, r := range t.rows {
				en.row = r.vals
				ok, err := matches(where, en)
				if err != nil {
					return "", err
				}
				if !ok {
					continue
				}
				// Every SET expression sees the row as it was before the
				// update, so "SET a = b, b = a" swaps the two columns.
				vals := append([]any(nil), r.vals...)
				for _, a := range assigns {
					if vals[a.col], err = a.value(en); err != nil {
						return "", err
					}
				}
				if err := t.checkConstraints(vals, r); err != nil {
					return "", err
				}
				old := r.vals
				r.vals = vals
				*undo = append(*undo, func() { r.vals = old })
				count++
			}
			return fmt.Sprintf("UPDATE %d", count), nil
		})
	}
	return &prepared{run: run}, nil
}

func (s *Session) buildDelete(n *sql.Delete, b *binder) (*prepared, error) {
	t, err := s.bindTable(n.Table, b)
	if err != nil {
		return nil, err
	}
	where, err := b.bindWhere(n.Where)
	if err != nil {
		return nil, err
	}

	run := func(ctx context.Context, params []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			if err := s.db.stillCurrent(t); err != nil {
				return "", err
			}
			en := &env{ctx: ctx, params: params}
			type removed struct {
				at int
				r  *row
			}
			var gone []removed
			kept := make([]*row, 0, len(t.rows))
			for i, r := range t.rows {
				en.row = r.vals
				ok, err := matches(where, en)
				if err != nil {
					return "", err
				}
				if ok {
					gone = append(gone, removed{i, r})
				} else {
					kept = append(kept, r)
				}
			}
			t.rows = kept
			// Undo puts the rows back where they were, lowest position
			// first so that each insertion index is still valid.
			*undo = append(*undo, func() {
				for _, g := range gone {
					at := min(g.at, len(t.rows))
					t.rows = append(t.rows, nil)
					copy(t.rows[at+1:], t.rows[at:])
					t.rows[at] = g.r
				}
			})
			return fmt.Sprintf("DELETE %d", len(gone)), nil
		})
	}
	return &prepared{run: run}, nil
}

func (s *Session) buildCreateTable(n *sql.CreateTable) (*prepared, error) {
	t := &table{name: n.Name.Name}
	primaryKeys := 0
	for _, c := range n.Cols {
		if t.colIndex(c.Name.Name) >= 0 {
			return nil, pgerr.New(pgerr.DuplicateColumn, "column %q specified more than once", c.Name.Name).At(c.Name.Pos)
		}
		if c.PrimaryKey {
			primaryKeys++
		}
		t.cols = append(t.cols, column{name: c.Name.Name, typ: c.Type, notNull: c.NotNull, pk: c.PrimaryKey})
	}
	if primaryKeys > 1 {
		return nil, pgerr.New("42P16", "multiple primary keys for table %q are not allowed", t.name).At(n.Name.Pos)
	}

	run := func(context.Context, []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			if _, exists := s.db.tables[t.name]; exists {
				if n.IfNotExists {
					return "CREATE TABLE", nil
				}
				return "", pgerr.New(pgerr.DuplicateTable, "relation %q already exists", t.name)
			}
			// A fresh table per execution: a prepared CREATE TABLE that is
			// run again after a DROP must not resurrect the old rows.
			created := &table{name: t.name, cols: t.cols}
			s.db.tables[t.name] = created
			*undo = append(*undo, func() {
				if s.db.tables[t.name] == created {
					delete(s.db.tables, t.name)
				}
			})
			return "CREATE TABLE", nil
		})
	}
	return &prepared{run: run}, nil
}

func (s *Session) buildDropTable(n *sql.DropTable) (*prepared, error) {
	name := n.Name.Name
	run := func(context.Context, []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			t, exists := s.db.tables[name]
			if !exists {
				if n.IfExists {
					return "DROP TABLE", nil
				}
				return "", pgerr.New(pgerr.UndefinedTable, "table %q does not exist", name)
			}
			delete(s.db.tables, name)
			*undo = append(*undo, func() {
				if _, taken := s.db.tables[name]; !taken {
					s.db.tables[name] = t
				}
			})
			return "DROP TABLE", nil
		})
	}
	return &prepared{run: run}, nil
}

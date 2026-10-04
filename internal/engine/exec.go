package engine

import (
	"context"
	"fmt"
	"io"
	"strings"

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
		if _, err := s.build(stmt.Node, ptypes); err != nil {
			return nil, err
		}
		for i, t := range ptypes {
			if t == sql.Unknown {
				ptypes[i] = sql.Text
			}
		}
	}
	p, err := s.build(stmt.Node, ptypes)
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

func (s *Session) build(node sql.Node, ptypes []sql.Type) (*prepared, error) {
	switch n := node.(type) {
	case *sql.Select:
		plan, err := s.planSelect(n, nil, ptypes)
		if err != nil {
			return nil, err
		}
		return &prepared{cols: plan.cols, run: func(ctx context.Context, params []any) (*result, error) {
			rows, err := plan.run(&env{ctx: ctx, params: params})
			if err != nil {
				return nil, err
			}
			return &result{rows: rows, tag: fmt.Sprintf("SELECT %d", len(rows))}, nil
		}}, nil
	case *sql.Insert:
		return s.buildInsert(n, ptypes)
	case *sql.Update:
		return s.buildUpdate(n, ptypes)
	case *sql.Delete:
		return s.buildDelete(n, ptypes)
	case *sql.CreateTable:
		return s.buildCreateTable(n, ptypes)
	case *sql.DropTable:
		return s.buildDropTable(n)
	case *sql.CreateIndex:
		return s.buildCreateIndex(n)
	case *sql.DropIndex:
		return s.buildDropIndex(n)
	case *sql.Begin:
		// The requested isolation level is accepted and ignored: there is
		// only one behaviour until MVCC exists, and SHOW reports it.
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
		return command(func() string {
			// Isolation settings are parsed for the benefit of drivers,
			// but must not overwrite the honest answer SHOW gives.
			if !strings.HasSuffix(n.Name, "transaction_isolation") {
				s.vars[n.Name] = n.Value
			}
			return "SET"
		}), nil
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

// targetTable resolves the table a data-modifying statement works on and
// returns a binder with its columns in scope.
func (s *Session) targetTable(ref sql.TableRef, ptypes []sql.Type) (*table, *binder, error) {
	s.db.mu.RLock()
	t, err := s.db.lookup(ref)
	s.db.mu.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	return t, &binder{sess: s, scope: &scope{cols: t.scopeCols(ref.Alias)}, ptypes: ptypes}, nil
}

// assignment computes the value to store in one column.
type assignment struct {
	col  int
	eval evalFn // already converted to the column type
}

// bindValue binds e as a value to be stored in column idx of t.
func (b *binder) bindValue(t *table, idx int, e sql.Expr) (assignment, error) {
	c := t.cols[idx]
	if _, isDefault := e.(*sql.DefaultValue); isDefault {
		def := c.def
		if def == nil {
			def = null
		}
		return assignment{col: idx, eval: def}, nil
	}
	be, err := b.bind(e, c.typ)
	if err != nil {
		return assignment{}, err
	}
	eval, err := storeAs(be, c, e.Position())
	return assignment{col: idx, eval: eval}, err
}

// storeAs wraps an expression with the implicit conversion to a column's type.
func storeAs(be bound, c column, pos int) (evalFn, error) {
	conv := assignFn(be.typ, c.typ)
	if conv == nil {
		return nil, pgerr.New(pgerr.DatatypeMismatch,
			"column %q is of type %s but expression is of type %s", c.name, c.typ, be.typ).At(pos)
	}
	return func(en *env) (any, error) {
		v, err := be.eval(en)
		if err != nil || v == nil {
			return nil, err
		}
		return conv(v)
	}, nil
}

// checkConstraints validates a new row version. self is the row being
// replaced on UPDATE, or nil on INSERT. The caller must hold db.mu.
func (t *table) checkConstraints(vals []any, self *row) error {
	for i, c := range t.cols {
		if c.notNull && vals[i] == nil {
			return pgerr.New(pgerr.NotNullViolation,
				"null value in column %q of relation %q violates not-null constraint", c.name, t.name)
		}
	}
next:
	for _, u := range t.uniques {
		unchanged := self != nil
		for _, c := range u.cols {
			// NULLs are distinct from each other: a key containing one
			// never conflicts.
			if vals[c] == nil {
				continue next
			}
			if self != nil && self.vals[c] != vals[c] {
				unchanged = false
			}
		}
		if unchanged {
			continue
		}
		// Linear scan: there are no real indexes until the B+tree lands.
	scan:
		for _, r := range t.rows {
			if r == self {
				continue
			}
			for _, c := range u.cols {
				if r.vals[c] != vals[c] {
					continue scan
				}
			}
			key := make([]string, len(u.cols))
			for i, c := range u.cols {
				key[i] = pgwire.TextValue(vals[c])
			}
			return pgerr.New(pgerr.UniqueViolation, "duplicate key value violates unique constraint %q", u.name).
				WithDetail("Key (%s)=(%s) already exists.", t.colNames(u.cols, ", "), strings.Join(key, ", "))
		}
	}
	return nil
}

// insert validates and appends a row. The caller must hold db.mu.
func (t *table) insert(vals []any, undo *[]func()) error {
	if err := t.checkConstraints(vals, nil); err != nil {
		return err
	}
	r := &row{vals: vals}
	t.rows = append(t.rows, r)
	*undo = append(*undo, func() { t.remove(r) })
	return nil
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

func (s *Session) buildInsert(n *sql.Insert, ptypes []sql.Type) (*prepared, error) {
	t, _, err := s.targetTable(n.Table, ptypes)
	if err != nil {
		return nil, err
	}
	// Values cannot refer to the target table's columns, so they are bound
	// in an empty scope.
	b := &binder{sess: s, scope: &scope{}, ptypes: ptypes}

	target := make([]int, 0, len(t.cols))
	listed := make([]bool, len(t.cols))
	if n.Cols == nil {
		for i := range t.cols {
			target = append(target, i)
		}
	} else {
		for _, c := range n.Cols {
			i := t.colIndex(c.Name)
			if i < 0 {
				return nil, pgerr.New(pgerr.UndefinedColumn,
					"column %q of relation %q does not exist", c.Name, t.name).At(c.Pos)
			}
			if listed[i] {
				return nil, pgerr.New(pgerr.DuplicateColumn, "column %q specified more than once", c.Name).At(c.Pos)
			}
			listed[i] = true
			target = append(target, i)
		}
	}
	// checkCount validates the number of values supplied for the targets.
	// With an explicit column list every listed column needs a value;
	// without one, trailing columns may be omitted.
	checkCount := func(got, pos int) error {
		if got > len(target) {
			return pgerr.New(pgerr.SyntaxError, "INSERT has more expressions than target columns").At(pos)
		}
		if got < len(target) && n.Cols != nil {
			return pgerr.New(pgerr.SyntaxError, "INSERT has more target columns than expressions").At(pos)
		}
		return nil
	}
	// newRow starts a row with the defaults of the columns that get no
	// value from the statement.
	newRow := func(supplied int, en *env) ([]any, error) {
		vals := make([]any, len(t.cols))
		given := make([]bool, len(t.cols))
		for _, c := range target[:supplied] {
			given[c] = true
		}
		for i, c := range t.cols {
			if !given[i] && c.def != nil {
				v, err := c.def(en)
				if err != nil {
					return nil, err
				}
				vals[i] = v
			}
		}
		return vals, nil
	}

	if n.Select != nil {
		plan, err := s.planSelect(n.Select, nil, ptypes)
		if err != nil {
			return nil, err
		}
		if err := checkCount(len(plan.cols), n.Table.Pos); err != nil {
			return nil, err
		}
		convs := make([]convFn, len(plan.cols))
		for i, typ := range plan.types {
			c := t.cols[target[i]]
			if convs[i] = assignFn(typ, c.typ); convs[i] == nil {
				return nil, pgerr.New(pgerr.DatatypeMismatch,
					"column %q is of type %s but expression is of type %s", c.name, c.typ, typ)
			}
		}
		return &prepared{run: func(ctx context.Context, params []any) (*result, error) {
			return s.write(func(undo *[]func()) (string, error) {
				if err := s.db.stillCurrent(t); err != nil {
					return "", err
				}
				en := &env{ctx: ctx, params: params, locked: true}
				// The query is evaluated in full before anything is
				// inserted, so "insert into t select * from t" terminates.
				rows, err := plan.run(en)
				if err != nil {
					return "", err
				}
				for _, src := range rows {
					vals, err := newRow(len(src), en)
					if err != nil {
						return "", err
					}
					for i, v := range src {
						if v != nil {
							if v, err = convs[i](v); err != nil {
								return "", err
							}
						}
						vals[target[i]] = v
					}
					if err := t.insert(vals, undo); err != nil {
						return "", err
					}
				}
				return fmt.Sprintf("INSERT 0 %d", len(rows)), nil
			})
		}}, nil
	}

	rows := make([][]assignment, len(n.Rows))
	for ri, exprs := range n.Rows {
		if err := checkCount(len(exprs), n.RowPos[ri]); err != nil {
			return nil, err
		}
		for i, e := range exprs {
			a, err := b.bindValue(t, target[i], e)
			if err != nil {
				return nil, err
			}
			rows[ri] = append(rows[ri], a)
		}
	}
	return &prepared{run: func(ctx context.Context, params []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			if err := s.db.stillCurrent(t); err != nil {
				return "", err
			}
			en := &env{ctx: ctx, params: params, locked: true}
			for _, assigns := range rows {
				vals, err := newRow(len(assigns), en)
				if err != nil {
					return "", err
				}
				for _, a := range assigns {
					if vals[a.col], err = a.eval(en); err != nil {
						return "", err
					}
				}
				if err := t.insert(vals, undo); err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("INSERT 0 %d", len(rows)), nil
		})
	}}, nil
}

func (s *Session) buildUpdate(n *sql.Update, ptypes []sql.Type) (*prepared, error) {
	t, b, err := s.targetTable(n.Table, ptypes)
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
		if assigns[i], err = b.bindValue(t, idx, set.Value); err != nil {
			return nil, err
		}
	}
	where, err := b.bindWhere(n.Where, "WHERE")
	if err != nil {
		return nil, err
	}

	return &prepared{run: func(ctx context.Context, params []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			if err := s.db.stillCurrent(t); err != nil {
				return "", err
			}
			en := &env{ctx: ctx, params: params, locked: true}
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
					if vals[a.col], err = a.eval(en); err != nil {
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
	}}, nil
}

func (s *Session) buildDelete(n *sql.Delete, ptypes []sql.Type) (*prepared, error) {
	t, b, err := s.targetTable(n.Table, ptypes)
	if err != nil {
		return nil, err
	}
	where, err := b.bindWhere(n.Where, "WHERE")
	if err != nil {
		return nil, err
	}

	return &prepared{run: func(ctx context.Context, params []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			if err := s.db.stillCurrent(t); err != nil {
				return "", err
			}
			en := &env{ctx: ctx, params: params, locked: true}
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
	}}, nil
}

func (s *Session) buildCreateTable(n *sql.CreateTable, ptypes []sql.Type) (*prepared, error) {
	t := &table{name: n.Name.Name}
	var pk *unique
	setPrimaryKey := func(cols []int, pos int) error {
		if pk != nil {
			return pgerr.New(pgerr.InvalidTableDefinition,
				"multiple primary keys for table %q are not allowed", t.name).At(pos)
		}
		pk = &unique{name: t.name + "_pkey", cols: cols}
		t.uniques = append(t.uniques, pk)
		return nil
	}

	b := &binder{sess: s, scope: &scope{}, ptypes: ptypes}
	for _, c := range n.Cols {
		if t.colIndex(c.Name.Name) >= 0 {
			return nil, pgerr.New(pgerr.DuplicateColumn, "column %q specified more than once", c.Name.Name).At(c.Name.Pos)
		}
		col := column{name: c.Name.Name, typ: c.Type, notNull: c.NotNull}
		if c.Default != nil {
			be, err := b.bind(c.Default, c.Type)
			if err != nil {
				return nil, err
			}
			if col.def, err = storeAs(be, col, c.Default.Position()); err != nil {
				return nil, err
			}
		}
		t.cols = append(t.cols, col)
	}
	// Constraints are processed after all columns exist, in the order
	// PostgreSQL names them: column constraints first.
	for i, c := range n.Cols {
		if c.PrimaryKey {
			if err := setPrimaryKey([]int{i}, c.Name.Pos); err != nil {
				return nil, err
			}
		}
		if c.Unique {
			t.uniques = append(t.uniques, &unique{name: t.name + "_" + c.Name.Name + "_key", cols: []int{i}})
		}
	}
	for _, tc := range n.Constraints {
		cols := make([]int, len(tc.Cols))
		for i, id := range tc.Cols {
			if cols[i] = t.colIndex(id.Name); cols[i] < 0 {
				return nil, pgerr.New(pgerr.UndefinedColumn, "column %q named in key does not exist", id.Name).At(id.Pos)
			}
		}
		if tc.PrimaryKey {
			if err := setPrimaryKey(cols, tc.Pos); err != nil {
				return nil, err
			}
		} else {
			t.uniques = append(t.uniques, &unique{name: t.name + "_" + t.colNames(cols, "_") + "_key", cols: cols})
		}
	}
	if pk != nil {
		for _, c := range pk.cols {
			t.cols[c].notNull = true
		}
	}

	return &prepared{run: func(context.Context, []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			if s.db.nameTaken(t.name) {
				if n.IfNotExists {
					return "CREATE TABLE", nil
				}
				return "", pgerr.New(pgerr.DuplicateTable, "relation %q already exists", t.name)
			}
			// A fresh table per execution: a prepared CREATE TABLE that is
			// run again after a DROP must not resurrect the old rows.
			created := &table{name: t.name, cols: t.cols, uniques: append([]*unique(nil), t.uniques...)}
			s.db.tables[t.name] = created
			*undo = append(*undo, func() {
				if s.db.tables[t.name] == created {
					delete(s.db.tables, t.name)
				}
			})
			return "CREATE TABLE", nil
		})
	}}, nil
}

// nameTaken reports whether a table or index has this name; like PostgreSQL,
// both live in one namespace. The caller must hold db.mu.
func (db *DB) nameTaken(name string) bool {
	return db.tables[name] != nil || db.indexes[name] != nil
}

func (s *Session) buildDropTable(n *sql.DropTable) (*prepared, error) {
	name := n.Name.Name
	return &prepared{run: func(context.Context, []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			t, exists := s.db.tables[name]
			if !exists {
				if n.IfExists {
					return "DROP TABLE", nil
				}
				return "", pgerr.New(pgerr.UndefinedTable, "table %q does not exist", name)
			}
			// The table's indexes go with it.
			var dropped []*index
			for _, ix := range s.db.indexes {
				if ix.table == t {
					dropped = append(dropped, ix)
				}
			}
			for _, ix := range dropped {
				delete(s.db.indexes, ix.name)
			}
			delete(s.db.tables, name)
			*undo = append(*undo, func() {
				if s.db.nameTaken(name) {
					return
				}
				s.db.tables[name] = t
				for _, ix := range dropped {
					s.db.indexes[ix.name] = ix
				}
			})
			return "DROP TABLE", nil
		})
	}}, nil
}

func (s *Session) buildCreateIndex(n *sql.CreateIndex) (*prepared, error) {
	name := n.Name.Name
	return &prepared{run: func(context.Context, []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			t, err := s.db.lookup(n.Table)
			if err != nil {
				return "", err
			}
			if s.db.nameTaken(name) {
				if n.IfNotExists {
					return "CREATE INDEX", nil
				}
				return "", pgerr.New(pgerr.DuplicateTable, "relation %q already exists", name)
			}
			cols := make([]int, len(n.Cols))
			for i, id := range n.Cols {
				if cols[i] = t.colIndex(id.Name); cols[i] < 0 {
					return "", pgerr.New(pgerr.UndefinedColumn, "column %q does not exist", id.Name).At(id.Pos)
				}
			}
			ix := &index{name: name, table: t}
			if n.Unique {
				// The existing rows must already satisfy the constraint.
				seen := make(map[string]bool, len(t.rows))
			rows:
				for _, r := range t.rows {
					var key []byte
					for _, c := range cols {
						if r.vals[c] == nil {
							continue rows
						}
						key = appendKey(key, r.vals[c])
					}
					if seen[string(key)] {
						return "", pgerr.New(pgerr.UniqueViolation, "could not create unique index %q", name).
							WithDetail("Key (%s) is duplicated.", t.colNames(cols, ", "))
					}
					seen[string(key)] = true
				}
				ix.unique = &unique{name: name, cols: cols}
				t.uniques = append(t.uniques, ix.unique)
			}
			s.db.indexes[name] = ix
			*undo = append(*undo, func() { s.db.dropIndex(ix) })
			return "CREATE INDEX", nil
		})
	}}, nil
}

// dropIndex removes an index and the uniqueness it enforced. The caller must
// hold db.mu.
func (db *DB) dropIndex(ix *index) {
	if db.indexes[ix.name] == ix {
		delete(db.indexes, ix.name)
	}
	if ix.unique != nil {
		t := ix.table
		for i, u := range t.uniques {
			if u == ix.unique {
				t.uniques = append(t.uniques[:i:i], t.uniques[i+1:]...)
				break
			}
		}
	}
}

func (s *Session) buildDropIndex(n *sql.DropIndex) (*prepared, error) {
	name := n.Name.Name
	return &prepared{run: func(context.Context, []any) (*result, error) {
		return s.write(func(undo *[]func()) (string, error) {
			ix, exists := s.db.indexes[name]
			if !exists {
				if n.IfExists {
					return "DROP INDEX", nil
				}
				return "", pgerr.New(pgerr.UndefinedObject, "index %q does not exist", name)
			}
			s.db.dropIndex(ix)
			*undo = append(*undo, func() {
				if s.db.nameTaken(name) {
					return
				}
				s.db.indexes[name] = ix
				if ix.unique != nil {
					ix.table.uniques = append(ix.table.uniques, ix.unique)
				}
			})
			return "DROP INDEX", nil
		})
	}}, nil
}

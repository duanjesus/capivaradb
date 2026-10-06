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
			rows, err := s.read(func() ([][]any, error) {
				return plan.run(&env{ctx: ctx, params: params})
			})
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
		return command(func() string { s.begin(n.Isolation); return "BEGIN" }), nil
	case *sql.Commit:
		return &prepared{run: func(ctx context.Context, _ []any) (*result, error) {
			// COMMIT of a failed transaction rolls it back and says so.
			if s.failed {
				s.rollback()
				return &result{tag: "ROLLBACK"}, nil
			}
			if err := s.commit(); err != nil {
				return nil, err
			}
			return &result{tag: "COMMIT"}, nil
		}}, nil
	case *sql.Checkpoint:
		return &prepared{run: func(ctx context.Context, _ []any) (*result, error) {
			if err := s.db.Checkpoint(); err != nil {
				return nil, err
			}
			return &result{tag: "CHECKPOINT"}, nil
		}}, nil
	case *sql.Rollback:
		return command(func() string { s.rollback(); return "ROLLBACK" }), nil
	case *sql.Set:
		return &prepared{run: func(context.Context, []any) (*result, error) {
			switch n.Name {
			case "default_transaction_isolation":
				s.defaultLevel = parseLevel(n.Value)
			case "transaction_isolation":
				// SET TRANSACTION applies to the current transaction
				// and must come before it has looked at anything.
				switch {
				case !s.inTx:
					// PostgreSQL warns and does nothing.
				case s.started:
					return nil, pgerr.New(pgerr.ActiveSQLTransaction,
						"SET TRANSACTION ISOLATION LEVEL must be called before any query")
				default:
					s.level = parseLevel(n.Value)
				}
			default:
				s.vars[n.Name] = n.Value
			}
			return &result{tag: "SET"}, nil
		}}, nil
	case *sql.Show:
		return s.buildShow(n)
	case *sql.Vacuum:
		return s.buildVacuum(n)
	}
	return nil, pgerr.New(pgerr.InternalError, "unhandled statement node %T", node)
}

// command wraps a statement that cannot fail and returns no rows.
func command(fn func() string) *prepared {
	return &prepared{run: func(ctx context.Context, _ []any) (*result, error) {
		return &result{tag: fn()}, nil
	}}
}

func (s *Session) buildShow(n *sql.Show) (*prepared, error) {
	value := func() string { return s.vars[n.Name] }
	switch n.Name {
	case "transaction_isolation":
		value = func() string { return s.isolation().String() }
	case "default_transaction_isolation":
		value = func() string { return s.defaultLevel.String() }
	default:
		if _, ok := s.vars[n.Name]; !ok {
			return nil, pgerr.New(pgerr.UndefinedObject, "unrecognized configuration parameter %q", n.Name).At(n.Pos)
		}
	}
	return &prepared{
		cols: []pgwire.Column{{Name: n.Name, OID: pgwire.OIDText}},
		run: func(ctx context.Context, _ []any) (*result, error) {
			return &result{rows: [][]any{{value()}}, tag: "SHOW"}, nil
		},
	}, nil
}

// buildVacuum removes dead row versions from one table or from all.
func (s *Session) buildVacuum(n *sql.Vacuum) (*prepared, error) {
	return &prepared{run: func(ctx context.Context, _ []any) (*result, error) {
		if s.inTx {
			return nil, pgerr.New(pgerr.ActiveSQLTransaction, "VACUUM cannot run inside a transaction block")
		}
		return s.write(ctx, func(ch *changes) (string, error) {
			names := s.db.tableNames()
			if n.Table != "" {
				if _, err := s.db.lookup(sql.TableRef{Name: n.Table, Pos: n.Pos}); err != nil {
					return "", err
				}
				names = []string{n.Table}
			}
			for _, name := range names {
				t := s.db.tables[name]
				removed, err := s.db.vacuum(t, ch)
				if err != nil {
					return "", err
				}
				t.dead = max(t.dead-removed, 0)
				t.vacuumAt = t.dead + autoVacuumThreshold
			}
			return "VACUUM", nil
		})
	}}, nil
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
			return s.write(ctx, func(ch *changes) (string, error) {
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
					if err := t.insertRow(vals, ch); err != nil {
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
		return s.write(ctx, func(ch *changes) (string, error) {
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
				if err := t.insertRow(vals, ch); err != nil {
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
		return s.write(ctx, func(ch *changes) (string, error) {
			if err := s.db.stillCurrent(t); err != nil {
				return "", err
			}
			// The rows are read in full before any is changed: an updated
			// row may move in the tree, and must not be met a second time.
			rows, err := s.db.scan(t, s.snap)
			if err != nil {
				return "", err
			}
			en := &env{ctx: ctx, params: params, locked: true}
			count := 0
			for _, r := range rows {
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
				if err := t.updateRow(r, vals, ch); err != nil {
					return "", err
				}
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
		return s.write(ctx, func(ch *changes) (string, error) {
			if err := s.db.stillCurrent(t); err != nil {
				return "", err
			}
			rows, err := s.db.scan(t, s.snap)
			if err != nil {
				return "", err
			}
			en := &env{ctx: ctx, params: params, locked: true}
			count := 0
			for _, r := range rows {
				en.row = r.vals
				ok, err := matches(where, en)
				if err != nil {
					return "", err
				}
				if !ok {
					continue
				}
				if err := t.deleteRow(r, ch); err != nil {
					return "", err
				}
				count++
			}
			return fmt.Sprintf("DELETE %d", count), nil
		})
	}}, nil
}

// indexDef is an index of a table that is yet to be created.
type indexDef struct {
	name   string
	cols   []int
	unique bool
}

func (s *Session) buildCreateTable(n *sql.CreateTable, ptypes []sql.Type) (*prepared, error) {
	name := n.Name.Name
	// colIndex needs a table to look columns up in while it is assembled.
	draft := &table{name: name}
	var pk []int
	var uniques []indexDef
	setPrimaryKey := func(cols []int, pos int) error {
		if pk != nil {
			return pgerr.New(pgerr.InvalidTableDefinition,
				"multiple primary keys for table %q are not allowed", name).At(pos)
		}
		pk = cols
		return nil
	}

	b := &binder{sess: s, scope: &scope{}, ptypes: ptypes}
	for _, c := range n.Cols {
		if draft.colIndex(c.Name.Name) >= 0 {
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
			col.defSQL = sql.FormatExpr(c.Default)
		}
		draft.cols = append(draft.cols, col)
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
			uniques = append(uniques, indexDef{name + "_" + c.Name.Name + "_key", []int{i}, true})
		}
	}
	for _, tc := range n.Constraints {
		cols := make([]int, len(tc.Cols))
		for i, id := range tc.Cols {
			if cols[i] = draft.colIndex(id.Name); cols[i] < 0 {
				return nil, pgerr.New(pgerr.UndefinedColumn, "column %q named in key does not exist", id.Name).At(id.Pos)
			}
		}
		if tc.PrimaryKey {
			if err := setPrimaryKey(cols, tc.Pos); err != nil {
				return nil, err
			}
		} else {
			uniques = append(uniques, indexDef{name + "_" + draft.colNames(cols, "_") + "_key", cols, true})
		}
	}
	for _, c := range pk {
		draft.cols[c].notNull = true
	}

	return &prepared{run: func(ctx context.Context, _ []any) (*result, error) {
		return s.write(ctx, func(ch *changes) (string, error) {
			if s.db.nameTaken(name) {
				if n.IfNotExists {
					return "CREATE TABLE", nil
				}
				return "", pgerr.New(pgerr.DuplicateTable, "relation %q already exists", name)
			}
			// A fresh table per execution: a prepared CREATE TABLE that is
			// run again after a DROP gets storage of its own.
			t := &table{name: name, cols: draft.cols, pk: pk}
			if err := s.db.createTable(t, uniques, ch); err != nil {
				return "", err
			}
			return "CREATE TABLE", nil
		})
	}}, nil
}

// createTable allocates the storage of t and of the indexes backing its
// UNIQUE constraints, and registers them. The caller must hold db.mu.
//
// Nothing here needs cleaning up on failure: every step records how to
// undo itself in ch, and the caller reverts ch if the statement fails.
func (db *DB) createTable(t *table, uniques []indexDef, ch *changes) (err error) {
	if t.tree, err = ch.createTree(); err != nil {
		return err
	}
	for _, def := range uniques {
		if db.nameTaken(def.name) {
			return pgerr.New(pgerr.DuplicateTable, "relation %q already exists", def.name)
		}
		tree, err := ch.createTree()
		if err != nil {
			return err
		}
		t.indexes = append(t.indexes, &index{name: def.name, table: t, cols: def.cols, unique: true, tree: tree})
	}
	return db.register(t, ch)
}

// register adds a table and its indexes to the catalog, on disk and in
// memory. The caller must hold db.mu.
func (db *DB) register(t *table, ch *changes) error {
	if err := db.saveTable(t, ch); err != nil {
		return err
	}
	for _, ix := range t.indexes {
		if err := db.saveIndex(ix, ch); err != nil {
			return err
		}
	}
	db.tables[t.name] = t
	for _, ix := range t.indexes {
		db.indexes[ix.name] = ix
	}
	indexes := t.indexes
	ch.onUndo(func() {
		delete(db.tables, t.name)
		for _, ix := range indexes {
			delete(db.indexes, ix.name)
		}
	})
	return nil
}

// unregister removes a table and its indexes from the catalog, leaving
// their pages alone.
func (db *DB) unregister(t *table, ch *changes) error {
	for _, ix := range t.indexes {
		if err := db.forget('i', ix.name, ch); err != nil {
			return err
		}
	}
	if err := db.forget('t', t.name, ch); err != nil {
		return err
	}
	indexes := t.indexes
	delete(db.tables, t.name)
	for _, ix := range indexes {
		delete(db.indexes, ix.name)
	}
	ch.onUndo(func() {
		db.tables[t.name] = t
		for _, ix := range indexes {
			db.indexes[ix.name] = ix
		}
	})
	return nil
}

// nameTaken reports whether a table or index has this name; like PostgreSQL,
// both live in one namespace. The caller must hold db.mu.
func (db *DB) nameTaken(name string) bool {
	return db.tables[name] != nil || db.indexes[name] != nil
}

// errOthersWriting is returned by DROP while another session has
// uncommitted changes. Its pages would be freed at commit, and that
// session's rollback would then write into them. Proper locks arrive with
// MVCC; until then the drop is refused.
func errOthersWriting(what, name string) error {
	return pgerr.New(pgerr.ObjectInUse,
		"cannot drop %s %q while another transaction has uncommitted changes", what, name)
}

func (s *Session) buildDropTable(n *sql.DropTable) (*prepared, error) {
	name := n.Name.Name
	return &prepared{run: func(ctx context.Context, _ []any) (*result, error) {
		return s.write(ctx, func(ch *changes) (string, error) {
			t, exists := s.db.tables[name]
			if !exists {
				if n.IfExists {
					return "DROP TABLE", nil
				}
				return "", pgerr.New(pgerr.UndefinedTable, "table %q does not exist", name)
			}
			if s.db.othersWriting(s) {
				return "", errOthersWriting("table", name)
			}
			// The table and its indexes leave the catalog now, but their
			// pages are only freed once the transaction has committed:
			// until then a rollback must be able to bring them back.
			if err := s.db.unregister(t, ch); err != nil {
				return "", err
			}
			for _, ix := range t.indexes {
				ch.drops = append(ch.drops, ix.tree.Root())
			}
			ch.drops = append(ch.drops, t.tree.Root())
			return "DROP TABLE", nil
		})
	}}, nil
}

func (s *Session) buildCreateIndex(n *sql.CreateIndex) (*prepared, error) {
	name := n.Name.Name
	return &prepared{run: func(ctx context.Context, _ []any) (*result, error) {
		return s.write(ctx, func(ch *changes) (string, error) {
			t, err := s.db.lookup(n.Table)
			if err != nil {
				return "", err
			}
			// Rows another transaction has not committed would get index
			// entries that its rollback knows nothing about.
			if s.db.othersWriting(s) {
				return "", pgerr.New(pgerr.ObjectInUse,
					"cannot create index %q while another transaction has uncommitted changes", name)
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
			ix := &index{name: name, table: t, cols: cols, unique: n.Unique}
			if err := s.db.buildIndex(ix, ch); err != nil {
				return "", err
			}
			if err := s.db.saveIndex(ix, ch); err != nil {
				return "", err
			}
			s.db.attachIndex(ix, ch)
			return "CREATE INDEX", nil
		})
	}}, nil
}

// buildIndex creates the index's tree and fills it from the table's rows.
// For a unique index the existing rows must already satisfy it.
func (db *DB) buildIndex(ix *index, ch *changes) error {
	t := ix.table
	// Every version gets an entry, visible or not: an index points at
	// versions, and which of them a reader sees is decided in the table.
	rows, err := db.scan(t, nil)
	if err != nil {
		return err
	}
	if ix.tree, err = ch.createTree(); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, r := range rows {
		// Only versions nobody has deleted count for uniqueness: a deleted
		// version and its successor would otherwise look like duplicates.
		if ix.unique && r.xmax == 0 && !hasNull(ix.cols, r.vals) {
			prefix := string(encodeKey(ix.cols, r.vals))
			if seen[prefix] {
				return pgerr.New(pgerr.UniqueViolation, "could not create unique index %q", ix.name).
					WithDetail("Key (%s) is duplicated.", t.colNames(ix.cols, ", "))
			}
			seen[prefix] = true
		}
		// The entries need no undo of their own: undoing the creation of
		// the tree frees them all at once.
		if err := ix.tree.Put(ix.entryKey(r.vals, r.key), nil); err != nil {
			return storageError(err)
		}
	}
	return nil
}

// attachIndex adds an index to the in-memory catalog and to its table.
func (db *DB) attachIndex(ix *index, ch *changes) {
	t := ix.table
	db.indexes[ix.name] = ix
	t.indexes = append(t.indexes[:len(t.indexes):len(t.indexes)], ix)
	ch.onUndo(func() { db.removeIndex(ix) })
}

// removeIndex takes an index out of the in-memory catalog and of its table.
func (db *DB) removeIndex(ix *index) {
	t := ix.table
	kept := make([]*index, 0, len(t.indexes))
	for _, other := range t.indexes {
		if other != ix {
			kept = append(kept, other)
		}
	}
	t.indexes = kept
	delete(db.indexes, ix.name)
}

func (s *Session) buildDropIndex(n *sql.DropIndex) (*prepared, error) {
	name := n.Name.Name
	return &prepared{run: func(ctx context.Context, _ []any) (*result, error) {
		return s.write(ctx, func(ch *changes) (string, error) {
			ix, exists := s.db.indexes[name]
			if !exists {
				if n.IfExists {
					return "DROP INDEX", nil
				}
				return "", pgerr.New(pgerr.UndefinedObject, "index %q does not exist", name)
			}
			if s.db.othersWriting(s) {
				return "", errOthersWriting("index", name)
			}
			if err := s.db.forget('i', name, ch); err != nil {
				return "", err
			}
			s.db.removeIndex(ix)
			ch.onUndo(func() {
				s.db.indexes[name] = ix
				ix.table.indexes = append(ix.table.indexes[:len(ix.table.indexes):len(ix.table.indexes)], ix)
			})
			ch.drops = append(ch.drops, ix.tree.Root())
			return "DROP INDEX", nil
		})
	}}, nil
}

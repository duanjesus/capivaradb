package sql

// Stmt is one parsed statement.
type Stmt struct {
	Node Node
	// NumParams is the highest $n referenced by the statement.
	NumParams int
}

// Node is a statement node: one of the pointer types below.
type Node any

// Ident is an identifier together with where it appeared.
type Ident struct {
	Name string
	Pos  int
}

// ---- SELECT ----

type Select struct {
	Distinct bool
	Items    []SelectItem
	From     TableExpr // nil when there is no FROM clause
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr

	// A set operation. When Op is not empty the query is
	// "Left Op [ALL] Right": Op is "union", "intersect" or "except", and
	// of the fields above only OrderBy, Limit and Offset are used, applying
	// to the combined result.
	Op          string
	All         bool
	Left, Right *Select
}

// SelectItem is one entry of the select list: an expression, "*" or "t.*".
type SelectItem struct {
	Star  bool
	Table string // qualifier of "t.*"
	Expr  Expr
	Alias string
	Pos   int
}

type OrderItem struct {
	Expr Expr
	Desc bool
	// NullsFirst is nil when not specified: the default then depends on
	// the direction (NULLs sort as if larger than everything).
	NullsFirst *bool
}

// TableExpr is an entry of the FROM clause: *TableRef, *Join or *DerivedTable.
type TableExpr interface {
	tableExpr()
}

// TableRef names a table, optionally under an alias.
type TableRef struct {
	Name  string
	Alias string
	Pos   int
}

type JoinKind uint8

const (
	InnerJoin JoinKind = iota
	LeftJoin
	CrossJoin
	RightJoin
	FullJoin
)

func (k JoinKind) String() string {
	switch k {
	case LeftJoin:
		return "left join"
	case CrossJoin:
		return "cross join"
	case RightJoin:
		return "right join"
	case FullJoin:
		return "full join"
	}
	return "inner join"
}

type Join struct {
	Kind  JoinKind
	Left  TableExpr
	Right TableExpr
	On    Expr // nil for a cross join, or when Using is set
	// Using lists the columns of JOIN ... USING (a, b): the join condition
	// is equality of those columns, and each appears once in the result.
	Using []Ident
	Pos   int
}

// DerivedTable is a subquery in FROM.
type DerivedTable struct {
	Select *Select
	Alias  string
	Pos    int
}

func (*TableRef) tableExpr()     {}
func (*Join) tableExpr()         {}
func (*DerivedTable) tableExpr() {}

// ---- other statements ----

type CreateTable struct {
	Name        Ident
	IfNotExists bool
	Cols        []ColumnDef
	Constraints []TableConstraint
}

type ColumnDef struct {
	Name       Ident
	Type       Type
	NotNull    bool
	PrimaryKey bool
	Unique     bool
	Default    Expr
}

// TableConstraint is a PRIMARY KEY or UNIQUE constraint written after the
// column list, which is the only way to declare one over several columns.
type TableConstraint struct {
	PrimaryKey bool // otherwise UNIQUE
	Cols       []Ident
	Pos        int
}

type DropTable struct {
	Name     Ident
	IfExists bool
}

type CreateIndex struct {
	Name        Ident
	Table       TableRef
	Cols        []Ident
	Unique      bool
	IfNotExists bool
}

type DropIndex struct {
	Name     Ident
	IfExists bool
}

type Insert struct {
	Table TableRef
	Cols  []Ident
	// Exactly one of Rows and Select is set.
	Rows   [][]Expr
	Select *Select
	// RowPos holds the position of each row's opening parenthesis.
	RowPos []int
}

type Assignment struct {
	Col   Ident
	Value Expr
}

type Update struct {
	Table TableRef
	Sets  []Assignment
	Where Expr
}

type Delete struct {
	Table TableRef
	Where Expr
}

type Begin struct {
	// Isolation is the requested level in lower case, or "" if none.
	Isolation string
}

type (
	Commit   struct{}
	Rollback struct{}
	// Checkpoint asks for every modified page to be written to disk.
	Checkpoint struct{}
)

type Set struct {
	Name  string
	Value string
}

type Show struct {
	Name string
	Pos  int
}

// ---- expressions ----

// Expr is an expression node.
type Expr interface {
	// Position is the 1-based character offset used when reporting an
	// error about this expression.
	Position() int
}

type Literal struct {
	// Val is nil, bool, int64, float64 or string.
	Val  any
	Type Type
	Pos  int
}

type Param struct {
	Index int // 1-based, as written: $1
	Pos   int
}

type ColumnRef struct {
	Table string
	Name  string
	Pos   int
}

type Unary struct {
	Op  string // "-", "not"
	X   Expr
	Pos int
}

type Binary struct {
	Op   string
	L, R Expr
	Pos  int // position of the operator
}

type IsNull struct {
	X   Expr
	Not bool
	Pos int
}

type Cast struct {
	X   Expr
	To  Type
	Pos int
}

type FuncCall struct {
	Name     string
	Args     []Expr
	Star     bool // count(*)
	Distinct bool // count(distinct x)
	Pos      int
}

type When struct {
	Cond Expr
	Then Expr
}

// Case is both forms of CASE: with an Operand each WHEN is compared to it,
// without one each WHEN is a condition.
type Case struct {
	Operand Expr
	Whens   []When
	Else    Expr
	Pos     int
}

// In is "x IN (a, b)" when List is set and "x IN (SELECT ...)" when Sub is.
type In struct {
	X    Expr
	List []Expr
	Sub  *Select
	Not  bool
	Pos  int
}

type Between struct {
	X, Lo, Hi Expr
	Not       bool
	Pos       int
}

type Like struct {
	X, Pattern Expr
	Not        bool
	ILike      bool
	Pos        int
}

// SubqueryExpr is a scalar subquery: a parenthesised SELECT used as a value.
type SubqueryExpr struct {
	Select *Select
	Pos    int
}

type Exists struct {
	Select *Select
	Pos    int
}

// DefaultValue is the keyword DEFAULT in an INSERT's VALUES list.
type DefaultValue struct {
	Pos int
}

func (e *Literal) Position() int      { return e.Pos }
func (e *Param) Position() int        { return e.Pos }
func (e *ColumnRef) Position() int    { return e.Pos }
func (e *Unary) Position() int        { return e.Pos }
func (e *Binary) Position() int       { return e.Pos }
func (e *IsNull) Position() int       { return e.Pos }
func (e *Cast) Position() int         { return e.Pos }
func (e *FuncCall) Position() int     { return e.Pos }
func (e *Case) Position() int         { return e.Pos }
func (e *In) Position() int           { return e.Pos }
func (e *Between) Position() int      { return e.Pos }
func (e *Like) Position() int         { return e.Pos }
func (e *SubqueryExpr) Position() int { return e.Pos }
func (e *Exists) Position() int       { return e.Pos }
func (e *DefaultValue) Position() int { return e.Pos }

// WalkExpr calls fn for e and then for each of its sub-expressions, depth
// first. If fn returns false the children of that node are skipped. It does
// not descend into subqueries: their expressions belong to another scope.
func WalkExpr(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	switch e := e.(type) {
	case *Unary:
		WalkExpr(e.X, fn)
	case *Binary:
		WalkExpr(e.L, fn)
		WalkExpr(e.R, fn)
	case *IsNull:
		WalkExpr(e.X, fn)
	case *Cast:
		WalkExpr(e.X, fn)
	case *FuncCall:
		for _, a := range e.Args {
			WalkExpr(a, fn)
		}
	case *Case:
		WalkExpr(e.Operand, fn)
		for _, w := range e.Whens {
			WalkExpr(w.Cond, fn)
			WalkExpr(w.Then, fn)
		}
		WalkExpr(e.Else, fn)
	case *In:
		WalkExpr(e.X, fn)
		for _, x := range e.List {
			WalkExpr(x, fn)
		}
	case *Between:
		WalkExpr(e.X, fn)
		WalkExpr(e.Lo, fn)
		WalkExpr(e.Hi, fn)
	case *Like:
		WalkExpr(e.X, fn)
		WalkExpr(e.Pattern, fn)
	}
}

// Vacuum removes dead row versions, from one table or, with an empty Table,
// from all of them.
type Vacuum struct {
	Table string
	Pos   int
}

// Explain shows the plan of a statement instead of running it, or, with
// Analyze, runs it and shows what happened.
type Explain struct {
	Stmt    Node
	Analyze bool
	// NoCosts and NoTiming leave the estimates and the measured times out,
	// which makes the output reproducible.
	NoCosts  bool
	NoTiming bool
	Pos      int
}

// Analyze gathers planner statistics, for one table or, with an empty
// Table, for all of them.
type Analyze struct {
	Table string
	Pos   int
}

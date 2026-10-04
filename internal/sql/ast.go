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

// TableRef names a table, optionally under an alias.
type TableRef struct {
	Name  string
	Alias string
	Pos   int
}

type Select struct {
	Items []SelectItem
	From  *TableRef
	Where Expr
}

type SelectItem struct {
	Star  bool
	Expr  Expr
	Alias string
}

type CreateTable struct {
	Name        Ident
	IfNotExists bool
	Cols        []ColumnDef
}

type ColumnDef struct {
	Name       Ident
	Type       Type
	NotNull    bool
	PrimaryKey bool
}

type DropTable struct {
	Name     Ident
	IfExists bool
}

type Insert struct {
	Table TableRef
	Cols  []Ident
	Rows  [][]Expr
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

type (
	Begin    struct{}
	Commit   struct{}
	Rollback struct{}
)

type Set struct {
	Name  string
	Value string
}

type Show struct {
	Name string
	Pos  int
}

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
	Op  string // "-", "+", "not"
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
	Name string
	Args []Expr
	Pos  int
}

func (e *Literal) Position() int   { return e.Pos }
func (e *Param) Position() int     { return e.Pos }
func (e *ColumnRef) Position() int { return e.Pos }
func (e *Unary) Position() int     { return e.Pos }
func (e *Binary) Position() int    { return e.Pos }
func (e *IsNull) Position() int    { return e.Pos }
func (e *Cast) Position() int      { return e.Pos }
func (e *FuncCall) Position() int  { return e.Pos }

# Architecture

This document describes the code as it stands at milestone 2 and marks what
each later milestone changes. It is updated with every milestone.

## Layers

| Package | Responsibility | Knows about |
|---------|----------------|-------------|
| `cmd/capivaradb` | Flags, listener, signal handling | everything |
| `internal/pgwire` | Wire protocol: bytes ⇄ interface calls | `pgerr` |
| `internal/engine` | Scopes, type checking, grouping rules, execution, transactions | `pgwire` interfaces, `sql`, `pgerr` |
| `internal/sql` | Lexer, AST, parser, canonical printer | `pgerr` |
| `internal/pgerr` | Error type carrying a SQLSTATE | nothing |

Dependencies point one way. `pgwire` never imports `engine` or `sql`; it is
handed a `Handler` and only sees interfaces.

## The protocol/engine boundary

```go
type Handler interface {
    NewSession(params map[string]string) (Session, error)
}

type Session interface {
    Parse(query string) ([]Stmt, error)
    Prepare(stmt Stmt, paramOIDs []uint32) (Prepared, error)
    TxStatus() byte
    OnError()
    Close()
}

type Prepared interface {
    ParamOIDs() []uint32
    Columns() []Column
    Execute(ctx context.Context, params []any) (Rows, error)
}

type Rows interface {
    Next(ctx context.Context) ([]any, error)
    Tag() string
    Close()
}
```

The shape follows the extended query protocol, which is the more demanding
of the two:

- `Parse` is separate from `Prepare` because the simple protocol needs to
  split a string into statements before running the first one, and the
  extended protocol must reject a `Parse` message holding more than one.
- `Prepare` returns parameter types and result columns *without executing*,
  because a driver sends `Describe` before `Bind` and expects both.
- `Rows` is an iterator rather than a slice because `Execute` can ask for
  at most N rows and come back later for more. The milestone-1 engine
  materialises results anyway; the Volcano executor of milestone 7 will
  produce them lazily through the same interface.
- `context.Context` carries cancellation from a `CancelRequest`, which
  arrives on a different connection, into whatever the statement is doing.

The simple query protocol is implemented on top of the same four calls, so
there is a single execution path.

## A connection's life

One goroutine per connection (`conn.serve`):

1. **Startup.** Read untyped startup packets until a real `StartupMessage`
   arrives, answering `SSLRequest` with `N` and handling `CancelRequest`
   in passing. Create the `Session`. Send `AuthenticationOk`, the
   `ParameterStatus` set drivers look at, `BackendKeyData`, `ReadyForQuery`.
2. **Message loop.** Read a type byte and a length-prefixed body, dispatch.
   Output is buffered and flushed at `ReadyForQuery`, on `Flush`, and after
   an error.
3. **Error recovery.** In the extended protocol an error sets
   `skipToSync`; everything up to the next `Sync` is discarded. A `FATAL`
   error closes the connection.
4. **Teardown.** Close portals, then the session, which rolls back an open
   transaction.

Connection state held by the protocol layer: named prepared statements,
named portals (statement + parameter values + result formats + a `Rows` in
progress), and the cancel function of the statement currently running.

## Query processing

```
query text ──lex──▶ tokens ──parse──▶ AST ──bind──▶ closures ──run──▶ rows
            sql/lexer   sql/parser      engine/bind     engine/select
                                                        engine/exec
```

**Lexer.** One pass over the bytes. Handles quoted identifiers, `''`
escapes, nested `/* */` comments and `$n` parameters. Token positions are
counted in characters rather than bytes, because that is the unit of the
`Position` field in an error response.

**Parser.** Recursive descent for statements, precedence climbing for
expressions, using PostgreSQL's precedence table (`IN`/`BETWEEN`/`LIKE` sit
between comparison and `||`; the bounds of `BETWEEN` are parsed above `AND`
so that its own `AND` is not mistaken for the logical one). Every node that
can be the subject of an error remembers its position. Nesting is limited
to 250 levels: the parser is recursive, and a stack overflow in Go cannot be
recovered from, so a query of a hundred thousand parentheses must be an
error rather than a crash.

**Printer.** `sql.Format` writes a tree back as canonical SQL — lower-case
keywords, every operator application parenthesised, identifiers quoted only
when necessary. Parsing that text gives the same tree. The property is
checked on a corpus and by the fuzzer, and the binder relies on it: two
expressions are "the same" for `GROUP BY` and `ORDER BY` matching when their
canonical text is equal.

**Binder.** Resolves names, checks types and compiles each expression into a
Go closure (`func(*env) (any, error)`), so execution does not walk the tree
again.

- *Scopes.* Each query level has a scope: the columns its `FROM` produces,
  each tagged with the alias it can be qualified by. A scope points to the
  enclosing query's scope, which is how a correlated subquery finds
  `outer.col`; at run time the closure walks the same number of steps up a
  chain of environments. An unqualified name that matches two columns of one
  level is an error, not a guess.
- *Grouping.* In a query with `GROUP BY` or aggregates, everything after
  `WHERE` is evaluated once per group. There, a column is legal only inside
  an aggregate or as (part of) a `GROUP BY` expression; the binder enforces
  that and reports PostgreSQL's `42803`. `GROUP BY` and `ORDER BY` accept
  select-list positions and output names, with the precedence PostgreSQL
  gives them (input column first for `GROUP BY`, output name first for
  `ORDER BY`).
- *Parameter types.* A parameter takes the type its context expects: the
  other operand of a comparison, the target column of an `INSERT` or
  `UPDATE`, the target of a cast. Inference can flow backwards
  (`select $1, id from t where id = $1`) and across subqueries, so a
  statement with parameters is bound twice: once to collect types, once to
  compile with all of them known. A parameter nothing constrains is `text`.
- *Untyped literals.* A quoted literal behaves like PostgreSQL's `unknown`
  type and adopts the type of what it is used with, so `id = '42'` compares
  integers. Drivers rely on this when they inline parameters.
- *Type unification.* The branches of `CASE`, the arguments of `COALESCE`
  and the elements of `IN` are brought to one type: integers widen to
  `bigint` and to `double precision`; anything else must match.

**Executor.** `engine/select.go` runs a query in the textbook order —
`FROM`, `WHERE`, grouping, `HAVING`, select list, `DISTINCT`, `ORDER BY`,
`OFFSET`/`LIMIT` — and each step materialises its result. Joins are nested
loops in the order written. This is deliberately the least clever correct
implementation: it is the reference that the planner (milestone 6) and the
iterator executor (milestone 7) must agree with, and sqllogictest pins its
answers.

A table is a slice of row pointers behind one `sync.RWMutex` for the whole
database. Readers copy the slice of row references under the read lock and
release it before evaluating anything; that is safe because a row's value
slice is never modified in place — an update swaps in a new slice. Writers
hold the write lock for the statement, and a subquery inside a writing
statement is told the lock is already held.

**Constraints.** `NOT NULL`, `PRIMARY KEY` and `UNIQUE` (including unique
indexes) are checked on every insert and update by scanning the table. As in
PostgreSQL, NULLs never conflict with each other, and constraints get the
names PostgreSQL would generate (`t_pkey`, `t_a_b_key`).

**Transactions.** Each change appends a function to an undo log. A failing
statement runs its own undo entries immediately, which makes statements
atomic; `ROLLBACK` runs the transaction's entries in reverse. DDL is
transactional too. There is no isolation: see the README.

## Values and types

| SQL type | OID | Go value |
|----------|-----|----------|
| `boolean` | 16 | `bool` |
| `integer` | 23 | `int64` (range-checked) |
| `bigint` | 20 | `int64` |
| `double precision` | 701 | `float64` |
| `text` | 25 | `string` |
| NULL | — | `nil` |

Integer arithmetic detects overflow and reports `22003` instead of wrapping.
Integer literals are `integer` if they fit in 32 bits and `bigint`
otherwise, as in PostgreSQL.

## What the next milestones replace

| Today | Replaced by |
|-------|-------------|
| Catalog entries for indexes, constraints checked by scanning | B+tree indexes (M3) |
| `[]*row` in memory | Slotted pages, buffer pool, B+tree (M3) |
| Undo closures | WAL records with redo and undo (M4) |
| One global lock, no isolation | MVCC snapshots, row versions, vacuum (M5) |
| Joins in the order written, filters applied last | Cost-based planner (M6) |
| Every step materialised, nested-loop joins | Iterator tree behind the same `Rows` interface (M7) |

`internal/pgwire` and the tests under `compat/` are expected to survive all
of that unchanged.

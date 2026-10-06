# Architecture

This document describes the code as it stands at milestone 5 and marks what
each later milestone changes. It is updated with every milestone.

## Layers

| Package | Responsibility | Knows about |
|---------|----------------|-------------|
| `cmd/capivaradb` | Flags, listener, signal handling | everything |
| `internal/pgwire` | Wire protocol: bytes ⇄ interface calls | `pgerr` |
| `internal/engine` | Scopes, type checking, grouping rules, execution, transactions, row and key encoding, catalog | `pgwire` interfaces, `sql`, `storage`, `pgerr` |
| `internal/storage` | Page file, buffer pool, B+trees, write-ahead log, recovery, consistency checks | nothing |
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
  at most N rows and come back later for more. For six milestones the
  engine computed whole results anyway; since the seventh they are
  produced lazily through the same interface, and each call to `Next` may
  arrive with a context of its own.
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

**Planner.** `engine/plan.go` turns `FROM` and `WHERE` into a tree of
scans and joins: it splits the `WHERE` clause into conditions and places
each as early as it can go, chooses for every table between a sequential
scan and an index, and orders the joins by estimated cost, using the
statistics in `engine/stats.go`. The same tree is what `EXPLAIN` prints.
See [planner.md](planner.md).

**Executor.** A plan is a tree of iterators (`engine/iter.go`,
`join.go`, `select.go`): each step produces its next row on request, by
asking the step below for one. The steps are scans, four kinds of join,
aggregation, the select list, duplicate removal, sorting, limits and set
operations. Nothing is computed before it is asked for, a step that must
remember more than `work_mem` writes the excess to temporary files, and
the result the protocol layer reads from is the top iterator, so rows
reach the client as they are produced. See [executor.md](executor.md).

With every choice switched off — no index scans, no hash or merge joins,
joins in the order written — a query runs the way the first executor ran
it, which is what every other configuration is tested against.

**Storage.** Each table is a B+tree and so is each index; see
[storage.md](storage.md). A scan decodes the rows of the tree, or of the
range of it an index condition selects, a batch at a time: it takes the
database's read lock for each batch and between batches holds only the key
it stopped at; writers hold the write lock for the statement, and a
subquery inside a writing statement is told the lock is already held. An
`UPDATE` or `DELETE` reads all the rows it might touch before changing any,
because a changed row can move within the tree.

**Constraints.** `NOT NULL` is checked on the row; `PRIMARY KEY` is the
table's own key, so a duplicate is found by a lookup; `UNIQUE` constraints
are unique indexes, checked by seeking the indexed values. As in
PostgreSQL, NULLs never conflict with each other, and constraints get the
names PostgreSQL would generate (`t_pkey`, `t_a_b_key`).

**Transactions.** Every statement that changes something belongs to a
transaction: the open `BEGIN` block, or one of its own. Before each change
to a tree the transaction logs how to reverse it; `ROLLBACK`, a failed
statement and crash recovery all reverse changes through that same log, so
statements are atomic and so are transactions, across a crash. `COMMIT`
makes the log durable. DDL is transactional too: a dropped table's pages are
only freed after the commit. See [recovery.md](recovery.md).

**Isolation.** Rows are versioned: each version records the transaction
that created it and the one that deleted it, and every statement reads
through a snapshot that decides which versions exist for it; a query keeps
its snapshot for as long as its cursor is open. Readers and
writers do not block each other; writers that want the same row wait for
one another, with deadlock detection. Two levels are offered, read
committed and repeatable read (snapshot isolation). See [mvcc.md](mvcc.md).

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

## What the boundary was for

The engine behind `internal/pgwire` was rebuilt milestone after milestone —
rows in memory, then B+trees on disk, then versioned rows, then an
iterator tree — and the protocol code and the client tests under `compat/`
did not change for any of it.
The last replacement even changed what a result *is*, from a list of rows
to a cursor over a running plan, and the `Rows` interface written in
milestone 1 already had the shape for it: `Next`, and `Close`.

What is still as simple as it was on day one: one writer at a time, under
a database-wide lock. MVCC gave isolation, not parallel writes.

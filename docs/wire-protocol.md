# Wire protocol support

CapivaraDB implements the server side of the PostgreSQL frontend/backend
protocol, version 3.0, as specified in
[chapter 55 of the PostgreSQL manual](https://www.postgresql.org/docs/current/protocol.html).
This page lists what is implemented, message by message.

## Startup

| Client sends | Server does |
|--------------|-------------|
| `SSLRequest` | Answers `N`; the client continues in plaintext on the same connection |
| `GSSENCRequest` | Answers `N` |
| `CancelRequest` | Cancels the matching statement if the key is right; closes without replying either way |
| `StartupMessage` 3.0 | `AuthenticationOk`, `ParameterStatus`…, `BackendKeyData`, `ReadyForQuery` |
| `StartupMessage` 3.x, x > 0, or with `_pq_.*` options | Same, preceded by `NegotiateProtocolVersion` naming 3.0 and the options not understood |
| Any other major version | `FATAL 0A000` |
| No `user` parameter | `FATAL 28000` |

`ParameterStatus` values sent: `server_version`, `server_encoding`,
`client_encoding`, `DateStyle`, `TimeZone`, `integer_datetimes`,
`standard_conforming_strings`, `application_name`, `session_authorization`,
`is_superuser`.

`server_version` is `16.0`. Drivers parse it to choose behaviour, so it has
to look like a PostgreSQL version; `select version()` tells the truth.

The process ID in `BackendKeyData` is a per-server counter (there is no
process per connection); the secret is 32 random bits from `crypto/rand`,
compared in constant time.

## Frontend messages

| Message | Supported | Notes |
|---------|-----------|-------|
| `Query` (Q) | yes | Any number of statements; empty string gives `EmptyQueryResponse` |
| `Parse` (P) | yes | Named and unnamed; declared parameter types are optional |
| `Bind` (B) | yes | Text and binary parameters; per-column result formats |
| `Describe` (D) | yes | Statement and portal variants |
| `Execute` (E) | yes | Row limit honoured; `PortalSuspended` when reached |
| `Close` (C) | yes | Closing a statement closes its portals |
| `Sync` (S) | yes | |
| `Flush` (H) | yes | |
| `Terminate` (X) | yes | |
| `CopyData` / `CopyDone` / `CopyFail` | ignored | There is no `COPY` |
| `FunctionCall` (F) | rejected | `0A000`; obsolete sub-protocol |
| `PasswordMessage` (p) | n/a | Authentication is trust-only |

## Backend messages

`AuthenticationOk`, `ParameterStatus`, `BackendKeyData`, `ReadyForQuery`,
`RowDescription`, `DataRow`, `CommandComplete`, `EmptyQueryResponse`,
`ErrorResponse`, `ParseComplete`, `BindComplete`, `CloseComplete`,
`ParameterDescription`, `NoData`, `PortalSuspended`,
`NegotiateProtocolVersion`.

Not sent: `NoticeResponse`, `NotificationResponse`, the `Copy*` responses,
and `ParameterStatus` updates after `SET`.

## Behaviour worth knowing

**Error recovery in the extended protocol.** After an error, every message
is discarded until `Sync`, then `ReadyForQuery` is sent. A driver that
pipelines `Parse`/`Bind`/`Execute`/`Sync` therefore always receives exactly
one error and one `ReadyForQuery`.

**Transaction status.** `ReadyForQuery` carries `I`, `T` or `E`. Any error
reported while in a transaction block moves it to `E`, after which every
statement except `COMMIT`/`ROLLBACK` fails with `25P02`. `COMMIT` in that
state rolls back and answers `ROLLBACK`.

**Portal lifetime.** Portals are destroyed when their transaction ends.
Outside a transaction block that is at the next `ReadyForQuery`, so a
suspended portal can only be resumed inside `BEGIN … COMMIT`.

**Flushing.** Output is flushed at `ReadyForQuery`, on `Flush`, and after an
error — not after each message.

**Cancellation.** A `CancelRequest` cancels the `context.Context` of the
statement running on the target connection, which surfaces as `57014`. The
connection stays usable. A request with a wrong key is silently ignored.

**Limits.** A startup packet may be at most 10 kB and a regular message at
most 64 MB; larger length prefixes are a `FATAL 08P01` without allocating.

**Error fields.** `S`, `V`, `C`, `M` always; `D`, `H`, `P` when available.
`P` is a 1-based character offset into the query text.

## Formats

| Type | Text | Binary |
|------|------|--------|
| `boolean` | `t` / `f` | 1 byte |
| `integer` | decimal | 4 bytes, big-endian |
| `bigint` | decimal | 8 bytes, big-endian |
| `double precision` | shortest round-trip decimal, `NaN`, `Infinity` | IEEE 754, 8 bytes |
| `text` | UTF-8 bytes | UTF-8 bytes |

Parameters may additionally be declared as `smallint` (21), `real` (700),
`varchar` (1043), `bpchar` (1042) or `name` (19); they are decoded in their
own width and handled as the nearest supported type.

Client encoding is assumed to be UTF-8; a `client_encoding` requested at
startup is not honoured.

## How it is tested

- `internal/pgwire/server_test.go`: a raw client written in the test file,
  asserting exact message sequences (for example `1tTZ` for
  `Parse`/`Describe`/`Sync`).
- `compat/pgx`: pgx in all five query execution modes.
- `compat/jdbc`: pgjdbc, including the switch to named server-side
  statements and binary transfer after the fifth execution.
- `compat/psql`: a psql session compared against a checked-in transcript.

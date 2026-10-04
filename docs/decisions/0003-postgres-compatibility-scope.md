# 3. How far PostgreSQL compatibility goes

Status: accepted

## Context

"PostgreSQL-compatible" can mean anything from "psql connects" to "runs
the PostgreSQL regression suite". The scope has to be explicit, or it
grows without bound.

## Decision

Compatibility is a property of the **protocol and of observable
behaviour**, not of the catalog or the feature list.

In scope:

- The v3 protocol as real drivers use it, including the parts that are
  easy to get subtly wrong: pipelining, error recovery, binary formats,
  parameter type inference, portal suspension, cancellation.
- SQLSTATE codes, command tags and error wording matching PostgreSQL where
  an equivalent exists, because client code depends on them.
- PostgreSQL's semantics for the SQL that is implemented: three-valued
  logic, integer overflow as an error, literal typing, transaction status
  and the failed-transaction state.

Out of scope, at least until the seven milestones are done:

- `pg_catalog` and `information_schema`, and therefore psql's `\d` family
  and GUI tools that introspect the server.
- Authentication methods beyond trust, and TLS.
- `COPY`, `LISTEN`/`NOTIFY`, replication.
- The long tail of data types and functions.

## Why

The interesting engineering is in storage, recovery, concurrency and
planning. Emulating the catalog is a large amount of work that teaches
little and proves nothing about those. Being exact about the protocol, on
the other hand, is what makes every other part testable with real clients.

## Consequences

- `server_version` reports `16.0` so that drivers enable modern behaviour;
  `select version()` identifies the server honestly.
- Tools that need the catalog will not work, and the README says so.
- Compatibility is measured rather than claimed: by the driver tests now,
  and by a sqllogictest pass rate from milestone 2 onwards.

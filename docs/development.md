# Development

## Requirements

- Go 1.27+
- For the client smoke tests: a JDK 11+ and `curl` (JDBC); `psql` or Docker
  (psql). They are optional — `go test ./...` needs only Go.

## Commands

| What | Command |
|------|---------|
| Run the server | `go run ./cmd/capivaradb -addr 127.0.0.1:5432 -v` |
| Core tests | `go test ./...` |
| Core tests with the race detector | `go test -race ./...` (needs a C compiler) |
| pgx driver tests | `cd compat/pgx && go test ./...` |
| JDBC smoke test | `bash scripts/jdbc-smoke.sh` |
| psql smoke test | `bash scripts/psql-smoke.sh` |
| Accept new psql output | `bash scripts/psql-smoke.sh --update` |
| Regenerate screenshots | `bash scripts/screenshots.sh m1` |
| Format check | `gofmt -l .` (must print nothing) |

The scripts are bash and run unchanged on Linux and under Git Bash on
Windows. They build the server into `.cache/`, start it on a private port
and stop it on exit.

## Module layout

There are two Go modules:

- The root module is the database. It has **no `require` lines** and CI
  checks that it stays that way.
- `compat/pgx` is a separate module that depends on pgx and points back at
  the root with a `replace` directive. Test-only dependencies live there so
  they can never leak into the database.

## Screenshots

`docs/screenshots/*.svg` are not hand-made. `scripts/screenshots.sh` runs
the real commands and pipes their output through `tools/termshot`, which
draws a terminal window as SVG. To add one, add a command to that script.
Files are prefixed with the milestone (`m1-`, `m2-`, …) so the history of
the project stays visible.

## Conventions

- Errors that can reach a client are `*pgerr.Error` values with a SQLSTATE
  from PostgreSQL's Appendix A, created where the problem is detected.
- Error messages copy PostgreSQL's wording where there is an equivalent;
  tools and people already know how to read them.
- Comments explain why, not what. Limitations are written down where they
  live in the code and collected in the README.
- Source files are UTF-8 with LF line endings (`.gitattributes` enforces it,
  which matters on Windows).

## Windows notes

- The race detector needs cgo and therefore `gcc`; without it, run the
  plain tests locally and rely on CI for `-race`.
- With no local `psql`, the psql smoke test runs it from the
  `postgres:17-alpine` image and reaches the server through
  `host.docker.internal`, so for that test only the server listens on all
  interfaces. Windows may show a firewall prompt the first time.

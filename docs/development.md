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
| psql smoke tests | `bash scripts/psql-smoke.sh` |
| sqllogictest | `bash scripts/slt.sh` (add `-v 20` to see failing records) |
| Accept new sqllogictest counts | `bash scripts/slt.sh --update` (also rewrites `docs/sqllogictest.md`) |
| Fuzz the parser | `go test ./internal/sql -run XXX -fuzz FuzzParse -fuzztime 1m` |
| Accept new psql output | `bash scripts/psql-smoke.sh --update` |
| Kill-and-recover test through psql | `bash scripts/restart-smoke.sh` |
| Crash tests (simulated disk, real kill) | `go test ./internal/engine -run "TestCrashRecovery|TestKillProcess" -v` |
| Mutation-test the crash, isolation, planner and executor tests | `bash scripts/mutation-test.sh` |
| Executor tests | `go test ./internal/engine -v -run "TestSetOperations|TestOuterJoins|TestSpilling|TestCursor|TestMemory"` |
| Planner tests | `go test ./internal/engine -v -run "TestPlansAgree|TestJoinOrder|TestAccessPath"` |
| See a plan | `explain analyze select ...` from any client |
| Isolation transcripts | `go test ./internal/engine -v -run "TestNoDirtyRead|TestDeadlock|TestWriteSkew"` |
| Fuzz the B+tree | `go test ./internal/storage -run XXX -fuzz FuzzTree -fuzztime 1m` |
| Benchmarks | `bash scripts/bench.sh` |
| Check a database file | `go run ./cmd/capivaradb -data file.cdb -check` |
| Regenerate screenshots | `bash scripts/screenshots.sh m7` |
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

## Adding SQL

A new construct usually touches four places, in this order:

1. `internal/sql/ast.go` and `parser.go` — the node and its grammar.
2. `internal/sql/format.go` — how it prints. Add a line to the corpus in
   `parser_test.go` with its canonical form; the round-trip test and the
   fuzzer then cover it automatically.
3. `internal/engine/bind.go` — name resolution and typing.
4. A table-driven case in `internal/engine/query_test.go`, error cases
   included, and `bash scripts/slt.sh` to see that nothing regressed.

A crasher found by the fuzzer is saved under
`internal/sql/testdata/fuzz/FuzzParse/`; commit it with the fix so that it
runs as a regression test from then on.

## Changing the storage or logging code

Three things to do before trusting a change under `internal/storage`:

1. `go test ./internal/storage ./internal/engine` — the crash tests are
   part of the normal run.
2. `bash scripts/mutation-test.sh` — it also fails if a mutant no longer
   applies because the line it targets has moved, which is the cue to
   update the script.
3. If the change adds a rule the design now depends on, add a mutant that
   breaks it, and make sure something fails.

A failing crash test prints its seed and round. The workload and the
simulated disk are both seeded, so the failure replays exactly.

#!/usr/bin/env bash
# Regenerates docs/screenshots from real command output.
#
#   bash scripts/screenshots.sh m2      the pictures of one milestone
#
# Every picture is the captured output of a command that is run here, drawn
# as SVG by tools/termshot. Nothing is typed by hand. Pictures of earlier
# milestones are left alone unless asked for: they record what the project
# looked like at the time.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

MILESTONE="${1:?usage: screenshots.sh m1|m2|m3}"
OUT="$ROOT/docs/screenshots"
mkdir -p "$OUT"
cd "$ROOT"

shot() { # shot NAME TITLE  (text on stdin)
  go run ./tools/termshot -title "$2" -o "$OUT/$MILESTONE-$1.svg"
  echo "wrote docs/screenshots/$MILESTONE-$1.svg"
}

# go test output without the timings, which change on every run.
tests() { go test -count=1 "$@" 2>&1 | grep -v 'no test files' | sed -E 's/\t[0-9.]+s$//'; }

case "$MILESTONE" in
m1)
  # psql talking to the server: the session and its expected output are the
  # same files the psql smoke test checks.
  bash scripts/psql-smoke.sh >/dev/null
  sed -n '/^select version/,/^-- Transactions/p' "$CACHE/psql-session.out" | sed '$d' |
    shot psql "psql (PostgreSQL 17) connected to CapivaraDB"
  sed -n '/^-- Transactions/,$p' "$CACHE/psql-session.out" |
    shot psql-errors "psql: transactions and error positions"

  # The official JDBC driver.
  { echo '$ scripts/jdbc-smoke.sh'; bash scripts/jdbc-smoke.sh 2>&1 | grep -v '^downloading'; } |
    shot jdbc "pgjdbc smoke test"

  # The test suites: the dependency-free core and the pgx driver tests.
  {
    echo '$ go test -count=1 ./...'
    tests ./...
    echo
    echo '$ cd compat/pgx && go test -count=1 -v ./... | grep -E "^(---|ok|FAIL)"'
    (cd compat/pgx && go test -count=1 -v ./... 2>&1 | grep -E '^(---|ok|FAIL)' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//')
  } | shot tests "go test"
  ;;

m2)
  bash scripts/psql-smoke.sh >/dev/null
  sed -n '/^-- Joins/,/^-- Subqueries/p' "$CACHE/psql-queries.out" | sed '$d' |
    shot psql-joins "psql: joins and aggregates"
  sed -n '/^-- Subqueries/,/^-- Constraints/p' "$CACHE/psql-queries.out" | sed '$d' |
    shot psql-subqueries "psql: subqueries, CASE, DISTINCT, LIMIT"
  sed -n '/^-- Constraints/,$p' "$CACHE/psql-queries.out" |
    shot psql-binder "psql: constraints and binder errors"

  # sqllogictest: the report of the regular run, without the timing column.
  { echo '$ scripts/slt.sh'; bash scripts/slt.sh 2>/dev/null | sed -E 's/ +[0-9.]+s$//; s/ +time$//'; } |
    shot slt "sqllogictest"

  # The parser fuzzer, for a fixed 30 seconds.
  {
    echo '$ go test ./internal/sql -run XXX -fuzz FuzzParse -fuzztime 30s'
    go test ./internal/sql -run XXX -fuzz FuzzParse -fuzztime 30s 2>&1 |
      grep -E '^(fuzz: elapsed: (0s|15s|30s),|PASS|ok|FAIL)' | sed -E 's/\t[0-9.]+s$//' | awk '!seen[$0]++'
  } | shot fuzz "parser fuzzing: parse, print, parse again"
  ;;

m3)
  # Data surviving the server process, through psql.
  bash scripts/restart-smoke.sh >/dev/null
  sed -n '1,/^## a new server/p' "$CACHE/restart.out" | sed '$d' |
    shot restart-before "first server: write, checkpoint, kill"
  sed -n '/^## a new server/,$p' "$CACHE/restart.out" |
    shot restart-after "second server, same file"

  # The storage tests, with what they report about the tree and the pool.
  {
    echo '$ go test ./internal/storage -v -run "TestRandomOperations|TestPersistence|TestChecksum"'
    go test -count=1 ./internal/storage -v -run 'TestRandomOperations|TestPersistence|TestChecksum' 2>&1 |
      grep -vE '^=== ' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//; s/^ +btree_test.go:[0-9]+: /    /'
    echo
    echo '$ go test ./internal/engine -v -run "Restart|BufferPool"'
    go test -count=1 ./internal/engine -v -run 'Restart|BufferPool' 2>&1 |
      grep -vE '^=== ' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//; s/^ +persist_test.go:[0-9]+: /    /'
  } | shot storage-tests "storage engine tests"

  { echo '$ scripts/bench.sh'; bash scripts/bench.sh 2>&1 | grep -vE '^(PASS|ok)'; } |
    shot bench "B+tree benchmarks"
  ;;

*)
  echo "unknown milestone: $MILESTONE" >&2
  exit 2
  ;;
esac

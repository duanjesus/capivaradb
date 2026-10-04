#!/usr/bin/env bash
# Regenerates docs/screenshots from real command output.
#
# Every picture is the captured output of a command that is run here, drawn
# as SVG by tools/termshot. Nothing is typed by hand.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

MILESTONE="${1:-m1}"
OUT="$ROOT/docs/screenshots"
mkdir -p "$OUT"
cd "$ROOT"

shot() { # shot NAME TITLE  (text on stdin)
  go run ./tools/termshot -title "$2" -o "$OUT/$MILESTONE-$1.svg"
  echo "wrote docs/screenshots/$MILESTONE-$1.svg"
}

# 1. psql talking to the server: the session and its expected output are the
#    same files the psql smoke test checks.
bash scripts/psql-smoke.sh >/dev/null
sed -n '/^select version/,/^-- Transactions/p' "$CACHE/psql.out" | sed '$d' |
  shot psql "psql (PostgreSQL 17) connected to CapivaraDB"
sed -n '/^-- Transactions/,$p' "$CACHE/psql.out" |
  shot psql-errors "psql: transactions and error positions"

# 2. The official JDBC driver.
{ echo '$ scripts/jdbc-smoke.sh'; bash scripts/jdbc-smoke.sh 2>&1 | grep -v '^downloading'; } |
  shot jdbc "pgjdbc smoke test"

# 3. The test suites: the dependency-free core and the pgx driver tests.
{
  echo '$ go test -count=1 ./...'
  go test -count=1 ./... 2>&1 | grep -v 'no test files' | sed -E 's/\t[0-9.]+s$//'
  echo
  echo '$ cd compat/pgx && go test -count=1 -v ./... | grep -E "^(---|ok|FAIL)"'
  (cd compat/pgx && go test -count=1 -v ./... 2>&1 | grep -E '^(---|ok|FAIL)' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//')
} | shot tests "go test"

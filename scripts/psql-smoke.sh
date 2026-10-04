#!/usr/bin/env bash
# Runs every compat/psql/*.sql through the real psql and compares the output
# with the .out file next to it.
#
# Uses psql from PATH if there is one, otherwise the postgres Docker image.
# Pass --update to rewrite the .out files from the current output.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PORT="${PORT:-54330}"
setup_psql "$PORT"
start_server "$PORT" "$PSQL_BIND"

status=0
for session in "$ROOT"/compat/psql/*.sql; do
  name="$(basename "$session" .sql)"
  # session.sql keeps its historical expected.out; the others use NAME.out.
  expected="$ROOT/compat/psql/$name.out"
  [ "$name" = session ] && expected="$ROOT/compat/psql/expected.out"
  actual="$CACHE/psql-$name.out"

  run_psql <"$session" >"$actual"
  compare_output "psql $name" "$expected" "$actual" "${1:-}" || status=1
done
exit $status

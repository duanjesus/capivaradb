#!/usr/bin/env bash
# Shows that data survives the server process: a psql session writes to a
# database file and checkpoints, the server is killed, a new server opens
# the same file, and a second session reads everything back.
#
# The combined transcript is compared with compat/restart/expected.out.
# Pass --update to rewrite it.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PORT="${PORT:-54331}"
DATA="$CACHE/restart.cdb"
ACTUAL="$CACHE/restart.out"
rm -f "$DATA"

setup_psql "$PORT"
{
  echo "## server started on an empty file"
  start_server "$PORT" "$PSQL_BIND" -data "$DATA"
  run_psql <"$ROOT/compat/restart/before.sql"
  stop_server

  echo
  echo "## server killed; checking the file offline"
  "$BIN" -data "$DATA" -check | sed -E 's/[0-9]+ pages \([0-9]+ kB\), [0-9]+ free/N pages/'

  echo
  echo "## a new server opens the same file"
  start_server "$PORT" "$PSQL_BIND" -data "$DATA"
  run_psql <"$ROOT/compat/restart/after.sql"
  stop_server
} >"$ACTUAL"

compare_output "restart" "$ROOT/compat/restart/expected.out" "$ACTUAL" "${1:-}"

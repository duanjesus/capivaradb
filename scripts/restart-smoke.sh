#!/usr/bin/env bash
# Shows crash recovery end to end through psql: a session commits some
# changes and leaves a transaction open, the server is killed outright (no
# checkpoint, no clean shutdown), a new server opens the same files and
# recovers, and a second session finds exactly the committed data.
#
# The combined transcript is compared with compat/restart/expected.out.
# Pass --update to rewrite it.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PORT="${PORT:-54331}"
DATA="$CACHE/restart.cdb"
ACTUAL="$CACHE/restart.out"
rm -f "$DATA" "$DATA.wal"

# The session must still be connected, with its transaction open, when the
# server dies. psql therefore runs in the background, kept alive by a sleep
# at the end of its input, and the server is killed while it waits.
setup_psql "$PORT"
{
  echo "## server started on an empty file"
  start_server "$PORT" "$PSQL_BIND" -data "$DATA"
  { cat "$ROOT/compat/restart/before.sql"; sleep 3; } | run_psql &
  client=$!
  sleep 2
  kill -9 "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
  wait "$client" 2>/dev/null || true
} 2>/dev/null | grep -v -e '^server closed the connection' -e 'terminated abnormally' -e 'before or while processing' -e '^connection to server was lost' >"$ACTUAL"

{
  echo
  echo "## server killed with kill -9; a new one opens the same files"
  start_server "$PORT" "$PSQL_BIND" -data "$DATA"
  # What the server said on the way up, with the counts blanked: they
  # depend on how far the kill let things get.
  grep '^recovered' "$CACHE/server.log" | sed -E 's/[0-9]+/N/g'
  run_psql <"$ROOT/compat/restart/after.sql"
  stop_server

  echo
  echo "## offline check of the file"
  "$BIN" -data "$DATA" -check 2>/dev/null | sed -E 's/[0-9]+ pages \([0-9]+ kB\), [0-9]+ free/N pages/'
} >>"$ACTUAL"

compare_output "restart" "$ROOT/compat/restart/expected.out" "$ACTUAL" "${1:-}"

#!/usr/bin/env bash
# Runs compat/psql/session.sql through the real psql and compares the output
# with compat/psql/expected.out.
#
# Uses psql from PATH if there is one, otherwise the postgres Docker image.
# Pass --update to rewrite expected.out from the current output.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PORT="${PORT:-54330}"
SESSION="$ROOT/compat/psql/session.sql"
EXPECTED="$ROOT/compat/psql/expected.out"
ACTUAL="$CACHE/psql.out"

# -X: ignore ~/.psqlrc. -a: echo the input. VERBOSITY and the fixed pager /
# encoding settings keep the output identical across machines.
PSQL_ARGS=(-X -a -v VERBOSITY=default -P pager=off -U ana -d capi -p "$PORT")

if command -v psql >/dev/null 2>&1; then
  start_server "$PORT"
  PGCLIENTENCODING=UTF8 psql "${PSQL_ARGS[@]}" -h 127.0.0.1 <"$SESSION" >"$ACTUAL" 2>&1 || true
else
  # The container reaches the host through host.docker.internal, which is
  # not the loopback interface, so the server has to listen on all of them.
  start_server "$PORT" 0.0.0.0
  # stderr is merged inside the container: Docker carries the two streams
  # separately, and merging them out here would shuffle errors and results.
  MSYS_NO_PATHCONV=1 docker run --rm -i --add-host=host.docker.internal:host-gateway \
    -e PGCLIENTENCODING=UTF8 "${PSQL_IMAGE:-postgres:17-alpine}" \
    sh -c 'exec psql "$@" 2>&1' sh "${PSQL_ARGS[@]}" -h host.docker.internal <"$SESSION" >"$ACTUAL" || true
fi

if [ "${1:-}" = "--update" ]; then
  cp "$ACTUAL" "$EXPECTED"
  echo "updated $EXPECTED"
  exit 0
fi

if diff -u --strip-trailing-cr "$EXPECTED" "$ACTUAL"; then
  echo "psql session matches expected output ($(grep -c '' "$EXPECTED") lines)"
else
  echo "psql output differs from compat/psql/expected.out" >&2
  exit 1
fi

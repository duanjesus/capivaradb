#!/usr/bin/env bash
# Runs every compat/psql/*.sql through the real psql and compares the output
# with the .out file next to it.
#
# Uses psql from PATH if there is one, otherwise the postgres Docker image.
# Pass --update to rewrite the .out files from the current output.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PORT="${PORT:-54330}"

# -X: ignore ~/.psqlrc. -a: echo the input. VERBOSITY and the fixed pager /
# encoding settings keep the output identical across machines.
PSQL_ARGS=(-X -a -v VERBOSITY=default -P pager=off -U ana -d capi -p "$PORT")

if command -v psql >/dev/null 2>&1; then
  start_server "$PORT"
  run_psql() { PGCLIENTENCODING=UTF8 psql "${PSQL_ARGS[@]}" -h 127.0.0.1 2>&1 || true; }
else
  # The container reaches the host through host.docker.internal, which is
  # not the loopback interface, so the server has to listen on all of them.
  start_server "$PORT" 0.0.0.0
  # stderr is merged inside the container: Docker carries the two streams
  # separately, and merging them out here would shuffle errors and results.
  run_psql() {
    MSYS_NO_PATHCONV=1 docker run --rm -i --add-host=host.docker.internal:host-gateway \
      -e PGCLIENTENCODING=UTF8 "${PSQL_IMAGE:-postgres:17-alpine}" \
      sh -c 'exec psql "$@" 2>&1' sh "${PSQL_ARGS[@]}" -h host.docker.internal || true
  }
fi

status=0
for session in "$ROOT"/compat/psql/*.sql; do
  name="$(basename "$session" .sql)"
  # session.sql keeps its historical expected.out; the others use NAME.out.
  expected="$ROOT/compat/psql/$name.out"
  [ "$name" = session ] && expected="$ROOT/compat/psql/expected.out"
  actual="$CACHE/psql-$name.out"

  run_psql <"$session" >"$actual"

  if [ "${1:-}" = "--update" ]; then
    cp "$actual" "$expected"
    echo "updated ${expected#"$ROOT"/}"
  elif diff -u --strip-trailing-cr "$expected" "$actual"; then
    echo "psql $name: matches expected output ($(grep -c '' "$expected") lines)"
  else
    echo "psql $name: output differs from ${expected#"$ROOT"/}" >&2
    status=1
  fi
done
exit $status

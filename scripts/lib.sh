# Shared helpers for the smoke scripts. Source it; do not run it.
#
# The scripts are bash so that the same file runs on Linux in CI and under
# Git Bash on Windows.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CACHE="$ROOT/.cache"
mkdir -p "$CACHE"

EXE=""
case "$(uname -s)" in MINGW* | MSYS* | CYGWIN*) EXE=".exe" ;; esac
BIN="$CACHE/capivaradb$EXE"

SERVER_PID=""

build_server() {
  (cd "$ROOT" && go build -o "$BIN" ./cmd/capivaradb)
}

# start_server PORT [ADDR [SERVER ARGS...]]: builds the server and runs it in
# the background.
start_server() {
  local port="$1" addr="${2:-127.0.0.1}"
  shift
  [ $# -gt 0 ] && shift
  build_server
  "$BIN" -addr "$addr:$port" "$@" 2>"$CACHE/server.log" &
  SERVER_PID=$!
  trap stop_server EXIT
  # Wait until it accepts connections.
  for _ in $(seq 1 50); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  echo "server did not start; log follows" >&2
  cat "$CACHE/server.log" >&2
  return 1
}

stop_server() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    SERVER_PID=""
  fi
}

# setup_psql PORT: defines run_psql, which feeds stdin to psql and prints
# its output, and sets PSQL_BIND to the address the server must listen on
# for it. psql comes from PATH if there is one, otherwise from the postgres
# Docker image.
setup_psql() {
  # -X: ignore ~/.psqlrc. -a: echo the input. VERBOSITY and the fixed pager
  # and encoding settings keep the output identical across machines.
  PSQL_ARGS=(-X -a -v VERBOSITY=default -P pager=off -U ana -d capi -p "$1")
  if command -v psql >/dev/null 2>&1; then
    PSQL_BIND=127.0.0.1
    run_psql() { PGCLIENTENCODING=UTF8 psql "${PSQL_ARGS[@]}" -h 127.0.0.1 2>&1 || true; }
  else
    # The container reaches the host through host.docker.internal, which is
    # not the loopback interface, so the server has to listen on all of them.
    PSQL_BIND=0.0.0.0
    # stderr is merged inside the container: Docker carries the two streams
    # separately, and merging them out here would shuffle errors and results.
    run_psql() {
      MSYS_NO_PATHCONV=1 docker run --rm -i --add-host=host.docker.internal:host-gateway \
        -e PGCLIENTENCODING=UTF8 "${PSQL_IMAGE:-postgres:17-alpine}" \
        sh -c 'exec psql "$@" 2>&1' sh "${PSQL_ARGS[@]}" -h host.docker.internal || true
    }
  fi
}

# compare_output NAME EXPECTED ACTUAL [--update]: diffs a transcript against
# its checked-in version, or replaces it.
compare_output() {
  local name="$1" expected="$2" actual="$3"
  if [ "${4:-}" = "--update" ]; then
    cp "$actual" "$expected"
    echo "updated ${expected#"$ROOT"/}"
  elif diff -u --strip-trailing-cr "$expected" "$actual"; then
    echo "$name: matches expected output ($(grep -c '' "$expected") lines)"
  else
    echo "$name: output differs from ${expected#"$ROOT"/}" >&2
    return 1
  fi
}

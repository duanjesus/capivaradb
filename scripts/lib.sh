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

# start_server PORT [ADDR]: builds the server and runs it in the background.
start_server() {
  local port="$1" addr="${2:-127.0.0.1}"
  (cd "$ROOT" && go build -o "$BIN" ./cmd/capivaradb)
  "$BIN" -addr "$addr:$port" 2>"$CACHE/server.log" &
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

#!/usr/bin/env bash
# Runs compat/jdbc/Smoke.java against a freshly started server using the
# official PostgreSQL JDBC driver. Needs a JDK (11+) and curl.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PGJDBC_VERSION="${PGJDBC_VERSION:-42.7.7}"
PORT="${PORT:-54329}"
JAR="$CACHE/postgresql-$PGJDBC_VERSION.jar"

if [ ! -f "$JAR" ]; then
  echo "downloading pgjdbc $PGJDBC_VERSION from Maven Central"
  curl -fsSL -o "$JAR" \
    "https://repo1.maven.org/maven2/org/postgresql/postgresql/$PGJDBC_VERSION/postgresql-$PGJDBC_VERSION.jar"
fi

start_server "$PORT"

# Java wants native paths and the native classpath separator on Windows.
SRC="$ROOT/compat/jdbc/Smoke.java"
if [ -n "$EXE" ]; then
  JAR="$(cygpath -w "$JAR")"
  SRC="$(cygpath -w "$SRC")"
fi
java -cp "$JAR" "$SRC" "jdbc:postgresql://127.0.0.1:$PORT/capi"

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

MILESTONE="${1:?usage: screenshots.sh m1|m2|m3|m4|m5|m6|m7}"
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

m4)
  # A server killed in the middle of a transaction, through psql.
  bash scripts/restart-smoke.sh >/dev/null
  sed -n '/^-- A transaction left open/,/^-- So are the rules/p' "$CACHE/restart.out" | sed '$d' |
    shot recovery "kill -9 mid-transaction, then recovery"

  # The crash tests, with what they report.
  {
    echo '$ go test ./internal/engine -v -run "TestCrashRecovery|TestKillProcess"'
    go test -count=1 ./internal/engine -v -run 'TestCrashRecovery|TestKillProcess' 2>&1 |
      grep -vE '^=== ' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//; s/^ +[a-z_]+_test.go:[0-9]+: /    /' | fold -s -w 110
    echo
    echo '$ go test ./internal/storage -v -run "Committed|Uncommitted|Torn|LogIs|Broken|UndoIsNot"'
    go test -count=1 ./internal/storage -v -run 'Committed|Uncommitted|Torn|LogIs|Broken|UndoIsNot' 2>&1 |
      grep -vE '^=== ' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//'
  } | shot crash-tests "crash tests"

  { echo '$ scripts/mutation-test.sh'; bash scripts/mutation-test.sh 2>&1; } |
    shot mutation "mutation testing: break a rule, the crash tests must fail"

  { echo '$ scripts/bench.sh'; bash scripts/bench.sh 2>&1 | grep -E '^(cpu|Benchmark)' | awk '!seen[$0]++' | sed -E 's/-8 +/  /; s/\t+/  /g'; } |
    shot bench "benchmarks: what a commit costs"
  ;;

m5)
  # One transcript for the README: the lost update that does not happen.
  {
    echo '$ go test ./internal/engine -v -run "TestReadCommittedUpdateWaits|TestRepeatableReadPreventsLostUpdate"'
    go test -count=1 ./internal/engine -v -run 'TestReadCommittedUpdateWaits|TestRepeatableReadPreventsLostUpdate' 2>&1 |
      grep -vE '^=== ' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//; s/^ +[a-z_]+_test.go:[0-9]+: /    /'
  } | shot isolation "two sessions, one row: read committed waits, repeatable read refuses"

  # The anomalies, deadlocks and write skew.
  {
    echo '$ go test ./internal/engine -v -run "TestNoDirtyRead|TestRepeatableReadIsStable|TestDeadlockIsDetected|TestWriteSkew"'
    go test -count=1 ./internal/engine -v -run 'TestNoDirtyRead|TestRepeatableReadIsStable|TestDeadlockIsDetected|TestWriteSkew' 2>&1 |
      grep -vE '^=== ' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//; s/^ +[a-z_]+_test.go:[0-9]+: /    /'
  } | shot transcripts "isolation transcripts"

  # Concurrency under load, and vacuum.
  {
    echo '$ go test ./internal/engine -v -run "TestConcurrentTransfers|TestVacuum|TestAutoVacuum|TestUnique"'
    go test -count=1 ./internal/engine -v -run 'TestConcurrentTransfers|TestVacuum|TestAutoVacuum|TestUnique' 2>&1 |
      grep -E '^(---|    ---|PASS|ok|FAIL)|transfers by' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//; s/^ +[a-z_]+_test.go:[0-9]+: /        /' | fold -s -w 110
  } | shot concurrency "concurrent transfers, unique conflicts, vacuum"

  { echo '$ scripts/mutation-test.sh'; bash scripts/mutation-test.sh 2>&1; } |
    shot mutation "mutation testing: break a rule, the tests must fail"
  ;;

m6)
  # EXPLAIN through psql: the chosen plan, then the same query with the
  # planner's switches off.
  bash scripts/psql-smoke.sh >/dev/null
  sed -n '/^-- A join written orders-first/,/^set enable_indexscan = on/p' "$CACHE/psql-planner.out" | sed '$d' |
    shot explain "psql: the plan chosen, and the plan rejected"
  sed -n '/^-- The primary key finds/,/^-- A join written orders-first/p' "$CACHE/psql-planner.out" | sed '$d' |
    shot access-paths "psql: primary key, secondary index, sequential scan"
  sed -n '/^-- EXPLAIN ANALYZE runs/,$p' "$CACHE/psql-planner.out" |
    shot explain-analyze "psql: EXPLAIN ANALYZE, and plans of UPDATE and DELETE"

  # The planner tests, with what the random comparison reports.
  {
    echo '$ go test ./internal/engine -v -run "TestAccessPath|TestIndexScan|TestPredicatePushdown|TestJoinOrder|TestLeftJoinPlans|TestExplain|TestStatistics|TestPlansAgree|TestSubqueryConditions"'
    go test -count=1 ./internal/engine -v -run 'TestAccessPath|TestIndexScan|TestPredicatePushdown|TestJoinOrder|TestLeftJoinPlans|TestExplain|TestStatistics|TestPlansAgree|TestSubqueryConditions' 2>&1 |
      grep -vE '^=== ' | sed -E 's/ \([0-9.]+s\)$//; s/\t[0-9.]+s$//; s/^ +[a-z_]+_test.go:[0-9]+: /    /' | fold -s -w 110
  } | shot planner-tests "planner tests: any plan, same answer"

  # sqllogictest: the report of the regular run, without the timing column.
  { echo '$ scripts/slt.sh'; bash scripts/slt.sh 2>/dev/null | sed -E 's/ +[0-9.]+s$//; s/ +time$//'; } |
    shot slt "sqllogictest, with the two many-table join scripts"

  {
    echo '$ go test ./internal/engine -run XXX -bench "PointLookup|SecondaryIndex|UpdateByKey|ThreeTableJoin"'
    go test ./internal/engine -run XXX -bench 'PointLookup|SecondaryIndex|UpdateByKey|ThreeTableJoin' -benchtime "${BENCHTIME:-2s}" 2>&1 |
      grep -E '^(cpu|Benchmark)' | sed -E 's/-8 +/  /; s/\t+/  /g'
  } | shot bench "benchmarks: the chosen plan against the rejected one"

  { echo '$ scripts/mutation-test.sh'; bash scripts/mutation-test.sh 2>&1; } |
    shot mutation "mutation testing: break a rule, the tests must fail"
  ;;

m7)
  # EXPLAIN ANALYZE through psql: the same query with memory to spare and
  # with almost none.
  bash scripts/psql-smoke.sh >/dev/null
  sed -n '/^-- What each step did/,/^set work_mem = .4MB.;/p' "$CACHE/psql-executor.out" | sed '$d' |
    shot work-mem "psql: one query, with memory to spare and with 64 kB"
  sed -n '/^-- Set operations/,/^-- Enough rows/p' "$CACHE/psql-executor.out" | sed '$d' |
    shot sql "psql: set operations, RIGHT and FULL JOIN, USING"
  sed -n '/^-- No index on orders.customer_id/,/^-- What each step did/p' "$CACHE/psql-executor.out" | sed '$d' |
    shot joins "psql: a hash join, and a merge join that needs no sort"
  sed -n '/^-- A query that wants three rows/,/^drop table orders/p' "$CACHE/psql-executor.out" | sed '$d' |
    shot early "psql: a query that wants three rows reads three rows"

  # The executor tests, with what they measure.
  {
    echo '$ go test ./internal/engine -v -run "TestSetOperations|TestOuterJoins|TestJoinKeys|TestLimitStops|Cursor|TestSpilling|TestHashJoinWithOne|TestTopN|TestExternalSort|TestMemory|TestPlansAgree"'
    go test -count=1 ./internal/engine -v -run 'TestSetOperations|TestOuterJoins|TestJoinKeys|TestLimitStops|Cursor|TestSpilling|TestHashJoinWithOne|TestTopN|TestExternalSort|TestMemory|TestPlansAgree' 2>&1 |
      grep -vE '^=== ' | sed -E 's/ ([0-9.]+s)$//; s/	[0-9.]+s$//; s/^ +[a-z_]+_test.go:[0-9]+: /    /' | fold -s -w 110
  } | shot executor-tests "executor tests"

  # The official JDBC driver, cursor included.
  { echo '$ scripts/jdbc-smoke.sh'; bash scripts/jdbc-smoke.sh 2>&1 | grep -v '^downloading'; } |
    shot jdbc "pgjdbc: a cursor read while the table is deleted under it"

  # sqllogictest: the report of the regular run, without the timing column.
  { echo '$ scripts/slt.sh'; bash scripts/slt.sh 2>/dev/null | sed -E 's/ +[0-9.]+s$//; s/ +time$//'; } |
    shot slt "sqllogictest"

  {
    echo '$ go test ./internal/engine -run XXX -bench "JoinMethod|Sort|FirstRows|SetOperation"'
    go test ./internal/engine -run XXX -bench 'JoinMethod|Sort|FirstRows|SetOperation' -benchtime "${BENCHTIME:-2s}" 2>&1 |
      grep -E '^(cpu|Benchmark)' | sed -E 's/-8 +/  /; s/	+/  /g'
  } | shot bench "benchmarks: join methods, sorts, early termination"

  { echo '$ scripts/mutation-test.sh'; bash scripts/mutation-test.sh 2>&1; } |
    shot mutation "mutation testing: break a rule, the tests must fail"
  ;;

*)
  echo "unknown milestone: $MILESTONE" >&2
  exit 2
  ;;
esac

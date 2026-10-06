#!/usr/bin/env bash
# Tests the tests.
#
# A crash test or an isolation test that passes proves little unless it
# would have failed had the code been wrong. This script breaks the
# machinery in one specific way at a time — skip an fsync, skip the undo
# pass, let a snapshot see uncommitted work — and checks that the tests
# notice every time. A mutant that survives means the tests have a blind
# spot.
#
# The source is restored after each mutant, and on exit whatever happens.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

cd "$ROOT"
PAGER=internal/storage/pager.go
RECOVERY=internal/storage/recovery.go
MVCC=internal/engine/mvcc.go
STORE=internal/engine/store.go
DB=internal/engine/db.go
PLAN=internal/engine/plan.go
FILES=("$PAGER" "$RECOVERY" "$MVCC" "$STORE" "$DB" "$PLAN")

BACKUP="$CACHE/mutation"
mkdir -p "$BACKUP"
backup_of() { echo "$BACKUP/$(echo "$1" | tr '/' '_')"; }
for f in "${FILES[@]}"; do cp "$f" "$(backup_of "$f")"; done
restore() {
  for f in "${FILES[@]}"; do cp "$(backup_of "$f")" "$f"; done
}
trap restore EXIT

survivors=0
# mutant DESCRIPTION FILE SED-EXPRESSION
mutant() {
  local description="$1" file="$2" expression="$3"
  echo "mutant: $description"
  sed -i "$expression" "$file"
  if cmp -s "$file" "$(backup_of "$file")"; then
    echo "  NOT APPLIED: the code it targets has changed; update this script" >&2
    survivors=$((survivors + 1))
    return
  fi
  if "${TESTS[@]}" >/dev/null 2>&1; then
    echo "  SURVIVED: the tests did not notice"
    survivors=$((survivors + 1))
  else
    echo "  killed"
  fi
  restore
}

baseline() {
  echo "baseline: the unmodified code must pass"
  if ! "${TESTS[@]}" >/dev/null 2>&1; then
    echo "  FAILED: fix the tests before mutating" >&2
    exit 1
  fi
  echo "  passes"
}

echo "== durability =="
TESTS=(go test -count=1 -run 'TestCrashRecovery|TestUndoIsNot|TestCommitted|TestUncommitted|TestTorn|TestLogIs' ./internal/engine ./internal/storage)
baseline

mutant "COMMIT returns without syncing the log" "$RECOVERY" \
  's|^\terr := p.wal.flush()$|\tvar err error|'

mutant "a page is written to the data file before its log record is durable" "$PAGER" \
  's|^\tif err := p.wal.flushTo(binary.BigEndian.Uint64(data\[offLSN:\])); err != nil {$|\tif err := error(nil); err != nil {|'

mutant "recovery skips the undo pass" "$RECOVERY" \
  's|^\t\tif err := p.ApplyUndo(item.rec, item.lsn); err != nil {$|\t\tif err := error(nil); err != nil {|'

mutant "recovery skips the redo pass" "$RECOVERY" \
  's|^\t\t\tif err := p.redo(r); err != nil {$|\t\t\tif err := error(nil); err != nil {|'

mutant "a page changed for the first time since a checkpoint is not logged in full" "$PAGER" \
  's|^\t\tbp := \&batchPage{full: !read \|\| !p.imaged\[id\]}$|\t\tbp := \&batchPage{full: !read}|'

mutant "a checkpoint does not sync the data file" "$PAGER" \
  's|^\t\tif err := p.file.Sync(); err != nil {$|\t\tif err := error(nil); err != nil {|'

mutant "a checkpoint discards the log while a transaction is still open" "$PAGER" \
  's|^\tif len(p.active) == 0 {$|\tif true {|'

mutant "undo steps are not marked as done in the log" "$RECOVERY" \
  's|^\tp.nextNote = note{kind: noteUndone, tx: u.Tx, arg: lsn}$|\tp.nextNote = note{}|'

mutant "trees dropped by a committed transaction are not freed after a crash" "$RECOVERY" \
  's|^\t\t\t\tif !st.dropped\[root\] {$|\t\t\t\tif false {|'

echo
echo "== isolation =="
# A deadlock that goes undetected hangs, hence the timeout.
TESTS=(go test -count=1 -timeout 60s -run 'TestNoDirty|TestReadCommitted|TestRepeatableRead|TestSnapshot|TestWaiter|TestDeadlock|TestThreeWay|TestUnique|TestOwnChanges|TestVacuum|TestConcurrentTransfers' ./internal/engine)
baseline

mutant "a snapshot treats transactions still in progress as committed" "$MVCC" \
  's|^\treturn tx == sn.self \|\| (tx < sn.next \&\& !sn.active\[tx\])$|\treturn tx == sn.self \|\| tx < sn.next|'

mutant "a version marked deleted is hidden even if the deleter has not committed" "$MVCC" \
  's|^\treturn sn.sees(xmin) \&\& (xmax == 0 \|\| !sn.sees(xmax))$|\treturn sn.sees(xmin) \&\& xmax == 0|'

mutant "repeatable read takes a new snapshot for every statement" "$DB" \
  's|^\tif s.inTx \&\& s.level == repeatableRead {$|\tif false {|'

mutant "a transaction overwrites a change it cannot see instead of failing" "$STORE" \
  's|^\treturn errSerialization()$|\treturn nil|'

mutant "a write does not wait for the uncommitted change of another transaction" "$STORE" \
  's|^\tif ch.db.isActive(v.xmax) {$|\tif false {|'

mutant "an insert does not wait for an uncommitted row with the same key" "$STORE" \
  's|^\tcase xmin != me \&\& db.isActive(xmin):$|\tcase false:|'

mutant "an insert ignores a key being deleted by a transaction still in progress" "$STORE" \
  's|^\tcase db.isActive(xmax):$|\tcase false:|'

mutant "deadlocks are not detected" "$MVCC" \
  's|^\t\tif next == waiter {$|\t\tif false {|'

mutant "vacuum removes versions that an open snapshot can still see" "$STORE" \
  's/|| !db.goneForEveryone(v.xmin, v.xmax) {$/|| false {/'

mutant "a transaction's versions become visible to others before it commits" "$MVCC" \
  's|^\t\t\tif tx != self {$|\t\t\tif false {|'


echo
echo "== planner =="
# The planner may choose any plan, never a different answer.
TESTS=(go test -count=1 -timeout 120s -run 'TestIndexScanResults|TestLeftJoinPlans|TestPlansAgree|TestJoins|TestSubquer|TestIndexScanRespects' ./internal/engine)
baseline

mutant "a WHERE condition on the nullable side of a LEFT JOIN is pushed below the join" "$PLAN" \
  's|^\t\tif !c.opaque \&\& inside(c.cols, loff, lend) {$|\t\tif !c.opaque {|'

mutant "a LEFT JOIN drops the rows that have no match" "$PLAN" \
  's|if !matched {$|if false {|'

mutant "a condition with a subquery is applied before all tables are joined" "$PLAN" \
  's|^\t\tif c.opaque {$|\t\tif false {|'

mutant "an index scan returns versions the snapshot cannot see" "$STORE" \
  's|^\t\tif sn != nil \&\& !sn.visible(xmin, binary.BigEndian.Uint64(val)) {$|\t\tif false {|'

mutant "an inclusive upper bound of an index range is treated as exclusive" "$STORE" \
  's|^\t\t\t\tif !kb.hiIncl {$|\t\t\t\tif true {|'

mutant "a join condition is not checked when the inner table is reached through an index" "$PLAN" \
  's|^\t\t\t\t\t\t\tok, err = allMatch(evals, en)$|\t\t\t\t\t\t\tok = true|'
echo
if [ "$survivors" -gt 0 ]; then
  echo "$survivors mutant(s) survived"
  exit 1
fi
echo "every mutant was killed"

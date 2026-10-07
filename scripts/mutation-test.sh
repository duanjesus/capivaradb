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
JOIN=internal/engine/join.go
ITER=internal/engine/iter.go
SELECT=internal/engine/select.go
EXEC=internal/engine/exec.go
FILES=("$PAGER" "$RECOVERY" "$MVCC" "$STORE" "$DB" "$PLAN" "$JOIN" "$ITER" "$SELECT" "$EXEC")

# Two runs at once would mutate and restore the same files under each other,
# and take each other's mutants for the original. One at a time.
LOCK="$CACHE/mutation.lock"
if ! mkdir "$LOCK" 2>/dev/null; then
  echo "another mutation run seems to be in progress ($LOCK exists); remove it if not" >&2
  exit 2
fi

BACKUP="$CACHE/mutation"
mkdir -p "$BACKUP"
backup_of() { echo "$BACKUP/$(echo "$1" | tr '/' '_')"; }
for f in "${FILES[@]}"; do cp "$f" "$(backup_of "$f")"; done
restore() {
  for f in "${FILES[@]}"; do cp "$(backup_of "$f")" "$f"; done
}
trap 'restore; rmdir "$LOCK"' EXIT

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
  # A mutant that does not compile fails every test without any test having
  # looked at it. That is not a kill.
  if ! go build ./internal/... >/dev/null 2>&1; then
    echo "  DOES NOT COMPILE: the mutation is malformed; fix this script" >&2
    survivors=$((survivors + 1))
  elif "${TESTS[@]}" >/dev/null 2>&1; then
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
TESTS=(go test -count=1 -timeout 180s -run 'TestIndexScanResults|TestLeftJoinPlans|TestPlansAgree|TestJoins|TestSubquer|TestIndexScanRespects|TestOuterJoins' ./internal/engine)
baseline

mutant "a WHERE condition on the nullable side of a LEFT JOIN is pushed below the join" "$PLAN" \
  's|^\t\tif u.kind != joinFull \&\& !c.opaque \&\& inside(c.cols, loff, lend) {$|\t\tif u.kind != joinFull \&\& !c.opaque {|'

mutant "a LEFT JOIN drops the rows that have no match" "$JOIN" \
  's|if !matched \&\& it.j.kind != joinInner {$|if !matched \&\& false {|'

mutant "a condition with a subquery is applied before all tables are joined" "$PLAN" \
  's|^\t\tif c.opaque {$|\t\tif false {|'

mutant "an index scan returns versions the snapshot cannot see" "$STORE" \
  's|^\t\tif sn != nil \&\& !sn.visible(xmin, binary.BigEndian.Uint64(val)) {$|\t\tif sn != nil \&\& !sn.visible(xmin, binary.BigEndian.Uint64(val)) \&\& false {|'

mutant "an inclusive upper bound of an index range is treated as exclusive" "$STORE" \
  's|^\t\t\t\tif !kb.hiIncl {$|\t\t\t\tif true {|'

mutant "a join condition is not checked when the inner table is reached through an index" "$JOIN" \
  's|^\t\t\tok, err = allMatch(it.j.conds, it.en)$|\t\t\tok = true|'

echo
echo "== executor =="
# Hash joins, merge joins, sorts on disk, set operations and cursors: each
# has a way of being almost right.
TESTS=(go test -count=1 -timeout 180s -run 'TestSetOperations|TestOuterJoins|TestJoinKeys|TestCursor|TestIndexCursor|TestSpilling|TestTopN|TestExternalSort|TestPlansAgree' ./internal/engine)
baseline

mutant "a scan that resumes returns again the row it stopped at" "$STORE" \
  's|^\t\tstart = append(append(\[\]byte(nil), resume...), 0)$|\t\tstart = append([]byte(nil), resume...)|'

mutant "a cursor reads through the snapshot of the session's latest statement, not its own" "$PLAN" \
  's|sc.db.scanBatch(sc.t, sc.ix, sc.cx.q.snap,|sc.db.scanBatch(sc.t, sc.ix, sc.cx.q.sess.snap,|'

mutant "a cursor's snapshot is released as soon as the query has started" "$EXEC" \
  's|^\treturn \&result{it: it, live: live, release: done}, nil$|\trelease()\n\treturn \&result{it: it, live: live, release: cx.q.cleanup}, nil|'

mutant "a hash join compares an integer with a double precision by their encodings" "$JOIN" \
  's|^\t\tif i, ok := v.(int64); ok \&\& k.float {$|\t\tif i, ok := v.(int64); ok \&\& false {|'

mutant "a hash join takes two NULL keys for equal" "$JOIN" \
  's|^\t\tif err != nil \|\| v == nil {$|\t\tif err != nil {|'

mutant "a hash FULL JOIN forgets the rows of its hashed side that matched nothing" "$JOIN" \
  's|^\t\t\t\th.tail = 0$|\t\t\t\th.tail = -1|'

mutant "a nested-loop FULL JOIN forgets the inner rows that matched nothing" "$JOIN" \
  's|^\t\t\tif !it.innerMatched\[it.tailIdx-1\] {$|\t\t\tif false {|'

mutant "an outer hash join on disk drops the rows whose key is NULL" "$JOIN" \
  's|^\t\tif !keepNull {$|\t\tif true {|'

mutant "a hash join that splits a partition again sends the two sides to different places" "$JOIN" \
  's|h.route(build, r, h.buildKeys, pp.depth, h.j.kind == joinFull)|h.route(build, r, h.buildKeys, pp.depth+1, h.j.kind == joinFull)|'

mutant "a merge join matches only the first of several outer rows with the same key" "$JOIN" \
  's|^\t\tif m.group != nil \&\& m.same(key, m.groupKey) == 0 {$|\t\tif false {|'

mutant "an external sort is not stable: equal rows from later runs come first" "$ITER" \
  's|^\treturn h.e\[i\].run < h.e\[j\].run$|\treturn h.e[i].run > h.e[j].run|'

mutant "ORDER BY with LIMIT keeps the last rows instead of the first" "$ITER" \
  's|} else if so.top.less(e, so.top.e\[0\]) {|} else if so.top.less(so.top.e[0], e) {|'

mutant "RIGHT JOIN keeps the rows of the left side" "$PLAN" \
  's|^\t\t\t\tu.left, u.right = right, left$|\t\t\t\tu.left, u.right = left, right|'

mutant "JOIN ... USING in a right join shows the left side's column" "$PLAN" \
  's|^\t\t\tshown, hidden = r, l$|\t\t\tshown, hidden = l, r|'

mutant "UNION does not remove duplicates" "$SELECT" \
  's|distinct: n.Op == "union" \&\& !n.All, width: width,|distinct: false, width: width,|'

mutant "INTERSECT ALL returns a row as often as the left side has it" "$SELECT" \
  's|^\t\t\ts.counts\[string(s.keyBuf)\] = n - 1$|\t\t\ts.counts[string(s.keyBuf)] = n|'

mutant "the rows of the right side of INTERSECT and EXCEPT are not recorded" "$SELECT" \
  's|^\t\ts.counts\[string(s.keyBuf)\] = 1$|\t\ts.mem++|'

echo
echo "== order and overflow =="
# Rows taken as sorted that are not, and rows set aside that are never
# picked up again.
TESTS=(go test -count=1 -timeout 180s -run 'TestOrderFromTheScan|TestHashTablesSpill|TestSetOperations|TestOuterJoins' ./internal/engine)
baseline

mutant "an index is taken to be in ORDER BY order for a column that may be NULL" "$PLAN" \
  's|^\treturn ok \&\& (nullsFirst \|\| it.table.cols\[tcol\].notNull)$|\treturn ok \&\& (nullsFirst \|\| true \|\| it.table.cols[tcol].notNull)|'

mutant "ORDER BY ... DESC is answered by a scan in ascending order" "$SELECT" \
  's|if col < 0 \|\| o.Desc \|\| !from.sortable(|if col < 0 \|\| !from.sortable(|'

mutant "a FULL JOIN is taken to keep the order of its outer side" "$PLAN" \
  's|^\tif spec.kind != joinFull {$|\tif spec.kind != joinFull \|\| true {|'

mutant "an index scan is taken to be sorted by a column an equality did not fix" "$PLAN" \
  's|^\t\tfor _, col := range cand.cols\[len(a.eq):\] {$|\t\tfor _, col := range cand.cols[min(len(a.eq)+1, len(cand.cols)):] {|'

mutant "the rows of groups that did not fit in memory are dropped" "$SELECT" \
  's|if err := over.add(keyBuf, row); err != nil {|if err := error(nil); err != nil {|'

mutant "DISTINCT never returns to the rows it set aside" "$SELECT" \
  's|^\t\t\t\td.pending = append(d.pending, spilled{f, d.depth + 1})$|\t\t\t\tf.remove()|'

mutant "the two sides of a set operation on disk are split by different hashes" "$SELECT" \
  's|s.rover, err = newOverflow(s.cx.q, s.depth, larger)|s.rover, err = newOverflow(s.cx.q, s.depth+1, larger)|'

mutant "a left row whose equals on the right were set aside is decided without them" "$SELECT" \
  's|^\t\t\tif s.rover != nil {$|\t\t\tif s.rover != nil \&\& false {|'

echo
if [ "$survivors" -gt 0 ]; then
  echo "$survivors mutant(s) survived"
  exit 1
fi
echo "every mutant was killed"

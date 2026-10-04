#!/usr/bin/env bash
# Tests the crash tests.
#
# A crash test that passes proves little unless it would have failed had the
# code been wrong. This script breaks the durability machinery in one
# specific way at a time — skip an fsync, skip the undo pass, and so on —
# and checks that the crash tests notice every time. A mutant that survives
# means the tests have a blind spot.
#
# The source is restored after each mutant, and on exit whatever happens.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

cd "$ROOT"
PAGER=internal/storage/pager.go
RECOVERY=internal/storage/recovery.go
BACKUP="$CACHE/mutation"
mkdir -p "$BACKUP"
cp "$PAGER" "$BACKUP/pager.go"
cp "$RECOVERY" "$BACKUP/recovery.go"
restore() {
  cp "$BACKUP/pager.go" "$PAGER"
  cp "$BACKUP/recovery.go" "$RECOVERY"
}
trap restore EXIT

TESTS=(go test -count=1 -run 'TestCrashRecovery|TestCommitted|TestUncommitted|TestTorn|TestLogIs' ./internal/engine ./internal/storage)

echo "baseline: the unmodified code must pass"
if ! "${TESTS[@]}" >/dev/null 2>&1; then
  echo "  FAILED: fix the tests before mutating" >&2
  exit 1
fi
echo "  passes"

survivors=0
# mutant DESCRIPTION FILE SED-EXPRESSION
mutant() {
  local description="$1" file="$2" expression="$3"
  sed -i "$expression" "$file"
  if cmp -s "$file" "$BACKUP/$(basename "$file")"; then
    echo "mutant: $description"
    echo "  NOT APPLIED: the code it targets has changed; update this script" >&2
    survivors=$((survivors + 1))
    return
  fi
  echo "mutant: $description"
  if "${TESTS[@]}" >/dev/null 2>&1; then
    echo "  SURVIVED: the tests did not notice"
    survivors=$((survivors + 1))
  else
    echo "  killed"
  fi
  restore
}

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
if [ "$survivors" -gt 0 ]; then
  echo "$survivors mutant(s) survived"
  exit 1
fi
echo "every mutant was killed"

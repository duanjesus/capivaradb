#!/usr/bin/env bash
# Runs a slice of SQLite's sqllogictest corpus against the engine and
# compares the pass counts with compat/slt/baseline.txt.
#
#   bash scripts/slt.sh             run, fail if any script got worse
#   bash scripts/slt.sh --update    run, accept the new counts and rewrite
#                                   docs/sqllogictest.md
#   bash scripts/slt.sh -v 20       also print the first 20 failing records
#
# The scripts are not vendored (they are large); they are downloaded once
# from a pinned commit of a GitHub mirror of the SQLite repository.
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

SLT_REPO="gregrahn/sqllogictest"
SLT_COMMIT="c67f97bf3ca7e590d12e073408bcacaf2ff0f3a0"
SLT_DIR="$CACHE/slt/$SLT_COMMIT"

# One script of each family is enough to measure; the families have
# hundreds of siblings generated from the same template.
SCRIPTS=(
  select1.test
  select2.test
  select3.test
  evidence/in1.test
  evidence/in2.test
  evidence/slt_lang_aggfunc.test
  evidence/slt_lang_update.test
  evidence/slt_lang_droptable.test
  evidence/slt_lang_dropindex.test
  index/between/1/slt_good_0.test
  index/commute/10/slt_good_0.test
  index/in/10/slt_good_0.test
  index/orderby/10/slt_good_0.test
  index/orderby_nosort/10/slt_good_0.test
  index/delete/1/slt_good_0.test
  random/select/slt_good_0.test
  random/expr/slt_good_0.test
  random/aggregates/slt_good_0.test
  random/groupby/slt_good_0.test
)

paths=()
for script in "${SCRIPTS[@]}"; do
  dest="$SLT_DIR/$script"
  if [ ! -f "$dest" ]; then
    mkdir -p "$(dirname "$dest")"
    echo "downloading $script" >&2
    curl -fsSL -o "$dest.part" "https://raw.githubusercontent.com/$SLT_REPO/$SLT_COMMIT/test/$script"
    mv "$dest.part" "$dest"
  fi
  paths+=("$dest")
done

cd "$ROOT"
args=(-root "$SLT_DIR" -baseline compat/slt/baseline.txt)
if [ "${1:-}" = "--update" ]; then
  shift
  args+=(-update -markdown "$CACHE/slt-report.md")
fi
go run ./tools/slt "${args[@]}" "$@" "${paths[@]}"

if [ -f "$CACHE/slt-report.md" ]; then
  {
    cat compat/slt/header.md
    cat "$CACHE/slt-report.md"
    cat compat/slt/footer.md
  } >docs/sqllogictest.md
  rm "$CACHE/slt-report.md"
  echo "wrote docs/sqllogictest.md"
fi

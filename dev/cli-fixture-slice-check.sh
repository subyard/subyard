#!/usr/bin/env bash
# Prove that a CLI test fixture migration slice keeps its coverage, against a base ref:
#   1. the go test -list names and the -json pass sets are equal, both sides are green,
#      and allowed added tests pass;
#   2. every base assertion survives unchanged, renamed by a reviewed rule, as an error
#      check replaced by its named helper, or under a reviewed waiver bound to its replacement;
#   3. the coverprofile blocks of non-test code covered on the base stay covered;
#   4. every listed mutant of a changed test file fails tests that pass without it.
# Both sides run from exports in /tmp without Git metadata: the base from git archive,
# the candidate from the current tracked and untracked, non-ignored files.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export TMPDIR=/tmp

usage() {
  cat <<'EOF'
Usage: dev/cli-fixture-slice-check.sh [options]
  --base REF                 base ref (default: origin/master)
  --package PKG              Go package to check (default: ./internal/cli)
  --mutants FILE             mutant list (default: dev/cli-fixture-mutants.tsv)
  --renames FILE             reviewed assertion rename rules (default: dev/cli-fixture-renames.tsv)
  --waivers FILE             reviewed assertion waivers (default: dev/cli-fixture-waivers.tsv)
  --all-mutants              run every listed mutant, not only those of changed test files
  --skip-mutants             skip proof 4
  --allow-added-test REGEX   accept added top-level tests matching REGEX (repeatable)
  --allow-error-check CALLEE=TEXT
                             accept a removed `if err != nil { t.Fatal(err) }` check of CALLEE
                             when its function adds one more occurrence of TEXT
                             (repeatable; default New=fixture.New()
  --without-coverage TEST    run top-level TEST without coverage on both sides (repeatable;
                             default: tests that re-execute the instrumented test binary)
  --keep                     keep the work directory
Source admission tests need a normal runtime: SUBYARD_SOURCE_QUERY_TOOLING_ROOT, or
.build/runtime/current from `make build VERSION=0.29.0-test`.

Mutant list: tab-separated test file, -run regex, product file, old text, new text; old
must occur exactly once. In the text fields \n, \t and \\ are escapes.
Rename rules: tab-separated Python regex and replacement, applied in order to a base
assertion unit that has no unchanged match.
Waivers: tab-separated base function, removed assertion unit as proof 2 prints it,
candidate function, replacement text that function must add, reason.
In all three lists # starts a comment line.
EOF
}

base=origin/master package=./internal/cli
mutants="$ROOT/dev/cli-fixture-mutants.tsv" renames="$ROOT/dev/cli-fixture-renames.tsv"
waivers="$ROOT/dev/cli-fixture-waivers.tsv"
all_mutants=0 skip_mutants=0 keep=0
allowed_tests=() replacements=("New=fixture.New(")
# This test's owner-protocol subtest re-executes the test binary and parses its
# output, which an instrumented binary extends with a coverage warning.
without_coverage=(TestResumedMigrateRunsPendingInitAfterRepair)
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base) base="${2:?}"; shift 2 ;;
    --package) package="${2:?}"; shift 2 ;;
    --mutants) mutants="${2:?}"; shift 2 ;;
    --renames) renames="${2:?}"; shift 2 ;;
    --waivers) waivers="${2:?}"; shift 2 ;;
    --all-mutants) all_mutants=1; shift ;;
    --skip-mutants) skip_mutants=1; shift ;;
    --allow-added-test) allowed_tests+=("${2:?}"); shift 2 ;;
    --allow-error-check)
      case "${2:?}" in *=?*) ;; *) printf 'FAIL: --allow-error-check takes CALLEE=TEXT\n' >&2; exit 2 ;; esac
      replacements+=("$2"); shift 2 ;;
    --without-coverage) without_coverage+=("${2:?}"); shift 2 ;;
    --keep) keep=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
case "$package" in ./*) ;; *) printf 'FAIL: --package must be a ./relative package\n' >&2; exit 2 ;; esac
for list in "$mutants" "$renames" "$waivers"; do
  [ -f "$list" ] || { printf 'FAIL: missing list %s\n' "$list" >&2; exit 2; }
done

tooling="${SUBYARD_SOURCE_QUERY_TOOLING_ROOT:-$ROOT/.build/runtime/current}"
[ -e "$tooling/bin/yard-engine" ] \
  || { printf 'FAIL: no normal runtime at %s; run make build VERSION=0.29.0-test\n' "$tooling" >&2; exit 2; }
export SUBYARD_SOURCE_QUERY_TOOLING_ROOT
SUBYARD_SOURCE_QUERY_TOOLING_ROOT="$(cd "$tooling" && pwd -P)"
base_commit="$(git -C "$ROOT" rev-parse --verify "$base^{commit}")"

work="$(mktemp -d /tmp/cli-fixture-slice.XXXXXX)"
cleanup() {
  if [ "$keep" = 1 ]; then
    printf 'work directory kept: %s\n' "$work"
  else
    chmod -R u+rwX -- "$work" 2>/dev/null || true
    rm -rf -- "$work"
  fi
}
trap cleanup EXIT

mkdir "$work/base" "$work/candidate"
git -C "$ROOT" archive --format=tar "$base_commit" | tar -xf - -C "$work/base"
git -C "$ROOT" ls-files -z --cached --others --exclude-standard \
  | ROOT="$ROOT" python3 -I -c '
import os, sys
root = os.environ["ROOT"]
names = [n for n in sys.stdin.buffer.read().split(b"\0") if n and os.path.lexists(os.path.join(root.encode(), n))]
sys.stdout.buffer.write(b"".join(n + b"\0" for n in names))' \
  | tar -C "$ROOT" --null -T - -cf - | tar -xf - -C "$work/candidate"
printf 'base %s (%s); candidate: current worktree of %s\n' "$base" "$base_commit" "$(git -C "$ROOT" rev-parse --short HEAD)"
cp -- "$ROOT/dev/cli-fixture-slice-check.py" "$work/check.py"
check() { python3 -I "$work/check.py" "$@"; }

plain_list="$(IFS=,; printf '%s' "${without_coverage[*]}")"
plain_pattern="^($(IFS='|'; printf '%s' "${without_coverage[*]}"))\$"
status=0
for side in base candidate; do
  go -C "$work/$side" test -list '.*' "$package" > "$work/$side.list"
  printf 'running %s %s with coverage\n' "$side" "$package"
  # Proof 1 reads every outcome, including package failures, from the JSON.
  go -C "$work/$side" test -count=1 -timeout=30m -json -coverpkg=./... -coverprofile="$work/$side.cover" \
    "$package" > "$work/$side.json" || true
  [ -s "$work/$side.cover" ] || { printf 'FAIL: %s produced no coverprofile\n' "$side" >&2; exit 1; }
  : > "$work/$side.plain.json"
  if [ "${#without_coverage[@]}" -gt 0 ]; then
    go -C "$work/$side" test -count=1 -timeout=30m -json -run "$plain_pattern" "$package" \
      > "$work/$side.plain.json" || true
  fi
done

printf '\n== proof 1: test list and pass set\n'
if check tests "$work/base.list" "$work/candidate.list" "$work/base.json" "$work/candidate.json" \
  "$work/base.plain.json" "$work/candidate.plain.json" "$plain_list" "${allowed_tests[@]}"; then
  printf 'PASS\n'
else status=1; printf 'FAIL\n'; fi

printf '\n== proof 2: base assertions survive or are bound to reviewed replacements\n'
if check asserts "$work/base/$package" "$work/candidate/$package" "$renames" "$waivers" "${replacements[@]}"; then
  printf 'PASS\n'
else status=1; printf 'FAIL\n'; fi

printf '\n== proof 3: covered non-test blocks are equal or a superset\n'
if check coverage "$work/base.cover" "$work/candidate.cover"; then
  printf 'PASS\n'
else status=1; printf 'FAIL\n'; fi

printf '\n== proof 4: mutants\n'
if [ "$skip_mutants" = 1 ]; then
  printf 'skipped\n'
else
  # A changed test file exists on both sides with different content; added files
  # have no base behaviour to preserve and are covered by proof 1.
  changed=()
  for file in "$work/candidate/$package"/*_test.go; do
    name="${package#./}/$(basename "$file")"
    if [ -f "$work/base/$name" ] && ! cmp -s "$file" "$work/base/$name"; then
      changed+=("$name")
    fi
  done
  selection=changed
  [ "$all_mutants" = 1 ] && selection=all
  mutant_status=0 count=0
  check mutants "$mutants" "$work/candidate" "$selection" "${changed[@]}" > "$work/mutants.rows" || mutant_status=1
  declare -A pristine=()
  while IFS= read -r -d '' line && IFS= read -r -d '' test_file && IFS= read -r -d '' pattern \
    && IFS= read -r -d '' product && IFS= read -r -d '' old && IFS= read -r -d '' new; do
    count=$((count + 1))
    if [ -z "${pristine[$pattern]+set}" ]; then
      go -C "$work/candidate" test -count=1 -timeout=10m -json -run "$pattern" "$package" > "$work/pristine.json" 2>&1 || true
      pristine[$pattern]="$(check verdict "$work/pristine.json")"
    fi
    target="$work/candidate/$product"
    if [ "${pristine[$pattern]}" != passed ]; then
      result="broken (selected tests ${pristine[$pattern]} without the mutant)"
    else
      cp -p -- "$target" "$work/original"
      if ! check apply "$target" "$old" "$new"; then
        result=invalid
      else
        go -C "$work/candidate" test -count=1 -timeout=10m -json -run "$pattern" "$package" > "$work/mutant.json" 2>&1 || true
        case "$(check verdict "$work/mutant.json")" in
          failed) result=killed ;;
          passed) result=survived ;;
          *) result="$(check verdict "$work/mutant.json")" ;;
        esac
      fi
      cp -p -- "$work/original" "$target"
    fi
    [ "$result" = killed ] || mutant_status=1
    printf '  %-8s %s:%s %s (%s) -run %s\n' "$result" "${mutants#"$ROOT"/}" "$line" "$product" "$test_file" "$pattern"
  done < "$work/mutants.rows"
  printf 'mutants run: %d\n' "$count"
  if [ "$mutant_status" = 0 ] && [ "$count" -gt 0 ]; then
    printf 'PASS\n'
  else status=1; printf 'FAIL\n'; fi
fi
exit "$status"

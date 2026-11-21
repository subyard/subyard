#!/usr/bin/env bash
# Prove that a CLI test fixture migration slice keeps its coverage, against a base ref:
#   1. the go test -list names and the pass sets are equal, every run exits 0, and
#      allowed added tests pass;
#   3. the coverprofile blocks of non-test code covered on the base stay covered in some
#      candidate run that exits 0 (a rerun absorbs blocks behind a lost race, and is reported);
#   4. every listed mutant of a changed test file builds and makes one of its selected
#      tests, which pass without it, report a failure.
# Assertions are reviewed, not proved: the migration rule in docs/testing.md keeps them
# byte-identical. Proof 2 was a heuristic for that and is gone; the numbering is kept.
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
  --all-mutants              run every listed mutant, not only those of changed test files
  --skip-mutants             skip proof 4
  --allow-added-test REGEX   accept added top-level tests matching REGEX (repeatable)
  --without-coverage TEST    skip top-level TEST in the coverage runs and run it alone
                             without coverage on both sides (repeatable; for ./internal/cli
                             the default is the test that re-executes the instrumented binary)
  --coverage-reruns N        rerun the candidate up to N times while proof 3 loses blocks,
                             counting blocks covered in any run (default: 1)
  --keep                     keep the work directory
Source admission tests need a normal runtime: SUBYARD_SOURCE_QUERY_TOOLING_ROOT, or
.build/runtime/current from `make build VERSION=0.29.0-test`.

Mutant list: tab-separated test file, -run regex, mutated file, old text, new text; old
must occur exactly once. The mutated file may be the test file itself, for example the
expected argv of an AssertCalls. In the text fields \n, \t and \\ are escapes; # starts
a comment line.
EOF
}

base=origin/master package=./internal/cli mutants="$ROOT/dev/cli-fixture-mutants.tsv"
all_mutants=0 skip_mutants=0 keep=0 coverage_reruns=1
allowed_tests=() without_coverage=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base) base="${2:?}"; shift 2 ;;
    --package) package="${2:?}"; shift 2 ;;
    --mutants) mutants="${2:?}"; shift 2 ;;
    --all-mutants) all_mutants=1; shift ;;
    --skip-mutants) skip_mutants=1; shift ;;
    --allow-added-test) allowed_tests+=("${2:?}"); shift 2 ;;
    --without-coverage) without_coverage+=("${2:?}"); shift 2 ;;
    --coverage-reruns)
      case "${2:?}" in *[!0-9]*) printf 'FAIL: --coverage-reruns takes a count\n' >&2; exit 2 ;; esac
      coverage_reruns="$2"; shift 2 ;;
    --keep) keep=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
case "$package" in ./*) ;; *) printf 'FAIL: --package must be a ./relative package\n' >&2; exit 2 ;; esac
if [ "$package" = ./internal/cli ] && [ "${#without_coverage[@]}" -eq 0 ]; then
  # Its owner-protocol subtest re-executes the test binary and parses its output,
  # which an instrumented binary extends with a coverage warning.
  without_coverage=(TestResumedMigrateRunsPendingInitAfterRepair)
fi
[ -f "$mutants" ] || { printf 'FAIL: missing mutant list %s\n' "$mutants" >&2; exit 2; }

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

# run NAME SIDE ARGS...: one go test run whose exit status must be 0; JSON goes to $work/NAME.json.
failed_runs=()
run() {
  local name="$1" side="$2" code=0
  shift 2
  go -C "$work/$side" test -count=1 -timeout=30m -json "$@" "$package" > "$work/$name.json" 2>&1 || code=$?
  if [ "$code" != 0 ]; then
    failed_runs+=("$name")
    printf '  FAIL %s run exited %d\n' "$name" "$code"
    tail -n 20 -- "$work/$name.json" | sed 's/^/    /'
  fi
  return "$code"
}

skip=()
plain_pattern=
if [ "${#without_coverage[@]}" -gt 0 ]; then
  plain_pattern="^($(IFS='|'; printf '%s' "${without_coverage[*]}"))\$"
  skip=(-skip "$plain_pattern")
fi
status=0
for side in base candidate; do
  go -C "$work/$side" test -list '.*' "$package" > "$work/$side.list"
  printf 'running %s %s with coverage\n' "$side" "$package"
  run "$side" "$side" "${skip[@]}" -coverpkg=./... -coverprofile="$work/$side.cover" || true
  if [ -n "$plain_pattern" ]; then
    printf 'running %s %s without coverage: %s\n' "$side" "$package" "$plain_pattern"
    run "$side.plain" "$side" -run "$plain_pattern" || true
  fi
done
runs_of() {
  local side="$1"
  if [ -n "$plain_pattern" ]; then printf '%s,%s' "$work/$side.json" "$work/$side.plain.json"
  else printf '%s' "$work/$side.json"; fi
}

printf '\n== proof 1: test list and pass set\n'
if [ "${#failed_runs[@]}" -eq 0 ] && check tests "$work/base.list" "$work/candidate.list" \
  "$(runs_of base)" "$(runs_of candidate)" "${allowed_tests[@]}"; then
  printf 'PASS\n'
else
  [ "${#failed_runs[@]}" -eq 0 ] || printf '  FAIL runs that did not exit 0: %s\n' "${failed_runs[*]}"
  status=1; printf 'FAIL\n'
fi

printf '\n== proof 3: covered non-test blocks are equal or a superset\n'
coverage_ok=0
if [ "${#failed_runs[@]}" -ne 0 ]; then
  printf '  FAIL coverage needs base and candidate runs that exited 0\n'
else
  profiles=("$work/candidate.cover")
  for rerun in $(seq 0 "$coverage_reruns"); do
    if [ "$rerun" -gt 0 ]; then
      printf 'rerunning candidate %s with coverage (%d of %d)\n' "$package" "$rerun" "$coverage_reruns"
      run "candidate.rerun$rerun" candidate "${skip[@]}" -coverpkg=./... \
        -coverprofile="$work/candidate.$rerun.cover" || break
      profiles+=("$work/candidate.$rerun.cover")
    fi
    if check coverage "$work/base.cover" "${profiles[@]}" > "$work/coverage.out"; then
      coverage_ok=1
      break
    fi
  done
  cat "$work/coverage.out"
fi
if [ "$coverage_ok" = 1 ]; then
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
    && IFS= read -r -d '' target_file && IFS= read -r -d '' old && IFS= read -r -d '' new; do
    count=$((count + 1))
    if [ -z "${pristine[$pattern]+set}" ]; then
      # The control must exit 0 and pass at least one selected test.
      if go -C "$work/candidate" test -count=1 -timeout=10m -json -run "$pattern" "$package" \
        > "$work/pristine.json" 2>&1 && [ "$(check passed "$work/pristine.json")" -gt 0 ]; then
        pristine[$pattern]=passed
      else
        pristine[$pattern]=failed
      fi
    fi
    target="$work/candidate/$target_file"
    if [ "${pristine[$pattern]}" != passed ]; then
      result="broken (selected tests do not pass without the mutant)"
    else
      cp -p -- "$target" "$work/original"
      if ! check apply "$target" "$old" "$new"; then
        result=invalid
      elif ! go -C "$work/candidate" test -c -o "$work/mutant.test" "$package" > "$work/mutant.build" 2>&1; then
        result="invalid (does not build)"
      elif go -C "$work/candidate" test -count=1 -timeout=10m -json -run "$pattern" "$package" > "$work/mutant.json" 2>&1; then
        result=survived
      elif check failed "$work/mutant.json" "$pattern" > /dev/null; then
        result=killed
      else
        result="invalid (exited non-zero without a selected test failing)"
      fi
      cp -p -- "$work/original" "$target"
    fi
    [ "$result" = killed ] || mutant_status=1
    printf '  %-8s %s:%s %s (%s) -run %s\n' "$result" "${mutants#"$ROOT"/}" "$line" "$target_file" "$test_file" "$pattern"
  done < "$work/mutants.rows"
  printf 'mutants run: %d\n' "$count"
  if [ "$mutant_status" = 0 ] && [ "$count" -gt 0 ]; then
    printf 'PASS\n'
  else status=1; printf 'FAIL\n'; fi
fi
exit "$status"

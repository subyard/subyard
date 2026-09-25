#!/usr/bin/env bash
# One host-free entrypoint used locally and in CI.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$ROOT/.build/test-runs"
RUN_DIR="$(mktemp -d "$ROOT/.build/test-runs/run.XXXXXX")"
SUMMARY="$RUN_DIR/summary.tsv"
printf 'kind\tsuite\tcheck\tstatus\texit_code\tduration_seconds\tlog\n' > "$SUMMARY"
printf 'RESULTS %s\n' "$SUMMARY"
RUN_STARTED=$SECONDS
CHECK_NAME=''
CHECK_COUNT=0
CURRENT_SUITE=preflight

record_check() {
  local rc="$1" status=passed duration=$((SECONDS - CHECK_STARTED))
  [ "$rc" -eq 0 ] || status=failed
  printf 'check\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$CURRENT_SUITE" "$CHECK_NAME" "$status" "$rc" "$duration" "$CHECK_LOG" >> "$SUMMARY"
  printf '%s %s duration_seconds=%s exit_code=%s\n' \
    "$status" "$CHECK_NAME" "$duration" "$rc"
  if [ "$rc" -ne 0 ]; then
    printf 'LOG %s/%s (last 40 lines)\n' "$RUN_DIR" "$CHECK_LOG" >&2
    tail -n 40 "$RUN_DIR/$CHECK_LOG" >&2
  fi
  CHECK_NAME=''
}

finish() {
  local rc="$1" status=passed
  trap - EXIT
  set +e
  [ -z "$CHECK_NAME" ] || record_check "$rc"
  [ "$rc" -eq 0 ] || status=failed
  printf 'run\tall\tall\t%s\t%s\t%s\t-\n' \
    "$status" "$rc" "$((SECONDS - RUN_STARTED))" >> "$SUMMARY"
  printf 'SUMMARY status=%s checks=%s duration_seconds=%s exit_code=%s results=%s\n' \
    "$status" "$CHECK_COUNT" "$((SECONDS - RUN_STARTED))" "$rc" "$SUMMARY"
  exit "$rc"
}
trap 'finish "$?"' EXIT

run_check() {
  CHECK_NAME="$1"; shift
  CHECK_COUNT=$((CHECK_COUNT + 1))
  CHECK_STARTED=$SECONDS
  printf -v CHECK_LOG '%03d.log' "$CHECK_COUNT"
  printf 'RUN %s\n' "$CHECK_NAME"
  # Do not put this command in an if/||: that disables errexit inside functions.
  (trap - EXIT; "$@") > "$RUN_DIR/$CHECK_LOG" 2>&1
  record_check 0
}

check_syntax() {
  mapfile -t syntax_files < <(
    printf '%s\n' "$ROOT/bin/yard"
    find "$ROOT/scripts" "$ROOT/dev" "$ROOT/config/profiles" "$ROOT/config/agents" "$ROOT/tests" \
      -type f -name '*.sh' -print | sort
  )
  for file in "${syntax_files[@]}"; do bash -n "$file"; done
}

check_format() {
  mapfile -t unformatted < <(gofmt -l "$ROOT/cmd" "$ROOT/internal")
  [ "${#unformatted[@]}" -eq 0 ] \
    || { printf 'FAIL: gofmt required: %s\n' "${unformatted[*]}" >&2; exit 1; }
}

check_build_layout() {
  [ -x "$ROOT/.build/yard" ] \
    || { printf 'FAIL: development engine was not built\n' >&2; exit 1; }
  [ ! -e "$ROOT/bin/yard-engine" ] \
    || { printf 'FAIL: tracked/source-tree bootstrap engine must not return\n' >&2; exit 1; }
}

check_manifests() {
  mapfile -t actual_tests < <(find "$ROOT/tests" -maxdepth 1 -type f -name '*.sh' ! -name run.sh -printf '%f\n' | sort)
  mapfile -t declared_tests < <(sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' \
    "$ROOT"/tests/suites/{unit,contract,integration}.list | sort)
  [ "${#actual_tests[@]}" -eq "${#declared_tests[@]}" ] \
    || { printf 'FAIL: test suite manifests omit or duplicate a top-level test\n' >&2; exit 1; }
  for i in "${!actual_tests[@]}"; do
    [ "${actual_tests[$i]}" = "${declared_tests[$i]}" ] \
      || { printf 'FAIL: test suite manifests drifted near %s / %s\n' \
        "${actual_tests[$i]}" "${declared_tests[$i]}" >&2; exit 1; }
  done
}

check_go_race() {
  local mask
  for mask in 0002 0022 0077; do
    printf 'Go race tests: umask=%s\n' "$mask"
    (umask "$mask"; go -C "$ROOT" test -race -count=1 ./...)
  done
}

run_check syntax check_syntax
run_check go-toolchain command -v go
CURRENT_SUITE=go
printf 'SUITE go\n'
run_check gofmt check_format
run_check go-vet go -C "$ROOT" vet ./...
run_check go-race check_go_race
run_check go-fuzz go -C "$ROOT" test ./internal/command -run '^$' -fuzz '^FuzzParseDoesNotPanic$' -fuzztime=1000x
run_check build "$ROOT/dev/build-engine.sh"
run_check build-layout check_build_layout
run_check manifests check_manifests

for suite in unit contract integration; do
  CURRENT_SUITE="$suite"
  printf 'SUITE %s\n' "$suite"
  while IFS= read -r test_name; do
    case "$test_name" in '' | '# '*) continue ;; esac
    run_check "tests/$test_name" bash "$ROOT/tests/$test_name"
  done < "$ROOT/tests/suites/$suite.list"
done

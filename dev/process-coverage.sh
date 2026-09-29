#!/usr/bin/env bash
# Merge package coverage with commands exercised through the real yard process boundary.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUTPUT="$ROOT/.build/coverage"
TEMP=''
DEV_ENGINE_CREATED=0

die() { printf 'process-coverage: %s\n' "$*" >&2; exit 2; }
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [ "$DEV_ENGINE_CREATED" = 1 ] && [ -f "$ROOT/.build/yard" ]; then
    ! cmp -s "$ROOT/.build/yard" "$TEMP/yard" || find "$ROOT/.build/yard" -delete
  fi
  if [ -n "$TEMP" ]; then
    case "$TEMP" in "$ROOT"/.build/.process-coverage.*) find "$TEMP" -depth -delete ;; esac
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) [ "$#" -ge 2 ] || die '--output needs a directory'; OUTPUT="$2"; shift 2 ;;
    -h | --help)
      printf 'Usage: dev/process-coverage.sh [--output DIRECTORY]\n'
      exit 0
      ;;
    *) die "unknown argument '$1'" ;;
  esac
done

command -v go >/dev/null 2>&1 || die 'Go is required'
command -v jq >/dev/null 2>&1 || die 'jq is required'
mkdir -p "$ROOT/.build"
TEMP="$(mktemp -d "$ROOT/.build/.process-coverage.XXXXXX")"
package_cov="$TEMP/package"
process_cov="$TEMP/process"
merged_cov="$TEMP/merged"
shell_log="$TEMP/shell.log"
shell_hook="$TEMP/bash-env"
bundle="$TEMP/worktree.tar.gz"
mkdir -p "$package_cov" "$process_cov" "$merged_cov"

# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
build_bundle "$ROOT" "$bundle"
bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"

cat > "$shell_hook" <<'EOF'
case "$0" in
  "$SUBYARD_COVERAGE_ROOT"/config/profiles/*/tests/*) ;;
  "$SUBYARD_COVERAGE_ROOT"/scripts/*|"$SUBYARD_COVERAGE_ROOT"/config/profiles/*|"$SUBYARD_COVERAGE_ROOT"/config/agents/*)
    printf '%s\n' "${0#"$SUBYARD_COVERAGE_ROOT"/}" >> "$SUBYARD_SHELL_COVERAGE_LOG"
    ;;
esac
EOF
chmod 0600 "$shell_hook"

printf '  [ .. ] package coverage bundle=%s\n' "$bundle_hash"
(
  cd "$ROOT"
  go test -cover -covermode=atomic -coverpkg=./... ./... \
    -args -test.gocoverdir="$package_cov"
  go build -cover -covermode=atomic -coverpkg=./... -buildvcs=false -trimpath \
    -o "$TEMP/yard" ./cmd/yard
)
if [ ! -e "$ROOT/.build/yard" ]; then
  install -m 0755 "$TEMP/yard" "$ROOT/.build/yard"
  DEV_ENGINE_CREATED=1
fi

export BASH_ENV="$shell_hook"
export GOCOVERDIR="$process_cov"
export SUBYARD_COVERAGE_ROOT="$ROOT"
export SUBYARD_SHELL_COVERAGE_LOG="$shell_log"
while IFS= read -r test_name; do
  case "$test_name" in ''|'# '*) continue ;; esac
  printf '  [ .. ] process contract tests/%s\n' "$test_name"
  YARD_ENGINE_PATH="$TEMP/yard" bash "$ROOT/tests/$test_name"
done < "$ROOT/tests/suites/process.list"
for process_list in "$ROOT"/config/profiles/*/tests/process.list; do
  [ -f "$process_list" ] || continue
  profile_tests="${process_list%/process.list}"
  while IFS= read -r test_name; do
    case "$test_name" in ''|'# '*) continue ;; esac
    [[ "$test_name" != /* && "$test_name" != *'..'* && "$test_name" != */* ]] \
      || die "invalid profile process test path in ${process_list#"$ROOT/"}"
    test_path="$profile_tests/$test_name"
    [ -f "$test_path" ] || die "missing profile process test ${test_path#"$ROOT/"}"
    printf '  [ .. ] process contract %s/%s\n' "${profile_tests#"$ROOT/"}" "$test_name"
    YARD_ENGINE_PATH="$TEMP/yard" bash "$test_path"
  done < "$process_list"
done
unset BASH_ENV GOCOVERDIR SUBYARD_COVERAGE_ROOT SUBYARD_SHELL_COVERAGE_LOG

go tool covdata merge -i="$package_cov,$process_cov" -o "$merged_cov"
mkdir -p "$OUTPUT"
go tool covdata textfmt -i="$merged_cov" -o "$OUTPUT/merged.cover"
go tool covdata percent -i="$merged_cov" | tee "$OUTPUT/percent.txt"
sort -u "$shell_log" > "$OUTPUT/shell-inventory.txt"
jq -n --arg bundle_hash "$bundle_hash" \
  --argjson shell_count "$(wc -l < "$OUTPUT/shell-inventory.txt")" '
  {
    schema_version: 1,
    bundle_hash: $bundle_hash,
    package_and_process_coverage: "merged.cover",
    shell_inventory: "shell-inventory.txt",
    executed_shell_contracts: $shell_count
  }
' > "$OUTPUT/evidence.json"
printf 'ok: merged package/process coverage and %s executed Shell contracts (bundle=%s)\n' \
  "$(wc -l < "$OUTPUT/shell-inventory.txt")" "$bundle_hash"

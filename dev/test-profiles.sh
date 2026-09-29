#!/usr/bin/env bash
# Run tests owned by each profile, separately from the core suite.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
suite=run.sh
if [ "${1:-}" = --e2e ]; then
  suite=e2e/acceptance.sh
  shift
fi
count=0
runners=()
for profile in "$ROOT"/config/profiles/*; do
  [ -d "$profile" ] || continue
  count=$((count + 1))
  runner="$profile/tests/$suite"
  exemption="${runner%.sh}.not-applicable"
  if [ -L "$profile" ] || [ -L "$profile/tests" ] || [ -L "$(dirname "$runner")" ]; then
    printf 'PROFILE TESTS %s incomplete: test directory must not be a symlink\n' "${profile##*/}" >&2
    exit 1
  fi
  if [ -f "$exemption" ] && [ ! -L "$exemption" ] && grep -q '[^[:space:]]' "$exemption" && [ ! -e "$runner" ] && [ ! -L "$runner" ]; then
    printf 'PROFILE TESTS %s not-applicable: ' "${profile##*/}"
    cat "$exemption"
    continue
  fi
  if [ ! -f "$runner" ] || [ -L "$runner" ] || [ -e "$exemption" ] || [ -L "$exemption" ]; then
    printf 'PROFILE TESTS %s incomplete: required runner missing, unsafe, or conflicting exemption: %s\n' \
      "${profile##*/}" "${runner#"$ROOT/"}" >&2
    exit 1
  fi
  runners+=("$runner")
done
[ "$count" -gt 0 ] || { printf 'PROFILE TESTS incomplete: no shipped profiles found\n' >&2; exit 1; }
# Check the complete inventory before a live runner can acquire a lease.
for runner in "${runners[@]}"; do
  printf 'PROFILE TESTS %s\n' "${runner#"$ROOT/"}"
  bash "$runner" "$@"
done

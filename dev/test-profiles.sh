#!/usr/bin/env bash
# Run tests owned by each profile, separately from the core suite.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
suite=run.sh
list=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --e2e) suite=e2e/acceptance.sh; shift ;;
    --list) list=1; shift ;;
    --) shift; break ;;
    *) break ;;
  esac
done
if [ "$list" = 1 ] && [ "$#" -gt 0 ]; then
  printf 'PROFILE TESTS --list does not accept runner arguments\n' >&2
  exit 2
fi
count=0
runners=()
records=()
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
    records+=("not-applicable"$'\t'"${profile##*/}"$'\t'"${exemption#"$ROOT/"}")
    continue
  fi
  if [ ! -f "$runner" ] || [ -L "$runner" ] || [ -e "$exemption" ] || [ -L "$exemption" ]; then
    printf 'PROFILE TESTS %s incomplete: required runner missing, unsafe, or conflicting exemption: %s\n' \
      "${profile##*/}" "${runner#"$ROOT/"}" >&2
    exit 1
  fi
  records+=("runner"$'\t'"${profile##*/}"$'\t'"${runner#"$ROOT/"}")
  runners+=("$runner")
done
[ "$count" -gt 0 ] || { printf 'PROFILE TESTS incomplete: no shipped profiles found\n' >&2; exit 1; }
if [ "$list" = 1 ]; then
  printf '%s\n' "${records[@]}"
  exit 0
fi
# Check the complete inventory before a live runner can acquire a lease.
for record in "${records[@]}"; do
  IFS=$'\t' read -r kind profile path <<<"$record"
  if [ "$kind" = not-applicable ]; then
    reason="$(tr '\n' ' ' < "$ROOT/$path")"
    printf 'PROFILE TESTS %s not-applicable: %s\n' "$profile" "$reason"
  fi
done
for runner in "${runners[@]}"; do
  printf 'PROFILE TESTS %s\n' "${runner#"$ROOT/"}"
  bash "$runner" "$@"
done

#!/usr/bin/env bash
# Repeat explicit Go file-creation contracts under the other supported umasks.
set -euo pipefail
export TMPDIR=/tmp
ROOT="${1:?repository root required}"; shift
packages=() names=()
for selection in "$@"; do
  packages+=("${selection%%:*}")
  IFS='|' read -r -a selected_names <<< "${selection#*:}"
  names+=("${selected_names[@]}")
done
[ "${#names[@]}" -gt 0 ] || { printf 'FAIL: empty Go permission selection\n' >&2; exit 1; }
for name in "${names[@]}"; do
  [[ "$name" =~ ^Test[A-Za-z0-9_]+$ ]] \
    || { printf 'FAIL: invalid selected Go test: %s\n' "$name" >&2; exit 1; }
done
printf -v pattern '%s|' "${names[@]}"
pattern="^(${pattern%|})$"
# A renamed/deleted test must fail explicitly, instead of an empty -run passing.
if available="$(go -C "$ROOT" test -list "$pattern" "${packages[@]}")"; then
  :
else
  rc=$?
  printf '%s\n' "$available"
  exit "$rc"
fi
for name in "${names[@]}"; do
  grep -Fxq "$name" <<< "$available" \
    || { printf 'FAIL: selected Go permission test missing: %s\n' "$name" >&2; exit 1; }
done
for mask in 0002 0077; do
  printf 'Go permission race tests: umask=%s\n' "$mask"
  (umask "$mask"; go -C "$ROOT" test -race -count=1 -run "$pattern" "${packages[@]}")
done

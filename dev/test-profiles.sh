#!/usr/bin/env bash
# Run tests owned by each profile, separately from the core suite.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
suite=run.sh
if [ "${1:-}" = --e2e ]; then
  suite=e2e/acceptance.sh
  shift
fi
for runner in "$ROOT"/config/profiles/*/tests/"$suite"; do
  [ -f "$runner" ] || continue
  printf 'PROFILE TESTS %s\n' "${runner#"$ROOT/"}"
  bash "$runner" "$@"
done

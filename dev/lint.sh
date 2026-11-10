#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
status=0
for script in dev/lint-workflows.sh dev/lint-python.sh dev/lint-go.sh; do
  if bash "$script"; then
    continue
  else
    code=$?
    printf '%s: failed (exit %s)\n' "$script" "$code" >&2
    if [ "$status" -eq 0 ]; then
      status=$code
    fi
  fi
done
exit "$status"

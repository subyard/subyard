#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
ruff="$(bash dev/pinned-tool.sh ruff)"
files="$(mktemp "$ROOT/.build/tools/ruff-files.XXXXXX")"
trap 'rm -f -- "$files"' EXIT
# Preserve unusual filenames and propagate a failed Git inventory read.
git ls-files -z -- '*.py' > "$files"
mapfile -d '' -t python_files < "$files"
if [ "${#python_files[@]}" -gt 0 ]; then
  # Ignore repository config/ignores and inspect source bytes without executing Python.
  "$ruff" check --isolated --select F --no-respect-gitignore \
    --cache-dir "$ROOT/.build/tools/ruff-cache" -- "${python_files[@]}"
fi

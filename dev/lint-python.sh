#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
ruff="$(bash dev/pinned-tool.sh ruff)"
files="$(mktemp "$ROOT/.build/tools/ruff-files.XXXXXX")"
trap 'rm -f -- "$files"' EXIT
# Preserve unusual filenames and propagate a failed inventory read. The source
# enumerator also covers a release snapshot without Git metadata.
bash tests/helpers/source-files.sh "$ROOT" > "$files"
mapfile -d '' -t sources < "$files"
python_files=()
for path in "${sources[@]}"; do [[ "$path" != *.py ]] || python_files+=("$path"); done
if [ "${#python_files[@]}" -eq 0 ]; then
  printf 'lint-python: no Python sources\n' >&2
  exit 1
else
  # Ignore repository config/ignores and inspect source bytes without executing Python.
  "$ruff" check --isolated --select F --no-respect-gitignore \
    --cache-dir "$ROOT/.build/tools/ruff-cache" -- "${python_files[@]}"
  "$ruff" check --force-exclude --no-cache --no-respect-gitignore \
    --config dev/ruff-safefs.toml -- "${python_files[@]}"
  python3 -I dev/lint-python-safefs.py "${python_files[@]}"
fi

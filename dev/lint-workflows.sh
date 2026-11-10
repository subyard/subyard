#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# actionlint otherwise silently skips shell analysis when ShellCheck is absent.
command -v shellcheck >/dev/null || { printf 'workflow lint requires shellcheck\n' >&2; exit 1; }
actionlint="$(bash dev/pinned-tool.sh actionlint)"
zizmor="$(bash dev/pinned-tool.sh zizmor)"
shopt -s nullglob
workflows=(.github/workflows/*.yml .github/workflows/*.yaml)
status=0
"$actionlint" -color "${workflows[@]}" || status=$?
"$zizmor" --offline --strict-collection --no-progress \
  --config "$ROOT/.github/zizmor.yml" .github/workflows || status=$?
exit "$status"

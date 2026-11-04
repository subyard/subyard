#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
if [ "${1:-}" = --help ]; then
  printf 'Usage: removal acceptance --slot N\nRequires a frozen packaged candidate transport bundle.\n'
  exit 0
fi
[ "$#" = 2 ] && [ "$1" = --slot ] || exit 2
exec bash "$ROOT/dev/e2e/disabled-components-release.sh" --slot "$2" \
  --payload config/retired-integrations/paseo/tests/e2e/owner.sh

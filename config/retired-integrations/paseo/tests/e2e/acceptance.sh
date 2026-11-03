#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
if [ "${1:-}" = --help ]; then
  printf 'Usage: retirement acceptance --slot N [--rollback]\nRequires a frozen packaged candidate transport bundle.\n'
  exit 0
fi
payload=owner.sh
if [ "$#" = 3 ] && [ "$3" = --rollback ]; then payload=owner-rollback.sh; set -- "$1" "$2"; fi
[ "$#" = 2 ] && [ "$1" = --slot ] || exit 2
exec bash "$ROOT/dev/e2e/disabled-components-release.sh" --slot "$2" \
  --payload "config/retired-integrations/paseo/tests/e2e/$payload"

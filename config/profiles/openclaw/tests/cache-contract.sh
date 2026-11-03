#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=/dev/null
source "$ROOT/config/profiles/openclaw/profile.conf"

# shellcheck disable=SC2154
[ "$CLEANUP_CACHES" = "$npm_config_store_dir $PIP_CACHE_DIR $npm_config_cache" ]
[[ " $CLEANUP_CACHES " != *" $PLAYWRIGHT_BROWSERS_PATH "* ]]
[[ " $CACHES " == *" $PLAYWRIGHT_BROWSERS_PATH "* ]]
printf 'openclaw disposable-cache contract: PASS\n'

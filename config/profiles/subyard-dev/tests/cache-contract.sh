#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=/dev/null
source "$ROOT/config/profiles/subyard-dev/profile.conf"
[ "$CLEANUP_CACHES" = "$GOCACHE $SUBYARD_CARGO_BUILD_ROOT $SUBYARD_NPM_PUBLIC_CACHE" ]
[[ " $CLEANUP_CACHES " != *" $GOMODCACHE "* ]] # Module storage also contains installed Go toolchains.
printf 'subyard-dev disposable-cache contract: PASS\n'

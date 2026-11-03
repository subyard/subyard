#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
export DEV_USER=builder
# shellcheck source=/dev/null
source "$ROOT/config/profiles/android/profile.conf"
[ "$CLEANUP_CACHES" = "$YARD_GRADLE_USER_HOME/caches /home/builder/.gradle/caches" ]
[[ " $CLEANUP_CACHES " != *" $ANDROID_HOME "* ]]
[ "$CACHES" = '' ]
printf 'android disposable-cache contract: PASS\n'

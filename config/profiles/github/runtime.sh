#!/usr/bin/env bash
# Release activation reuses the profile's ordinary owner/guest lifecycle.
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "$profile/runtime.py" "$@"

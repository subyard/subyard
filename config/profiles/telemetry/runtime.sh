#!/usr/bin/env bash
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$profile/profile.sh" --runtime "$@"

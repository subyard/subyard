#!/usr/bin/env bash
# Preserve the installed runtime protocol owned by this profile.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$ROOT/resources/orca/handler.sh" _runtime-contract "$@"

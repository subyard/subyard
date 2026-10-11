#!/usr/bin/env bash
# Installed runtime activation uses the existing profile transport and CAS protocol.
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ "${1-}" = apply ]; then
  # Leave room for core's fixed two-minute process-group cancellation boundary.
  exec timeout --signal=TERM --kill-after=2s 110 "$profile/owner.sh" _runtime-contract "$@"
fi
exec "$profile/owner.sh" _runtime-contract "$@"

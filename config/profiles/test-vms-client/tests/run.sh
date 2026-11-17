#!/usr/bin/env bash
# Run the host-free fixtures through the live lane's preflight.
set -euo pipefail
[ "$#" -eq 0 ] || { printf 'Usage: tests/run.sh\n' >&2; exit 2; }
tests="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "$tests/periodic-preflight.sh"

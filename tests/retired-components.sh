#!/usr/bin/env bash
# The data owner pins the concrete removed IDs; core tests use synthetic inputs.
set -euo pipefail
export TMPDIR="${SUBYARD_RUN_TEMP:-/tmp}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
go -C "$ROOT" test -p 2 -count=1 ./tests/retiredcomponents

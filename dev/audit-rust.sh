#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
scanner="$(bash dev/pinned-tool.sh osv-scanner)"
"$scanner" scan source \
  --lockfile veranda/client/Cargo.lock --lockfile veranda/desktop/Cargo.lock

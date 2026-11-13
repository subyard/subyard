#!/usr/bin/env bash
# Host-free PR statistics contract; fixtures live under /tmp.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 -B "$ROOT/dev/pr-stats-test.py"

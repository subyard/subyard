#!/usr/bin/env bash
# Host-free CLI fixture slice-check contract; fixtures live under /tmp.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 -B "$ROOT/dev/cli-fixture-slice-check-test.py"

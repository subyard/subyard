#!/usr/bin/env bash
# Synthetic contract tests for the shared checkout discovery library.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 -B -m unittest discover -s "$ROOT/tests/checkout-discovery"

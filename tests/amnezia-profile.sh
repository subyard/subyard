#!/usr/bin/env bash
# Host-free behavior checks for the optional Amnezia owner and guest resources.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 -B "$ROOT/tests/helpers/amnezia_profile_test.py"

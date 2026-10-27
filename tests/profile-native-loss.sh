#!/usr/bin/env bash
# Generic native-loss identity and original-OFD predicates; synthetic inputs only.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 -B "$ROOT/tests/helpers/profile-native-loss-test.py"

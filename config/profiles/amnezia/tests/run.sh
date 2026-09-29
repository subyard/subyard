#!/usr/bin/env bash
# Host-free checks owned by this profile, using Subyard's configuration APIs.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
python3 -B "$ROOT/config/profiles/amnezia/tests/test_runtime.py"
bash "$ROOT/tests/helpers/profile-go.sh" amnezia

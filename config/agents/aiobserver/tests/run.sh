#!/usr/bin/env bash
# Host-free package contract and provisioning; proxy has its compatibility entrypoint.
set -euo pipefail
TESTS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export TMPDIR=/tmp
cd "$TESTS"
go test -count=1 .
go test -count=1 ../observation ../cmd/observation
python3 -B "$TESTS/lifecycle.py"
bash "$TESTS/provision.sh"

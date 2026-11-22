#!/usr/bin/env bash
set -euo pipefail
umask 0022
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$root"
shellcheck -x -S warning config/profiles/herdr/{provision.sh,runtime.sh,resources/herdr/*.sh,tests/*.sh}
go test -race -count=1 ./config/profiles/herdr/...
bash config/profiles/herdr/tests/install-core.sh
bash config/profiles/herdr/tests/handler.sh

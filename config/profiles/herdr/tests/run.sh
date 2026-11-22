#!/usr/bin/env bash
set -euo pipefail
umask 0022
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
cd "$root"
mapfile -t shell_files < <(find config/profiles/herdr -type f -name '*.sh' | sort)
shellcheck -x -S warning "${shell_files[@]}"
go test -race -count=1 ./config/profiles/herdr/...
bash config/profiles/herdr/tests/install-core.sh
bash config/profiles/herdr/tests/handler.sh
bash config/profiles/herdr/tests/route.sh
bash config/profiles/herdr/tests/guest-ssh.sh

#!/usr/bin/env bash
# Broker and integration checks owned by the GitHub profile.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
mapfile -t unformatted < <(gofmt -l "$ROOT/config/profiles/github")
[ "${#unformatted[@]}" -eq 0 ] \
  || { printf 'FAIL: gofmt required: %s\n' "${unformatted[*]}" >&2; exit 1; }
go -C "$ROOT" vet ./config/profiles/github/...
printf 'GitHub profile race tests: full suite umask=0022\n'
(umask 0022; go -C "$ROOT" test -race -count=1 ./config/profiles/github/...)
bash "$ROOT/tests/helpers/go-permission-umasks.sh" "$ROOT" \
  './config/profiles/github/broker:TestLoadConfigAndKeyRejectUnsafeFiles|TestProtectedFilesRejectSymlinksAndOversize'
bash "$ROOT/config/profiles/github/tests/cleanup.sh"
bash "$ROOT/config/profiles/github/tests/teardown-identity.sh"
bash "$ROOT/config/profiles/github/tests/composition.sh"

#!/usr/bin/env bash
# Broker and integration checks owned by the GitHub profile.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
mapfile -t unformatted < <(gofmt -l "$ROOT/config/profiles/github")
[ "${#unformatted[@]}" -eq 0 ] \
  || { printf 'FAIL: gofmt required: %s\n' "${unformatted[*]}" >&2; exit 1; }
go -C "$ROOT" vet ./config/profiles/github/...
for mask in 0002 0022 0077; do
  printf 'GitHub profile race tests: umask=%s\n' "$mask"
  (umask "$mask"; go -C "$ROOT" test -race -count=1 ./config/profiles/github/...)
done
bash "$ROOT/config/profiles/github/tests/cleanup.sh"
bash "$ROOT/config/profiles/github/tests/teardown-identity.sh"
bash "$ROOT/config/profiles/github/tests/composition.sh"

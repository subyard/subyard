#!/usr/bin/env bash
# Format, vet and race-test Go checks owned by one profile.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
profile="${1:-}"
[[ "$profile" =~ ^[a-z][a-z0-9-]*$ ]] || { printf 'usage: profile-go.sh PROFILE\n' >&2; exit 2; }
tests="$ROOT/config/profiles/$profile/tests"
[ -d "$tests" ] || { printf 'profile Go tests missing: %s\n' "$profile" >&2; exit 2; }
mapfile -t unformatted < <(gofmt -l "$tests")
[ "${#unformatted[@]}" -eq 0 ] \
  || { printf 'FAIL: gofmt required: %s\n' "${unformatted[*]}" >&2; exit 1; }
go -C "$ROOT" vet "./config/profiles/$profile/tests"
printf '%s profile race tests: full suite umask=0022\n' "$profile"
(umask 0022; go -C "$ROOT" test -race -count=1 "./config/profiles/$profile/tests")

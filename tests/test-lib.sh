#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="$ROOT/tests/helpers/test-lib.sh"
# shellcheck source=tests/helpers/test-lib.sh
. "$LIB"
unset TEST_LIB_DIE_STATUS
TMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-test-lib.XXXXXX")"
trap 'rm -rf -- "$TMP"' EXIT

check() {
  local expected_status=$1 expected_error=$2 code=$3 status=0
  bash -euo pipefail -c "$code" test-lib "$LIB" >"$TMP/output" 2>"$TMP/error" || status=$?
  [ "$status" = "$expected_status" ] || fail "expected exit $expected_status, got $status for $code"
  diff -u /dev/null "$TMP/output"
  diff -u <(printf '%s' "$expected_error") "$TMP/error"
}

# The child loads the library independently under the same strict shell options.
# shellcheck disable=SC2016
check 1 $'FAIL: 100% failure\n' '. "$1"; fail "100% failure"'
# shellcheck disable=SC2016
check 1 $'FAIL: fixture: a b\n' '. "$1"; TEST_LIB_PREFIX=fixture; fail a b'
# shellcheck disable=SC2016
check 1 $'FAIL: a b\n' 'export TEST_LIB_PREFIX=outer; . "$1"; fail a b'
# shellcheck disable=SC2016
check 2 $'default message\n' '. "$1"; die "default message"'
# shellcheck disable=SC2016
check 1 $'custom message\n' 'TEST_LIB_DIE_STATUS=1; . "$1"; die "custom message"'
# shellcheck disable=SC2016
check 46 $'custom message\n' 'TEST_LIB_DIE_STATUS=46; . "$1"; die "custom message"'
# shellcheck disable=SC2016
check 2 '' '. "$1"; die'

printf 'ok: shell test library\n'

#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="$ROOT/tests/helpers/test-lib.sh"
# shellcheck source=tests/helpers/test-lib.sh
. "$LIB"
unset TEST_LIB_DIE_STATUS
test_tmp

check() {
  local expected_status=$1 expected_output=$2 expected_error=$3 code=$4 status=0
  bash -euo pipefail -c "$code" test-lib "$LIB" >"$TMP/output" 2>"$TMP/error" || status=$?
  [ "$status" = "$expected_status" ] || fail "expected exit $expected_status, got $status for $code"
  diff -u <(printf '%s' "$expected_output") "$TMP/output"
  diff -u <(printf '%s' "$expected_error") "$TMP/error"
}

# The child loads the library independently under the same strict shell options.
# shellcheck disable=SC2016
check 1 '' $'FAIL: 100% failure\n' '. "$1"; fail "100% failure"'
# shellcheck disable=SC2016
check 1 '' $'FAIL: fixture: a b\n' '. "$1"; TEST_LIB_PREFIX=fixture; fail a b'
# shellcheck disable=SC2016
check 1 '' $'FAIL: a b\n' 'export TEST_LIB_PREFIX=outer; . "$1"; fail a b'
# shellcheck disable=SC2016
check 2 '' $'default message\n' '. "$1"; die "default message"'
# shellcheck disable=SC2016
check 1 '' $'custom message\n' 'TEST_LIB_DIE_STATUS=1; . "$1"; die "custom message"'
# shellcheck disable=SC2016
check 46 '' $'custom message\n' 'TEST_LIB_DIE_STATUS=46; . "$1"; die "custom message"'
# shellcheck disable=SC2016
check 2 '' '' '. "$1"; die'

# shellcheck disable=SC2016
check 37 $'second sees 37: <space and *><>\nfirst sees 37\n' '' '
  . "$1"
  first() { printf "first sees %s\n" "$?"; }
  second() { printf "second sees %s: <%s><%s>\n" "$?" "$1" "$2"; exit 99; }
  on_exit first
  on_exit second "space and *" ""
  exit 37
'
# shellcheck disable=SC2016
check 73 $'second sees 0\nfirst sees 0\nfinished\n' '' '
  . "$1"
  first() { printf "first sees %s\n" "$?"; exit 72; }
  second() { printf "second sees %s\n" "$?"; bash -c "exit 73"; printf "unreachable\n"; }
  on_exit printf "%s\n" finished
  on_exit first
  on_exit second
'

# shellcheck disable=SC2016
check 73 $'finished\n' '' '
  set -E
  trap "exit 91" ERR
  . "$1"
  cleanup() { bash -c "exit 73"; printf "unreachable\n"; }
  on_exit printf "%s\n" finished
  on_exit cleanup
'

# shellcheck disable=SC2016
check 1 $'original EXIT\n' $'FAIL: on_exit: an EXIT, INT, or TERM trap is already set\n' '
  trap "echo original EXIT" EXIT
  . "$1"
  on_exit printf "%s\n" unreachable
'
for signal in INT TERM; do
  # shellcheck disable=SC2016
  check 1 '' $'FAIL: on_exit: an EXIT, INT, or TERM trap is already set\n' '
    . "$1"
    trap "" '"$signal"'
    on_exit printf "%s\n" unreachable
  '
done

# shellcheck disable=SC2016
check 0 $'child\nparent\n' '' '
  . "$1"
  on_exit printf "%s\n" parent
  (on_exit printf "%s\n" child)
'

for signal in INT TERM; do
  case "$signal" in INT) status=130 ;; TERM) status=143 ;; esac
  # shellcheck disable=SC2016
  check "$status" "cleanup sees $status"$'\n' '' '
    . "$1"
    cleanup() { printf "cleanup sees %s\n" "$?"; }
    on_exit cleanup
    kill -s '"$signal"' "$BASHPID"
    exit 99
  '
done

mkdir "$TMP/with space"
temporary="$(TMPDIR="$TMP/with space" bash -euo pipefail -c '
  . "$1"
  test_tmp
  [ -d "$TMP" ]
  printf "%s\n" "$TMP"
' test-lib "$LIB")"
[ ! -e "$temporary" ] || fail "temporary directory survived: $temporary"

printf 'ok: shell test library\n'

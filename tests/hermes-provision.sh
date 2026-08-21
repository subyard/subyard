#!/usr/bin/env bash
# Host-free behavior contract for the Hermes substrate-only profile.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/config/profiles/hermes/provision.sh"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -x "$HOOK" ] || fail 'Hermes provision hook is not executable'
mkdir -p "$TMP/bin" "$TMP/home"
LOG="$TMP/commands.log"
PACKAGE_DB="$TMP/packages"
: >"$LOG"
: >"$PACKAGE_DB"

cat >"$TMP/bin/dpkg-query" <<'DPKG_QUERY'
#!/usr/bin/env bash
set -euo pipefail
package="${!#}"
grep -Fxq "$package" "$HERMES_TEST_PACKAGE_DB" || exit 1
printf 'install ok installed'
DPKG_QUERY

cat >"$TMP/bin/apt-get" <<'APT_GET'
#!/usr/bin/env bash
set -euo pipefail
printf 'apt-get' >>"$HERMES_TEST_LOG"
printf ' <%s>' "$@" >>"$HERMES_TEST_LOG"
printf '\n' >>"$HERMES_TEST_LOG"
if [ "${1:-}" = install ]; then
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in -*) shift ;; *) printf '%s\n' "$1" >>"$HERMES_TEST_PACKAGE_DB"; shift ;; esac
  done
  sort -u -o "$HERMES_TEST_PACKAGE_DB" "$HERMES_TEST_PACKAGE_DB"
fi
APT_GET

for command in curl git python3 runuser; do
  cat >"$TMP/bin/$command" <<'FORBIDDEN'
#!/usr/bin/env bash
set -euo pipefail
printf 'forbidden %s' "$(basename "$0")" >>"$HERMES_TEST_LOG"
printf ' <%s>' "$@" >>"$HERMES_TEST_LOG"
printf '\n' >>"$HERMES_TEST_LOG"
exit 90
FORBIDDEN
done
chmod 0755 "$TMP/bin/"*

common_env=(
  PATH="$TMP/bin:$PATH"
  HOME="$TMP/home"
  HERMES_DEV_HOME="$TMP/home"
  HERMES_TEST_ALLOW_NON_ROOT=1
  HERMES_TEST_LOG="$LOG"
  HERMES_TEST_PACKAGE_DB="$PACKAGE_DB"
)

state_signature() {
  tar -C "$TMP/home" --sort=name --numeric-owner --format=gnu \
    -cf - .hermes .local/bin/hermes | sha256sum
}

set +e
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
status=$?
set -e
[ "$status" -eq 10 ] || fail "empty substrate check status=$status, want 10"
[ ! -e "$TMP/home/.hermes" ] && [ ! -L "$TMP/home/.hermes" ] \
  || fail 'fresh substrate check created Hermes state'
[ ! -e "$TMP/home/.local/bin/hermes" ] && [ ! -L "$TMP/home/.local/bin/hermes" ] \
  || fail 'fresh substrate check installed a Hermes launcher'
[ ! -s "$LOG" ] || fail 'substrate check performed a package, network or Hermes-software action'

env "${common_env[@]}" bash "$HOOK" >/dev/null \
  || fail 'substrate provision failed'
[ ! -e "$TMP/home/.hermes" ] && [ ! -L "$TMP/home/.hermes" ] \
  || fail 'fresh substrate provision created Hermes state'
[ ! -e "$TMP/home/.local/bin/hermes" ] && [ ! -L "$TMP/home/.local/bin/hermes" ] \
  || fail 'fresh substrate provision installed a Hermes launcher'
grep -Fxq 'apt-get <update> <-qq>' "$LOG" \
  || fail 'profile did not refresh the package index'
grep -Fxq 'apt-get <install> <-y> <-qq> <build-essential> <ca-certificates> <curl> <git> <libffi-dev> <python3-dev> <xz-utils>' \
  "$LOG" || fail 'profile package set escaped the generic substrate allowlist'
[ "$(wc -l <"$LOG")" -eq 2 ] \
  || fail 'profile performed an action beyond the two expected package-manager commands'

mkdir -p "$TMP/home/.hermes/operator-owned" "$TMP/home/.local/bin"
printf 'opaque-state\n' >"$TMP/home/.hermes/operator-owned/state.bin"
printf '#!/usr/bin/env sh\nexit 0\n' >"$TMP/home/.local/bin/hermes"
chmod 0700 "$TMP/home/.hermes"
chmod 0600 "$TMP/home/.hermes/operator-owned/state.bin"
chmod 0755 "$TMP/home/.local/bin/hermes"
before="$(state_signature)"

effects_before_check="$(sha256sum "$LOG")"
env "${common_env[@]}" bash "$HOOK" --check >/dev/null \
  || fail 'converged substrate check failed'
[ "$(state_signature)" = "$before" ] || fail 'converged check changed operator-owned Hermes state'
[ "$(sha256sum "$LOG")" = "$effects_before_check" ] \
  || fail 'converged check performed an external action'

env "${common_env[@]}" bash "$HOOK" >/dev/null \
  || fail 'repeat substrate provision failed'
[ "$(state_signature)" = "$before" ] || fail 'repeat provision changed operator-owned Hermes state'
[ "$(sha256sum "$LOG")" = "$effects_before_check" ] \
  || fail 'repeat provision reran package or Hermes-software actions'

grep -Fxv xz-utils "$PACKAGE_DB" >"$TMP/packages-drifted"
mv "$TMP/packages-drifted" "$PACKAGE_DB"
set +e
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
status=$?
set -e
[ "$status" -eq 10 ] || fail "drifted substrate check status=$status, want 10"
[ "$(state_signature)" = "$before" ] || fail 'drifted check changed operator-owned Hermes state'
[ "$(sha256sum "$LOG")" = "$effects_before_check" ] \
  || fail 'drifted check performed an external action'

env "${common_env[@]}" bash "$HOOK" >/dev/null \
  || fail 'drifted substrate reconciliation failed'
grep -Fxq xz-utils "$PACKAGE_DB" || fail 'drifted prerequisite was not restored'
[ "$(state_signature)" = "$before" ] \
  || fail 'drifted reconciliation changed operator-owned Hermes state'
[ "$(grep -Fxc 'apt-get <update> <-qq>' "$LOG")" -eq 2 ] \
  && [ "$(grep -Fxc 'apt-get <install> <-y> <-qq> <build-essential> <ca-certificates> <curl> <git> <libffi-dev> <python3-dev> <xz-utils>' "$LOG")" -eq 2 ] \
  && [ "$(wc -l <"$LOG")" -eq 4 ] \
  || fail 'drifted reconciliation performed an unexpected external action'

printf 'ok: Hermes profile provides generic prerequisites without managing Hermes software\n'

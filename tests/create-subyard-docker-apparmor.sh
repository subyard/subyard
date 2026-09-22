#!/usr/bin/env bash
# Container yards hide AppArmor when their Incus daemon has already disabled it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home"
export ASSUME_YES=1
export SUBYARD_NO_AUDIT=1
export SUBYARD_POWER_DESIRED=running
export YARD_NAME=test-yard
export PATH="$TMP/bin:$PATH"
export MOCK_INCUS_LOG="$TMP/incus.log"
export MOCK_INCUS_DEVICES=''
export MOCK_INCUS_APPARMOR_ENV=''
export MOCK_PROBE_EXIT=0
export MOCK_POWER_STATE=STOPPED
install -d -m 0755 "$TMP/bin"

cat > "$TMP/bin/systemctl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'is-active NetworkManager') printf 'inactive\n'; exit 3 ;;
  'show incus.service -p Environment --value')
    printf '%s\n' "$MOCK_INCUS_APPARMOR_ENV"
    exit "$MOCK_PROBE_EXIT" ;;
  *) printf 'unexpected systemctl call: %s\n' "$*" >&2; exit 90 ;;
esac
MOCK

cat > "$TMP/bin/ip" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[ "$*" = '-4 route show default' ] \
  || { printf 'unexpected ip call: %s\n' "$*" >&2; exit 90; }
MOCK

cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_INCUS_LOG"
case "${1:-} ${2:-} ${3:-}" in
  'info  ' | 'info yard --project' | 'project show subyard') ;;
  'config device list')
    tr ' ' '\n' <<<"$MOCK_INCUS_DEVICES"
    ;;
  'config device get')
    case "${4:-}:${5:-}" in
      subyard-docker-apparmor:type) printf 'disk\n' ;;
      subyard-docker-apparmor:source) printf '/dev/null\n' ;;
      subyard-docker-apparmor:path) printf '/sys/module/apparmor/parameters/enabled\n' ;;
      subyard-docker-apparmor:readonly) printf 'true\n' ;;
    esac
    ;;
  'config device add' | 'config device remove') ;;
  'config get yard')
    [ "${4:-}" != security.nesting ] || printf 'true\n'
    ;;
  'config set yard' | 'config unset yard') ;;
  'storage volume show') ;;
  'list yard --project') printf '%s\n' "$MOCK_POWER_STATE" ;;
  'start yard --project') ;;
  *) printf 'unexpected incus call: %s\n' "$*" >&2; exit 90 ;;
esac
MOCK
chmod +x "$TMP/bin/systemctl" "$TMP/bin/ip" "$TMP/bin/incus"

run_create() {
  : > "$MOCK_INCUS_LOG"
  bash "$ROOT/scripts/03-create-subyard.sh" --yes >/dev/null
}

MOCK_INCUS_APPARMOR_ENV='INCUS_SECURITY_APPARMOR=false'
MOCK_INCUS_DEVICES=''
run_create
grep -Fxq \
  'config device add yard subyard-docker-apparmor disk --project subyard source=/dev/null path=/sys/module/apparmor/parameters/enabled readonly=true' \
  "$MOCK_INCUS_LOG" \
  || fail 'yard did not hide the unavailable AppArmor indicator from Docker'

MOCK_INCUS_APPARMOR_ENV=''
MOCK_INCUS_DEVICES='subyard-docker-apparmor'
run_create
grep -Fxq \
  'config device remove yard subyard-docker-apparmor --project subyard' \
  "$MOCK_INCUS_LOG" \
  || fail 'yard kept the Docker AppArmor mask after Incus AppArmor was restored'

# A failed observation must stop before instance, route, power or mask mutation.
for MOCK_INCUS_DEVICES in '' subyard-docker-apparmor; do
  for MOCK_POWER_STATE in STOPPED RUNNING; do
    MOCK_PROBE_EXIT=1
    : > "$MOCK_INCUS_LOG"
    if bash "$ROOT/scripts/03-create-subyard.sh" --yes >"$TMP/output" 2>&1; then
      fail 'failed AppArmor probe was accepted'
    fi
    grep -q 'unknown' "$TMP/output" || fail 'missing unknown diagnostic'
    if grep -Eq '^(init |config (set|unset|device (add|remove)) |storage volume create |start |stop )' "$MOCK_INCUS_LOG"; then
      fail 'failed probe allowed target mutation'
    fi
  done
done
MOCK_POWER_STATE=STOPPED
MOCK_PROBE_EXIT=0

for MOCK_INCUS_APPARMOR_ENV in \
  'INCUS_SECURITY_APPARMOR=false INCUS_SECURITY_APPARMOR=true' \
  'INCUS_SECURITY_APPARMOR=false INCUS_SECURITY_APPARMOR=false' \
  'INCUS_SECURITY_APPARMOR=maybe' \
  '"INCUS_SECURITY_APPARMOR=fa\lse"' \
  '"INCUS_SECURITY_APPARMOR=false' \
  'NOTE=private-sentinel\' \
  'private-sentinel' \
  'BAD-NAME=value' \
  $'INCUS_SECURITY_APPARMOR=false\nNOTE=x'; do
  : > "$MOCK_INCUS_LOG"
  if bash "$ROOT/scripts/03-create-subyard.sh" --yes >"$TMP/output" 2>&1; then
    fail 'malformed AppArmor probe was accepted'
  fi
  grep -q 'unknown' "$TMP/output" || fail 'missing unknown diagnostic'
  ! grep -q 'private-sentinel' "$TMP/output" || fail 'probe leaked environment'
  ! grep -Eq '^(init |config (set|unset|device (add|remove)) |storage volume create |start |stop )' "$MOCK_INCUS_LOG" \
    || fail 'malformed probe allowed target mutation'
done

# Complete assignments matter; flag-shaped text inside another value does not.
MOCK_INCUS_DEVICES=''
for MOCK_INCUS_APPARMOR_ENV in \
  '"NOTE=before INCUS_SECURITY_APPARMOR=false after"' \
  '"NOTE=\" INCUS_SECURITY_APPARMOR=false \""' \
  'NOTE=before\ INCUS_SECURITY_APPARMOR=false' \
  "'NOTE=before INCUS_SECURITY_APPARMOR=false after'" \
  'OTHER_INCUS_SECURITY_APPARMOR=false' \
  'INCUS_SECURITY_APPARMOR=true'; do
  run_create
  ! grep -q 'device add yard subyard-docker-apparmor' "$MOCK_INCUS_LOG" \
    || fail 'unrelated environment value enabled the mask'
done
MOCK_INCUS_APPARMOR_ENV='"INCUS_SECURITY_APPARMOR=false"'
run_create
grep -q 'device add yard subyard-docker-apparmor' "$MOCK_INCUS_LOG" \
  || fail 'quoted disabled assignment was not recognized'

# Go passes its fresh Apply observation before writing power metadata. The shell must
# consume it, not make a second fallible observation after those writes.
export SUBYARD_PREPARED_INCUS_APPARMOR=disabled
MOCK_PROBE_EXIT=90
run_create
grep -q 'device add yard subyard-docker-apparmor' "$MOCK_INCUS_LOG" \
  || fail 'prepared disabled state was not applied'
SUBYARD_PREPARED_INCUS_APPARMOR=invalid
if run_create 2>"$TMP/output"; then fail 'invalid prepared state was accepted'; fi
! grep -Eq '^(init |config (set|unset|device (add|remove)) |storage volume create |start |stop )' "$MOCK_INCUS_LOG" \
  || fail 'invalid prepared state allowed mutation'
unset SUBYARD_PREPARED_INCUS_APPARMOR

# VM cleanup must not consult the container-only capability.
cat > "$TMP/bin/dpkg" <<'MOCK'
#!/bin/sh
exit 0
MOCK
chmod +x "$TMP/bin/dpkg"
YARD_KIND=vm
MOCK_INCUS_DEVICES=subyard-docker-apparmor
run_create
grep -q 'device remove yard subyard-docker-apparmor' "$MOCK_INCUS_LOG" \
  || fail 'VM kept a stale container-only mask'

printf 'ok: AppArmor probe errors preserve yard state; known container and VM states converge\n'

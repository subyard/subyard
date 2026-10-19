#!/usr/bin/env bash
# Container yards hide AppArmor when their Incus daemon has already disabled it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-create-apparmor.XXXXXX)"
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    [ ! -f "$TMP/output" ] || cat "$TMP/output" >&2
    [ ! -f "$TMP/incus.log" ] || cat "$TMP/incus.log" >&2
  fi
  rm -rf "$TMP"
  exit "$rc"
}
trap cleanup EXIT
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
export MOCK_INSTANCE_FILE="$TMP/instance" MOCK_CATALOG_FILE="$TMP/catalog"
export MOCK_CATALOG_ATTEMPTS="$TMP/catalog-attempts"
export MOCK_CAPTURE_FILE="$TMP/metadata-root"
export MOCK_CATALOG_MODE=success MOCK_INIT_EXIT=0
export MOCK_INIT_CREATED_ON_FAILURE=0
export MOCK_INIT_SIGNAL=0
export MOCK_CATALOG_REMOTE=images:
export MOCK_TMP_CHECK_FILE="$TMP/tmp-checks" MOCK_REBOOT=0
export MOCK_BOOT_AFTER_RESTART=0
export MOCK_BOOT_WAIT_EXIT=0 MOCK_CHANGED_UUID=0
for file in "$MOCK_INSTANCE_FILE" "$MOCK_CATALOG_FILE" "$MOCK_CATALOG_ATTEMPTS" "$MOCK_CAPTURE_FILE"; do
  install -m 0600 /dev/null "$file"
done
printf '1\n' > "$MOCK_INSTANCE_FILE"
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
  'info  ' | 'project show subyard') ;;
  'info yard --project') [ "$(cat "$MOCK_INSTANCE_FILE")" = 1 ] ;;
  'image list images:'|'image list cache:'|'image list --project')
    [ "$*" = "image list${MOCK_CATALOG_REMOTE:+ $MOCK_CATALOG_REMOTE} --project subyard --format=json" ] || exit 90
    [ "$(stat -Lc %a "/proc/$$/fd/1")" = 600 ] && [ "$(stat -Lc %a "/proc/$$/fd/2")" = 600 ] || exit 91
    dirname "$(readlink "/proc/$$/fd/1")" > "$MOCK_CAPTURE_FILE"
    attempts=$(cat "$MOCK_CATALOG_ATTEMPTS")
    attempts=$((attempts + 1))
    printf '%s\n' "$attempts" > "$MOCK_CATALOG_ATTEMPTS"
    case "$MOCK_CATALOG_MODE:$attempts" in
      dns-first:1|dns-always:*) printf 'Error: lookup image.invalid: no such host private-sentinel\n' >&2; exit 1 ;;
      terminal:*) printf 'Error: not authorized private-sentinel\n' >&2; exit 7 ;;
      canceled:*) exit 130 ;;
    esac
    cat "$MOCK_CATALOG_FILE" ;;
  'init '*)
    [ "$3" = yard ] || exit 90
    [ "$(stat -Lc %a "/proc/$$/fd/2")" = 600 ] || exit 91
    dirname "$(readlink "/proc/$$/fd/2")" > "$MOCK_CAPTURE_FILE"
    if [ "$MOCK_INIT_SIGNAL" = 1 ]; then
      # Signal only this fixture's attached Bash parent while it waits for us.
      kill -TERM "$PPID"
      exit 0
    fi
    if [ "$2" = images:debian/13 ] && [ "$MOCK_INIT_EXIT" != 0 ]; then
      [ "$MOCK_INIT_CREATED_ON_FAILURE" != 1 ] || printf '1\n' > "$MOCK_INSTANCE_FILE"
      printf 'Error: Failed getting image: lookup image.invalid: no such host private-sentinel\n' >&2
      exit "$MOCK_INIT_EXIT"
    fi
    printf '1\n' > "$MOCK_INSTANCE_FILE" ;;
  'config device list')
    tr ' ' '\n' <<<"$MOCK_INCUS_DEVICES"
    ;;
  'config device get')
    case "${5:-}:${6:-}" in
      subyard-docker-apparmor:type) printf 'disk\n' ;;
      subyard-docker-apparmor:source) printf '/dev/null\n' ;;
      subyard-docker-apparmor:path) printf '/sys/module/apparmor/parameters/enabled\n' ;;
      subyard-docker-apparmor:readonly) printf 'true\n' ;;
    esac
    ;;
  'config device add' | 'config device remove') ;;
  'config get yard')
    [ "${4:-}" != security.nesting ] || printf 'true\n'
    if [ "${4:-}" = volatile.uuid ]; then
      if [ "$MOCK_CHANGED_UUID" = 1 ] && [ -e "$MOCK_TMP_CHECK_FILE" ]; then
        printf 'replacement-yard-uuid\n'
      else printf 'fixture-new-yard-uuid\n'; fi
    fi
    [ "${4:-}" != limits.cpu ] || printf '%s\n' "${LIMITS_CPU:-}"
    [ "${4:-}" != limits.memory ] || printf '%s\n' "${LIMITS_MEMORY:-}"
    [ "${4:-}" != user.subyard.owner_create ] || printf 'fixture-owner-token\n'
    ;;
  'config set yard' | 'config unset yard') ;;
  'storage volume show') ;;
  'list yard --project')
    if [ "$MOCK_REBOOT" = 1 ] && [ -f "$MOCK_TMP_CHECK_FILE.power" ]; then
      cat "$MOCK_TMP_CHECK_FILE.power"
    else printf '%s\n' "$MOCK_POWER_STATE"; fi ;;
  'start yard --project') [ "$MOCK_REBOOT" != 1 ] || printf 'RUNNING\n' > "$MOCK_TMP_CHECK_FILE.power" ;;
  'stop yard --project')
    printf 'STOPPED\n' > "$MOCK_TMP_CHECK_FILE.power"
    [ "$MOCK_BOOT_AFTER_RESTART" != 1 ] || printf 'initializing\n' > "$MOCK_TMP_CHECK_FILE.boot" ;;
  'exec yard --project')
    case "$*" in
      'exec yard --project subyard -- true') ;;
      'exec yard --project subyard -T -- sh -s -- apply') cat >/dev/null ;;
      'exec yard --project subyard -T -- sh -s -- wait-boot')
        cat >/dev/null
        [ "$MOCK_BOOT_WAIT_EXIT" = 0 ] || exit "$MOCK_BOOT_WAIT_EXIT"
        printf 'running\n' > "$MOCK_TMP_CHECK_FILE.boot" ;;
      'exec yard --project subyard -T -- sh -s -- check')
        cat >/dev/null
        if [ "$MOCK_REBOOT" = 1 ] && [ ! -e "$MOCK_TMP_CHECK_FILE" ]; then
          : > "$MOCK_TMP_CHECK_FILE"
          exit 10
        fi ;;
      *'ip -4 -o address show dev'*)
        if [ "$MOCK_BOOT_AFTER_RESTART" = 1 ]; then
          [ "$(cat "$MOCK_TMP_CHECK_FILE.boot" 2>/dev/null)" = running ] || exit 1
        fi
        printf '2: eth0    inet 10.80.0.10/24 brd 10.80.0.255 scope global eth0\n' ;;
      *) printf 'unexpected incus call: %s\n' "$*" >&2; exit 90 ;;
    esac
    ;;
  'query /1.0/instances/yard?project=subyard ')
    printf '%s\n' '{"devices":{"eth0":{"type":"nic","network":"incusbr0","ipv4.address":"10.80.0.10"}}}'
    ;;
  *) printf 'unexpected incus call: %s\n' "$*" >&2; exit 90 ;;
esac
MOCK
chmod +x "$TMP/bin/systemctl" "$TMP/bin/ip" "$TMP/bin/incus"

run_create() {
  local rc=0
  : > "$MOCK_INCUS_LOG"
  : > "$MOCK_CAPTURE_FILE"
  bash "$ROOT/scripts/03-create-subyard.sh" "$@" --yes >"$TMP/output" 2>&1 || rc=$?
  if [ -s "$MOCK_CAPTURE_FILE" ]; then
    [ ! -e "$(cat "$MOCK_CAPTURE_FILE")" ] || fail 'private image metadata capture survived the operation'
  fi
  return "$rc"
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
if run_create; then fail 'invalid prepared state was accepted'; fi
! grep -Eq '^(init |config (set|unset|device (add|remove)) |storage volume create |start |stop )' "$MOCK_INCUS_LOG" \
  || fail 'invalid prepared state allowed mutation'
unset SUBYARD_PREPARED_INCUS_APPARMOR

# VM cleanup must not consult the container-only capability.
cat > "$TMP/bin/dpkg" <<'MOCK'
#!/bin/sh
exit 0
MOCK
chmod +x "$TMP/bin/dpkg"
MOCK_INCUS_DEVICES=subyard-docker-apparmor
YARD_KIND=vm VM_PIN_IPV4=1 VM_FREE_PAGE_REPORTING=0 SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=1 run_create
grep -Fxq 'exec yard --project subyard -- true' "$MOCK_INCUS_LOG" \
  || fail 'VM cleanup did not verify agent readiness'
grep -q 'device remove yard subyard-docker-apparmor' "$MOCK_INCUS_LOG" \
  || fail 'VM kept a stale container-only mask'

printf 'ok: AppArmor probe errors preserve yard state; known container and VM states converge\n'

# Image transport failures must not switch distribution or retry a mutating init.
reset_creation() {
  printf '0\n' > "$MOCK_INSTANCE_FILE"
  printf '0\n' > "$MOCK_CATALOG_ATTEMPTS"
  MOCK_CATALOG_MODE=success MOCK_INIT_EXIT=0 MOCK_INIT_CREATED_ON_FAILURE=0 MOCK_INIT_SIGNAL=0
  MOCK_PROBE_EXIT=0 MOCK_INCUS_APPARMOR_ENV=''
  printf '[{"fingerprint":"%064d","type":"container","aliases":[{"name":"debian/13"}]}]\n' 1 > "$MOCK_CATALOG_FILE"
}
assert_no_init() { ! grep -q '^init ' "$MOCK_INCUS_LOG" || fail 'unproven image availability allowed creation'; }
reset_creation
MOCK_INIT_EXIT=1
rc=0
run_create || rc=$?
[ "$rc" = 1 ] || fail 'mutating init DNS failure lost its native status or switched to fallback'
[ "$(grep -c '^init ' "$MOCK_INCUS_LOG")" = 1 ] || fail 'mutating init was retried'
! grep -q '^init images:ubuntu/24.04 ' "$MOCK_INCUS_LOG" || fail 'mutating failure selected fallback'
! grep -q private-sentinel "$TMP/output" || fail 'private init stderr leaked'

reset_creation
MOCK_INIT_EXIT=130
rc=0
run_create || rc=$?
[ "$rc" = 130 ] || fail 'mutating init cancellation exit was lost'
[ "$(grep -c '^init ' "$MOCK_INCUS_LOG")" = 1 ] || fail 'canceled init was retried'
! grep -q private-sentinel "$TMP/output" || fail 'canceled init stderr leaked'

reset_creation
MOCK_INIT_SIGNAL=1
rc=0
run_create || rc=$?
[ "$rc" = 143 ] || fail 'real init TERM signal did not preserve interruption'
[ "$(grep -c '^init ' "$MOCK_INCUS_LOG")" = 1 ] || fail 'interrupted init was retried'

reset_creation
MOCK_INIT_EXIT=17 MOCK_INIT_CREATED_ON_FAILURE=1
run_create
grep -q 'initialization warning (incus init exit 17)' "$TMP/output" || fail 'created instance warning was lost'
[ "$(grep -c '^init ' "$MOCK_INCUS_LOG")" = 1 ] || fail 'warning caused a second init'
! grep -q private-sentinel "$TMP/output" || fail 'initialization warning stderr leaked'

reset_creation
MOCK_CATALOG_MODE=dns-first
run_create
[ "$(cat "$MOCK_CATALOG_ATTEMPTS")" = 2 ] || fail 'transient metadata failure was not retried'
[ "$(grep -c '^init images:debian/13 ' "$MOCK_INCUS_LOG")" = 1 ] || fail 'recovered metadata did not retain preferred image'
! grep -q private-sentinel "$TMP/output" || fail 'metadata stderr leaked'

for mode in terminal canceled dns-always; do
  reset_creation
  MOCK_CATALOG_MODE=$mode
  rc=0
  run_create || rc=$?
  [ "$rc" != 0 ] || fail "$mode metadata failure was accepted"
  assert_no_init
  expected=1
  [ "$mode" != dns-always ] || expected=3
  [ "$(cat "$MOCK_CATALOG_ATTEMPTS")" = "$expected" ] || fail "$mode metadata retry count changed"
  [ "$mode" != terminal ] || [ "$rc" = 7 ] || fail 'terminal metadata exit was lost'
  [ "$mode" != canceled ] || [ "$rc" = 130 ] || fail 'cancellation exit was lost'
  ! grep -q private-sentinel "$TMP/output" || fail 'private metadata stderr leaked'
done

for catalog in 'not-json' '{}' '[{"fingerprint":"bad","type":"container","aliases":[]}]'; do
  reset_creation
  printf '%s\n' "$catalog" > "$MOCK_CATALOG_FILE"
  if run_create; then fail 'malformed image catalog was accepted'; fi
  assert_no_init
done

# Incus accepts unaliased fingerprint prefixes and legacy container types.
reset_creation
printf '[{"fingerprint":"%064d","type":"","aliases":null}]\n' 1 > "$MOCK_CATALOG_FILE"
YARD_IMAGE=images:000000000000 run_create
grep -q '^init images:000000000000 yard ' "$MOCK_INCUS_LOG" || fail 'unique fingerprint prefix was not preserved'
reset_creation
printf '[{"fingerprint":"%064d","type":"container","aliases":null},{"fingerprint":"%064d","type":"container","aliases":[]}]\n' 1 2 > "$MOCK_CATALOG_FILE"
if YARD_IMAGE=images:000000000000 run_create; then fail 'ambiguous fingerprint prefix was accepted'; fi
assert_no_init
sed -i 's/"type":"container"/"type":"virtual-machine"/2' "$MOCK_CATALOG_FILE"
if YARD_IMAGE=images:000000000000 run_create; then fail 'cross-kind fingerprint ambiguity was accepted'; fi
assert_no_init

for catalog in '[] []' '[{"fingerprint":"0000000000000000000000000000000000000000000000000000000000000001","type":"container","aliases":false}]'; do
  reset_creation
  printf '%s\n' "$catalog" > "$MOCK_CATALOG_FILE"
  if run_create; then fail 'invalid catalog framing or aliases were accepted'; fi
  assert_no_init
done

reset_creation
YARD_IMAGE=debian/13 MOCK_CATALOG_REMOTE='' run_create
grep -q '^init debian/13 yard ' "$MOCK_INCUS_LOG" || fail 'bare image reference lost its default remote'
reset_creation
YARD_IMAGE=cache:debian/13 MOCK_CATALOG_REMOTE=cache: run_create
grep -q '^init cache:debian/13 yard ' "$MOCK_INCUS_LOG" || fail 'configured remote was changed'

reset_creation
python3 - "$MOCK_CATALOG_FILE" <<'PY'
import json,sys
with open(sys.argv[1], 'w') as output:
    json.dump([dict(fingerprint='0'*63+'1', type='container', aliases=[dict(name='debian/13')],
                   properties=dict(description='x'*(3*1024*1024)))], output)
PY
run_create
grep -q '^init images:debian/13 ' "$MOCK_INCUS_LOG" || fail 'catalog larger than ARG_MAX was not read from its file'

reset_creation
printf '[{"fingerprint":"%064d","type":"container","aliases":[{"name":"debian/13/cloud"}]}]\n' 1 > "$MOCK_CATALOG_FILE"
run_create
grep -q '^init images:ubuntu/24.04 ' "$MOCK_INCUS_LOG" || fail 'substring alias was treated as exact availability'

reset_creation
printf '[]\n' > "$MOCK_CATALOG_FILE"
run_create
[ "$(grep -c '^init ' "$MOCK_INCUS_LOG")" = 1 ] || fail 'positive absence did not create once'
grep -q '^init images:ubuntu/24.04 yard --project subyard ' "$MOCK_INCUS_LOG" || fail 'positive absence did not select fallback'

# Kind is part of availability, including the unchanged VM launch flag.
reset_creation
YARD_KIND=vm VM_FREE_PAGE_REPORTING=0 run_create
grep -q '^init images:ubuntu/24.04 yard --project subyard --vm ' "$MOCK_INCUS_LOG" || fail 'container image was used for a VM'

printf '1\n' > "$MOCK_INSTANCE_FILE"
MOCK_CATALOG_MODE=terminal
run_create
! grep -Eq '^(image list |init )' "$MOCK_INCUS_LOG" || fail 'existing instance consulted image metadata'
printf 'ok: image metadata retries preserve preferred image and fallback requires proven absence\n'

# First-boot temporary storage convergence is part of ordinary creation for both
# kinds, even with no optional profiles. Existing instances never cross this stop.
for kind in container vm; do
  reset_creation
  MOCK_INCUS_DEVICES=''
  MOCK_REBOOT=1 MOCK_POWER_STATE=RUNNING
  rm -f "$MOCK_TMP_CHECK_FILE" "$MOCK_TMP_CHECK_FILE.power"
  YARD_KIND="$kind" VM_FREE_PAGE_REPORTING=0 ENVIRONMENT_PROFILES='' run_create
  [ "$(grep -c '^stop yard --project subyard$' "$MOCK_INCUS_LOG")" = 1 ] \
    || fail "new $kind did not finish the first tmpfs boot"
  [ "$(grep -c '^exec yard --project subyard -T -- sh -s -- check$' "$MOCK_INCUS_LOG")" = 2 ] \
    || fail "new $kind did not verify actual storage after boot"
  run_create
  ! grep -q '^stop ' "$MOCK_INCUS_LOG" || fail "existing $kind was stopped for temporary storage"
done
printf 'ok: new container and VM disk /tmp converge before handoff; existing yards remain running\n'

# An agent and masked disk /tmp can be visible before the restarted VM has a
# default route. Normal creation must wait for boot before its native IPv4 probe.
reset_creation
MOCK_INCUS_DEVICES='' MOCK_REBOOT=1 MOCK_POWER_STATE=STOPPED MOCK_BOOT_AFTER_RESTART=1
rm -f "$MOCK_TMP_CHECK_FILE" "$MOCK_TMP_CHECK_FILE.power" "$MOCK_TMP_CHECK_FILE.boot"
YARD_KIND=vm VM_FREE_PAGE_REPORTING=0 VM_PIN_IPV4=0 run_create
grep -Fxq 'exec yard --project subyard -T -- sh -s -- wait-boot' "$MOCK_INCUS_LOG" \
  || fail 'new VM storage restart did not wait for native boot completion'
MOCK_BOOT_AFTER_RESTART=0
printf 'ok: first VM storage restart waits past exec readiness before native IPv4 inspection\n'

for kind in container vm; do
  reset_creation
  MOCK_REBOOT=1 MOCK_POWER_STATE=RUNNING MOCK_BOOT_WAIT_EXIT=23
  rm -f "$MOCK_TMP_CHECK_FILE" "$MOCK_TMP_CHECK_FILE.power"
  if YARD_KIND="$kind" VM_FREE_PAGE_REPORTING=0 run_create; then
    fail "$kind storage restart accepted failed boot readiness"
  fi
  grep -Fq 'new yard boot did not complete after temporary storage restart' "$TMP/output" \
    || fail "$kind storage restart lost its boot failure diagnostic"
  [ "$(grep -c '^exec yard --project subyard -T -- sh -s -- check$' "$MOCK_INCUS_LOG")" = 1 ] \
    || fail "$kind checked storage after failed boot readiness"
done
MOCK_BOOT_WAIT_EXIT=0
reset_creation
MOCK_REBOOT=1 MOCK_CHANGED_UUID=1
rm -f "$MOCK_TMP_CHECK_FILE" "$MOCK_TMP_CHECK_FILE.power"
if YARD_KIND=vm VM_FREE_PAGE_REPORTING=0 run_create; then
  fail 'storage restart accepted a replacement instance identity'
fi
grep -Fq 'new yard identity changed before temporary storage boot' "$TMP/output" \
  || fail 'storage restart lost its identity refusal'
! grep -q '^stop ' "$MOCK_INCUS_LOG" || fail 'storage restart stopped a replacement instance'
MOCK_CHANGED_UUID=0
printf 'ok: new-yard storage restart refuses failed boot readiness and replaced instance identity\n'

# Native creation precedes the split shell portions. Only a fresh POST result
# may authorize their initial storage restart; retained identity alone may not.
(
  export SUBYARD_PREPARED_OWNER_PHASE=1
  export SUBYARD_PREPARED_OWNER_INSTANCE_UUID=fixture-new-yard-uuid
  export SUBYARD_PREPARED_OWNER_INSTANCE_TOKEN=fixture-owner-token
  export SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID=fixture-new-yard-uuid
  for kind in container vm; do
    reset_creation
    printf '1\n' > "$MOCK_INSTANCE_FILE"
    MOCK_INCUS_DEVICES='' MOCK_REBOOT=1 MOCK_POWER_STATE=RUNNING
    rm -f "$MOCK_TMP_CHECK_FILE" "$MOCK_TMP_CHECK_FILE.power"
    YARD_KIND="$kind" VM_FREE_PAGE_REPORTING=0 run_create --owner-before-srv
    ! grep -Eq '^(init |image |stop |exec .*sh -s -- apply)' "$MOCK_INCUS_LOG" \
      || fail "native $kind before-srv crossed the storage boot boundary"
    YARD_KIND="$kind" VM_FREE_PAGE_REPORTING=0 run_create --owner-after-srv
    [ "$(grep -c '^stop yard --project subyard$' "$MOCK_INCUS_LOG")" = 1 ] \
      || fail "fresh native $kind did not complete the initial storage restart"
    [ "$(grep -c '^exec yard --project subyard -T -- sh -s -- check$' "$MOCK_INCUS_LOG")" = 2 ] \
      || fail "fresh native $kind did not verify actual storage after restart"
    ! grep -Eq '^(init |image )' "$MOCK_INCUS_LOG" || fail "native $kind repeated SDK creation"

    rm -f "$MOCK_TMP_CHECK_FILE" "$MOCK_TMP_CHECK_FILE.power"
    SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID='' YARD_KIND="$kind" VM_FREE_PAGE_REPORTING=0 \
      run_create --owner-after-srv
    ! grep -Eq '^(stop |exec .*sh -s -- apply)' "$MOCK_INCUS_LOG" \
      || fail "retained native $kind identity authorized a storage restart"
    if SUBYARD_PREPARED_OWNER_FRESH_INSTANCE_UUID=replacement-uuid YARD_KIND="$kind" VM_FREE_PAGE_REPORTING=0 \
        run_create --owner-after-srv; then
      fail "native $kind accepted a different fresh-instance UUID"
    fi
    ! grep -Eq '^(start |stop |config (set|device (add|remove)))' "$MOCK_INCUS_LOG" \
      || fail "native $kind mutated a different fresh-instance identity"
  done
)
printf 'ok: prepared split creation converges fresh disk /tmp and never restarts retained instance results\n'

# Consume the physical fixture's actual generated inputs through normal creation.
# The selected project has its own local catalog: a bare default-project cache
# alias must not replace the explicitly remote source for either guest kind.
(
  eval "$(sed -n '/^write_yard_config() {/,/^}/p' "$ROOT/dev/e2e/disk-tmp-acceptance.sh")"
  export STATE="$TMP/image-selection" name=fixture-input port=34567
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
  identity_bin="$TMP/identity-bin"
  install -d -m 0700 "$identity_bin"
  export MOCK_ACCOUNT_FILE="$TMP/fixture-account" MOCK_ACCOUNT_CALLS="$TMP/fixture-account-calls"
  install -m 0600 /dev/null "$MOCK_ACCOUNT_FILE"
  install -m 0600 /dev/null "$MOCK_ACCOUNT_CALLS"
  # Execute the real account boundary with generated settings. UID 1000 is
  # reserved, matching the cloud-image failure; no host account commands run.
  sed -n '/^# Groups + unprivileged/,/^# dev sudo/{ /^# dev sudo/!p; }' \
    "$ROOT/scripts/04-provision-subyard.sh" > "$TMP/provision-user.sh"
  cat > "$identity_bin/id" <<'MOCK'
#!/bin/sh
test "$*" = '-u dev' || exit 90
test -s "$MOCK_ACCOUNT_FILE" || exit 1
cat "$MOCK_ACCOUNT_FILE"
MOCK
  cat > "$identity_bin/useradd" <<'MOCK'
#!/bin/sh
test "$*" = '-u 1001 -m -s /bin/bash dev' || exit 1
echo "useradd $*" >> "$MOCK_ACCOUNT_CALLS"
echo 1001 > "$MOCK_ACCOUNT_FILE"
MOCK
  cat > "$identity_bin/groupadd" <<'MOCK'
#!/bin/sh
case "$*" in '-f yard'|'-f kvm') ;; *) exit 90 ;; esac
MOCK
  cat > "$identity_bin/usermod" <<'MOCK'
#!/bin/sh
test "$*" = '-aG yard,kvm,docker dev'
MOCK
  chmod 0700 "$identity_bin/"*
  for kind in container vm; do
    reset_creation
    MOCK_INCUS_DEVICES='' MOCK_REBOOT=0 MOCK_POWER_STATE=RUNNING
    write_yard_config
    set -a
    # shellcheck source=/dev/null
    . "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
    set +a
    image_type=container
    [ "$kind" != vm ] || image_type=virtual-machine
    printf '[{"fingerprint":"%064d","type":"%s","aliases":[{"name":"debian/13/cloud"}]}]\n' \
      1 "$image_type" > "$MOCK_CATALOG_FILE"
    run_create
    grep -Fxq 'image list images: --project subyard --format=json' "$MOCK_INCUS_LOG" \
      || fail "$kind fixture did not query its explicit remote source"
    grep -Fq 'init images:debian/13/cloud yard --project subyard ' "$MOCK_INCUS_LOG" \
      || fail "$kind fixture image did not reach its selected isolated project"
    ! grep -Fq 'preferred image is absent' "$TMP/output" \
      || fail "$kind fixture relied on a default-project image alias"
    [ "$DEV_USER:$DEV_UID" = dev:1001 ] || fail "$kind fixture identity was not explicit"
    : > "$MOCK_ACCOUNT_FILE"
    : > "$MOCK_ACCOUNT_CALLS"
    for _ in 1 2; do
      PATH="$identity_bin:/usr/bin:/bin" bash -euo pipefail "$TMP/provision-user.sh" \
        > "$TMP/identity-output" 2>&1 || { cat "$TMP/identity-output" >&2; fail "$kind configured user did not provision"; }
      [ ! -s "$TMP/identity-output" ] || fail "$kind configured user drifted on repeat"
    done
    [ "$(cat "$MOCK_ACCOUNT_CALLS")" = 'useradd -u 1001 -m -s /bin/bash dev' ] \
      || fail "$kind generated identity did not reach native useradd exactly once"
    : > "$MOCK_ACCOUNT_FILE"
    if DEV_UID=1000 PATH="$identity_bin:/usr/bin:/bin" bash -euo pipefail "$TMP/provision-user.sh" \
        > "$TMP/identity-output" 2>&1; then
      fail "$kind native account fixture accepted the reserved image UID"
    fi
    grep -Fq 'uid 1000 is taken in the yard' "$TMP/identity-output" \
      || fail "$kind native account fixture lost the occupied-UID refusal"
  done
)
printf 'ok: physical fixture image inputs resolve remotely in the selected isolated project for both kinds\n'
printf 'ok: generated cloud fixture identity provisions and repeats through the native account boundary\n'

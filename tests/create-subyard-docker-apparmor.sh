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
install -d -m 0755 "$TMP/bin"

cat > "$TMP/bin/systemctl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'is-active NetworkManager') printf 'inactive\n'; exit 3 ;;
  'show incus.service -p Environment --value') printf '%s\n' "$MOCK_INCUS_APPARMOR_ENV" ;;
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
  'list yard --project') printf 'STOPPED\n' ;;
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

printf 'ok: container yard exposes Docker only to the AppArmor support Incus provides\n'

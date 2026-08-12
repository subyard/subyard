#!/usr/bin/env bash
# NetworkManager privilege and fail-closed power checks.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$tmp"
mkdir -p "$tmp/bin"
export MOCK_SYSTEMCTL_LOG="$tmp/systemctl.log"
export MOCK_SUDO_LOG="$tmp/sudo.log"
export MOCK_NM_LOG="$tmp/nm.log"
export MOCK_NM_COUNT="$tmp/nm.count"
export MOCK_INCUS_LOG="$tmp/incus.log"
export MOCK_INCUS_EXEC_COUNT="$tmp/incus-exec.count"
export MOCK_SUDO_AUTH="$tmp/sudo.auth"

cat > "$tmp/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  is-active)
    case "${MOCK_NM_STATE:-inactive}" in
      active) printf 'active\n'; exit 0 ;;
      inactive) printf 'inactive\n'; exit 3 ;;
      error) exit 1 ;;
      *) printf '%s\n' "$MOCK_NM_STATE"; exit 3 ;;
    esac
    ;;
  reload) printf 'reload\n' >> "$MOCK_SYSTEMCTL_LOG"; exit "${MOCK_RELOAD_RC:-0}" ;;
  *) exit 90 ;;
esac
SH
cat > "$tmp/bin/id" <<'SH'
#!/usr/bin/env bash
if [ "${1:-}" = -u ]; then printf '%s\n' "${MOCK_UID:-1000}"; else exec /usr/bin/id "$@"; fi
SH
cat > "$tmp/bin/sudo" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_SUDO_LOG"
case "${1:-}" in
  -v)
    [ "${MOCK_SUDO_V_RC:-0}" = 0 ] || exit "$MOCK_SUDO_V_RC"
    : > "$MOCK_SUDO_AUTH"
    ;;
  -n)
    [ "${MOCK_SUDO_N_RC:-0}" = 0 ] || exit "$MOCK_SUDO_N_RC"
    [ "${MOCK_SUDO_REQUIRE_V:-1}" = 0 ] || [ -f "$MOCK_SUDO_AUTH" ] || exit 1
    shift
    [ "${1:-}" != -v ] || exit 0
    [ "${1:-}" != -- ] || shift
    exec env MOCK_NM_PRIVILEGED=1 "$@"
    ;;
  *) exit 91 ;;
esac
SH
cat > "$tmp/bin/NetworkManager" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = --print-config ] || exit 90
n=0
[ ! -f "$MOCK_NM_COUNT" ] || n="$(cat "$MOCK_NM_COUNT")"
n=$((n + 1)); printf '%s\n' "$n" > "$MOCK_NM_COUNT"
printf '%s:%s\n' "$n" "${MOCK_NM_PRIVILEGED:-direct}" >> "$MOCK_NM_LOG"
mode="${MOCK_NM_MODE:-valid}"
[ "$mode" != valid-then-invalid ] || { if [ "$n" -eq 1 ]; then mode=valid; else mode=invalid; fi; }
[ "$mode" != fail ] || exit 7
if [ -n "${MOCK_NM_CONFIG_FILE:-}" ]; then
  cat "$MOCK_NM_CONFIG_FILE"
  exit 0
fi
if [ "$mode" = valid ]; then
  cat <<'CFG'
[main]
no-auto-default=type:veth;interface-name:incusbr0;
[keyfile]
unmanaged-devices=type:veth;driver:veth;interface-name:incusbr0;
[device-subyard]
managed=0
CFG
else
  printf '[main]\nno-auto-default=none\n'
fi
SH
cat > "$tmp/bin/ip" <<'SH'
#!/usr/bin/env bash
case "$*" in '-4 route show default') exit 0 ;; *) exit 1 ;; esac
SH
cat > "$tmp/bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  list) printf 'STOPPED\n' ;;
  network)
    [ "${2:-}" = list ] || exit 90
    [ "${MOCK_INCUS_NETWORK_RC:-0}" = 0 ] || exit "$MOCK_INCUS_NETWORK_RC"
    printf '%s' "${MOCK_INCUS_NETWORKS:-}"
    ;;
  start) printf 'start\n' >> "$MOCK_INCUS_LOG" ;;
  stop) printf 'stop\n' >> "$MOCK_INCUS_LOG"; exit "${MOCK_INCUS_STOP_RC:-0}" ;;
  exec)
    if [ "${MOCK_INCUS_EXEC_HANG:-0}" = 1 ]; then
      /bin/sleep 30
      exit 1
    fi
    if [[ " $* " = *" -- sh -eu -c "* ]]; then
      printf '%s\n' "${MOCK_INCUS_ETH0_ADDRESS:-}"
      exit 0
    fi
    n=0; [ ! -f "$MOCK_INCUS_EXEC_COUNT" ] || n="$(cat "$MOCK_INCUS_EXEC_COUNT")"
    n=$((n + 1)); printf '%s\n' "$n" > "$MOCK_INCUS_EXEC_COUNT"
    [ "$n" -ge "${MOCK_INCUS_EXEC_READY_AFTER:-1}" ]
    ;;
  *) exit 90 ;;
esac
SH
cat > "$tmp/bin/nmcli" <<'SH'
#!/usr/bin/env bash
exit "${MOCK_NMCLI_RC:-1}"
SH
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH"

# shellcheck source=scripts/lib-power.sh
# shellcheck disable=SC1091
. "$ROOT/scripts/lib-power.sh"
power_nm_binary() { printf '%s\n' "$tmp/bin/NetworkManager"; }

ROOT="$ROOT" bash -c '
  set -euo pipefail
  . "$ROOT/scripts/lib/host.sh"
  declare -F power_nm_active >/dev/null
' || fail "host adapter does not load its NetworkManager safety dependency"

reset_case() {
  : > "$MOCK_SYSTEMCTL_LOG"
  : > "$MOCK_SUDO_LOG"
  : > "$MOCK_NM_LOG"
  : > "$MOCK_INCUS_LOG"
  rm -f "$MOCK_NM_COUNT" "$MOCK_SUDO_AUTH" "$MOCK_INCUS_EXEC_COUNT"
  POWER_ERROR=''
  export MOCK_NM_STATE=active MOCK_UID=1000 MOCK_SUDO_V_RC=0 MOCK_SUDO_N_RC=0 \
    MOCK_SUDO_REQUIRE_V=1 MOCK_NM_MODE=valid MOCK_RELOAD_RC=0 MOCK_NMCLI_RC=1 \
    MOCK_INCUS_STOP_RC=0 MOCK_INCUS_EXEC_READY_AFTER=1 MOCK_INCUS_EXEC_HANG=0 \
    MOCK_INCUS_NETWORK_RC=0 MOCK_INCUS_NETWORKS='' MOCK_NM_CONFIG_FILE=''
}

reset_case
MOCK_NM_STATE=inactive
power_nm_guard_effective incusbr0 || fail "inactive NetworkManager was rejected"
[ ! -s "$MOCK_NM_LOG" ] || fail "inactive NetworkManager invoked its reader"

reset_case
MOCK_NM_STATE=error
if power_nm_guard_effective incusbr0; then fail "NetworkManager state error was accepted"; fi
case "$POWER_ERROR" in *inspect*) ;; *) fail "NetworkManager state error is unclear" ;; esac

reset_case
MOCK_UID=0
power_nm_guard_effective incusbr0 || fail "root effective-config read failed"
[ ! -s "$MOCK_SUDO_LOG" ] || fail "root effective-config read invoked sudo"
grep -Fq '1:direct' "$MOCK_NM_LOG" || fail "root did not read NetworkManager directly"

reset_case
power_nm_prepare_reader || fail "sudo credential preparation failed"
power_nm_guard_effective incusbr0 || fail "privileged non-root read failed"
grep -Fxq -- '-v' "$MOCK_SUDO_LOG" || fail "sudo credential was not prepared"
grep -Fxq -- "-n -- $tmp/bin/NetworkManager --print-config" "$MOCK_SUDO_LOG" \
  || fail "effective config did not use sudo -n"
grep -Fq '1:1' "$MOCK_NM_LOG" || fail "NetworkManager did not run through sudo"

reset_case
: > "$MOCK_SUDO_AUTH"
SUBYARD_SUDO_PREAUTHORIZED=1 power_nm_prepare_reader \
  || fail "preauthorized adapter did not accept cached sudo credentials"
grep -Fxq -- '-n true' "$MOCK_SUDO_LOG" \
  || fail "preauthorized adapter attempted an interactive sudo prompt"

reset_case
MOCK_SUDO_V_RC=1
if power_nm_prepare_reader; then fail "failed sudo authorization was accepted"; fi
case "$POWER_ERROR" in *authorize*) ;; *) fail "sudo authorization error is unclear" ;; esac

reset_case
MOCK_SUDO_N_RC=1
power_nm_prepare_reader || fail "sudo preparation failed before read-error case"
if power_nm_guard_effective incusbr0; then fail "failed privileged read was accepted"; fi
case "$POWER_ERROR" in *"as root"*) ;; *) fail "privileged read error is unclear" ;; esac

reset_case
MOCK_NM_MODE=invalid
power_nm_prepare_reader || fail "sudo preparation failed before invalid-config case"
if power_nm_guard_effective incusbr0; then fail "incomplete effective config was accepted"; fi

reset_case
if power_nm_guard_effective incusbr0; then fail "unprepared privileged read was accepted"; fi

reset_case
power_nm_prepare_reader || fail "sudo preparation failed before precheck case"
MOCK_SUDO_N_RC=1
if power_start_guarded test-project test-yard incusbr0; then fail "start passed without a readable guard"; fi
[ ! -s "$MOCK_INCUS_LOG" ] || fail "precheck failure reached incus start"

reset_case
power_nm_prepare_reader || fail "sudo preparation failed before post-check case"
MOCK_NM_MODE=valid-then-invalid
if power_start_guarded test-project test-yard incusbr0; then fail "unsafe post-start check succeeded"; fi
grep -Fxq start "$MOCK_INCUS_LOG" || fail "post-check case did not start the instance"
grep -Fxq stop "$MOCK_INCUS_LOG" || fail "post-check failure did not stop the instance"

reset_case
power_nm_prepare_reader || fail "sudo preparation failed before stop-error case"
MOCK_NM_MODE=valid-then-invalid MOCK_INCUS_STOP_RC=1
if power_start_guarded test-project test-yard incusbr0; then fail "failed fail-closed stop returned success"; fi
case "$POWER_ERROR" in *"FAILED to stop unsafe"*) ;; *) fail "stop failure was reported as success" ;; esac

# shellcheck disable=SC2034 # consumed by sourced config module
SUBYARD_CONFIG_LOADED=1
DEV_UID="$(id -u)"
CONTROL_PLANE_ROOT="$ROOT"
# shellcheck source=tests/helpers/source-control-plane.sh
. "$ROOT/tests/helpers/source-control-plane.sh"
export MOCK_INCUS_ETH0_ADDRESS='2: eth0    inet 10.88.0.42/24 brd 10.88.0.255 scope global dynamic eth0'
[ "$(incus_instance_primary_ipv4 test-project test-yard)" = 10.88.0.42 ] \
  || fail "VM address selection did not use the default-route interface's global IPv4"
reset_case
MOCK_INCUS_EXEC_READY_AFTER=2
incus_wait_instance_agent test-project test-yard || fail "instance agent did not become ready"
[ "$(cat "$MOCK_INCUS_EXEC_COUNT")" = 2 ] || fail "instance agent wait did not retry"

reset_case
started=$SECONDS
if MOCK_INCUS_EXEC_HANG=1 SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=1 \
  incus_wait_instance_agent test-project test-yard; then
  fail "hung instance agent probe returned success"
fi
[ "$((SECONDS - started))" -le 2 ] || fail "hung instance agent probe exceeded its deadline"

reset_case
MOCK_UID=0
guard_conf="$tmp/NetworkManager/conf.d/zz-subyard-unmanaged.conf"
umask 077
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null
[ "$(stat -c '%a' "$guard_conf")" = 644 ] || fail "NetworkManager drop-in mode is not 0644"
[ "$(wc -l < "$MOCK_SYSTEMCTL_LOG")" -eq 1 ] || fail "new guard was not reloaded"
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null
[ "$(wc -l < "$MOCK_SYSTEMCTL_LOG")" -eq 2 ] || fail "unchanged guard did not retry reload"

reset_case
MOCK_UID=0 MOCK_NM_CONFIG_FILE="$guard_conf"
MOCK_INCUS_NETWORKS=$'incusbr0,bridge\n'
nm_unmanaged_guard nestedbr0 "$guard_conf" >/dev/null
grep -Fq 'interface-name:incusbr0' "$guard_conf" \
  || fail "guard omitted the surviving default bridge"
grep -Fq 'interface-name:nestedbr0' "$guard_conf" \
  || fail "guard omitted the requested nested bridge"
MOCK_INCUS_NETWORKS=$'incusbr0,bridge\n'
nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null
grep -Fq 'interface-name:incusbr0' "$guard_conf" \
  || fail "reconcile removed the surviving default bridge"
if grep -Fq 'interface-name:nestedbr0' "$guard_conf"; then
  fail "reconcile retained a deleted nested bridge"
fi
guard_before="$(sha256sum "$guard_conf" | awk '{print $1}')"
MOCK_INCUS_NETWORK_RC=7
if nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null 2>&1; then
  fail "reconcile accepted a failed Incus bridge inventory"
fi
[ "$(sha256sum "$guard_conf" | awk '{print $1}')" = "$guard_before" ] \
  || fail "failed inventory changed the fail-closed guard"
mv "$tmp/bin/incus" "$tmp/bin/incus.off"
if nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null 2>&1; then
  fail "reconcile accepted a missing Incus inventory command"
fi
[ "$(sha256sum "$guard_conf" | awk '{print $1}')" = "$guard_before" ] \
  || fail "missing Incus inventory changed the fail-closed guard"
mv "$tmp/bin/incus.off" "$tmp/bin/incus"
MOCK_INCUS_NETWORK_RC=0 MOCK_INCUS_NETWORKS=
nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null
[ ! -e "$guard_conf" ] || fail "final bridge removal retained the guard"

reset_case
MOCK_UID=0 MOCK_NM_STATE=error
if (nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then
  fail "setup accepted an unknown NetworkManager state"
fi

reset_case
MOCK_UID=0 MOCK_RELOAD_RC=1 MOCK_NMCLI_RC=1
if (nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then
  fail "setup accepted a failed NetworkManager reload"
fi

if grep -Fq 'incus launch' "$ROOT/scripts/03-create-subyard.sh"; then
  fail "instance creation still bypasses the guarded start"
fi
srv_line="$(grep -nF 'incus config device add "$YARD_INSTANCE_NAME" srv disk' "$ROOT/scripts/03-create-subyard.sh" | cut -d: -f1)"
start_line="$(grep -nF 'power_start_guarded "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" "$BRIDGE"' "$ROOT/scripts/03-create-subyard.sh" | cut -d: -f1)"
[ "$srv_line" -lt "$start_line" ] || fail "VM /srv can still be hot-added after the first start"
grep -Fq 'incus_wait_instance_agent "$INCUS_PROJECT" "$YARD_INSTANCE_NAME"' \
  "$ROOT/scripts/lifecycle-guard.sh" || fail "yard start can return before the VM agent is ready"

printf 'ok: NetworkManager power guard\n'

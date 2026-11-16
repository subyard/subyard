#!/usr/bin/env bash
# NetworkManager privilege and fail-closed power checks.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

tmp="$(mktemp -d /tmp/subyard-power-network.XXXXXX)"
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
export MOCK_NMCLI_LOG="$tmp/nmcli.log" MOCK_NMCLI_PINS="$tmp/nmcli.pins"

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
  reload|restart|try-restart|reload-or-restart) printf '%s\n' "$*" >> "$MOCK_SYSTEMCTL_LOG"; exit "${MOCK_RELOAD_RC:-0}" ;;
  show) printf '%s\n' "${MOCK_NM_INVOCATION-invocation-1}" ;;
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
dir="${MOCK_NM_CONF_DIR:-}"
if [ "${1:-}" = --config-dir ]; then
  dir="$2"; shift 2
  [ "${MOCK_NM_PROPOSED_RC:-0}" = 0 ] || exit "$MOCK_NM_PROPOSED_RC"
else
  [ "${MOCK_NM_REQUIRE_ROOT:-1}" = 0 ] || [ "${MOCK_UID:-1000}" = 0 ] \
    || [ "${MOCK_NM_PRIVILEGED:-0}" = 1 ] || exit 7
  n=0
  [ ! -f "$MOCK_NM_COUNT" ] || n="$(cat "$MOCK_NM_COUNT")"
  n=$((n + 1)); printf '%s\n' "$n" > "$MOCK_NM_COUNT"
  printf '%s:%s\n' "$n" "${MOCK_NM_PRIVILEGED:-direct}" >> "$MOCK_NM_LOG"
fi
[ "${1:-}" = --print-config ] || exit 90
mode="${MOCK_NM_MODE:-valid}"
[ "$mode" != valid-then-invalid ] || { if [ "$n" -eq 1 ]; then mode=valid; else mode=invalid; fi; }
[ "$mode" != fail ] || exit 7
if [ -n "$dir" ]; then
  # Like NetworkManager: one merged [keyfile] list, then [device*] sections by priority.
  exec python3 -I - "$dir" <<'PY'
import os, sys
directory = sys.argv[1]
files = sorted(name for name in os.listdir(directory) if name.endswith(".conf")) if os.path.isdir(directory) else []
listed, sections = None, []
for name in files:
    section = None
    for line in open(os.path.join(directory, name)):
        line = line.strip()
        if line.startswith("["):
            section = [line, []]; sections.append(section); continue
        if not section or "=" not in line or line.startswith("#"):
            continue
        key, value = line.split("=", 1)
        if section[0] == "[keyfile]" and key.rstrip("+") == "unmanaged-devices":
            listed = (listed + "," + value) if key.endswith("+") and listed else value
            continue
        section[1].append(line)
print("[main]\nplugins=keyfile")
if listed is not None:
    print("[keyfile]\nunmanaged-devices=" + listed.replace(";", ","))
for header, lines in reversed(sections):
    if header.startswith("[device"):
        print(header); print("\n".join(lines))
PY
fi
if [ "$mode" = valid ]; then
  cat <<'CFG'
[keyfile]
unmanaged-devices=interface-name:enp5s0
[device-subyard]
match-device=type:veth,driver:veth,interface-name:tap????????,interface-name:incusbr0
managed=0
CFG
else
  printf '[main]\nno-auto-default=none\n'
fi
SH
cat > "$tmp/bin/ip" <<'SH'
#!/usr/bin/env bash
case "$*" in
  '-4 route show default') printf '%s' "${MOCK_DEFAULT_ROUTES:-}"; exit 0 ;;
  "-d -o link show dev "*)
    device="${*: -1}"
    case " ${MOCK_VETHS:-} " in *" $device "*) printf '9: %s@if8: <UP> mtu 1500 veth\n' "$device" ;; *) printf '2: %s: <UP>\n' "$device" ;; esac ;;
  *) exit 1 ;;
esac
SH
cat > "$tmp/bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  list) printf '%s,STOPPED\n' "$2" ;;
  network)
    [ "${2:-}" = list ] || exit 90
    [ "${MOCK_INCUS_NETWORK_RC:-0}" = 0 ] || exit "$MOCK_INCUS_NETWORK_RC"
    printf '%s' "${MOCK_INCUS_NETWORKS:-[]}"
    ;;
  start) printf 'start\n' >> "$MOCK_INCUS_LOG" ;;
  stop) printf 'stop\n' >> "$MOCK_INCUS_LOG"; exit "${MOCK_INCUS_STOP_RC:-0}" ;;
  exec)
    if [ "${MOCK_INCUS_EXEC_HANG:-0}" = 1 ]; then
      /bin/sleep 30
      exit 1
    fi
    if [[ " $* " = *" -- sh -eu -c "* ]]; then
      [ -z "${MOCK_INCUS_EXEC_DELAY:-}" ] || /bin/sleep "$MOCK_INCUS_EXEC_DELAY"
      # The default route appears after MOCK_INCUS_ROUTE_AFTER polls.
      n=0; [ ! -f "$MOCK_INCUS_ROUTE_COUNT" ] || n="$(cat "$MOCK_INCUS_ROUTE_COUNT")"
      n=$((n + 1)); printf '%s\n' "$n" > "$MOCK_INCUS_ROUTE_COUNT"
      if [ "$n" -lt "${MOCK_INCUS_ROUTE_AFTER:-1}" ]; then
        printf 'route none\n'
      else
        printf 'route eth0\n%s\n' "${MOCK_INCUS_ETH0_ADDRESS:-}"
      fi
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
printf '%s\n' "$*" >> "$MOCK_NMCLI_LOG"
pinned() { [ -f "$MOCK_NMCLI_PINS" ] && grep -Fxq -- "$1" "$MOCK_NMCLI_PINS"; }
case "$*" in
  "-t -f DEVICE,STATE device"|"-t -f DEVICE,TYPE,STATE device")
    [ "${MOCK_NMCLI_READ_RC:-0}" = 0 ] || exit "$MOCK_NMCLI_READ_RC"
    while IFS=: read -r device type state; do
      [ -n "$device" ] || continue
      ! pinned "$device" || state=unmanaged
      if [ "$3" = DEVICE,STATE ]; then printf '%s:%s\n' "$device" "$state"; else printf '%s:%s:%s\n' "$device" "$type" "$state"; fi
    done <<< "${MOCK_NM_DEVICES:-}" ;;
  "device set "*" managed no")
    [ "${MOCK_PIN_RC:-0}" = 0 ] || exit "$MOCK_PIN_RC"
    [ "${MOCK_PIN_NOEFFECT:-0}" = 1 ] || printf '%s\n' "$3" >> "$MOCK_NMCLI_PINS" ;;
  "-g GENERAL.STATE device show "*)
    device="$5"
    if pinned "$device"; then printf '10 (unmanaged)\n'; exit 0; fi
    awk -F: -v d="$device" '$1 == d { print ($3 == "unmanaged" ? "10 (unmanaged)" : "100 (" $3 ")") }' <<< "${MOCK_NM_DEVICES:-}" ;;
  # Subyard never reloads or restarts NetworkManager: any such call fails the suite.
  general\ reload*|networking\ *) printf 'FORBIDDEN %s\n' "$*" >> "$MOCK_NMCLI_LOG"; exit 0 ;;
  *) exit 90 ;;
esac
SH
chmod +x "$tmp/bin/"*
export PATH="$tmp/bin:$PATH"

# shellcheck source=scripts/lib-power.sh
# shellcheck disable=SC1091
. "$ROOT/scripts/lib-power.sh"
power_nm_binary() { printf '%s\n' "$tmp/bin/NetworkManager"; }
power_operator_terminal() { [ "$MOCK_TERMINAL" = 1 ]; }

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
  : > "$MOCK_NMCLI_LOG"
  rm -f "$MOCK_NMCLI_PINS"
  rm -f "$MOCK_NM_COUNT" "$MOCK_SUDO_AUTH" "$MOCK_INCUS_EXEC_COUNT"
  POWER_ERROR=''
  unset SUBYARD_SUDO_PREAUTHORIZED
  export MOCK_TERMINAL=1 MOCK_NM_STATE=active MOCK_UID=1000 MOCK_SUDO_V_RC=0 MOCK_SUDO_N_RC=0 \
    MOCK_SUDO_REQUIRE_V=1 MOCK_NM_MODE=valid MOCK_RELOAD_RC=0 MOCK_NMCLI_RC=1 \
    MOCK_INCUS_STOP_RC=0 MOCK_INCUS_EXEC_READY_AFTER=1 MOCK_INCUS_EXEC_HANG=0 \
    MOCK_INCUS_NETWORK_RC=0 MOCK_INCUS_NETWORKS='[]' MOCK_NM_CONF_DIR='' MOCK_NM_REQUIRE_ROOT=1 \
    MOCK_NM_PROPOSED_RC=0 MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nincusbr0:bridge:unmanaged' \
    MOCK_VETHS='' MOCK_DEFAULT_ROUTES='' MOCK_NM_INVOCATION=invocation-1 \
    MOCK_NMCLI_READ_RC=0 MOCK_PIN_RC=0 MOCK_PIN_NOEFFECT=0
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

# NOPASSWD:ALL beside a password-requiring group rule: sudo -n works while sudo -v cannot.
reset_case
MOCK_SUDO_REQUIRE_V=0 MOCK_SUDO_V_RC=1 MOCK_TERMINAL=0
power_nm_prepare_reader || fail "passwordless sudo was refused: $POWER_ERROR"
[ "$SUBYARD_SUDO_PREAUTHORIZED" = 1 ] || fail "passwordless sudo did not record preauthorization"
grep -Fxq -- '-v' "$MOCK_SUDO_LOG" && fail "passwordless sudo fell back to sudo -v"
power_nm_guard_effective incusbr0 || fail "passwordless privileged read failed"

reset_case
MOCK_TERMINAL=0
if power_nm_prepare_reader; then fail "password-required sudo without a terminal was accepted"; fi
case "$POWER_ERROR" in *"operator terminal"*) ;; *) fail "no-terminal sudo refusal lacks a next step" ;; esac
grep -Fxq -- '-v' "$MOCK_SUDO_LOG" && fail "no-terminal sudo attempted a password prompt"
[ "${SUBYARD_SUDO_PREAUTHORIZED:-0}" != 1 ] || fail "no-terminal refusal recorded preauthorization"

# The real /dev/tty probe: setsid leaves no controlling terminal.
reset_case
if output="$(ROOT="$ROOT" setsid -w bash -c '
  . "$ROOT/scripts/lib-power.sh"
  power_nm_prepare_reader || { printf "%s\n" "$POWER_ERROR"; exit 1; }
' </dev/null 2>&1)"; then fail "sudo without a controlling terminal was accepted"; fi
case "$output" in *"required to verify"*"operator terminal"*) ;; *) fail "terminal probe refusal is unclear: $output" ;; esac
grep -Fxq -- '-v' "$MOCK_SUDO_LOG" && fail "terminal probe attempted a password prompt"

reset_case
SUBYARD_SUDO_PREAUTHORIZED=1
if power_nm_prepare_reader; then fail "expired preauthorized sudo was accepted"; fi
case "$POWER_ERROR" in *"authorization expired"*"yard command"*) ;; *) fail "expired sudo refusal is unclear: $POWER_ERROR" ;; esac
grep -Fxq -- '-v' "$MOCK_SUDO_LOG" && fail "expired preauthorized sudo attempted a password prompt"

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
[ ! -s "$MOCK_SUDO_LOG" ] || fail "unprepared NetworkManager read invoked sudo"

reset_case
: > "$MOCK_SUDO_AUTH"
if power_nm_guard_effective incusbr0; then fail "cached sudo bypassed missing operation authorization"; fi
[ ! -s "$MOCK_SUDO_LOG" ] || fail "read-only planning used cached sudo credentials"

reset_case
MOCK_NM_REQUIRE_ROOT=0
power_nm_guard_effective incusbr0 || fail "readable effective configuration required sudo"
[ ! -s "$MOCK_SUDO_LOG" ] || fail "read-only effective configuration invoked sudo"
grep -Fq '1:direct' "$MOCK_NM_LOG" || fail "read-only configuration did not use the direct reader"

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

for engine_case in unset missing nonexecutable; do
  reset_case
  MOCK_NM_STATE=inactive
  VM_CPU_WEIGHT=1000
  case "$engine_case" in
    unset) SUBYARD_DISPATCHER_PATH='' ;;
    missing) SUBYARD_DISPATCHER_PATH="$tmp/missing-dispatcher" ;;
    nonexecutable)
      SUBYARD_DISPATCHER_PATH="$tmp/nonexecutable-dispatcher"
      printf '#!/bin/sh\nexit 0\n' > "$SUBYARD_DISPATCHER_PATH"
      chmod 0600 "$SUBYARD_DISPATCHER_PATH"
      ;;
  esac
  if power_start_guarded test-project test-yard incusbr0; then
    fail "unavailable CPU scheduling engine was accepted: $engine_case"
  fi
  [ "$(cat "$MOCK_INCUS_LOG")" = $'start\nstop' ] \
    || fail "unavailable CPU scheduling engine left the VM running: $engine_case"
  case "$POWER_ERROR" in *"engine is unavailable"*"was stopped fail-closed"*) ;; *)
    fail "CPU scheduling engine failure lost its diagnostic: $engine_case" ;;
  esac
done
reset_case
MOCK_NM_STATE=inactive MOCK_INCUS_STOP_RC=1
if power_start_guarded test-project test-yard incusbr0; then
  fail 'CPU scheduling failure with failed stop returned success'
fi
case "$POWER_ERROR" in *"engine is unavailable"*"FAILED to stop"*) ;; *)
  fail 'CPU scheduling stop failure was reported as success' ;;
esac
unset VM_CPU_WEIGHT SUBYARD_DISPATCHER_PATH

# shellcheck disable=SC2034 # consumed by sourced config module
SUBYARD_CONFIG_LOADED=1
DEV_UID="$(id -u)"
CONTROL_PLANE_ROOT="$ROOT"
# shellcheck source=tests/helpers/source-control-plane.sh
. "$ROOT/tests/helpers/source-control-plane.sh"
export MOCK_INCUS_ETH0_ADDRESS='2: eth0    inet 10.88.0.42/24 brd 10.88.0.255 scope global dynamic eth0'
export MOCK_INCUS_ROUTE_COUNT="$tmp/route-count"
[ "$(incus_instance_primary_ipv4 test-project test-yard)" = 10.88.0.42 ] \
  || fail "VM address selection did not use the default-route interface's global IPv4"
# After a restart the default route appears only after some polls.
(
  sleep() { :; }
  rm -f "$MOCK_INCUS_ROUTE_COUNT"
  [ "$(MOCK_INCUS_ROUTE_AFTER=4 incus_instance_primary_ipv4 test-project test-yard)" = 10.88.0.42 ] \
    || fail 'VM address selection did not wait for the default route'
  [ "$(cat "$MOCK_INCUS_ROUTE_COUNT")" = 4 ] || fail 'VM address selection did not poll until the route appeared'
) || exit 1
# A route that never appears fails closed at the deadline, naming the observation.
(
  # Unset SECONDS stops following wall time, so only the mocked sleep moves the clock.
  unset SECONDS; SECONDS=0
  sleep() { SECONDS=$((SECONDS + 1)); }
  rm -f "$MOCK_INCUS_ROUTE_COUNT"
  if MOCK_INCUS_ROUTE_AFTER=1000 SUBYARD_VM_IPV4_TIMEOUT=5 incus_instance_primary_ipv4 test-project test-yard 2> "$tmp/ipv4.err"; then
    fail 'VM without a default route returned an address'
  fi
  grep -Fxq 'VM test-project/test-yard has no primary IPv4 address after 5s: no default IPv4 route' "$tmp/ipv4.err" \
    || fail "missing default route lost its diagnostic: $(cat "$tmp/ipv4.err")"
  rm -f "$MOCK_INCUS_ROUTE_COUNT"
  if MOCK_INCUS_ETH0_ADDRESS='' SUBYARD_VM_IPV4_TIMEOUT=2 incus_instance_primary_ipv4 test-project test-yard 2> "$tmp/ipv4.err"; then
    fail 'VM without a global address returned an address'
  fi
  grep -Fq 'default route on eth0 without a global IPv4 address' "$tmp/ipv4.err" || fail 'missing address lost its diagnostic'
) || exit 1
# A probe slower than the remaining budget is cut to it; its late answer is not
# accepted and the reported duration is the measured one.
rm -f "$MOCK_INCUS_ROUTE_COUNT"
ipv4_started=$SECONDS
if MOCK_INCUS_EXEC_DELAY=3 SUBYARD_VM_IPV4_TIMEOUT=1 incus_instance_primary_ipv4 test-project test-yard 2> "$tmp/ipv4.err"; then
  fail 'VM address answered after the deadline was accepted'
fi
ipv4_elapsed=$((SECONDS - ipv4_started))
ipv4_reported="$(sed -n 's|^VM test-project/test-yard has no primary IPv4 address after \([0-9]*\)s: .*|\1|p' "$tmp/ipv4.err")"
[ -n "$ipv4_reported" ] && [ "$ipv4_reported" -le "$ipv4_elapsed" ] && [ "$ipv4_elapsed" -le $((ipv4_reported + 1)) ] \
  || fail "VM address wait misreported its duration ($ipv4_elapsed s): $(cat "$tmp/ipv4.err")"
reset_case
MOCK_INCUS_EXEC_READY_AFTER=2
incus_wait_instance_agent test-project test-yard || fail "instance agent did not become ready"
[ "$(cat "$MOCK_INCUS_EXEC_COUNT")" = 2 ] || fail "instance agent wait did not retry"

# Simulate a healthy agent arriving after the container deadline, without waiting minutes.
(
  # Unset SECONDS stops following wall time, so only the mocked sleep moves the clock.
  unset SECONDS; SECONDS=0
  sleep() { SECONDS=$((SECONDS + 130)); }
  unset SUBYARD_INCUS_AGENT_WAIT_TIMEOUT
  for kind in vm container; do
    reset_case
    MOCK_INCUS_EXEC_READY_AFTER=2
    if YARD_KIND="$kind" incus_wait_instance_agent test-project test-yard; then
      [ "$kind" = vm ] || fail "container exceeded its agent deadline"
    else
      [ "$kind" = container ] || fail "slow VM agent was rejected"
    fi
  done
  reset_case
  MOCK_INCUS_EXEC_READY_AFTER=2
  if YARD_KIND=vm SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=120 \
    incus_wait_instance_agent test-project test-yard; then
    fail "VM agent wait ignored the explicit deadline"
  fi
)

# Cloud-image seeding can require two boots before the first executable agent.
(
  probe_limits="$tmp/vm-agent-probe-limits"
  timeout() {
    printf '%s\n' "$2" >> "$probe_limits"
    shift 2
    "$@"
  }
  # Unset SECONDS stops following wall time, so only the mocked sleep moves the clock.
  unset SECONDS; SECONDS=0
  sleep() { SECONDS=$((SECONDS + arrival)); }
  unset SUBYARD_INCUS_AGENT_WAIT_TIMEOUT
  for arrival in 301 599 600; do
    reset_case
    : > "$probe_limits"
    SECONDS=0
    MOCK_INCUS_EXEC_READY_AFTER=2
    if YARD_KIND=vm incus_wait_instance_agent test-project test-yard; then
      [ "$arrival" -lt 600 ] || fail "VM agent exceeded its absolute deadline"
    else
      [ "$arrival" -eq 600 ] || fail "cloud VM agent was rejected before its deadline"
    fi
    [ "$(head -n 1 "$probe_limits")" = 5 ] || fail "VM agent probe exceeded five seconds"
    if [ "$arrival" -eq 599 ]; then
      [ "$(tail -n 1 "$probe_limits")" = 1 ] || fail "VM agent probe exceeded remaining time"
    elif [ "$arrival" -eq 600 ]; then
      [ "$(cat "$MOCK_INCUS_EXEC_COUNT")" = 1 ] || fail "VM agent deadline started another probe"
    fi
  done
)

reset_case
started=$SECONDS
if MOCK_INCUS_EXEC_HANG=1 SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=1 \
  incus_wait_instance_agent test-project test-yard; then
  fail "hung instance agent probe returned success"
fi
[ "$((SECONDS - started))" -le 2 ] || fail "hung instance agent probe exceeded its deadline"

# --- NetworkManager guard: shared rule vectors (also run by the Go hostruntime tests) ---
for vector in "$ROOT"/tests/fixtures/nm-guard-vectors/*/; do
  read -r -a vector_bridges < "$vector/bridges" || vector_bridges=()
  got="$(power_nm_config_violations "${vector_bridges[@]}" < "$vector/config")"
  [ "$got" = "$(cat "$vector/violations")" ] || fail "guard vector $(basename "$vector"): $got"
done

conf_dir="$tmp/NetworkManager/conf.d"
guard_conf="$conf_dir/zz-subyard-unmanaged.conf"
legacy_conf="$conf_dir/99-subyard-unmanaged.conf"
mkdir -p "$conf_dir" "$tmp/run"
POWER_NM_REBOOT_PENDING="$tmp/run/nm-guard-reboot-pending"
printf 'boot-1\n' > "$tmp/boot_id"
power_nm_boot_id() { cat "$tmp/boot_id"; }
incus_networks='[{"name":"incusbr0","type":"bridge","managed":true},{"name":"nestedbr0","type":"bridge","managed":true},{"name":"br0","type":"bridge","managed":false},{"name":"docker0","type":"bridge","managed":false},{"name":"incusnet1","type":"ovn","managed":true}]'
guard_case() {
  reset_case
  export MOCK_UID=0 MOCK_NM_CONF_DIR="$conf_dir" MOCK_INCUS_NETWORKS="$incus_networks"
  export MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nenp5s0:ethernet:connected\nincusbr0:bridge:unmanaged\nbr0:bridge:connected'
}
hash_of() { if [ -e "$1" ]; then sha256sum "$1" | cut -d ' ' -f1; else printf absent; fi; }
pending_kind() { power_nm_guard_pending; }
no_reload() { # NetworkManager is never reloaded or restarted, by any path.
  ! grep -q '^FORBIDDEN' "$MOCK_NMCLI_LOG" && [ ! -s "$MOCK_SYSTEMCTL_LOG" ] \
    || fail "$1 reloaded or restarted NetworkManager: $(cat "$MOCK_NMCLI_LOG" "$MOCK_SYSTEMCTL_LOG")"
}
expected_guard='# Subyard: keep yard veths, Incus tap devices and Incus bridges unmanaged.
[device-subyard]
match-device=type:veth,driver:veth,interface-name:tap????????,interface-name:incusbr0,interface-name:nestedbr0
managed=0'

# First write with NetworkManager running: the record says `unread` before the file exists, the
# exact guard is written, and init stops until NetworkManager starts; starts refuse meanwhile.
guard_case
rm -f "$guard_conf" "$POWER_NM_REBOOT_PENDING"
umask 077
original_write_file="$(declare -f nm_guard_write_file)"
nm_guard_write_file() {
  [ "$1" != "$guard_conf" ] || [ "$(cut -d ' ' -f3 "$POWER_NM_REBOOT_PENDING" 2>/dev/null)" = unread ] \
    || fail "the guard was written before its unread record"
  temporary="${1%/*}/.${1##*/}.$$.tmp"; printf '%s\n' "$2" > "$temporary" && chmod 0644 "$temporary" && mv -f "$temporary" "$1"
}
if (POWER_NM_GUARD_COMMAND='yard init' nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out" 2>&1); then
  fail "init continued before NetworkManager read the guard"
fi
eval "$original_write_file"
grep -Fq "Reboot the host or restart NetworkManager ('sudo systemctl restart NetworkManager'), then rerun yard init" "$tmp/guard.out" \
  || fail "the first write did not ask for a reboot or NetworkManager restart: $(cat "$tmp/guard.out")"
[ "$(cat "$guard_conf")" = "$expected_guard" ] || fail "guard bytes for two Incus bridges: $(cat "$guard_conf")"
[ "$(stat -c '%a' "$guard_conf")" = 644 ] || fail "NetworkManager drop-in mode is not 0644"
[ "$(cat "$POWER_NM_REBOOT_PENDING")" = 'boot-1 invocation-1 unread' ] || fail "unread record: $(cat "$POWER_NM_REBOOT_PENDING")"
! ls "$conf_dir"/.*tmp >/dev/null 2>&1 || fail "atomic write left a temporary file"
grep -Fq 'device set' "$MOCK_NMCLI_LOG" && fail "the unread first write pinned a device"
if nm_guard_file_current "$guard_conf" incusbr0; then fail "planning accepted an unread guard"; fi
if power_host_safe incusbr0; then fail "a start was accepted before NetworkManager read the guard"; fi
case "$POWER_ERROR" in *'has not read the Subyard guard'*'restart NetworkManager'*) ;; *) fail "unread start refusal: $POWER_ERROR" ;; esac
# A rerun before NetworkManager starts stops again without changes.
if (nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "a rerun continued while the guard is unread"; fi
[ "$(pending_kind)" = unread ] || fail "the rerun lost the unread record"
# NetworkManager restarted, or the host rebooted: the guard is read and init continues.
MOCK_NM_INVOCATION="invocation-2"
[ -z "$(pending_kind)" ] || fail "a NetworkManager restart did not clear the unread record"
nm_guard_file_current "$guard_conf" incusbr0 || fail "the read guard reported drift"
power_host_safe incusbr0 || fail "a start was refused after NetworkManager read the guard: $POWER_ERROR"
nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out"
grep -Fq 'already current' "$tmp/guard.out" || fail "steady state was not reported"
MOCK_NM_INVOCATION="invocation-1"
printf 'boot-2\n' > "$tmp/boot_id"
[ -z "$(pending_kind)" ] || fail "a new boot did not clear the unread record"
printf 'boot-1\n' > "$tmp/boot_id"
rm -f "$POWER_NM_REBOOT_PENDING"
no_reload 'the first write'

# A guard NetworkManager has read: a new bridge rewrites the file and pins only own bridges that
# NetworkManager still manages, verifying each; a failed pin refuses.
guard_case
MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nenp5s0:ethernet:connected\nincusbr0:bridge:unmanaged\nnestedbr0:bridge:connected (externally)\nbr0:bridge:connected'
MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true},{"name":"nestedbr0","type":"bridge","managed":true}]'
nm_guard_write_file "$guard_conf" "$(nm_guard_render incusbr0)"
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null
[ "$(cat "$guard_conf")" = "$expected_guard" ] || fail "the new bridge was not added"
[ "$(cat "$MOCK_NMCLI_PINS")" = nestedbr0 ] || fail "pins: $(cat "$MOCK_NMCLI_PINS" 2>/dev/null)"
[ -z "$(pending_kind)" ] || fail "a read guard wrote a pending record"
rm -f "$MOCK_NMCLI_PINS"
if (MOCK_PIN_RC=1 nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "a failed pin was accepted"; fi
if (MOCK_PIN_NOEFFECT=1 nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/pin.out" 2>&1); then fail "an ineffective pin was accepted"; fi
grep -Fq 'still manages the yard bridge nestedbr0' "$tmp/pin.out" || fail "ineffective pin unreported: $(cat "$tmp/pin.out")"
no_reload 'a new bridge'

# Every refusal is reported together; nothing is written.
guard_case
nm_guard_write_file "$guard_conf" "$(nm_guard_render incusbr0)"
before_hash="$(hash_of "$guard_conf")"
printf '[device-later]\nmatch-device=type:veth\nmanaged=1\n' > "$conf_dir/zzz-admin.conf"
MOCK_NM_DEVICES=$'veth1234:ethernet:connected\nincusbr0:bridge:unmanaged\nnestedbr0:bridge:unmanaged'
MOCK_VETHS=veth1234 MOCK_DEFAULT_ROUTES=$'default via 192.0.2.1 dev nestedbr0\n'
if nm_guard_apply "$guard_conf" write $'incusbr0\nnestedbr0' >/dev/null; then fail "unsafe guard change was applied"; fi
for expected in '[device-later] has priority over [device-subyard]' \
  'NetworkManager manages veth1234, which the guard covers; release it first: sudo nmcli device set veth1234 managed no' \
  'the host default route uses nestedbr0' 'then rerun yard init'; do
  case "$POWER_ERROR" in *"$expected"*) ;; *) fail "refusal lacks '$expected': $POWER_ERROR" ;; esac
done
[ "$(hash_of "$guard_conf")" = "$before_hash" ] || fail "a refusal changed the guard"
rm -f "$conf_dir/zzz-admin.conf"
MOCK_VETHS='' MOCK_DEFAULT_ROUTES=''
if (MOCK_NM_PROPOSED_RC=7 nm_guard_apply "$guard_conf" write nestedbr0 >/dev/null); then fail "an invalid proposal was applied"; fi
MOCK_NM_PROPOSED_RC=7 nm_guard_apply "$guard_conf" write nestedbr0 >/dev/null || true
case "$POWER_ERROR" in *'NetworkManager rejects the proposed configuration'*) ;; *) fail "invalid proposal unreported" ;; esac
[ "$(hash_of "$guard_conf")" = "$before_hash" ] || fail "an invalid proposal changed the guard"
# Failed nmcli reads refuse; they never stand for an empty inventory.
if (MOCK_NMCLI_READ_RC=10 nm_guard_apply "$guard_conf" write $'incusbr0\nnestedbr0' >/dev/null 2>&1); then fail "a failed device read was accepted"; fi
MOCK_NM_DEVICES='' nm_guard_apply "$guard_conf" write $'incusbr0\nnestedbr0' >/dev/null && fail "an empty device list was accepted"
case "$POWER_ERROR" in *"cannot read NetworkManager's devices"*) ;; *) fail "empty read unreported: $POWER_ERROR" ;; esac
if MOCK_NMCLI_READ_RC=10 power_nm_guard_effective incusbr0; then fail "the validator accepted a failed device read"; fi
[ "$(hash_of "$guard_conf")" = "$before_hash" ] || fail "a failed read changed the guard"
no_reload 'refusals'

# NetworkManager inactive: the file is written for its next start, with no record and no pins.
guard_case
MOCK_NM_STATE=inactive
rm -f "$guard_conf"
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null
[ -f "$guard_conf" ] && [ ! -e "$POWER_NM_REBOOT_PENDING" ] || fail "inactive guard write is wrong"
grep -Fq 'device set' "$MOCK_NMCLI_LOG" && fail "an inactive NetworkManager was asked to pin"
nm_guard_file_current "$guard_conf" incusbr0 || fail "inactive guard reported drift"
no_reload 'an inactive NetworkManager'

# Teardown keeps the guard for surviving Incus bridges and removes it after the last one,
# dropping an unread record with the file.
guard_case
MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true},{"name":"nestedbr0","type":"bridge","managed":true}]'
nm_guard_write_file "$guard_conf" "$(nm_guard_render incusbr0)"
nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null
grep -Fq 'interface-name:nestedbr0' "$guard_conf" || fail "teardown dropped a surviving bridge"
before_hash="$(hash_of "$guard_conf")"
if (MOCK_INCUS_NETWORK_RC=7 nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null 2>&1); then fail "teardown accepted a failed inventory"; fi
[ "$(hash_of "$guard_conf")" = "$before_hash" ] || fail "a failed inventory changed the guard"
printf 'boot-1 invocation-1 unread\n' > "$POWER_NM_REBOOT_PENDING"
MOCK_INCUS_NETWORKS='[{"name":"br0","type":"bridge","managed":false}]' nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null
[ ! -e "$guard_conf" ] && [ ! -e "$POWER_NM_REBOOT_PENDING" ] || fail "teardown kept the guard or its unread record"
no_reload 'teardown'

# Migration: a provable takeover refuses with nothing changed; the hand-back then migrates without
# touching NetworkManager, recording `lists`, which blocks nothing.
guard_case
released_spec='type:veth;driver:veth;interface-name:veth*;interface-name:docker*;interface-name:br-*;interface-name:virbr*;interface-name:vnet*;interface-name:tap*;interface-name:macvtap*;interface-name:incusbr0'
write_released() {
  printf '[main]\nno-auto-default=%s\n\n[keyfile]\nunmanaged-devices=%s\n\n[device-subyard]\nmatch-device=%s\nmanaged=0\n' \
    "$released_spec" "$released_spec" "${released_spec//;/,}" > "$guard_conf"
  printf '[keyfile]\nunmanaged-devices=interface-name:legacybr0\n' > "$legacy_conf"
}
migration_refuses() { # <admin list> <description>
  write_released
  printf '[keyfile]\nunmanaged-devices=%s\n' "$1" > "$conf_dir/00-admin.conf"
  before_hash="$(hash_of "$guard_conf")"
  if (POWER_NM_GUARD_COMMAND='yard update' nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out" 2>&1); then
    fail "migration applied with a hijacked device ($2)"
  fi
  grep -Fq "NetworkManager manages enp6s0, which the administrator's unmanaged-devices list marks unmanaged" "$tmp/migrate.out" \
    && grep -Fq "'sudo nmcli device set enp6s0 managed no' (or the administrator's own way)" "$tmp/migrate.out" \
    && grep -Fq 'then rerun yard update' "$tmp/migrate.out" || fail "hijack refusal ($2): $(cat "$tmp/migrate.out")"
  [ "$(hash_of "$guard_conf")" = "$before_hash" ] && [ -e "$legacy_conf" ] && [ ! -e "$POWER_NM_REBOOT_PENDING" ] \
    || fail "a refused migration changed the host ($2)"
}
MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nenp6s0:ethernet:connected\nincusbr0:bridge:unmanaged'
write_released
power_nm_guard_effective incusbr0 || fail "the released guard failed the rule before its migration: $POWER_ERROR"
migration_refuses 'interface-name:enp6s0' 'exact name'
migration_refuses '*' 'match-all'
migration_refuses 'except:interface-name:wlan0' 'except-only'
migration_refuses 'type:ethernet' 'type'
migration_refuses '*,except:type:wifi,except:type:gsm,except:type:cdma' 'Ubuntu stock list'
# Lists Subyard cannot decide migrate with a notice naming the list and the managed devices.
for undecided in 'interface-name:docker*;interface-name:virbr*' 'mac:00:11:22:33:44:55'; do
  write_released
  printf '[keyfile]\nunmanaged-devices=%s\n' "$undecided" > "$conf_dir/00-admin.conf"
  nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out" 2>&1 || fail "an undecidable list refused the migration: $(cat "$tmp/migrate.out")"
  grep -Fq "cannot decide whether the administrator's unmanaged-devices list (${undecided//;/,}) marks these devices NetworkManager manages: enp6s0" "$tmp/migrate.out" \
    && grep -Fq 'Do not reload or restart NetworkManager before the reboot' "$tmp/migrate.out" \
    || fail "undecidable-list notice: $(cat "$tmp/migrate.out")"
  rm -f "$POWER_NM_REBOOT_PENDING"
done
# After the hand-back the migration rewrites the guard and asks for the reboot.
write_released
printf '[keyfile]\nunmanaged-devices=interface-name:enp6s0\n' > "$conf_dir/00-admin.conf"
MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nenp6s0:ethernet:unmanaged\nincusbr0:bridge:unmanaged'
nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out"
nm_guard_format_current "$guard_conf" && [ ! -e "$legacy_conf" ] || fail "migration did not rewrite the guard"
[ "$(cat "$POWER_NM_REBOOT_PENDING")" = 'boot-1 invocation-1 lists' ] || fail "lists record: $(cat "$POWER_NM_REBOOT_PENDING")"
grep -Fq 'reboot the host once' "$tmp/migrate.out" || fail "migration did not ask for the reboot"
grep -Fq 'device set' "$MOCK_NMCLI_LOG" && fail "migration pinned a device"
power_host_safe incusbr0 || fail "a lists record blocked the start: $POWER_ERROR"
nm_guard_file_current "$guard_conf" incusbr0 || fail "planning reported drift while the migration waits for the reboot"
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null || fail "init failed while the migration waits for the reboot"
nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out"
grep -Fq 'no released form' "$tmp/migrate.out" || fail "a repeated migration was not a no-op"
power_nm_reboot_pending || fail "the reboot reminder is not pending"
MOCK_NM_INVOCATION=invocation-2 power_nm_reboot_pending && fail "a NetworkManager restart did not clear the reboot reminder"
printf 'boot-2\n' > "$tmp/boot_id"
power_nm_reboot_pending && fail "the reboot reminder survived a new boot"
printf 'boot-1\n' > "$tmp/boot_id"
rm -f "$POWER_NM_REBOOT_PENDING" "$conf_dir/00-admin.conf"
# NetworkManager inactive during the migration: rewrite with no record.
write_released
MOCK_NM_STATE=inactive nm_unmanaged_guard_migrate "$guard_conf" >/dev/null
nm_guard_format_current "$guard_conf" && [ ! -e "$POWER_NM_REBOOT_PENDING" ] || fail "inactive migration is wrong"
no_reload 'the migration'

# NetworkManager device-list semantics: order-independent, exceptions win, types from nmcli.
while IFS='|' read -r list device type want; do
  [ -n "$device" ] || continue
  if nm_list_selects "$list" "$device" "$type"; then got=0; else got=$?; fi
  [ "$got" = "$want" ] || fail "nm_list_selects '$list' $device $type returned $got, want $want"
done < "$ROOT/tests/fixtures/nm-list-selects.txt"

# Pending record kinds match the Go reader.
while IFS='|' read -r record boot invocation want; do
  if [ -n "$record" ]; then printf '%s\n' "$record" > "$POWER_NM_REBOOT_PENDING"; else rm -f "$POWER_NM_REBOOT_PENDING"; fi
  printf '%s\n' "$boot" > "$tmp/boot_id"
  got="$(MOCK_NM_INVOCATION="$invocation" pending_kind)"
  [ "$got" = "$want" ] || fail "pending record '$record' boot=$boot nm=$invocation gave '$got', want '$want'"
done < "$ROOT/tests/fixtures/nm-guard-pending.txt"
printf 'boot-1\n' > "$tmp/boot_id"
rm -f "$POWER_NM_REBOOT_PENDING"

# Guard format classification matches the Go fixtures, dangling links included.
while IFS=' ' read -r name want; do
  cp "$ROOT/tests/fixtures/nm-guard-formats/$name.conf" "$tmp/format.conf"
  if nm_guard_format_current "$tmp/format.conf"; then got=current; else got=released; fi
  [ "$got" = "$want" ] || fail "guard format $name classified $got, want $want"
done < "$ROOT/tests/fixtures/nm-guard-formats/expected"
guard_case
rm -f "$guard_conf" "$legacy_conf"
ln -sfn "$tmp/missing" "$guard_conf"
nm_guard_released "$guard_conf" || fail "a dangling guard link was not treated as released"
rm -f "$guard_conf"
ln -sfn "$tmp/missing" "$legacy_conf"
nm_guard_released "$guard_conf" || fail "a dangling legacy link was not treated as released"
MOCK_NM_STATE=inactive MOCK_INCUS_NETWORKS='[]' nm_unmanaged_guard_migrate "$guard_conf" >/dev/null
nm_path_exists "$legacy_conf" && fail "migration kept the dangling legacy link"
[ "$(sed -n 3p "$guard_conf")" = 'match-device=type:veth,driver:veth,interface-name:tap????????' ] \
  || fail "an empty inventory did not keep the veth and tap entries"

# No NetworkManager on the host: nothing to guard.
guard_case
rm -f "$guard_conf" "$legacy_conf"
(power_nm_binary() { return 1; }; MOCK_NM_STATE=inactive; nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null)
[ ! -e "$guard_conf" ] || fail "a host without NetworkManager got a guard"

# The guard and the route checks both report.
guard_case
MOCK_NM_CONF_DIR='' MOCK_NM_MODE=invalid MOCK_DEFAULT_ROUTES=$'default via 192.0.2.1 dev veth1234\n'
if power_host_safe incusbr0; then fail "an unsafe host was accepted"; fi
case "$POWER_ERROR" in *'no [device-subyard] guard section'*"unsafe host default route uses 'veth1234'"*) ;; *) fail "host safety hid an error: $POWER_ERROR" ;; esac

# Route devices are Subyard's own only.
for device in br-lan br-0123456789ab docker0 virbr0 vnet0 tap0; do
  if power_route_device_unsafe "$device" incusbr0; then fail "$device was treated as a yard device"; fi
done
for device in incusbr0 veth1234 tap1234abcd; do
  power_route_device_unsafe "$device" incusbr0 || fail "$device was accepted as a route device"
done
no_reload 'the guard suite'

if grep -Fq 'incus launch' "$ROOT/scripts/03-create-subyard.sh"; then
  fail "instance creation still bypasses the guarded start"
fi
srv_line="$(grep -nF 'incus config device add "$YARD_INSTANCE_NAME" srv disk' "$ROOT/scripts/03-create-subyard.sh" | cut -d: -f1)"
start_line="$(grep -nF 'power_start_guarded "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" "$BRIDGE"' "$ROOT/scripts/03-create-subyard.sh" | cut -d: -f1 | sed -n '1p')"
[ "$srv_line" -lt "$start_line" ] || fail "VM /srv can still be hot-added after the first start"
grep -Fq 'incus_wait_yard_agent "instance' \
  "$ROOT/scripts/lifecycle-guard.sh" || fail "yard start can return before the VM agent is ready"

printf 'ok: NetworkManager power guard\n'

# Exact bridge rule retirement: preserve another yard's shared probe grant,
# propagate command failures, and reject successful commands leaving drift.
export SUBYARD_UFW_RULES_FILE="$tmp/ufw.rules"
incus() { [ "${TEST_BRIDGE_PRESENT:-0}" = 0 ] && printf '[]\n' || printf '[{"name":"owned"}]\n'; }
ufw_rules_set_probe_access() { printf '%s\n' "$1" >> "$tmp/probe-access"; }
ufw() {
  [ "${TEST_UFW_ERROR:-0}" = 0 ] || return 4
  [ "${TEST_UFW_STALE:-0}" = 0 ] || return 0
  local bridge
  if [ "$2" = route ]; then bridge="$7"; else bridge="$6"; fi
  awk -v bridge="$bridge" '$10 != "in_" bridge && $10 != "out_" bridge' \
    "$SUBYARD_UFW_RULES_FILE" > "$tmp/ufw.next"
  mv "$tmp/ufw.next" "$SUBYARD_UFW_RULES_FILE"
}
write_ufw_fixture() {
  printf '%s\n' \
    '### tuple ### allow udp 67 0.0.0.0/0 any 0.0.0.0/0 in_owned' \
    '### tuple ### allow any 53 0.0.0.0/0 any 0.0.0.0/0 in_owned' \
    '### tuple ### route:allow any any 0.0.0.0/0 any 0.0.0.0/0 in_owned' \
    '### tuple ### route:allow any any 0.0.0.0/0 any 0.0.0.0/0 out_owned' \
    > "$SUBYARD_UFW_RULES_FILE"
  chmod 0600 "$SUBYARD_UFW_RULES_FILE"
}
write_ufw_fixture
TEST_BRIDGE_PRESENT=1
if ufw_yard_rules_remove owned; then fail 'surviving bridge firewall rules were removed'; fi
[ "$(wc -l < "$SUBYARD_UFW_RULES_FILE")" = 4 ] || fail 'surviving bridge rules changed'
TEST_BRIDGE_PRESENT=0
printf '%s\n' '### tuple ### route:allow any any 0.0.0.0/0 any 0.0.0.0/0 in_foreign' >> "$SUBYARD_UFW_RULES_FILE"
ufw_yard_rules_remove owned || fail 'exact bridge rules did not retire'
[ "$(wc -l < "$SUBYARD_UFW_RULES_FILE")" = 1 ] && grep -Fq in_foreign "$SUBYARD_UFW_RULES_FILE" \
  || fail 'foreign bridge firewall policy changed'
[ ! -e "$tmp/probe-access" ] || fail 'surviving yard lost shared probe grant'
write_ufw_fixture
TEST_UFW_ERROR=1
if ufw_yard_rules_remove owned; then fail 'firewall command failure was suppressed'; fi
TEST_UFW_ERROR=0 TEST_UFW_STALE=1
if ufw_yard_rules_remove owned; then fail 'remaining approved rules were accepted'; fi
TEST_UFW_STALE=0
ufw_yard_rules_remove owned || fail 'final bridge firewall cleanup failed'
[ ! -s "$SUBYARD_UFW_RULES_FILE" ] && grep -Fxq disable "$tmp/probe-access" \
  || fail 'last owned policy retained its probe grant'
printf 'ok: exact UFW bridge cleanup preserves shared rules and rejects failures\n'

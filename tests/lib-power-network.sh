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
# Like NetworkManager, only regular .conf files are read.
files = sorted(name for name in os.listdir(directory) if name.endswith(".conf") and os.path.isfile(os.path.join(directory, name))) if os.path.isdir(directory) else []
listed, no_auto, sections = None, None, []
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
        if section[0] == "[main]" and key.rstrip("+") == "no-auto-default":
            no_auto = (no_auto + "," + value) if key.endswith("+") and no_auto else value
            continue
        section[1].append(line)
print("[main]\nplugins=keyfile")
if no_auto is not None:
    print("no-auto-default=" + no_auto.replace(";", ","))
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
  '-4 -o address show scope global') printf '2: eth0 inet 192.0.2.5/24\n' ;;
  '-4 route show default') printf '%s' "${MOCK_DEFAULT_ROUTES:-}"; exit 0 ;;
  "-d -o link show dev "*)
    device="${*: -1}"
    case " ${MOCK_VETHS:-} " in *" $device "*) printf '9: %s@if8: <UP> mtu 1500 veth\n' "$device" ;; *) printf '2: %s: <UP>\n' "$device" ;; esac ;;
  "-o link show dev "*)
    device="${*: -1}"
    [ "$device" != "${MOCK_LINK_FAIL:-}" ] || exit 1
    case " ${MOCK_DOWN:-} " in *" $device "*) printf '5: %s: <BROADCAST,MULTICAST> mtu 1500 state DOWN\n' "$device" ;; *) printf '5: %s: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 state UP\n' "$device" ;; esac ;;
  *) exit 1 ;;
esac
SH
cat > "$tmp/bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  list) printf '%s,STOPPED\n' "$2" ;;
  network)
    [ "$*" != "network show incusbr0 --project default" ] || exit 0
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
export TEST_NM_VALIDATOR="${YARD_ENGINE_PATH:-$ROOT/.build/yard}"
[ -x "$TEST_NM_VALIDATOR" ] || fail "build the test engine before running this check"
cat > "$tmp/bin/dispatcher" <<'SH'
#!/usr/bin/env bash
case "${1:-}" in
  _nm-guard-violations) exec "$TEST_NM_VALIDATOR" "$@" ;;
  _publications) exit 0 ;;
  *) exit 90 ;;
esac
SH
chmod 0755 "$tmp/bin/dispatcher"
export SUBYARD_DISPATCHER_PATH="$tmp/bin/dispatcher"

# shellcheck source=scripts/lib-power.sh
# shellcheck disable=SC1091
. "$ROOT/scripts/lib-power.sh"
power_nm_binary() { printf '%s\n' "$tmp/bin/NetworkManager"; }
power_operator_terminal() { [ "$MOCK_TERMINAL" = 1 ]; }
# Until the guard section, the read state is not under test.
original_guard_state="$(declare -f power_nm_guard_state)"
power_nm_guard_state() { printf 'loaded\n'; }

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
    MOCK_VETHS='' MOCK_DEFAULT_ROUTES='' MOCK_LINK_FAIL='' \
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

# A bridge-only refusal must lead to init, which pins it.
reset_case
MOCK_UID=0 MOCK_NM_DEVICES='incusbr0:bridge:connected (externally)'
if power_nm_guard_effective incusbr0; then fail "a managed bridge passed the guard"; fi
[ "$POWER_ERROR" = 'NetworkManager guard is not effective: NetworkManager manages yard bridge incusbr0 — rerun yard init' ] \
  || fail "bridge-only remedy: $POWER_ERROR"

reset_case
MOCK_UID=0 MOCK_NMCLI_READ_RC=1
if SUBYARD_DISPATCHER_PATH='' power_nm_guard_effective incusbr0; then
  fail "guard passed without a validator"
fi
case "$POWER_ERROR" in *"cannot validate NetworkManager's effective configuration"*"cannot read NetworkManager's devices"*) ;;
  *) fail "validator failure hid an independent refusal: $POWER_ERROR" ;;
esac

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
unset VM_CPU_WEIGHT
SUBYARD_DISPATCHER_PATH="$tmp/bin/dispatcher"

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
eval "$original_guard_state"
for vector in "$ROOT"/tests/fixtures/nm-guard-vectors/*/; do
  read -r -a vector_bridges < "$vector/bridges" || vector_bridges=()
  got="$(power_nm_config_violations "${vector_bridges[@]}" < "$vector/config")"
  [ "$got" = "$(cat "$vector/violations")" ] || fail "guard vector $(basename "$vector"): $got"
done
# The read-state rule matches the Go rule.
while IFS='|' read -r record boot file hash want; do
  got="$(power_nm_guard_state_rule "$record" "$boot" "$file" "$hash")"
  [ "$got" = "$want" ] || fail "state '$record' boot=$boot file=$file hash=$hash gave $got, want $want"
done < "$ROOT/tests/fixtures/nm-guard-state.txt"

conf_dir="$tmp/NetworkManager/conf.d"
guard_conf="$conf_dir/zz-subyard-unmanaged.conf"
legacy_conf="$conf_dir/99-subyard-unmanaged.conf"
mkdir -p "$conf_dir" "$tmp/state"
POWER_NM_GUARD_CONF="$guard_conf" NM_GUARD_CONF="$guard_conf"
POWER_NM_GUARD_STATE="$tmp/state/nm-guard-state"
boot_n=1
printf 'boot-1\n' > "$tmp/boot_id"
power_nm_boot_id() { cat "$tmp/boot_id"; }
reboot_host() { boot_n=$((boot_n + 1)); printf 'boot-%s\n' "$boot_n" > "$tmp/boot_id"; }
# NetworkManager read the guard on disk when it started in an earlier boot.
mark_loaded() { nm_guard_write_file "$POWER_NM_GUARD_STATE" "boot-0 $(power_nm_file_id "$guard_conf") $(power_nm_guard_static_hash "$guard_conf") held init"; }
record_for() { printf 'boot-%s %s %s %s\n' "$boot_n" "$(power_nm_file_id "$guard_conf")" "$(power_nm_guard_static_hash "$guard_conf")" "$1"; }
incus_networks='[{"name":"incusbr0","type":"bridge","managed":true},{"name":"nestedbr0","type":"bridge","managed":true},{"name":"br0","type":"bridge","managed":false},{"name":"docker0","type":"bridge","managed":false},{"name":"incusnet1","type":"ovn","managed":true}]'
guard_case() {
  reset_case
  export MOCK_UID=0 MOCK_NM_CONF_DIR="$conf_dir" MOCK_INCUS_NETWORKS="$incus_networks" MOCK_DOWN=''
  export MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nenp5s0:ethernet:connected\nincusbr0:bridge:unmanaged\nnestedbr0:bridge:unmanaged\nbr0:bridge:connected'
  rm -f "$POWER_NM_GUARD_STATE"
}
hash_of() { if [ -e "$1" ]; then sha256sum "$1" | cut -d ' ' -f1; else printf absent; fi; }
state_of() { power_nm_guard_state "$guard_conf"; }
record_state() { cut -d ' ' -f4,5 "$POWER_NM_GUARD_STATE" 2>/dev/null; }
no_reload() { # NetworkManager is never reloaded or restarted, by any path.
  ! grep -q '^FORBIDDEN' "$MOCK_NMCLI_LOG" && [ ! -s "$MOCK_SYSTEMCTL_LOG" ] \
    || fail "$1 reloaded or restarted NetworkManager: $(cat "$MOCK_NMCLI_LOG" "$MOCK_SYSTEMCTL_LOG")"
}
render() { nm_guard_render "$1"; }
expected_guard='# Subyard: keep yard veths, Incus tap devices and Incus bridges unmanaged.
[device-subyard]
match-device=type:veth,driver:veth,interface-name:tap????????,interface-name:incusbr0,interface-name:nestedbr0
managed=0'

# The static content matches the Go rule on the shared fixtures; bridge names are not part of it.
for fixture in "$ROOT"/tests/fixtures/nm-guard-static/*.conf; do
  got="$(power_nm_guard_static_text < "$fixture"; printf x)"
  [ "$got" = "$(cat "${fixture%.conf}.static"; printf x)" ] || fail "static content of $(basename "$fixture"): ${got%x}"
done
[ "$(power_nm_guard_text_hash "$(render incusbr0)")" = "$(power_nm_guard_text_hash "$(render $'incusbr0\nnestedbr0')")" ] \
  || fail "bridge names changed the static hash"

# Removing any one rendered entry fails the rule.
rendered_entries="$(render $'incusbr0' | sed -n 's/^match-device=//p' | tr ',' '\n')"
while IFS= read -r entry; do
  kept="$(grep -Fxv -- "$entry" <<< "$rendered_entries" | paste -sd ',')"
  [ -n "$(printf '[device-subyard]\nmatch-device=%s\nmanaged=0\n' "$kept" | power_nm_config_violations incusbr0)" ] \
    || fail "a guard without $entry passed the rule"
done <<< "$rendered_entries"

# An active unit without a known binary is not an absent installation.
guard_case
rm -f "$guard_conf" "$legacy_conf"
if (power_nm_binary() { return 1; }; nm_unmanaged_guard_early "$guard_conf" >"$tmp/guard.out" 2>&1); then
  fail "init accepted an active NetworkManager without its binary"
fi
grep -Fq 'NetworkManager is active but its binary is unavailable at a known path' "$tmp/guard.out" \
  || fail "unknown binary refusal: $(cat "$tmp/guard.out")"
[ ! -e "$guard_conf" ] && [ ! -e "$POWER_NM_GUARD_STATE" ] || fail "unknown binary changed the guard"

# A failed conf.d copy must never validate or publish a partial proposal. Independent reads remain.
(nm_guard_nm_state
 nm_guard_write_file "$guard_conf" "$expected_guard"
 before_hash="$(hash_of "$guard_conf")"
 cp() { return 1; }
 MOCK_NMCLI_READ_RC=1
 if nm_guard_apply_current "$guard_conf" "$(render incusbr0)" incusbr0 1 1; then
   fail "a failed preflight copy was accepted"
 fi
 [ -z "$NM_GUARD_PROPOSED" ] || fail "a partial proposal was validated"
 for finding in 'cannot copy NetworkManager configuration for preflight' "cannot read NetworkManager's devices with nmcli"; do
   [[ "$POWER_ERROR" == *"$finding"* ]] || fail "missing preflight finding: $POWER_ERROR"
 done
 [ "$(hash_of "$guard_conf")" = "$before_hash" ] && [ ! -e "$POWER_NM_GUARD_STATE" ] \
   || fail "a failed preflight copy changed the guard")

# A dispatcher that exits unsuccessfully must prevent publication.
guard_case
before_hash="$(hash_of "$guard_conf")"
if (SUBYARD_DISPATCHER_PATH=/bin/false nm_unmanaged_guard_early "$guard_conf" >"$tmp/guard.out" 2>&1); then
  fail "init published a guard without validation"
fi
grep -Fq 'cannot validate the proposed NetworkManager configuration' "$tmp/guard.out" \
  || fail "validator execution failure: $(cat "$tmp/guard.out")"
if [ "$(hash_of "$guard_conf")" != "$before_hash" ] || [ -e "$POWER_NM_GUARD_STATE" ]; then
  fail "validator failure changed the guard"
fi

# First install with NetworkManager running: the early call writes the static section, records
# `unread init` for this boot and stops; planning, starts and init refuse until the host reboots,
# also without the record or with a record for other content.
guard_case
rm -f "$guard_conf"
umask 077
if (POWER_NM_GUARD_COMMAND='yard init' nm_unmanaged_guard_early "$guard_conf" >"$tmp/guard.out" 2>&1); then
  fail "init continued before NetworkManager read the guard"
fi
grep -Fq "never reloads or restarts NetworkManager. Reboot the host once, then rerun yard init" "$tmp/guard.out" \
  || fail "the first write did not ask for a reboot: $(cat "$tmp/guard.out")"
grep -Fq 'restart NetworkManager (' "$tmp/guard.out" && fail "the first write offered a NetworkManager restart"
[ "$(sed -n 3p "$guard_conf")" = 'match-device=type:veth,driver:veth,interface-name:tap????????' ] \
  || fail "the early call wrote bridges: $(cat "$guard_conf")"
[ "$(stat -c '%a' "$guard_conf")" = 644 ] && [ "$(stat -c '%a' "$POWER_NM_GUARD_STATE")" = 644 ] \
  || fail "NetworkManager drop-in or record mode is not 0644"
[ "$(cat "$POWER_NM_GUARD_STATE")" = "$(record_for 'unread init')" ] \
  || fail "record: $(cat "$POWER_NM_GUARD_STATE" 2>/dev/null)"
grep -Fq 'device set' "$MOCK_NMCLI_LOG" && fail "the early call pinned a device"
[ "$(state_of)" = unread-init ] || fail "the first write did not read as unread"
for attempt in record no-record foreign; do
  case "$attempt" in
    no-record) rm -f "$POWER_NM_GUARD_STATE" ;;
    foreign) nm_guard_write_file "$POWER_NM_GUARD_STATE" "boot-0 $(power_nm_file_id "$guard_conf") $(power_nm_guard_text_hash '[device-subyard]') held init" ;;
  esac
  if nm_guard_file_current "$guard_conf" incusbr0; then fail "planning accepted an unread guard ($attempt)"; fi
  if power_host_safe incusbr0; then fail "a start was accepted before NetworkManager read the guard ($attempt)"; fi
  if [ "$attempt" = record ]; then
    case "$POWER_ERROR" in *'has not read the Subyard guard'*'reboot the host once, then rerun'*) ;; *) fail "unread start refusal ($attempt): $POWER_ERROR" ;; esac
  else
    # Without a bound record only init can record what NetworkManager holds.
    case "$POWER_ERROR" in *'no Subyard record binds the guard'*'run yard init, then reboot the host once, then rerun'*) ;; *) fail "unbound start refusal ($attempt): $POWER_ERROR" ;; esac
  fi
  case "$POWER_ERROR" in *'restart NetworkManager'*) fail "the start refusal offered a NetworkManager restart" ;; esac
  if (nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "init continued while the guard is unread ($attempt)"; fi
  [ "$(record_state)" = 'unread init' ] || fail "init did not record the unread guard ($attempt): $(cat "$POWER_NM_GUARD_STATE")"
done
# The reboot loads it: the record names another boot.
first_record="$(cut -d ' ' -f1,3- "$POWER_NM_GUARD_STATE")"
reboot_host
[ "$(state_of)" = loaded ] || fail "the reboot did not load the guard"
# The late call adds the bridges without a reboot: boot, content and state stay, the identity
# follows the rewritten file.
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null || fail "the late call failed after the reboot: $POWER_ERROR"
[ "$(cat "$guard_conf")" = "$expected_guard" ] || fail "guard bytes for two Incus bridges: $(cat "$guard_conf")"
[ "$(cut -d ' ' -f1,3- "$POWER_NM_GUARD_STATE")" = "$first_record" ] \
  && [ "$(cut -d ' ' -f2 "$POWER_NM_GUARD_STATE")" = "$(power_nm_file_id "$guard_conf")" ] \
  || fail "bridge names changed the record: $(cat "$POWER_NM_GUARD_STATE")"
# Removing and adding back an Incus bridge rewrites only bridge names.
MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true}]' nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null \
  || fail "a removed bridge on a loaded guard stopped init"
[ "$(cat "$guard_conf")" = "$(render incusbr0)" ] && [ "$(state_of)" = loaded ] || fail "a removed bridge unloaded the guard"
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null || fail "a new bridge on a loaded guard stopped init"
[ "$(state_of)" = loaded ] || fail "a new bridge unloaded the guard"
# The same bridge edit by hand is not Subyard's: the identity no longer matches.
sleep 0.03
nm_guard_write_file "$guard_conf" "$(render incusbr0)"
[ "$(state_of)" = unbound ] || fail "a hand edit read as $(state_of)"
if (nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "init continued after a hand edit"; fi
reboot_host
power_host_safe incusbr0 || fail "a start was refused after NetworkManager read the guard: $POWER_ERROR"
# Run both standalone E2E assertions against the same loaded guard without an ambient dispatcher.
(
  unset SUBYARD_DISPATCHER_PATH
  OPERATOR_HOME="$tmp/boot-operator"
  runtime="$OPERATOR_HOME/.subyard/runtime"
  release="$runtime/releases/candidate"
  mkdir -p "$release/scripts" "$release/bin"
  ln -s releases/candidate "$runtime/current"
  ln -s "$TEST_NM_VALIDATOR" "$release/bin/yard-engine"
  cp "$ROOT/scripts/lib-power.sh" "$release/scripts/lib-power.sh"
  {
    printf 'tmp=%q\n' "$tmp"
    declare -f power_nm_binary power_nm_boot_id
    printf 'POWER_NM_GUARD_CONF=%q POWER_NM_GUARD_STATE=%q\n' "$POWER_NM_GUARD_CONF" "$POWER_NM_GUARD_STATE"
  } >> "$release/scripts/lib-power.sh"
  operator_env() { "$@"; }
  eval "$(sed -n '/^assert_guards() {/,/^}/p' "$ROOT/dev/e2e/proxy-address-boot.sh")"
  assert_guards

  YARD_BIN="$TEST_NM_VALIDATOR"
  BASELINE_BRIDGE=incusbr0
  HOST_DEFAULT_ROUTE_BEFORE="$(ip -4 route show default | sort)"
  HOST_GLOBAL_IPV4_BEFORE="$(ip -4 -o address show scope global | awk '{print $2, $4}' | sort)"
  export YARD_BIN BASELINE_BRIDGE HOST_DEFAULT_ROUTE_BEFORE HOST_GLOBAL_IPV4_BEFORE
  eval "$(sed -n '/^assert_host_network_unchanged() {/,/^}/p' "$ROOT/dev/e2e/nested-teardown-data-boundary.sh")"
  assert_host_network_unchanged
)
inode_before="$(stat -c '%i:%Z' "$guard_conf")"
nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out"
grep -Fq 'already current' "$tmp/guard.out" && [ "$(stat -c '%i:%Z' "$guard_conf")" = "$inode_before" ] \
  || fail "equal bytes touched the guard inode"
no_reload 'the first install'

# Repairs change the static content: edited-out entries or managed=1 record `unread` for this
# boot and init stops for the reboot, even when the previous record was held.
for broken in edited managed; do
  guard_case
  nm_guard_write_file "$guard_conf" "$expected_guard"
  mark_loaded
  if [ "$broken" = edited ]; then
    printf '[device-subyard]\nmatch-device=interface-name:incusbr0,interface-name:nestedbr0\nmanaged=0\n' > "$guard_conf"
  else
    sed -i 's/^managed=0$/managed=1/' "$guard_conf"
  fi
  [ "$(state_of)" = unbound ] || fail "changed static content read as $(state_of) ($broken)"
  # The old evidence is gone before the repaired guard is renamed in: a crash in between leaves
  # no record that could match the repaired content.
  (eval "original_$(declare -f nm_guard_write_file)"
   nm_guard_write_file() {
     [ "$1" != "$guard_conf" ] || [ ! -e "$POWER_NM_GUARD_STATE" ] || fail "the repair renamed the guard before invalidating the record"
     original_nm_guard_write_file "$@"
   }
   if (nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out" 2>&1); then fail "a repair continued without a reboot ($broken): $(cat "$tmp/guard.out")"; fi)
  [ "$(cat "$POWER_NM_GUARD_STATE")" = "$(record_for 'unread init')" ] \
    && [ "$(cat "$guard_conf")" = "$expected_guard" ] || fail "repair record ($broken): $(cat "$POWER_NM_GUARD_STATE")"
  if power_host_safe incusbr0; then fail "a start passed after a repair ($broken)"; fi
  reboot_host
  [ "$(state_of)" = loaded ] || fail "the reboot did not load the repair ($broken)"
done
# A link in place of the guard is never bound, even with a record for its unchanged target: the
# link could have been retargeted and restored after NetworkManager read another file. Starts
# send the operator to init, and init replaces the link with a regular file, recorded unread.
guard_case
printf '%s\n' "$expected_guard" > "$tmp/linked.conf"
ln -sfn "$tmp/linked.conf" "$guard_conf"
nm_guard_write_file "$POWER_NM_GUARD_STATE" "boot-0 $(power_nm_file_id "$guard_conf") $(power_nm_guard_static_hash "$guard_conf") held init"
[ "$(state_of)" = unbound ] || fail "a linked guard with a record for its target read as $(state_of)"
if power_host_safe incusbr0; then fail "a start passed on a linked guard"; fi
case "$POWER_ERROR" in *'is not a regular file (a link, for example); init replaces it with a file'*'run yard init, then reboot the host once'*) ;; *) fail "linked guard refusal: $POWER_ERROR" ;; esac
if (nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "init continued after replacing a linked guard"; fi
[ -f "$guard_conf" ] && [ ! -L "$guard_conf" ] && [ "$(cat "$tmp/linked.conf")" = "$expected_guard" ] \
  || fail "init did not replace the link with a file, or changed its target"
[ "$(cat "$POWER_NM_GUARD_STATE")" = "$(record_for 'unread init')" ] || fail "linked guard record: $(cat "$POWER_NM_GUARD_STATE")"
reboot_host
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null || fail "init failed after the reboot read the replaced guard: $POWER_ERROR"
ln -sfn "$tmp/missing.conf" "$guard_conf"
[ "$(state_of)" = unbound ] || fail "a dangling guard link read as $(state_of)"
rm -f "$guard_conf"
# A link to a directory: init replaces the link itself with a regular file and leaves the
# directory untouched (mv -T never moves into it).
guard_case
mkdir -p "$tmp/linked-dir" && printf 'keep\n' > "$tmp/linked-dir/admin.txt"
ln -sfn "$tmp/linked-dir" "$guard_conf"
[ "$(state_of)" = unbound ] || fail "a guard link to a directory read as $(state_of)"
if (nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out" 2>&1); then fail "init continued after replacing a directory link"; fi
[ -f "$guard_conf" ] && [ ! -L "$guard_conf" ] && [ "$(cat "$guard_conf")" = "$expected_guard" ] \
  || fail "init did not replace the directory link with the guard file: $(cat "$tmp/guard.out"; ls -la "$guard_conf")"
[ "$(ls -A "$tmp/linked-dir")" = admin.txt ] && [ "$(cat "$tmp/linked-dir/admin.txt")" = keep ] \
  || fail "init changed the linked directory: $(ls -A "$tmp/linked-dir")"
# A non-regular guard is a form Subyard did not write, so it goes through the migration.
[ "$(record_state)" = 'unread migration' ] || fail "directory link record: $(cat "$POWER_NM_GUARD_STATE")"
# A FIFO in place of the guard reads unbound at once and is never opened; init replaces it.
guard_case
rm -f "$guard_conf" && mkfifo "$guard_conf"
[ "$(timeout 5 bash -c '. "$1"; POWER_NM_GUARD_STATE="$2" power_nm_guard_state "$3"' _ "$ROOT/scripts/lib-power.sh" "$POWER_NM_GUARD_STATE" "$guard_conf")" = unbound ] \
  || fail "a FIFO guard did not read unbound at once"
if (timeout 20 bash -c "$(declare -f); $(declare -p | grep -E '^declare -[^ ]*x? *(POWER_|NM_GUARD|MOCK_|tmp=|ROOT=)' || true); nm_unmanaged_guard incusbr0 '$guard_conf'" >"$tmp/guard.out" 2>&1); then
  fail "init continued after replacing a FIFO guard"
fi
[ -f "$guard_conf" ] && [ "$(cat "$guard_conf")" = "$expected_guard" ] || fail "init did not replace the FIFO guard: $(cat "$tmp/guard.out")"
# Equal bytes without a binding and NetworkManager explicitly inactive at both samples: `held`.
guard_case
MOCK_NM_STATE=inactive
nm_guard_write_file "$guard_conf" "$(render '')"
nm_unmanaged_guard_early "$guard_conf" >/dev/null || fail "an inactive NetworkManager stopped the early call on an unbound guard"
[ "$(record_state)" = 'held init' ] || fail "unbound guard with inactive NetworkManager recorded: $(cat "$POWER_NM_GUARD_STATE")"
# A hand restore after NetworkManager read something else: equal bytes in another file are
# unbound; init records them unread for this boot and only the reboot loads them.
guard_case
nm_guard_write_file "$guard_conf" "$expected_guard"
mark_loaded
reboot_host
sleep 0.03 # ctime ticks with the kernel's coarse clock; a real restore is never within one tick
cp "$guard_conf" "$tmp/restore.conf" && rm -f "$guard_conf" && cp "$tmp/restore.conf" "$guard_conf"
[ "$(state_of)" = unbound ] || fail "a hand-restored guard read as $(state_of)"
if power_host_safe incusbr0; then fail "a start passed on a hand-restored guard"; fi
if (nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "init continued on a hand-restored guard"; fi
[ "$(cat "$POWER_NM_GUARD_STATE")" = "$(record_for 'unread init')" ] || fail "hand-restore record: $(cat "$POWER_NM_GUARD_STATE")"
reboot_host
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null || fail "init failed after the reboot read the restored guard: $POWER_ERROR"
# A crash after the guard rename and before its record (the record is already gone), then a
# reboot: starts send the operator to init, init records the guard unread and stops, and after
# the next reboot init and starts work.
guard_case
nm_guard_write_file "$guard_conf" "$expected_guard"
reboot_host
if power_host_safe incusbr0; then fail "a start passed after a crashed publish"; fi
case "$POWER_ERROR" in *'run yard init, then reboot the host once, then rerun'*) ;; *) fail "crashed publish remedy: $POWER_ERROR" ;; esac
if nm_guard_file_current "$guard_conf" incusbr0; then fail "planning accepted a crashed publish"; fi
if (nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "init continued after a crashed publish"; fi
[ "$(record_state)" = 'unread init' ] || fail "init did not record the crashed publish: $(cat "$POWER_NM_GUARD_STATE" 2>/dev/null)"
reboot_host
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null || fail "init failed after the recovery reboot: $POWER_ERROR"
power_host_safe incusbr0 || fail "a start failed after the recovery reboot: $POWER_ERROR"
# NetworkManager inactive across the write: `held`; started during the write: `unread`.
guard_case
MOCK_NM_STATE=inactive
rm -f "$guard_conf"
nm_unmanaged_guard_early "$guard_conf" >/dev/null || fail "an inactive NetworkManager stopped the early call"
[ "$(record_state)" = 'held init' ] || fail "inactive record: $(cat "$POWER_NM_GUARD_STATE")"
guard_case
MOCK_NM_STATE=inactive
rm -f "$guard_conf"
# Active or unknown at the second sample is `unread`, and the early call stops before the
# installer installs packages or restarts Incus.
for second in active error; do
  guard_case
  MOCK_NM_STATE=inactive
  rm -f "$guard_conf"
  if (eval "original_$(declare -f nm_guard_write_file)"
   nm_guard_write_file() { original_nm_guard_write_file "$@" || return; [ "$1" != "$guard_conf" ] || export MOCK_NM_STATE="$second"; }
   nm_unmanaged_guard_early "$guard_conf" >"$tmp/guard.out" 2>&1); then
    fail "the early call continued after NetworkManager became $second during the write"
  fi
  grep -Fq 'Reboot the host once' "$tmp/guard.out" || fail "mid-write stop ($second): $(cat "$tmp/guard.out")"
  [ "$(record_state)" = 'unread init' ] || fail "NetworkManager $second during the write recorded: $(cat "$POWER_NM_GUARD_STATE")"
done
# A failed record write or boot ID read leaves no record; starts refuse.
guard_case
nm_guard_write_file "$guard_conf" "$(render incusbr0)"
# The record's directory cannot exist: a file holds its name.
POWER_NM_GUARD_STATE="$tmp/state-blocked/nm-guard-state"
: > "$tmp/state-blocked"
if (nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out" 2>&1); then fail "init continued without a record"; fi
[ ! -e "$POWER_NM_GUARD_STATE" ] || fail "a failed record write left a record"
grep -Fq "could not write the NetworkManager guard record $POWER_NM_GUARD_STATE" "$tmp/guard.out" \
  || fail "a failed record write gave no warning: $(cat "$tmp/guard.out")"
if power_host_safe incusbr0; then fail "a start passed after a failed record write"; fi
POWER_NM_GUARD_STATE="$tmp/state/nm-guard-state"
mark_loaded
sed -i 's/^managed=0$/managed=1/' "$guard_conf"
if (power_nm_boot_id() { return 1; }; nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "init continued without a boot ID"; fi
[ ! -e "$POWER_NM_GUARD_STATE" ] || fail "a failed boot ID read left a record"
(power_nm_boot_id() { return 1; }; [ "$(state_of)" = unread-init ]) || fail "a failed boot ID read gave loaded"
# An unreadable record never yields loaded.
mark_loaded
if [ "$(id -u)" != 0 ]; then
  chmod 0000 "$POWER_NM_GUARD_STATE"
  [ "$(state_of)" = unbound ] || fail "an unreadable record read as $(state_of)"
  chmod 0644 "$POWER_NM_GUARD_STATE"
fi
no_reload 'the read state'

# Bridge ownership: names come only from this run's Incus inventory, and only bridges are pinned.
guard_case
nm_guard_write_file "$guard_conf" "$(render incusbr0)"
mark_loaded
if (nm_unmanaged_guard br0 "$guard_conf" >"$tmp/guard.out" 2>&1); then fail "init accepted a yard bridge Incus does not manage"; fi
grep -Fq 'the yard bridge br0 is not an Incus-managed bridge' "$tmp/guard.out" || fail "non-Incus bridge refusal: $(cat "$tmp/guard.out")"
grep -Eq 'interface-name:br0(,|$)' "$guard_conf" && fail "the guard lists the administrator's br0"
grep -Fq 'device set' "$MOCK_NMCLI_LOG" && fail "a non-Incus bridge was pinned"
MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nincusbr0:bridge:unmanaged\nnestedbr0:bridge:connected (externally)\nbr0:bridge:connected\nnestedbr0x:ethernet:connected'
MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true},{"name":"nestedbr0","type":"bridge","managed":true}]'
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null || fail "init failed with a second Incus bridge: $POWER_ERROR"
[ "$(cat "$MOCK_NMCLI_PINS")" = nestedbr0 ] || fail "pins: $(cat "$MOCK_NMCLI_PINS" 2>/dev/null)"
rm -f "$MOCK_NMCLI_PINS"
if (MOCK_PIN_RC=1 nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "a failed pin was accepted"; fi
if (MOCK_PIN_NOEFFECT=1 nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/pin.out" 2>&1); then fail "an ineffective pin was accepted"; fi
grep -Fq 'still manages the Incus bridge nestedbr0' "$tmp/pin.out" || fail "ineffective pin unreported: $(cat "$tmp/pin.out")"
# A failed inventory: planning reports drift, the late call dies, the early call keeps what is listed.
guard_case
nm_guard_write_file "$guard_conf" "$(render $'incusbr0\nnestedbr0')"
mark_loaded
before_hash="$(hash_of "$guard_conf")"
if MOCK_INCUS_NETWORK_RC=7 nm_guard_file_current "$guard_conf" incusbr0; then fail "planning accepted a failed inventory"; fi
if (MOCK_INCUS_NETWORK_RC=7 nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "the late call accepted a failed inventory"; fi
MOCK_INCUS_NETWORK_RC=7 nm_unmanaged_guard_early "$guard_conf" >/dev/null || fail "the early call failed on a failed inventory: $POWER_ERROR"
[ "$(hash_of "$guard_conf")" = "$before_hash" ] || fail "a failed inventory changed the guard"
MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true},{"name":"nestedbr0","type":"bridge","managed":true},{"name":"thirdbr0","type":"bridge","managed":true}]'
if nm_guard_file_current "$guard_conf" incusbr0; then fail "planning missed a new Incus bridge"; fi
# Without incus no Incus bridge exists: the early call writes the static section.
(power_cmd_incus_off() { :; }; PATH="$tmp/noincus:$PATH"; hash -r
 mkdir -p "$tmp/noincus"; for tool in bash awk sed sort cat grep mktemp chmod cp rm mv install stat head tr paste comm cut python3 date getconf systemctl nmcli NetworkManager ip id sha256sum sync; do
   command -v "$tool" >/dev/null && ln -sf "$(command -v "$tool")" "$tmp/noincus/$tool"; done
 PATH="$tmp/noincus" nm_unmanaged_guard_early "$guard_conf" >/dev/null) || fail "the early call failed without incus"
[ "$(sed -n 3p "$guard_conf")" = 'match-device=type:veth,driver:veth,interface-name:tap????????' ] || fail "no-incus early call: $(cat "$guard_conf")"
no_reload 'bridge ownership'

# Every refusal is reported together; nothing is written.
guard_case
nm_guard_write_file "$guard_conf" "$(render incusbr0)"
mark_loaded
before_hash="$(hash_of "$guard_conf")"
printf '[device-later]\nmatch-device=interface-name:eth9\nmanaged=1\n' > "$conf_dir/zzz-admin.conf"
MOCK_NM_DEVICES=$'veth1234:ethernet:connected\nincusbr0:bridge:unmanaged\nnestedbr0:bridge:unmanaged'
MOCK_VETHS=veth1234 MOCK_DEFAULT_ROUTES=$'default via 192.0.2.1 dev nestedbr0\n'
if (nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out" 2>&1); then fail "unsafe guard change was applied"; fi
for expected in '[device-later] has priority over [device-subyard] and sets managed=1' \
  'NetworkManager manages veth1234, which the guard covers; release it first: sudo nmcli device set veth1234 managed no' \
  'the host default route uses nestedbr0' 'then rerun yard init'; do
  grep -Fq -- "$expected" "$tmp/guard.out" || fail "refusal lacks '$expected': $(cat "$tmp/guard.out")"
done
[ "$(hash_of "$guard_conf")" = "$before_hash" ] || fail "a refusal changed the guard"
rm -f "$conf_dir/zzz-admin.conf"
MOCK_VETHS='' MOCK_DEFAULT_ROUTES=''
if (MOCK_NM_PROPOSED_RC=7 nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out" 2>&1); then fail "an invalid proposal was applied"; fi
grep -Fq 'NetworkManager rejects the proposed configuration' "$tmp/guard.out" || fail "invalid proposal unreported"
if (MOCK_NMCLI_READ_RC=10 nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null 2>&1); then fail "a failed device read was accepted"; fi
# A failed --print-config or device read hides no independent refusal.
MOCK_DEFAULT_ROUTES=$'default via 192.0.2.1 dev incusbr0\n'
if (MOCK_NM_MODE=fail MOCK_NMCLI_READ_RC=10 nm_unmanaged_guard incusbr0 "$guard_conf" >"$tmp/guard.out" 2>&1); then fail "failed reads were accepted"; fi
for expected in "cannot read NetworkManager's effective configuration" "cannot read NetworkManager's devices with nmcli" \
  'the host default route uses incusbr0'; do
  grep -Fq -- "$expected" "$tmp/guard.out" || fail "failed-read refusal lacks '$expected': $(cat "$tmp/guard.out")"
done
MOCK_DEFAULT_ROUTES=''
MOCK_NM_DEVICES=$'incusbr0:bridge:connected'
if (MOCK_NM_MODE=fail power_nm_guard_effective incusbr0); then fail "the validator accepted a failed --print-config"; fi
MOCK_NM_MODE=fail power_nm_guard_effective incusbr0 || true
case "$POWER_ERROR" in *"effective configuration"*'manages yard bridge incusbr0'*) ;; *) fail "the validator hid a managed bridge: $POWER_ERROR" ;; esac
MOCK_NM_DEVICES=$'veth1234:ethernet:connected\nincusbr0:bridge:unmanaged\nnestedbr0:bridge:unmanaged'
if MOCK_NMCLI_READ_RC=10 power_nm_guard_effective incusbr0; then fail "the validator accepted a failed device read"; fi
[ "$(hash_of "$guard_conf")" = "$before_hash" ] || fail "a refusal changed the guard"
# A higher-priority override and a missing entry name their remedies.
printf '[device-later]\nmatch-device=interface-name:eth9\nmanaged=1\n' > "$conf_dir/zzz-admin.conf"
if power_nm_guard_effective incusbr0; then fail "a higher-priority override passed"; fi
case "$POWER_ERROR" in *'correct the NetworkManager configuration, then reboot the host once, then rerun'*) ;; *) fail "override remedy: $POWER_ERROR" ;; esac
rm -f "$conf_dir/zzz-admin.conf"
nm_guard_write_file "$guard_conf" "$(render '')"
if power_nm_guard_effective incusbr0; then fail "a guard without the yard bridge passed"; fi
case "$POWER_ERROR" in *'match-device lacks interface-name:incusbr0'*'run yard init'*) ;; *) fail "missing entry remedy: $POWER_ERROR" ;; esac
no_reload 'refusals'

# NetworkManager inactive, or not installed: the file is written and held, no stop, no pins.
guard_case
MOCK_NM_STATE=inactive
rm -f "$guard_conf"
nm_unmanaged_guard_early "$guard_conf" >/dev/null && nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null \
  || fail "an inactive NetworkManager stopped init"
[ "$(cat "$guard_conf")" = "$expected_guard" ] && [ "$(record_state)" = 'held init' ] || fail "inactive guard write is wrong"
grep -Fq 'device set' "$MOCK_NMCLI_LOG" && fail "an inactive NetworkManager was asked to pin"
nm_guard_file_current "$guard_conf" incusbr0 || fail "inactive guard reported drift"
(power_nm_binary() { return 1; }; nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null) || fail "no NetworkManager binary failed"
rm -f "$guard_conf"
(power_nm_binary() { return 1; }; nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null)
[ ! -e "$guard_conf" ] || fail "a host without NetworkManager got a guard"
no_reload 'an inactive NetworkManager'

# Teardown keeps the guard for surviving Incus bridges and removes it, the legacy file and the
# record after the last one; a failed inventory keeps everything.
guard_case
MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true},{"name":"nestedbr0","type":"bridge","managed":true}]'
nm_guard_write_file "$guard_conf" "$(render incusbr0)"
mark_loaded
nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null || fail "teardown failed: $POWER_ERROR"
grep -Fq 'interface-name:nestedbr0' "$guard_conf" || fail "teardown dropped a surviving bridge"
before_hash="$(hash_of "$guard_conf")"
if (MOCK_INCUS_NETWORK_RC=7 nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null 2>&1); then fail "teardown accepted a failed inventory"; fi
[ "$(hash_of "$guard_conf")" = "$before_hash" ] || fail "a failed inventory changed the guard"
nm_guard_write_file "$POWER_NM_GUARD_STATE" "boot-1 x unread init"
MOCK_INCUS_NETWORKS='[{"name":"br0","type":"bridge","managed":false}]' nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null
[ ! -e "$guard_conf" ] && [ ! -e "$POWER_NM_GUARD_STATE" ] || fail "teardown kept the guard or its record"
no_reload 'teardown'

# Migration of a released guard.
released_spec='type:veth;driver:veth;interface-name:veth*;interface-name:docker*;interface-name:br-*;interface-name:virbr*;interface-name:vnet*;interface-name:tap*;interface-name:macvtap*;interface-name:incusbr0;interface-name:br0'
write_released() {
  printf '[main]\nno-auto-default=%s\n\n[keyfile]\nunmanaged-devices=%s\n\n[device-subyard]\nmatch-device=%s\nmanaged=0\n' \
    "$released_spec" "$released_spec" "${released_spec//;/,}" > "$guard_conf"
  printf '[keyfile]\nunmanaged-devices=interface-name:legacybr0\n' > "$legacy_conf"
  rm -f "$POWER_NM_GUARD_STATE"
}
guard_case
MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true}]'
write_released
if power_nm_guard_effective incusbr0; then fail "a released guard without a record passed the rule"; fi
migration_refuses() { # <admin list> <description> [teardown]
  write_released
  printf '[keyfile]\nunmanaged-devices=%s\n' "$1" > "$conf_dir/00-admin.conf"
  before_hash="$(hash_of "$guard_conf")"
  if [ -n "${3:-}" ]; then
    if MOCK_INCUS_NETWORKS='[]' nm_unmanaged_guard_reconcile "$guard_conf" >"$tmp/migrate.out" 2>&1; then fail "teardown removed a guard over a hijack ($2)"; fi
  elif (POWER_NM_GUARD_COMMAND='yard update' nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out" 2>&1); then
    fail "migration applied with a hijacked device ($2)"
  fi
  grep -Fq "NetworkManager manages enp6s0, which the administrator's unmanaged-devices list marks unmanaged" "$tmp/migrate.out" \
    && grep -Fq "'sudo nmcli device set enp6s0 managed no' (or the administrator's own way)" "$tmp/migrate.out" \
    && grep -Fq "change the administrator's own unmanaged-devices list if NetworkManager should keep the device" "$tmp/migrate.out" \
    || fail "hijack refusal ($2): $(cat "$tmp/migrate.out")"
  [ "$(hash_of "$guard_conf")" = "$before_hash" ] && [ -e "$legacy_conf" ] && [ ! -e "$POWER_NM_GUARD_STATE" ] \
    || fail "a refused migration changed the host ($2)"
}
MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nenp6s0:ethernet:connected\nincusbr0:bridge:unmanaged'
migration_refuses 'interface-name:enp6s0' 'exact name'
migration_refuses '*' 'match-all'
migration_refuses 'interface-name:*' 'interface-name match-all'
migration_refuses 'except:interface-name:wlan0' 'except-only'
migration_refuses 'type:ethernet' 'type'
migration_refuses '*,except:type:wifi,except:type:gsm,except:type:cdma' 'Ubuntu stock list'
migration_refuses 'interface-name:enp6s0' 'teardown' teardown
# Connection metadata is not inspected: this inventory covers explicit persisted profiles
# (including a server with no-auto-default=*) and automatic connections alike. Neither refuses.
for listed in 'interface-name:enp6s0' '*' 'except:interface-name:wlan0' 'type:ethernet'; do
  write_released
  printf '[main]\nno-auto-default=%s\n' "$listed" > "$conf_dir/00-admin.conf"
  nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out" 2>&1 \
    || fail "no-auto-default refused a device with an explicit profile: $(cat "$tmp/migrate.out")"
  grep -Fq "the administrator's no-auto-default list selects enp6s0; if enp6s0 relies on an automatic connection, create an explicit profile before the reboot" "$tmp/migrate.out" \
    || fail "no-auto-default notice missing: $(cat "$tmp/migrate.out")"
  [ -z "$NM_GUARD_REFUSALS" ] && [ ! -e "$legacy_conf" ] || fail "no-auto-default prevented migration"
  grep -Fq 'device set' "$MOCK_NMCLI_LOG" && fail "no-auto-default changed device management"
done
# Teardown also leaves a selected managed NIC alone.
write_released
MOCK_INCUS_NETWORKS='[]' nm_unmanaged_guard_reconcile "$guard_conf" >"$tmp/migrate.out" 2>&1 \
  || fail "no-auto-default refused teardown: $(cat "$tmp/migrate.out")"
[ ! -e "$guard_conf" ] && [ ! -e "$legacy_conf" ] || fail "no-auto-default kept a retired guard"
grep -Fq "the administrator's no-auto-default list selects enp6s0; if enp6s0 relies on an automatic connection, create an explicit profile before the reboot" "$tmp/migrate.out" \
  || fail "teardown dropped the no-auto-default notice: $(cat "$tmp/migrate.out")"
# Lists Subyard cannot decide migrate with a notice naming the list and the managed devices.
for key in unmanaged-devices no-auto-default; do
  section=keyfile
  [ "$key" != no-auto-default ] || section=main
  for undecided in 'interface-name:docker*;interface-name:virbr*' 'mac:00:11:22:33:44:55'; do
    write_released
    printf '[%s]\n%s=%s\n' "$section" "$key" "$undecided" > "$conf_dir/00-admin.conf"
    nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out" 2>&1 || fail "an undecidable list refused the migration: $(cat "$tmp/migrate.out")"
    grep -Fq "cannot decide whether the administrator's $key list (${undecided//;/,}) marks these devices NetworkManager manages: enp6s0" "$tmp/migrate.out" \
      && grep -Fq 'Do not reload or restart NetworkManager before the reboot' "$tmp/migrate.out" \
      || fail "undecidable-list notice: $(cat "$tmp/migrate.out")"
  done
done
# After the hand-back: with evidence (every up Incus bridge the released guard names is unmanaged)
# the record says `held migration` and starts work until the reboot; without it, `unread`.
printf '[keyfile]\nunmanaged-devices=interface-name:enp6s0\n' > "$conf_dir/00-admin.conf"
MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nenp6s0:ethernet:unmanaged\nincusbr0:bridge:unmanaged\nbr0:bridge:connected'
write_released
nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out" || fail "migration failed: $POWER_ERROR"
nm_guard_format_current "$guard_conf" && [ ! -e "$legacy_conf" ] || fail "migration did not rewrite the guard"
grep -Eq 'interface-name:br0(,|$)' "$guard_conf" && fail "the migrated guard kept the administrator's br0"
[ "$(record_state)" = 'held migration' ] || fail "evidence record: $(cat "$POWER_NM_GUARD_STATE")"
grep -Fq 'reboot the host once' "$tmp/migrate.out" || fail "migration did not ask for the reboot"
grep -Fq 'device set' "$MOCK_NMCLI_LOG" && fail "migration pinned a device"
[ "$(state_of)" = loaded-reboot ] || fail "migrated state: $(state_of)"
power_nm_reboot_pending || fail "the reboot reminder is not pending"
power_host_safe incusbr0 || fail "a held migration blocked the start: $POWER_ERROR"
nm_guard_file_current "$guard_conf" incusbr0 || fail "planning reported drift while the migration waits for the reboot"
nm_unmanaged_guard incusbr0 "$guard_conf" >/dev/null || fail "init failed while the migration waits for the reboot"
reboot_host
power_nm_reboot_pending && fail "the reboot did not clear the reboot reminder"
[ "$(state_of)" = loaded ] || fail "the reboot did not load the migrated guard"
for evidence in down managed link-read; do
  write_released
  case "$evidence" in
    down) MOCK_DOWN=incusbr0 ;;
    managed) MOCK_NM_DEVICES=$'enp6s0:ethernet:unmanaged\nincusbr0:bridge:connected (externally)' ;;
    # Two named up bridges: a failed link read of the second one is no evidence.
    link-read) MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true},{"name":"vbr0","type":"bridge","managed":true}]'
      sed -i 's/interface-name:incusbr0/interface-name:incusbr0,interface-name:vbr0/' "$guard_conf"
      MOCK_LINK_FAIL=vbr0 ;;
  esac
  nm_unmanaged_guard_migrate "$guard_conf" >"$tmp/migrate.out" 2>&1 || fail "migration without evidence failed ($evidence): $(cat "$tmp/migrate.out")"
  [ "$(record_state)" = 'unread migration' ] || fail "no-evidence record ($evidence): $(cat "$POWER_NM_GUARD_STATE")"
  if power_host_safe incusbr0; then fail "a start passed before the reboot without evidence ($evidence)"; fi
  case "$POWER_ERROR" in *'reboot the host once'*) ;; *) fail "unread migration remedy: $POWER_ERROR" ;; esac
  MOCK_DOWN='' MOCK_LINK_FAIL='' MOCK_NM_DEVICES=$'lo:loopback:connected (externally)\nenp6s0:ethernet:unmanaged\nincusbr0:bridge:unmanaged'
  MOCK_INCUS_NETWORKS='[{"name":"incusbr0","type":"bridge","managed":true}]'
done
# Without incus, and with NetworkManager inactive, the migration writes the static section.
write_released
MOCK_NM_STATE=inactive
PATH="$tmp/noincus" nm_unmanaged_guard_migrate "$guard_conf" >/dev/null || fail "migration without incus failed"
[ "$(sed -n 3p "$guard_conf")" = 'match-device=type:veth,driver:veth,interface-name:tap????????' ] && [ "$(record_state)" = 'held migration' ] \
  || fail "no-incus migration: $(cat "$guard_conf") $(cat "$POWER_NM_GUARD_STATE" 2>/dev/null)"
# The early call stops after a migration NetworkManager has not provably read, before the
# installer installs packages or restarts Incus; a record-write failure stops it too.
MOCK_NM_STATE=active
write_released
if (PATH="$tmp/noincus" nm_unmanaged_guard_early "$guard_conf" >"$tmp/migrate.out" 2>&1); then fail "the early call continued after an unread migration without incus"; fi
grep -Fq 'Reboot the host once' "$tmp/migrate.out" || fail "early unread migration stop: $(cat "$tmp/migrate.out")"
nm_guard_format_current "$guard_conf" && [ "$(record_state)" = 'unread migration' ] || fail "the early call did not migrate the released guard"
for truncated in empty partial; do
  rm -f "$legacy_conf" "$POWER_NM_GUARD_STATE"
  if [ "$truncated" = empty ]; then : > "$guard_conf"; else printf '[main]\nno-auto-default=type:veth' > "$guard_conf"; fi
  nm_guard_released "$guard_conf" || fail "a $truncated guard is not a released form"
  if (nm_unmanaged_guard_early "$guard_conf" >"$tmp/migrate.out" 2>&1); then fail "the early call continued after migrating a $truncated guard"; fi
  grep -Fq 'Reboot the host once' "$tmp/migrate.out" && [ "$(record_state)" = 'unread migration' ] \
    || fail "$truncated guard: $(cat "$tmp/migrate.out") $(cat "$POWER_NM_GUARD_STATE" 2>/dev/null)"
done
: > "$guard_conf"; rm -f "$POWER_NM_GUARD_STATE"
POWER_NM_GUARD_STATE="$tmp/state-blocked/nm-guard-state"
if (nm_unmanaged_guard_early "$guard_conf" >"$tmp/migrate.out" 2>&1); then fail "the early call continued after a failed migration record"; fi
grep -Fq "could not write the NetworkManager guard record $POWER_NM_GUARD_STATE, so no yard starts" "$tmp/migrate.out" \
  && [ "$(grep -c 'no yard starts' "$tmp/migrate.out")" = 1 ] || fail "failed migration record: $(cat "$tmp/migrate.out")"
POWER_NM_GUARD_STATE="$tmp/state/nm-guard-state"
# A failed inventory keeps released names NetworkManager reports as unmanaged bridges and drops
# a bridge it runs (the administrator's br0).
write_released
MOCK_NM_DEVICES=$'enp6s0:ethernet:unmanaged\nincusbr0:bridge:unmanaged\nbr0:bridge:connected'
(MOCK_INCUS_NETWORK_RC=7 nm_unmanaged_guard_early "$guard_conf" >/dev/null 2>&1) || true
grep -Fq 'interface-name:incusbr0' "$guard_conf" && ! grep -Eq 'interface-name:br0(,|$)' "$guard_conf" \
  || fail "the early migration with a failed inventory listed: $(cat "$guard_conf")"
# With NetworkManager inactive nothing tells the bridges apart: every released name stays.
write_released
(MOCK_NM_STATE=inactive MOCK_INCUS_NETWORK_RC=7 nm_unmanaged_guard_early "$guard_conf" >/dev/null 2>&1) \
  || fail "the early migration with NetworkManager inactive failed"
grep -Fq 'interface-name:incusbr0' "$guard_conf" && grep -Eq 'interface-name:br0(,|$)' "$guard_conf" \
  || fail "the early migration with NetworkManager inactive dropped released names: $(cat "$guard_conf")"
# Teardown of a released guard without a hijack removes both files and the record.
write_released
MOCK_INCUS_NETWORKS='[]' nm_unmanaged_guard_reconcile "$guard_conf" >/dev/null || fail "teardown of a released guard failed: $POWER_ERROR"
! nm_path_exists "$guard_conf" && ! nm_path_exists "$legacy_conf" && [ ! -e "$POWER_NM_GUARD_STATE" ] || fail "teardown kept a released file"
rm -f "$conf_dir/00-admin.conf"
no_reload 'the migration'

# NetworkManager device-list semantics: order-independent, exceptions win, types from nmcli.
while IFS='|' read -r list device type want; do
  [ -n "$device" ] || continue
  if nm_list_selects "$list" "$device" "$type"; then got=0; else got=$?; fi
  [ "$got" = "$want" ] || fail "nm_list_selects '$list' $device $type returned $got, want $want"
done < "$ROOT/tests/fixtures/nm-list-selects.txt"

# Guard format classification matches the Go fixtures, dangling links included.
while IFS=' ' read -r name want; do
  cp "$ROOT/tests/fixtures/nm-guard-formats/$name.conf" "$tmp/format.conf"
  if nm_guard_format_current "$tmp/format.conf"; then got=current; else got=released; fi
  [ "$got" = "$want" ] || fail "guard format $name classified $got, want $want"
done < "$ROOT/tests/fixtures/nm-guard-formats/expected"
rm -f "$guard_conf" "$legacy_conf"
ln -sfn "$tmp/missing" "$guard_conf"
nm_guard_released "$guard_conf" || fail "a dangling guard link was not treated as released"
rm -f "$guard_conf"
ln -sfn "$tmp/missing" "$legacy_conf"
nm_guard_released "$guard_conf" || fail "a dangling legacy link was not treated as released"
rm -f "$legacy_conf"

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

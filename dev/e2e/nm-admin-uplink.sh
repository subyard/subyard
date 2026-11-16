#!/usr/bin/env bash
# NetworkManager guard acceptance on one disposable VM with real reboots.
#   upgrade: published 0.29.0 init stops after its guard (bad image), so the boot reconciler is
#            absent; that guard replaced the administrator's list and NetworkManager took the
#            uplink. The candidate update refuses and names the hand-back; after it, the update
#            rewrites the guard without a reload and asks for a reboot. Docker and libvirt
#            bridges, a restart experiment, the reboot, candidate init and start, a new veth,
#            a later managed=1 section, full teardown and a final reboot follow.
#   fresh:   an except-only administrator list, an NM-managed br0 and a Docker-style bridge
#            through a fresh candidate install, init, start and teardown, without a reboot.
# A watchdog restores the uplink address if it changes and fails the lane.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OLD_VERSION=0.29.0
OLD_INSTALLER_SHA256=cbf54fbfc846c2e936c53a0d7efa6366f83220e906dfb893f0671a057b29400d
OLD_BUNDLE_SHA256=9d9031ea8bd7c310d97a3d2060109c88754cc44f864d41b2f5c558710e77c3df

usage() {
  printf 'Usage: dev/e2e/nm-admin-uplink.sh --preflight | --slot N --lane upgrade|fresh\n' >&2
  exit 2
}

controller_reboot() {
  local before after down=0 up=0
  before="$(ssh -F "$CLIENT_CONFIG" -T e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id)"
  timeout --foreground 20 ssh -F "$CLIENT_CONFIG" -T -o ConnectTimeout=3 -o ConnectionAttempts=1 \
    -o ServerAliveInterval=2 -o ServerAliveCountMax=2 e2e-vm-1 -- sudo -n systemctl reboot \
    </dev/null >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    if ! timeout --foreground 5 ssh -F "$CLIENT_CONFIG" -T -o ConnectTimeout=2 -o ConnectionAttempts=1 \
      e2e-vm-1 -- true </dev/null >/dev/null 2>&1; then down=1; break; fi
    sleep 1
  done
  [ "$down" = 1 ] || die 'VM did not go down for reboot'
  for _ in $(seq 1 240); do
    after="$(timeout --foreground 6 ssh -F "$CLIENT_CONFIG" -T -o ConnectTimeout=3 -o ConnectionAttempts=1 \
      e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id 2>/dev/null)" || after=''
    if [ -n "$after" ] && [ "$after" != "$before" ]; then up=1; break; fi
    sleep 1
  done
  [ "$up" = 1 ] || die 'VM did not return with a new boot ID'
  printf 'evidence: preboot=%s postboot=%s\n' "$before" "$after"
}

controller_main() {
  local slot='' lane='' phase
  [ "$#" = 4 ] && [ "$1" = --slot ] && [ "$3" = --lane ] || usage
  slot="$2" lane="$4"
  case "$lane" in upgrade|fresh) ;; *) usage ;; esac
  # shellcheck source=dev/agent-e2e.sh
  . "$ROOT/dev/agent-e2e.sh"
  set_requested_slot "$slot" --slot
  # Consumed by the sourced lease client.
  # shellcheck disable=SC2034
  VM_COUNT_REQUESTED=1 LEASE_PURPOSE="nm-guard-$lane"
  LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
  NM_LANE_BUNDLE="$LOCAL_TEMP/worktree.tar.gz"
  trap 'cleanup_on_exit "" "$?"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  build_bundle "$ROOT" "$NM_LANE_BUNDLE"
  NM_LANE_HASH="$(sha256sum "$NM_LANE_BUNDLE" | awk '{print $1}')"
  acquire_lease
  start_lease_keeper
  NM_LANE_TOKEN="$LEASE_GENERATION"
  [[ "$NM_LANE_TOKEN" =~ ^[1-9][0-9]*$ ]] || die 'invalid lease generation'
  if [ "$lane" = fresh ]; then
    run_guest 1 "$NM_LANE_BUNDLE" "$NM_LANE_HASH" bash dev/e2e/nm-admin-uplink.sh fresh "$NM_LANE_TOKEN"
    cleanup_guest 1
    exit 0
  fi
  for phase in prepare resume final; do
    [ "$phase" = prepare ] || controller_reboot
    run_guest 1 "$NM_LANE_BUNDLE" "$NM_LANE_HASH" bash dev/e2e/nm-admin-uplink.sh "$phase" "$NM_LANE_TOKEN"
    cleanup_guest 1
  done
  exit 0
}

if [ "${1:-}" = --preflight ]; then
  bash -n "$0"
  bash "$ROOT/tests/lib-power-network.sh"
  bash "$ROOT/tests/init-network-convergence.sh"
  TMPDIR=/tmp go test ./internal/adapters/hostruntime ./internal/adapters/reconcileruntime \
    -run 'NetworkGuard|NetworkManager|RouteDevices|HostCheckReminds|TestPowerProbeSeparatesInstallFromFinalMetadata' -count=1
  exit
fi
case "${SUBYARD_E2E_VM:-}" in
  1) ;;
  *) controller_main "$@"; exit ;;
esac

# --- guest ------------------------------------------------------------------------------------
MODE="${1:?mode}" TOKEN="${2:?token}"
[[ "$TOKEN" =~ ^[0-9]+$ ]] || { printf 'nm-admin-uplink: invalid token\n' >&2; exit 2; }
STATE="/var/tmp/subyard-nm-guard-$TOKEN"
name="nmguard$TOKEN"
conf_dir=/etc/NetworkManager/conf.d
guard="$conf_dir/zz-subyard-unmanaged.conf"
admin_conf="$conf_dir/00-keep-uplink.conf"
pin=/etc/udev/rules.d/00-subyard-e2e-uplink-pin.rules
pending=/run/subyard/nm-guard-reboot-pending
launcher="$STATE/bin/yard"
export SUBYARD_OPERATOR_HOME="$STATE/home" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/runtime" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1

die() { printf 'nm-admin-uplink: %s\n' "$*" >&2; exit 2; }
pass() { printf 'PASS: %s\n' "$*"; }
evidence() { printf 'evidence: %s\n' "$*"; }
owner() {
  local command
  command=$(python3 -c 'import shlex,sys; print(shlex.join(sys.argv[1:]))' "$@")
  sg incus-admin -c "exec $command"
}
yard() { owner "$launcher" -Y "$name" "$@"; }
effective_unmanaged() {
  sudo -n NetworkManager --print-config | awk '
    /^\[/ { section = $0; next }
    section == "[keyfile]" && /^unmanaged-devices=/ { sub(/^[^=]*=/, ""); value = $0 }
    END { gsub(/;/, ",", value); print value }'
}
device_state() { nmcli -t -f DEVICE,STATE device | awk -F: -v d="$1" '$1 == d { print $2 }'; }
managedness() { nmcli -t -f DEVICE,STATE device | awk -F: '{ print $1 ":" ($2 == "unmanaged" ? "unmanaged" : "managed") }' | sort; }
address_of() { ip -4 -o addr show dev "$1" scope global 2>/dev/null | awk '{print $4; exit}'; }
settle() { sleep "${1:-8}"; }
nm_reloads_since() { sudo -n journalctl -u NetworkManager --since "@$1" --no-pager -q 2>/dev/null | grep -Eci 'config: (signal|update)|reload' || true; }
load_uplink() { # shellcheck disable=SC1091
  . "$STATE/uplink.env"
}
watchdog_start() {
  cat > "$STATE/watchdog.sh" <<WATCHDOG
#!/bin/bash
while :; do
  if [ "\$(ip -4 -o addr show dev $UP scope global | awk '{print \$4; exit}')" != "$UP_ADDRESS" ]; then
    printf '%s address changed\n' "\$(date -Is)" >> "$STATE/watchdog.log"
    ip addr add "$UP_ADDRESS" dev "$UP" 2>/dev/null
    ip route replace default via "$UP_GATEWAY" dev "$UP"
  fi
  sleep 1
done
WATCHDOG
  chmod 0755 "$STATE/watchdog.sh"
  sudo -n systemctl stop "subyard-e2e-nm-watchdog-$TOKEN" 2>/dev/null || true
  sudo -n systemd-run --unit "subyard-e2e-nm-watchdog-$TOKEN" --collect "$STATE/watchdog.sh" >/dev/null
}
watchdog_clean() { [ ! -s "$STATE/watchdog.log" ] || die "watchdog restored the uplink: $(cat "$STATE/watchdog.log")"; }
uplink_admins() { # <label>: unmanaged and addressed, no NetworkManager profile active on it
  [ "$(device_state "$UP")" = unmanaged ] || die "$1: NetworkManager manages the uplink $UP ($(device_state "$UP"))"
  [ -n "$(address_of "$UP")" ] || die "$1: the uplink $UP has no IPv4 address"
  ip -4 route show default | grep -q " dev $UP" || die "$1: the default route left $UP"
}
yard_devices_unmanaged() { # <label>
  local device state checked=0
  while IFS=: read -r device state; do
    case "$device" in incusbr0|veth*) ;; *) continue ;; esac
    [ "$state" = unmanaged ] || die "$1: yard device $device is $state"
    checked=$((checked + 1))
  done < <(nmcli -t -f DEVICE,STATE device)
  [ "$checked" -ge 1 ] || die "$1: no yard device was present"
}
# No LIMITS_* settings: full teardown currently refuses them as adapter context keys.
write_yard_config() { # <image>
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
  cat > "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
SSH_PORT=$((36000 + TOKEN % 10000))
YARD_KIND=container
YARD_IMAGE=$1
YARD_IMAGE_FALLBACK=$1
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=git-fetch
DEV_USER=dev
DEV_UID=1001
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
}
install_network_manager() {
  # shellcheck disable=SC2024 # the operator owns the log
  sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends network-manager \
    >"$STATE/apt.log" 2>&1 || die 'network-manager install failed'
  settle
  evidence "NetworkManager $(NetworkManager --version)"
}
pin_uplink() { # on|off
  if [ "$1" = on ]; then
    printf 'ENV{INTERFACE}=="%s", ENV{NM_UNMANAGED}="1"\n' "$UP" | sudo -n tee "$pin" >/dev/null
  else
    sudo -n rm -f "$pin"
  fi
  sudo -n udevadm control --reload
  sudo -n udevadm trigger --action=change --subsystem-match=net
  sudo -n udevadm settle
}
install_candidate() {
  owner env YARD_RELEASE_BASE_URL="file://$ROOT/.subyard-acceptance/release" YARD_RELEASE_CACHE="$STATE/releases" \
    YARD_BIN_DIR="$STATE/bin" YARD_SHELL_RC="$STATE/bashrc" YARD_LOGIN_RC="$STATE/profile" \
    bash "$ROOT/.subyard-acceptance/release/subyard-install.sh" --version "$CANDIDATE_VERSION" --yes \
    >"$STATE/candidate-install.log" 2>&1 || die 'candidate install failed'
}
dump_logs() {
  local log
  for log in "$STATE"/*.log; do
    [ -f "$log" ] || continue
    printf '%s (last 40 lines):\n' "${log##*/}"
    tail -n 40 "$log"
  done
}
trap 'rc=$?; [ "$rc" = 0 ] || dump_logs; exit "$rc"' EXIT

sudo -n true || die 'passwordless sudo is required on the disposable VM'
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] || die 'a frozen candidate bundle is required'
CANDIDATE_VERSION="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"

case "$MODE" in
prepare)
  install -d -m 0700 "$STATE" "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
  UP="$(ip -4 route show default | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit }}')"
  UP_GATEWAY="$(ip -4 route show default | awk '{print $3; exit}')"
  UP_ADDRESS="$(ip -4 -o addr show dev "$UP" scope global | awk '{print $4; exit}')"
  [ -n "$UP" ] && [ -n "$UP_ADDRESS" ] && [ -n "$UP_GATEWAY" ] || die 'cannot identify the VM uplink'
  printf 'UP=%q\nUP_ADDRESS=%q\nUP_GATEWAY=%q\n' "$UP" "$UP_ADDRESS" "$UP_GATEWAY" > "$STATE/uplink.env"
  # Docker- and libvirt-style bridges that persist across reboots, owned by networkd.
  for bridge in docker0:172.17.0.1/16 virbr0:192.168.122.1/24; do
    printf '[NetDev]\nName=%s\nKind=bridge\n' "${bridge%%:*}" | sudo -n tee "/etc/systemd/network/50-e2e-${bridge%%:*}.netdev" >/dev/null
    printf '[Match]\nName=%s\n[Network]\nAddress=%s\nConfigureWithoutCarrier=yes\n' "${bridge%%:*}" "${bridge#*:}" \
      | sudo -n tee "/etc/systemd/network/50-e2e-${bridge%%:*}.network" >/dev/null
  done
  sudo -n networkctl reload
  sleep 3
  [ -n "$(address_of docker0)" ] && [ -n "$(address_of virbr0)" ] || die 'networkd did not create docker0 and virbr0'
  # The administrator keeps the uplink out of NetworkManager; a static profile with the uplink's own
  # address lets the released guard's takeover keep the VM reachable.
  sudo -n install -d -m 0755 "$conf_dir"
  printf '[keyfile]\nunmanaged-devices=interface-name:%s\n' "$UP" | sudo -n tee "$admin_conf" >/dev/null
  watchdog_start
  install_network_manager
  uplink_admins 'NetworkManager install'
  sudo -n nmcli connection add type ethernet ifname "$UP" con-name e2e-uplink autoconnect yes \
    ipv4.method manual ipv4.addresses "$UP_ADDRESS" ipv4.gateway "$UP_GATEWAY" ipv6.method ignore >/dev/null
  pass "NetworkManager leaves the administrator's uplink $UP unmanaged"

  bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
  export STORAGE_PATH
  STORAGE_PATH="$(sudo -n /usr/bin/incus storage get default source)"
  curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-all-errors --retry-delay 2 --connect-timeout 20 --max-time 300 \
    "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" -o "$STATE/subyard-install.sh"
  [ "$(sha256sum "$STATE/subyard-install.sh" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] || die 'published installer checksum changed'
  owner env YARD_RELEASE_VERSION="$OLD_VERSION" YARD_RELEASE_CACHE="$STATE/releases" YARD_BIN_DIR="$STATE/bin" \
    YARD_SHELL_RC="$STATE/bashrc" YARD_LOGIN_RC="$STATE/profile" \
    bash "$STATE/subyard-install.sh" --version "$OLD_VERSION" --yes >"$STATE/install.log" 2>&1 || die 'published install failed'
  archive="$(find "$STATE/releases" -name "subyard-$OLD_VERSION-linux-amd64.tar.gz" -type f -print -quit)"
  [ -n "$archive" ] && [ "$(sha256sum "$archive" | awk '{print $1}')" = "$OLD_BUNDLE_SHA256" ] || die 'published archive checksum changed'
  # The published init writes its guard in the Incus stage, then stops at the missing image.
  write_yard_config images:subyard-e2e/does-not-exist
  if yard init --yes >"$STATE/published-init.log" 2>&1; then die 'the published init unexpectedly succeeded'; fi
  grep -q '^\[device-subyard\]' "$guard" && grep -q '^unmanaged-devices=' "$guard" || die 'the published init wrote no released guard'
  [ ! -e /etc/systemd/system/subyard-power-reconcile.service ] || die 'the published partial init installed the boot reconciler'
  settle
  [ "$(device_state "$UP")" != unmanaged ] || die 'the released guard did not let NetworkManager take the uplink'
  case ",$(effective_unmanaged)," in *",interface-name:$UP,"*) die 'the released guard kept the administrator list' ;; esac
  for bridge in docker0 virbr0; do [ "$(device_state "$bridge")" = unmanaged ] || die "released guard: $bridge is managed"; done
  pass "published $OLD_VERSION partial init: the released guard let NetworkManager take $UP; no boot reconciler"

  guard_hash="$(sha256sum "$guard")"
  invocation="$(systemctl show -p InvocationID --value NetworkManager)"
  if owner env YARD_RELEASE_BASE_URL="file://$ROOT/.subyard-acceptance/release" \
    "$launcher" update --version "$CANDIDATE_VERSION" --yes >"$STATE/update-refused.log" 2>&1; then
    die 'the update migrated while NetworkManager manages the administrator uplink'
  fi
  grep -Fq "NetworkManager manages $UP" "$STATE/update-refused.log" \
    && grep -Fq "sudo nmcli device set $UP managed no" "$STATE/update-refused.log" \
    || die 'the update refusal did not name the uplink and its hand-back'
  [ "$(sha256sum "$guard")" = "$guard_hash" ] || die 'the refused update changed the guard'
  [ "$(systemctl show -p InvocationID --value NetworkManager)" = "$invocation" ] || die 'the refused update restarted NetworkManager'
  pass 'the update refused, naming the uplink and the hand-back command, and changed nothing'

  sudo -n nmcli device set "$UP" managed no
  settle 3
  # The released guard's takeover may briefly reconfigure the uplink; that is the reproduced
  # defect, not the candidate. From the hand-back on, any watchdog restore fails the lane.
  if [ -s "$STATE/watchdog.log" ]; then
    sed 's/^/evidence: watchdog during the released takeover: /' "$STATE/watchdog.log"
    : > "$STATE/watchdog.log"
  fi
  [ "$(device_state "$UP")" = unmanaged ] && [ "$(address_of "$UP")" = "$UP_ADDRESS" ] || die 'the hand-back lost the uplink'
  before_table="$(managedness)"
  since="$(date +%s)"
  owner env YARD_RELEASE_BASE_URL="file://$ROOT/.subyard-acceptance/release" \
    "$launcher" update --version "$CANDIDATE_VERSION" --yes >"$STATE/update.log" 2>&1 \
    || { owner "$launcher" update --yes >>"$STATE/update.log" 2>&1 || die 'the update failed after the hand-back'; }
  [ "$(owner "$launcher" version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate is not active'
  settle 3
  grep -q '^unmanaged-devices' "$guard" && die 'the update kept the released guard'
  grep -q '^\[device-subyard\]' "$guard" || die 'the update wrote no guard section'
  [ ! -e /etc/systemd/system/subyard-power-reconcile.service ] || die 'the guard-only migration installed the boot reconciler'
  [ "$(cat "$pending")" = "$(cat /proc/sys/kernel/random/boot_id)" ] || die 'the update recorded no pending reboot'
  [ "$(systemctl show -p InvocationID --value NetworkManager)" = "$invocation" ] || die 'the update restarted NetworkManager'
  [ "$(nm_reloads_since "$since")" = 0 ] || die "the update reloaded NetworkManager: $(sudo -n journalctl -u NetworkManager --since "@$since" -q | tail -5)"
  [ "$(managedness)" = "$before_table" ] || die 'the update changed a device managed state'
  grep -Fq 'reboot the host once' "$STATE/update.log" || evidence 'update output has no reboot Next line (the published runtime printed it)'
  owner "$launcher" status >"$STATE/status.log" 2>&1 || true
  grep -Fq 'Reminder: reboot the host once' "$STATE/status.log" || die 'yard status shows no reboot reminder'
  owner "$launcher" check >"$STATE/check.log" 2>&1 || true
  grep -Fq 'reboot the host once' "$STATE/check.log" || die 'yard check shows no reboot reminder'
  pass 'after the hand-back the update rewrote the guard without a reload or restart; status and check ask for a reboot'

  # Would a NetworkManager restart do instead of a reboot? Observe, without asserting.
  {
    printf 'before restart:\n'; nmcli -t -f DEVICE,TYPE,STATE,CONNECTION device
    for device in "$UP" docker0 virbr0 incusbr0; do printf '  %s %s\n' "$device" "$(address_of "$device")"; done
  } > "$STATE/restart-experiment.txt"
  sudo -n systemctl restart NetworkManager
  settle 10
  {
    printf 'after restart:\n'; nmcli -t -f DEVICE,TYPE,STATE,CONNECTION device
    for device in "$UP" docker0 virbr0 incusbr0; do printf '  %s %s\n' "$device" "$(address_of "$device")"; done
    printf 'uplink owner after restart: %s\n' "$(nmcli -t -f GENERAL.STATE,GENERAL.CONNECTION device show "$UP" | tr '\n' ' ')"
  } >> "$STATE/restart-experiment.txt"
  sed 's/^/evidence: restart-experiment: /' "$STATE/restart-experiment.txt"
  watchdog_clean
  pass 'restart experiment recorded'
  ;;

resume)
  load_uplink
  watchdog_start
  [ ! -e "$pending" ] || [ "$(cat "$pending")" != "$(cat /proc/sys/kernel/random/boot_id)" ] || die 'the reboot left the reminder pending'
  owner "$launcher" status >"$STATE/status-after.log" 2>&1 || true
  ! grep -Fq 'Reminder: reboot the host once' "$STATE/status-after.log" || die 'yard status still asks for a reboot'
  uplink_admins 'after the reboot'
  ! nmcli -t -f NAME,DEVICE connection show --active | grep -q ":$UP$" || die 'an NetworkManager profile is active on the uplink'
  [ "$(effective_unmanaged)" = "interface-name:$UP" ] || die "effective unmanaged-devices is not exactly the administrator's: $(effective_unmanaged)"
  for bridge in docker0 virbr0; do
    [ -n "$(address_of "$bridge")" ] || die "$bridge lost its address after the reboot"
    evidence "after reboot $bridge is $(device_state "$bridge") with $(address_of "$bridge")"
  done
  pass 'after the reboot the uplink is the administrator'"'"'s and docker0/virbr0 keep their addresses'

  write_yard_config images:debian/13/cloud
  before_table="$(managedness)"
  yard init --yes >"$STATE/candidate-init.log" 2>&1 || die 'candidate init failed'
  yard start --yes >"$STATE/start.log" 2>&1 || die 'candidate start failed'
  settle
  uplink_admins 'candidate init and start'
  yard_devices_unmanaged 'running yard'
  [ -n "$(address_of incusbr0)" ] || die 'the yard bridge has no address'
  for line in $before_table; do
    case "$line" in incusbr0:*|veth*) continue ;; esac
    grep -Fxq "$line" <<< "$(managedness)" || die "init changed the managed state of ${line%%:*}"
  done
  [ -e /etc/systemd/system/subyard-power-reconcile.service ] || die 'candidate init did not install the boot reconciler'
  pass 'candidate init and start keep every other device; the yard bridge and veths are unmanaged'

  sudo -n ip link add vethe2e0 type veth peer name vethe2e1
  sudo -n ip tuntap add tap1234abcd mode tap
  sudo -n ip link set vethe2e0 up; sudo -n ip link set tap1234abcd up
  settle 4
  for device in vethe2e0 vethe2e1 tap1234abcd; do
    [ "$(device_state "$device")" = unmanaged ] || die "new device $device was born $(device_state "$device")"
  done
  sudo -n ip link del vethe2e0; sudo -n ip tuntap del tap1234abcd mode tap
  pass 'a new veth pair and an Incus-style tap device are born unmanaged'

  yard stop --yes >"$STATE/stop.log" 2>&1 || die 'yard stop failed'
  printf '[device-e2e-later]\nmatch-device=type:veth\nmanaged=1\n' | sudo -n tee "$conf_dir/zzz-e2e-later.conf" >/dev/null
  sudo -n nmcli general reload conf
  if yard start --yes >"$STATE/start-refused.log" 2>&1; then die 'start accepted a later managed=1 section'; fi
  grep -Fq '[device-e2e-later] has priority over [device-subyard]' "$STATE/start-refused.log" \
    || die 'the start refusal did not name the later section'
  sudo -n rm -f "$conf_dir/zzz-e2e-later.conf"
  sudo -n nmcli general reload conf
  yard start --yes >"$STATE/start-again.log" 2>&1 || die 'start failed after removing the later section'
  pass 'a later managed=1 section for veths refuses the start, naming it'

  owner "$launcher" teardown --full --yes >"$STATE/teardown.log" 2>&1 || die 'full teardown failed'
  settle
  [ ! -e "$guard" ] || die 'full teardown kept the guard'
  [ -f "$admin_conf" ] || die 'full teardown removed the administrator file'
  uplink_admins 'full teardown'
  [ "$(effective_unmanaged)" = "interface-name:$UP" ] || die 'teardown changed the administrator list'
  watchdog_clean
  pass 'full teardown removed only the Subyard guard'
  ;;

final)
  load_uplink
  uplink_admins 'after the final reboot'
  [ "$(effective_unmanaged)" = "interface-name:$UP" ] || die 'the final boot has a different unmanaged-devices list'
  [ ! -e "$guard" ] || die 'the guard came back'
  pass 'after the final reboot NetworkManager runs only the administrator configuration'
  ;;

fresh)
  install -d -m 0700 "$STATE" "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
  UP="$(ip -4 route show default | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit }}')"
  UP_GATEWAY="$(ip -4 route show default | awk '{print $3; exit}')"
  UP_ADDRESS="$(ip -4 -o addr show dev "$UP" scope global | awk '{print $4; exit}')"
  printf 'UP=%q\nUP_ADDRESS=%q\nUP_GATEWAY=%q\n' "$UP" "$UP_ADDRESS" "$UP_GATEWAY" > "$STATE/uplink.env"
  # An except-only list: NetworkManager manages br0 and nothing else.
  sudo -n install -d -m 0755 "$conf_dir"
  printf '[keyfile]\nunmanaged-devices=except:interface-name:br0\n' | sudo -n tee "$admin_conf" >/dev/null
  watchdog_start
  install_network_manager
  uplink_admins 'NetworkManager install'
  sudo -n nmcli connection add type bridge ifname br0 con-name e2e-br0 autoconnect yes bridge.stp no \
    ipv4.method manual ipv4.addresses 198.19.0.1/24 ipv6.method ignore >/dev/null
  sudo -n ip link add br-0123456789ab type bridge
  sudo -n ip addr add 198.18.7.1/24 dev br-0123456789ab
  sudo -n ip link set br-0123456789ab up
  settle
  [ "$(device_state br0)" = connected ] || die "NetworkManager did not run br0 ($(device_state br0))"
  docker_style="$(device_state br-0123456789ab)"
  bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
  export STORAGE_PATH
  STORAGE_PATH="$(sudo -n /usr/bin/incus storage get default source)"
  install_candidate
  launcher="$STATE/bin/yard"
  write_yard_config images:debian/13/cloud
  yard init --yes >"$STATE/init.log" 2>&1 || die 'candidate init failed'
  yard start --yes >"$STATE/start.log" 2>&1 || die 'candidate start failed'
  settle
  uplink_admins 'except-only list through init and start'
  [ "$(effective_unmanaged)" = 'except:interface-name:br0' ] || die "effective list differs from the administrator's: $(effective_unmanaged)"
  [ "$(device_state br0)" = connected ] && [ "$(address_of br0)" = 198.19.0.1/24 ] || die 'init touched the NM-managed br0'
  [ "$(device_state br-0123456789ab)" = "$docker_style" ] && [ "$(address_of br-0123456789ab)" = 198.18.7.1/24 ] \
    || die 'init touched the Docker-style bridge'
  grep -Eq 'interface-name:(br0|br-0123456789ab)(,|$)' "$guard" && die 'the guard lists a bridge Incus does not manage'
  yard_devices_unmanaged 'running yard'
  pass 'except-only list, NM-managed br0 and a Docker-style bridge are untouched by install, init and start'
  owner "$launcher" teardown --full --yes >"$STATE/teardown.log" 2>&1 || die 'full teardown failed'
  settle
  [ ! -e "$guard" ] && [ -f "$admin_conf" ] || die 'teardown changed the wrong files'
  uplink_admins 'teardown'
  [ "$(device_state br0)" = connected ] || die 'teardown touched br0'
  watchdog_clean
  pass 'teardown removed only the Subyard guard; the uplink and br0 are untouched'
  ;;
*) die "unknown mode $MODE" ;;
esac

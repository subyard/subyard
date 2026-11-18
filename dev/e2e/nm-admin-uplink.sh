#!/usr/bin/env bash
# NetworkManager guard acceptance on one disposable VM with real reboots.
#   upgrade: published 0.29.0 init stops after its guard (bad image), so the boot reconciler is
#            absent; that guard replaced the administrator's list and NetworkManager took the
#            uplink. The candidate update refuses and names the hand-back; after it, the update
#            rewrites the guard without a reload and asks for a reboot. Docker and libvirt
#            bridges, a restart experiment, the reboot, candidate init and start, a new veth,
#            a later managed=1 section, teardown and a final reboot follow.
#   fresh:   an except-only administrator list, an NM-managed br0 and a Docker-style bridge
#            through a fresh candidate install whose first init stops for a reboot (a
#            NetworkManager restart does not count), then init, start, a repair that waits for
#            another reboot, the boot restore and teardown.
# A watchdog restores the uplink address if it changes and fails the lane.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OLD_VERSION=0.29.0
OLD_INSTALLER_SHA256=cbf54fbfc846c2e936c53a0d7efa6366f83220e906dfb893f0671a057b29400d
OLD_BUNDLE_SHA256=9d9031ea8bd7c310d97a3d2060109c88754cc44f864d41b2f5c558710e77c3df

usage() {
  printf 'Usage: dev/e2e/nm-admin-uplink.sh --preflight | --slot N --lane upgrade|fresh [--image debian-13|ubuntu-24.04|ubuntu-26.04]\n' >&2
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
  { [ "$#" = 4 ] || [ "$#" = 6 ]; } && [ "$1" = --slot ] && [ "$3" = --lane ] || usage
  slot="$2" lane="$4"
  case "$lane" in upgrade|fresh) ;; *) usage ;; esac
  # shellcheck source=dev/agent-e2e.sh
  . "$ROOT/dev/agent-e2e.sh"
  if [ "$#" = 6 ]; then
    [ "$5" = --image ] || usage
    allocation_option --image "$6"
  fi
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
  phases=(prepare resume final)
  [ "$lane" = upgrade ] || phases=(fresh-prepare fresh-resume fresh-final)
  for phase in "${phases[@]}"; do
    case "$phase" in prepare|fresh-prepare) ;; *) controller_reboot ;; esac
    run_guest 1 "$NM_LANE_BUNDLE" "$NM_LANE_HASH" bash dev/e2e/nm-admin-uplink.sh "$phase" "$NM_LANE_TOKEN"
    cleanup_guest 1
  done
  exit 0
}

if [ "${1:-}" = --preflight ]; then
  bash -n "$0"
  TMPDIR=/tmp bash "$ROOT/dev/build-engine.sh"
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
state_record=/var/lib/subyard/nm-guard-state
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
address_of() { ip -4 -o addr show dev "$1" scope global 2>/dev/null | awk 'NR == 1 {print $4}'; }
settle() { sleep "${1:-8}"; }
nm_reloads_since() { sudo -n journalctl -u NetworkManager --since "@$1" --no-pager -q 2>/dev/null | grep -Eci 'config: (signal|update)|reload' || true; }
load_uplink() { # shellcheck disable=SC1091
  . "$STATE/uplink.env"
}
watchdog_start() {
  cat > "$STATE/watchdog.sh" <<WATCHDOG
#!/bin/bash
while :; do
  if [ "\$(ip -4 -o addr show dev $UP scope global | awk 'NR == 1 {print \$4}')" != "$UP_ADDRESS" ]; then
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
  ip -4 route show default | grep -a >/dev/null " dev $UP" || die "$1: the default route left $UP"
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
  UP="$(ip -4 route show default | awk '!found {for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); found=1; break }}')"
  UP_GATEWAY="$(ip -4 route show default | awk 'NR == 1 {print $3}')"
  UP_ADDRESS="$(ip -4 -o addr show dev "$UP" scope global | awk 'NR == 1 {print $4}')"
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
  # The list also has a glob Subyard cannot decide for an NM-managed device (the docker* variant).
  printf '[keyfile]\nunmanaged-devices=interface-name:%s;interface-name:docker*\n' "$UP" | sudo -n tee "$admin_conf" >/dev/null
  # The fixture platform runs before NetworkManager exists, so it writes no guard.
  bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
  watchdog_start
  install_network_manager
  uplink_admins 'NetworkManager install'
  sudo -n nmcli connection add type ethernet ifname "$UP" con-name e2e-uplink autoconnect yes \
    ipv4.method manual ipv4.addresses "$UP_ADDRESS" ipv4.gateway "$UP_GATEWAY" ipv6.method ignore >/dev/null
  # The released guard listed every host bridge, so the undecidable case needs a non-bridge
  # device NetworkManager runs: a dummy.
  sudo -n nmcli connection add type dummy ifname dum0 con-name e2e-dum0 autoconnect yes \
    ipv4.method manual ipv4.addresses 198.19.5.1/24 ipv6.method ignore >/dev/null
  settle
  [ "$(device_state dum0)" = connected ] || die "NetworkManager did not run dum0 ($(device_state dum0))"
  pass "NetworkManager leaves the administrator's uplink $UP unmanaged and runs dum0"

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
    sudo -n truncate -s 0 "$STATE/watchdog.log"
  fi
  [ "$(device_state "$UP")" = unmanaged ] && [ "$(address_of "$UP")" = "$UP_ADDRESS" ] || die 'the hand-back lost the uplink'
  before_table="$(managedness)"
  since="$(date +%s)"
  # The refused update left the candidate active and its transition recovering; one plain
  # update finishes it, and that single run must succeed on its own.
  owner "$launcher" update --yes >"$STATE/update.log" 2>&1 || die 'the update failed after the hand-back'
  grep -Fq 'Next: reboot the host once' "$STATE/update.log" || die 'the update did not ask for the reboot'
  [ "$(owner "$launcher" version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate is not active'
  settle 3
  grep -q '^unmanaged-devices' "$guard" && die 'the update kept the released guard'
  grep -q '^\[device-subyard\]' "$guard" || die 'the update wrote no guard section'
  [ ! -e /etc/systemd/system/subyard-power-reconcile.service ] || die 'the guard-only migration installed the boot reconciler'
  # Every up Incus bridge the released guard named was unmanaged: evidence that NetworkManager held
  # an adequate guard, so the migration is `held` and only waits for the reboot.
  [ "$(cut -d ' ' -f1 "$state_record")" = "$(cat /proc/sys/kernel/random/boot_id)" ] \
    && [ "$(cut -d ' ' -f4,5 "$state_record")" = 'held migration' ] || die "the update record: $(cat "$state_record" 2>/dev/null)"
  [ "$(systemctl show -p InvocationID --value NetworkManager)" = "$invocation" ] || die 'the update restarted NetworkManager'
  [ "$(nm_reloads_since "$since")" = 0 ] || die "the update reloaded NetworkManager: $(sudo -n journalctl -u NetworkManager --since "@$since" -q | tail -5)"
  [ "$(managedness)" = "$before_table" ] || die 'the update changed a device managed state'
  if owner "$launcher" update --rollback --yes >"$STATE/rollback.log" 2>&1; then
    die 'rollback to the released guard was accepted'
  fi
  # The rollback-compatibility blocker (the device-guard epoch), not another rollback-incompatible one.
  # Its message is unique to that blocker (resource transition.rollback-compatibility).
  sed 's/^/evidence: rollback: /' "$STATE/rollback.log"
  # The source provenance check runs before it and refuses a published 0.29.0 target first; the
  # CLI exposes no rollback check past it, so that case is a stated limit and the epoch blocker
  # itself is proven host-free (go:releasetransition). Any other refusal fails the lane.
  grep -Fq 'code=rollback-incompatible' "$STATE/rollback.log" || die 'rollback was refused without code=rollback-incompatible'
  if grep -Fq 'the retained release cannot safely accept the current migration ledger' "$STATE/rollback.log"; then
    evidence 'rollback refused by the rollback-compatibility blocker'
  elif grep -Fq 'source release has invalid or foreign V2 registry provenance' "$STATE/rollback.log"; then
    evidence 'LIMIT: rollback refused by the source provenance check, which runs before the rollback-compatibility blocker for every published target before this epoch; that blocker is proven host-free'
  else
    die 'rollback was refused by neither the rollback-compatibility blocker nor the provenance check'
  fi
  # The installed 0.29.0 and the candidate registries differ only by the NetworkManager device
  # guard migration and power-metadata 3 -> 4.
  installed_registry="$(find "$STATE/runtime/releases" -path "*$OLD_VERSION*/config/release-transition.json" -print -quit)"
  [ -n "$installed_registry" ] || die 'cannot find the installed 0.29.0 release-transition.json'
  jq -S '.currentEpochs["power-metadata"] = 3 | .migrations |= map(select(.id != "introduce-nm-device-guard-v4"))' \
    "$ROOT/config/release-transition.json" > "$STATE/candidate-registry.json"
  jq -S . "$installed_registry" > "$STATE/installed-registry.json"
  cmp -s "$STATE/candidate-registry.json" "$STATE/installed-registry.json" \
    || die "the registries differ beyond the device guard boundary: $(diff "$STATE/installed-registry.json" "$STATE/candidate-registry.json" | sed -n '1,20p')"
  jq -e '.currentEpochs["power-metadata"] == 4 and ([.migrations[] | select(.id == "introduce-nm-device-guard-v4" and .fromEpoch == 3 and .toEpoch == 4)] | length == 1)' \
    "$ROOT/config/release-transition.json" >/dev/null || die 'the candidate registry lacks the device guard boundary'
  [ "$(owner "$launcher" version)" = "yard $CANDIDATE_VERSION" ] || die 'the refused rollback changed the active release'
  pass 'rollback to the published release is refused before activation'
  owner "$launcher" status >"$STATE/status.log" 2>&1 || true
  grep -Fq 'Reminder: reboot the host once' "$STATE/status.log" || die 'yard status shows no reboot reminder'
  owner "$launcher" check >"$STATE/check.log" 2>&1 || true
  grep -Fq 'reboot the host once' "$STATE/check.log" || die 'yard check shows no reboot reminder'
  grep -Fq "cannot decide whether the administrator's unmanaged-devices list" "$STATE/update.log" \
    && grep -Eq 'manages: ([^ ]+, )*dum0' "$STATE/update.log" \
    && grep -Fq 'Do not reload or restart NetworkManager before the reboot' "$STATE/update.log" \
    || die "the update gave no notice for the docker* list and the NM-managed dum0; devices before the update: $before_table; now: $(nmcli -t -f DEVICE,TYPE,STATE device | tr '\n' ' '); list: $(effective_unmanaged)"
  [ "$(device_state dum0)" = connected ] || die 'the migration touched dum0'
  pass 'after the hand-back the update rewrote the guard without a reload or restart; status and check ask for a reboot; the docker* list gave a notice for dum0'

  # Waiting for the reboot blocks nothing: the released section in memory covers the yard.
  write_yard_config images:debian/13/cloud
  yard init --yes >"$STATE/pre-reboot-init.log" 2>&1 || die 'init failed while the migration waits for the reboot'
  yard start --yes >"$STATE/pre-reboot-start.log" 2>&1 || die 'start failed while the migration waits for the reboot'
  yard stop --yes >"$STATE/pre-reboot-stop.log" 2>&1 || die 'stop failed while the migration waits for the reboot'
  yard start --yes >"$STATE/pre-reboot-start2.log" 2>&1 || die 'restart failed while the migration waits for the reboot'
  settle
  [ "$(systemctl show -p InvocationID --value NetworkManager)" = "$invocation" ] || die 'init or start restarted NetworkManager'
  uplink_admins 'init and start before the reboot'
  yard_devices_unmanaged 'yard before the reboot'
  pass 'init, start, stop and start work before the reboot without touching NetworkManager'

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
  [ -e "$state_record" ] && [ "$(cut -d ' ' -f1 "$state_record")" != "$(cat /proc/sys/kernel/random/boot_id)" ] \
    || die "the persistent record did not survive the reboot or names this boot: $(cat "$state_record" 2>/dev/null)"
  owner "$launcher" status >"$STATE/status-after.log" 2>&1 || true
  ! grep -Fq 'Reminder: reboot the host once' "$STATE/status-after.log" || die 'yard status still asks for a reboot'
  uplink_admins 'after the reboot'
  ! nmcli -t -f NAME,DEVICE connection show --active | grep -a >/dev/null ":$UP$" || die 'an NetworkManager profile is active on the uplink'
  [ "$(effective_unmanaged)" = "interface-name:$UP,interface-name:docker*" ] || die "effective unmanaged-devices is not exactly the administrator's: $(effective_unmanaged)"
  for bridge in docker0 virbr0; do
    [ -n "$(address_of "$bridge")" ] || die "$bridge lost its address after the reboot"
    evidence "after reboot $bridge is $(device_state "$bridge") with $(address_of "$bridge")"
  done
  pass 'after the reboot the uplink is the administrator'"'"'s and docker0/virbr0 keep their addresses'

  for _ in $(seq 1 60); do
    sudo -n /usr/bin/incus list --all-projects -f csv -c ns 2>/dev/null | grep -a >/dev/null ",RUNNING$" && break
    sleep 3
  done
  sudo -n /usr/bin/incus list --all-projects -f csv -c ns | grep -a >/dev/null ",RUNNING$" || die 'the boot reconciler did not restore the running yard'
  yard_devices_unmanaged 'yard restored at boot'
  [ "$(device_state dum0)" = connected ] || die 'the reboot lost dum0'
  pass 'the boot reconciler restored the running yard with its veth unmanaged'
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

  # The fixture platform owns the storage pool, so full teardown refuses; the ordinary teardown
  # keeps the shared bridge and therefore the guard for it.
  yard teardown --yes >"$STATE/teardown.log" 2>&1 || die 'teardown failed'
  settle
  [ -f "$admin_conf" ] || die 'teardown removed the administrator file'
  if ip link show incusbr0 >/dev/null 2>&1; then
    grep -Fq 'interface-name:incusbr0' "$guard" || die 'teardown dropped the guard of the surviving bridge'
  else
    [ ! -e "$guard" ] || die 'teardown kept the guard after the last Incus bridge'
  fi
  uplink_admins 'teardown'
  [ "$(effective_unmanaged)" = "interface-name:$UP,interface-name:docker*" ] || die 'teardown changed the administrator list'
  watchdog_clean
  pass 'teardown left the administrator configuration and the uplink untouched'
  ;;

final)
  load_uplink
  uplink_admins 'after the final reboot'
  [ "$(effective_unmanaged)" = "interface-name:$UP,interface-name:docker*" ] || die 'the final boot has a different unmanaged-devices list'
  ! grep -q '^unmanaged-devices' "$guard" 2>/dev/null || die 'a released guard came back'
  pass 'after the final reboot the uplink and unmanaged-devices list are the administrator'"'"'s alone'
  ;;

fresh-prepare)
  install -d -m 0700 "$STATE" "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
  UP="$(ip -4 route show default | awk '!found {for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); found=1; break }}')"
  UP_GATEWAY="$(ip -4 route show default | awk 'NR == 1 {print $3}')"
  UP_ADDRESS="$(ip -4 -o addr show dev "$UP" scope global | awk 'NR == 1 {print $4}')"
  printf 'UP=%q\nUP_ADDRESS=%q\nUP_GATEWAY=%q\n' "$UP" "$UP_ADDRESS" "$UP_GATEWAY" > "$STATE/uplink.env"
  # The fixture platform runs before NetworkManager exists, so it writes no guard.
  bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
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
  device_state br-0123456789ab > "$STATE/docker-style.state"
  # An administrator file nobody has applied yet; Subyard never reloads, so it stays unapplied.
  printf '[keyfile]\nunmanaged-devices=interface-name:br0\n' | sudo -n tee "$conf_dir/50-e2e-later.conf" >/dev/null
  export STORAGE_PATH
  STORAGE_PATH="$(sudo -n /usr/bin/incus storage get default source)"
  install_candidate
  launcher="$STATE/bin/yard"
  write_yard_config images:debian/13/cloud
  invocation="$(systemctl show -p InvocationID --value NetworkManager)"
  before_table="$(managedness)"
  if yard init --yes >"$STATE/init-first.log" 2>&1; then die 'the first init continued before NetworkManager read the guard'; fi
  grep -Fq 'Reboot the host once' "$STATE/init-first.log" && ! grep -Fq 'restart NetworkManager (' "$STATE/init-first.log" \
    || die "the first init did not ask for a reboot only: $(tail -5 "$STATE/init-first.log")"
  [ "$(systemctl show -p InvocationID --value NetworkManager)" = "$invocation" ] || die 'the first init restarted NetworkManager'
  [ "$(managedness)" = "$before_table" ] || die 'the first init changed a device managed state'
  [ "$(address_of "$UP")" = "$UP_ADDRESS" ] && [ "$(device_state br0)" = connected ] || die 'the first init touched the uplink or br0'
  [ "$(cut -d ' ' -f1 "$state_record")" = "$(cat /proc/sys/kernel/random/boot_id)" ] \
    && [ "$(cut -d ' ' -f4,5 "$state_record")" = 'unread init' ] \
    && [ "$(cut -d ' ' -f2 "$state_record")" = "$(sudo -n stat -L -c '%i:%.9Z' "$guard")" ] \
    || die "the first init record: $(cat "$state_record" 2>/dev/null)"
  [ "$(stat -c '%U %a' "$state_record")" = 'root 644' ] || die "record ownership: $(stat -c '%U %a' "$state_record")"
  grep -q '^\[device-subyard\]' "$guard" || die 'the first init wrote no guard'
  # Losing the record opens nothing.
  sudo -n rm -f "$state_record"
  if yard init --yes >"$STATE/init-no-record.log" 2>&1; then die 'init continued after the unread record was deleted'; fi
  grep -Fq 'Reboot the host once' "$STATE/init-no-record.log" || die "init without the record: $(tail -5 "$STATE/init-no-record.log")"
  sudo -n rm -f "$conf_dir/50-e2e-later.conf"
  pass 'the first init writes the guard and stops for a reboot without touching NetworkManager, devices or the uplink'
  # A NetworkManager restart reads the guard, but only a reboot counts as proof.
  sudo -n systemctl restart NetworkManager
  settle
  uplink_admins 'NetworkManager restart before the reboot'
  if yard init --yes >"$STATE/init-after-restart.log" 2>&1; then die 'a NetworkManager restart released the first init'; fi
  grep -Fq 'Reboot the host once' "$STATE/init-after-restart.log" || die "init after the restart: $(tail -5 "$STATE/init-after-restart.log")"
  pass 'a NetworkManager restart does not count as reading the guard'
  ;;

fresh-resume)
  load_uplink
  watchdog_start
  launcher="$STATE/bin/yard"
  [ -e "$state_record" ] && [ "$(cut -d ' ' -f1 "$state_record")" != "$(cat /proc/sys/kernel/random/boot_id)" ] \
    || die "the persistent record did not survive the reboot or names this boot: $(cat "$state_record" 2>/dev/null)"
  yard init --yes >"$STATE/init.log" 2>&1 || die 'init failed after the reboot'
  yard start --yes >"$STATE/start.log" 2>&1 || die 'start failed after the reboot'
  settle
  uplink_admins 'except-only list through init and start'
  [ "$(effective_unmanaged)" = 'except:interface-name:br0' ] || die "effective list differs from the administrator's: $(effective_unmanaged)"
  [ "$(device_state br0)" = connected ] && [ "$(address_of br0)" = 198.19.0.1/24 ] || die 'init touched the NM-managed br0'
  grep -Eq 'interface-name:(br0|br-0123456789ab)(,|$)' "$guard" && die 'the guard lists a bridge Incus does not manage'
  yard_devices_unmanaged 'running yard'
  pass 'after the reboot init and start complete; the except-only list and NM-managed br0 are untouched'
  sudo -n ip link add eth9 type veth peer name eth9p
  sudo -n ip tuntap add tap1234abcd mode tap
  sudo -n ip link set eth9 up; sudo -n ip link set tap1234abcd up
  settle 4
  for device in eth9 eth9p tap1234abcd; do
    [ "$(device_state "$device")" = unmanaged ] || die "new device $device was born $(device_state "$device")"
  done
  sudo -n ip link del eth9; sudo -n ip tuntap del tap1234abcd mode tap
  pass 'a veth named eth9 and an Incus-style tap device are born unmanaged'
  yard stop --yes >"$STATE/stop.log" 2>&1 || die 'stop failed'
  yard start --yes >"$STATE/start-again.log" 2>&1 || die 'start after stop failed'
  settle
  yard_devices_unmanaged 'restarted yard'
  pass 'restarting the guest gives an unmanaged veth'
  sudo -n /usr/bin/incus network create e2ebr1 ipv4.address=198.18.9.1/24 ipv4.nat=false ipv6.address=none >/dev/null
  settle 4
  evidence "new Incus bridge e2ebr1 is $(device_state e2ebr1) before init"
  record_before="$(cut -d ' ' -f1,3- "$state_record")"
  yard init --yes >"$STATE/init-bridge.log" 2>&1 || die 'init failed with a second bridge (no reboot is needed for bridge names)'
  grep -Fq 'interface-name:e2ebr1' "$guard" || die 'the guard does not list the second bridge'
  # Boot, content and state stay; the identity follows Subyard's rewrite of the guard.
  [ "$(cut -d ' ' -f1,3- "$state_record")" = "$record_before" ] \
    && [ "$(cut -d ' ' -f2 "$state_record")" = "$(sudo -n stat -L -c '%i:%.9Z' "$guard")" ] \
    || die "a bridge name changed the record: $(cat "$state_record")"
  [ "$(device_state e2ebr1)" = unmanaged ] && [ "$(address_of e2ebr1)" = 198.18.9.1/24 ] \
    || die "the second bridge is $(device_state e2ebr1) with $(address_of e2ebr1)"
  pass 'a second Incus bridge is listed and unmanaged with its address intact'

  # A yard whose INCUS_BRIDGE names the administrator's br0 refuses; br0 is never guarded or pinned.
  other="nmguardother$TOKEN"
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$other"
  sed "s/^SSH_PORT=.*/SSH_PORT=$((37000 + TOKEN % 10000))/" "$SUBYARD_CONFIG_HOME/yards/$name/config.env" > "$SUBYARD_CONFIG_HOME/yards/$other/config.env"
  printf 'INCUS_BRIDGE=br0\n' >> "$SUBYARD_CONFIG_HOME/yards/$other/config.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$other/config.env"
  if owner "$launcher" -Y "$other" init --yes >"$STATE/init-br0.log" 2>&1; then die 'init accepted INCUS_BRIDGE=br0'; fi
  # Incus itself may refuse the profile NIC first; either way br0 is never guarded or pinned.
  grep -Eq 'the yard bridge br0 is not an Incus-managed bridge|Failed to load network "br0"' "$STATE/init-br0.log" \
    || die "INCUS_BRIDGE=br0 refusal: $(tail -5 "$STATE/init-br0.log")"
  ! grep -Fq 'device set br0' "$STATE/init-br0.log" || die 'init pinned br0'
  [ "$(device_state br0)" = connected ] && [ "$(address_of br0)" = 198.19.0.1/24 ] || die 'INCUS_BRIDGE=br0 touched br0'
  grep -Eq 'interface-name:br0(,|$)' "$guard" && die 'the guard lists br0'
  rm -rf "$SUBYARD_CONFIG_HOME/yards/$other"
  # The refused init may have created that yard's project; it must not hold the shared pool.
  sudo -n /usr/bin/incus project delete "subyard-$other" >/dev/null 2>&1 || true
  pass 'INCUS_BRIDGE=br0 refuses and leaves the administrator bridge untouched'

  # A guard whose veth entries were edited out: whatever NetworkManager holds now is unknown, so
  # init repairs the file, records it unread and stops for a reboot (fresh-final resumes).
  sudo -n sed -i 's/type:veth,driver:veth,//' "$guard"
  if yard init --yes >"$STATE/init-repair.log" 2>&1; then die 'init continued after repairing the guard'; fi
  grep -Fq 'Reboot the host once' "$STATE/init-repair.log" || die "repair stop: $(tail -5 "$STATE/init-repair.log")"
  grep -Fq 'type:veth,driver:veth' "$guard" || die 'init did not repair the veth entries'
  [ "$(cut -d ' ' -f1 "$state_record")" = "$(cat /proc/sys/kernel/random/boot_id)" ] \
    && [ "$(cut -d ' ' -f4,5 "$state_record")" = 'unread init' ] || die "repair record: $(cat "$state_record")"
  uplink_admins 'after the repair'
  pass 'an edited-out guard is repaired and init waits for a reboot'
  watchdog_clean
  ;;

fresh-final)
  load_uplink
  launcher="$STATE/bin/yard"
  for _ in $(seq 1 60); do
    sudo -n /usr/bin/incus list --all-projects -f csv -c ns 2>/dev/null | grep -a >/dev/null ",RUNNING$" && break
    sleep 3
  done
  sudo -n /usr/bin/incus list --all-projects -f csv -c ns | grep -a >/dev/null ",RUNNING$" || die 'the boot reconciler did not restore the running yard'
  settle
  yard_devices_unmanaged 'yard restored at boot'
  uplink_admins 'after the final reboot'
  [ "$(device_state br0)" = connected ] && [ "$(device_state e2ebr1)" = unmanaged ] || die 'the reboot changed br0 or e2ebr1'
  pass 'the boot reconciler restored the running yard; its veth and both bridges keep their state'
  yard init --yes >"$STATE/init-repaired.log" 2>&1 || die "init failed after the reboot read the repaired guard: $(tail -5 "$STATE/init-repaired.log")"
  pass 'after the reboot init accepts the repaired guard'
  sudo -n /usr/bin/incus network delete e2ebr1 >/dev/null 2>&1 || true
  yard teardown --yes >"$STATE/teardown.log" 2>&1 || die 'teardown failed'
  settle
  [ -f "$admin_conf" ] || die 'teardown removed the administrator file'
  uplink_admins 'teardown'
  [ "$(device_state br0)" = connected ] || die 'teardown touched br0'
  [ "$(effective_unmanaged)" = 'except:interface-name:br0' ] || die 'teardown changed the administrator list'
  pass 'teardown left the uplink, br0 and the administrator list untouched'
  ;;
*) die "unknown mode $MODE" ;;
esac

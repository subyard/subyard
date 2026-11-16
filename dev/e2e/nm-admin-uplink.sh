#!/usr/bin/env bash
# Admin-managed NetworkManager uplink across published 0.29.0 -> frozen candidate on one
# broker-owned disposable VM. The administrator keeps the uplink unmanaged with a plain
# conf.d list; the published guard replaced that list, the candidate's update and init
# must restore it, keep yard bridges and veths strictly unmanaged and never touch the uplink.
# A temporary udev pin protects the uplink only while the published guard is installed;
# a watchdog restores it and fails the lane if NetworkManager ever takes it over.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OLD_VERSION=0.29.0
OLD_INSTALLER_SHA256=cbf54fbfc846c2e936c53a0d7efa6366f83220e906dfb893f0671a057b29400d
OLD_BUNDLE_SHA256=9d9031ea8bd7c310d97a3d2060109c88754cc44f864d41b2f5c558710e77c3df
if [ "${1:-}" = --preflight ]; then
  bash -n "$0"
  bash "$ROOT/tests/lib-power-network.sh"
  bash "$ROOT/tests/init-network-convergence.sh"
  TMPDIR=/tmp go test ./internal/adapters/hostruntime ./internal/adapters/reconcileruntime \
    -run 'NetworkGuard|NetworkManager|DockerBridge|TestPowerProbeSeparatesInstallFromFinalMetadata' -count=1
  exit
fi
[ "${SUBYARD_E2E_VM:-}" = 1 ] || { printf 'nm-admin-uplink: run through dev/agent-e2e.sh --vm 1\n' >&2; exit 2; }
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] \
  || { printf 'nm-admin-uplink: a frozen candidate bundle is required\n' >&2; exit 2; }
CANDIDATE_VERSION="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
STATE="$(mktemp -d /var/tmp/subyard-nm-uplink.XXXXXX)"
RUN_ID="${SUBYARD_E2E_RUN_ID:?broker lease required}"
printf '%s\n' "$RUN_ID" > "$STATE/.fixture-run"
name="nmup-${STATE##*.}"
name="${name,,}"
launcher=''
conf_dir=/etc/NetworkManager/conf.d
guard="$conf_dir/zz-subyard-unmanaged.conf"
admin_conf="$conf_dir/00-keep-uplink.conf"
pin=/etc/udev/rules.d/00-subyard-e2e-uplink-pin.rules
watchdog_log="$STATE/watchdog.log"

die() { printf 'nm-admin-uplink: %s\n' "$*" >&2; exit 2; }
pass() { printf 'PASS: %s\n' "$*"; }
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
uplink_intact() { # <label>: unmanaged, original address, no NetworkManager profile
  local state address
  state="$(device_state "$UP")"
  [ "$state" = unmanaged ] || die "$1: NetworkManager took the admin uplink $UP (state $state)"
  address="$(ip -4 -o addr show dev "$UP" scope global | awk '{print $4; exit}')"
  [ "$address" = "$UP_ADDRESS" ] || die "$1: uplink address changed from $UP_ADDRESS to $address"
  ! nmcli -t -f DEVICE con show | grep -Fxq "$UP" || die "$1: NetworkManager holds a profile on $UP"
}
yard_devices_unmanaged() { # <label>
  local device state checked=0
  while IFS=: read -r device state; do
    case "$device" in incusbr0|veth*) ;; *) continue ;; esac
    [ "$state" = unmanaged ] || die "$1: yard device $device is $state"
    checked=$((checked + 1))
  done < <(nmcli -t -f DEVICE,STATE device)
  [ "$checked" -ge 2 ] || die "$1: expected the yard bridge and a veth, saw $checked devices"
}
nm_settle() { sleep 8; }
cleanup() {
  local original=$? cleanup_exit=0
  trap - EXIT
  set +e
  if [ "$original" != 0 ]; then
    for log in "$STATE"/*.log; do
      [ -f "$log" ] || continue
      printf '%s (last 30 lines):\n' "${log##*/}"
      tail -n 30 "$log"
    done
  fi
  if [ -n "$launcher" ]; then
    yard teardown --yes >"$STATE/teardown.log" 2>&1 || cleanup_exit=3
  fi
  sudo -n systemctl stop subyard-e2e-uplink-watchdog.service 2>/dev/null
  if [ "$cleanup_exit" = 0 ] && [ ! -L "$STATE" ] && [ "$(cat "$STATE/.fixture-run")" = "$RUN_ID" ]; then
    sudo -n find "$STATE" -depth -delete || cleanup_exit=3
  fi
  printf 'nm-admin-uplink: original_exit=%s cleanup_exit=%s\n' "$original" "$cleanup_exit"
  [ "$original" != 0 ] || original=$cleanup_exit
  exit "$original"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

sudo -n true || die 'passwordless sudo is required on the disposable VM'
UP="$(ip -4 route show default | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit }}')"
UP_GATEWAY="$(ip -4 route show default | awk '{print $3; exit}')"
UP_ADDRESS="$(ip -4 -o addr show dev "$UP" scope global | awk '{print $4; exit}')"
[ -n "$UP" ] && [ -n "$UP_ADDRESS" ] && [ -n "$UP_GATEWAY" ] || die 'cannot identify the VM uplink'

# The administrator's own configuration, written before NetworkManager exists.
sudo -n install -d -m 0755 "$conf_dir"
printf '[main]\nno-auto-default=interface-name:%s\n\n[keyfile]\nunmanaged-devices=interface-name:%s\n' "$UP" "$UP" \
  | sudo -n tee "$admin_conf" >/dev/null
printf 'ENV{INTERFACE}=="%s", ENV{NM_UNMANAGED}="1"\n' "$UP" | sudo -n tee "$pin" >/dev/null
sudo -n udevadm control --reload
sudo -n udevadm trigger --action=change --subsystem-match=net
sudo -n udevadm settle
cat > "$STATE/watchdog.sh" <<WATCHDOG
#!/bin/bash
while :; do
  state="\$(nmcli -t -f DEVICE,STATE device 2>/dev/null | awk -F: '\$1 == "$UP" { print \$2 }')"
  if [ -n "\$state" ] && [ "\$state" != unmanaged ]; then
    printf '%s %s\n' "\$(date -Is)" "\$state" >> "$watchdog_log"
    nmcli device set "$UP" managed no 2>/dev/null
    for profile in \$(nmcli -t -f UUID,DEVICE con show | awk -F: '\$2 == "$UP" { print \$1 }'); do
      nmcli con delete "\$profile" >/dev/null 2>&1
    done
    ip addr flush dev "$UP" scope global
    ip addr add "$UP_ADDRESS" dev "$UP"
    ip route replace default via "$UP_GATEWAY" dev "$UP"
  fi
  sleep 1
done
WATCHDOG
chmod 0755 "$STATE/watchdog.sh"
# shellcheck disable=SC2024 # the operator owns the log
sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends network-manager \
  >"$STATE/apt.log" 2>&1 || die 'network-manager install failed'
sudo -n systemd-run --unit subyard-e2e-uplink-watchdog --collect "$STATE/watchdog.sh" >/dev/null
nm_settle
NetworkManager --version >"$STATE/nm-version.log"
uplink_intact 'NetworkManager install'
pass "NetworkManager $(cat "$STATE/nm-version.log") leaves the admin uplink $UP unmanaged"

bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
export SUBYARD_OPERATOR_HOME="$STATE/home" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/runtime" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH
STORAGE_PATH="$(sudo -n /usr/bin/incus storage get default source)"
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"

curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-all-errors --retry-delay 2 --connect-timeout 20 --max-time 300 \
  "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" -o "$STATE/subyard-install.sh"
[ "$(sha256sum "$STATE/subyard-install.sh" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
  || die 'published installer checksum changed'
owner env YARD_RELEASE_VERSION="$OLD_VERSION" YARD_RELEASE_CACHE="$STATE/releases" YARD_BIN_DIR="$STATE/bin" \
  YARD_SHELL_RC="$STATE/bashrc" YARD_LOGIN_RC="$STATE/profile" \
  bash "$STATE/subyard-install.sh" --version "$OLD_VERSION" --yes >"$STATE/install.log" 2>&1 \
  || die 'published install failed'
archive="$(find "$STATE/releases" -name "subyard-$OLD_VERSION-linux-amd64.tar.gz" -type f -print -quit)"
[ -n "$archive" ] && [ "$(sha256sum "$archive" | awk '{print $1}')" = "$OLD_BUNDLE_SHA256" ] \
  || die 'published runtime archive checksum changed'
launcher="$STATE/bin/yard"
[ "$(owner "$launcher" --version)" = "yard $OLD_VERSION" ] || die 'published runtime is not active'

install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
cat > "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
SSH_PORT=$((36000 + $$ % 10000))
YARD_KIND=container
YARD_IMAGE=images:debian/13/cloud
YARD_IMAGE_FALLBACK=images:debian/13/cloud
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=git-fetch
DEV_USER=dev
DEV_UID=1001
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
LIMITS_CPU=1
LIMITS_MEMORY=1GiB
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
yard init --yes >"$STATE/published-init.log" 2>&1 || die 'published init failed'
grep -q '^\[device-subyard\]' "$guard" || die 'the published init did not write its released guard'
case ",$(effective_unmanaged)," in
  *",interface-name:$UP,"*) die 'the published guard unexpectedly kept the admin uplink entry' ;;
esac
pass "published $OLD_VERSION guard replaced the admin unmanaged-devices list (uplink held only by the pin)"

owner env YARD_RELEASE_BASE_URL="file://$ROOT/.subyard-acceptance/release" \
  "$launcher" update --version "$CANDIDATE_VERSION" --yes >"$STATE/update.log" 2>&1 \
  || die 'candidate update failed'
[ "$(owner "$launcher" version)" = "yard $CANDIDATE_VERSION" ] || die 'candidate is not active'
grep -Fq 'rewrite the released Subyard NetworkManager guard' "$STATE/update.log" \
  || printf 'note: the update output did not show the planned guard rewrite\n'
grep -q '^unmanaged-devices+=' "$guard" || die 'update did not rewrite the guard to its appending form'
if grep -Eq '^\[main\]|^\[device-subyard\]|^unmanaged-devices=|br-\*' "$guard"; then
  die 'update kept a released guard element'
fi
[ ! -e "$conf_dir/99-subyard-unmanaged.conf" ] || die 'update kept the legacy guard'
effective=",$(effective_unmanaged),"
for entry in "interface-name:$UP" type:veth driver:veth interface-name:incusbr0; do
  case "$effective" in *",$entry,"*) ;; *) die "effective unmanaged-devices lacks $entry after update" ;; esac
done
pass 'update rewrote the guard; the effective list holds the admin uplink and the yard entries'

# From here the administrator's own file is the uplink's only protection.
sudo -n rm -f "$pin"
sudo -n udevadm control --reload
sudo -n udevadm trigger --action=change --subsystem-match=net
sudo -n udevadm settle
sudo -n systemctl reload NetworkManager
nm_settle
uplink_intact 'reload without the pin'
sudo -n systemctl restart NetworkManager
nm_settle
uplink_intact 'NetworkManager restart'
pass 'the admin conf.d list alone keeps the uplink unmanaged across reload and restart'

guard_before="$(sha256sum "$guard")"
yard init --yes >"$STATE/candidate-init.log" 2>&1 || die 'candidate init failed'
nm_settle
[ "$(sha256sum "$guard")" = "$guard_before" ] || die 'repeat init changed the current guard'
uplink_intact 'candidate init'
yard start --yes >"$STATE/start.log" 2>&1 || die 'candidate start failed'
nm_settle
uplink_intact 'candidate start'
yard_devices_unmanaged 'running yard'
veth="$(nmcli -t -f DEVICE device | grep -m1 '^veth')" || die 'no yard veth is present'
sudo -n nmcli device set "$veth" managed yes || true
sleep 4
[ "$(device_state "$veth")" = unmanaged ] || die "runtime override made $veth managed"
pass 'repeat init and start keep the uplink; yard bridge and veths stay strictly unmanaged'

yard teardown --yes >"$STATE/teardown.log" 2>&1 || die 'teardown failed'
launcher=''
nm_settle
[ -f "$admin_conf" ] || die 'teardown removed the admin NetworkManager file'
uplink_intact 'teardown'
pass 'teardown leaves the admin NetworkManager configuration and the uplink untouched'

[ ! -s "$watchdog_log" ] || die "watchdog restored the uplink: $(cat "$watchdog_log")"
pass 'the watchdog never had to restore the uplink'

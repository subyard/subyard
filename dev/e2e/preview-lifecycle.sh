#!/usr/bin/env bash
# Synthetic static projects and real initialized yards, only on allocated VMs.
set -Eeuo pipefail
umask 022
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
RUN_ID="${SUBYARD_E2E_RUN_ID:?run through preview-acceptance.sh}"
VM="${SUBYARD_E2E_VM:?allocated VM is required}"
[[ "$RUN_ID" =~ ^[0-9a-f]+$ ]] || { printf 'preview-lifecycle: invalid run identity\n' >&2; exit 2; }
[ "${SUBYARD_E2E_TYPE:-}" = subyard-pair ] || { printf 'preview-lifecycle: requires a disposable pair\n' >&2; exit 2; }
PHASE="${1:-}"
REMOTE_ONLY="${SUBYARD_PREVIEW_REMOTE_ONLY:-0}"
TAILNET="${SUBYARD_PREVIEW_TAILNET:-0}"
CANONICAL="${SUBYARD_PREVIEW_CANONICAL:-0}"
KIND="${SUBYARD_PREVIEW_KIND:-container}"
TAILNET_ADDRESS=100.64.12.20
case "$KIND" in container|vm) ;; *) printf 'preview-lifecycle: invalid yard kind\n' >&2; exit 2 ;; esac
case "$REMOTE_ONLY" in 0|1) ;; *) printf 'preview-lifecycle: invalid fixture scope\n' >&2; exit 2 ;; esac
case "$TAILNET" in 0|1) ;; *) printf 'preview-lifecycle: invalid Tailnet fixture scope\n' >&2; exit 2 ;; esac
case "$CANONICAL" in 0|1) ;; *) printf 'preview-lifecycle: invalid canonical fixture scope\n' >&2; exit 2 ;; esac
case "$VM:$PHASE" in 1:controller|2:owner-setup|2:owner-cleanup) ;; *) printf 'preview-lifecycle: invalid phase or VM\n' >&2; exit 2 ;; esac
STATE="/var/tmp/subyard-preview-$RUN_ID"
MARKER="subyard-preview-acceptance-v1:$RUN_ID:$VM"
NAME="pv-$RUN_ID"
NORMAL_PID='' HELPER_PID='' BUSY_PID=''
trap 'printf "preview-lifecycle: failed phase=%s line=%s exit=%s\n" "$PHASE" "$LINENO" "$?" >&2' ERR
fail() {
  printf 'preview-lifecycle: %s\n' "$*" >&2
  # These fixture logs contain only synthetic project and SSH diagnostics.
  local log
  for log in "$STATE/busy-code.log" "$STATE"/code-*.log "$STATE/normal-ssh.log" "$STATE/helper.err"; do
    [ ! -f "$log" ] || [ ! -s "$log" ] || {
      printf 'preview-lifecycle: %s\n' "${log##*/}" >&2
      tail -n 30 "$log" >&2
    }
  done
  exit 1
}
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export SUBYARD_REPOSITORY_ROOT="$ROOT" SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export PATH="$STATE/bin:$ROOT/bin:/usr/local/bin:/usr/bin:/bin"

# shellcheck source=scripts/lib/host.sh
. "$ROOT/scripts/lib/host.sh"

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
wait_vm_agent() {
  if [ "$KIND" = vm ]; then
    YARD_KIND=vm incus_wait_instance_agent "$1" "$2" \
      || fail 'VM agent did not become ready for preview verification'
  fi
}
print_fixture_logs() {
  local _modified log
  [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ] || return 0
  while IFS=$'\t' read -r _modified log; do
    [ -n "$log" ] || continue
    printf 'preview-lifecycle: %s\n' "${log##*/}" >&2
    tail -n 30 "$log" >&2
  done < <(find "$STATE" -maxdepth 1 -type f -name '*.log' -printf '%T@\t%p\n' \
    | sort -nr | head -n 3 || true)
}
yard() {
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' timeout --foreground 1800 "$ROOT/.build/yard" "$@"
    sg incus-admin -c "exec $command"
  else
    timeout --foreground 1800 "$ROOT/.build/yard" "$@"
  fi
}
peer() {
  local argument quoted command=''
  for argument in "$@"; do
    printf -v quoted '%q' "$argument"
    command+="${command:+ }$quoted"
  done
  ssh -F "${SUBYARD_PREVIEW_PEER_CONFIG:?}" peer-admin -- "$command"
}
stop_sessions() {
  local pid
  if [ -f "$STATE/code.pid" ]; then
    pid="$(cat "$STATE/code.pid")"
    [[ "$pid" =~ ^[0-9]+$ ]] && kill "$pid" 2>/dev/null || true
    rm -f "$STATE/code.pid"
  fi
  for pid in "$HELPER_PID" "$NORMAL_PID" "$BUSY_PID"; do
    [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
    [ -z "$pid" ] || wait "$pid" 2>/dev/null || true
  done
  NORMAL_PID='' HELPER_PID='' BUSY_PID=''
}
cleanup_state() {
  local failed=0 name instance project managed
  [ -e "$STATE" ] || return 0
  [ -d "$STATE" ] && [ ! -L "$STATE" ] && [ "$(cat "$STATE/.marker" 2>/dev/null)" = "$MARKER" ] \
    || { printf 'preview-lifecycle: refusing unowned cleanup\n' >&2; return 1; }
  if [ -f "$STATE/project-role-config.backup" ]; then
    install -m 0600 "$STATE/project-role-config.backup" "$SUBYARD_CONFIG_HOME/yards/$NAME/config.env" || failed=1
  fi
  stop_sessions
  if [ -f "$STATE/isolation-enabled" ]; then
    yard network isolation off --yes > "$STATE/isolation-cleanup.log" 2>&1 || failed=1
  fi
  if [ -f "$STATE/sshd.pid" ]; then
    sudo -n python3 - "$STATE" <<'PY' || failed=1
import os, pathlib, signal, sys
root = pathlib.Path(sys.argv[1])
pid = int((root / "sshd.pid").read_text())
try:
    command = pathlib.Path("/proc", str(pid), "cmdline").read_bytes()
except FileNotFoundError:
    pass
else:
    assert str(root / "sshd_config").encode() in command, "unowned sshd"
    os.kill(pid, signal.SIGTERM)
PY
  fi
  # A sentinel retains the reconciled host substrate while the temporary yards go.
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
  printf 'SSH_PORT=64995\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
  for name in default "$NAME"; do
    [ -f "$SUBYARD_CONFIG_HOME/yards/$name/config.env" ] || continue
    grep -Fqx "# $MARKER" "$SUBYARD_CONFIG_HOME/yards/$name/config.env" || { failed=1; continue; }
    if [ "$name" = default ]; then instance=yard; project=subyard; else instance="yard-$name"; project="subyard-$name"; fi
    managed="$(incus config get "$instance" user.subyard.managed --project "$project" 2>/dev/null || true)"
    if [ -z "$managed" ] || [ "$managed" = true ]; then
      if [ "$KIND" = vm ] && [ "$(incus list "$instance" --project "$project" -f csv -c s)" = RUNNING ]; then
        YARD_KIND=vm incus_wait_instance_agent "$project" "$instance" || failed=1
      fi
      yard -Y "$name" teardown --yes > "$STATE/cleanup-$name.log" 2>&1 || failed=1
    else
      failed=1
    fi
  done
  if [ -f "$SUBYARD_CONFIG_HOME/yards/preview-remote/config.env" ]; then
    yard remote remove preview-remote --yes > "$STATE/cleanup-remote.log" 2>&1 || failed=1
  fi
  if [ -f "$STATE/ssh-config.backup" ]; then
    install -m 0600 "$STATE/ssh-config.backup" "$HOME/.ssh/config" || failed=1
  elif [ -f "$STATE/ssh-config.absent" ]; then
    rm -f "$HOME/.ssh/config"
  fi
  if [ -f "$STATE/tailnet-interface" ]; then
    # The stub and addresses are created only after this fixture's marker exists.
    if [ -f "$STATE/tailscale-installed" ]; then
      sudo -n cmp -s "$STATE/tailscale" /usr/local/bin/tailscale \
        && sudo -n rm /usr/local/bin/tailscale || failed=1
    fi
    for address in "$TAILNET_ADDRESS" 100.64.12.21; do
      sudo -n ip address del "$address/32" dev "$(cat "$STATE/tailnet-interface")" 2>/dev/null || true
    done
  fi
  if [ -f "$STATE/public-interface" ]; then
    sudo -n ip address del 203.0.113.10/32 dev "$(cat "$STATE/public-interface")" 2>/dev/null || true
  fi
  if [ -f "$STATE/tailnet-route" ]; then
    sudo -n ip route del "$TAILNET_ADDRESS/32" via "$(cat "$STATE/tailnet-route")" || failed=1
  fi
  if [ "$failed" = 0 ]; then
    sudo -n find "$STATE" -depth -delete || return 1
  else
    printf 'preview-lifecycle: cleanup failed; marker-owned evidence retained\n' >&2
  fi
  return "$failed"
}
if [ "$PHASE" = owner-cleanup ]; then cleanup_state; exit; fi
[ ! -e "$STATE" ] && [ ! -L "$STATE" ] || fail 'fixture state already exists'
sudo -n true || fail 'disposable VM requires passwordless sudo'
if [ "$KIND" = vm ] && ! command -v qemu-system-x86_64 >/dev/null 2>&1; then
  sudo -n apt-get update >/dev/null
  sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-system-x86 >/dev/null
fi
[ -x "$ROOT/.build/yard" ] || make -C "$ROOT" build > /dev/null
install -d -m 0700 "$STATE" "$STATE/bin" "$STATE/config/yards" "$STATE/data" "$HOME/.ssh"
printf '%s\n' "$MARKER" > "$STATE/.marker"
if [ -f "$HOME/.ssh/config" ]; then
  cp "$HOME/.ssh/config" "$STATE/ssh-config.backup"
else
  touch "$STATE/ssh-config.absent"
fi
cleanup() {
  local rc=$?
  trap - EXIT INT TERM ERR
  set +e
  [ "$rc" = 0 ] || print_fixture_logs
  cleanup_state || {
    printf 'preview-lifecycle: cleanup failed after original exit %s\n' "$rc" >&2
    print_fixture_logs
    rc=3
  }
  exit "$rc"
}
trap cleanup EXIT INT TERM

if [ "$KIND" = vm ]; then
  # The bounded readiness helper executes Incus under timeout, outside shell functions.
  cat > "$STATE/bin/incus" <<'INCUS'
#!/usr/bin/env bash
if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
  exec sudo -n /usr/bin/incus "$@"
fi
exec /usr/bin/incus "$@"
INCUS
  chmod 0700 "$STATE/bin/incus"
fi

setup_yard() {
  local name="$1" port preview_port config instance project platform marker temporary
  if [ "$name" = default ]; then instance=yard; project=subyard; else instance="yard-$name"; project="subyard-$name"; fi
  ! incus info "$instance" --project "$project" >/dev/null 2>&1 || fail 'fixture target already exists'
  read -r port preview_port < <(python3 -c 'import socket; a=socket.socket(); b=socket.socket(); a.bind(("127.0.0.1",0)); b.bind(("127.0.0.1",0)); print(a.getsockname()[1], b.getsockname()[1])')
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
  config="$SUBYARD_CONFIG_HOME/yards/$name/config.env"
  cat > "$config" <<EOF
# $MARKER
SSH_PORT=$port
YARD_KIND=$KIND
LIMITS_MEMORY=1GiB
LIMITS_CPU=2
WEB_PREVIEW_HOST_PORT=$preview_port
HOST_BASE=$STATE/host-$name
RESTRICTED_DISK_PATHS=$STATE/host-$name
CODING_TOOL_INTEGRATIONS=''
ENVIRONMENT_PROFILES=
FORWARD_SSH_AGENT=0
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
EOF
  if [ "$KIND" = vm ]; then
    # Match the existing VM fixture: avoid hot-adding Incus 6.0 virtiofs mounts.
    printf 'HOST_MOUNTS=\nHOST_LINKS=\n' >> "$config"
    # Exercise ordinary local pinning and the profile pin needed by isolation.
    [ "$name" = default ] || printf 'VM_PIN_IPV4=1\n' >> "$config"
  fi
  chmod 0600 "$config"
  platform="$HOME/.cache/subyard-e2e-platform"
  marker="$platform/.subyard-e2e-platform-marker"
  [ ! -L "$HOME/.cache" ] && [ ! -L "$platform" ] || fail 'unsafe platform directory'
  if [ -e "$marker" ] || [ -L "$marker" ]; then
    [ -f "$marker" ] && [ ! -L "$marker" ] \
      && [ "$(sudo -n cat "$marker")" = subyard-e2e-platform-v1 ] || fail 'unexpected platform marker'
  fi
  sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0711 "$platform"
  yard -Y "$name" init --yes > "$STATE/init-$name.log" 2>&1 || {
    tail -n 80 "$STATE/init-$name.log" >&2
    fail 'real yard init failed'
  }
  yard -Y "$name" start --yes > "$STATE/start-$name.log" 2>&1 || fail 'real yard start failed'
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
  temporary="$(mktemp "$platform/.platform-marker.XXXXXX")"
  printf '%s\n' subyard-e2e-platform-v1 > "$temporary"
  chmod 0600 "$temporary"
  mv -fT "$temporary" "$marker"
  incus exec "$instance" --project "$project" -- sh -eu -c '
    test -x /usr/local/bin/subyard-preview
    test "$(stat -c %u:%a /usr/local/bin/subyard-preview)" = 0:755
    test "$(stat -c %u:%g:%a /etc/subyard/preview.json)" = 0:0:644
  ' || fail 'provisioned helper ownership or mode is wrong'
  # Upgrade an old managed snippet that still contains the retired editor route.
  local alias snippet
  if [ "$name" = default ]; then alias=yard; snippet="$HOME/.ssh/subyard.config"; else alias="yard-$name"; snippet="$HOME/.ssh/subyard-$name.config"; fi
  printf '\nHost %s.code\n    LocalForward 127.0.0.1:8765 127.0.0.1:8765\n    ExitOnForwardFailure yes\n' "$alias" >> "$snippet"
  yard -Y "$name" init --yes > "$STATE/reinit-$name.log" 2>&1 || fail 'repeat init failed'
  wait_vm_agent "$project" "$instance"
  ! grep -Fqx "Host $alias.code" "$snippet" || fail 'repeat init retained the retired code alias'
}

if [ "$PHASE" = owner-setup ]; then
  if [ "$TAILNET" = 1 ]; then
    ! command -v tailscale >/dev/null 2>&1 || fail 'synthetic Tailnet fixture requires no installed tailscale'
    [ ! -e /usr/local/bin/tailscale ] && [ ! -L /usr/local/bin/tailscale ] || fail 'synthetic tailscale path already exists'
  fi
  setup_yard "$NAME"
  instance="yard-$NAME" project="subyard-$NAME"
  preview_port="$(sed -n 's/^WEB_PREVIEW_HOST_PORT=//p' "$SUBYARD_CONFIG_HOME/yards/$NAME/config.env")"
  owner_address="$(ip -j -4 route get 1.1.1.1 | jq -er '.[0].prefsrc')"
  python3 - "$owner_address" <<'PYPRIVATE' || fail 'disposable owner requires a private default-route source'
import ipaddress, sys
address = ipaddress.IPv4Address(sys.argv[1])
assert any(address in ipaddress.IPv4Network(network) for network in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))
PYPRIVATE
  assert_route() {
    local host="$1" port="$2" guest_address='' device receipt
    # Isolation and reconciliation can restart the VM before its guest agent is ready.
    wait_vm_agent "$project" "$instance"
    incus exec "$instance" --project "$project" -- cat /etc/subyard/preview.json > "$STATE/route.json"
    if [ "$host" = 127.0.0.1 ]; then
      jq -e '. == {version:1,host:"127.0.0.1",port:8765}' "$STATE/route.json" >/dev/null \
        || fail 'loopback metadata is incorrect'
      incus query "/1.0/instances/$instance?project=$project" \
        | jq -e '.devices["subyard-preview"] == null and .config["user.subyard.preview_proxy"] == null' >/dev/null \
        || fail 'loopback fallback retained an owner proxy'
      return
    fi
    if [ "$KIND" = vm ]; then
      guest_address="$(jq -er .bindHost "$STATE/route.json")"
      receipt="v2:$host:$port:$guest_address"
      device="$(jq -cn --arg listen "tcp:$host:$port" --arg connect "tcp:$guest_address:8765" '{type:"proxy",bind:"host",listen:$listen,connect:$connect,nat:"true"}')"
    else
      receipt="v1:$host:$port"
      device="$(jq -cn --arg listen "tcp:$host:$port" '{type:"proxy",bind:"host",listen:$listen,connect:"tcp:127.0.0.1:8765"}')"
      [ "$(sudo -n ss -Hltn "sport = :$port" | awk '{print $4}')" = "$host:$port" ] \
        || fail 'owner preview listener is not bound to the exact selected address'
    fi
    jq -e --arg host "$host" --argjson port "$port" --arg guest "$guest_address" '
      . == ({version:1,host:$host,port:$port} + if $guest == "" then {} else {bindHost:$guest} end)
    ' "$STATE/route.json" >/dev/null || fail 'installed preview endpoint is incorrect'
    incus query "/1.0/instances/$instance?project=$project" \
      | jq -e --argjson device "$device" --arg receipt "$receipt" --arg guest "$guest_address" '
        .devices["subyard-preview"] == $device
        and .config["user.subyard.preview_proxy"] == $receipt
        and ($guest == "" or .expanded_devices.eth0["ipv4.address"] == $guest)
      ' >/dev/null || fail 'owner proxy, ownership receipt or static VM address is incorrect'
  }
  assert_route "$owner_address" "$preview_port"
  # Missing owned devices are recoverable; unreceipted and divergent devices are not.
  incus config device remove "$instance" subyard-preview --project "$project" >/dev/null
  yard -Y "$NAME" init --yes > "$STATE/route-recovery.log" 2>&1 || fail 'missing preview device recovery failed'
  assert_route "$owner_address" "$preview_port"
  receipt="$(incus config get "$instance" user.subyard.preview_proxy --project "$project")"
  incus config unset "$instance" user.subyard.preview_proxy --project "$project"
  before="$(incus query "/1.0/instances/$instance?project=$project" | jq -c '.devices["subyard-preview"]')"
  if yard -Y "$NAME" security --require-live > "$STATE/foreign-security.log" 2>&1; then fail 'live security audit accepted a foreign preview device'; fi
  if yard -Y "$NAME" init --yes > "$STATE/foreign-init.log" 2>&1; then fail 'init adopted a foreign preview device'; fi
  [ "$(incus query "/1.0/instances/$instance?project=$project" | jq -c '.devices["subyard-preview"]')" = "$before" ] \
    && [ -z "$(incus config get "$instance" user.subyard.preview_proxy --project "$project")" ] \
    || fail 'init changed a foreign preview device'
  incus config set "$instance" user.subyard.preview_proxy "$receipt" --project "$project"
  listen="$(incus config device get "$instance" subyard-preview listen --project "$project")"
  drift_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  incus config device set "$instance" subyard-preview "listen=tcp:$owner_address:$drift_port" --project "$project"
  if yard -Y "$NAME" init --yes > "$STATE/divergent-init.log" 2>&1; then fail 'init replaced a divergent preview device'; fi
  [ "$(incus config device get "$instance" subyard-preview listen --project "$project")" = "tcp:$owner_address:$drift_port" ] \
    && [ "$(incus config get "$instance" user.subyard.preview_proxy --project "$project")" = "$receipt" ] \
    || fail 'init changed a divergent preview device'
  incus config device set "$instance" subyard-preview "listen=$listen" --project "$project"
  assert_route "$owner_address" "$preview_port"
  if [ "$KIND" = vm ]; then
    # Keep generic isolation active through endpoint drift and the later peer HTTP checks.
    touch "$STATE/isolation-enabled"
    yard network isolation on --yes > "$STATE/isolation-enable.log" 2>&1 || fail 'VM owner network isolation failed'
    yard network status --json | jq -e '.isolation == true and .appliedIsolation == true and .converged == true' >/dev/null \
      || fail 'VM owner network isolation did not converge'
    assert_route "$owner_address" "$preview_port"
    incus config device remove "$instance" subyard-preview --project "$project" >/dev/null
    yard -Y "$NAME" init --yes > "$STATE/isolated-recovery.log" 2>&1 || fail 'isolated VM preview recovery failed'
    assert_route "$owner_address" "$preview_port"
  fi
  yard -Y "$NAME" security --require-live > "$STATE/private-security.log" 2>&1 \
    || fail 'owned private preview failed the live security audit'
  # A documentation-range source models a public owner without changing its route.
  interface="$(ip -j -4 route get 1.1.1.1 | jq -er '.[0].dev')"
  [ "$(ip -j -4 address show | jq -r '[.[].addr_info[] | select(.local == "203.0.113.10")] | length')" = 0 ] \
    || fail 'synthetic public owner address already exists'
  printf '%s\n' "$interface" > "$STATE/public-interface"
  sudo -n ip address add 203.0.113.10/32 dev "$interface"
  cat > "$STATE/bin/ip" <<'PYIP'
#!/usr/bin/env python3
import json, subprocess, sys
arguments = sys.argv[1:]
if arguments == ["-j", "-4", "route", "get", "1.1.1.1"]:
    routes = json.loads(subprocess.check_output(["/usr/sbin/ip", *arguments]))
    routes[0]["prefsrc"] = "203.0.113.10"
    print(json.dumps(routes))
else:
    sys.exit(subprocess.call(["/usr/sbin/ip", *arguments]))
PYIP
  chmod 0755 "$STATE/bin/ip"
  yard -Y "$NAME" init --yes > "$STATE/public-fallback.log" 2>&1 || fail 'public owner fallback init failed'
  assert_route 127.0.0.1 8765
  rm "$STATE/bin/ip"
  sudo -n ip address del 203.0.113.10/32 dev "$interface"
  rm "$STATE/public-interface"
  yard -Y "$NAME" init --yes > "$STATE/private-restore.log" 2>&1 || fail 'private owner route restore failed'
  assert_route "$owner_address" "$preview_port"
  if [ "$TAILNET" = 1 ]; then
    interface="$(ip -j route get "${SUBYARD_PREVIEW_PEER_IP:?}" | jq -er '.[0].dev')"
    [ "$(ip -j -4 address show | jq -r '[.[].addr_info[] | select(.local == "100.64.12.20" or .local == "100.64.12.21")] | length')" = 0 ] \
      || fail 'synthetic Tailnet addresses already exist'
    cat > "$STATE/tailscale" <<EOF
#!/bin/sh
# $MARKER
[ "\$*" = 'ip -4' ] || exit 1
cat '$STATE/tailscale-address'
EOF
    chmod 0755 "$STATE/tailscale"
    printf '%s\n' "$interface" > "$STATE/tailnet-interface"
    printf '%s\n' "$TAILNET_ADDRESS" > "$STATE/tailscale-address"
    sudo -n ip address add "$TAILNET_ADDRESS/32" dev "$interface"
    sudo -n install -o root -g root -m 0755 "$STATE/tailscale" /usr/local/bin/tailscale
    touch "$STATE/tailscale-installed"
    yard -Y "$NAME" init --yes > "$STATE/tailnet-init.log" 2>&1 || fail 'Tailnet init failed'
    assert_route "$TAILNET_ADDRESS" "$preview_port"
    yard -Y "$NAME" init --yes > "$STATE/tailnet-repeat.log" 2>&1 || fail 'Tailnet repeat init failed'
    assert_route "$TAILNET_ADDRESS" "$preview_port"
    yard -Y "$NAME" security --require-live > "$STATE/tailnet-security.log" 2>&1 || fail 'owned preview failed the live security audit'
    printf '%s\n' 100.64.12.21 > "$STATE/tailscale-address"
    sudo -n ip address add 100.64.12.21/32 dev "$interface"
    yard -Y "$NAME" init --yes > "$STATE/tailnet-drift.log" 2>&1 || fail 'address drift init failed'
    assert_route 100.64.12.21 "$preview_port"
    : > "$STATE/tailscale-address"
    yard -Y "$NAME" init --yes > "$STATE/tailnet-fallback.log" 2>&1 || fail 'no-Tailnet private fallback init failed'
    assert_route "$owner_address" "$preview_port"
    printf '%s\n' "$TAILNET_ADDRESS" > "$STATE/tailscale-address"
    yard -Y "$NAME" init --yes > "$STATE/tailnet-restore.log" 2>&1 || fail 'Tailnet restore init failed'
    assert_route "$TAILNET_ADDRESS" "$preview_port"
  fi
  if [ "$KIND" = vm ]; then
    yard network status --json | jq -e '.isolation == true and .appliedIsolation == true and .converged == true' >/dev/null \
      || fail 'VM preview drift left network isolation unconverged'
  fi
  printf 'ok: %s owner route, repeat init, owned recovery and foreign-device protection\n' "$KIND"
  printf '%s\n' "$ROOT" > "$STATE/source-root"
  trap - EXIT INT TERM
  printf 'ok: remote owner yard initialized with installed preview helper\n'
  exit 0
fi

if [ "$REMOTE_ONLY" = 0 ]; then
  setup_yard default
  setup_yard "$NAME"
fi
if [ "$TAILNET" = 1 ]; then
  [ -z "$(ip -j route show "$TAILNET_ADDRESS/32" | jq -r '.[] | .dst')" ] || fail 'synthetic Tailnet peer route already exists'
  sudo -n ip route add "$TAILNET_ADDRESS/32" via "${SUBYARD_PREVIEW_PEER_IP:?}"
  printf '%s\n' "$SUBYARD_PREVIEW_PEER_IP" > "$STATE/tailnet-route"
fi
install -d -m 0700 "$STATE/PreviewFixture/site"
git -C "$STATE/PreviewFixture" init -q
printf 'initial preview\n' > "$STATE/PreviewFixture/site/index.html"
# Match ordinary user multiplexing so a normal session is already connected.
cat >> "$HOME/.ssh/config" <<'EOF'

Host *
    BatchMode yes
    ConnectTimeout 5
    LogLevel ERROR
    ControlMaster auto
    ControlPath ~/.ssh/subyard-cm-%C
    ControlPersist no
EOF

cat > "$STATE/bin/code" <<'PY'
#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
from urllib.parse import urlsplit, unquote
state = pathlib.Path(os.environ["PREVIEW_TEST_STATE"])
(state / "code.called").touch()
assert len(sys.argv) == 2, "unexpected code arguments"
workspace = json.loads(pathlib.Path(sys.argv[1]).read_text())
alias = os.environ["PREVIEW_EXPECTED_ALIAS"]
authority = "ssh-remote+" + alias
assert workspace["remoteAuthority"] == authority, "incorrect resolved code authority"
folder = urlsplit(workspace["folders"][0]["uri"])
assert folder.scheme == "vscode-remote" and folder.netloc == authority, "incorrect folder authority"
(state / "project.path").write_text(unquote(folder.path))
with (state / "code-ssh.log").open("wb") as output:
    # A real command keeps the multiplexed editor session active; -N can exit
    # successfully when the ordinary alias already has a persistent master.
    process = subprocess.Popen(["ssh", "-T", alias, "--", "sleep", "120"], stdout=output, stderr=output, start_new_session=True)
(state / "code.pid").write_text(str(process.pid))
PY
chmod 0755 "$STATE/bin/code"
export PREVIEW_TEST_STATE="$STATE"

assert_no_listener() {
  ! ss -Hltn 'sport = :8765' | grep -q . || fail 'unexpected preview listener remains'
}
check_preview() {
  local name="$1" alias="$2" selector="${3:-$1}" command path preview_url code_pid bind_host
  export PREVIEW_EXPECTED_ALIAS="$alias"
  yard -Y "$selector" sync "$STATE/PreviewFixture" --yes > "$STATE/sync-$name.log" 2>&1 || {
    tail -n 80 "$STATE/sync-$name.log" >&2
    fail 'fixture sync failed'
  }
  ssh -G "$alias" > "$STATE/code.options" 2>/dev/null
  ! grep -q '^localforward ' "$STATE/code.options" || fail 'ordinary alias has a preview forward'
  ssh -G "$alias.code" 2>/dev/null | grep -Fqx "hostname $alias.code" \
    || fail 'retired code alias still resolves to the yard'
  ssh -T "$alias" -- sleep 120 > "$STATE/normal-ssh.log" 2>&1 & NORMAL_PID=$!
  sleep 0.2
  kill -0 "$NORMAL_PID" || { cat "$STATE/normal-ssh.log" >&2; fail 'ordinary SSH session failed'; }
  assert_no_listener
  python3 - "$STATE/busy.ready" <<'PYBUSY' > "$STATE/busy.log" 2>&1 &
import pathlib, socket, sys, time
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("127.0.0.1", 8765)); listener.listen()
pathlib.Path(sys.argv[1]).touch()
time.sleep(120)
PYBUSY
  BUSY_PID=$!
  for _ in {1..50}; do [ ! -f "$STATE/busy.ready" ] || break; sleep 0.1; done
  [ -f "$STATE/busy.ready" ] || fail 'collision listener failed'
  rm -f "$STATE/code.called"
  yard -Y "$selector" code PreviewFixture > "$STATE/busy-code.log" 2>&1 \
    || fail 'yard code failed with occupied controller preview port'
  [ -f "$STATE/code.called" ] && [ -s "$STATE/project.path" ] || fail 'VS Code did not receive the workspace'
  kill -0 "$BUSY_PID" || fail 'yard code disturbed the occupied controller port'
  code_pid="$(cat "$STATE/code.pid")"
  [[ "$code_pid" =~ ^[0-9]+$ ]] || fail 'invalid code session PID'
  kill "$code_pid"
  rm -f "$STATE/code.pid"
  kill "$BUSY_PID"; wait "$BUSY_PID" 2>/dev/null || true; BUSY_PID=''
  rm -f "$STATE/busy.ready"
  assert_no_listener
  yard -Y "$selector" code PreviewFixture > "$STATE/code-$name.log" 2>&1 || fail 'yard code failed'
  code_pid="$(cat "$STATE/code.pid")"
  for _ in {1..50}; do
    [ -z "$(ss -Hltn 'sport = :8765')" ] || fail 'yard code created a controller preview listener'
    kill -0 "$code_pid" || fail 'code SSH session failed'
    sleep 0.1
  done
  ssh -G "$alias" > "$STATE/normal.options.after" 2>/dev/null
  cmp -s "$STATE/code.options" "$STATE/normal.options.after" || fail 'code launch changed the ordinary SSH route'
  # Preview starts in a normal terminal with every editor session already closed.
  kill "$code_pid"
  rm -f "$STATE/code.pid"
  path="$(cat "$STATE/project.path")"
  ssh -T "$alias" -- cat /etc/subyard/preview.json > "$STATE/preview.json"
  preview_url="$(jq -er '"http://" + .host + ":" + (.port|tostring) + "/"' "$STATE/preview.json")"
  [ "$(jq -r .host "$STATE/preview.json")" != 127.0.0.1 ] || fail 'private owner did not publish an external preview route'
  bind_host="$(jq -r '.bindHost // ""' "$STATE/preview.json")"
  printf -v command 'cd %q; printf "%%s\\n" "$$" > .preview-test.pid; exec subyard-preview site' "$path"
  ssh -T "$alias" -- "$command" > "$STATE/helper.log" 2> "$STATE/helper.err" & HELPER_PID=$!
  for _ in {1..100}; do
    grep -Fqx "Preview: $preview_url" "$STATE/helper.log" && break
    kill -0 "$HELPER_PID" || fail 'foreground helper exited before readiness'
    sleep 0.1
  done
  [ "$(cat "$STATE/helper.log")" = "Preview: $preview_url" ] || fail 'helper readiness URL is incorrect'
  preview_url="$(sed -n 's/^Preview: //p' "$STATE/helper.log")"
  # VM NAT is exercised from the other machine, as an ordinary browser would be.
  fetch_preview() {
    if [ "$KIND" = vm ] && [ "$name" != preview-remote ]; then
      peer curl --noproxy '*' -fsS --max-time 3 -D - "$preview_url"
    else
      curl --noproxy '*' -fsS --max-time 3 -D - "$preview_url"
    fi
  }
  for _ in {1..50}; do
    fetch_preview > "$STATE/response" 2>/dev/null && break
    sleep 0.1
  done
  grep -Fqx 'initial preview' "$STATE/response" || fail 'printed owner URL returned an incorrect initial page'
  grep -Fqi 'Cache-Control: no-store' "$STATE/response" || fail 'direct response lacks no-store'
  assert_no_listener
  printf -v command 'python3 -c %q %q' 'import pathlib, socket, struct, sys; expected={"127.0.0.1"}; expected.update([sys.argv[1]] if sys.argv[1] else []); rows=pathlib.Path("/proc/net/tcp").read_text().splitlines()[1:]; listeners={socket.inet_ntoa(struct.pack("<I", int(r.split()[1].split(":")[0],16))) for r in rows if r.split()[3]=="0A" and r.split()[1].endswith(":223D")}; assert listeners==expected, (listeners,expected)' "$bind_host"
  ssh -T "$alias" -- "$command" || fail 'yard preview is not bound to its exact allowed addresses'
  printf -v command 'cd %q; printf "edited preview\\n" > site/index.html' "$path"
  ssh -T "$alias" -- "$command"
  fetch_preview > "$STATE/response" || fail 'live edit URL fetch failed'
  grep -Fqx 'edited preview' "$STATE/response" || fail 'live edit did not reach the printed owner URL'
  assert_no_listener
  printf -v command 'cd %q; kill "$(cat .preview-test.pid)"; rm -f .preview-test.pid; printf "initial preview\\n" > site/index.html' "$path"
  ssh -T "$alias" -- "$command"
  wait "$HELPER_PID" 2>/dev/null || true; HELPER_PID=''
  if fetch_preview >/dev/null 2>&1; then fail 'owner preview survived helper termination'; fi
  [ ! -s "$STATE/helper.err" ] || fail 'helper emitted unexpected diagnostics'
  stop_sessions
  assert_no_listener
  printf 'ok: %s ordinary code alias, occupied port independence, terminal preview, live edits and foreground shutdown\n' "$name"
}
if [ "$REMOTE_ONLY" = 0 ]; then
  check_preview default yard
  check_preview "$NAME" "yard-$NAME"
fi

# The peer owner SSH server accepts only a temporary synthetic key. Its root
# administration route is used only to create this fixture inside allocated VM2.
PEER_IP="${SUBYARD_PREVIEW_PEER_IP:?}"
[[ "$PEER_IP" =~ ^[0-9a-fA-F:.]+$ ]] || fail 'invalid peer address'
ssh-keygen -q -t ed25519 -N '' -f "$STATE/owner-key"
peer dd "of=$STATE/owner-authorized-key" status=none < "$STATE/owner-key.pub"
peer bash -s -- "$STATE" "$RUN_ID" <<'EOS'
set -euo pipefail
state="$1" run="$2"
test "$(cat "$state/.marker")" = "subyard-preview-acceptance-v1:$run:2"
root="$(cat "$state/source-root")"
case "$root" in /tmp/subyard-worktree.*/src) ;; *) exit 2 ;; esac
ssh-keygen -q -t ed25519 -N '' -f "$state/owner-host-key"
python3 - "$state" "$root" <<'PY'
import pathlib, shlex, sys
state, root = sys.argv[1:]
environment = {
    "HOME": "/home/dev", "SUBYARD_OPERATOR_HOME": "/home/dev",
    "SUBYARD_CONFIG_HOME": state+"/config", "SUBYARD_HOME": state+"/data",
    "SUBYARD_REPOSITORY_ROOT": root, "SUBYARD_NO_AUDIT": "1",
    "SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": "1",
    "STORAGE_PATH": "/home/dev/.cache/subyard-e2e-platform/incus/incus/storage",
    "PATH": root+"/bin:/usr/local/bin:/usr/bin:/bin",
}
pathlib.Path(state, "owner-env").write_text("\n".join(
    "export "+key+"="+shlex.quote(value) for key,value in environment.items())+"\n")
pathlib.Path(state, "owner-shell").write_text(
    "#!/bin/bash\nset -euo pipefail\nexport BASH_ENV="+shlex.quote(state+"/owner-env")+
    '\n. "$BASH_ENV"\nexec /usr/bin/bash -c "$SSH_ORIGINAL_COMMAND"\n')
PY
chmod 0700 "$state/owner-shell"
chmod 0600 "$state/owner-env" "$state/owner-authorized-key"
chown dev:dev "$state/owner-shell" "$state/owner-env" "$state/owner-authorized-key"
cat > "$state/sshd_config" <<EOF
Port 25222
ListenAddress 0.0.0.0
HostKey $state/owner-host-key
PidFile $state/sshd.pid
AuthorizedKeysFile $state/owner-authorized-key
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
AllowUsers dev
AllowTcpForwarding yes
PermitOpen 127.0.0.1:*
PermitTTY no
ForceCommand $state/owner-shell
LogLevel ERROR
EOF
chmod 0600 "$state/sshd_config"
/usr/sbin/sshd -t -f "$state/sshd_config"
nohup /usr/sbin/sshd -D -e -f "$state/sshd_config" > "$state/sshd.log" 2>&1 &
for _ in {1..50}; do
  [ ! -s "$state/sshd.pid" ] || exit 0
  sleep 0.1
done
exit 1
EOS
peer cat "$STATE/owner-host-key.pub" > "$STATE/owner-host-key.pub"
printf 'preview-owner-key ' > "$STATE/owner-known-hosts"
cat "$STATE/owner-host-key.pub" >> "$STATE/owner-known-hosts"
OWNER_ALIAS="preview-owner-$RUN_ID"
cat > "$STATE/owner-ssh" <<EOF
Host $OWNER_ALIAS
    HostName $PEER_IP
    Port 25222
    User dev
    IdentityFile $STATE/owner-key
    IdentitiesOnly yes
    BatchMode yes
    StrictHostKeyChecking yes
    HostKeyAlias preview-owner-key
    UserKnownHostsFile $STATE/owner-known-hosts
    GlobalKnownHostsFile /dev/null
    ConnectTimeout 5
    LogLevel ERROR
EOF
chmod 0600 "$STATE/owner-ssh" "$STATE/owner-known-hosts"
{ printf 'Include %s\n' "$STATE/owner-ssh"; cat "$HOME/.ssh/config"; } > "$STATE/ssh-config.next"
install -m 0600 "$STATE/ssh-config.next" "$HOME/.ssh/config"
yard host add "$OWNER_ALIAS" --yes > "$STATE/host-add.log" 2>&1 || {
  tail -n 80 "$STATE/host-add.log" >&2
  fail 'canonical owner registration failed'
}
yard remote add preview-remote "$OWNER_ALIAS" --yard "$NAME" --yes \
  > "$STATE/remote-add.log" 2>&1 || {
    tail -n 80 "$STATE/remote-add.log" >&2
    fail 'production remote registration failed'
  }
printf '\nHost yard-preview-remote.code\n    LocalForward 127.0.0.1:8765 127.0.0.1:8765\n' >> "$HOME/.ssh/subyard-preview-remote.config"
yard remote add preview-remote "$OWNER_ALIAS" --yard "$NAME" --yes \
  > "$STATE/remote-repeat.log" 2>&1 || fail 'remote registration did not converge its old SSH snippet'
check_remote_preview() {
  local selector="$1"
  check_preview preview-remote yard-preview-remote "$selector"
  if [ "$TAILNET" = 1 ]; then
    # Reconcile the owner from an ordinary terminal, then fetch its printed fallback URL.
    ssh -T "$OWNER_ALIAS" -- "truncate -s 0 '$STATE/tailscale-address'; yard -Y '$NAME' init --yes" \
      > "$STATE/remote-fallback.log" 2>&1 || fail 'remote private fallback init failed'
    check_preview preview-remote yard-preview-remote "$selector"
    ssh -T "$OWNER_ALIAS" -- "yard -Y '$NAME' security --require-live" \
      > "$STATE/remote-fallback-security.log" 2>&1 || fail 'remote private fallback failed the live security audit'
    ssh -T "$OWNER_ALIAS" -- "printf '%s\\n' '$TAILNET_ADDRESS' > '$STATE/tailscale-address'; yard -Y '$NAME' init --yes" \
      > "$STATE/remote-restore.log" 2>&1 || fail 'remote Tailnet restore init failed'
  fi
}
if [ "$CANONICAL" = 1 ]; then
  owner_id="$(peer cat "$STATE/config/host-id")"
  [[ "$owner_id" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || fail 'invalid canonical owner identity'
  check_remote_preview "$owner_id/$NAME"
  # The controller's default role permits projects; only the owner can deny this yard.
  peer bash -s -- "$STATE" "$RUN_ID" "$NAME" <<'EOS'
set -euo pipefail
state="$1" run="$2" name="$3"
[ "$(cat "$state/.marker")" = "subyard-preview-acceptance-v1:$run:2" ]
root="$(cat "$state/source-root")"
case "$root" in /tmp/subyard-worktree.*/src) ;; *) exit 2 ;; esac
preset="$root/config/yards/profiles/canonical-no-projects.env"
[ ! -e "$preset" ] && [ ! -L "$preset" ]
install -d -m 0755 "${preset%/*}"
printf 'ALLOWS_PROJECTS=false\n' > "$preset"
chmod 0644 "$preset"
cp "$state/config/yards/$name/config.env" "$state/project-role-config.backup"
chmod 0600 "$state/project-role-config.backup"
chown dev:dev "$state/project-role-config.backup"
printf 'YARD_TEMPLATE=canonical-no-projects\n' >> "$state/config/yards/$name/config.env"
EOS
  # Probe current inventory without writing the cache before testing role refusal.
  yard list --live > "$STATE/role-inventory.log" 2>&1 || fail 'role fixture inventory refresh failed'
  cat > "$STATE/state-fingerprint.py" <<'PY'
import hashlib, json, os, pathlib, stat, sys
for index, directory in enumerate(sys.argv[1:]):
    root = pathlib.Path(directory)
    for path in sorted([root, *root.rglob("*")]):
        info = path.lstat()
        value = None
        if stat.S_ISREG(info.st_mode):
            value = hashlib.sha256(path.read_bytes()).hexdigest()
        elif stat.S_ISLNK(info.st_mode):
            value = os.readlink(path)
        print(json.dumps((index, str(path.relative_to(root)), info.st_mode, info.st_uid, info.st_gid, value)))
PY
  controller_before="$(python3 "$STATE/state-fingerprint.py" "$STATE/config" "$STATE/data")"
  owner_before="$(peer python3 - "$STATE/config" "$STATE/data" < "$STATE/state-fingerprint.py")"
  rm -f "$STATE/code.called"
  for command in sync code; do
    arguments=(PreviewFixture)
    [ "$command" != sync ] || arguments=("$STATE/PreviewFixture")
    if yard -Y "$owner_id/$NAME" "$command" "${arguments[@]}" --yes > "$STATE/denied-$command.log" 2>&1; then
      fail "canonical $command accepted a denied owner role"
    fi
    grep -Fq 'selected yard role does not accept work projects' "$STATE/denied-$command.log" \
      || fail "canonical $command did not report the owner role denial"
    [ ! -f "$STATE/code.called" ] || fail 'role denial reached VS Code'
    controller_after="$(python3 "$STATE/state-fingerprint.py" "$STATE/config" "$STATE/data")"
    if [ "$controller_after" != "$controller_before" ]; then
      diff -u <(printf '%s\n' "$controller_before") <(printf '%s\n' "$controller_after") | tail -n 40 >&2 || true
      fail 'denied assessment changed controller state'
    fi
    owner_after="$(peer python3 - "$STATE/config" "$STATE/data" < "$STATE/state-fingerprint.py")"
    if [ "$owner_after" != "$owner_before" ]; then
      diff -u <(printf '%s\n' "$owner_before") <(printf '%s\n' "$owner_after") | tail -n 40 >&2 || true
      fail 'denied assessment changed owner state'
    fi
  done
  printf 'ok: canonical remote sync/code and owner role denial without state writes\n'
else
  check_remote_preview preview-remote
fi
grep -Fqx "proxyjump $OWNER_ALIAS" "$STATE/code.options" || fail 'remote ordinary alias lost ProxyJump'
printf 'ok: remote owner stays free of controller-port preview listeners\n'

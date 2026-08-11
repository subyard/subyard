#!/usr/bin/env bash
# Minimal stock Orca server inside a yard; owner-host transport stays outside.
set -euo pipefail

RESOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBYARD_ROOT="$(cd "$RESOURCE_DIR/../../../../.." && pwd)"
SCRIPT_DIR="$SUBYARD_ROOT/scripts"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"
# shellcheck source=scripts/lib-service.sh
. "$SCRIPT_DIR/lib-service.sh"

ORCA_PROFILE_DIR="$(cd "$RESOURCE_DIR/../.." && pwd)"
# shellcheck source=config/profiles/orca/release.env
. "$ORCA_PROFILE_DIR/release.env"
ORCA_UNIT=subyard-orca.service
ORCA_DEVICE=orca-server
ORCA_EXEC=/usr/bin/orca-ide
ORCA_STATE=/srv/agents/orca
ORCA_READY="$ORCA_STATE/ready.json"
ORCA_CAPTURE=/usr/local/libexec/subyard/orca-capture-ready
ORCA_INGRESS=/usr/local/libexec/subyard/orca-ingress
ORCA_SYNC=/usr/local/libexec/subyard/projects-changed.d/orca
ORCA_CONTRACT_DIGEST=/usr/local/libexec/subyard/orca-contract.sha256
ORCA_CONTRACT_VERSION=1
ORCA_GUEST_PORT=6768
ORCA_RUNTIME_CHANGED=0
ORCA_TMP_DIR=
ORCA_GUEST_TMP_DIR=

valid_guest_tmp_dir() {
  [[ "$1" =~ ^/tmp/subyard-orca\.[A-Za-z0-9]{6,}$ ]]
}

cleanup_guest() {
  [ -z "$ORCA_GUEST_TMP_DIR" ] && return 0
  valid_guest_tmp_dir "$ORCA_GUEST_TMP_DIR" || return 1
  yexec rm -rf -- "$ORCA_GUEST_TMP_DIR" || return 1
  ORCA_GUEST_TMP_DIR=
}

cleanup() {
  local status=$? cleanup_failed=0
  cleanup_guest >/dev/null 2>&1 || cleanup_failed=1
  if [ -n "$ORCA_TMP_DIR" ]; then
    rm -rf -- "$ORCA_TMP_DIR" || cleanup_failed=1
  fi
  [ "$status" -ne 0 ] || [ "$cleanup_failed" -eq 0 ] || return 1
  return "$status"
}
trap cleanup EXIT

device_exists() {
  incus config device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null |
    grep -qx "$ORCA_DEVICE"
}

device_value() {
  incus config device get "$YARD_INSTANCE_NAME" "$ORCA_DEVICE" "$1" "${PROJ[@]}" 2>/dev/null
}

release_ready() {
  yexec test -x "$ORCA_EXEC" >/dev/null 2>&1 &&
    [ "$(yexec dpkg-query -W -f='${Version}' orca-ide 2>/dev/null)" = "$ORCA_VERSION" ]
}

readiness_ready() {
  yexec jq -e '
    .type == "orca_server_ready" and
    .schemaVersion == 1 and
    .pairing.available == true and
    (.pairing.url | type == "string" and startswith("orca://pair?"))
  ' "$ORCA_READY" >/dev/null 2>&1
}

service_ready() {
  yexec systemctl is-active --quiet "$ORCA_UNIT" >/dev/null 2>&1 &&
    yexec bash -c "exec 3<>/dev/tcp/127.0.0.1/$ORCA_GUEST_PORT" >/dev/null 2>&1 &&
    readiness_ready
}

owner_endpoint_ready() {
  curl --silent --output /dev/null --connect-timeout 3 --max-time 5 \
    "http://$ORCA_OWNER_IP:$ORCA_HOST_PORT/"
}

ingress_active() {
  yexec nft list chain inet subyard_orca input 2>/dev/null |
    grep -Fq 'comment "subyard-orca-managed"'
}

wait_service_ready() {
  local _
  for _ in $(seq 1 120); do
    service_ready && return 0
    sleep 1
  done
  return 1
}

wait_owner_endpoint() {
  local _
  for _ in $(seq 1 30); do
    owner_endpoint_ready && return 0
    sleep 1
  done
  return 1
}

select_release() {
  case "$(yexec dpkg --print-architecture)" in
    amd64)
      ORCA_RELEASE_URL="$ORCA_DEB_AMD64_URL"
      ORCA_RELEASE_SHA256="$ORCA_DEB_AMD64_SHA256"
      ;;
    arm64)
      ORCA_RELEASE_URL="$ORCA_DEB_ARM64_URL"
      ORCA_RELEASE_SHA256="$ORCA_DEB_ARM64_SHA256"
      ;;
    *) die "Orca $ORCA_VERSION has no pinned deb for this yard architecture" ;;
  esac
}

require_runtime_settings() {
  [ -n "${ORCA_HOST_PORT:-}" ] \
    || die "ORCA_HOST_PORT is required for this yard (set a unique per-yard port)"
  [ -n "${ORCA_ADVERTISE_HOST:-}" ] \
    || die "ORCA_ADVERTISE_HOST is required (Tailscale hostname or 127.0.0.1 for SSH)"
  [[ "$ORCA_ADVERTISE_HOST" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] \
    || die "ORCA_ADVERTISE_HOST must be a hostname or IPv4 address without scheme, path or port"
  case "$ORCA_HOST_PORT" in
    *[!0-9]*|'') die "ORCA_HOST_PORT must be a decimal integer" ;;
  esac
  [ "$ORCA_HOST_PORT" -ge 1 ] && [ "$ORCA_HOST_PORT" -le 65535 ] \
    || die "ORCA_HOST_PORT must be in range 1..65535"
  [ "$ORCA_HOST_PORT" != "${SSH_PORT:-}" ] \
    || die "ORCA_HOST_PORT collides with this yard's SSH_PORT"
  [ "$ORCA_HOST_PORT" != "${ADB_PROXY_PORT:-}" ] \
    || die "ORCA_HOST_PORT collides with this yard's ADB_PROXY_PORT"
  [ "$ORCA_HOST_PORT" != "${ADB_CONSOLE_PROXY_PORT:-}" ] \
    || die "ORCA_HOST_PORT collides with this yard's ADB_CONSOLE_PROXY_PORT"
}

resolve_owner_address() {
  local candidate count=0
  local -a tailscale_ips=() resolved_ips=()
  case "$ORCA_ADVERTISE_HOST" in
    127.0.0.1|localhost)
      ORCA_OWNER_IP=127.0.0.1
      ORCA_TRANSPORT=SSH
      return
      ;;
  esac
  command -v tailscale >/dev/null 2>&1 \
    || die "tailscale CLI is required on the owner host for a non-loopback Orca address"
  mapfile -t tailscale_ips < <(tailscale ip -4 2>/dev/null | sed '/^$/d')
  [ "${#tailscale_ips[@]}" -gt 0 ] || die "the owner host has no active Tailscale IPv4 address"
  mapfile -t resolved_ips < <(
    getent ahostsv4 "$ORCA_ADVERTISE_HOST" 2>/dev/null |
      awk '{print $1}' | sort -u
  )
  for candidate in "${tailscale_ips[@]}"; do
    if printf '%s\n' "${resolved_ips[@]}" | grep -Fqx "$candidate" &&
      ip -4 -brief address show scope global |
        awk '{sub(/\/.*/, "", $3); print $3}' | grep -Fqx "$candidate"; then
      ORCA_OWNER_IP="$candidate"
      count=$((count + 1))
    fi
  done
  [ "$count" -eq 1 ] \
    || die "ORCA_ADVERTISE_HOST must resolve to exactly one active IPv4 address from 'tailscale ip -4'"
  ORCA_TRANSPORT=Tailscale
}

route_matches() {
  device_exists &&
    [ "$(device_value listen)" = "tcp:$ORCA_OWNER_IP:$ORCA_HOST_PORT" ] &&
    [ "$(device_value connect)" = "tcp:127.0.0.1:$ORCA_GUEST_PORT" ]
}

refuse_port_collision() {
  route_matches && return 0
  if ss -Hltn "sport = :$ORCA_HOST_PORT" |
    awk '{print $4}' |
    grep -Eq "^($ORCA_OWNER_IP|0\\.0\\.0\\.0|\\*):$ORCA_HOST_PORT$"; then
    die "owner endpoint $ORCA_OWNER_IP:$ORCA_HOST_PORT is already in use"
  fi
}

download_release() {
  ORCA_TMP_DIR="$(mktemp -d)"
  ORCA_ARTIFACT="$ORCA_TMP_DIR/orca-ide_$ORCA_VERSION.deb"
  info "downloading pinned Orca $ORCA_VERSION deb"
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
    --retry 3 --retry-all-errors --connect-timeout 20 --max-time 1800 \
    "$ORCA_RELEASE_URL" -o "$ORCA_ARTIFACT" \
    || die "could not download pinned Orca $ORCA_VERSION"
  printf '%s  %s\n' "$ORCA_RELEASE_SHA256" "$ORCA_ARTIFACT" |
    sha256sum -c - >/dev/null ||
    die "Orca deb SHA-256 mismatch; installed runtime was not changed"
}

dependencies_ready() {
  yexec bash -se <<'YARD'
for package in file jq nftables zlib1g-dev \
  libasound2t64 libgbm1 libgtk-3-0t64 libnss3; do
  [ "$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null)" = 'install ok installed' ] || exit 1
done
YARD
}

ensure_dependencies() {
  dependencies_ready && { ok "Orca headless dependencies already installed"; return 0; }
  info "installing Orca headless dependencies"
  yexec apt-get update -qq
  yexec env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    file jq nftables zlib1g-dev \
    libasound2t64 libgbm1 libgtk-3-0t64 libnss3 >/dev/null
  dependencies_ready || die "Orca headless dependencies did not converge"
}

install_release() {
  release_ready && { ok "Orca $ORCA_VERSION release already verified"; return 0; }
  download_release
  local guest_artifact="/tmp/subyard-orca-$ORCA_VERSION.deb"
  incus file push "$ORCA_ARTIFACT" "$YARD_INSTANCE_NAME$guest_artifact" \
    "${PROJ[@]}" --mode 0644 >/dev/null
  yexec env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$guest_artifact" >/dev/null
  yexec rm -f -- "$guest_artifact"
  release_ready || die "verified Orca release did not install correctly"
  ORCA_RUNTIME_CHANGED=1
  ok "installed verified Orca $ORCA_VERSION release"
}

stage_runtime_contract() {
  ORCA_TMP_DIR="${ORCA_TMP_DIR:-$(mktemp -d)}"
  local ingress="$ORCA_TMP_DIR/orca-ingress"
  local capture="$ORCA_TMP_DIR/orca-capture-ready"
  local sync="$ORCA_TMP_DIR/orca-sync"
  local unit="$ORCA_TMP_DIR/$ORCA_UNIT"
  ORCA_GUEST_TMP_DIR="$(yexec mktemp -d /tmp/subyard-orca.XXXXXX)"
  valid_guest_tmp_dir "$ORCA_GUEST_TMP_DIR" \
    || die "Orca guest staging returned an unsafe temporary path"
  local guest_ingress="$ORCA_GUEST_TMP_DIR/orca-ingress"
  local guest_capture="$ORCA_GUEST_TMP_DIR/orca-capture-ready"
  local guest_sync="$ORCA_GUEST_TMP_DIR/orca-sync"
  local guest_unit="$ORCA_GUEST_TMP_DIR/$ORCA_UNIT"
  cat >"$ingress" <<'INGRESS'
#!/usr/bin/env bash
set -euo pipefail
table=subyard_orca
marker=subyard-orca-managed
case "${1:-}" in
  up)
    port="${2:?guest port is required}"
    if nft list table inet "$table" >/dev/null 2>&1; then
      nft list chain inet "$table" input | grep -Fq "comment \"$marker\"" \
        || { printf 'refusing unowned nft table inet %s\n' "$table" >&2; exit 1; }
      nft delete table inet "$table"
    fi
    nft add table inet "$table"
    nft "add chain inet $table input { type filter hook input priority -10; policy accept; comment \"$marker\"; }"
    nft add rule inet "$table" input iifname != lo tcp dport "$port" reject
    ;;
  down)
    if nft list table inet "$table" >/dev/null 2>&1; then
      nft list chain inet "$table" input | grep -Fq "comment \"$marker\"" \
        || { printf 'refusing unowned nft table inet %s\n' "$table" >&2; exit 1; }
      nft delete table inet "$table"
    fi
    ;;
  *) printf 'usage: %s up <port> | down\n' "$0" >&2; exit 2 ;;
esac
INGRESS
  cat >"$capture" <<'CAPTURE'
#!/usr/bin/env bash
set -euo pipefail
ready="${1:?ready file is required}"
shift
umask 077
: >"$ready"
exec "$@" >"$ready"
CAPTURE
  cat >"$sync" <<SYNC_HEAD
#!/usr/bin/env bash
set -euo pipefail
ORCA_EXEC=$ORCA_EXEC
ORCA_UNIT=$ORCA_UNIT
export HOME=/home/${DEV_USER:-dev}
export XDG_CONFIG_HOME=$ORCA_STATE/config
export XDG_DATA_HOME=$ORCA_STATE/data
export XDG_STATE_HOME=$ORCA_STATE/state
cd /srv/workspaces
SYNC_HEAD
  cat >>"$sync" <<'SYNC'
systemctl is-active --quiet "$ORCA_UNIT" || exit 0
repos="$("$ORCA_EXEC" repo list --json)"
while IFS= read -r -d '' metadata; do
  project_dir="${metadata%/.subyard-meta.json}"
  project_id="${project_dir##*/}"
  checkout="$project_dir/src"
  jq -e --arg id "$project_id" \
    '.schema == 1 and .projectId == $id' "$metadata" >/dev/null 2>&1 || continue
  [ -d "$checkout" ] && [ ! -L "$checkout" ] || continue
  [ "$(realpath -e "$checkout")" = "$checkout" ] || continue
  if ! jq -e --arg path "$checkout" \
    '.ok == true and any(.result.repos[]?; .path == $path)' <<<"$repos" >/dev/null; then
    "$ORCA_EXEC" repo add --path "$checkout" --json >/dev/null
    repos="$("$ORCA_EXEC" repo list --json)"
  fi
done < <(
  find /srv/workspaces -mindepth 2 -maxdepth 2 -type f \
    -name .subyard-meta.json -print0 | sort -z
)
SYNC
  cat >"$unit" <<UNIT
[Unit]
Description=Subyard Orca remote server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${DEV_USER:-dev}
Group=${DEV_USER:-dev}
Environment=HOME=/home/${DEV_USER:-dev}
Environment=XDG_CONFIG_HOME=$ORCA_STATE/config
Environment=XDG_DATA_HOME=$ORCA_STATE/data
Environment=XDG_STATE_HOME=$ORCA_STATE/state
Environment=LIBGL_ALWAYS_SOFTWARE=1
WorkingDirectory=/srv/workspaces
ExecStartPre=+$ORCA_INGRESS up $ORCA_GUEST_PORT
ExecStart=$ORCA_CAPTURE $ORCA_READY $ORCA_EXEC serve --port $ORCA_GUEST_PORT --pairing-address $ORCA_ADVERTISE_HOST:$ORCA_HOST_PORT --json
ExecStopPost=+$ORCA_INGRESS down
Restart=on-failure
RestartSec=2
TimeoutStartSec=120
TimeoutStopSec=30
KillMode=mixed
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT
  chmod 0755 "$ingress" "$capture" "$sync"
  incus file push "$ingress" "$YARD_INSTANCE_NAME$guest_ingress" \
    "${PROJ[@]}" --mode 0755 >/dev/null
  incus file push "$capture" "$YARD_INSTANCE_NAME$guest_capture" \
    "${PROJ[@]}" --mode 0755 >/dev/null
  incus file push "$sync" "$YARD_INSTANCE_NAME$guest_sync" \
    "${PROJ[@]}" --mode 0755 >/dev/null
  incus file push "$unit" "$YARD_INSTANCE_NAME$guest_unit" \
    "${PROJ[@]}" --mode 0644 >/dev/null
  if ! yexec cmp -s "$guest_ingress" "$ORCA_INGRESS" ||
    ! yexec cmp -s "$guest_capture" "$ORCA_CAPTURE" ||
    ! yexec cmp -s "$guest_sync" "$ORCA_SYNC" ||
    ! yexec cmp -s "$guest_unit" "/etc/systemd/system/$ORCA_UNIT" ||
    ! ingress_active; then
    ORCA_RUNTIME_CHANGED=1
  fi
  yexec install -d -m 0755 "$(dirname "$ORCA_CAPTURE")" "$(dirname "$ORCA_SYNC")"
  yexec install -m 0755 "$guest_ingress" "$ORCA_INGRESS"
  yexec install -m 0755 "$guest_capture" "$ORCA_CAPTURE"
  yexec install -m 0755 "$guest_sync" "$ORCA_SYNC"
  yexec install -m 0644 "$guest_unit" "/etc/systemd/system/$ORCA_UNIT"
  yexec bash -se -- "$ORCA_CONTRACT_DIGEST" "$ORCA_CONTRACT_VERSION" \
    "$ORCA_INGRESS" "$ORCA_CAPTURE" "$ORCA_SYNC" "/etc/systemd/system/$ORCA_UNIT" <<'YARD'
set -euo pipefail
marker="$1"; version="$2"; shift 2
digest="$(sha256sum "$@" | sha256sum | awk '{print $1}')"
temporary="$marker.$$"
printf '%s:%s\n' "$version" "$digest" >"$temporary"
chmod 0644 "$temporary"
mv "$temporary" "$marker"
YARD
  yexec bash -se -- "${DEV_USER:-dev}" "$ORCA_STATE" "$ORCA_READY" <<'YARD'
set -euo pipefail
dev_user="$1"
state="$2"
ready="$3"
install -d -o "$dev_user" -g "$dev_user" -m 0700 \
  "$state" "$state/config" "$state/data" "$state/state"
touch "$ready"
chown "$dev_user:$dev_user" "$ready"
chmod 0600 "$ready"
YARD
  yexec systemctl daemon-reload
  cleanup_guest || die "Orca guest staging directory could not be removed"
}

remove_route() {
  device_exists || return 0
  incus config device remove "$YARD_INSTANCE_NAME" "$ORCA_DEVICE" "${PROJ[@]}" >/dev/null
}

ensure_route() {
  if route_matches; then
    ok "owner route already exact: $ORCA_OWNER_IP:$ORCA_HOST_PORT"
    return 0
  fi
  remove_route
  incus config device add "$YARD_INSTANCE_NAME" "$ORCA_DEVICE" proxy "${PROJ[@]}" \
    "listen=tcp:$ORCA_OWNER_IP:$ORCA_HOST_PORT" \
    "connect=tcp:127.0.0.1:$ORCA_GUEST_PORT" bind=host >/dev/null
}

run_project_sync() {
  yexec runuser -u "${DEV_USER:-dev}" -- "$ORCA_SYNC"
}

runtime_contract_ready() {
  if ! yexec bash -se -- "$ORCA_CONTRACT_DIGEST" "$ORCA_CONTRACT_VERSION" \
    "$ORCA_INGRESS" "$ORCA_CAPTURE" "$ORCA_SYNC" "/etc/systemd/system/$ORCA_UNIT" <<'YARD'
set -euo pipefail
marker="$1"; version="$2"; shift 2
[ -r "$marker" ]
expected="$(cat "$marker")"
digest="$(sha256sum "$@" | sha256sum | awk '{print $1}')"
[ "$expected" = "$version:$digest" ]
YARD
  then
    return 1
  fi
  yexec grep -Fqx \
    "ExecStart=$ORCA_CAPTURE $ORCA_READY $ORCA_EXEC serve --port $ORCA_GUEST_PORT --pairing-address $ORCA_ADVERTISE_HOST:$ORCA_HOST_PORT --json" \
    "/etc/systemd/system/$ORCA_UNIT" >/dev/null 2>&1
}

service_enabled() {
  yexec systemctl is-enabled --quiet "$ORCA_UNIT" >/dev/null 2>&1
}

# Read-only comparison of canonical Subyard roots with the repos Orca already knows.
projects_synced() {
  yexec bash -se -- "${DEV_USER:-dev}" "$ORCA_EXEC" "$ORCA_STATE" <<'YARD'
set -euo pipefail
dev_user="$1"; orca_exec="$2"; state="$3"
export HOME="/home/$dev_user"
export XDG_CONFIG_HOME="$state/config"
export XDG_DATA_HOME="$state/data"
export XDG_STATE_HOME="$state/state"
repos="$(runuser -u "$dev_user" -- "$orca_exec" repo list --json)"
while IFS= read -r -d '' metadata; do
  project_dir="${metadata%/.subyard-meta.json}"
  project_id="${project_dir##*/}"
  checkout="$project_dir/src"
  jq -e --arg id "$project_id" \
    '.schema == 1 and .projectId == $id' "$metadata" >/dev/null 2>&1 || continue
  [ -d "$checkout" ] && [ ! -L "$checkout" ] || continue
  [ "$(realpath -e "$checkout")" = "$checkout" ] || continue
  jq -e --arg path "$checkout" \
    '.ok == true and any(.result.repos[]?; .path == $path)' <<<"$repos" >/dev/null || exit 1
done < <(
  find /srv/workspaces -mindepth 2 -maxdepth 2 -type f \
    -name .subyard-meta.json -print0 | sort -z
)
YARD
}

up_converged() {
  release_ready && dependencies_ready && runtime_contract_ready && service_enabled &&
    service_ready && ingress_active && route_matches && owner_endpoint_ready && projects_synced
}

cmd_up() {
  require_runtime_settings
  resolve_owner_address
  select_release
  refuse_port_collision
  if up_converged; then
    ok "Orca runtime, route and project registrations are already converged"
    return 0
  fi
  ensure_dependencies
  install_release
  stage_runtime_contract
  yexec systemctl enable "$ORCA_UNIT" >/dev/null
  if yexec systemctl is-active --quiet "$ORCA_UNIT"; then
    [ "$ORCA_RUNTIME_CHANGED" -eq 0 ] || yexec systemctl restart "$ORCA_UNIT"
  else
    yexec systemctl start "$ORCA_UNIT"
  fi
  if ! wait_service_ready; then
    yexec journalctl -u "$ORCA_UNIT" --no-pager -n 80 >&2 || true
    remove_route
    yexec systemctl disable --now "$ORCA_UNIT" >/dev/null 2>&1 || true
    die "Orca service did not become ready; owner route was not published"
  fi
  if ! ensure_route || ! wait_owner_endpoint; then
    remove_route
    yexec systemctl disable --now "$ORCA_UNIT" >/dev/null 2>&1 || true
    die "Orca owner endpoint failed readiness; route and service were rolled back"
  fi
  run_project_sync
  ok "Orca ready through $ORCA_TRANSPORT at $ORCA_ADVERTISE_HOST:$ORCA_HOST_PORT"
}

cmd_pair() {
  require_runtime_settings
  service_ready || die "Orca is not ready; run '$(yard_cmd_hint) orca up' first"
  yexec systemctl restart "$ORCA_UNIT"
  wait_service_ready || die "Orca did not become ready after restart"
  yexec jq -er '
    select(.type == "orca_server_ready" and .schemaVersion == 1) |
    .pairing | select(.available == true) | .url
  ' "$ORCA_READY" || die "Orca did not publish a pairing link"
}

cmd_sync() {
  yexec systemctl is-active --quiet "$ORCA_UNIT" \
    || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
  run_project_sync
  ok "Subyard project roots are registered in Orca"
}

cmd_status() {
  select_release
  printf 'Orca %s in yard %s\n' "$ORCA_VERSION" "${YARD_NAME:-default}"
  if release_ready; then ok "pinned release verified"; else warn "pinned release missing or corrupt"; fi
  if service_ready; then ok "service ready"; else warn "service inactive or not ready"; fi
  if ingress_active; then ok "L1 ingress guard active"; else warn "L1 ingress guard inactive"; fi
  if device_exists; then
    ok "owner route: $(device_value listen) -> $(device_value connect)"
  else
    warn "owner route absent"
  fi
}

cmd_down() {
  local active=0 guarded=0 routed=0
  yexec systemctl is-active --quiet "$ORCA_UNIT" && active=1
  ingress_active && guarded=1
  device_exists && routed=1
  if [ "$active" -eq 0 ] && [ "$guarded" -eq 0 ] && [ "$routed" -eq 0 ]; then
    ok "Orca already down"
    return 0
  fi
  remove_route
  yexec systemctl disable --now "$ORCA_UNIT" >/dev/null 2>&1 || true
  if yexec test -x "$ORCA_INGRESS"; then
    yexec "$ORCA_INGRESS" down
  fi
  ok "Orca stopped and unpublished; state preserved"
}

emit_resource_assessment() { # <local-action> <true|false> [fixed consequence...]
  local action="$1" changed="$2" separator=""
  shift 2
  printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":%s,"consequences":[' \
    "$action" "$changed"
  local consequence
  for consequence in "$@"; do
    printf '%s"%s"' "$separator" "$consequence"
    separator=,
  done
  printf ']}\n'
}

require_no_resource_arguments() {
  local verb="$1"
  shift
  [ "$#" -eq 0 ] || die "'$verb' does not accept additional arguments"
}

require_resource_apply() { # <expected-local-action>
  local expected="$1"
  [ "${SUBYARD_RESOURCE_MODE:-}" = apply ] || die "resource apply mode is required"
  [ "${SUBYARD_RESOURCE_ACTION:-}" = "$expected" ] \
    || die "prepared resource action mismatch (expected '$expected')"
  [ -n "${SUBYARD_OPERATION_ID:-}" ] || die "resource apply operation ID is required"
}

prepare_resource() { # <public-verb>
  local verb="$1" changed=false
  shift
  require_no_resource_arguments "$verb" "$@"
  case "$verb" in
    up)
      svc_require_yard_running
      require_runtime_settings
      resolve_owner_address
      select_release
      refuse_port_collision
      release_ready || changed=true
      dependencies_ready || changed=true
      runtime_contract_ready || changed=true
      service_enabled || changed=true
      service_ready || changed=true
      ingress_active || changed=true
      route_matches || changed=true
      owner_endpoint_ready || changed=true
      projects_synced || changed=true
      if [ "$changed" = true ]; then
        emit_resource_assessment up true \
          "converge the pinned Orca package, dependencies and service contract" \
          "publish the owned guarded endpoint for the selected yard" \
          "register canonical Subyard project roots in Orca"
      else
        emit_resource_assessment up false
      fi
      ;;
    pair)
      svc_require_yard_running
      require_runtime_settings
      service_ready || die "Orca is not ready; run '$(yard_cmd_hint) orca up' first"
      emit_resource_assessment pair true \
        "restart the Orca service and issue one fresh single-client pairing link"
      ;;
    sync)
      svc_require_yard_running
      yexec systemctl is-active --quiet "$ORCA_UNIT" \
        || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
      if projects_synced; then
        emit_resource_assessment sync false
      else
        emit_resource_assessment sync true "register missing canonical Subyard project roots in Orca"
      fi
      ;;
    down)
      svc_require_yard_running
      if yexec systemctl is-active --quiet "$ORCA_UNIT" || ingress_active || device_exists; then
        emit_resource_assessment down true \
          "stop the Orca service and ingress guard and remove its owned owner-host proxy"
      else
        emit_resource_assessment down false
      fi
      ;;
    is-up|status|logs)
      emit_resource_assessment "$verb" false
      ;;
    *) die "unknown Orca resource verb '$verb'" ;;
  esac
}

cmd_is_up() {
  incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 || return 1
  yexec systemctl is-active --quiet "$ORCA_UNIT" >/dev/null 2>&1
}

sub="${1:-}"
shift || true

case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    [ -n "$sub" ] || die "resource verb is required"
    prepare_resource "$sub" "$@"
    ;;
  apply)
    case "$sub" in
      up|is-up|status|pair|sync|logs|down) ;;
      *) die "unknown Orca apply verb '$sub'" ;;
    esac
    require_no_resource_arguments "$sub" "$@"
    require_resource_apply "$sub"
    if [ "$sub" = is-up ]; then cmd_is_up; exit $?; fi
    svc_require_yard_running
    case "$sub" in
      up) cmd_up ;;
      status) cmd_status ;;
      pair) cmd_pair ;;
      sync) cmd_sync ;;
      logs) yexec journalctl --no-pager -u "$ORCA_UNIT" ;;
      down) cmd_down ;;
    esac
    ;;
  '')
    case "$sub" in
      is-up) require_no_resource_arguments is-up "$@"; cmd_is_up ;;
      -h|--help|help|"")
        printf 'Usage: %s orca <up|is-up|status|pair|sync|logs|down>\n' "${PROG:-yard}"
        ;;
      *) die "typed resource dispatcher required for 'yard orca $sub'" ;;
    esac
    ;;
  *) die "unknown resource execution mode '${SUBYARD_RESOURCE_MODE:-}'" ;;
esac

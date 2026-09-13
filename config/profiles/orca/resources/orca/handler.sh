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
ORCA_REGISTRATION=/usr/local/libexec/subyard/orca-registration
ORCA_CODEX_PROFILE=/etc/profile.d/subyard-orca-codex.sh
ORCA_CONTRACT_DIGEST=/usr/local/libexec/subyard/orca-contract.sha256
ORCA_CONTRACT_VERSION=3
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

service_endpoint_ready() {
  yexec systemctl is-active --quiet "$ORCA_UNIT" >/dev/null 2>&1 &&
    yexec bash -c "exec 3<>/dev/tcp/127.0.0.1/$ORCA_GUEST_PORT" >/dev/null 2>&1
}

service_ready() {
  service_endpoint_ready && readiness_ready
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

wait_service_endpoint_ready() {
  local _
  for _ in $(seq 1 120); do
    service_endpoint_ready && return 0
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
for package in file git python3 jq nftables zlib1g-dev \
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
    file git python3 jq nftables zlib1g-dev \
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
  local codex_profile="$ORCA_TMP_DIR/orca-codex-profile"
  local unit="$ORCA_TMP_DIR/$ORCA_UNIT"
  local source guest_helper helper contract_version
  local -a helpers=()
  mapfile -t helpers < <(registration_files)
  contract_version="$(registration_contract_version)" || die "Orca registration helper contract unavailable"
  ORCA_GUEST_TMP_DIR="$(yexec mktemp -d /tmp/subyard-orca.XXXXXX)"
  valid_guest_tmp_dir "$ORCA_GUEST_TMP_DIR" \
    || die "Orca guest staging returned an unsafe temporary path"
  local guest_ingress="$ORCA_GUEST_TMP_DIR/orca-ingress"
  local guest_capture="$ORCA_GUEST_TMP_DIR/orca-capture-ready"
  local guest_sync="$ORCA_GUEST_TMP_DIR/orca-sync"
  local guest_codex_profile="$ORCA_GUEST_TMP_DIR/orca-codex-profile"
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
  # Orca's bash startup sources /etc/profile before injecting its agent command.
  # Remote clients can send their own stock YOLO argument, bypassing server defaults.
  # Keep the native CLI and SSH/VS Code shells unchanged; this is a launch default,
  # not a security boundary against explicit commands or in-session mode changes.
  cat >"$codex_profile" <<CODEX_PROFILE
if [ "\${SUBYARD_ORCA_CODEX_CONFIG:-}" = 1 ]; then
  codex() { /usr/bin/python3 -B $ORCA_REGISTRATION/codex_launch.py "\$@"; }
fi
CODEX_PROFILE
  cat >"$sync" <<SYNC_HEAD
#!/usr/bin/env bash
set -euo pipefail
systemctl is-active --quiet $ORCA_UNIT || exit 0
/usr/bin/python3 -B $ORCA_REGISTRATION/settings.py
status=0
report="\$(/usr/bin/python3 -B $ORCA_REGISTRATION/main.py sync)" || status=\$?
if ! jq -e '(.ready | type == "boolean") and (.errors | type == "array") and (.warnings | type == "array")' <<<"\$report" >/dev/null; then
  printf 'Orca project registration failed; run yard orca status\n' >&2
  exit 1
fi
jq -r '(.errors[] | "Orca registration error: " + .), (.warnings[] | "Orca registration warning: " + .)' <<<"\$report" >&2
jq -r '"Orca checkouts registered: \(.registered)/\(.total)"' <<<"\$report"
jq -e '.ready' <<<"\$report" >/dev/null || status=1
exit "\$status"
SYNC_HEAD
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
Environment=SUBYARD_ORCA_CODEX_CONFIG=1
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
  incus file push "$codex_profile" "$YARD_INSTANCE_NAME$guest_codex_profile" \
    "${PROJ[@]}" --mode 0644 >/dev/null
  incus file push "$unit" "$YARD_INSTANCE_NAME$guest_unit" \
    "${PROJ[@]}" --mode 0644 >/dev/null
  yexec install -d -m 0755 "$ORCA_REGISTRATION"
  for source in "$RESOURCE_DIR"/registration/*.py; do
    guest_helper="$ORCA_GUEST_TMP_DIR/${source##*/}"
    helper="$ORCA_REGISTRATION/${source##*/}"
    incus file push "$source" "$YARD_INSTANCE_NAME$guest_helper" \
      "${PROJ[@]}" --mode 0644 >/dev/null
    yexec cmp -s "$guest_helper" "$helper" || ORCA_RUNTIME_CHANGED=1
    yexec install -m 0644 "$guest_helper" "$helper"
  done
  if ! yexec cmp -s "$guest_ingress" "$ORCA_INGRESS" ||
    ! yexec cmp -s "$guest_capture" "$ORCA_CAPTURE" ||
    ! yexec cmp -s "$guest_sync" "$ORCA_SYNC" ||
    ! yexec cmp -s "$guest_codex_profile" "$ORCA_CODEX_PROFILE" ||
    ! yexec cmp -s "$guest_unit" "/etc/systemd/system/$ORCA_UNIT" ||
    ! ingress_active; then
    ORCA_RUNTIME_CHANGED=1
  fi
  yexec install -d -m 0755 "$(dirname "$ORCA_CAPTURE")" "$(dirname "$ORCA_SYNC")"
  yexec install -m 0755 "$guest_ingress" "$ORCA_INGRESS"
  yexec install -m 0755 "$guest_capture" "$ORCA_CAPTURE"
  yexec install -m 0755 "$guest_sync" "$ORCA_SYNC"
  yexec install -m 0644 "$guest_codex_profile" "$ORCA_CODEX_PROFILE"
  yexec install -m 0644 "$guest_unit" "/etc/systemd/system/$ORCA_UNIT"
  yexec bash -se -- "$ORCA_CONTRACT_DIGEST" "$contract_version" \
    "$ORCA_INGRESS" "$ORCA_CAPTURE" "$ORCA_SYNC" "$ORCA_CODEX_PROFILE" "/etc/systemd/system/$ORCA_UNIT" "${helpers[@]}" <<'YARD'
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

codex_defaults_ready() {
  yexec runuser -u "${DEV_USER:-dev}" -- /usr/bin/python3 -B \
    "$ORCA_REGISTRATION/settings.py" --check >/dev/null 2>&1
}

runtime_contract_ready() {
  local contract_version
  local -a helpers=()
  mapfile -t helpers < <(registration_files)
  contract_version="$(registration_contract_version)" || return 1
  if ! yexec bash -se -- "$ORCA_CONTRACT_DIGEST" "$contract_version" \
    "$ORCA_INGRESS" "$ORCA_CAPTURE" "$ORCA_SYNC" "$ORCA_CODEX_PROFILE" "/etc/systemd/system/$ORCA_UNIT" "${helpers[@]}" <<'YARD'
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

# Read-only discovery, kind and group checks use the same component as the project hook.
project_registration_report() {
  local report
  report="$(yexec runuser -u "${DEV_USER:-dev}" -- /usr/bin/python3 -B \
    "$ORCA_REGISTRATION/main.py" status)" || true
  jq -e '(.ready | type == "boolean") and
    (.registered | type == "number") and (.total | type == "number") and
    .registered >= 0 and .registered <= .total and
    (.errors | type == "array") and (.warnings | type == "array")' \
    <<<"$report" >/dev/null || return 1
  printf '%s\n' "$report"
}

projects_synced() {
  local report
  report="$(project_registration_report)" || return 1
  jq -e '.ready == true' <<<"$report" >/dev/null
}

# A source digest makes a new profile helper invalidate an otherwise intact installed contract.
registration_contract_version() {
  local digest
  digest="$(cd "$RESOURCE_DIR/registration" && sha256sum ./*.py | sha256sum | cut -d ' ' -f 1)" || return 1
  printf '%s:%s\n' "$ORCA_CONTRACT_VERSION" "$digest"
}

registration_files() {
  local source
  for source in "$RESOURCE_DIR"/registration/*.py; do
    [ -f "$source" ] || return 1
    printf '%s/%s\n' "$ORCA_REGISTRATION" "${source##*/}"
  done
}

orca_profile_selected() {
  local profile
  local -a profiles=()
  if [ -n "${ENVIRONMENT_PROFILES:-}" ]; then
    read -r -a profiles <<<"$ENVIRONMENT_PROFILES"
  fi
  for profile in "${profiles[@]}"; do
    [ "$profile" = orca ] && return 0
  done
  return 1
}

project_dispatcher_ready() {
  local expected
  expected="$(sha256sum "$SUBYARD_ROOT/config/projects-changed.sh" | cut -d ' ' -f 1)" || return 1
  yexec test -x /usr/local/libexec/subyard/projects-changed >/dev/null 2>&1 || return 1
  yexec bash -se -- "$expected" <<'YARD'
set -euo pipefail
[ "$(sha256sum /usr/local/libexec/subyard/projects-changed | cut -d ' ' -f 1)" = "$1" ]
[ -r /etc/subyard/agent-project-hooks ]
[ -d /usr/local/libexec/subyard/projects-changed.d ]
YARD
}

automatic_project_hook_ready() {
  project_dispatcher_ready &&
    yexec test -x "$ORCA_SYNC" >/dev/null 2>&1 && runtime_contract_ready
}

up_converged() {
  release_ready && dependencies_ready && runtime_contract_ready && service_enabled &&
    service_ready && ingress_active && route_matches && owner_endpoint_ready &&\
    automatic_project_hook_ready && codex_defaults_ready && projects_synced
}

cmd_up() {
  require_runtime_settings
  resolve_owner_address
  select_release
  refuse_port_collision
  project_dispatcher_ready || die "automatic project dispatcher missing or stale; run '$(yard_cmd_hint) init'"
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
  codex_defaults_ready || die "Orca Codex launch defaults did not converge"
  automatic_project_hook_ready && projects_synced || die "Orca project registration did not converge"
  ok "Orca ready through $ORCA_TRANSPORT at $ORCA_ADVERTISE_HOST:$ORCA_HOST_PORT"
}

cmd_pair() {
  require_runtime_settings
  resolve_owner_address
  runtime_contract_ready && route_matches && owner_endpoint_ready \
    || die "Orca endpoint settings are not applied; run '$(yard_cmd_hint) orca up' first"
  service_ready || die "Orca is not ready; run '$(yard_cmd_hint) orca up' first"
  yexec systemctl restart "$ORCA_UNIT"
  wait_service_ready || die "Orca did not become ready after restart"
  wait_owner_endpoint || die "Orca owner endpoint is not reachable after restart"
  run_project_sync
  projects_synced || die "Orca project registrations did not converge"
  yexec jq -er '
    select(.type == "orca_server_ready" and .schemaVersion == 1) |
    .pairing | select(.available == true) | .url
  ' "$ORCA_READY" || die "Orca did not publish a pairing link"
}

cmd_sync() {
  yexec systemctl is-active --quiet "$ORCA_UNIT" \
    || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
  run_project_sync
  projects_synced || die "Orca project registration did not converge"
  ok "Subyard roots and nested Git checkouts are registered in their Orca project groups"
}

cmd_restart() {
  yexec systemctl is-active --quiet "$ORCA_UNIT" \
    || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
  yexec systemctl restart "$ORCA_UNIT"
  wait_service_endpoint_ready || die "Orca did not become ready after restart"
  ok "Orca service restarted"
}

cmd_status() {
  local report registered total diagnostic service_is_ready=0
  select_release
  printf 'Orca %s in yard %s\n' "$ORCA_VERSION" "${YARD_NAME:-default}"
  if orca_profile_selected; then
    ok "Orca profile selected for yard init"
  else
    warn "Orca profile is not selected in ENVIRONMENT_PROFILES; yard init will omit it"
  fi
  if release_ready; then ok "pinned release verified"; else warn "pinned release missing or corrupt"; fi
  if service_endpoint_ready; then service_is_ready=1; ok "service ready"; else warn "service inactive or not ready"; fi
  if ingress_active; then ok "L1 ingress guard active"; else warn "L1 ingress guard inactive"; fi
  if device_exists; then
    ok "owner route: $(device_value listen) -> $(device_value connect)"
  else
    warn "owner route absent"
  fi
  if automatic_project_hook_ready; then
    ok "automatic project hook ready"
  elif project_dispatcher_ready; then
    warn "Orca project hook missing or stale; run '$(yard_cmd_hint) orca up'"
  else
    warn "automatic project dispatcher missing or stale; run '$(yard_cmd_hint) init'"
  fi
  if [ "$service_is_ready" -eq 1 ]; then
    if codex_defaults_ready; then
      ok "Codex stock YOLO launch override disabled; yard config supplies defaults"
    else
      warn "Codex launch defaults need repair; run '$(yard_cmd_hint) orca up'"
    fi
    if report="$(project_registration_report)"; then
      registered="$(jq -r '.registered' <<<"$report")"
      total="$(jq -r '.total' <<<"$report")"
      if jq -e '.ready' <<<"$report" >/dev/null; then
        ok "checkouts registered: $registered/$total (project groups and kinds verified)"
      else
        warn "checkouts registered: $registered/$total; registration incomplete; run '$(yard_cmd_hint) orca sync'"
      fi
      while IFS= read -r diagnostic; do
        warn "$diagnostic"
      done < <(jq -r '.errors[], .warnings[]' <<<"$report")
    else
      warn "project registration status unavailable; run '$(yard_cmd_hint) orca up'"
    fi
  else
    warn "project registration status unavailable while service is not ready"
  fi
}

cmd_logs() {
  case "$#" in
    0) yexec journalctl --no-pager -u "$ORCA_UNIT" -n 18000 ;;
    1)
      [ "$1" = --follow ] || svc_usage_error "'logs' accepts only '--follow'"
      yexec journalctl --no-pager -u "$ORCA_UNIT" -n 18000 --follow
      ;;
    *) svc_usage_error "'logs' accepts only '--follow'" ;;
  esac
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
  [ "$#" -eq 0 ] || svc_usage_error "'$verb' does not accept additional arguments"
}

validate_resource_arguments() {
  local verb="$1"
  shift
  if [ "$verb" = logs ]; then
    case "$#" in
      0) return 0 ;;
      1) [ "$1" = --follow ] || svc_usage_error "'logs' accepts only '--follow'" ;;
      *) svc_usage_error "'logs' accepts only '--follow'" ;;
    esac
    return 0
  fi
  require_no_resource_arguments "$verb" "$@"
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
  validate_resource_arguments "$verb" "$@"
  case "$verb" in
    up)
      require_runtime_settings
      resolve_owner_address
      refuse_port_collision
      # The engine composes init with this resource action. A first-run plan
      # must be inspectable before Incus or the selected yard is installed.
      if incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 &&
        [ "$(incus list "$YARD_INSTANCE_NAME" "${PROJ[@]}" -f csv -c s 2>/dev/null)" = RUNNING ]; then
        select_release
        release_ready || changed=true
        dependencies_ready || changed=true
        runtime_contract_ready || changed=true
        service_enabled || changed=true
        service_ready || changed=true
        ingress_active || changed=true
        route_matches || changed=true
        owner_endpoint_ready || changed=true
        automatic_project_hook_ready || changed=true
        codex_defaults_ready || changed=true
        projects_synced || changed=true
      else
        changed=true
      fi
      if [ "$changed" = true ]; then
        emit_resource_assessment up true \
          "converge the pinned Orca package, dependencies and service contract" \
          "use the yard Codex configuration instead of the stock Orca YOLO launch default" \
          "publish the owned guarded endpoint for the selected yard" \
          "register Subyard roots and nested Git checkouts in their project groups"
      else
        emit_resource_assessment up false
      fi
      ;;
    pair)
      svc_require_yard_running
      require_runtime_settings
      resolve_owner_address
      runtime_contract_ready && route_matches && owner_endpoint_ready \
        || die "Orca endpoint settings are not applied; run '$(yard_cmd_hint) orca up' first"
      service_ready || die "Orca is not ready; run '$(yard_cmd_hint) orca up' first"
      emit_resource_assessment pair true \
        "restart the Orca service, reconcile project groups and checkouts and issue one fresh single-client pairing link"
      ;;
    restart)
      svc_require_yard_running
      yexec systemctl is-active --quiet "$ORCA_UNIT" \
        || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
      emit_resource_assessment restart true \
        "restart the existing Orca service without returning a pairing capability"
      ;;
    sync)
      svc_require_yard_running
      yexec systemctl is-active --quiet "$ORCA_UNIT" \
        || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
      # Always run an explicit scan, including diagnostics for stale paths on an otherwise ready catalog.
      emit_resource_assessment sync true "reconcile Subyard project groups, roots and nested Git checkouts"
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
    *) svc_usage_error "unknown Orca resource verb '$verb'" ;;
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
    [ -n "$sub" ] || svc_usage_error "resource verb is required"
    prepare_resource "$sub" "$@"
    ;;
  apply)
    case "$sub" in
      up|is-up|status|pair|restart|sync|logs|down) ;;
      *) die "unknown Orca apply verb '$sub'" ;;
    esac
    validate_resource_arguments "$sub" "$@"
    require_resource_apply "$sub"
    if [ "$sub" = is-up ]; then cmd_is_up; exit $?; fi
    svc_require_yard_running
    case "$sub" in
      up) cmd_up ;;
      status) cmd_status ;;
      pair) cmd_pair ;;
      restart) cmd_restart ;;
      sync) cmd_sync ;;
      logs) cmd_logs "$@" ;;
      down) cmd_down ;;
    esac
    ;;
  '')
    case "$sub" in
      is-up) require_no_resource_arguments is-up "$@"; cmd_is_up ;;
      -h|--help|help|"")
        printf 'Usage: %s orca <up|is-up|status|pair|restart|sync|logs|down>\n' "${PROG:-yard}"
        ;;
      *) die "typed resource dispatcher required for 'yard orca $sub'" ;;
    esac
    ;;
  *) die "unknown resource execution mode '${SUBYARD_RESOURCE_MODE:-}'" ;;
esac

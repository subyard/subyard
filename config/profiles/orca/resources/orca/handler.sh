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
ORCA_MOBILE_REQUEST="$ORCA_READY.mobile-request"
ORCA_PAIR_PENDING=0
ORCA_CAPTURE=/usr/local/libexec/subyard/orca-capture-ready
ORCA_INGRESS=/usr/local/libexec/subyard/orca-ingress
ORCA_SYNC=/usr/local/libexec/subyard/projects-changed.d/orca
ORCA_REGISTRATION=/usr/local/libexec/subyard/orca-registration
ORCA_CODEX_PROFILE=/etc/profile.d/subyard-orca-codex.sh
ORCA_CONTRACT_DIGEST=/usr/local/libexec/subyard/orca-contract.sha256
ORCA_REGISTRATION_DIGEST=/usr/local/libexec/subyard/orca-registration.sha256
ORCA_REGISTRATION_LOCK=/usr/local/libexec/subyard/orca-registration.lock
ORCA_DISCOVERY_UNIT=subyard-orca-discovery.service
ORCA_DISCOVERY_TIMER=subyard-orca-discovery.timer
ORCA_CONTRACT_VERSION=5
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
  if [ "$ORCA_PAIR_PENDING" -eq 1 ]; then
    clear_mobile_request >/dev/null 2>&1 || cleanup_failed=1
  fi
  cleanup_guest >/dev/null 2>&1 || cleanup_failed=1
  if [ -n "$ORCA_TMP_DIR" ]; then
    rm -rf -- "$ORCA_TMP_DIR" || cleanup_failed=1
  fi
  [ "$status" -ne 0 ] || [ "$cleanup_failed" -eq 0 ] || return 1
  return "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

clear_mobile_request() {
  yexec runuser -u "${DEV_USER:-dev}" -- bash -c '
    if [ -f "$1" ] && IFS= read -r owner < "$1" && [ "$owner" = "$2" ]; then
      rm -f -- "$1"
    fi
  ' _ "$ORCA_MOBILE_REQUEST" "$SUBYARD_OPERATION_ID" || return 1
  ORCA_PAIR_PENDING=0
}

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
  # Drain the stream so a matching marker cannot cause producer SIGPIPE.
  yexec nft list chain inet subyard_orca input 2>/dev/null |
    grep -F 'comment "subyard-orca-managed"' >/dev/null
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

orca_device_fingerprint() {
  local configuration
  configuration="$(incus config device show "$YARD_INSTANCE_NAME" "${PROJ[@]}" |
    awk -v device="$ORCA_DEVICE" '$0 == device ":" {inside=1; print; next} inside && /^[^[:space:]]/ {exit} inside {print}')" \
    || die 'Orca native proxy metadata unavailable'
  [ -n "$configuration" ] || die 'Orca native proxy metadata unavailable'
  printf '%s\n' "$configuration" | sha256sum | awk '{print $1}'
}

orca_device_exact_shape() {
  local keys
  keys="$(incus config device show "$YARD_INSTANCE_NAME" "${PROJ[@]}" |
    awk -v device="$ORCA_DEVICE" '$0 == device ":" {inside=1;next} inside && /^[^[:space:]]/ {exit}
      inside && /^  [A-Za-z0-9_.-]+:/ {key=$0;sub(/^  /,"",key);sub(/:.*/,"",key);print key}' | LC_ALL=C sort)"
  [ "$keys" = $'bind\nconnect\nlisten\ntype' ]
}

route_matches() {
  device_exists && orca_device_exact_shape &&
    [ "$(device_value type)" = proxy ] && [ "$(device_value bind)" = host ] &&
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

registration_host_name() {
  local name path="$SUBYARD_CONFIG_HOME/host-id"
  if [ -e "$path" ] || [ -L "$path" ]; then
    [ -f "$path" ] && [ ! -L "$path" ] || return 1
    name="$(cat -- "$path")" || return 1
  else
    name="$(hostname)" || return 1
    name="${name%.}"
  fi
  [[ "$name" =~ ^[a-zA-Z0-9._][a-zA-Z0-9._-]*$ ]] &&
    [ "$name" != . ] && [ "$name" != .. ] || return 1
  printf '%s\n' "$name"
}

render_registration_hook() {
  local version host_name mode="${1:-sync}"
  version="$(registration_contract_version)" || return 1
  host_name="$(registration_host_name)" || return 1
  cat <<SYNC_HEAD
#!/usr/bin/env bash
set -euo pipefail
exec 9<$ORCA_REGISTRATION_LOCK
flock -s 9
[ "\$(cat $ORCA_REGISTRATION_DIGEST 2>/dev/null)" = "$version" ] || {
  printf "Orca project helper is stale or incomplete; run 'yard init' first\n" >&2
  exit 1
}
systemctl is-active --quiet $ORCA_UNIT || exit 0
if [ -n "\${SUBYARD_PROJECT_HOOK_BINDING:-}" ]; then
  export SUBYARD_ORCA_REGISTRATION_SCOPE="\$SUBYARD_PROJECT_HOOK_BINDING"
fi
status=0
report="\$(/usr/bin/python3 -B $ORCA_REGISTRATION/main.py $mode --host-name '$host_name')" || status=\$?
if ! jq -e '(.ready | type == "boolean") and (.errors | type == "array") and (.warnings | type == "array")' <<<"\$report" >/dev/null; then
  printf 'Orca project registration failed; run yard orca status\n' >&2
  exit 1
fi
jq -r '(.errors[] | "Orca registration error: " + .), (.warnings[] | "Orca registration warning: " + .)' <<<"\$report" >&2
jq -r '"Orca checkouts registered: \(.registered)/\(.total)"' <<<"\$report"
jq -e '.ready' <<<"\$report" >/dev/null || status=1
[ "\$status" -ne 0 ] || /usr/bin/python3 -B $ORCA_REGISTRATION/settings.py
exit "\$status"
SYNC_HEAD
}

render_discovery_service() {
  cat <<UNIT
[Unit]
Description=Subyard Orca repository discovery portion
After=$ORCA_UNIT
PartOf=$ORCA_UNIT

[Service]
Type=oneshot
User=${DEV_USER:-dev}
Group=${DEV_USER:-dev}
Environment=HOME=/home/${DEV_USER:-dev}
ExecStart=$ORCA_REGISTRATION/discover
TimeoutStartSec=100
UMask=0077
Nice=10
UNIT
}

render_discovery_timer() {
  cat <<UNIT
[Unit]
Description=Periodically discover Subyard Orca repositories
PartOf=$ORCA_UNIT

[Timer]
OnBootSec=15s
OnUnitInactiveSec=15s
AccuracySec=1s
Unit=$ORCA_DISCOVERY_UNIT

[Install]
WantedBy=timers.target
UNIT
}

registration_desired_digest() {
  local version
  version="$(registration_contract_version)" || return 1
  {
    printf '755 0:0 '
    render_registration_hook | sha256sum | awk '{print $1}'
    local source
    for source in "$RESOURCE_DIR"/registration/*.py; do
      [ -f "$source" ] || return 1
      printf '644 0:0 %s ' "${source##*/}"
      sha256sum "$source" | awk '{print $1}'
    done
    printf '755 0:0 discover '
    render_registration_hook discover | sha256sum | awk '{print $1}'
    printf '644 0:0 discovery-service '
    render_discovery_service | sha256sum | awk '{print $1}'
    printf '644 0:0 discovery-timer '
    render_discovery_timer | sha256sum | awk '{print $1}'
    printf 'enabled discovery-timer\n'
    printf '644 0:0 lock\n'
    printf '644 0:0 marker %s\n' "$version"
  } | sha256sum | awk '{print $1}'
}

observe_registration_contract() {
  local desired
  local -a names=()
  desired="$(registration_desired_digest)" || die "Orca registration helper contract unavailable"
  mapfile -t names < <(cd "$RESOURCE_DIR/registration" && printf '%s\n' ./*.py | sed 's#^./##')
  yexec bash -se -- "$desired" "$ORCA_SYNC" "$ORCA_REGISTRATION" \
    "$ORCA_REGISTRATION_DIGEST" "$ORCA_REGISTRATION_LOCK" "$ORCA_EXEC" \
    "/etc/systemd/system/$ORCA_UNIT" "/etc/systemd/system/$ORCA_DISCOVERY_UNIT" \
    "/etc/systemd/system/$ORCA_DISCOVERY_TIMER" "${names[@]}" <<'YARD'
set -euo pipefail
desired="$1"; hook="$2"; directory="$3"; marker="$4"; lock="$5"; executable="$6"; unit="$7"
discovery_service="$8"; discovery_timer="$9"; shift 9
if [ ! -e "$executable" ] && [ ! -e "$unit" ] && [ ! -e "$hook" ] &&
  [ ! -e "$directory" ] && [ ! -e "$marker" ] && [ ! -e "$lock" ]; then
  printf '{"state":"absent","actual":"","desired":""}\n'
  exit 0
fi
actual="$({
  if [ -f "$hook" ]; then
    printf '%s %s:%s ' "$(stat -c %a "$hook")" "$(stat -c %u "$hook")" "$(stat -c %g "$hook")"
    sha256sum "$hook" | awk '{print $1}'
  else printf 'missing\n'; fi
  for name in "$@"; do
    if [ -f "$directory/$name" ]; then
      printf '%s %s:%s %s ' "$(stat -c %a "$directory/$name")" \
        "$(stat -c %u "$directory/$name")" "$(stat -c %g "$directory/$name")" "$name"
    else
      printf '%s ' "$name"
    fi
    if [ -f "$directory/$name" ]; then
      sha256sum "$directory/$name" | awk '{print $1}'
    else
      printf 'missing\n'
    fi
  done
  for installed in "$directory"/*.py; do
    [ -e "$installed" ] || continue
    case " $* " in
      *" ${installed##*/} "*) ;;
      *) printf 'unexpected %s\n' "${installed##*/}" ;;
    esac
  done
  for artifact in "$directory/discover" "$discovery_service" "$discovery_timer"; do
    case "$artifact" in
      "$directory/discover") label=discover ;;
      "$discovery_service") label=discovery-service ;;
      *) label=discovery-timer ;;
    esac
    if [ -f "$artifact" ]; then
      printf '%s %s:%s %s ' "$(stat -c %a "$artifact")" "$(stat -c %u "$artifact")" "$(stat -c %g "$artifact")" "$label"
      sha256sum "$artifact" | awk '{print $1}'
    else printf 'missing %s\n' "$label"; fi
  done
  if systemctl is-enabled --quiet "${discovery_timer##*/}"; then
    printf 'enabled discovery-timer\n'
  else printf 'disabled discovery-timer\n'; fi
  if [ -f "$lock" ]; then
    printf '%s %s:%s lock\n' "$(stat -c %a "$lock")" "$(stat -c %u "$lock")" "$(stat -c %g "$lock")"
  else
    printf 'missing lock\n'
  fi
  if [ -f "$marker" ]; then
    printf '%s %s:%s marker ' "$(stat -c %a "$marker")" \
      "$(stat -c %u "$marker")" "$(stat -c %g "$marker")"
    cat "$marker"
  else
    printf 'missing marker\n'
  fi
} | sha256sum | awk '{print $1}')"
state=stale
if [ "$actual" = "$desired" ]; then
  state=current
fi
printf '{"state":"%s","actual":"%s","desired":"%s"}\n' "$state" "$actual" "$desired"
YARD
}

stage_registration_contract() {
  ORCA_TMP_DIR="${ORCA_TMP_DIR:-$(mktemp -d)}"
  local sync="$ORCA_TMP_DIR/orca-sync"
  local source desired version
  render_registration_hook >"$sync"
  render_registration_hook discover >"$ORCA_TMP_DIR/discover"
  render_discovery_service >"$ORCA_TMP_DIR/$ORCA_DISCOVERY_UNIT"
  render_discovery_timer >"$ORCA_TMP_DIR/$ORCA_DISCOVERY_TIMER"
  chmod 0755 "$sync"
  desired="$(registration_desired_digest)" || die "Orca registration helper contract unavailable"
  version="$(registration_contract_version)" || die "Orca registration helper contract unavailable"
  [ -n "$ORCA_GUEST_TMP_DIR" ] || ORCA_GUEST_TMP_DIR="$(yexec mktemp -d /tmp/subyard-orca.XXXXXX)"
  valid_guest_tmp_dir "$ORCA_GUEST_TMP_DIR" \
    || die "Orca guest staging returned an unsafe temporary path"
  incus file push "$sync" "$YARD_INSTANCE_NAME$ORCA_GUEST_TMP_DIR/orca-sync" \
    "${PROJ[@]}" --mode 0755 >/dev/null
  for source in "$RESOURCE_DIR"/registration/*.py; do
    incus file push "$source" "$YARD_INSTANCE_NAME$ORCA_GUEST_TMP_DIR/${source##*/}" \
      "${PROJ[@]}" --mode 0644 >/dev/null
  done
  for source in "$ORCA_TMP_DIR/discover" "$ORCA_TMP_DIR/$ORCA_DISCOVERY_UNIT" "$ORCA_TMP_DIR/$ORCA_DISCOVERY_TIMER"; do
    incus file push "$source" "$YARD_INSTANCE_NAME$ORCA_GUEST_TMP_DIR/${source##*/}" "${PROJ[@]}" --mode 0644 >/dev/null
  done
  yexec bash -se -- "$ORCA_GUEST_TMP_DIR" "$ORCA_SYNC" "$ORCA_REGISTRATION" \
    "$ORCA_REGISTRATION_DIGEST" "$ORCA_REGISTRATION_LOCK" "$version" \
    "/etc/systemd/system/$ORCA_DISCOVERY_UNIT" "/etc/systemd/system/$ORCA_DISCOVERY_TIMER" <<'YARD'
set -euo pipefail
stage="$1"; hook="$2"; directory="$3"; marker="$4"; lock="$5"; version="$6"
discovery_service="$7"; discovery_timer="$8"
install -d -m 0755 "$(dirname "$lock")"
touch "$lock"
chmod 0644 "$lock"
chown 0:0 "$lock"
exec 9>"$lock"
flock -x 9
install -d -m 0755 "$directory" "$(dirname "$hook")" "$(dirname "$discovery_service")"
rm -f -- "$marker"
for installed in "$directory"/*.py; do
  [ -e "$installed" ] || continue
  [ -f "$stage/${installed##*/}" ] || rm -f -- "$installed"
done
for source in "$stage"/*.py; do install -m 0644 "$source" "$directory/${source##*/}"; done
install -m 0755 "$stage/orca-sync" "$hook"
install -m 0755 "$stage/discover" "$directory/discover"
install -m 0644 "$stage/${discovery_service##*/}" "$discovery_service"
install -m 0644 "$stage/${discovery_timer##*/}" "$discovery_timer"
temporary="$marker.$$"
printf '%s\n' "$version" >"$temporary"
chmod 0644 "$temporary"
mv "$temporary" "$marker"
YARD
  yexec systemctl daemon-reload
  yexec systemctl enable --now "$ORCA_DISCOVERY_TIMER" >/dev/null
}

stage_runtime_contract() {
  ORCA_TMP_DIR="${ORCA_TMP_DIR:-$(mktemp -d)}"
  local ingress="$ORCA_TMP_DIR/orca-ingress"
  local capture="$ORCA_TMP_DIR/orca-capture-ready"
  local sync="$ORCA_TMP_DIR/orca-sync"
  local codex_profile="$ORCA_TMP_DIR/orca-codex-profile"
  local unit="$ORCA_TMP_DIR/$ORCA_UNIT"
  local source contract_version
  local -a helpers=()
  mapfile -t helpers < <(registration_files)
  contract_version="$(registration_contract_version)" || die "Orca registration helper contract unavailable"
  ORCA_GUEST_TMP_DIR="$(yexec mktemp -d /tmp/subyard-orca.XXXXXX)"
  valid_guest_tmp_dir "$ORCA_GUEST_TMP_DIR" \
    || die "Orca guest staging returned an unsafe temporary path"
  local guest_ingress="$ORCA_GUEST_TMP_DIR/orca-ingress"
  local guest_capture="$ORCA_GUEST_TMP_DIR/orca-capture-ready"
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
      nft list chain inet "$table" input | grep -F "comment \"$marker\"" >/dev/null \
        || { printf 'refusing unowned nft table inet %s\n' "$table" >&2; exit 1; }
      nft delete table inet "$table"
    fi
    nft add table inet "$table"
    nft "add chain inet $table input { type filter hook input priority -10; policy accept; comment \"$marker\"; }"
    nft add rule inet "$table" input iifname != lo tcp dport "$port" reject
    ;;
  down)
    if nft list table inet "$table" >/dev/null 2>&1; then
      nft list chain inet "$table" input | grep -F "comment \"$marker\"" >/dev/null \
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
# Consume the request before starting Orca, including a failed startup. A later
# systemd retry or ordinary restart must use the normal Desktop pairing scope.
if [ -f "$ready.mobile-request" ]; then
  rm -- "$ready.mobile-request"
  set -- "$@" --mobile-pairing
fi
exec "$@" >"$ready"
CAPTURE
  # Orca's bash startup sources /etc/profile before injecting its agent command.
  # Remote clients can send their own stock YOLO argument, bypassing server defaults.
  # Keep the native CLI and SSH/VS Code shells unchanged; this is a launch default,
  # not a security boundary against explicit commands or in-session mode changes.
  # An existing function suppresses Orca's own wrapper; codex_launch.py also owns
  # its supported --no-daemon policy and managed-account hook preparation.
  cat >"$codex_profile" <<CODEX_PROFILE
if [ "\${SUBYARD_ORCA_CODEX_CONFIG:-}" = 1 ]; then
  codex() { /usr/bin/python3 -B $ORCA_REGISTRATION/codex_launch.py "\$@"; }
fi
CODEX_PROFILE
  render_registration_hook >"$sync"
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
Environment=SSH_AUTH_SOCK=/home/${DEV_USER:-dev}/.ssh/subyard-agent.sock
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
  incus file push "$codex_profile" "$YARD_INSTANCE_NAME$guest_codex_profile" \
    "${PROJ[@]}" --mode 0644 >/dev/null
  incus file push "$unit" "$YARD_INSTANCE_NAME$guest_unit" \
    "${PROJ[@]}" --mode 0644 >/dev/null
  stage_registration_contract
  if ! yexec cmp -s "$guest_ingress" "$ORCA_INGRESS" ||
    ! yexec cmp -s "$guest_capture" "$ORCA_CAPTURE" ||
    ! yexec cmp -s "$guest_codex_profile" "$ORCA_CODEX_PROFILE" ||
    ! yexec cmp -s "$guest_unit" "/etc/systemd/system/$ORCA_UNIT" ||
    ! ingress_active; then
    ORCA_RUNTIME_CHANGED=1
  fi
  yexec install -d -m 0755 "$(dirname "$ORCA_CAPTURE")" "$(dirname "$ORCA_SYNC")"
  yexec install -m 0755 "$guest_ingress" "$ORCA_INGRESS"
  yexec install -m 0755 "$guest_capture" "$ORCA_CAPTURE"
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
  local expected=""
  if [ -n "${SUBYARD_RESOURCE_STEPS:-}" ]; then
    if [ "$(jq -r '.[]|select(.id == "registration")|.decision' <<<"$SUBYARD_RESOURCE_STEPS")" = skip ]; then
      if [ "$(jq -r '.[]|select(.id == "codex-defaults")|.decision' <<<"$SUBYARD_RESOURCE_STEPS")" != skip ];then
        yexec runuser -u "${DEV_USER:-dev}" -- /usr/bin/python3 -B "$ORCA_REGISTRATION/settings.py"
      fi
      return
    fi
    expected="$(jq -r '.[]|select(.id == "registration")|.observed|select(startswith("catalog:"))|sub("^catalog:";"")|split(":scope:")[0]' <<<"$SUBYARD_RESOURCE_STEPS")"
    [ -n "$expected" ] || expected=conditional
  fi
  if [ -z "$expected" ];then yexec runuser -u "${DEV_USER:-dev}" -- "$ORCA_SYNC";return;fi
  yexec runuser -u "${DEV_USER:-dev}" -- env "SUBYARD_ORCA_REGISTRATION_SCOPE=$expected" "$ORCA_SYNC"
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
  local report host_name
  host_name="$(registration_host_name)" || return 1
  report="$(yexec runuser -u "${DEV_USER:-dev}" -- /usr/bin/python3 -B \
    "$ORCA_REGISTRATION/main.py" status --host-name "$host_name")" || true
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
    yexec test -x "$ORCA_SYNC" >/dev/null 2>&1 && registration_contract_ready
}

registration_contract_ready() {
  [ "$(observe_registration_contract | jq -r .state)" = current ]
}

service_contract_endpoint_ready() {
  yexec grep -Fqx \
    "ExecStart=$ORCA_CAPTURE $ORCA_READY $ORCA_EXEC serve --port $ORCA_GUEST_PORT --pairing-address $ORCA_ADVERTISE_HOST:$ORCA_HOST_PORT --json" \
    "/etc/systemd/system/$ORCA_UNIT" >/dev/null 2>&1
}

up_converged() {
  release_ready && dependencies_ready && runtime_contract_ready && service_enabled &&
    service_ready && ingress_active && route_matches && owner_endpoint_ready &&\
    automatic_project_hook_ready && discovery_scheduled && codex_defaults_ready && projects_synced
}

discovery_scheduled() {
  yexec systemctl is-enabled --quiet "$ORCA_DISCOVERY_TIMER" &&
    yexec systemctl is-active --quiet "$ORCA_DISCOVERY_TIMER"
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
  yexec systemctl start "$ORCA_DISCOVERY_TIMER"
  codex_defaults_ready || die "Orca Codex launch defaults did not converge"
  automatic_project_hook_ready && projects_synced || die "Orca project registration did not converge"
  ok "Orca ready through $ORCA_TRANSPORT at $ORCA_ADVERTISE_HOST:$ORCA_HOST_PORT"
}

require_pair_ready() {
  yexec systemctl is-active --quiet "$ORCA_UNIT" \
    || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
  require_runtime_settings
  resolve_owner_address
  registration_contract_ready && service_contract_endpoint_ready && route_matches && owner_endpoint_ready \
    || die "Orca endpoint settings are not applied; run '$(yard_cmd_hint) orca up' first"
  service_ready || die "Orca is not ready; run '$(yard_cmd_hint) orca up' first"
}

cmd_pair() {
  local scope=runtime
  require_pair_ready
  if [ "${1:-}" = --mobile ]; then
    scope=mobile
    # Record cleanup before the guest call: it may publish the request even if
    # transport fails. Atomic publication tags ownership without clobbering an
    # existing request, so cancellation only removes this operation's request.
    ORCA_PAIR_PENDING=1
    yexec runuser -u "${DEV_USER:-dev}" -- bash -c '
      set -euo pipefail
      umask 077
      temporary="$(mktemp "$1.XXXXXX")"
      trap '\''rm -f -- "$temporary"'\'' EXIT
      trap '\''exit 130'\'' INT
      trap '\''exit 143'\'' TERM
      printf "%s\n" "$2" > "$temporary"
      ln -T -- "$temporary" "$1"
    ' _ "$ORCA_MOBILE_REQUEST" "$SUBYARD_OPERATION_ID" \
      || die "Orca mobile pairing request could not be installed"
  fi
  yexec systemctl restart "$ORCA_UNIT"
  yexec systemctl start "$ORCA_DISCOVERY_TIMER"
  wait_service_ready || die "Orca did not become ready after restart"
  if [ "$ORCA_PAIR_PENDING" -eq 1 ]; then
    clear_mobile_request || die "Orca mobile pairing request could not be cleared"
  fi
  wait_owner_endpoint || die "Orca owner endpoint is not reachable after restart"
  run_project_sync
  projects_synced || die "Orca project registrations did not converge"
  yexec jq -er --arg scope "$scope" '
    select(.type == "orca_server_ready" and .schemaVersion == 1) |
    .pairing | select(.available == true and .scope == $scope) |
    .url | select(type == "string" and startswith("orca://pair?code="))
  ' "$ORCA_READY" || die "Orca did not publish a $scope pairing link"
}

cmd_sync() {
  yexec systemctl is-active --quiet "$ORCA_UNIT" \
    || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
  registration_contract_ready \
    || die "Orca project helper is stale or incomplete; run '$(yard_cmd_hint) init' first"
  run_project_sync
  projects_synced || die "Orca project registration did not converge"
  ok "Subyard roots and nested Git checkouts are registered in their Orca project groups"
}

cmd_restart() {
  yexec systemctl is-active --quiet "$ORCA_UNIT" \
    || die "Orca is not running; run '$(yard_cmd_hint) orca up' first"
  if [ -z "${SUBYARD_RESOURCE_STEPS:-}" ] || [ "$(jq -r '.[]|select(.id == "service")|.decision' <<<"$SUBYARD_RESOURCE_STEPS")" != skip ]; then
    yexec systemctl restart "$ORCA_UNIT"
  fi
  yexec systemctl start "$ORCA_DISCOVERY_TIMER"
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
  if discovery_scheduled; then ok "periodic repository discovery scheduled"; else warn "repository discovery timer inactive; run 'yard orca up'"; fi
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
      if jq -e '.discovery' <<<"$report" >/dev/null; then
        info "repository discovery: $(jq -r '.discovery.state + " (generation " + (.discovery.generation | tostring) + ")"' <<<"$report")"
        while IFS= read -r diagnostic; do
          warn "$diagnostic"
        done < <(jq -r '.discovery.errors[]' <<<"$report")
      fi
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
    stop_discovery
    ok "Orca already down"
    return 0
  fi
  stop_discovery
  remove_route
  yexec systemctl disable --now "$ORCA_UNIT" >/dev/null 2>&1 || true
  if yexec test -x "$ORCA_INGRESS"; then
    yexec "$ORCA_INGRESS" down
  fi
  ok "Orca stopped and unpublished; state preserved"
}

stop_discovery() {
  local unit
  for unit in "$ORCA_DISCOVERY_TIMER" "$ORCA_DISCOVERY_UNIT"; do
    if yexec systemctl is-active --quiet "$unit"; then
      yexec systemctl stop "$unit"
    fi
  done
}

orca_steps_emit() {
  local action="$1" steps="$2" binding
  binding="$( { jq -c '[.[]|{id,target,desired,preconditions,dependsOn,verify}]' <<<"$steps";
    registration_contract_version; sha256sum "$RESOURCE_DIR/handler.sh" "$ORCA_PROFILE_DIR/release.env" | awk '{print $1}';
    printf '%s\n' "${DEV_USER:-dev}" "$DEV_UID" "$ORCA_GUEST_PORT"; } | sha256sum | awk '{print $1}')"
  jq -cn --arg action "$action" --arg binding "$binding" --argjson steps "$steps" \
    '{schema:"yard.resource-action-assessment.v2",action:$action,binding:$binding,steps:$steps,
    changed:any($steps[];.decision != "skip"),consequences:[$steps[]|select(.decision != "skip")|.consequence]}'
}

orca_step_add() {
  local id="$1" target="$2" observed="$3" desired="$4" decision="$5" verify="$6" consequence="$7" dependencies="${8:-[]}"
  ORCA_STEPS="$(jq -cn --argjson steps "$ORCA_STEPS" --arg id "$id" --arg target "incus:$INCUS_PROJECT/$YARD_INSTANCE_NAME/$target" \
    --arg observed "$observed" --arg desired "$desired" --arg decision "$decision" --arg verify "$verify" --arg consequence "$consequence" --argjson dependencies "$dependencies" \
    '$steps + [{id:$id,target:$target,observed:$observed,desired:$desired,decision:$decision,
    preconditions:["only the declared profile-owned target scope; unavailable guest observations resolve after approved yard startup"],
    dependsOn:$dependencies,verify:$verify,consequence:$consequence}]')"
}

orca_registration_observation() {
  local report expected catalog
  report="$(project_registration_report)" || { printf 'unavailable'; return; }
  expected="$(jq -r '.scopeDigest // empty' <<<"$report")"
  [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die 'Orca native root-scope fingerprint unavailable; refresh the profile runtime'
  catalog="$(jq -r '.catalogDigest // empty' <<<"$report")"
  [[ "$catalog" =~ ^[0-9a-f]{64}$ ]] || die 'Orca native catalog fingerprint unavailable; refresh the profile runtime'
  if [ "${SUBYARD_RESOURCE_MODE:-}" = verify ] && [ -n "${SUBYARD_RESOURCE_STEPS:-}" ]; then
    local captured
    captured="$(jq -r '.[]|select(.id == "registration")|.observed|select(startswith("catalog:"))|split(":scope:")[1] // empty' <<<"$SUBYARD_RESOURCE_STEPS")"
    [ -z "$captured" ] || [ "$captured" = "$expected" ] || die 'Orca native verified-root scope changed after apply'
  fi
  if jq -e '.ready' <<<"$report" >/dev/null; then printf 'registered'; else printf 'catalog:%s:scope:%s' "$catalog" "$expected"; fi
}

emit_up_assessment() {
  local available=false id predicate target desired observed decision report dependencies
  require_runtime_settings
  resolve_owner_address
  refuse_port_collision
  ORCA_STEPS='[]'
  if incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1 &&
    [ "$(incus list "$YARD_INSTANCE_NAME" "${PROJ[@]}" -f csv -c s 2>/dev/null)" = RUNNING ]; then
    available=true
    select_release
  fi
  # Incus exec forwards stdin; keep the plan rows separate from predicate input.
  while IFS='|' read -r id predicate target desired <&3; do
    observed=unknown; decision=conditional
    if [ "$available" = true ]; then
      observed=unready; decision=apply
      if "$predicate"; then observed="$desired"; decision=skip; fi
      if [ "$id" = host-route ] && [ "$decision" = apply ];then
        observed=absent
        if device_exists;then observed="device:$(orca_device_fingerprint)";fi
      fi
    fi
    dependencies='[]'
    if [ "$id" = service ];then
      dependencies='["package","runtime-contract"]'
      if jq -e 'any(.[]; (.id == "package" or .id == "runtime-contract") and .decision != "skip")' <<<"$ORCA_STEPS" >/dev/null;then
        observed=unknown;decision=conditional
      fi
    fi
    orca_step_add "$id" "$target" "$observed" "$desired" "$decision" "native $id postcondition equals the approved desired fact" "converge the declared Orca $id target" "$dependencies"
  done 3<<STEPS
package|release_ready|package/orca-ide|pinned-$ORCA_VERSION
headless-dependencies|dependencies_ready|packages/orca-headless|declared-headless-package-set-installed
runtime-contract|runtime_contract_ready|profile/orca-runtime|profile-helper-source-and-service-contract-current
service-enable|service_enabled|$ORCA_UNIT/enabled|enabled
service|service_ready|$ORCA_UNIT/readiness|running-and-native-ready
host-route|route_matches|$ORCA_DEVICE|tcp:$ORCA_OWNER_IP:$ORCA_HOST_PORT-to-loopback:$ORCA_GUEST_PORT
ingress|ingress_active|inet/subyard_orca|managed-ingress-active
owner-endpoint|owner_endpoint_ready|endpoint/$ORCA_OWNER_IP:$ORCA_HOST_PORT|reachable
project-hook|automatic_project_hook_ready|profile/project-dispatcher|current-hook-and-helper-contract
discovery|discovery_scheduled|$ORCA_DISCOVERY_TIMER|enabled-and-active
codex-defaults|codex_defaults_ready|settings/codex-defaults|custom-arguments-retained-or-stock-default-disabled
STEPS
  observed=unknown; decision=conditional
  if [ "$available" = true ] && registration_contract_ready && service_endpoint_ready; then
    observed="$(orca_registration_observation)"; decision=apply
    if [ "$observed" = registered ]; then decision=skip; fi
    if [ "$observed" = unavailable ]; then observed=unknown; decision=conditional; fi
  fi
  orca_step_add registration workspace-driver/srv/workspaces "$observed" registered "$decision" \
    'native verified local roots registered in their project groups; native saved-tab retention and removal checks succeed' \
    'reconcile only the captured native verified-root catalog, or the bounded catalog revealed by approved startup'
  orca_steps_emit up "$ORCA_STEPS"
}

emit_restart_assessment() {
  local invocation previous desired observed decision
  svc_require_yard_running
  yexec systemctl is-active --quiet "$ORCA_UNIT" || die 'Orca is not running'
  invocation="$(yexec systemctl show -p InvocationID --value "$ORCA_UNIT")"
  [[ "$invocation" =~ ^[0-9a-f]{32}$ ]] || die 'Orca native service invocation identity unavailable'
  previous="$invocation"
  if [ -n "${SUBYARD_RESOURCE_STEPS:-}" ]; then
    previous="$(jq -r '.[]|select(.id == "service")|.desired|sub("^running-after:";"")' <<<"$SUBYARD_RESOURCE_STEPS")"
    [[ "$previous" =~ ^[0-9a-f]{32}$ ]] || die 'Orca approved restart identity invalid'
  fi
  desired="running-after:$previous"; observed="invocation:$invocation"; decision=apply
  if [ "$invocation" != "$previous" ] && service_endpoint_ready; then observed="$desired"; decision=skip; fi
  ORCA_STEPS='[]'
  orca_step_add service "$ORCA_UNIT" "$observed" "$desired" "$decision" 'new native service invocation is running and its loopback endpoint is ready' 'restart the captured Orca service invocation'
  observed=inactive;decision=apply
  if yexec systemctl is-active --quiet "$ORCA_DISCOVERY_TIMER";then observed=active;decision=skip;fi
  orca_step_add discovery "$ORCA_DISCOVERY_TIMER" "$observed" active "$decision" 'native discovery timer is active' 'start the selected Orca discovery timer'
  orca_steps_emit restart "$ORCA_STEPS"
}

emit_sync_assessment() {
  local observed decision
  svc_require_yard_running
  yexec systemctl is-active --quiet "$ORCA_UNIT" || die 'Orca is not running'
  registration_contract_ready || die "Orca project helper is stale or incomplete; run '$(yard_cmd_hint) init' first"
  ORCA_STEPS='[]'
  observed=unready;decision=apply
  if codex_defaults_ready;then observed=custom-arguments-retained-or-stock-default-disabled;decision=skip;fi
  orca_step_add codex-defaults settings/codex-defaults "$observed" custom-arguments-retained-or-stock-default-disabled "$decision" \
    'native Codex custom arguments are retained or stock default disabled' 'converge the bounded Orca Codex launch default'
  observed="$(orca_registration_observation)";decision=apply
  [ "$observed" != unavailable ] || die 'Orca native root catalog is unavailable'
  if [ "$observed" = registered ];then
    if [ "${SUBYARD_RESOURCE_MODE:-}" = verify ];then decision=skip;else
      observed="$(project_registration_report | jq -r '"catalog:" + .catalogDigest + ":scope:" + .scopeDigest')"
    fi
  fi
  orca_step_add registration workspace-driver/srv/workspaces "$observed" registered "$decision" \
    'captured native verified-root catalog converged with saved-tab retention' 'reconcile the captured native verified-root catalog'
  orca_steps_emit sync "$ORCA_STEPS"
}

orca_native_guard() {
  [ -n "${SUBYARD_RESOURCE_STEPS:-}" ] || return 0
  local fresh
  fresh="$("emit_${1}_assessment")"
  [ "$(jq -r .binding <<<"$fresh")" = "${SUBYARD_RESOURCE_BINDING:-}" ] || die 'plan_stale: Orca native desired scope changed'
  jq -en --argjson approved "$SUBYARD_RESOURCE_STEPS" --argjson current "$(jq -c .steps <<<"$fresh")" '
    ($approved|length) == ($current|length) and all(range(0; $approved|length); . as $i |
    $approved[$i] as $a | $current[$i] as $c | $a.id == $c.id and $a.target == $c.target and $a.desired == $c.desired and
    (($c.decision == "skip") or ($a.decision == "conditional") or ($a.decision == "apply" and $a.observed == $c.observed)))' >/dev/null \
    || die 'plan_stale: Orca native captured observation changed'
  SUBYARD_RESOURCE_STEPS="$(jq -c .steps <<<"$fresh")"
}

emit_down_assessment() {
  local unit observed steps='[]' route=absent binding
  for unit in "$ORCA_UNIT" "$ORCA_DISCOVERY_TIMER" "$ORCA_DISCOVERY_UNIT"; do
    observed=stopped
    if yexec systemctl is-active --quiet "$unit"; then observed=running; fi
    steps="$(jq -cn --argjson steps "$steps" --arg unit "$unit" --arg target "incus:$INCUS_PROJECT/$YARD_INSTANCE_NAME/$unit" --arg observed "$observed" \
      '$steps + [{id:($unit|gsub("[.]";"-")),target:$target,observed:$observed,desired:"stopped",
      decision:(if $observed == "stopped" then "skip" else "apply" end),preconditions:["selected profile-owned unit"],verify:"native systemd unit is inactive",
      consequence:("stop the captured Orca unit " + $unit + " while retaining its persistent data")}]')"
  done
  observed=absent
  if ingress_active; then observed=present; fi
  steps="$(jq -cn --argjson steps "$steps" --arg target "incus:$INCUS_PROJECT/$YARD_INSTANCE_NAME/inet/subyard_orca" --arg observed "$observed" \
    '$steps + [{id:"ingress",target:$target,observed:$observed,desired:"absent",
    decision:(if $observed == "absent" then "skip" else "apply" end),preconditions:["profile-owned nft table only"],verify:"managed ingress marker is absent",
    consequence:"remove the captured Orca ingress table"}]')"
  if device_exists; then
    route="sha256:$(orca_device_fingerprint)"
  fi
  steps="$(jq -cn --argjson steps "$steps" --arg target "incus:$INCUS_PROJECT/$YARD_INSTANCE_NAME/$ORCA_DEVICE" --arg observed "$route" \
    '$steps + [{id:"route",target:$target,observed:$observed,desired:"absent",
    decision:(if $observed == "absent" then "skip" else "apply" end),preconditions:["captured selected proxy device metadata unchanged"],verify:"selected proxy device is absent",
    consequence:"remove the captured Orca proxy device"}]')"
  binding="$(printf '%s\n' "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" "$ORCA_UNIT" "$ORCA_DEVICE" | sha256sum | awk '{print $1}')"
  jq -cn --argjson steps "$steps" --arg binding "$binding" \
    '{schema:"yard.resource-action-assessment.v2",action:"down",binding:$binding,
    changed:any($steps[];.decision == "apply"),consequences:[$steps[]|select(.decision != "skip")|.consequence],steps:$steps}'
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
  if [ "$verb" = pair ]; then
    case "$#" in
      0) return 0 ;;
      1) [ "$1" = --mobile ] || svc_usage_error "'pair' accepts only '--mobile'" ;;
      *) svc_usage_error "'pair' accepts only one optional '--mobile'" ;;
    esac
    return 0
  fi
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
    up) emit_up_assessment ;;
    pair)
      svc_require_yard_running
      require_pair_ready
      emit_resource_assessment pair true \
        "briefly restart the Orca service, preserving existing client grants and server state" \
        "reconcile project groups and checkouts and issue one ${1:+mobile }single-client pairing link"
      ;;
    restart) emit_restart_assessment ;;
    sync) emit_sync_assessment ;;
    down)
      svc_require_yard_running
      emit_down_assessment
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

if [ "$sub" = _runtime-contract ]; then
  case "${1:-}" in
    observe)
      [ "$#" -eq 1 ] || die "usage: _runtime-contract observe"
      observation="$(observe_registration_contract)"
      hook_binding=""
      if [ "$(jq -r .state <<<"$observation")" = current ] &&
        [ "$(incus list "$YARD_INSTANCE_NAME" "${PROJ[@]}" -f csv -c s 2>/dev/null)" = RUNNING ] &&
        yexec systemctl is-active --quiet "$ORCA_UNIT";then
        hook_binding="$(project_registration_report | jq -r '.catalogDigest // empty')"
        [[ "$hook_binding" =~ ^[0-9a-f]{64}$ ]] || die 'Orca native hook catalog fingerprint unavailable'
      fi
      jq --arg binding "$hook_binding" '. + {hook_binding:$binding}' <<<"$observation"
      ;;
    apply)
      [ "$#" -eq 4 ] \
        || die "usage: _runtime-contract apply <operation-id> <expected-actual> <expected-desired>"
      [ -n "${2:-}" ] || die "runtime contract apply requires an operation ID"
      SUBYARD_OPERATION_ID="$2"
      observation="$(observe_registration_contract)"
      [ "$(jq -r .actual <<<"$observation")" = "$3" ] &&
        [ "$(jq -r .desired <<<"$observation")" = "$4" ] \
        || die "Orca runtime contract changed since assessment; retry yard init"
      case "$(jq -r .state <<<"$observation")" in
        absent) printf '%s\n' "$observation" ;;
        current) printf '%s\n' "$observation" ;;
        stale)
          stage_registration_contract
          cleanup_guest || die "Orca guest staging directory could not be removed"
          observe_registration_contract | jq -e 'select(.state == "current")'
          ;;
        *) die "invalid Orca runtime contract observation" ;;
      esac
      ;;
    *) die "usage: _runtime-contract observe | apply <operation-id> <expected-actual> <expected-desired>" ;;
  esac
  exit
fi

case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare|prepare-start)
    if [ "${SUBYARD_RESOURCE_MODE:-}" = prepare-start ] && [ "$sub" != up ];then die 'Orca startup scope must use up';fi
    [ -n "$sub" ] || svc_usage_error "resource verb is required"
    prepare_resource "$sub" "$@"
    ;;
  verify)
    case "$sub" in
      up) validate_resource_arguments up "$@"; emit_up_assessment; exit ;;
      restart) validate_resource_arguments restart "$@"; emit_restart_assessment; exit ;;
      sync) validate_resource_arguments sync "$@"; emit_sync_assessment; exit ;;
      down) ;;
      *) die "unsupported Orca native verifier" ;;
    esac
    validate_resource_arguments "$sub" "$@"
    svc_require_yard_running
    emit_down_assessment
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
      up) orca_native_guard up; cmd_up ;;
      status) cmd_status ;;
      pair) cmd_pair "$@" ;;
      restart) orca_native_guard restart; cmd_restart ;;
      sync) orca_native_guard sync; cmd_sync ;;
      logs) cmd_logs "$@" ;;
      down)
        if [ -n "${SUBYARD_RESOURCE_STEPS:-}" ]; then
          fresh="$(emit_down_assessment)"
          [ "$(jq -r .binding <<<"$fresh")" = "${SUBYARD_RESOURCE_BINDING:-}" ] \
            || die "plan_stale: Orca down target binding changed"
          jq -en --argjson approved "$SUBYARD_RESOURCE_STEPS" --argjson current "$(jq -c .steps <<<"$fresh")" '
            ($approved|length) == ($current|length) and
            all(range(0; $approved|length); . as $i |
              $approved[$i] as $a | $current[$i] as $c |
              $a.id == $c.id and $a.target == $c.target and $a.desired == $c.desired and
              (($c.decision == "skip") or ($a.decision == "apply" and $a.observed == $c.observed)))' >/dev/null \
            || die "plan_stale: Orca down native scope changed"
        fi
        cmd_down
        ;;
    esac
    ;;
  '')
    case "$sub" in
      is-up) require_no_resource_arguments is-up "$@"; cmd_is_up ;;
      -h|--help|help|"")
        printf 'Usage: %s orca <up|is-up|status|pair|restart|sync|logs|down>\n' "${PROG:-yard}"
        printf '  pair [--mobile]  Issue a Desktop or mobile link after a brief service restart\n'
        ;;
      *) die "typed resource dispatcher required for 'yard orca $sub'" ;;
    esac
    ;;
  *) die "unknown resource execution mode '${SUBYARD_RESOURCE_MODE:-}'" ;;
esac

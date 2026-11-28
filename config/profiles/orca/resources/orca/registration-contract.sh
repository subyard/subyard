# shellcheck shell=bash
# Orca installs its own modules and the shared checkout discovery library into one
# directory. Sources are listed in installed-name order.
registration_sources() {
  local library="$RESOURCE_DIR/../../../../../scripts/lib/checkout-discovery" source
  for source in "$RESOURCE_DIR"/registration/*.py "$library/discovery.py" "$library/inventory.py"; do
    [ -f "$source" ] || return 1
    printf '%s\t%s\n' "${source##*/}" "$source"
  done | LC_ALL=C sort | cut -f 2
}

# Process substitution drops registration_sources' status; callers check the set.
registration_sources_complete() {
  local library="$RESOURCE_DIR/../../../../../scripts/lib/checkout-discovery"
  [ -f "$library/discovery.py" ] && [ -f "$library/inventory.py" ] &&
    compgen -G "$RESOURCE_DIR/registration/*.py" >/dev/null
}

registration_source() {
  local source
  while IFS= read -r source; do
    if [ "${source##*/}" = "$1.py" ]; then printf '%s\n' "$source"; return 0; fi
  done < <(registration_sources)
  return 1
}

registration_names() {
  registration_sources | sed 's#^.*/##'
}

registration_contract_version() {
  local digest source
  local -a sources=()
  mapfile -t sources < <(registration_sources)
  registration_sources_complete || return 1
  digest="$(for source in "${sources[@]}"; do
    printf '%s  ./%s\n' "$(sha256sum < "$source" | cut -d ' ' -f 1)" "${source##*/}"
  done | sha256sum | cut -d ' ' -f 1)" || return 1
  printf '%s:%s\n' "$ORCA_CONTRACT_VERSION" "$digest"
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
  local version host_name mode="${1:-sync}" launch_command=':' reset_command=':'
  version="$(registration_contract_version)" || return 1
  host_name="$(registration_host_name)" || return 1
  if [ "$mode" != discover ]; then
    launch_command="/usr/bin/python3 -B $ORCA_REGISTRATION/settings.py --launch-defaults --deadline \"\$launch_deadline\""
    reset_command="/usr/bin/python3 -B $ORCA_REGISTRATION/settings.py --reset-chats --deadline \"\$deadline\""
  fi
  cat <<SYNC_HEAD
#!/usr/bin/env bash
set -euo pipefail
# One budget below the dispatcher's 120 seconds: lock wait up to 10 s, required launch
# defaults until 40 s, registration and cleanup until 95 s, the resumable chat reset until 110 s.
read -r launch_deadline registration_deadline deadline < <(/usr/bin/python3 -c 'import time; now = time.monotonic(); print(now + 40, now + 95, now + 110)')
exec 9<$ORCA_REGISTRATION_LOCK
flock -s -w 10 9 || {
  printf "Orca project helper is busy; run yard orca status\n" >&2
  exit 1
}
[ "\$(cat $ORCA_REGISTRATION_DIGEST 2>/dev/null)" = "$version" ] || {
  printf "Orca project helper is stale or incomplete; run 'yard init' first\n" >&2
  exit 1
}
systemctl is-active --quiet $ORCA_UNIT || exit 0
if [ -n "\${SUBYARD_PROJECT_HOOK_BINDING:-}" ]; then
  export SUBYARD_ORCA_REGISTRATION_SCOPE="\$SUBYARD_PROJECT_HOOK_BINDING"
fi
launch_status=0
$launch_command || launch_status=1
# main.py exits with strict readiness; the hook succeeds from the validated settlement.
report="\$(/usr/bin/python3 -B $ORCA_REGISTRATION/main.py $mode --host-name '$host_name' --deadline "\$registration_deadline")" || true
if ! jq -e '(.ready | type == "boolean") and (.settled | type == "boolean") and (.errors | type == "array") and (.warnings | type == "array")' <<<"\$report" >/dev/null; then
  printf 'Orca project registration failed; run yard orca status\n' >&2
  exit 1
fi
jq -r '(.errors[] | "Orca registration error: " + .), (.warnings[] | "Orca registration warning: " + .)' <<<"\$report" >&2
jq -r '.cleanup[]? | "Orca cleanup pending: " + .path + ": " + .reason + "; " + .nextAction' <<<"\$report" >&2
jq -r '"Orca checkouts registered: \(.registered)/\(.total)"' <<<"\$report"
status=0
jq -e '.settled' <<<"\$report" >/dev/null || status=1
[ "\$status" -ne 0 ] || [ "\$launch_status" -ne 0 ] || $reset_command
[ "\$launch_status" -eq 0 ] || status=1
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

render_codex_profile() {
  cat <<CODEX_PROFILE
if [ "\${SUBYARD_ORCA_CODEX_CONFIG:-}" = 1 ]; then
  export PATH=${ORCA_CODEX_BIN%/*}:\$PATH
  codex() { /usr/bin/python3 -B $ORCA_REGISTRATION/codex_launch.py "\$@"; }
fi
CODEX_PROFILE
}

render_codex_bin() {
  cat <<CODEX_BIN
#!/bin/sh
exec /usr/bin/python3 -B $ORCA_REGISTRATION/codex_appserver.py /usr/local/bin/codex "\$@"
CODEX_BIN
}

registration_desired_digest() {
  local version
  version="$(registration_contract_version)" || return 1
  {
    printf '755 0:0 '
    render_registration_hook | sha256sum | awk '{print $1}'
    local source
    local -a sources=()
    mapfile -t sources < <(registration_sources)
    registration_sources_complete || return 1
    for source in "${sources[@]}"; do
      printf '644 0:0 %s ' "${source##*/}"
      sha256sum "$source" | awk '{print $1}'
    done
    printf '755 0:0 discover '
    render_registration_hook discover | sha256sum | awk '{print $1}'
    printf '644 0:0 discovery-service '
    render_discovery_service | sha256sum | awk '{print $1}'
    printf '644 0:0 discovery-timer '
    render_discovery_timer | sha256sum | awk '{print $1}'
    printf '644 0:0 codex-profile '
    render_codex_profile | sha256sum | awk '{print $1}'
    printf '755 0:0 codex-bin '
    render_codex_bin | sha256sum | awk '{print $1}'
    printf 'enabled discovery-timer\n'
    printf '644 0:0 lock\n'
    printf '644 0:0 marker %s\n' "$version"
  } | sha256sum | awk '{print $1}'
}


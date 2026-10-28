# shellcheck shell=bash
registration_contract_version() {
  local digest
  digest="$(cd "$RESOURCE_DIR/registration" && sha256sum ./*.py | sha256sum | cut -d ' ' -f 1)" || return 1
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
  local version host_name mode="${1:-sync}" settings_command=':'
  version="$(registration_contract_version)" || return 1
  host_name="$(registration_host_name)" || return 1
  if [ "$mode" != discover ]; then
    settings_command="/usr/bin/python3 -B $ORCA_REGISTRATION/settings.py"
  fi
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
jq -r '.cleanup[]? | "Orca cleanup pending: " + .path + ": " + .reason + "; " + .nextAction' <<<"\$report" >&2
jq -r '"Orca checkouts registered: \(.registered)/\(.total)"' <<<"\$report"
jq -e '.ready' <<<"\$report" >/dev/null || status=1
[ "\$status" -ne 0 ] || $settings_command
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
    printf '644 0:0 codex-profile '
    render_codex_profile | sha256sum | awk '{print $1}'
    printf '755 0:0 codex-bin '
    render_codex_bin | sha256sum | awk '{print $1}'
    printf 'enabled discovery-timer\n'
    printf '644 0:0 lock\n'
    printf '644 0:0 marker %s\n' "$version"
  } | sha256sum | awk '{print $1}'
}


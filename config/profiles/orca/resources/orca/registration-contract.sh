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

# Print "unselected" only for valid guest runtime facts of a completed refresh
# whose selection omits orca. The facts and their directories must be owned
# like the guest root and not writable by others; anything else prints nothing.
# Arguments: a fixed argv marker, then the guest root. Contains no single quote.
render_selection_check() {
  cat <<'SELECTION'
import json, os, re, stat, sys
root = os.path.abspath(sys.argv[2])
facts = os.path.join(root, "etc/subyard/guest-runtime.json")
names = re.compile("[A-Za-z0-9][A-Za-z0-9_.-]{0,127}")
def unique(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate field")
        value[key] = item
    return value
def snapshot(value):
    if not isinstance(value, dict) or set(value) != {"version", "yard", "selected_profiles"}:
        raise ValueError("snapshot")
    yard, profiles = value["yard"], value["selected_profiles"]
    if (not isinstance(value["version"], str) or not re.fullmatch("[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}", value["version"]) or
            not isinstance(yard, dict) or set(yard) != {"name", "project", "instance"} or
            any(not isinstance(item, str) or not names.fullmatch(item) for item in yard.values()) or
            not isinstance(profiles, list) or
            any(not isinstance(item, str) or not re.fullmatch("[a-z0-9][a-z0-9_-]*", item) for item in profiles) or
            len(set(profiles)) != len(profiles)):
        raise ValueError("snapshot")
    return value
top = os.lstat(root)
owner = (top.st_uid, top.st_gid)
directory = os.path.dirname(facts)
while True:
    info = os.lstat(directory)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != owner[0] or info.st_mode & 0o022:
        sys.exit(1)
    if directory == root:
        break
    directory = os.path.dirname(directory)
descriptor = os.open(facts, os.O_RDONLY | os.O_NOFOLLOW)
info = os.fstat(descriptor)
if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o644 or (info.st_uid, info.st_gid) != owner or
        info.st_nlink != 1 or info.st_size > 16384):
    sys.exit(1)
with os.fdopen(descriptor, "rb") as stream:
    value = json.loads(stream.read(16385), object_pairs_hook=unique)
with open(os.path.join(root, "etc/machine-id")) as stream:
    machine = stream.read().strip()
if (not isinstance(value, dict) or set(value) != {"schema", "machine_id", "status", "target", "last_success", "capabilities", "readiness"} or
        type(value["schema"]) is not int or value["schema"] != 1 or not re.fullmatch("[0-9a-f]{32}", machine) or
        value["machine_id"] != machine or value["status"] != "current" or
        value["capabilities"] != "unknown" or value["readiness"] != "unknown" or
        snapshot(value["target"]) != snapshot(value["last_success"])):
    sys.exit(1)
if "orca" not in value["target"]["selected_profiles"]:
    print("unselected")
SELECTION
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
# A yard that does not select Orca keeps an installed Orca but has nothing to
# register. Only valid guest facts that omit orca skip the hook.
selection="\$(/usr/bin/python3 -I -B -c '$(render_selection_check)' subyard-orca-selection $ORCA_GUEST_ROOT 2>/dev/null)" || selection=
[ "\$selection" != unselected ] || exit 0
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


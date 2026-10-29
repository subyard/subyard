#!/usr/bin/env bash
# Physical convergence of the selected GitHub profile and its owner-side user service.
set -euo pipefail
PROFILE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$PROFILE_DIR/../../.." && pwd)"
SCRIPT_DIR="$root/scripts"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"

mode=apply
case "${1:-}" in --check) mode=check ;; --remove) mode=remove ;; --pause) mode=pause ;; --resume) mode=resume ;; --yes|'') ;; *) die 'invalid GitHub broker lifecycle argument' ;; esac
yard="${SUBYARD_YARD:-default}"
case "$yard" in ''|*[!a-zA-Z0-9_-]*) die 'invalid GitHub broker yard' ;; esac
unit="subyard-github-$yard.service"
unit_dir="$SUBYARD_OPERATOR_HOME/.config/systemd/user"
unit_file="$unit_dir/$unit"
boot_dir="$unit_dir/default.target.wants"
boot_link="$boot_dir/$unit"
state_dir="$SUBYARD_HOME/github-broker"
runtime_file="$state_dir/$yard.json"
broker_engine="$state_dir/$yard-engine"
app_config="$SUBYARD_CONFIG_HOME/github-app.json"
engine="$PROFILE_DIR/bin/broker"
[ -x "$engine" ] || engine="$root/.build/profiles/github/bin/broker"
profile_dir="$PROFILE_DIR"
guest_engine=/usr/local/libexec/subyard/github-client
marker='# Managed by Subyard GitHub broker'
enabled="${SUBYARD_PROFILE_SELECTED:-0}"
[ "$mode" != remove ] || enabled=0

operator="${SUBYARD_USER:-$(id -un)}"
operator_uid="$(id -u "$operator")"
user_systemctl() {
  local runtime_dir="/run/user/$operator_uid"
  if [ "$(id -u)" = "$operator_uid" ]; then
    XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="unix:path=$runtime_dir/bus" systemctl --user "$@"
  else
    runuser -u "$operator" -- env XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="unix:path=$runtime_dir/bus" systemctl --user "$@"
  fi
}
unit_quote() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g'; }
render_runtime() {
  jq -n --arg app "$app_config" --argjson port "$SSH_PORT" --arg dev "$DEV_USER" \
    --arg key "${SUBYARD_KEYS_CONSUMER_ROOT:-$SUBYARD_CONFIG_HOME/generated}/github/connection.json" \
    --arg identity "$SUBYARD_HOME/ssh/id_ed25519" --arg known "$SUBYARD_HOME/ssh/known_hosts" \
    '{app_config:$app,key_file:$key,ssh_port:$port,developer:$dev,identity_file:$identity,known_hosts_file:$known}'
}
render_unit() {
  printf '%s\n' "$marker"
  printf '# engine: %s\n' "$(sha256sum "$engine" | cut -d' ' -f1)"
  cat <<UNIT
[Unit]
Description=Subyard GitHub token broker ($yard)
StartLimitIntervalSec=0
[Service]
ExecStart="$(unit_quote "$broker_engine")" _github-broker "$(unit_quote "$runtime_file")"
Restart=always
RestartSec=5
TimeoutStopSec=10
UMask=0077
LimitCORE=0
MemoryMax=128M
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
[Install]
WantedBy=default.target
UNIT
}
owned_service() {
  [ ! -L "$unit_file" ] && [ -f "$unit_file" ] && [ "$(head -n 1 "$unit_file")" = "$marker" ]
}
boot_registration_current() {
  # is-enabled reads persistent registration; it does not prove the loaded graph.
  [ -L "$boot_link" ] || { boot_reason="boot-link-missing"; return 1; }
  [ "$(readlink -- "$boot_link")" = "$unit_file" ] || { boot_reason="boot-link-target-mismatch"; return 1; }
  [ "$(user_systemctl show "$unit" --property=FragmentPath --value)" = "$unit_file" ] || { boot_reason="unit-fragment-mismatch"; return 1; }
  local wants
  boot_reason="boot-dependency-unloaded"
  wants="$(user_systemctl show default.target --property=Wants --value)" || return 1
  case " $wants " in *" $unit "*) ;; *) return 1 ;; esac
}
check_drift() {
  printf 'subyard-profile-readiness-v1 %s\n' "$1" >&2
  exit 1
}
# Normal stop temporarily disconnects only our transport before checking user sessions.
if [ "$mode" = pause ] || [ "$mode" = resume ]; then
  owned_service || exit 0
  if [ "$mode" = resume ]; then
    user_systemctl start "$unit"
  else
    state="$(user_systemctl show "$unit" --property=ActiveState --value)"
    case "$state" in
      active|activating|reloading)
        user_systemctl stop "$unit"
        printf 'paused\n'
        ;;
    esac
  fi
  exit 0
fi
guest_hook() {
  tar -C "$profile_dir" -cf - provision.sh git_wiring.py SKILL.md skills.json bin/subyard-github bin/git-remote-subyard-github \
    -C "$root/config/skills" --transform='s,^deliver.py$,agent-skills.py,' deliver.py | incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" \
    --env DEV_USER="$DEV_USER" --env GITHUB_GIT_MODE="${GITHUB_GIT_MODE:-off}" -- bash -euo pipefail -c '
      bundle="$(mktemp -d /tmp/subyard-github.XXXXXX)"
      trap '\''rm -rf -- "$bundle"'\'' EXIT
      tar -xf - -C "$bundle"
      bash "$bundle/provision.sh" "$@"
    ' subyard "$@"
}

guest_status() {
  incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" \
    -- runuser -u "$DEV_USER" -- /usr/local/bin/subyard-github status "$@"
}
guest_runtime_current() {
  local wanted
  wanted="$(sha256sum "$engine" | cut -d' ' -f1)"
  guest_status --runtime | jq -e --arg wanted "$wanted" \
    '.protocol == 1 and .broker_sha256 == $wanted and .client_sha256 == $wanted' >/dev/null
}

if [ "$enabled" != 1 ]; then
  if [ "$mode" = check ]; then
    [ ! -e "$unit_file" ] && [ ! -L "$unit_file" ] && [ ! -e "$runtime_file" ] && [ ! -e "$broker_engine" ] || exit 1
    if incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- getent passwd "$DEV_USER" >/dev/null 2>&1; then
      guest_hook --check-removed >/dev/null || exit 1
    fi
    exit 0
  fi
  if [ "$mode" = remove ]; then
    for artifact in "$unit_file" "$runtime_file" "$broker_engine"; do
      python3 "$SCRIPT_DIR/lib/teardown-plan.py" guard-artifact "$artifact" \
        || exit 75
    done
  fi
  if [ -e "$unit_file" ] || [ -L "$unit_file" ]; then
    owned_service || die 'refusing to remove unmanaged GitHub broker service'
    # Keep the manager's link to an absolute-path unit until it has stopped.
    user_systemctl stop "$unit"
    if [ "$mode" = remove ]; then
      python3 "$SCRIPT_DIR/lib/teardown-plan.py" guard-artifact "$unit_file" \
        || exit 75
    fi
    user_systemctl disable "$unit" >/dev/null
    if [ "$mode" = remove ]; then
      python3 "$SCRIPT_DIR/lib/teardown-plan.py" remove-artifact "$unit_file" \
        || exit 75
    else
      rm -- "$unit_file"
    fi
    user_systemctl daemon-reload
  fi
  # The host authorization is already gone; guest cleanup needs a running instance.
  if incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- true 2>/dev/null; then
    if incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- getent passwd "$DEV_USER" >/dev/null 2>&1; then
      guest_hook --remove
    fi
    incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- rm -f -- "$guest_engine"
  fi
  if [ "$mode" = remove ]; then
    python3 "$SCRIPT_DIR/lib/teardown-plan.py" remove-artifact "$runtime_file" \
      || exit 75
  else
    [ ! -L "$runtime_file" ] || die 'unsafe GitHub broker runtime file'
    [ ! -e "$runtime_file" ] || rm -- "$runtime_file"
  fi
  if [ "$mode" = remove ]; then
    python3 "$SCRIPT_DIR/lib/teardown-plan.py" remove-artifact "$broker_engine" \
      || exit 75
  else
    [ ! -L "$broker_engine" ] || die 'unsafe GitHub broker engine path'
    [ ! -e "$broker_engine" ] || rm -- "$broker_engine"
  fi
  exit
fi

[ -x "$engine" ] && [ -f "$engine" ] || die 'native GitHub broker engine is missing'
[ ! -L "$state_dir" ] && [ ! -L "$broker_engine" ] && [ ! -L "$runtime_file" ] && [ ! -L "$unit_file" ] || die 'unsafe GitHub broker managed path'
[ ! -L "$unit_dir" ] && [ ! -L "$boot_dir" ] || die 'unsafe GitHub broker registration path'
if [ -e "$unit_file" ]; then owned_service || die 'refusing to overwrite unmanaged GitHub broker service'; fi

host_payload_current() {
  [ -f "$runtime_file" ] && cmp -s <(render_runtime) "$runtime_file" || return 1
  owned_service && cmp -s <(render_unit) "$unit_file" || return 1
  cmp -s "$engine" "$broker_engine"
}
guest_payload_current() {
  local wanted actual
  wanted="$(sha256sum "$engine" | cut -d' ' -f1)"
  actual="$(incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- sha256sum "$guest_engine" 2>/dev/null | cut -d' ' -f1)" || return 1
  [ "$actual" = "$wanted" ] || return 1
  guest_hook --check >/dev/null
}
if [ "$mode" = check ]; then
  host_payload_current || exit 1
  boot_registration_current || check_drift "$boot_reason"
  user_systemctl is-enabled --quiet "$unit" 2>/dev/null || exit 1
  [ "$(loginctl show-user "$operator" -p Linger --value 2>/dev/null)" = yes ] || exit 1
  [ "${SUBYARD_PROFILE_STOPPED:-0}" != 1 ] || exit 0
  user_systemctl is-active --quiet "$unit" 2>/dev/null || check_drift unit-inactive
  guest_payload_current || exit 1
  guest_runtime_current >/dev/null 2>&1 || check_drift transport-unavailable
  exit
fi

if [ -e "$boot_link" ] || [ -L "$boot_link" ]; then
  [ -L "$boot_link" ] && [ "$(readlink -- "$boot_link")" = "$unit_file" ] \
    || die 'refusing to overwrite unmanaged GitHub broker boot link'
fi

# Static skill repair leaves healthy engines and the user manager untouched.
# Only an existing successful provider registration can be refreshed here.
payload_current=0
if [ "$mode" = apply ] && [ "${SUBYARD_PROFILE_STOPPED:-0}" != 1 ]; then
  guest_hook --refresh-skills
  if "$PROFILE_DIR/owner.sh" --check >/dev/null 2>&1; then
    exit 0
  fi
  if host_payload_current && guest_payload_current; then payload_current=1; fi
fi

if [ "$payload_current" = 0 ]; then
  install -d -m 0700 "$state_dir" "$unit_dir"
  install -m 0700 "$engine" "$broker_engine.tmp"
  mv -f -- "$broker_engine.tmp" "$broker_engine"
  render_runtime > "$runtime_file.tmp"
  chmod 0600 "$runtime_file.tmp"
  mv -f -- "$runtime_file.tmp" "$runtime_file"
  # Stream the engine through stdin, not through a guest-writable destination path on the host.
  incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- sh -eu -c '
    install -d -m 0755 /usr/local/libexec/subyard
    temp="$(mktemp /usr/local/libexec/subyard/.github-client.XXXXXX)"
    trap '\''rm -f -- "$temp"'\'' EXIT
    cat > "$temp"
    chmod 0755 "$temp"
    mv -f -- "$temp" /usr/local/libexec/subyard/github-client
  ' < "$engine"
  guest_hook
  render_unit > "$unit_file.tmp"
  chmod 0644 "$unit_file.tmp"
  mv -f -- "$unit_file.tmp" "$unit_file"
fi
if [ "$(loginctl show-user "$operator" -p Linger --value 2>/dev/null || true)" != yes ]; then
  host_sudo loginctl enable-linger "$operator"
fi
if ! user_systemctl show-environment >/dev/null 2>&1; then
  host_sudo systemctl start "user@$operator_uid.service"
fi
user_systemctl daemon-reload
# The caller's HOME may differ from the running user manager's unit search path.
user_systemctl enable "$unit_file" >/dev/null
boot_registration_current || die 'GitHub broker boot registration did not converge'
if [ "$payload_current" = 0 ] || ! user_systemctl is-active --quiet "$unit" \
  || ! guest_runtime_current >/dev/null 2>&1; then
  user_systemctl restart "$unit"
fi
ready=0
for _ in {1..30}; do
  if guest_runtime_current >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || die "GitHub broker did not connect; inspect systemctl --user status $unit"
guest_hook --register-skills
printf 'GitHub broker profile configured; App connection stays on the owner host\n'

#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
MODE="${1:-normal}"
CHECKPOINT="$HOME/.cache/subyard-github-e2e-checkpoint.json"
STATE=''
MARKER=''
PROJECT=''
INSTANCE=''
DEV_UID=''
DEV_GID=''
UNIT='subyard-github-default.service'
UNIT_FILE=''
SSH_PORT=''
PROJECT_CLAIMED=0
HERMES_YARD=''
HERMES_PROJECT=''
HERMES_INSTANCE=''
HERMES_UID=''
HERMES_GID=''
HERMES_UNIT=''
HERMES_UNIT_FILE=''
HERMES_PROJECT_CLAIMED=0

die() { printf 'github-broker-e2e: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

controller_main() {
  # EXIT also runs after errexit has unwound this function's local scope.
  slot='' target_alias=e2e-vm-1 bundle='' bundle_hash='' before_boot='' after_boot=''
  boot_down=0 boot_ready=0 prepared=0 cleanup_failed=0 rc=0 system_rc=0
  system_state='' power_snapshot='' power_started='' power_ready=0
  wait_value='' wait_number='' run_hermes=0
  usage() { printf 'Usage: dev/e2e/github-broker.sh --slot N [--wait N|Ns|Nm] [--hermes]\n' >&2; return 2; }
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --slot)
        if [ "$#" -lt 2 ] || [ -n "$slot" ]; then usage; return 2; fi
        slot="$2"
        shift 2
        ;;
      --wait)
        [ "$#" -ge 2 ] || { usage; return 2; }
        wait_value="$2"
        shift 2
        ;;
      --hermes) run_hermes=1; shift ;;
      *) usage; return 2 ;;
    esac
  done
  [ -n "$slot" ] || { usage; return 2; }
  # shellcheck source=dev/agent-e2e.sh
  . "$ROOT/dev/agent-e2e.sh"
  set_requested_slot "$slot" --slot
  # shellcheck disable=SC2034
  LEASE_PURPOSE='github-broker'
  if [ -n "$wait_value" ]; then
    case "$wait_value" in
      *m|*s)
        wait_number="${wait_value%[ms]}"
        [[ "$wait_number" =~ ^[0-9]+$ ]] || die '--wait must be seconds or end in s/m'
        WAIT_SECONDS="$wait_number"
        [ "${wait_value: -1}" = m ] && WAIT_SECONDS=$((WAIT_SECONDS * 60))
        ;;
      *) WAIT_SECONDS="$wait_value" ;;
    esac
    [[ "$WAIT_SECONDS" =~ ^[0-9]+$ ]] || die '--wait must be seconds or end in s/m'
  fi
  LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-github-e2e.XXXXXX")"
  controller_cleanup() {
    rc=$?
    local existing_directory
    trap - EXIT INT TERM
    set +e
    if [ "$prepared" = 1 ] && [ -n "${bundle:-}" ] && [ -n "${bundle_hash:-}" ]; then
      existing_directory="${GUEST_DIRS[1]:-}"
      if [ -n "$existing_directory" ]; then
        cleanup_guest 1 >/dev/null 2>&1 || cleanup_failed=1
      fi
      if [ "$cleanup_failed" = 0 ]; then
        run_guest 1 "$bundle" "$bundle_hash" bash dev/e2e/github-broker.sh --cleanup \
          >/dev/null 2>&1 || cleanup_failed=1
        cleanup_guest 1 >/dev/null 2>&1 || cleanup_failed=1
      fi
    fi
    for cleanup_vm in "${!GUEST_DIRS[@]}"; do
      cleanup_guest "$cleanup_vm" >/dev/null 2>&1 || cleanup_failed=1
    done
    if [ -n "${LEASE_KEEPER_PID:-}" ]; then
      kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      LEASE_KEEPER_PID=''
    fi
    release_lease >/dev/null 2>&1 || cleanup_failed=1
    case "${LOCAL_TEMP:-}" in
      /tmp/subyard-github-e2e.*|"${TMPDIR:-/tmp}"/subyard-github-e2e.*)
        find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_failed=1 ;;
    esac
    [ "$cleanup_failed" = 0 ] || rc=3
    exit "$rc"
  }
  trap controller_cleanup EXIT INT TERM
  acquire_lease
  start_lease_keeper
  bundle="$LOCAL_TEMP/worktree.tar.gz"
  info 'packing current public worktree'
  build_bundle "$ROOT" "$bundle"
  bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
  ok "worktree bundle ready (sha256=$bundle_hash)"
  info 'removing any prior marked GitHub broker checkpoint'
  run_guest 1 "$bundle" "$bundle_hash" bash dev/e2e/github-broker.sh --cleanup
  cleanup_guest 1
  run_guest 1 "$bundle" "$bundle_hash" bash dev/e2e/github-broker.sh --prepare-reboot
  prepared=1
  cleanup_guest 1
  before_boot="$(timeout --foreground 15 ssh -F "$CLIENT_CONFIG" -T \
    -o ConnectTimeout=3 -o ConnectionAttempts=1 "$target_alias" -- \
    cat /proc/sys/kernel/random/boot_id)" || die 'could not read owner boot ID'
  info 'rebooting the retained owner VM'
  timeout --foreground 20 ssh -F "$CLIENT_CONFIG" -T \
    -o ConnectTimeout=3 -o ConnectionAttempts=1 -o ServerAliveInterval=2 \
    -o ServerAliveCountMax=2 "$target_alias" -- sudo -n systemctl reboot \
    </dev/null >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    if ! timeout --foreground 5 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=2 -o ConnectionAttempts=1 "$target_alias" -- true \
      >/dev/null 2>&1; then boot_down=1; break; fi
    sleep 1
  done
  [ "$boot_down" = 1 ] || die 'owner VM never went down for reboot'
  for _ in $(seq 1 120); do
    after_boot="$(timeout --foreground 5 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=2 -o ConnectionAttempts=1 "$target_alias" -- \
      cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)"
    [ -n "$after_boot" ] && [ "$after_boot" != "$before_boot" ] && { boot_ready=1; break; }
    sleep 1
  done
  [ "$boot_ready" = 1 ] || die 'owner VM did not acquire a new boot ID'
  for _ in $(seq 1 120); do
    system_rc=0
    system_state="$(timeout --foreground 5 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=2 -o ConnectionAttempts=1 "$target_alias" -- \
      systemctl is-system-running 2>/dev/null)" || system_rc=$?
    case "$system_state:$system_rc" in
      running:0|degraded:1) break ;;
      initializing:1|starting:1|*:255|*:124) ;;
      *) die "owner VM returned in unexpected system state: ${system_state:-unknown}" ;;
    esac
    sleep 1
  done
  case "$system_state:$system_rc" in running:0|degraded:1) ;; *) die 'owner VM systemd did not become ready' ;; esac
  for _ in $(seq 1 120); do
    power_snapshot="$(timeout --foreground 5 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=2 -o ConnectionAttempts=1 "$target_alias" -- \
      systemctl show subyard-power-reconcile.service \
      -p LoadState -p ActiveState -p SubState -p Result -p ExecMainStatus \
      -p ExecMainStartTimestampMonotonic 2>/dev/null || true)"
    power_started="$(sed -n 's/^ExecMainStartTimestampMonotonic=//p' <<<"$power_snapshot")"
    if [[ "$power_started" =~ ^[1-9][0-9]*$ ]] \
      && grep -Fxq 'ActiveState=inactive' <<<"$power_snapshot" \
      && grep -Fxq 'SubState=dead' <<<"$power_snapshot" \
      && grep -Fxq 'Result=success' <<<"$power_snapshot" \
      && grep -Fxq 'ExecMainStatus=0' <<<"$power_snapshot"; then
      power_ready=1
      break
    fi
    grep -Fxq 'ActiveState=failed' <<<"$power_snapshot" \
      && die 'owner power reconciler failed after reboot'
    grep -Eq '^LoadState=(not-found|error|bad-setting|masked)$' <<<"$power_snapshot" \
      && die 'owner power reconciler is unavailable'
    sleep 1
  done
  [ "$power_ready" = 1 ] || die 'owner power reconciler did not finish after reboot'
  run_guest 1 "$bundle" "$bundle_hash" bash dev/e2e/github-broker.sh --verify-reboot
  cleanup_guest 1
  prepared=0
  if [ "$run_hermes" = 1 ]; then
    run_guest 1 "$bundle" "$bundle_hash" bash dev/e2e/hermes-profile.sh
    cleanup_guest 1
  fi
  ok 'GitHub broker reboot lifecycle passed on one retained lease'
  exit 0
}

case "${SUBYARD_E2E_VM:-}" in
  1|2) ;;
  *) controller_main "$@"; exit ;;
esac

case "$MODE" in
  normal|--prepare-reboot|--verify-reboot|--cleanup) ;;
  *) die "unknown mode: $MODE" ;;
esac
if [ "$MODE" = --cleanup ] && [ ! -e "$CHECKPOINT" ] && [ ! -L "$CHECKPOINT" ]; then
  exit 0
fi
for command in grep jq openssl sha256sum sed ss sudo systemctl timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() { "$ROOT/.build/yard" "$@"; }
systemctl_user() {
  local runtime_dir
  runtime_dir="/run/user/$(id -u)"
  XDG_RUNTIME_DIR="$runtime_dir" DBUS_SESSION_BUS_ADDRESS="unix:path=$runtime_dir/bus" \
    systemctl --user "$@"
}
setting() {
  local output
  output="$(yard config show "$1")"
  printf '%s\n' "$output" | sed -n 's/^effective: //p'
}
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
guest_dev() {
  incus exec "$INSTANCE" --project "$PROJECT" --user "$DEV_UID" --group "$DEV_GID" \
    --env HOME=/home/dev --env USER=dev --env LOGNAME=dev -- "$@"
}
hermes_yard() { "$ROOT/.build/yard" -Y "$HERMES_YARD" "$@"; }
hermes_setting() {
  local output
  output="$(hermes_yard config show "$1")"
  printf '%s\n' "$output" | sed -n 's/^effective: //p'
}
hermes_guest() { incus exec "$HERMES_INSTANCE" --project "$HERMES_PROJECT" -- "$@"; }
hermes_dev() {
  incus exec "$HERMES_INSTANCE" --project "$HERMES_PROJECT" \
    --user "$HERMES_UID" --group "$HERMES_GID" \
    --env HOME=/home/dev --env USER=dev --env LOGNAME=dev -- "$@"
}
next_port() {
  local candidate="$1"
  while ss -Hln "sport = :$candidate" 2>/dev/null | grep -q .; do candidate=$((candidate + 1)); done
  printf '%s\n' "$candidate"
}
save_checkpoint() {
  local temporary
  [ ! -e "$CHECKPOINT" ] && [ ! -L "$CHECKPOINT" ] \
    || die "refusing to overwrite reboot checkpoint: $CHECKPOINT"
  install -d -m 0700 "$(dirname "$CHECKPOINT")"
  temporary="$CHECKPOINT.tmp.$$"
  jq -n --arg state "$STATE" --arg project "$PROJECT" --arg instance "$INSTANCE" \
    --arg port "$SSH_PORT" --arg boot_id "$(cat /proc/sys/kernel/random/boot_id)" \
    '{STATE:$state,PROJECT:$project,INSTANCE:$instance,SSH_PORT:$port,boot_id:$boot_id}' \
    > "$temporary"
  chmod 0600 "$temporary"
  mv -n -- "$temporary" "$CHECKPOINT"
  [ -f "$CHECKPOINT" ] || die 'could not create reboot checkpoint'
}
load_checkpoint() {
  local old_boot current_boot keys
  [ -f "$CHECKPOINT" ] && [ ! -L "$CHECKPOINT" ] \
    || die "reboot checkpoint is missing: $CHECKPOINT"
  [ "$(stat -c '%a:%u' "$CHECKPOINT")" = "600:$(id -u)" ] \
    || die 'reboot checkpoint must be owner-readable mode 0600'
  keys="$(jq -er 'type == "object" and (keys | sort) == ["INSTANCE","PROJECT","SSH_PORT","STATE","boot_id"]' "$CHECKPOINT")" \
    || die 'reboot checkpoint has an unexpected schema'
  [ "$keys" = true ] || die 'reboot checkpoint has an unexpected schema'
  STATE="$(jq -er '.STATE | strings' "$CHECKPOINT")" || die 'checkpoint STATE is invalid'
  PROJECT="$(jq -er '.PROJECT | strings' "$CHECKPOINT")" || die 'checkpoint PROJECT is invalid'
  INSTANCE="$(jq -er '.INSTANCE | strings' "$CHECKPOINT")" || die 'checkpoint INSTANCE is invalid'
  SSH_PORT="$(jq -er '.SSH_PORT | strings' "$CHECKPOINT")" || die 'checkpoint SSH_PORT is invalid'
  old_boot="$(jq -er '.boot_id | strings' "$CHECKPOINT")" || die 'checkpoint boot_id is invalid'
  [[ "$STATE" =~ ^/var/tmp/subyard-github-broker\.[A-Za-z0-9]+$ ]] || die 'checkpoint STATE is outside fixture scope'
  [ "$PROJECT" = "github-e2e-$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')" ] || die 'checkpoint PROJECT is outside fixture scope'
  [ "$INSTANCE" = github-e2e ] || die 'checkpoint INSTANCE is outside fixture scope'
  [[ "$SSH_PORT" =~ ^[0-9]+$ ]] || die 'checkpoint SSH_PORT is invalid'
  if [ "$MODE" = --cleanup ] && [ ! -e "$STATE" ] && [ ! -L "$STATE" ] \
    && incus info >/dev/null 2>&1 && ! incus project show "$PROJECT" >/dev/null 2>&1; then
    UNIT_FILE="$HOME/.config/systemd/user/$UNIT"
    cleanup_unit || die 'could not remove the retired fixture service'
    rm -- "$CHECKPOINT"
    exit 0
  fi
  [ -d "$STATE" ] && [ ! -L "$STATE" ] && [ ! -L "$STATE/.marker" ] \
    || die 'checkpoint state directory is unsafe'
  MARKER="subyard-github-broker-e2e-v1-$(basename "$STATE")"
  if [ -f "$STATE/.marker" ]; then
    [ "$(cat "$STATE/.marker")" = "$MARKER" ] || die 'checkpoint state marker is invalid'
  elif [ "$MODE" = --cleanup ] \
    && ! incus project show "$PROJECT" >/dev/null 2>&1 \
    && [ "$(incus storage get default source 2>/dev/null || true)" = "$STATE/storage" ]; then
    : # An interrupted legacy cleanup left only the retained storage pool.
  else
    die 'checkpoint state marker is missing'
  fi
  if [ "$MODE" = --verify-reboot ]; then
    current_boot="$(cat /proc/sys/kernel/random/boot_id)"
    [ "$old_boot" != "$current_boot" ] || die 'reboot checkpoint boot ID has not changed'
  fi
  PROJECT_CLAIMED=1
}
wait_active() {
  local _=0
  for _ in $(seq 1 120); do systemctl_user is-active --quiet "$UNIT" && return 0; sleep 1; done
  systemctl_user status "$UNIT" --no-pager >&2 || true
  die 'owner GitHub broker user service did not become active'
}
status() { guest_dev /usr/local/bin/subyard-github status 2>/dev/null | tr -d '\n'; }
wait_status() {
  local expected="$1" output='' _=0
  for _ in $(seq 1 120); do
    output="$(status || true)"
    [ "$output" = "$expected" ] && return 0
    sleep 1
  done
  printf '%s\n' "$output" >&2
  die "guest GitHub status did not converge to $expected"
}
write_config() {
  local explicit=''
  [ "$#" -eq 0 ] || explicit="$1"
  {
    printf '# %s\nSSH_PORT=%s\nCODING_TOOL_INTEGRATIONS=\n' "$MARKER" "$SSH_PORT"
    [ "$explicit" = disable ] && printf 'ENVIRONMENT_PROFILES=\n'
    printf 'HOST_BASE=%s/host\nHOST_MOUNTS=\nHOST_LINKS=\n' "$STATE"
    printf 'RESTRICTED_DISK_PATHS=%s/host\nFORWARD_SSH_AGENT=0\n' "$STATE"
    printf 'HOST_CLAUDE_MD=\nHOST_CODEX_AGENTS_MD=\nHOST_OPENCODE_AGENTS_MD=\n'
  } > "$SUBYARD_CONFIG_HOME/config.env.tmp"
  chmod 0600 "$SUBYARD_CONFIG_HOME/config.env.tmp"
  mv -f -- "$SUBYARD_CONFIG_HOME/config.env.tmp" "$SUBYARD_CONFIG_HOME/config.env"
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/default"
  printf 'INCUS_PROJECT=%s\nYARD_INSTANCE_NAME=%s\n' "$PROJECT" "$INSTANCE" \
    > "$SUBYARD_CONFIG_HOME/yards/default/config.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/default/config.env"
}
assert_wiring() {
  local path
  for path in /home/dev/.claude/skills/subyard-github /home/dev/.codex/skills/subyard-github \
    /home/dev/.config/opencode/skills/subyard-github /home/dev/.pi/agent/skills/subyard-github \
    /home/dev/.hermes/skills/subyard-github; do
    guest test -L "$path" || die "missing GitHub skill discovery link: $path"
    [ "$(guest readlink "$path")" = /usr/local/share/subyard/skills/subyard-github ] \
      || die "GitHub skill link points elsewhere: $path"
    guest test -r "$path/SKILL.md" || die "unreadable GitHub skill: $path"
  done
  guest test -x /usr/local/bin/subyard-github || die 'guest GitHub command is missing'
  guest test -x /usr/local/libexec/subyard/github-client || die 'guest GitHub client is missing'
  guest sh -c '[ ! -e /etc/subyard/github.gitconfig ] && [ ! -L /etc/subyard/github.gitconfig ]' \
    || die 'legacy persistent GitHub helper config was installed'
  if guest_dev git config --show-origin --get-all credential.https://github.com.helper \
    >/dev/null 2>&1; then
    die 'persistent GitHub credential helper was installed'
  fi
  guest_dev /usr/local/bin/subyard-github --help | grep -Fq 'Git HTTPS authorization' \
    || die 'GitHub helper contract is missing from the client'
}
assert_removed() {
  local path
  for path in /usr/local/bin/subyard-github /usr/local/libexec/subyard/github-client \
    /usr/local/share/subyard/skills/subyard-github /etc/subyard/github.gitconfig \
    /home/dev/.claude/skills/subyard-github /home/dev/.codex/skills/subyard-github \
    /home/dev/.config/opencode/skills/subyard-github /home/dev/.pi/agent/skills/subyard-github \
    /home/dev/.hermes/skills/subyard-github; do
    guest sh -c '[ ! -e "$1" ] && [ ! -L "$1" ]' _ "$path" || die "GitHub artifact survived disable: $path"
  done
  if guest_dev git config --show-origin --get-all credential.https://github.com.helper >/dev/null 2>&1; then
    die 'GitHub HTTPS helper survived explicit disable'
  fi
}
cleanup_unit_file() {
  local unit="$1" unit_file="$2"
  if [ -e "$unit_file" ] || [ -L "$unit_file" ]; then
    [ -f "$unit_file" ] && [ ! -L "$unit_file" ] \
      && [ "$(head -n 1 "$unit_file")" = '# Managed by Subyard GitHub broker' ] || return 1
    grep -Fq "$STATE/data/github-broker/" "$unit_file" || return 1
    systemctl_user disable --now "$unit" >/dev/null 2>&1 || true
    rm -f -- "$unit_file"
    systemctl_user daemon-reload >/dev/null 2>&1 || true
  fi
  [ ! -e "$unit_file" ] && [ ! -L "$unit_file" ]
}
cleanup_unit() {
  cleanup_unit_file "$UNIT" "$UNIT_FILE"
}
cleanup_hermes() {
  local project_marker='' instance_marker=''
  if [ -n "$HERMES_PROJECT" ] && incus project show "$HERMES_PROJECT" >/dev/null 2>&1; then
    project_marker="$(incus project get "$HERMES_PROJECT" user.subyard.e2e 2>/dev/null || true)"
    instance_marker="$(incus config get "$HERMES_INSTANCE" user.subyard.e2e \
      --project "$HERMES_PROJECT" 2>/dev/null || true)"
    if { [ "$project_marker" = "$MARKER" ] && [ "$instance_marker" = "$MARKER" ]; } \
      || { [ "$HERMES_PROJECT_CLAIMED" = 1 ] && [ -z "$project_marker" ] \
        && [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; }; then
      hermes_yard teardown --yes >/dev/null 2>&1 || return 1
    else
      printf 'github-broker-e2e: refusing to remove unmarked Hermes project %s\n' \
        "$HERMES_PROJECT" >&2
      return 1
    fi
  fi
  cleanup_unit_file "$HERMES_UNIT" "$HERMES_UNIT_FILE"
}
cleanup() {
  local rc=$? state_cleaned=0
  trap - EXIT INT TERM
  set +e
  if [ "$rc" -ne 0 ] && [ -n "$UNIT_FILE" ] && [ -f "$UNIT_FILE" ]; then
    systemctl_user show "$UNIT" -p ActiveState -p Result -p ExecMainStatus >&2
    journalctl --user -u "$UNIT" --no-pager -n 8 >&2
  fi
  [ -z "$HERMES_PROJECT" ] || cleanup_hermes || rc=3
  if [ -n "$PROJECT" ] && incus project show "$PROJECT" >/dev/null 2>&1; then
    project_marker="$(incus project get "$PROJECT" user.subyard.e2e 2>/dev/null || true)"
    instance_marker="$(incus config get "$INSTANCE" user.subyard.e2e --project "$PROJECT" 2>/dev/null || true)"
    if [ "$project_marker" = "$MARKER" ] && [ "$instance_marker" = "$MARKER" ] \
      || { [ "$PROJECT_CLAIMED" = 1 ] && [ -z "$project_marker" ] && [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; }; then
      yard teardown --yes >/dev/null 2>&1 || rc=3
    else
      printf 'github-broker-e2e: refusing to remove unmarked project %s\n' "$PROJECT" >&2
      rc=3
    fi
  fi
  cleanup_unit || rc=3
  if [ "$rc" != 3 ] && [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-github-broker.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; then
    if [ "$(incus storage get default source 2>/dev/null || true)" = "$STATE/storage" ]; then
      # A retained Incus pool from an older fixture must outlive its test state.
      for child in "$STATE"/*; do
        [ "$child" != "$STATE/storage" ] || continue
        [ -e "$child" ] || [ -L "$child" ] || continue
        sudo -n find "$child" -depth -delete || rc=3
      done
    else
      sudo -n find "$STATE" -depth -delete || rc=3
    fi
    [ "$rc" = 3 ] || state_cleaned=1
  fi
  if { [ "$MODE" = --verify-reboot ] || [ "$MODE" = --cleanup ]; } && [ "$rc" != 3 ] \
    && { [ "$state_cleaned" = 1 ] || { [ "$MODE" = --cleanup ] && [ "$rc" = 0 ]; }; } \
    && [ -f "$CHECKPOINT" ] && [ "$(stat -c '%a' "$CHECKPOINT")" = 600 ] \
    && [ "$(jq -r '.STATE' "$CHECKPOINT" 2>/dev/null || true)" = "$STATE" ]; then
    rm -f -- "$CHECKPOINT"
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

if [ "$MODE" = --prepare-reboot ] \
  && { [ -e "$CHECKPOINT" ] || [ -L "$CHECKPOINT" ]; }; then
  die "refusing to overwrite reboot checkpoint: $CHECKPOINT"
fi
if [ "$MODE" = --verify-reboot ] || [ "$MODE" = --cleanup ]; then
  load_checkpoint
else
  STATE="$(mktemp -d /var/tmp/subyard-github-broker.XXXXXX)"
  chmod 0711 "$STATE"
  MARKER="subyard-github-broker-e2e-v1-$(basename "$STATE")"
  printf '%s\n' "$MARKER" > "$STATE/.marker"
  chmod 0600 "$STATE/.marker"
fi
export STATE MARKER
[ -x "$ROOT/.build/yard" ] || { command -v go >/dev/null 2>&1 || die 'Go is required'; "$ROOT/dev/build-engine.sh" --force; }
incus info >/dev/null 2>&1 || die 'Incus owner API is unavailable on the disposable VM'
if [ "$MODE" = --verify-reboot ] || [ "$MODE" = --cleanup ]; then
  if incus project show "$PROJECT" >/dev/null 2>&1 || [ "$MODE" = --verify-reboot ]; then
    [ "$(incus project get "$PROJECT" user.subyard.e2e 2>/dev/null || true)" = "$MARKER" ] \
      || die 'checkpoint project marker does not match this fixture'
    [ "$(incus config get "$INSTANCE" user.subyard.e2e --project "$PROJECT" 2>/dev/null || true)" = "$MARKER" ] \
      || die 'checkpoint instance marker does not match this fixture'
  fi
fi
if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ] \
  && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin \
  && ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin; then
  printf -v reexec 'exec env SUBYARD_E2E_VM=%q bash %q %q' \
    "$SUBYARD_E2E_VM" "$ROOT/dev/e2e/github-broker.sh" "$MODE"
  exec sg incus-admin -c "$reexec"
fi

export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export SUBYARD_KEYS_CONSUMER_ROOT="$STATE/key-consumers"
export STORAGE_PATH
STORAGE_PATH="$(incus storage get default source 2>/dev/null || true)"
[ -n "$STORAGE_PATH" ] || STORAGE_PATH="$HOME/.cache/subyard-github-e2e-platform/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export HOST_CLAUDE_MD='' HOST_CODEX_AGENTS_MD='' HOST_OPENCODE_AGENTS_MD=''
unset ENVIRONMENT_PROFILES
install -d -m 0700 "$SUBYARD_CONFIG_HOME"
if [ "$MODE" != --verify-reboot ] && [ "$MODE" != --cleanup ]; then
  SSH_PORT="$(next_port "$((36000 + ($$ % 12000)))")"
  PROJECT="github-e2e-$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
  INSTANCE="github-e2e"
  if incus project show "$PROJECT" >/dev/null 2>&1; then die 'fixture project already exists'; fi
  PROJECT_CLAIMED=1
  write_config
fi
UNIT_FILE="$SUBYARD_OPERATOR_HOME/.config/systemd/user/$UNIT"
[ "$MODE" = --verify-reboot ] || [ "$MODE" = --cleanup ] || {
  [ ! -e "$UNIT_FILE" ] && [ ! -L "$UNIT_FILE" ] || die "fixture service path exists: $UNIT_FILE"
}

if [ "$MODE" = --verify-reboot ]; then
  info 'restoring the retained default yard after owner reboot'
  guest_ready=0
  for _ in $(seq 1 120); do
    if guest true >/dev/null 2>&1; then guest_ready=1; break; fi
    sleep 1
  done
  [ "$guest_ready" = 1 ] || die 'automatic boot recovery did not restore the retained yard'
  DEV_UID="$(guest id -u dev)"
  DEV_GID="$(guest id -g dev)"
  [[ "$DEV_UID" =~ ^[0-9]+$ && "$DEV_GID" =~ ^[0-9]+$ ]] || die 'invalid guest dev identity'
  wait_active
  wait_status '{"configured":true}'
  ok 'rebooted owner restored configured broker status'
elif [ "$MODE" = --cleanup ]; then
  info 'disabling and removing the marked default broker fixture'
  if incus project show "$PROJECT" >/dev/null 2>&1; then
    write_config disable
    yard init --yes
    DEV_UID="$(guest id -u dev)"
    DEV_GID="$(guest id -g dev)"
    [ ! -e "$UNIT_FILE" ] && [ ! -L "$UNIT_FILE" ] || die 'cleanup left owner broker service'
    assert_removed
  fi
  ok 'marked default broker fixture disabled'
  exit 0
else
  info 'initializing default yard with implicit GitHub profile selection'
  yard init --yes
  [ "$(setting INCUS_PROJECT)" = "$PROJECT" ] || die 'unexpected project identity'
  [ "$(setting YARD_INSTANCE_NAME)" = "$INSTANCE" ] || die 'unexpected instance identity'
  incus project set "$PROJECT" user.subyard.e2e="$MARKER"
  incus config set "$INSTANCE" user.subyard.e2e="$MARKER" --project "$PROJECT"
  yard start --yes
  DEV_UID="$(guest id -u dev)"
  DEV_GID="$(guest id -g dev)"
  [[ "$DEV_UID" =~ ^[0-9]+$ && "$DEV_GID" =~ ^[0-9]+$ ]] || die 'invalid guest dev identity'
  wait_active
  wait_status '{"configured":false}'
  assert_wiring
  ok 'default broker, guest status, agent skills and Git helper are connected'

  before_pid="$(systemctl_user show "$UNIT" -p MainPID --value)"
  before_unit_hash="$(sha256sum "$UNIT_FILE" | awk '{print $1}')"
  info 're-running init to prove broker convergence'
  yard init --yes
  wait_active
  [ "$(systemctl_user show "$UNIT" -p MainPID --value)" = "$before_pid" ] || die 'repeated init restarted broker'
  [ "$(sha256sum "$UNIT_FILE" | awk '{print $1}')" = "$before_unit_hash" ] || die 'repeated init rewrote broker unit'
  wait_status '{"configured":false}'
  ok 'repeated init preserved owner service and guest wiring'

  info 'killing broker process and waiting for systemd recovery'
  systemctl_user kill --kill-who=main --signal=SIGKILL "$UNIT"
  after_pid=''
  for _ in $(seq 1 120); do
    after_pid="$(systemctl_user show "$UNIT" -p MainPID --value 2>/dev/null || true)"
    if systemctl_user is-active --quiet "$UNIT" && [[ "$after_pid" =~ ^[1-9][0-9]*$ ]] && [ "$after_pid" != "$before_pid" ]; then break; fi
    sleep 1
  done
  [ "$after_pid" != "$before_pid" ] || die 'systemd did not restart crashed broker'
  wait_status '{"configured":false}'
  ok 'owner user systemd restarted crashed broker'

  info 'stopping and starting yard to prove transport reconnection'
  yard stop --yes
  yard start --yes
  wait_active
  wait_status '{"configured":false}'
  ok 'yard stop/start reconnected owner broker'

  info 'checking broker with a synthetic App key imported through yard keys'
  APP_KEY="$STATE/synthetic-app-key.pem"
  APP_CONFIG="$SUBYARD_CONFIG_HOME/github-app.json"
  openssl genrsa -out "$APP_KEY" 2048 >/dev/null 2>&1
  chmod 0600 "$APP_KEY"
  printf '{"app_id":"123456","installation_id":42}\n' > "$APP_CONFIG"
  yard keys import "$APP_KEY" --label github-broker-e2e --consumer github-app-key --yes
  yard keys materialize global --yes
  MATERIALIZED_KEY="$SUBYARD_KEYS_CONSUMER_ROOT/github/github-app.pem"
  cmp -s "$APP_KEY" "$MATERIALIZED_KEY" || die 'ledger did not materialize the App key'
  [ "$(stat -c %a "$MATERIALIZED_KEY")" = 600 ] || die 'materialized App key is not protected'
  chmod 0600 "$APP_CONFIG"
  wait_status '{"configured":true}'
  credential="$(yard keys list | awk -F '\t' '$8=="github-broker-e2e" {print $1}')"
  [ -n "$credential" ] || die 'GitHub key record is missing'
  openssl genrsa -out "$APP_KEY" 2048 >/dev/null 2>&1
  yard keys rotate "$credential" --file "$APP_KEY" --yes
  yard keys materialize global --yes
  cmp -s "$APP_KEY" "$MATERIALIZED_KEY" || die 'App key rotation did not materialize'
  wait_status '{"configured":true}'
  guest test ! -e "$MATERIALIZED_KEY" || die 'owner App key entered the yard'
  ok 'broker loaded and rotated the yard keys consumer without a GitHub request'
  if [ "$MODE" = --prepare-reboot ]; then
    save_checkpoint
    trap - EXIT INT TERM
    printf 'ok: reboot checkpoint saved; the controller can reboot and run --verify-reboot\n'
    exit 0
  fi
fi

info 'explicitly disabling GitHub in default yard'
write_config disable
yard init --yes
[ ! -e "$UNIT_FILE" ] && [ ! -L "$UNIT_FILE" ] || die 'disable left owner broker service'
assert_removed
ok 'explicit no-github selection removed service, helper and skills'

info 'initializing a fresh named Hermes yard from its preset'
HERMES_YARD="hermes-e2e-$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
HERMES_PROJECT="subyard-$HERMES_YARD"
HERMES_INSTANCE="yard-$HERMES_YARD"
HERMES_UNIT="subyard-github-$HERMES_YARD.service"
HERMES_UNIT_FILE="$SUBYARD_OPERATOR_HOME/.config/systemd/user/$HERMES_UNIT"
if incus project show "$HERMES_PROJECT" >/dev/null 2>&1; then
  die "Hermes fixture project already exists: $HERMES_PROJECT"
fi
[ ! -e "$HERMES_UNIT_FILE" ] && [ ! -L "$HERMES_UNIT_FILE" ] \
  || die "Hermes fixture service path exists: $HERMES_UNIT_FILE"
HERMES_PROJECT_CLAIMED=1
if ss -Hln 'sport = :2224' 2>/dev/null | grep -q .; then
  # Retained VMs can have another fixture on the preset's standard port.
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$HERMES_YARD"
  install -m 0600 "$ROOT/config/profiles/hermes/yard.env" \
    "$SUBYARD_CONFIG_HOME/yards/$HERMES_YARD/config.env"
  hermes_yard config set SSH_PORT "$(next_port "$((SSH_PORT + 1))")" --scope yard --yes
  hermes_yard init --yes
else
  hermes_yard init --profile hermes --yes
fi
[ "$(hermes_setting INCUS_PROJECT)" = "$HERMES_PROJECT" ] \
  || die 'Hermes preset resolved an unexpected project'
[ "$(hermes_setting YARD_INSTANCE_NAME)" = "$HERMES_INSTANCE" ] \
  || die 'Hermes preset resolved an unexpected instance'
[ "$(hermes_setting DEV_SUDO)" = 0 ] || die 'Hermes preset enabled DEV_SUDO'
[ "$(hermes_setting CODING_TOOL_INTEGRATIONS)" = '<unset>' ] \
  || die 'Hermes preset selected broad coding-tool integrations'
hermes_profiles="$(hermes_setting ENVIRONMENT_PROFILES)"
printf '%s\n' "$hermes_profiles" | tr ' ' '\n' | grep -Fxq hermes \
  || die 'Hermes preset omitted hermes'
printf '%s\n' "$hermes_profiles" | tr ' ' '\n' | grep -Fxq github \
  || die 'Hermes preset omitted github'
incus project set "$HERMES_PROJECT" user.subyard.e2e="$MARKER"
incus config set "$HERMES_INSTANCE" user.subyard.e2e="$MARKER" --project "$HERMES_PROJECT"
hermes_yard start --yes
HERMES_UID="$(hermes_guest id -u dev)"
HERMES_GID="$(hermes_guest id -g dev)"
[[ "$HERMES_UID" =~ ^[0-9]+$ && "$HERMES_GID" =~ ^[0-9]+$ ]] \
  || die 'invalid Hermes guest dev identity'
for _ in $(seq 1 120); do
  systemctl_user is-active --quiet "$HERMES_UNIT" && break
  sleep 1
done
systemctl_user is-active --quiet "$HERMES_UNIT" \
  || die 'named Hermes GitHub broker user service did not become active'
hermes_status() { hermes_dev /usr/local/bin/subyard-github status 2>/dev/null | tr -d '\n'; }
for _ in $(seq 1 120); do
  [ "$(hermes_status || true)" = '{"configured":true}' ] && break
  sleep 1
done
[ "$(hermes_status || true)" = '{"configured":true}' ] \
  || die 'named Hermes guest broker status did not become configured'
hermes_guest test -L /home/dev/.hermes/skills/subyard-github \
  || die 'named Hermes GitHub skill link is missing'
[ "$(hermes_guest readlink /home/dev/.hermes/skills/subyard-github)" = \
  /usr/local/share/subyard/skills/subyard-github ] \
  || die 'named Hermes GitHub skill link points elsewhere'
hermes_guest test ! -x /usr/local/bin/hermes \
  || die 'named Hermes fixture installed the Hermes application'

opaque=/home/dev/.hermes/operator-opaque/github-state
hermes_dev install -d -m 0700 "$(dirname "$opaque")"
hermes_dev sh -c 'printf "%s\n" hermes-opaque-fixture > /home/dev/.hermes/operator-opaque/github-state && chmod 0600 /home/dev/.hermes/operator-opaque/github-state'
opaque_before="$(hermes_dev sha256sum "$opaque" | awk '{print $1}')"
info 're-running Hermes provision and init with DEV_SUDO=0'
hermes_yard provision --yes
[ "$(hermes_setting DEV_SUDO)" = 0 ] || die 'Hermes provision changed DEV_SUDO'
[ "$(hermes_dev sha256sum "$opaque" | awk '{print $1}')" = "$opaque_before" ] \
  || die 'Hermes provision changed operator-managed opaque state'
hermes_yard init --yes
[ "$(hermes_dev sha256sum "$opaque" | awk '{print $1}')" = "$opaque_before" ] \
  || die 'Hermes init changed operator-managed opaque state'
ok 'named Hermes init/provision selects GitHub, preserves opaque state and keeps DEV_SUDO=0'
credential="$(yard keys list | awk -F '\t' '$8=="github-broker-e2e" {print $1}')"
[ -n "$credential" ] || die 'GitHub key record is missing after reboot'
yard keys revoke "$credential" --yes
[ ! -e "$SUBYARD_KEYS_CONSUMER_ROOT/github/github-app.pem" ] || die 'revoked App key remained materialized'
[ "$(hermes_status || true)" = '{"configured":false}' ] || die 'Hermes broker kept using a revoked App key'
if hermes_dev /usr/local/bin/subyard-github run -- true >/dev/null 2>&1; then
  die 'broker authorized a command after key revocation'
fi
ok 'key revocation stopped new broker authorization without restarting its service'
printf 'ok: GitHub broker lifecycle and profile cleanup passed\n'

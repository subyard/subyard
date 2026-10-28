# shellcheck shell=bash
# Source-only connection operations. Callers own environment, traps and phases.
fresh() {
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$@"
    sg incus-admin -c "exec $command"
  else
    "$@"
  fi
}
init() {
  local rc
  if incus project show "$PROJECT" >/dev/null 2>&1; then
    [ "$(incus project get "$PROJECT" user.subyard.e2e)" = "$MARKER" ] || fail 'fixture project ownership differs'
  fi
  storage_diagnostics
  if yard init --yes; then :; else
    rc=$?
    storage_diagnostics >&2 || true
    return "$rc"
  fi
  incus project set "$PROJECT" user.subyard.e2e "$MARKER"
  incus config set "$INSTANCE" user.subyard.e2e "$MARKER" --project "$PROJECT"
}
start_auto_sync() {
  local rc capture_rc=0
  if systemctl_user start "$AUTO_SERVICE"; then return 0; else rc=$?; fi
  # Failure facts stay in memory and cannot replace the actual service-start exit.
  # shellcheck disable=SC2154
  XDG_RUNTIME_DIR="/run/user/$(id -u)" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$(id -u)/bus" \
    timeout --signal=TERM --kill-after=1 12 python3 "$(dirname "${BASH_SOURCE[0]}")/connection-diagnostic.py" "$AUTO_SERVICE" "$due" >&2 || capture_rc=$?
  printf 'github_connection_auto_sync_capture exit_code=%s\n' "$capture_rc" >&2 || true
  return "$rc"
}
prepare_connection_runtime() {
  local native_incus="${1:-/usr/bin/incus}"
    # The supported installer activates an immutable current-worktree runtime.
    # The base pins Incus packages; its owner API and operator access still need setup.
    bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)$$"
    fresh "$native_incus" query '/1.0/instances?project=default' >/dev/null
    fresh env HOME="$STATE/installer-home" SUBYARD_HOME="$STATE/data" SUBYARD_CONFIG_HOME="$STATE/config" \
      YARD_BIN_DIR="$STATE/bin" YARD_SHELL_RC="$STATE/installer-home/.bashrc" YARD_LOGIN_RC="$STATE/installer-home/.profile" \
      YARD_RELEASE_BASE_URL="file://$RELEASE" YARD_RELEASE_VERSION="$VERSION" \
      "$RELEASE/subyard-install.sh" --yes >/dev/null
    context
    runtime="$(readlink -f "$STATE/bin/yard")"
    case "$runtime" in "$STATE/data/runtime/releases/$VERSION-"*/bin/yard) ;; *) fail 'installed immutable runtime is missing' ;; esac
    (cd "${runtime%/bin/yard}" && sha256sum -c runtime-files.sha256 >/dev/null)
}
verify_connection_runtime() {
    app="${2:?app required}"; installation="${3:?installation required}"
    [ ! -e "$SUBYARD_CONFIG_HOME/github-app.json" ] || fail 'receiver unexpectedly needs local ID input'
    [ "$(stat -c %a "$CONNECTION")" = 600 ] || fail 'connection bundle is not protected'
    wait_status '{"configured":true}'
    issuer "$app" "$installation"
    incus exec "$INSTANCE" --project "$PROJECT" -- test ! -e "$CONNECTION" || fail 'connection entered the yard'
    incus exec "$INSTANCE" --project "$PROJECT" -- test ! -e "$SUBYARD_CONFIG_HOME/github-app.json" || fail 'App config entered the yard'
    encrypted
}
cleanup_connection_fixture() {
    units_removed=0
    if incus project show "$PROJECT" >/dev/null 2>&1; then
      [ "$(incus project get "$PROJECT" user.subyard.e2e)" = "$MARKER" ] || fail 'refusing unowned project cleanup'
      if incus info "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1; then
        [ "$(incus config get "$INSTANCE" user.subyard.e2e --expanded --project "$PROJECT")" = "$MARKER" ] || fail 'refusing unowned instance cleanup'
      fi
      yard teardown --yes >/dev/null
    fi
    for unit in "$BROKER_SERVICE" subyard-keys-sync.service subyard-keys-sync.timer "$AUTO_SERVICE"; do
      file="$HOME/.config/systemd/user/$unit"
      if [ -e "$file" ]; then
        case "$unit" in
          subyard-keys-sync.timer) cmp -s "$file" "$STATE/data/runtime/current/config/systemd/subyard-keys-sync.timer.in" || fail 'timer ownership differs' ;;
          *) rg -Fq "$STATE/data/runtime" "$file" || rg -Fq "$STATE/data/github-broker" "$file" || fail 'user service ownership differs' ;;
        esac
        systemctl_user disable --now "$unit" >/dev/null 2>&1 || true
        rm -f "$file"
        units_removed=1
      fi
    done
    [ "$units_removed" = 0 ] || systemctl_user daemon-reload
    if [ -e "/run/systemd/system/$SSH_SERVICE" ]; then
      rg -Fqx "# GitHub connection fixture $MARKER" "/run/systemd/system/$SSH_SERVICE" || fail 'SSH service ownership differs'
      sudo -n systemctl stop "$SSH_SERVICE"
      sudo -n rm -f "/run/systemd/system/$SSH_SERVICE"
      sudo -n systemctl daemon-reload
    fi
    owned; rm -rf -- "$STATE"
    printf 'ok: marker-owned connection fixtures removed\n'
}
cleanup_acceptance() {
  local rc=${1:-$?} vm failed=0 cleanup_rc cleanup_log
  trap - EXIT INT TERM
  set +e
  for vm in 1 2; do
    [ -n "${GUEST_DIRS[$vm]:-}" ] || continue
    mkdir -p "$ROOT/.build/github-connection-sync"
    cleanup_log="$ROOT/.build/github-connection-sync/$LEASE_RUN-cleanup-vm$vm.log"
    fixture "$vm" cleanup >"$cleanup_log" 2>&1
    cleanup_rc=$?
    if [ "$cleanup_rc" != 0 ]; then
      failed=1
      printf 'github-connection-e2e: VM%s cleanup exit=%s; evidence=%s\n' "$vm" "$cleanup_rc" "$cleanup_log" >&2
    fi
  done
  [ "$failed" = 0 ] || printf 'github-connection-e2e: fixture phase exit=%s; cleanup also failed (exit=3)\n' "$rc" >&2
  [ "$failed" = 0 ] || [ "$rc" != 0 ] || rc=3
  cleanup_on_exit '' "$rc"
}

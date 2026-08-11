#!/usr/bin/env bash
# handler.sh — Android profile's host-facing bridge to its in-yard emulator.
#
# The emulator boots headless inside the yard (config/profiles/android/emulator-run.sh)
# and listens on the yard's loopback (adb :5555 for the first AVD). Agents in the yard
# already use that loopback directly; these verbs add a host-side view of the same
# emulator — through the yard, never exposing it on the LAN.
#
# One symmetric pair — the host bridge is managed automatically, never as a separate verb:
#   up [avd] [-- args]  boot the emulator headless in the yard (launcher via cage+Xwayland
#                     HW-GPU, detached) AND bridge it to the host: an Incus proxy device
#                     host 127.0.0.1:$ADB_PROXY_PORT -> yard 127.0.0.1:$ADB_EMULATOR_PORT
#                     (loopback only, never on the LAN). Idempotent; waits for the adb port.
#   down              stop the emulator (disrupts agents using it; consent is owned by Go) AND
#                     remove the proxy device(s). The full reverse of `up`.
#   status            show emulator (process / adb port / boot_completed) and bridge state.
#   view [--no-control]  `adb connect` + scrcpy the screen (bridge ensured). Control is ON
#                     by default; --no-control (alias --view-only) = look-but-don't-touch,
#                     for when an agent is driving the emulator. `-- args` go to scrcpy.
#                     Needs host adb+scrcpy.
# The old bridge-only `adb`/`tunnel` verbs and the `stop` alias are gone.
#
# Operator-owned; no root. Config: config/ports.env + config/incus.project.env + subyard.env.
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
. "$SCRIPT_DIR/lib-service.sh"   # profile shared-resource helpers: yexec, svc_require_yard_running
# shellcheck source=config/profiles/android/resources/emulator/process-identity.sh
. "$RESOURCE_DIR/process-identity.sh"

DEV_USER="${DEV_USER:-dev}"
SSH_HOST="${SSH_HOST:-yard}"
ADB_EMULATOR_PORT="${ADB_EMULATOR_PORT:-5555}"
ADB_PROXY_PORT="${ADB_PROXY_PORT:-15555}"
ADB_CONSOLE_EMULATOR_PORT="${ADB_CONSOLE_EMULATOR_PORT:-5554}"
ADB_CONSOLE_PROXY_PORT="${ADB_CONSOLE_PROXY_PORT:-}"

ADB_DEVICE=adb-emu              # Incus proxy device names (yard config)
ADB_CONSOLE_DEVICE=adb-emu-console

# Where the launcher is staged in the yard, and where its boot log goes. EMU_DIR is
# root-owned (push target); EMU_LOG lives in /tmp so the dev user can write it.
EMU_DIR=/tmp/subyard-android
EMU_LOG=/tmp/subyard-android-emu.log
PROFILE_SRC="$SCRIPT_DIR/../config/profiles/android"
EMU_CONTROL="$EMU_DIR/emulator-control.sh"

device_exists() { incus config device list "$INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null | grep -qx "$1"; }

proxy_exact() { # <device> <host-port> <yard-port>
  local dev="$1" host_port="$2" yard_port="$3"
  device_exists "$dev" &&
    [ "$(incus config device get "$INSTANCE_NAME" "$dev" listen "${PROJ[@]}" 2>/dev/null || true)" = "tcp:127.0.0.1:$host_port" ] &&
    [ "$(incus config device get "$INSTANCE_NAME" "$dev" connect "${PROJ[@]}" 2>/dev/null || true)" = "tcp:127.0.0.1:$yard_port" ] &&
    [ "$(incus config device get "$INSTANCE_NAME" "$dev" bind "${PROJ[@]}" 2>/dev/null || true)" = host ]
}

# Is something listening on the in-yard adb port? (emulator fully up). Best-effort; needs ss.
emulator_listening() {
  yexec sh -c "command -v ss >/dev/null 2>&1 && ss -Hltn 'sport = :$ADB_EMULATOR_PORT' 2>/dev/null | grep -q ." 2>/dev/null
}

# Execute the staged controller through the dev login shell while preserving argument boundaries.
emu_control() {
  local command=
  printf -v command '%q ' "$EMU_CONTROL" "$@"
  yexec su - "$DEV_USER" -c "$command"
}

emulator_control_available() { yexec test -x "$EMU_CONTROL" >/dev/null 2>&1; }

# New launches have an owned process-group state. The exact argv matcher is an upgrade bridge for
# an emulator that was already running before the controller was staged; it disappears naturally
# after that legacy process is stopped and the first controller-owned launch is made.
legacy_emulator_proc() { yexec pgrep -u "$DEV_USER" -f -- "$EMU_PROCESS_PATTERN" >/dev/null 2>&1; }
emulator_proc() {
  if emulator_control_available; then emu_control is-running >/dev/null 2>&1
  else legacy_emulator_proc
  fi
}

stop_legacy_emulator() {
  local _i
  yexec pkill -TERM -u "$DEV_USER" -f -- "$EMU_PROCESS_PATTERN" >/dev/null 2>&1 || true
  for _i in $(seq 1 20); do
    legacy_emulator_proc || return 0
    sleep 0.1
  done
  yexec pkill -KILL -u "$DEV_USER" -f -- "$EMU_PROCESS_PATTERN" >/dev/null 2>&1 || true
  for _i in $(seq 1 20); do
    legacy_emulator_proc || return 0
    sleep 0.05
  done
  return 1
}

# Yard must be reachable and running before we can touch its devices or reach the emulator.
# Shared across profile resources (lib-service.sh): incus reachable + instance RUNNING.
require_yard_running() { svc_require_yard_running; }

# Best-effort: warn (do not fail) when nothing is listening on the in-yard adb port — the
# proxy is still valid, but `adb connect` will hang until the emulator finishes booting.
warn_if_emulator_down() {
  if yexec sh -c "command -v ss >/dev/null 2>&1" 2>/dev/null && ! emulator_listening; then
    warn "nothing is listening on yard 127.0.0.1:$ADB_EMULATOR_PORT yet — boot it: yard emu up (then wait for boot_completed)."
  fi
}

# Idempotent proxy-device add. $1 device name, $2 host port, $3 yard port, $4 'quiet' to
# skip the announce/confirm (the caller's own announce already covered the bridge).
ensure_proxy() {
  local dev="$1" hport="$2" yport="$3"
  if proxy_exact "$dev" "$hport" "$yport"; then
    ok "proxy device '$dev' already attached (127.0.0.1:$hport -> yard:$yport)"
    return 0
  fi
  if device_exists "$dev"; then
    incus config device remove "$INSTANCE_NAME" "$dev" "${PROJ[@]}" >/dev/null
  fi
  incus config device add "$INSTANCE_NAME" "$dev" proxy "${PROJ[@]}" \
    listen="tcp:127.0.0.1:$hport" connect="tcp:127.0.0.1:$yport" bind=host >/dev/null
  ok "added proxy 127.0.0.1:$hport -> yard:$yport"
}

# The whole host bridge (adb proxy + optional console proxy), used by `up` and `view`.
ensure_bridge() {
  ensure_proxy "$ADB_DEVICE" "$ADB_PROXY_PORT" "$ADB_EMULATOR_PORT"
  if [ -n "$ADB_CONSOLE_PROXY_PORT" ]; then
    ensure_proxy "$ADB_CONSOLE_DEVICE" "$ADB_CONSOLE_PROXY_PORT" "$ADB_CONSOLE_EMULATOR_PORT"
  fi
}

# scrcpy/adb are host tools — detect→advise (we do not install them onto the host).
need_host_tool() {
  command -v "$1" >/dev/null 2>&1 && return 0
  die "host tool '$1' not found — install it on the host, then re-run (e.g. apt install $1 / brew install $1)."
}

# scrcpy ≥ 2.4 dropped the old SurfaceControl.createDisplay(String,boolean) path that
# modern Android (14+) removed; an older scrcpy dies with NoSuchMethodException on a
# recent AVD. Warn (don't block) — the host's Android may be older. Needs sort -V.
SCRCPY_MIN=2.4
warn_if_old_scrcpy() {
  local ver
  ver="$(scrcpy --version 2>/dev/null | head -n1 | awk '{print $2}')"
  [ -n "$ver" ] || return 0
  if [ "$(printf '%s\n%s\n' "$SCRCPY_MIN" "$ver" | sort -V | head -n1)" != "$SCRCPY_MIN" ]; then
    warn "scrcpy $ver is older than $SCRCPY_MIN — recent AVDs (Android 14+) need a newer scrcpy."
    warn "upgrade it (e.g. 'sudo snap install scrcpy', or a release from github.com/Genymobile/scrcpy),"
    warn "else scrcpy fails with NoSuchMethodException (SurfaceControl.createDisplay / IClipboard)."
  fi
}

UP_FORWARD_ARGS=()
parse_up_arguments() {
  local avd_seen=0
  UP_FORWARD_ARGS=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --)
        shift
        UP_FORWARD_ARGS+=("$@")
        return 0
        ;;
      -*) die "'yard emu up' accepts one optional AVD name; put emulator options after --" ;;
      *)
        [ "$avd_seen" -eq 0 ] \
          || die "'yard emu up' accepts at most one AVD name before --"
        UP_FORWARD_ARGS+=("$1")
        avd_seen=1
        shift
        ;;
    esac
  done
}

VIEW_CONTROL=
VIEW_EXTRA=()
parse_view_arguments() {
  VIEW_CONTROL=
  VIEW_EXTRA=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --control) VIEW_CONTROL=; shift ;;
      --no-control|--view-only) VIEW_CONTROL=--no-control; shift ;;
      --)
        shift
        VIEW_EXTRA=("$@")
        return 0
        ;;
      *) die "'yard emu view' accepts control mode only; put scrcpy options after --" ;;
    esac
  done
}

cmd_view() {
  # Control ON by default (interactive); --no-control (alias --view-only) for read-only.
  parse_view_arguments "$@"
  local control="$VIEW_CONTROL"
  local extra=("${VIEW_EXTRA[@]}")
  need_host_tool adb
  need_host_tool scrcpy
  warn_if_old_scrcpy
  require_yard_running
  if ! emulator_listening; then
    info "emulator is not ready; starting its runtime and loopback bridge before opening the viewer"
    cmd_up
  else
    echo "adb bridge:"
    ensure_bridge
  fi
  local serial="127.0.0.1:$ADB_PROXY_PORT"
  # Reset first: if the proxy was attached before the emulator booted, adb cached the
  # device as 'offline' and a plain `adb connect` won't clear it. disconnect→connect does.
  info "adb (re)connect $serial"
  adb disconnect "$serial" >/dev/null 2>&1 || true
  adb connect "$serial" >/dev/null || die "adb could not connect to $serial — is the emulator booted? (yard emu status)"
  # Wait for it to come online so scrcpy doesn't race a half-up device.
  local _i state=
  for _i in $(seq 1 15); do
    state="$(adb -s "$serial" get-state 2>/dev/null | tr -d '\r' || true)"
    [ "$state" = device ] && break
    sleep 1
  done
  [ "$state" = device ] || die "device $serial is '$state' — not ready. Check: yard emu status"
  info "scrcpy -s $serial ${control:-(control enabled)}"
  exec scrcpy -s "$serial" ${control:+$control} ${extra[@]+"${extra[@]}"}
}

# Remove the proxy device(s) — the bridge half of `down`.
remove_bridge() {
  local removed=0 dev
  for dev in "$ADB_DEVICE" "$ADB_CONSOLE_DEVICE"; do
    if device_exists "$dev"; then
      incus config device remove "$INSTANCE_NAME" "$dev" "${PROJ[@]}" >/dev/null
      ok "removed proxy device '$dev'"
      removed=1
    fi
  done
  [ "$removed" = 1 ] || ok "no emu proxy device attached — nothing to remove"
}

# --- emulator lifecycle (in the yard) ----------------------------------------------
# Stage the launcher (emulator-run.sh + profile.conf) into the yard. The repo/config is
# not mounted in the yard, so push the two files the launcher needs side by side.
stage_launcher() {
  [ -r "$PROFILE_SRC/emulator-run.sh" ] || die "launcher missing: $PROFILE_SRC/emulator-run.sh"
  [ -r "$PROFILE_SRC/emulator-control.sh" ] || die "controller missing: $PROFILE_SRC/emulator-control.sh"
  [ -r "$PROFILE_SRC/profile.conf" ]    || die "profile.conf missing: $PROFILE_SRC/profile.conf"
  incus file push "$PROFILE_SRC/emulator-run.sh" "$INSTANCE_NAME$EMU_DIR/emulator-run.sh" \
    "${PROJ[@]}" --create-dirs --mode 0755 >/dev/null
  incus file push "$PROFILE_SRC/emulator-control.sh" "$INSTANCE_NAME$EMU_CONTROL" \
    "${PROJ[@]}" --mode 0755 >/dev/null
  incus file push "$PROFILE_SRC/profile.conf" "$INSTANCE_NAME$EMU_DIR/profile.conf" \
    "${PROJ[@]}" --mode 0644 >/dev/null
}

cmd_up() {
  # Pass an optional AVD name and any `-- extra` straight to the launcher.
  parse_up_arguments "$@"
  local fwd=("${UP_FORWARD_ARGS[@]}")
  require_yard_running
  if emulator_listening; then
    ok "emulator already listening on yard 127.0.0.1:$ADB_EMULATOR_PORT — nothing to boot."
    echo "adb bridge:"
    ensure_bridge
    finish_up
    return 0
  fi
  if emulator_proc; then
    # A boot is already in progress — attach to it (wait), do NOT launch a second emulator.
    info "an emulator is already starting in the yard — waiting for the adb port (not launching another)…"
  else
    info "starting the shared emulator runtime in yard '$INSTANCE_NAME'"
    stage_launcher
    ok "launcher staged at $EMU_DIR (in the yard)"
    # Detached: setsid + redirect + </dev/null so it outlives this incus exec session. A
    # login shell (su -) sources /etc/profile.d/subyard-android.sh for ANDROID_HOME/PATH.
    local start_result
    start_result="$(emu_control start "$EMU_DIR/emulator-run.sh" "$EMU_LOG" "${fwd[@]}")" \
      || die "could not launch the emulator (see $EMU_LOG in the yard: yard shell -- tail -n40 $EMU_LOG)"
    case "$start_result" in
      started) info "emulator launching — waiting for adb 127.0.0.1:$ADB_EMULATOR_PORT in the yard (up to ~180s)…" ;;
      already-running) info "another request started the emulator first — waiting for its adb port…" ;;
      *) die "emulator controller returned an unexpected result: ${start_result:-<empty>}" ;;
    esac
  fi

  # Poll for the adb port; bail early only if the whole process tree is gone (real failure).
  local _i
  for _i in $(seq 1 60); do
    if emulator_listening; then
      ok "emulator is up — adb listening on yard 127.0.0.1:$ADB_EMULATOR_PORT"
      echo "adb bridge:"
      ensure_bridge
      finish_up
      return 0
    fi
    if ! emulator_proc; then
      warn "emulator process tree is gone — boot likely failed. Last log lines:"
      yexec sh -c "tail -n 20 $EMU_LOG 2>/dev/null" || true
      die "emulator did not start (full log in the yard: yard shell -- cat $EMU_LOG)"
    fi
    sleep 3
  done
  warn "emulator still not listening after ~180s — it may still be booting. Check: yard emu status"
  warn "log (in the yard): yard shell -- tail -n40 $EMU_LOG"
}

# Shared tail of a successful `up`: the connect line + next steps.
finish_up() {
  cat <<MSG

Connect from the host:
  adb connect 127.0.0.1:$ADB_PROXY_PORT
  yard emu status                  # check boot_completed
  yard emu view                    # scrcpy the screen
Shut it down (emulator + bridge):  yard emu down
MSG
}

cmd_down() {
  require_yard_running
  # Emulator half, then the bridge half. Central typed consent already covered this apply. `down` is
  # the full reverse of `up`: nothing emulator-related stays behind.
  if emulator_listening || emulator_proc; then
    info "stopping the shared emulator runtime in yard '$INSTANCE_NAME'"
    local controlled=0
    emulator_control_available && emu_control is-running >/dev/null 2>&1 && controlled=1
    # Clean shutdown via the console if reachable, then make sure the processes are gone.
    yexec su - "$DEV_USER" -c 'adb -s emulator-'"$ADB_CONSOLE_EMULATOR_PORT"' emu kill 2>/dev/null; true' >/dev/null 2>&1 || true
    if [ "$controlled" = 1 ]; then
      local stop_result
      stop_result="$(emu_control stop)" || die "emulator controller could not stop its process group"
      case "$stop_result" in
        stopped | not-running) ;;
        *) die "emulator controller returned an unexpected stop result: ${stop_result:-<empty>}" ;;
      esac
    else
      # Non-disruptive upgrade path for an emulator launched by the previous handler.
      stop_legacy_emulator || die "legacy emulator process group did not stop"
    fi
    ok "emulator stopped"
  else
    ok "no emulator running in the yard"
  fi
  echo "adb bridge:"
  remove_bridge
}

cmd_status() {
  require_yard_running
  echo "Emulator (in the yard):"
  if emulator_listening; then
    ok "adb port: listening on yard 127.0.0.1:$ADB_EMULATOR_PORT"
    local booted
    booted="$(yexec su - "$DEV_USER" -c 'adb shell getprop sys.boot_completed 2>/dev/null' 2>/dev/null | tr -d '\r' || true)"
    case "$booted" in
      1) ok "boot_completed: 1 (ready)" ;;
      *) warn "boot_completed: ${booted:-<no answer>} (still booting)" ;;
    esac
  elif emulator_proc; then
    warn "process is running but adb 127.0.0.1:$ADB_EMULATOR_PORT not up yet — still booting."
  else
    warn "not running. Boot it: yard emu up"
  fi

  echo "Host bridge:"
  if device_exists "$ADB_DEVICE"; then
    ok "proxy 'adb-emu' attached: host 127.0.0.1:$ADB_PROXY_PORT -> yard 127.0.0.1:$ADB_EMULATOR_PORT"
    info "connect: adb connect 127.0.0.1:$ADB_PROXY_PORT   |   view: yard emu view"
  else
    warn "no bridge attached. 'yard emu up' adds it (with the emulator)."
  fi
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

prepare_resource() { # <public-verb> [validated args...]
  local verb="$1" runtime_change=0 bridge_change=0
  shift
  case "$verb" in
    up)
      parse_up_arguments "$@"
      require_yard_running
      if ! emulator_listening && ! emulator_proc; then runtime_change=1; fi
      if ! proxy_exact "$ADB_DEVICE" "$ADB_PROXY_PORT" "$ADB_EMULATOR_PORT"; then bridge_change=1; fi
      if [ -n "$ADB_CONSOLE_PROXY_PORT" ] &&
        ! proxy_exact "$ADB_CONSOLE_DEVICE" "$ADB_CONSOLE_PROXY_PORT" "$ADB_CONSOLE_EMULATOR_PORT"; then
        bridge_change=1
      fi
      if [ "$runtime_change" -eq 1 ] || [ "$bridge_change" -eq 1 ]; then
        local consequences=()
        [ "$runtime_change" -eq 0 ] || consequences+=("start the Android emulator runtime in the selected yard")
        [ "$bridge_change" -eq 0 ] || consequences+=("converge the host-loopback emulator proxy devices")
        emit_resource_assessment up true "${consequences[@]}"
      else
        emit_resource_assessment up false
      fi
      ;;
    down)
      require_no_resource_arguments down "$@"
      require_yard_running
      if emulator_listening || emulator_proc; then runtime_change=1; fi
      if device_exists "$ADB_DEVICE" || device_exists "$ADB_CONSOLE_DEVICE"; then bridge_change=1; fi
      if [ "$runtime_change" -eq 1 ] || [ "$bridge_change" -eq 1 ]; then
        local consequences=()
        [ "$runtime_change" -eq 0 ] || consequences+=("stop the shared Android emulator runtime")
        [ "$bridge_change" -eq 0 ] || consequences+=("remove the host-loopback emulator proxy devices")
        emit_resource_assessment down true "${consequences[@]}"
      else
        emit_resource_assessment down false
      fi
      ;;
    status)
      require_no_resource_arguments status "$@"
      require_yard_running
      emit_resource_assessment status false
      ;;
    view)
      parse_view_arguments "$@"
      need_host_tool adb
      need_host_tool scrcpy
      require_yard_running
      if ! emulator_listening && ! emulator_proc; then runtime_change=1; fi
      if ! proxy_exact "$ADB_DEVICE" "$ADB_PROXY_PORT" "$ADB_EMULATOR_PORT"; then bridge_change=1; fi
      if [ -n "$ADB_CONSOLE_PROXY_PORT" ] &&
        ! proxy_exact "$ADB_CONSOLE_DEVICE" "$ADB_CONSOLE_PROXY_PORT" "$ADB_CONSOLE_EMULATOR_PORT"; then
        bridge_change=1
      fi
      if [ "$runtime_change" -eq 1 ] || [ "$bridge_change" -eq 1 ]; then
        local consequences=("open an interactive scrcpy session")
        [ "$runtime_change" -eq 0 ] || consequences+=("start the Android emulator runtime as part of the requested session")
        [ "$bridge_change" -eq 0 ] || consequences+=("converge the host-loopback emulator proxy devices for the session")
        emit_resource_assessment view true "${consequences[@]}"
      else
        emit_resource_assessment view false
      fi
      ;;
    *) die "unknown 'yard emu' resource verb: '$verb'" ;;
  esac
}

require_resource_apply() { # <expected-local-action>
  local expected="$1"
  [ "${SUBYARD_RESOURCE_MODE:-}" = apply ] || die "resource apply mode is required"
  [ "${SUBYARD_RESOURCE_ACTION:-}" = "$expected" ] \
    || die "prepared resource action mismatch (expected '$expected')"
  [ -n "${SUBYARD_OPERATION_ID:-}" ] || die "resource apply operation ID is required"
}

sub="${1:-}"; [ $# -gt 0 ] && shift
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    [ -n "$sub" ] || die "resource verb is required"
    prepare_resource "$sub" "$@"
    ;;
  apply)
    case "$sub" in
      up)     require_resource_apply up; cmd_up "$@" ;;
      down)   require_resource_apply down; cmd_down "$@" ;;
      status) require_resource_apply status; cmd_status "$@" ;;
      view)   require_resource_apply view; cmd_view "$@" ;;
      *) die "unknown 'yard emu' apply verb: '$sub'" ;;
    esac
    ;;
  '')
    case "$sub" in
      is-up) emulator_listening && exit 0 || exit 1 ;;  # internal silent status probe
      ''|-h|--help) _yard_help_and_exit ;;
      *) die "typed resource dispatcher required for 'yard emu $sub'" ;;
    esac
    ;;
  *) die "unknown resource execution mode '${SUBYARD_RESOURCE_MODE:-}'" ;;
esac

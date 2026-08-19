#!/usr/bin/env bash
# Real-PID1 regression for the production power reconciler restart contract.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TEMPLATE="$ROOT/config/systemd/subyard-power-reconcile.service.in"

die() { printf 'power-reconciler-systemd-e2e: %s\n' "$*" >&2; exit 2; }

case "${SUBYARD_E2E_VM:-}" in
  1|2) ;;
  *) die 'run on an allocated P0 worker VM through dev/agent-e2e.sh' ;;
esac
for command in bash sed sudo systemctl; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
[ -r "$TEMPLATE" ] || die "production unit template is unavailable: $TEMPLATE"

RUN_ID="${SUBYARD_E2E_RUN_ID:-}"
[[ "$RUN_ID" =~ ^[0-9a-f]{8}$ ]] || die 'the allocated run ID must be eight hexadecimal characters'

MARKER="subyard-e2e-power-reconciler-systemd-v1 run=$RUN_ID"
UNIT="subyard-e2e-power-reconciler-systemd-$RUN_ID.service"
UNIT_PATH="/run/systemd/system/$UNIT"
DROPIN_DIR="/run/systemd/system/$UNIT.d"
DROPIN_PATH="$DROPIN_DIR/restart.conf"
HELPER_PATH="/usr/local/libexec/subyard-e2e-power-reconciler-systemd-$RUN_ID.sh"
RUNTIME_DIR="subyard-e2e-power-reconciler-systemd-$RUN_ID"
RUNTIME_PATH="/run/$RUNTIME_DIR"
ATTEMPT_PATH="$RUNTIME_PATH/attempts"
MODE_PATH="$RUNTIME_PATH/mode"
SYSTEMCTL_BIN="$(command -v systemctl)"
CURRENT_MODE=''

unit_state_snapshot() {
  sudo -n "$SYSTEMCTL_BIN" show "$UNIT" \
    --property=ActiveState --property=SubState --property=Result \
    --property=ExecMainStatus --property=NRestarts
}

show_diagnostics() {
  sudo -n "$SYSTEMCTL_BIN" show "$UNIT" \
    --property=LoadState --property=ActiveState --property=SubState \
    --property=Result --property=ExecMainStatus --property=NRestarts \
    --property=StartLimitIntervalUSec --property=StartLimitBurst >&2 || true
  sudo -n "$SYSTEMCTL_BIN" --no-pager --full status "$UNIT" >&2 || true
  sudo -n journalctl --no-pager -u "$UNIT" -n 80 >&2 || true
  sudo -n sed -n '1,80p' "$MODE_PATH" "$ATTEMPT_PATH" >&2 || true
}

wait_for() {
  local description="$1" deadline
  shift
  deadline=$((SECONDS + 20))
  while (( SECONDS < deadline )); do
    if "$@"; then
      return 0
    fi
    sleep 0.1
  done
  show_diagnostics
  die "timed out waiting for $description"
}

has_unit_state() {
  local active="$1" sub="$2" result="$3" status="$4" snapshot
  snapshot="$(unit_state_snapshot)" || return 1
  grep -Fxq "ActiveState=$active" <<<"$snapshot" \
    && grep -Fxq "SubState=$sub" <<<"$snapshot" \
    && grep -Fxq "Result=$result" <<<"$snapshot" \
    && grep -Fxq "ExecMainStatus=$status" <<<"$snapshot"
}

has_terminal_state() {
  local active="$1" sub="$2" result="$3" snapshot
  snapshot="$(unit_state_snapshot)" || return 1
  grep -Fxq "ActiveState=$active" <<<"$snapshot" \
    && grep -Fxq "SubState=$sub" <<<"$snapshot" \
    && grep -Fxq "Result=$result" <<<"$snapshot"
}

attempt_count() {
  local lines
  lines="$(sudo -n wc -l "$ATTEMPT_PATH" | awk '{print $1}')"
  printf '%s\n' "$((lines - 1))"
}

has_start_limit() {
  local snapshot
  snapshot="$(unit_state_snapshot)" || return 1
  grep -Fxq 'ActiveState=failed' <<<"$snapshot" \
    && grep -Fxq 'SubState=failed' <<<"$snapshot" \
    && grep -Fxq 'Result=exit-code' <<<"$snapshot" \
    && grep -Fxq 'ExecMainStatus=75' <<<"$snapshot" \
    && [ "$(attempt_count)" = 6 ] \
    && grep -Fxq 'NRestarts=6' <<<"$snapshot"
}

has_start_limit_journal() {
  sudo -n journalctl -b -u "$UNIT" --no-pager -o cat \
    | grep -F 'Start request repeated too quickly' >/dev/null
}

has_load_and_limits() {
  local snapshot wants
  snapshot="$(sudo -n "$SYSTEMCTL_BIN" show "$UNIT" \
    --property=LoadState --property=StartLimitIntervalUSec --property=StartLimitBurst \
    --property=TimeoutStartUSec --property=RuntimeMaxUSec --property=Wants)" || return 1
  wants="$(sed -n 's/^Wants=//p' <<<"$snapshot")"
  grep -Fxq 'LoadState=loaded' <<<"$snapshot" \
    && grep -Fxq 'StartLimitIntervalUSec=15min' <<<"$snapshot" \
    && grep -Fxq 'StartLimitBurst=6' <<<"$snapshot" \
    && grep -Fxq 'TimeoutStartUSec=2min' <<<"$snapshot" \
    && grep -Fxq 'RuntimeMaxUSec=2min' <<<"$snapshot" \
    && [[ " $wants " == *' incus.service '* ]] \
    && [[ " $wants " == *' incus.socket '* ]] \
    && [[ " $wants " == *' network-online.target '* ]]
}

remove_marker_file() {
  local path="$1" marker_line="$2" actual
  sudo -n test ! -L "$path" || return 1
  if sudo -n test -e "$path"; then
    actual="$(sudo -n sed -n "${marker_line}p" "$path")" || return 1
    [ "$actual" = "# $MARKER" ] || return 1
    sudo -n rm -f -- "$path"
  fi
}

assert_marker_owned_or_absent() {
  local path="$1" marker_line="$2" actual
  if sudo -n test ! -e "$path" && sudo -n test ! -L "$path"; then
    return 0
  fi
  sudo -n test -f "$path" && sudo -n test ! -L "$path" \
    || die "refusing to replace unsafe runtime path: $path"
  actual="$(sudo -n sed -n "${marker_line}p" "$path")" \
    || die "cannot inspect runtime path: $path"
  [ "$actual" = "# $MARKER" ] \
    || die "refusing to replace unowned runtime path: $path"
}

preflight_runtime_paths() {
  local entries runtime_entries
  assert_marker_owned_or_absent "$UNIT_PATH" 1
  assert_marker_owned_or_absent "$HELPER_PATH" 2
  assert_marker_owned_or_absent "$ATTEMPT_PATH" 1
  assert_marker_owned_or_absent "$MODE_PATH" 1
  if sudo -n test -e "$RUNTIME_PATH" || sudo -n test -L "$RUNTIME_PATH"; then
    sudo -n test -d "$RUNTIME_PATH" && sudo -n test ! -L "$RUNTIME_PATH" \
      || die "refusing unsafe runtime directory: $RUNTIME_PATH"
    runtime_entries="$(sudo -n find "$RUNTIME_PATH" -mindepth 1 -maxdepth 1 \
      -printf '%f\n' | sort)"
    [ "$runtime_entries" = $'attempts\nmode' ] \
      || die "refusing non-exclusive runtime directory: $RUNTIME_PATH"
  fi
  if sudo -n test -e "$DROPIN_DIR" || sudo -n test -L "$DROPIN_DIR"; then
    sudo -n test -d "$DROPIN_DIR" && sudo -n test ! -L "$DROPIN_DIR" \
      || die "refusing unsafe drop-in directory: $DROPIN_DIR"
    assert_marker_owned_or_absent "$DROPIN_PATH" 1
    entries="$(sudo -n find "$DROPIN_DIR" -mindepth 1 -maxdepth 1 -printf '%f\n')"
    [ "$entries" = "$(basename "$DROPIN_PATH")" ] \
      || die "refusing non-exclusive drop-in directory: $DROPIN_DIR"
  fi
}

cleanup() {
  local rc=$? cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  sudo -n "$SYSTEMCTL_BIN" stop "$UNIT" >/dev/null 2>&1 || true
  sudo -n "$SYSTEMCTL_BIN" reset-failed "$UNIT" >/dev/null 2>&1 || true
  remove_marker_file "$UNIT_PATH" 1 || cleanup_failed=1
  remove_marker_file "$DROPIN_PATH" 1 || cleanup_failed=1
  remove_marker_file "$HELPER_PATH" 2 || cleanup_failed=1
  remove_marker_file "$ATTEMPT_PATH" 1 || cleanup_failed=1
  remove_marker_file "$MODE_PATH" 1 || cleanup_failed=1
  if sudo -n test -d "$RUNTIME_PATH"; then
    sudo -n rmdir -- "$RUNTIME_PATH" || cleanup_failed=1
  fi
  if sudo -n test -d "$DROPIN_DIR"; then
    sudo -n rmdir -- "$DROPIN_DIR" || cleanup_failed=1
  fi
  sudo -n "$SYSTEMCTL_BIN" daemon-reload || cleanup_failed=1
  if [ "$cleanup_failed" -ne 0 ] && [ "$rc" -eq 0 ]; then
    rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

write_helper() {
  sudo -n install -D -o root -g root -m 0755 /dev/stdin "$HELPER_PATH" <<'EOF'
#!/usr/bin/env bash
# subyard-e2e-power-reconciler-systemd-v1 token marker follows from the owning path.
set -euo pipefail

mode="$(sed -n '2p' "${SUBYARD_E2E_POWER_MODE_FILE:?}")"
printf 'attempt %s\n' "$mode" >> "${SUBYARD_E2E_POWER_ATTEMPTS:?}"
attempts="$(wc -l < "${SUBYARD_E2E_POWER_ATTEMPTS:?}")"
attempts="$((attempts - 1))"

case "$mode" in
  success)
    exit 0
    ;;
  retry-once)
    case "$attempts" in
      1) exit 75 ;;
      2) exit 0 ;;
      *) exit 76 ;;
    esac
    ;;
  exit-one)
    exit 1
    ;;
  signal)
    exec /bin/sh -c 'kill -KILL $$'
    ;;
  timeout)
    sleep 300
    ;;
  persistent-75)
    exit 75
    ;;
  *)
    exit 64
    ;;
esac
EOF
  sudo -n sed -i "2c\\# $MARKER" "$HELPER_PATH"
}

materialize_unit() {
  local mode="$1"
  case "$mode" in
    success|retry-once|exit-one|signal|timeout|persistent-75) ;;
    *) die "unknown mode: $mode" ;;
  esac
  CURRENT_MODE="$mode"
  {
    printf '# %s\n' "$MARKER"
    sed "s|@SUBYARD_POWER_RECONCILER@|$HELPER_PATH|g" "$TEMPLATE"
    printf '\n[Service]\n'
    printf 'Environment=SUBYARD_E2E_POWER_ATTEMPTS=%s\n' "$ATTEMPT_PATH"
    printf 'Environment=SUBYARD_E2E_POWER_MODE_FILE=%s\n' "$MODE_PATH"
  } | sudo -n install -D -o root -g root -m 0644 /dev/stdin "$UNIT_PATH"
  sudo -n install -d -o root -g root -m 0755 "$DROPIN_DIR"
  {
    printf '# %s\n' "$MARKER"
    printf '[Service]\n'
    printf 'RestartSec=100ms\n'
    printf 'RuntimeDirectory=%s\nRuntimeDirectoryPreserve=yes\n' "$RUNTIME_DIR"
  } | sudo -n install -o root -g root -m 0644 /dev/stdin "$DROPIN_PATH"
  sudo -n "$SYSTEMCTL_BIN" daemon-reload
}

set_runtime_max_test_override() {
  {
    printf '# %s\n' "$MARKER"
    printf '[Service]\n'
    printf 'RestartSec=100ms\nRuntimeMaxSec=500ms\n'
    printf 'RuntimeDirectory=%s\nRuntimeDirectoryPreserve=yes\n' "$RUNTIME_DIR"
  } | sudo -n install -o root -g root -m 0644 /dev/stdin "$DROPIN_PATH"
  sudo -n "$SYSTEMCTL_BIN" daemon-reload
}

start_case() {
  sudo -n "$SYSTEMCTL_BIN" stop "$UNIT" >/dev/null 2>&1 || true
  sudo -n "$SYSTEMCTL_BIN" reset-failed "$UNIT" >/dev/null 2>&1 || true
  sudo -n install -d -o root -g root -m 0755 "$RUNTIME_PATH"
  printf '# %s\n' "$MARKER" \
    | sudo -n install -o root -g root -m 0600 /dev/stdin "$ATTEMPT_PATH"
  printf '# %s\n%s\n' "$MARKER" "$CURRENT_MODE" \
    | sudo -n install -o root -g root -m 0600 /dev/stdin "$MODE_PATH"
  sudo -n "$SYSTEMCTL_BIN" start --no-block "$UNIT"
}

# Mutations caught by this test:
# - Removing RestartForceExitStatus=75 or changing it so a transient 75 does not restart.
# - Changing the normal zero/one exit handling or the service result reported by PID1.
# - Disabling or altering the production 15-minute, six-start rate limit.
# - Removing or altering either production two-minute execution bound.
# - Replacing the production unit with one that does not load or execute under real systemd.
preflight_runtime_paths
write_helper

materialize_unit success
wait_for 'the materialized production unit and its rate limits' has_load_and_limits

start_case
wait_for 'a single successful attempt' \
  has_unit_state inactive dead success 0
[ "$(attempt_count)" = 1 ] || die 'success executed more than once'

materialize_unit retry-once
start_case
wait_for 'one retry after exit 75, followed by success' \
  has_unit_state inactive dead success 0
[ "$(attempt_count)" = 2 ] || die 'temporary failure did not execute exactly twice'

materialize_unit exit-one
start_case
wait_for 'terminal exit 1 without a restart' \
  has_unit_state failed failed exit-code 1
[ "$(attempt_count)" = 1 ] || die 'permanent failure was restarted'

materialize_unit signal
start_case
wait_for 'terminal signal without a restart' \
  has_terminal_state failed failed signal
[ "$(attempt_count)" = 1 ] || die 'signal failure was restarted'

materialize_unit timeout
set_runtime_max_test_override
start_case
wait_for 'terminal runtime timeout without a restart' \
  has_terminal_state failed failed timeout
[ "$(attempt_count)" = 1 ] || die 'runtime timeout was restarted'

materialize_unit persistent-75
start_case
wait_for 'the production six-start limit after persistent exit 75' \
  has_start_limit
has_start_limit_journal \
  || die 'systemd journal omitted the start-limit terminal diagnostic'

printf 'ok: production power reconciler systemd restart and start-limit contract\n'

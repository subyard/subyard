#!/usr/bin/env bash
# Host-free contract for the bounded Incus launch used by the systemd 255 fixture.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE="$ROOT/dev/e2e/power-reconciler-systemd-255.sh"
TEMPORARY="$(mktemp -d /tmp/subyard-systemd255-launch.XXXXXX)"
FAKE_BIN="$TEMPORARY/bin"
STATE="$TEMPORARY/state"
TIMEOUT_LOG="$TEMPORARY/timeout.log"
INCUS_LOG="$TEMPORARY/incus.log"

fail() {
  printf 'power-reconciler-systemd-255-launch: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  case "$TEMPORARY" in
    /tmp/subyard-systemd255-launch.*) find "$TEMPORARY" -depth -delete ;;
  esac
}
trap cleanup EXIT

mkdir -p "$FAKE_BIN" "$STATE"

cat > "$FAKE_BIN/sudo" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" != -n ] || shift
exec "$@"
EOF

cat > "$FAKE_BIN/timeout" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [[ "${1:-}" == --* ]]; do shift; done
deadline="${1:?missing timeout deadline}"
shift
printf '%s|%s\n' "$deadline" "$*" >> "$FAKE_TIMEOUT_LOG"
"$@"
EOF

cat > "$FAKE_BIN/incus" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_INCUS_LOG"
verb="${1:-}"
shift || true

case "$verb" in
  project)
    action="${1:-}"
    case "$action" in
      show)
        [ -f "$FAKE_INCUS_STATE/project" ]
        ;;
      create)
        : > "$FAKE_INCUS_STATE/project"
        ;;
      get)
        printf '%s\n' "$FAKE_INCUS_MARKER"
        ;;
      delete)
        rm -f "$FAKE_INCUS_STATE/project"
        ;;
      *) exit 64 ;;
    esac
    ;;
  profile|file)
    ;;
  launch)
    if [ "$FAKE_INCUS_MODE" = launch-timeout ]; then
      exit 124
    fi
    if [ "$FAKE_INCUS_MODE" = slow-success ]; then
      sleep 2.2
    fi
    : > "$FAKE_INCUS_STATE/instance"
    ;;
  restart)
    : > "$FAKE_INCUS_STATE/restarted"
    ;;
  delete)
    rm -f "$FAKE_INCUS_STATE/instance"
    ;;
  image)
    action="${1:-}"
    case "$action" in
      info)
        [ -f "$FAKE_INCUS_STATE/image" ]
        if [ "$FAKE_INCUS_MODE" = wrong-image ]; then
          printf 'Type: virtual-machine\n'
        else
          printf 'Type: container\n'
        fi
        ;;
      copy)
        if [ "$FAKE_INCUS_MODE" = image-timeout ]; then
          exit 124
        fi
        if [ "$FAKE_INCUS_MODE" = slow-success ]; then
          sleep 2.2
        fi
        : > "$FAKE_INCUS_STATE/image"
        ;;
      list)
        ;;
      *) exit 64 ;;
    esac
    ;;
  config)
    action="${1:-}"
    case "$action" in
      show)
        [ -f "$FAKE_INCUS_STATE/instance" ]
        ;;
      get)
        printf '%s\n' "$FAKE_INCUS_MARKER"
        ;;
      *) exit 64 ;;
    esac
    ;;
  exec)
    args=" $* "
    case "$args" in
      *' systemd-analyze --version '*)
        printf 'systemd 255 (255.4-1ubuntu8)\n'
        ;;
      *' systemd-analyze verify '*v080*)
        printf "RestartForceExitStatus= set, which isn't allowed for Type=oneshot services\n" >&2
        exit 1
        ;;
      *' systemctl show '*v080*' --property=LoadState --value '*)
        printf 'bad-setting\n'
        ;;
      *' systemctl show '*candidate*' --property=LoadState --value '*)
        printf 'loaded\n'
        ;;
      *' systemctl show '*install*' --property=LoadState --value '*)
        if [ -f "$FAKE_INCUS_STATE/installed" ]; then
          printf 'loaded\n'
        else
          printf 'bad-setting\n'
        fi
        ;;
      *' install-power-reconciler.sh --yes '*)
        : > "$FAKE_INCUS_STATE/installed"
        ;;
      *' systemctl show '*install*)
        printf '%s\n' \
          'LoadState=loaded' \
          'NeedDaemonReload=no' \
          'ActiveState=inactive' \
          'SubState=dead' \
          'Result=success' \
          'ExecMainStatus=0' \
          'ExecMainStartTimestampMonotonic=123'
        ;;
      *' cat /proc/sys/kernel/random/boot_id '*)
        if [ -f "$FAKE_INCUS_STATE/restarted" ]; then
          printf 'boot-after\n'
        else
          printf 'boot-before\n'
        fi
        ;;
      *)
        ;;
    esac
    ;;
  *)
    exit 64
    ;;
esac
EOF

chmod +x "$FAKE_BIN/incus" "$FAKE_BIN/sudo" "$FAKE_BIN/timeout"

run_fixture() { # <mode> <run-id> [image-timeout] [launch-timeout] [restart-timeout]
  local mode="$1" run_id="$2" image_timeout="${3:-}" launch_timeout="${4:-}" \
    restart_timeout="${5:-}"
  local marker="subyard-e2e-systemd255-v1 run=$run_id"
  rm -f "$STATE"/* "$TIMEOUT_LOG" "$INCUS_LOG"
  [ "$mode" != cached-success ] || : > "$STATE/image"
  PATH="$FAKE_BIN:$PATH" \
    FAKE_INCUS_STATE="$STATE" \
    FAKE_INCUS_MARKER="$marker" \
    FAKE_INCUS_MODE="$mode" \
    FAKE_INCUS_LOG="$INCUS_LOG" \
    FAKE_TIMEOUT_LOG="$TIMEOUT_LOG" \
    SUBYARD_E2E_VM=2 \
    SUBYARD_E2E_RUN_ID="$run_id" \
    E2E_PROGRESS_INTERVAL=1 \
    SUBYARD_SYSTEMD255_IMAGE_TIMEOUT_SECONDS="$image_timeout" \
    SUBYARD_SYSTEMD255_LAUNCH_TIMEOUT_SECONDS="$launch_timeout" \
    SUBYARD_SYSTEMD255_RESTART_TIMEOUT_SECONDS="$restart_timeout" \
    bash "$FIXTURE" 2>&1
}

# Mutations caught by this test:
# - Pulling the remote Ubuntu image inside the launch timeout instead of a retained alias.
# - Restoring the flaky 300-second launch budget.
# - Applying the cold launch budget to restart as well.
# - Retrying a timed-out image fetch or launch.
# - Removing operator-visible progress or operation/limit failure diagnostics.
success_output="$(run_fixture slow-success a1b2c3d4)" \
  || fail "the simulated successful fixture failed: $success_output"
grep -Eq '^900\|.*incus image copy images:ubuntu/24.04/cloud local: .*--alias subyard-e2e-ubuntu-24.04-systemd255-container .*--target-project default' \
  "$TIMEOUT_LOG" \
  || fail 'the default image-cache preparation budget is not 900 seconds'
grep -Eq '^600\|.*incus launch subyard-e2e-ubuntu-24.04-systemd255-container ' \
  "$TIMEOUT_LOG" \
  || fail 'the default local-image launch budget is not 600 seconds'
grep -Eq '^300\|.*incus restart systemd255-a1b2c3d4 ' "$TIMEOUT_LOG" \
  || fail 'the default restart budget is not independently 300 seconds'
grep -Fq '[ .. ] caching Ubuntu 24.04/systemd 255 fixture image' <<<"$success_output" \
  || fail 'cold image-cache preparation is not visible to the operator'
grep -Fq '[ .. ] launching Ubuntu 24.04/systemd 255 fixture from local cache' \
  <<<"$success_output" \
  || fail 'local-image launch progress is not visible to the operator'
grep -Fq '(still working, ' <<<"$success_output" \
  || fail 'cold image-cache preparation does not emit a periodic heartbeat'

cached_output="$(run_fixture cached-success cafefeed)" \
  || fail "the cached-image fixture failed: $cached_output"
[ "$(grep -c '^image copy ' "$INCUS_LOG" || true)" -eq 0 ] \
  || fail 'the fixture fetched an already cached systemd-255 image'
[ "$(grep -c '^launch subyard-e2e-ubuntu-24.04-systemd255-container ' "$INCUS_LOG")" -eq 1 ] \
  || fail 'the cached-image fixture did not launch the local alias exactly once'

set +e
wrong_image_output="$(run_fixture wrong-image f00dcafe)"
wrong_image_rc=$?
set -e
[ "$wrong_image_rc" -eq 2 ] \
  && grep -Fq 'cached systemd 255 fixture image subyard-e2e-ubuntu-24.04-systemd255-container is not a container image' \
    <<<"$wrong_image_output" \
  || fail 'an incompatible cached image alias was not rejected'

set +e
image_failure_output="$(run_fixture image-timeout 0badcafe 17 19 11)"
image_failure_rc=$?
set -e
[ "$image_failure_rc" -eq 2 ] \
  || fail "timed-out image fetch returned $image_failure_rc instead of fixture failure 2: $image_failure_output"
[ "$(grep -c '^image copy images:ubuntu/24.04/cloud local: ' "$INCUS_LOG")" -eq 1 ] \
  || fail 'the fixture retried the timed-out image fetch'
[ "$(grep -c '^launch ' "$INCUS_LOG" || true)" -eq 0 ] \
  || fail 'the fixture launched after its image fetch timed out'
grep -Fq 'timed out after 17s while caching Ubuntu 24.04/systemd 255 fixture image' \
  <<<"$image_failure_output" \
  || fail "timed-out image fetch omitted its operation and limit: $image_failure_output"

set +e
failure_output="$(run_fixture launch-timeout d4c3b2a1 13 17 11)"
failure_rc=$?
set -e
[ "$failure_rc" -eq 2 ] \
  || fail "timed-out launch returned $failure_rc instead of fixture failure 2: $failure_output"
[ "$(grep -c '^launch subyard-e2e-ubuntu-24.04-systemd255-container ' "$INCUS_LOG")" -eq 1 ] \
  || fail 'the fixture retried the timed-out cold launch'
grep -Fq 'timed out after 17s while launching Ubuntu 24.04/systemd 255 fixture' \
    <<<"$failure_output" \
  || fail "timed-out launch omitted its operation and limit: $failure_output"

set +e
invalid_image_output="$(run_fixture success 55667788 0 17 11)"
invalid_image_rc=$?
invalid_launch_output="$(run_fixture success 11223344 13 0 11)"
invalid_launch_rc=$?
invalid_restart_output="$(run_fixture success 44332211 13 17 0)"
invalid_restart_rc=$?
set -e
[ "$invalid_image_rc" -eq 2 ] \
  && grep -Fq 'SUBYARD_SYSTEMD255_IMAGE_TIMEOUT_SECONDS must be a positive integer' \
    <<<"$invalid_image_output" \
  || fail 'zero image-cache preparation budget was not rejected explicitly'
[ "$invalid_launch_rc" -eq 2 ] \
  && grep -Fq 'SUBYARD_SYSTEMD255_LAUNCH_TIMEOUT_SECONDS must be a positive integer' \
    <<<"$invalid_launch_output" \
  || fail 'zero cold launch budget was not rejected explicitly'
[ "$invalid_restart_rc" -eq 2 ] \
  && grep -Fq 'SUBYARD_SYSTEMD255_RESTART_TIMEOUT_SECONDS must be a positive integer' \
    <<<"$invalid_restart_output" \
  || fail 'zero restart budget was not rejected explicitly'

printf 'ok: systemd-255 image caching and launch are bounded, observable and not retried\n'

#!/usr/bin/env bash
# Reproduce the v0.7.2 parser failure and candidate load on Ubuntu 24.04/systemd 255.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_ID="${SUBYARD_E2E_RUN_ID:-}"
PROJECT="subyard-systemd255-$RUN_ID"
INSTANCE="systemd255-$RUN_ID"
MARKER="subyard-e2e-systemd255-v1 run=$RUN_ID"
OLD_TEMPLATE="$ROOT/tests/fixtures/systemd/subyard-power-reconcile-v0.7.2.service.in"
CANDIDATE_TEMPLATE="$ROOT/config/systemd/subyard-power-reconcile.service.in"
OLD_UNIT="subyard-e2e-power-v072-$RUN_ID.service"
CANDIDATE_UNIT="subyard-e2e-power-candidate-$RUN_ID.service"
INSTALL_UNIT="subyard-e2e-power-install-$RUN_ID.service"
INSTALL_ROOT="/run/subyard-e2e-power-install-$RUN_ID"
INSTALL_ARCHIVE="$INSTALL_ROOT.tar.gz"
TEMPORARY=''
PROJECT_CREATED=0
INCUS_COMMAND_TIMEOUT="${SUBYARD_SYSTEMD255_INCUS_TIMEOUT_SECONDS:-30}"
INCUS_LAUNCH_TIMEOUT=300
INCUS_KILL_AFTER_SECONDS=10

die() { printf 'power-reconciler-systemd-255: %s\n' "$*" >&2; exit 2; }

case "${SUBYARD_E2E_VM:-}" in
  1|2) ;;
  *) die 'run on an allocated P0 worker VM through the retained P0 lease' ;;
esac
[[ "$RUN_ID" =~ ^[0-9a-f]{8}$ ]] \
  || die 'the allocated run ID must be eight hexadecimal characters'
for command in incus sed sudo tar; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
[ -r "$OLD_TEMPLATE" ] && [ -r "$CANDIDATE_TEMPLATE" ] \
  || die 'power reconciler unit templates are unavailable'
[[ "$INCUS_COMMAND_TIMEOUT" =~ ^[1-9][0-9]*$ ]] \
  || die 'SUBYARD_SYSTEMD255_INCUS_TIMEOUT_SECONDS must be a positive integer'

run_incus() {
  local deadline="$1"
  shift
  local binary=/usr/bin/incus socket=/var/lib/incus/unix.socket
  if [ -S "$socket" ] && [ ! -w "$socket" ]; then
    timeout --signal=TERM --kill-after="$INCUS_KILL_AFTER_SECONDS" "$deadline" \
      sudo -n "$binary" "$@" </dev/null
  else
    timeout --signal=TERM --kill-after="$INCUS_KILL_AFTER_SECONDS" "$deadline" \
      "$binary" "$@" </dev/null
  fi
}

bounded_incus() { run_incus "$INCUS_COMMAND_TIMEOUT" "$@"; }
launch_incus() { run_incus "$INCUS_LAUNCH_TIMEOUT" launch "$@"; }

assert_owned_project() {
  [ "$(bounded_incus project get "$PROJECT" user.subyard.systemd255 2>/dev/null)" = \
    "$MARKER" ]
}

assert_owned_instance() {
  bounded_incus config show "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1 \
    || return 1
  [ "$(bounded_incus config get "$INSTANCE" user.subyard.systemd255 \
    --project "$PROJECT" 2>/dev/null)" = "$MARKER" ]
}

delete_marked_instance() {
  local attempt
  for attempt in 1 2 3; do
    bounded_incus config show "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1 \
      || return 0
    assert_owned_project && assert_owned_instance || {
      printf 'power-reconciler-systemd-255: refusing to delete unmarked instance %s/%s\n' \
        "$PROJECT" "$INSTANCE" >&2
      return 1
    }
    if bounded_incus delete "$INSTANCE" --project "$PROJECT" --force >/dev/null; then
      return 0
    fi
    bounded_incus config show "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1 \
      || return 0
    [ "$attempt" -lt 3 ] || {
      printf 'power-reconciler-systemd-255: could not delete marked instance %s/%s after 3 attempts\n' \
        "$PROJECT" "$INSTANCE" >&2
      return 1
    }
    printf '  [warn] cleanup delete of %s/%s failed; retrying (%s/3)\n' \
      "$PROJECT" "$INSTANCE" "$((attempt + 1))"
    sleep 2
  done
}

cleanup() {
  local rc=$? cleanup_failed=0 fingerprint fingerprints=''
  trap - EXIT INT TERM
  set +e
  if [ -n "$TEMPORARY" ]; then
    case "$TEMPORARY" in
      /tmp/*) find "$TEMPORARY" -depth -delete || cleanup_failed=1 ;;
      *) cleanup_failed=1 ;;
    esac
  fi
  if [ "$PROJECT_CREATED" = 1 ]; then
    if ! bounded_incus project show "$PROJECT" >/dev/null 2>&1; then
      printf 'power-reconciler-systemd-255: project lookup failed during cleanup: %s\n' \
        "$PROJECT" >&2
      cleanup_failed=1
    fi
  fi
  if [ "$PROJECT_CREATED" = 1 ] && [ "$cleanup_failed" = 0 ]; then
    assert_owned_project || cleanup_failed=1
    if [ "$cleanup_failed" = 0 ]; then
      if bounded_incus config show "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1; then
        delete_marked_instance || cleanup_failed=1
      fi
      if [ "$cleanup_failed" = 0 ]; then
        fingerprints="$(bounded_incus image list --project "$PROJECT" --format csv -c f)" \
          || cleanup_failed=1
      fi
      if [ "$cleanup_failed" = 0 ]; then
        while IFS= read -r fingerprint; do
          [ -n "$fingerprint" ] || continue
          bounded_incus image delete "$fingerprint" --project "$PROJECT" >/dev/null \
            || cleanup_failed=1
        done <<<"$fingerprints"
      fi
      if [ "$cleanup_failed" = 0 ]; then
        assert_owned_project \
          && bounded_incus project delete "$PROJECT" >/dev/null \
          || cleanup_failed=1
      fi
    fi
  fi
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

! bounded_incus project show "$PROJECT" >/dev/null 2>&1 \
  || die "fixture project already exists: $PROJECT"
bounded_incus project create "$PROJECT" \
  -c features.images=true \
  -c features.profiles=true \
  -c features.storage.volumes=true \
  -c user.subyard.systemd255="$MARKER" >/dev/null
PROJECT_CREATED=1
bounded_incus profile device add default root disk path=/ pool=default \
  --project "$PROJECT" >/dev/null
bounded_incus profile device add default eth0 nic name=eth0 network=incusbr0 \
  --project "$PROJECT" >/dev/null
launch_incus images:ubuntu/24.04/cloud "$INSTANCE" --project "$PROJECT" \
  -c user.subyard.systemd255="$MARKER" >/dev/null

for _ in $(seq 1 120); do
  bounded_incus exec "$INSTANCE" --project "$PROJECT" -- true >/dev/null 2>&1 && break
  sleep 1
done
bounded_incus exec "$INSTANCE" --project "$PROJECT" -- true >/dev/null 2>&1 \
  || die 'Ubuntu 24.04 parser container did not become ready'

version="$(bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemd-analyze --version | awk 'NR == 1 {print $2}')"
[ "$version" = 255 ] || die "Ubuntu 24.04 image has systemd $version, want 255"

TEMPORARY="$(mktemp -d "/tmp/subyard-e2e-systemd255.$RUN_ID.XXXXXX")"
sed 's|@SUBYARD_POWER_RECONCILER@|/bin/true|g' "$OLD_TEMPLATE" \
  > "$TEMPORARY/$OLD_UNIT"
sed 's|@SUBYARD_POWER_RECONCILER@|/bin/true|g' "$CANDIDATE_TEMPLATE" \
  > "$TEMPORARY/$CANDIDATE_UNIT"
sed 's|@SUBYARD_POWER_RECONCILER@|/bin/true|g' "$OLD_TEMPLATE" \
  > "$TEMPORARY/$INSTALL_UNIT"
tar -C "$ROOT" -czf "$TEMPORARY/product.tar.gz" scripts config/systemd
bounded_incus file push "$TEMPORARY/$OLD_UNIT" \
  "$INSTANCE/run/systemd/system/$OLD_UNIT" --project "$PROJECT" >/dev/null
bounded_incus file push "$TEMPORARY/$CANDIDATE_UNIT" \
  "$INSTANCE/run/systemd/system/$CANDIDATE_UNIT" --project "$PROJECT" >/dev/null
bounded_incus file push "$TEMPORARY/$INSTALL_UNIT" \
  "$INSTANCE/run/systemd/system/$INSTALL_UNIT" --project "$PROJECT" >/dev/null
bounded_incus file push "$TEMPORARY/product.tar.gz" \
  "$INSTANCE$INSTALL_ARCHIVE" \
  --project "$PROJECT" >/dev/null
bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  install -d -m 0755 "$INSTALL_ROOT"
bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  tar -xzf "$INSTALL_ARCHIVE" -C "$INSTALL_ROOT"
bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  test -r "$INSTALL_ROOT/scripts/install-power-reconciler.sh" \
  || die 'production installer was not unpacked into the systemd 255 fixture'

set +e
diagnostics="$(bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemd-analyze verify "/run/systemd/system/$OLD_UNIT" 2>&1)"
verify_rc=$?
set -e
[ "$verify_rc" -ne 0 ] || die 'systemd 255 unexpectedly verified the v0.7.2 unit'
grep -Fq "RestartForceExitStatus= set, which isn't allowed for Type=oneshot services" \
  <<<"$diagnostics" \
  || die 'systemd 255 omitted the expected v0.7.2 parser diagnostic'
bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemd-analyze verify "/run/systemd/system/$CANDIDATE_UNIT"

bounded_incus exec "$INSTANCE" --project "$PROJECT" -- systemctl daemon-reload
bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemctl enable "$INSTALL_UNIT" >/dev/null
bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemctl is-enabled --quiet "$INSTALL_UNIT" \
  || die 'systemd 255 production-installer fixture is not enabled before upgrade'
[ "$(bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemctl show "$OLD_UNIT" --property=LoadState --value)" = bad-setting ] \
  || die 'systemd 255 did not reproduce v0.7.2 LoadState=bad-setting'
[ "$(bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemctl show "$CANDIDATE_UNIT" --property=LoadState --value)" = loaded ] \
  || die 'systemd 255 did not load the candidate unit'
[ "$(bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemctl show "$INSTALL_UNIT" --property=LoadState --value)" = bad-setting ] \
  || die 'systemd 255 did not load the production-installer fixture as bad-setting'

bounded_incus exec "$INSTANCE" --project "$PROJECT" -- env \
  ASSUME_YES=1 \
  SUBYARD_ENGINE_CONTEXT=1 \
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1 \
  SUBYARD_OPERATOR_HOME=/root \
  SUBYARD_CONFIG_DIR="$INSTALL_ROOT/config" \
  SUBYARD_CONFIG_HOME="$INSTALL_ROOT/config-home" \
  SUBYARD_HOME="$INSTALL_ROOT/data" \
  STORAGE_PATH="$INSTALL_ROOT/data/incus/storage" \
  HOST_BASE="$INSTALL_ROOT/host" \
  RESTRICTED_DISK_PATHS="$INSTALL_ROOT/host" \
  ACCESS_KIND=local \
  YARD_KIND=container \
  YARD_INSTANCE_NAME=yard \
  INCUS_PROJECT=subyard \
  INCUS_BRIDGE=incusbr0 \
  SSH_HOST=yard \
  DEV_USER=dev \
  DEV_UID=2000 \
  DEV_SUDO=1 \
  FORWARD_SSH_AGENT=1 \
  NESTED_E2E_VMS=0 \
  SUBYARD_POWER_ENGINE_SOURCE=/bin/true \
  SUBYARD_POWER_LIBEXEC_DIR="/run/subyard-e2e-power-$RUN_ID" \
  SUBYARD_POWER_RECONCILER_PATH="/run/subyard-e2e-power-$RUN_ID/reconciler" \
  SUBYARD_POWER_UNIT_PATH="/run/systemd/system/$INSTALL_UNIT" \
  /bin/bash "$INSTALL_ROOT/scripts/install-power-reconciler.sh" --yes
manager_state="$(bounded_incus exec "$INSTANCE" --project "$PROJECT" -- \
  systemctl show "$INSTALL_UNIT" \
    --property=LoadState --property=NeedDaemonReload)"
grep -Fxq 'LoadState=loaded' <<<"$manager_state" \
  && grep -Fxq 'NeedDaemonReload=no' <<<"$manager_state" \
  || die "production installer left stale systemd 255 manager state: ${manager_state//$'\n'/, }"

printf 'ok: Ubuntu 24.04/systemd 255 rejects v0.7.2 and production installer loads the candidate unit\n'

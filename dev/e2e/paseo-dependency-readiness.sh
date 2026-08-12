#!/usr/bin/env bash
# Clean real-yard acceptance for Paseo's implicit Codex dependency and SSH readiness path.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=''
YARD_NAME=''

die() { printf 'paseo-dependency-e2e: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
for command in go incus sed sudo; do
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
yard() { "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"; }

stage_candidate_check() {
  sed -e 's|@DEV_USER@|dev|g' -e 's|@DEV_HOME@|/home/dev|g' \
    "$ROOT/config/agents/paseo/bin/paseo-check" \
    | incus exec "yard-$YARD_NAME" --project "subyard-$YARD_NAME" -- \
      sh -euc '
        temporary=$(mktemp /usr/local/bin/.paseo-check.XXXXXX)
        trap '\''rm -f -- "$temporary"'\'' EXIT HUP INT TERM
        cat > "$temporary"
        chmod 0755 "$temporary"
        chown root:root "$temporary"
        mv -f -- "$temporary" /usr/local/bin/paseo-check
        trap - EXIT HUP INT TERM
      '
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$YARD_NAME" ] && [ -n "${SUBYARD_CONFIG_HOME:-}" ] \
    && [ -f "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" ]; then
    # This fixture shares the disposable VM's platform pool. A second registered local yard makes
    # teardown retain shared infrastructure while still deleting this fixture's instance/project.
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64999\n' \
      > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    yard teardown --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-paseo-dependency.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-paseo-dependency-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-paseo-dependency.XXXXXX)"
printf '%s\n' subyard-paseo-dependency-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="paseo-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME"

# The candidate source is dirty, but the Paseo deploy artifact is release-owned.
YARD_BUILD_VERSION=0.7.0 "$ROOT/dev/build-engine.sh" --force
port=$((33000 + ($$ % 20000)))
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"
printf 'SSH_PORT=%s\nAGENTS=paseo\nFORWARD_SSH_AGENT=0\n' "$port" \
  > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
yard init --yes
yard start --yes
stage_candidate_check

effective="$(yard config show CODING_TOOL_INTEGRATIONS | sed -n 's/^effective: //p')"
[ "$effective" = 'codex paseo' ] \
  || die "effective agents are '$effective', expected 'codex paseo'"

project="subyard-$YARD_NAME"
instance="yard-$YARD_NAME"
assert_guest_ready() {
  incus exec "$instance" --project "$project" --user 1000 --group 1000 \
    --env HOME=/home/dev --env CODEX_HOME=/home/dev/.codex -- sh -euc '
      [ "$(codex --version)" = "codex-cli 0.147.0" ]
      [ "$(paseo --version)" = "0.2.1" ]
      codex-check >/dev/null
      [ ! -e /home/dev/.codex/auth.json ]
    '
  if ! incus exec "$instance" --project "$project" --user 1000 --group 1000 \
    --env HOME=/home/dev --env CODEX_HOME=/home/dev/.codex -- paseo-check >/dev/null; then
    incus exec "$instance" --project "$project" -- \
      systemctl --no-pager --full status paseo.service >&2 || true
    incus exec "$instance" --project "$project" -- \
      journalctl --no-pager -u paseo.service -n 120 >&2 || true
    die 'Paseo readiness failed after yard start'
  fi
}
assert_guest_ready

first_invocation="$(incus exec "$instance" --project "$project" -- \
  systemctl show paseo.service -p InvocationID --value)"
yard init --yes
yard start --yes
stage_candidate_check
assert_guest_ready
second_invocation="$(incus exec "$instance" --project "$project" -- \
  systemctl show paseo.service -p InvocationID --value)"
[ -n "$first_invocation" ] && [ -n "$second_invocation" ] \
  || die 'Paseo service invocation identity is unavailable'

printf 'ok: clean Paseo dependency/readiness lifecycle (%s -> %s)\n' \
  "$first_invocation" "$second_invocation"

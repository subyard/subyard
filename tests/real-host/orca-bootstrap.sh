#!/usr/bin/env bash
# Fresh `yard orca up` bootstrap against real Incus and stock Orca. Disposable E2E VM only.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=config/profiles/orca/release.env
. "$ROOT/config/profiles/orca/release.env"

STATE=''
PROJECT=''
INSTANCE=''
COLLISION_PID=''
ORIGINAL_HOME="$HOME"
YARD_BIN="$ROOT/.build/yard"
EXISTING_YARD="${SUBYARD_E2E_ORCA_EXISTING_YARD:-0}"
INSTALLED_RELEASE=''

stage() { printf 'orca-bootstrap-e2e: %s\n' "$*" >&2; }
die() { printf 'orca-bootstrap-e2e: %s\n' "$*" >&2; exit 1; }

[ "${SUBYARD_E2E_ORCA_BOOTSTRAP:-}" = 1 ] \
  || die 'set SUBYARD_E2E_ORCA_BOOTSTRAP=1 inside a disposable test host'
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
case "$EXISTING_YARD" in
  0|1) ;;
  *) die 'SUBYARD_E2E_ORCA_EXISTING_YARD must be 0 or 1' ;;
esac
for command in git go incus jq python3 rg script sudo timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
platform_marker="$ORIGINAL_HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker"

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() { timeout --signal=TERM --kill-after=5s 900 "$YARD_BIN" "$@"; }
guest_root() { incus --project "$PROJECT" exec "$INSTANCE" -- "$@"; }

cleanup() {
  local rc=$? cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  if [ -n "$COLLISION_PID" ] && kill -0 "$COLLISION_PID" 2>/dev/null; then
    kill -TERM "$COLLISION_PID" 2>/dev/null || true
    wait "$COLLISION_PID" 2>/dev/null || true
  fi
  if [ -f "${SUBYARD_CONFIG_HOME:-}/yards/default/config.env" ]; then
    yard teardown --yes >/dev/null 2>&1 || { rc=3; cleanup_failed=1; }
  fi
  if [ "$cleanup_failed" = 0 ] && [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-orca-bootstrap.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

setting_value() {
  yard config show "$1" | awk -F': ' '$1 == "effective" {print $2}'
}

release_ready() {
  local phase="$1" output error
  output="$STATE/release-$phase.json"
  error="$STATE/release-$phase.err"
  yard update --check --offline --version "$release_version" >"$output" 2>"$error" || return 1
  jq -s -e --arg target "$INSTALLED_RELEASE" '
    map(select(type == "object") | (.inspection // .)) |
    map(select(has("outcome") and has("assessment"))) as $reports |
    ($reports | length) == 1 and
    $reports[0].outcome.status == "ready" and
    $reports[0].outcome.reachedGoal == true and
    $reports[0].outcome.active == $target and
    $reports[0].outcome.target == $target and
    $reports[0].assessment.changed == false and
    (($reports[0].blockers // []) | length) == 0
  ' "$output" >/dev/null
}

print_release_state() {
  local phase="$1" output
  output="$STATE/release-$phase.json"
  if [ -f "$output" ]; then
    jq -s -r '
      map(select(type == "object") | (.inspection // .)) |
      map(select(has("outcome") and has("assessment"))) as $reports |
      if ($reports | length) == 1 then
        $reports[0] |
        "release-readiness: status=\(.outcome.status // "unavailable") " +
        "code=\(.outcome.code // "unavailable") " +
        "active=\(.outcome.active // "unavailable") " +
        "target=\(.outcome.target // "unavailable") " +
        "changed=\(if (.assessment | has("changed")) then .assessment.changed else "unavailable" end) " +
        "blockers=\((.blockers // []) | length)"
      else
        "release-readiness: reports=\($reports | length)"
      end
    ' "$output" >&2 || true
  fi
}

compare_json_managed_projection() {
  local source="$1" destination="$2" label="$3" diagnostics="${4:-0}"
  guest_root python3 -c '
import json
import re
import sys

safe = re.compile(r"^[A-Za-z0-9_.-]{1,64}$")
reported = 0
mismatch = False

def name(path):
    return ".".join(part if safe.fullmatch(part) else "<field>" for part in path) or "<root>"

def report(kind, path):
    global reported
    if reported < 64:
        print(f"materialized-json-{kind}:{name(path)}")
    reported += 1

def compare(expected, actual, path=()):
    global mismatch
    if isinstance(expected, dict):
        if not isinstance(actual, dict):
            report("managed-mismatch", path)
            mismatch = True
            return
        for key, value in expected.items():
            if key not in actual:
                report("managed-missing", path + (key,))
                mismatch = True
            else:
                compare(value, actual[key], path + (key,))
        if sys.argv[2] == "1":
            for key in actual.keys() - expected.keys():
                report("runtime-field", path + (key,))
    elif expected != actual:
        report("managed-mismatch", path)
        mismatch = True

try:
    expected = json.load(sys.stdin)
    with open(sys.argv[1], encoding="utf-8") as handle:
        actual = json.load(handle)
    compare(expected, actual)
except (OSError, UnicodeError, json.JSONDecodeError):
    print("materialized-json-state:unavailable-or-invalid")
    mismatch = True
sys.exit(1 if mismatch else 0)
' "$destination" "$diagnostics" <"$source" | sed "s|^|$label:|" >&2
}

assert_materialized_json() {
  local asset destination
  for asset in claude/settings.json pi/settings.json; do
    case "$asset" in
      claude/*) destination=/home/dev/.claude/settings.json ;;
      pi/*) destination=/home/dev/.pi/agent/settings.json ;;
    esac
    compare_json_managed_projection "$ROOT/config/agents/$asset" "$destination" "$asset" \
      || return 1
  done
}

print_materialized_json_diagnostics() {
  local asset destination
  for asset in claude/settings.json pi/settings.json; do
    case "$asset" in
      claude/*) destination=/home/dev/.claude/settings.json ;;
      pi/*) destination=/home/dev/.pi/agent/settings.json ;;
    esac
    compare_json_managed_projection "$ROOT/config/agents/$asset" "$destination" "$asset" 1 \
      || true
  done
}

client_status() {
	local pairing="$1" profile="$2" output
	output="$STATE/$profile-status.json"
  install -d -m 0700 \
    "$STATE/$profile/home" "$STATE/$profile/config" "$STATE/$profile/data" "$STATE/$profile/state"
  HOME="$STATE/$profile/home" \
    XDG_CONFIG_HOME="$STATE/$profile/config" \
    XDG_DATA_HOME="$STATE/$profile/data" \
    XDG_STATE_HOME="$STATE/$profile/state" \
    LIBGL_ALWAYS_SOFTWARE=1 \
    timeout 30 /usr/bin/orca-ide --pairing-code "$pairing" status --json \
    >"$output" 2>"$STATE/$profile-status.err" \
    || die 'stock Orca client could not use the private pairing link'
  jq -e '.ok == true and .result.runtime.reachable == true' "$output" >/dev/null \
    || die 'stock Orca client did not reach the bootstrapped runtime'
}

STATE="$(mktemp -d /var/tmp/subyard-orca-bootstrap.XXXXXX)"
printf '%s\n' subyard-orca-bootstrap-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
PROJECT="subyard-orca-bootstrap-$token"
INSTANCE="yard-orca-bootstrap-$token"
SSH_PORT="$(free_port)"

bash "$ROOT/dev/build-engine.sh" >/dev/null
# Keep the disposable VM's prepared toolchain caches when isolating the operator home.
GOMODCACHE="$(go env GOMODCACHE)"
GOCACHE="$(go env GOCACHE)"
export GOMODCACHE GOCACHE
install -d -m 0700 "$STATE/home" "$STATE/config/yards/default" "$STATE/data" "$STATE/host"
export HOME="$STATE/home"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$ORIGINAL_HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
coding_integrations=''
yard_profiles=subyard-dev
if [ "$EXISTING_YARD" = 1 ]; then
  coding_integrations='claude pi'
  yard_profiles='subyard-dev orca'
fi
cat > "$SUBYARD_CONFIG_HOME/config.env" <<EOF
SSH_PORT=$SSH_PORT
INCUS_PROJECT=$PROJECT
YARD_INSTANCE_NAME=$INSTANCE
CODING_TOOL_INTEGRATIONS="$coding_integrations"
ORCA_ADVERTISE_HOST=127.0.0.1
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
printf 'ENVIRONMENT_PROFILES=%q\n' "$yard_profiles" \
  > "$SUBYARD_CONFIG_HOME/yards/default/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env" \
  "$SUBYARD_CONFIG_HOME/yards/default/config.env"

stage 'installing a packaged candidate through the public release installer'
# The VM receives public source without Git metadata. Give packaging its normal
# tracked-file allowlist in a disposable copy, without altering the checkout.
bash "$ROOT/tests/helpers/source-files.sh" >"$STATE/source-files"
mkdir "$STATE/source"
tar -C "$ROOT" --null -T "$STATE/source-files" -cf - | tar -C "$STATE/source" -xf -
git -C "$STATE/source" init --quiet
git -C "$STATE/source" add --all
release_version=0.13.3-orca-bootstrap-e2e
bash "$STATE/source/dev/package-engine.sh" --version "$release_version" \
  --output-dir "$STATE/release" >/dev/null
YARD_RELEASE_BASE_URL="file://$STATE/release" YARD_RELEASE_VERSION="$release_version" \
  YARD_BIN_DIR="$HOME/.local/bin" SHELL=/bin/bash \
  "$STATE/release/subyard-install.sh" --yes >/dev/null
YARD_BIN="$HOME/.local/bin/yard"
[ "$(yard --version)" = "yard $release_version" ] || die 'packaged candidate is not active'
installed_target="$(readlink "$SUBYARD_HOME/runtime/current")"
case "$installed_target" in
  releases/*) ;;
  *) die 'packaged candidate current link is not a canonical release target' ;;
esac
INSTALLED_RELEASE="${installed_target#releases/}"
[ -n "$INSTALLED_RELEASE" ] && [ -d "$SUBYARD_HOME/runtime/releases/$INSTALLED_RELEASE" ] \
  || die 'packaged candidate release directory is unavailable'
stage 'checking the installed release before Orca bootstrap'
yard update --check --offline --version "$release_version"

if [ "$EXISTING_YARD" = 1 ]; then
  stage 'initializing an existing yard with Orca and materialized agent configs selected'
  yard init --yes
  stage 'completing and verifying the public release transition before Orca startup'
  yard update --offline --version "$release_version" --yes \
    >"$STATE/release-before-up.out" 2>"$STATE/release-before-up.err" \
    || die 'public release update did not complete before Orca startup'
  if ! release_ready before-up; then
    print_release_state before-up
    die 'release was not ready before Orca startup'
  fi
  assert_materialized_json \
    || die 'existing yard agent configuration was not materialized before Orca startup'
fi

stage 'holding the preferred owner port to exercise automatic collision selection'
python3 -m http.server 6768 --bind 127.0.0.1 \
  >"$STATE/collision.out" 2>"$STATE/collision.err" &
COLLISION_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$COLLISION_PID" 2>/dev/null || {
    sed -n '1,40p' "$STATE/collision.err" >&2
    die 'preferred-port collision listener exited'
  }
  python3 -c 'import socket; s=socket.create_connection(("127.0.0.1", 6768), 0.2); s.close()' \
    >/dev/null 2>&1 && break
  sleep 0.1
done
python3 -c 'import socket; s=socket.create_connection(("127.0.0.1", 6768), 0.2); s.close()' \
  >/dev/null 2>&1 || die 'preferred-port collision listener did not become ready'

if [ "$EXISTING_YARD" = 1 ]; then
  stage 'starting Orca interactively in an initialized release-ready yard'
else
  stage 'bootstrapping an uninitialized yard interactively with one confirmation'
fi
export SUBYARD_TTY_TEST_ENGINE="$YARD_BIN"
# A real controlling terminal catches background-handler stops that --yes and
# redirected stdin hide. Enter accepts the normal top-level default-yes prompt.
env -u ASSUME_YES timeout --signal=TERM --kill-after=5s 1800 \
  script --quiet --return --command 'exec "$SUBYARD_TTY_TEST_ENGINE" orca up' /dev/null \
  <<< '' >"$STATE/bootstrap-terminal.out" 2>&1 \
  || { tail -n 60 "$STATE/bootstrap-terminal.out" >&2; die 'interactive Orca bootstrap failed'; }
[ "$(grep -Fc 'Proceed? [Y/n]' "$STATE/bootstrap-terminal.out")" = 1 ] \
  || die 'interactive Orca bootstrap did not use exactly one default-yes confirmation'
status_reached=0
timeout --signal=TERM --kill-after=5s 60 \
  script --quiet --return --command 'exec "$SUBYARD_TTY_TEST_ENGINE" orca status' /dev/null \
  >"$STATE/status-terminal.out" 2>&1 \
  && grep -Fq 'Orca profile selected for yard init' "$STATE/status-terminal.out" \
  && status_reached=1
stage 'checking release convergence after Orca bootstrap'
if [ "$EXISTING_YARD" = 1 ]; then
  if ! release_ready after-up; then
    print_release_state after-up
    print_materialized_json_diagnostics
    if [ "$status_reached" != 1 ]; then
      sed -n '1,40p' "$STATE/status-terminal.out" >&2
    fi
    die 'release was not ready after Orca startup'
  fi
  assert_materialized_json || die 'Orca startup changed a Subyard-managed JSON field'
else
  yard update --check --offline --version "$release_version"
fi
[ "$status_reached" = 1 ] \
  || { sed -n '1,40p' "$STATE/status-terminal.out" >&2; die 'interactive status did not reach the Orca resource'; }
profiles="$(setting_value ENVIRONMENT_PROFILES)"
[ "$profiles" = 'subyard-dev orca' ] \
  || die "Orca bootstrap did not preserve the selected profiles: $profiles"
[ "$(setting_value ORCA_ADVERTISE_HOST)" = 127.0.0.1 ] \
  || die 'fresh Orca bootstrap did not retain the explicit loopback endpoint'
ORCA_PORT="$(setting_value ORCA_HOST_PORT)"
case "$ORCA_PORT" in
  ''|*[!0-9]*) die 'fresh Orca bootstrap did not select a numeric endpoint port' ;;
esac
[ "$ORCA_PORT" != 6768 ] || die 'automatic endpoint selection reused the occupied preferred port'
guest_root systemctl is-active --quiet subyard-orca.service \
  || die 'Orca did not start through the production bootstrap path'
guest_root test -x /usr/local/libexec/subyard/projects-changed \
  || die 'Orca bootstrap did not run the existing init reconciler'
[ "$(incus --project "$PROJECT" config device get "$INSTANCE" orca-server listen)" = \
  "tcp:127.0.0.1:$ORCA_PORT" ] || die 'Orca bootstrap published the wrong owner endpoint'
if [ ! -f "$platform_marker" ]; then
  incus info >/dev/null
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
  install -d -m 0700 "${platform_marker%/*}"
  printf '%s\n' subyard-e2e-platform-v1 > "$platform_marker"
  chmod 0600 "$platform_marker"
fi
[ "$(<"$platform_marker")" = subyard-e2e-platform-v1 ] \
  || die 'unexpected shared E2E platform marker'

kill -TERM "$COLLISION_PID"
wait "$COLLISION_PID" 2>/dev/null || true
COLLISION_PID=''

stage 'installing a stock Orca client without exposing its pairing capability'
case "$(dpkg --print-architecture)" in
  amd64) url="$ORCA_DEB_AMD64_URL"; digest="$ORCA_DEB_AMD64_SHA256" ;;
  arm64) url="$ORCA_DEB_ARM64_URL"; digest="$ORCA_DEB_ARM64_SHA256" ;;
  *) die 'unsupported architecture' ;;
esac
artifact="$STATE/orca.deb"
cache="/var/tmp/subyard-orca-$ORCA_VERSION-$digest.deb"
if printf '%s  %s\n' "$digest" "$cache" | sha256sum -c --status 2>/dev/null; then
  cp "$cache" "$artifact"
else
  curl --proto '=https' --tlsv1.2 -fsSL \
    --retry 3 --retry-all-errors --connect-timeout 20 --max-time 1200 \
    "$url" -o "$artifact"
  printf '%s  %s\n' "$digest" "$artifact" | sha256sum -c - >/dev/null
  cp "$artifact" "$cache"
fi
sudo -n apt-get update -qq
sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  "$artifact" file jq nftables zlib1g-dev \
  libasound2t64 libgbm1 libgtk-3-0t64 libnss3 >/dev/null
test -x /usr/bin/orca-ide || die 'stock Orca client is unavailable'

pairing="$(yard orca pair --yes | tail -n1)"
case "$pairing" in
  orca://pair\?code=*) ;;
  *) die 'Orca pair did not return a private stock pairing link' ;;
esac
client_status "$pairing" paired-client

stage 'preserving the selected endpoint and grant across restart and down/up'
yard orca restart --yes >/dev/null
client_status "$pairing" paired-client
yard orca down --yes >/dev/null
yard orca up --yes >/dev/null
[ "$(setting_value ORCA_HOST_PORT)" = "$ORCA_PORT" ] \
  || die 'down/up changed the saved endpoint port'
[ "$(setting_value ORCA_ADVERTISE_HOST)" = 127.0.0.1 ] \
  || die 'down/up changed the explicit loopback endpoint'
[ "$(setting_value ENVIRONMENT_PROFILES)" = 'subyard-dev orca' ] \
  || die 'down/up lost the selected Orca profile'
[ "$(incus --project "$PROJECT" config device get "$INSTANCE" orca-server listen)" = \
  "tcp:127.0.0.1:$ORCA_PORT" ] || die 'down/up changed the published owner endpoint'
client_status "$pairing" paired-client

printf 'ok: fresh Orca bootstrap reconciled the yard and preserved its endpoint and grant\n'

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

stage() { printf 'orca-bootstrap-e2e: %s\n' "$*" >&2; }
die() { printf 'orca-bootstrap-e2e: %s\n' "$*" >&2; exit 1; }

[ "${SUBYARD_E2E_ORCA_BOOTSTRAP:-}" = 1 ] \
  || die 'set SUBYARD_E2E_ORCA_BOOTSTRAP=1 inside a disposable test host'
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
for command in go incus jq python3 sudo; do
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
yard() { "$ROOT/.build/yard" "$@"; }
guest_root() { incus --project "$PROJECT" exec "$INSTANCE" -- "$@"; }

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$COLLISION_PID" ] && kill -0 "$COLLISION_PID" 2>/dev/null; then
    kill -TERM "$COLLISION_PID" 2>/dev/null || true
    wait "$COLLISION_PID" 2>/dev/null || true
  fi
  if [ -f "${SUBYARD_CONFIG_HOME:-}/yards/default/config.env" ]; then
    yard teardown --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-orca-bootstrap.* ]] \
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
install -d -m 0700 "$STATE/home" "$STATE/config/yards/default" "$STATE/data" "$STATE/host"
export HOME="$STATE/home"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$ORIGINAL_HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
cat > "$SUBYARD_CONFIG_HOME/config.env" <<EOF
SSH_PORT=$SSH_PORT
INCUS_PROJECT=$PROJECT
YARD_INSTANCE_NAME=$INSTANCE
CODING_TOOL_INTEGRATIONS=
ORCA_ADVERTISE_HOST=127.0.0.1
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
cat > "$SUBYARD_CONFIG_HOME/yards/default/config.env" <<'EOF'
ENVIRONMENT_PROFILES=subyard-dev
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env" \
  "$SUBYARD_CONFIG_HOME/yards/default/config.env"

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

stage 'bootstrapping an uninitialized yard and unselected Orca profile with one command'
yard orca up --yes >/dev/null
profiles="$(setting_value ENVIRONMENT_PROFILES)"
[ "$profiles" = 'subyard-dev orca' ] \
  || die "fresh Orca bootstrap did not preserve the existing profile: $profiles"
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

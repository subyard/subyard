#!/usr/bin/env bash
# Opt-in RPC framing contract through a real ephemeral loopback OpenSSH server.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SSHD="${SUBYARD_REAL_SSHD:-/usr/sbin/sshd}"
[ -x "$SSHD" ] || { printf 'ssh-rpc: sshd is unavailable: %s\n' "$SSHD" >&2; exit 2; }
for tool in ssh ssh-keygen ssh-keyscan jq od dd; do
  command -v "$tool" >/dev/null 2>&1 || { printf 'ssh-rpc: %s is required\n' "$tool" >&2; exit 2; }
done
command -v go >/dev/null 2>&1 || { printf 'ssh-rpc: Go is required to build the acceptance engine\n' >&2; exit 2; }
"$ROOT/dev/build-engine.sh" >/dev/null
ENGINE="$ROOT/.build/yard"

TMP="$(mktemp -d /tmp/subyard-ssh-rpc.XXXXXX)"
sshd_pid=''
occupied_sshd_pid=''
cleanup() {
  if [ -n "$sshd_pid" ] && kill -0 "$sshd_pid" 2>/dev/null; then
    kill -TERM "$sshd_pid" 2>/dev/null || true
    wait "$sshd_pid" 2>/dev/null || true
  fi
  if [ -n "$occupied_sshd_pid" ] && kill -0 "$occupied_sshd_pid" 2>/dev/null; then
    kill -TERM "$occupied_sshd_pid" 2>/dev/null || true
    wait "$occupied_sshd_pid" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT
fail() { printf 'ssh-rpc: %s\n' "$*" >&2; exit 1; }
# shellcheck source=tests/helpers/loopback-sshd.sh
. "$ROOT/tests/helpers/loopback-sshd.sh"

ssh-keygen -q -t ed25519 -N '' -f "$TMP/host-key"
ssh-keygen -q -t ed25519 -N '' -f "$TMP/client-key"
user="$(id -un)"
port=$((42000 + RANDOM % 10000))
remote_home="$TMP/remote-home"
remote_config="$TMP/remote-config"
install -d -m 0700 "$remote_home" "$remote_config/yards/fixture"
printf 'SSH_PORT=3333\n' > "$remote_config/yards/fixture/config.env"
chmod 0600 "$remote_config/yards/fixture/config.env"
public_key="$(cat "$TMP/client-key.pub")"
printf 'restrict,command="env HOME=%s SUBYARD_OPERATOR_HOME=%s SUBYARD_CONFIG_HOME=%s SUBYARD_NO_AUDIT=1 %s -Y fixture rpc --stdio" %s\n' \
  "$remote_home" "$remote_home" "$remote_config" "$ENGINE" "$public_key" > "$TMP/authorized_keys"
chmod 0600 "$TMP/authorized_keys"

cat > "$TMP/sshd_config" <<EOF
Port $port
ListenAddress 127.0.0.1
HostKey $TMP/host-key
PidFile $TMP/sshd.pid
AuthorizedKeysFile $TMP/authorized_keys
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
UsePAM no
PermitRootLogin no
AllowUsers $user
LogLevel VERBOSE
EOF

"$SSHD" -t -f "$TMP/sshd_config"
occupied="$TMP/occupied"
mkdir -p "$occupied"
ssh-keygen -q -t ed25519 -N '' -f "$occupied/host-key"
cat > "$occupied/sshd_config" <<EOF
ListenAddress 127.0.0.1
HostKey $occupied/host-key
PidFile $occupied/sshd.pid
AuthorizedKeysFile none
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
AllowUsers $user
EOF
start_loopback_sshd "$occupied" "$SSHD" "$port" \
  || fail 'collision fixture sshd did not become ready'
occupied_port="$port"
occupied_sshd_pid="$sshd_pid"
sshd_pid=''
start_loopback_sshd "$TMP" "$SSHD" "$occupied_port" \
  || fail 'ephemeral sshd did not become ready'
if [ "$port" = "$occupied_port" ] || ! kill -0 "$occupied_sshd_pid" 2>/dev/null; then
  fail 'ephemeral sshd did not preserve a foreign listener while retrying'
fi

append_frame() { # json output
  local payload="$1" output="$2" hex
  hex="$(printf '%08x' "${#payload}")"
  {
    printf '%b' "\\x${hex:0:2}\\x${hex:2:2}\\x${hex:4:2}\\x${hex:6:2}"
    printf '%s' "$payload"
  } >> "$output"
}
request="$TMP/request"
append_frame '{"version":1,"type":"request","id":"negotiate","method":"rpc.negotiate"}' "$request"
append_frame '{"version":1,"type":"request","id":"settings","method":"settings.list"}' "$request"

ssh -F /dev/null -T -p "$port" -i "$TMP/client-key" \
  -o BatchMode=yes -o IdentitiesOnly=yes -o UserKnownHostsFile="$TMP/known_hosts" \
  -o StrictHostKeyChecking=yes -o LogLevel=ERROR \
  "$user@127.0.0.1" -- yard rpc --stdio < "$request" > "$TMP/response"

offset=0
for index in 1 2; do
  header="$(dd if="$TMP/response" bs=1 skip="$offset" count=4 status=none | od -An -tx1 | tr -d ' \n')"
  [ "${#header}" -eq 8 ] || fail "response frame $index has no complete header"
  size=$((16#$header))
  dd if="$TMP/response" bs=1 skip=$((offset + 4)) count="$size" status=none > "$TMP/frame-$index.json"
  [ "$(stat -c '%s' "$TMP/frame-$index.json")" -eq "$size" ] || fail "response frame $index is truncated"
  offset=$((offset + 4 + size))
done
[ "$offset" -eq "$(stat -c '%s' "$TMP/response")" ] || fail 'SSH RPC returned an unexpected extra frame'
jq -e '.id=="negotiate" and .result.version==1 and .error==null' "$TMP/frame-1.json" >/dev/null \
  || fail 'SSH negotiation response is invalid'
jq -e '.type=="response" and .id=="settings" and .error==null and
  .result.yardName=="fixture" and
  any(.result.settings[]; .name=="SSH_PORT" and .value=="3333" and .valueAvailable==true)' \
  "$TMP/frame-2.json" >/dev/null || fail 'SSH native query did not use the selected persisted yard'

printf 'ok: real loopback OpenSSH handshake and selected-yard native settings RPC\n'

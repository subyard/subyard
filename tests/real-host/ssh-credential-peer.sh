#!/usr/bin/env bash
# Opt-in credential sync contract through a real ephemeral loopback OpenSSH server.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SSHD="${SUBYARD_REAL_SSHD:-/usr/sbin/sshd}"
TOOLS_DIR="${SUBYARD_REAL_KEYS_TOOLS_DIR:-}"
[ -x "$SSHD" ] || { printf 'ssh-credential-peer: sshd is unavailable: %s\n' "$SSHD" >&2; exit 2; }
case "$TOOLS_DIR" in /*) ;; *) printf 'ssh-credential-peer: set SUBYARD_REAL_KEYS_TOOLS_DIR\n' >&2; exit 2 ;; esac
for tool in age age-keygen sops; do
  [ -x "$TOOLS_DIR/bin/$tool" ] || { printf 'ssh-credential-peer: missing %s\n' "$tool" >&2; exit 2; }
done
for tool in ssh ssh-keygen ssh-keyscan jq git; do
  command -v "$tool" >/dev/null 2>&1 || { printf 'ssh-credential-peer: %s is required\n' "$tool" >&2; exit 2; }
done
command -v go >/dev/null 2>&1 || { printf 'ssh-credential-peer: Go is required\n' >&2; exit 2; }
"$ROOT/dev/build-engine.sh" >/dev/null

TMP="$(mktemp -d)"
sshd_pid=''
cleanup() {
  if [ -n "$sshd_pid" ] && kill -0 "$sshd_pid" 2>/dev/null; then
    kill -TERM "$sshd_pid" 2>/dev/null || true
    wait "$sshd_pid" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT
fail() { printf 'ssh-credential-peer: %s\n' "$*" >&2; exit 1; }

local_root="$TMP/local"
remote_root="$TMP/remote"
local_keys="$local_root/keys"
remote_keys="$remote_root/keys"
install -d -m 0700 \
  "$local_root/home" "$local_root/config" "$local_root/config/yards" \
  "$remote_root/home" "$remote_root/config"

bootstrap_keys() { # context-root keys-root
  HOME="$1/home" SUBYARD_OPERATOR_HOME="$1/home" SUBYARD_CONFIG_HOME="$1/config" \
    SUBYARD_HOME="$1/data" HOST_BASE="$1/host-data" RESTRICTED_DISK_PATHS="$1/host-data" \
    SUBYARD_KEYS_ROOT="$2" SUBYARD_KEYS_TOOLS_DIR="$TOOLS_DIR" \
    YARD_ENGINE_PATH="$ROOT/.build/yard" "$ROOT/bin/yard" _keys-init
}
bootstrap_keys "$local_root" "$local_keys" >/dev/null
bootstrap_keys "$remote_root" "$remote_keys" >/dev/null

ssh-keygen -q -t ed25519 -N '' -f "$TMP/host-key"
ssh-keygen -q -t ed25519 -N '' -f "$TMP/client-key"
user="$(id -un)"
port=$((42000 + RANDOM % 10000))
mkdir -p "$TMP/remote-bin" "$TMP/client-bin"
ln -s "$ROOT/.build/yard" "$TMP/remote-bin/yard"
cat > "$TMP/remote-bin/bash" <<'SH'
#!/bin/sh
if [ "${1:-}" = -lc ]; then shift; exec /bin/bash -c "${1:-}"; fi
exec /bin/bash "$@"
SH
chmod 0755 "$TMP/remote-bin/bash"
cat > "$TMP/remote-command" <<EOF
#!/usr/bin/env bash
set -euo pipefail
export HOME=$remote_root/home
export SUBYARD_OPERATOR_HOME=$remote_root/home
export SUBYARD_CONFIG_HOME=$remote_root/config
export SUBYARD_HOME=$remote_root/data
export HOST_BASE=$remote_root/host-data
export RESTRICTED_DISK_PATHS=$remote_root/host-data
export SUBYARD_KEYS_ROOT=$remote_keys
export SUBYARD_KEYS_TOOLS_DIR=$TOOLS_DIR
export SUBYARD_KEYS_CONSUMER_ROOT=$remote_root/consumer
export SUBYARD_REPOSITORY_ROOT=$ROOT
export SUBYARD_NO_AUDIT=1
export PATH=$TMP/remote-bin:/usr/bin:/bin
exec /bin/bash -c "\${SSH_ORIGINAL_COMMAND:?missing SSH command}"
EOF
chmod 0755 "$TMP/remote-command"
public_key="$(cat "$TMP/client-key.pub")"
printf 'restrict,command="%s" %s\n' "$TMP/remote-command" "$public_key" > "$TMP/authorized_keys"
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
"$SSHD" -D -e -f "$TMP/sshd_config" > "$TMP/sshd.log" 2>&1 &
sshd_pid=$!
for _ in $(seq 1 50); do
  kill -0 "$sshd_pid" 2>/dev/null || { sed -n '1,80p' "$TMP/sshd.log" >&2; fail 'ephemeral sshd exited'; }
  if ssh-keyscan -T 1 -p "$port" 127.0.0.1 > "$TMP/known_hosts" 2>/dev/null; then break; fi
  sleep 0.1
done
[ -s "$TMP/known_hosts" ] || fail 'ephemeral sshd did not become ready'

cat > "$TMP/ssh_config" <<EOF
Host peer-two
  HostName 127.0.0.1
  User $user
  Port $port
  IdentityFile $TMP/client-key
  IdentitiesOnly yes
  UserKnownHostsFile $TMP/known_hosts
  StrictHostKeyChecking yes
  LogLevel ERROR
EOF
cat > "$TMP/client-bin/ssh" <<EOF
#!/bin/sh
exec /usr/bin/ssh -F $TMP/ssh_config "\$@"
EOF
chmod 0755 "$TMP/client-bin/ssh"
cat > "$local_root/config/yards/remote-two.env" <<'EOF'
ACCESS_KIND=remote
OWNER_ENDPOINT=peer-two
SSH_PORT=3222
EOF

export HOME="$local_root/home"
export SUBYARD_OPERATOR_HOME="$local_root/home"
export SUBYARD_CONFIG_HOME="$local_root/config"
export SUBYARD_HOME="$local_root/data"
export HOST_BASE="$local_root/host-data"
export RESTRICTED_DISK_PATHS="$local_root/host-data"
export SUBYARD_KEYS_ROOT="$local_keys"
export SUBYARD_KEYS_TOOLS_DIR="$TOOLS_DIR"
export SUBYARD_KEYS_CONSUMER_ROOT="$local_root/consumer"
export SUBYARD_NO_AUDIT=1
export PATH="$TMP/client-bin:$PATH"

"$ROOT/.build/yard" keys trust @remote-two --yes >/dev/null
jq -e '.transport=="ssh" and .dest=="peer-two" and .trusted==true' \
  "$local_keys/peers/remote-two.json" >/dev/null || fail 'controller did not retain the real SSH route'
jq -e '.transport=="inbound" and .trusted==true' "$remote_keys/peers/default.json" >/dev/null \
  || fail 'remote owner did not retain reciprocal inbound trust'

expected="$TMP/expected"
printf 'subyard-synthetic-real-ssh-fixture\n' > "$expected"
chmod 0600 "$expected"
"$ROOT/.build/yard" keys add real-ssh --kind file --zone real-ssh --consumer staging-env \
  --file "$expected" --yes >/dev/null
credential="$("$ROOT/.build/yard" keys list | awk -F '\t' '$8=="real-ssh" {print $1}')"
[ -n "$credential" ] || fail 'synthetic SSH credential was not created'
"$ROOT/.build/yard" keys sync @remote-two --now --yes >/dev/null
ssh peer-two -- bash -lc "$(printf '%q' 'yard keys materialize real-ssh --yes')" >/dev/null
cmp -s "$expected" "$remote_root/consumer/staging/real-ssh.env" \
  || fail 'real SSH peer did not decrypt the synchronized credential'
if grep -R -F -q -- 'subyard-synthetic-real-ssh-fixture' "$local_keys" "$remote_keys"; then
  fail 'synthetic plaintext reached an SSH-synchronized ledger'
fi
"$ROOT/.build/yard" keys revoke "$credential" --yes >/dev/null
"$ROOT/.build/yard" keys sync @remote-two --now --yes >/dev/null
ssh peer-two -- bash -lc "$(printf '%q' 'yard keys materialize real-ssh --yes')" >/dev/null
[ ! -e "$remote_root/consumer/staging/real-ssh.env" ] \
  || fail 'revoked SSH credential remained materialized'

printf 'ok: real OpenSSH credential trust, sync, decrypt and revoke contract\n'

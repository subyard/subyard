#!/usr/bin/env bash
# Real-OpenSSH HostID lifecycle acceptance. Run on VM1 through dev/agent-e2e.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$ROOT"
RUN_ID="${SUBYARD_E2E_RUN_ID:?run through dev/agent-e2e.sh}"
[ "${SUBYARD_E2E_VM:?}" = 1 ] || { printf 'entity-naming-lifecycle: requires VM1\n' >&2; exit 2; }

fail() { printf 'entity-naming-lifecycle: %s\n' "$*" >&2; exit 1; }
REMOTE="/tmp/subyard-hostid-$RUN_ID"
LOCAL="$(mktemp -d "/tmp/subyard-hostid-controller-$RUN_ID.XXXXXX")"
PORT=$((24000 + 16#${RUN_ID:0:2} % 1000))
ADMIN_CONFIG="${SUBYARD_ENTITY_PEER_CONFIG:-}"
ADMIN_ALIAS="${SUBYARD_ENTITY_PEER_ALIAS:-e2e-vm-2}"
if [ -n "$ADMIN_CONFIG" ]; then
  REMOTE_HOST="${SUBYARD_ENTITY_PEER_IP:?peer IP is required with peer config}"
else
  REMOTE_HOST="$(getent ahostsv4 "$ADMIN_ALIAS" | awk '$2=="STREAM" {print $1; exit}')"
fi
[[ "$REMOTE_HOST" =~ ^[0-9a-fA-F:.]+$ ]] || fail 'cannot resolve VM2 address'

remote() {
  local argument quoted command=''
  for argument in "$@"; do
    printf -v quoted '%q' "$argument"
    command+="${command:+ }$quoted"
  done
  if [ -n "$ADMIN_CONFIG" ]; then
    ssh -F "$ADMIN_CONFIG" "$ADMIN_ALIAS" -- "$command"
  else
    ssh "$ADMIN_ALIAS" -- "$command"
  fi
}
remote_dev() {
  remote /usr/sbin/runuser -u dev -- bash -c 'cd "$1"; shift; exec "$@"' _ "$REMOTE" "$@"
}
copy_remote() {
  remote dd "of=$2" status=none < "$1"
}
cleanup() {
  remote bash -s -- "$REMOTE" <<'EOS' >/dev/null 2>&1 || true
root="$1"
if [ -r "$root/sshd.pid" ]; then kill "$(cat "$root/sshd.pid")" 2>/dev/null || true; fi
find "$root" -depth -delete 2>/dev/null || true
EOS
  find "$LOCAL" -depth -delete 2>/dev/null || true
}
trap cleanup EXIT

make -C "$ROOT" build >/dev/null
install -d -m 0700 "$LOCAL/ssh" "$LOCAL/bin"
ssh-keygen -q -t ed25519 -N '' -C "subyard-hostid-$RUN_ID" -f "$LOCAL/ssh/id_ed25519"
remote install -d -m 0700 "$REMOTE"
copy_remote "$ROOT/.build/yard" "$REMOTE/yard"
tar -C "$ROOT" -cf - config | remote tar -C "$REMOTE" -xf -
copy_remote "$LOCAL/ssh/id_ed25519.pub" "$REMOTE/controller.pub"

remote bash -s -- "$REMOTE" "$PORT" <<'EOS'
set -euo pipefail
root="$1" port="$2"
install -d -m 0700 "$root/home" "$root/config-home/yards" "$root/data" "$root/sshd"
chmod 0755 "$root/yard"
printf 'owner-a\n' > "$root/config-home/host-id"
chmod 0600 "$root/config-home/host-id"
printf 'SSH_PORT=3222\n' > "$root/config-home/yards/default.env"
chmod 0600 "$root/config-home/yards/default.env"
ssh-keygen -q -t ed25519 -N '' -C hostid-key-one -f "$root/sshd/key-one"
ssh-keygen -q -t ed25519 -N '' -C hostid-key-two -f "$root/sshd/key-two"
ssh-keygen -q -t ed25519 -N '' -C hostid-key-three -f "$root/sshd/key-three"
cat > "$root/yard-rpc" <<EOF
#!/usr/bin/env bash
export HOME='$root/home'
export SUBYARD_OPERATOR_HOME='$root/home'
export SUBYARD_CONFIG_DIR='$root/config'
export SUBYARD_CONFIG_HOME='$root/config-home'
export SUBYARD_HOME='$root/data'
export SUBYARD_HOST_ID=owner-a
export SUBYARD_REPOSITORY_ROOT='$root'
export PATH=/usr/local/bin:/usr/bin:/bin
cd '$root'
exec '$root/yard' rpc --stdio
EOF
chmod 0700 "$root/yard-rpc"
key="$(awk '{print $1" "$2}' "$root/controller.pub")"
printf 'restrict,command="%s" %s\n' "$root/yard-rpc" "$key" > "$root/authorized_keys"
chmod 0600 "$root/authorized_keys"
cat > "$root/sshd/config" <<EOF
Port $port
ListenAddress 0.0.0.0
HostKey $root/sshd/key-one
PidFile $root/sshd.pid
AuthorizedKeysFile $root/authorized_keys
StrictModes no
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
AllowUsers dev
LogLevel VERBOSE
EOF
chmod 0600 "$root/sshd/config"
chown -R dev:dev "$root"
nohup /usr/sbin/sshd -D -e -f "$root/sshd/config" >"$root/sshd.log" 2>&1 &
for _ in $(seq 1 50); do [ -s "$root/sshd.pid" ] && exit 0; sleep 0.1; done
cat "$root/sshd.log" >&2
exit 1
EOS

cat > "$LOCAL/ssh/config" <<EOF
Include ${ADMIN_CONFIG:-/home/dev/.ssh/config}
Host $REMOTE_HOST
    Port $PORT
    User dev
    IdentityFile $LOCAL/ssh/id_ed25519
    IdentitiesOnly yes
Host owner-alias
    HostName $REMOTE_HOST
    Port $PORT
    User dev
    IdentityFile $LOCAL/ssh/id_ed25519
    IdentitiesOnly yes
Host owner-proxy-jump
    HostName 127.0.0.1
    Port $PORT
    User dev
    IdentityFile $LOCAL/ssh/id_ed25519
    IdentitiesOnly yes
    ProxyJump $ADMIN_ALIAS
Host owner-proxy-command
    HostName 127.0.0.1
    Port $PORT
    User dev
    IdentityFile $LOCAL/ssh/id_ed25519
    IdentitiesOnly yes
    ProxyCommand /usr/bin/ssh -F ${ADMIN_CONFIG:-/home/dev/.ssh/config} $ADMIN_ALIAS -W %h:%p
EOF
chmod 0600 "$LOCAL/ssh/config"
cat > "$LOCAL/bin/ssh" <<EOF
#!/usr/bin/env bash
exec /usr/bin/ssh -F '$LOCAL/ssh/config' "\$@"
EOF
chmod 0700 "$LOCAL/bin/ssh"

yard_env() { # fixture-root command...
  local fixture="$1"; shift
  install -d -m 0700 "$fixture/home" "$fixture/config/yards" "$fixture/data"
  printf 'controller-%s\n' "$(basename "$fixture")" > "$fixture/config/host-id"
  chmod 0600 "$fixture/config/host-id"
  printf 'SSH_PORT=3223\n' > "$fixture/config/yards/default.env"
  env HOME="$fixture/home" SUBYARD_OPERATOR_HOME="$fixture/home" \
    SUBYARD_CONFIG_DIR="$ROOT/config" SUBYARD_CONFIG_HOME="$fixture/config" \
    SUBYARD_HOME="$fixture/data" PATH="$LOCAL/bin:/usr/local/bin:/usr/bin:/bin" \
    "$ROOT/.build/yard" "$@"
}

expected_fingerprint="$(remote ssh-keygen -lf "$REMOTE/sshd/key-one.pub" -E sha256 | awk '{print $2}')"
for case_name in direct alias proxy-jump proxy-command; do
  fixture="$LOCAL/$case_name"
  case "$case_name" in
    direct) endpoint="dev@$REMOTE_HOST" ;;
    alias) endpoint=owner-alias ;;
    proxy-jump) endpoint=owner-proxy-jump ;;
    proxy-command) endpoint=owner-proxy-command ;;
  esac
  output="$fixture.add.out"
  printf 'y\n' | yard_env "$fixture" host add "$endpoint" >"$output" 2>&1 \
    || { sed -n '1,120p' "$output" >&2; fail "$case_name host add failed"; }
  [ "$(grep -c '^Proceed?' "$output" || true)" = 1 ] || fail "$case_name did not ask exactly once"
  connection="$fixture/data/owner-inventory/connections/owner-a.json"
  cache="$fixture/data/owner-inventory/owners/owner-a.json"
  jq -e --arg endpoint "$endpoint" --arg fp "$expected_fingerprint" \
    '.hostId=="owner-a" and .destination==$endpoint and .trust.fingerprint==$fp' "$connection" >/dev/null \
    || fail "$case_name stored the wrong identity or fingerprint"
  jq -e '.inventory.hostId=="owner-a" and (.inventory.yards | length) >= 1' "$cache" >/dev/null \
    || fail "$case_name did not cache authoritative yards"
done

# Continue with the alias fixture so all lifecycle mutations share one controller transaction log.
FIXTURE="$LOCAL/alias"
ROUTING="$FIXTURE/data/owner-inventory/routing/owner-a/default/state"
install -d -m 0700 "$(dirname "$ROUTING")"
printf 'preserved\n' > "$ROUTING"
remote_dev env HOME="$REMOTE/home" SUBYARD_OPERATOR_HOME="$REMOTE/home" \
  SUBYARD_CONFIG_DIR="$REMOTE/config" SUBYARD_CONFIG_HOME="$REMOTE/config-home" \
  SUBYARD_HOME="$REMOTE/data" SUBYARD_REPOSITORY_ROOT="$REMOTE" \
  "$REMOTE/yard" host rename owner-b --yes >/dev/null
sleep 31
yard_env "$FIXTURE" yards --json >/dev/null || fail 'same-key automatic refresh failed'
[ -f "$FIXTURE/data/owner-inventory/connections/owner-b.json" ] \
  && [ -f "$FIXTURE/data/owner-inventory/routing/owner-b/default/state" ] \
  && [ ! -e "$FIXTURE/data/owner-inventory/connections/owner-a.json" ] \
  || fail 'same-key rename did not atomically move controller state'

remote bash -s -- "$REMOTE" <<'EOS'
set -euo pipefail
root="$1"
kill "$(cat "$root/sshd.pid")"
for _ in $(seq 1 50); do [ ! -e "$root/sshd.pid" ] && break; sleep 0.1; done
sed -i "s#HostKey $root/sshd/key-one#HostKey $root/sshd/key-two#" "$root/sshd/config"
nohup /usr/sbin/sshd -D -e -f "$root/sshd/config" >"$root/sshd.log" 2>&1 &
for _ in $(seq 1 50); do [ -s "$root/sshd.pid" ] && kill -0 "$(cat "$root/sshd.pid")" 2>/dev/null && exit 0; sleep 0.1; done
cat "$root/sshd.log" >&2; exit 1
EOS
sleep 31
before="$(find "$FIXTURE/data/owner-inventory" -type f ! -path '*/tmp/*' -print0 | sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}')"
if yard_env "$FIXTURE" yards --json >"$LOCAL/changed-key.out" 2>&1; then
  fail 'changed SSH key was accepted'
fi
grep -Fq "$expected_fingerprint" "$LOCAL/changed-key.out" \
  && grep -Fq 'yard host repair' "$LOCAL/changed-key.out" \
  || fail 'changed-key rejection omitted the pinned fingerprint or repair action'
after="$(find "$FIXTURE/data/owner-inventory" -type f ! -path '*/tmp/*' -print0 | sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}')"
[ "$before" = "$after" ] || fail 'changed-key rejection mutated controller state'

yard_env "$FIXTURE" host repair owner-b --yes >/dev/null || fail 'explicit key-only repair failed'
key_two_fingerprint="$(remote ssh-keygen -lf "$REMOTE/sshd/key-two.pub" -E sha256 | awk '{print $2}')"
jq -e --arg fp "$key_two_fingerprint" '.hostId=="owner-b" and .trust.fingerprint==$fp' \
  "$FIXTURE/data/owner-inventory/connections/owner-b.json" >/dev/null \
  || fail 'key-only repair changed HostID or stored the wrong key'
[ -f "$FIXTURE/data/owner-inventory/routing/owner-b/default/state" ] \
  || fail 'key-only repair lost routing state'

remote_dev env HOME="$REMOTE/home" SUBYARD_OPERATOR_HOME="$REMOTE/home" \
  SUBYARD_CONFIG_DIR="$REMOTE/config" SUBYARD_CONFIG_HOME="$REMOTE/config-home" \
  SUBYARD_HOME="$REMOTE/data" SUBYARD_REPOSITORY_ROOT="$REMOTE" \
  "$REMOTE/yard" host rename owner-c --yes >/dev/null
remote bash -s -- "$REMOTE" <<'EOS'
set -euo pipefail
root="$1"
kill "$(cat "$root/sshd.pid")"
for _ in $(seq 1 50); do [ ! -e "$root/sshd.pid" ] && break; sleep 0.1; done
sed -i "s#HostKey $root/sshd/key-two#HostKey $root/sshd/key-three#" "$root/sshd/config"
nohup /usr/sbin/sshd -D -e -f "$root/sshd/config" >"$root/sshd.log" 2>&1 &
for _ in $(seq 1 50); do [ -s "$root/sshd.pid" ] && kill -0 "$(cat "$root/sshd.pid")" 2>/dev/null && exit 0; sleep 0.1; done
cat "$root/sshd.log" >&2; exit 1
EOS
yard_env "$FIXTURE" host repair owner-b --yes >/dev/null || fail 'explicit key+HostID repair failed'
new_fingerprint="$(remote ssh-keygen -lf "$REMOTE/sshd/key-three.pub" -E sha256 | awk '{print $2}')"
jq -e --arg fp "$new_fingerprint" '.hostId=="owner-c" and .trust.fingerprint==$fp' \
  "$FIXTURE/data/owner-inventory/connections/owner-c.json" >/dev/null \
  || fail 'repair did not commit the new key and HostID'
[ -f "$FIXTURE/data/owner-inventory/routing/owner-c/default/state" ] \
  || fail 'repair lost routing state'

remote kill "$(remote cat "$REMOTE/sshd.pid")"
sleep 31
if yard_env "$FIXTURE" yards --json >"$LOCAL/offline.out" 2>&1; then
  fail 'offline owner reported a fresh successful refresh'
fi
grep -qi 'stale' "$LOCAL/offline.out" || fail 'offline owner omitted the stale marker'

# Routing project references guard removal even after the owner comes back.
remote bash -s -- "$REMOTE" <<'EOS'
set -euo pipefail
root="$1"
nohup /usr/sbin/sshd -D -e -f "$root/sshd/config" >"$root/sshd.log" 2>&1 &
for _ in $(seq 1 50); do [ -s "$root/sshd.pid" ] && kill -0 "$(cat "$root/sshd.pid")" 2>/dev/null && exit 0; sleep 0.1; done
exit 1
EOS
install -d -m 0700 "$FIXTURE/data/owner-inventory/routing/owner-c/default/projects"
printf 'guard\n' > "$FIXTURE/data/owner-inventory/routing/owner-c/default/projects/project-a"
if yard_env "$FIXTURE" host remove owner-c --yes >"$LOCAL/remove-guard.out" 2>&1; then
  fail 'host removal ignored controller project routing state'
fi
grep -qi 'project routing state' "$LOCAL/remove-guard.out" || fail 'remove guard was not actionable'
find "$FIXTURE/data/owner-inventory/routing/owner-c/default/projects" -depth -delete
yard_env "$FIXTURE" host remove owner-c --yes >/dev/null || fail 'guard-free host removal failed'
[ ! -e "$FIXTURE/data/owner-inventory/connections/owner-c.json" ] \
  && [ ! -e "$FIXTURE/data/owner-inventory/owners/owner-c.json" ] \
  && [ ! -e "$FIXTURE/data/owner-inventory/routing/owner-c" ] \
  || fail 'host removal left controller state'

printf 'ok: real OpenSSH direct, alias, proxy and full HostID lifecycle verified\n'

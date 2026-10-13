#!/usr/bin/env bash
# Real-OpenSSH HostID lifecycle acceptance. Run on VM1 through dev/agent-e2e.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=dev/e2e/lib-owner-inventory-fixture.sh
. "$ROOT/dev/e2e/lib-owner-inventory-fixture.sh"
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

VERSION=hostid-e2e
RELEASE="$LOCAL/release"
"$ROOT/dev/package-engine.sh" --output-dir "$RELEASE" --version "$VERSION" >/dev/null
runtime_bundle="$(find "$RELEASE" -maxdepth 1 -name 'subyard-*-linux-*.tar.gz' -print)"
[ -f "$runtime_bundle" ] || fail 'expected one current-worktree runtime bundle'
runtime_digest="$(sha256sum "$runtime_bundle" | cut -d' ' -f1)"
printf 'evidence: current_worktree_runtime_sha256=%s version=%s\n' "$runtime_digest" "$VERSION"
install -d -m 0700 "$LOCAL/ssh" "$LOCAL/bin"
ssh-keygen -q -t ed25519 -N '' -C "subyard-hostid-$RUN_ID" -f "$LOCAL/ssh/id_ed25519"
remote install -d -m 0700 "$REMOTE"
remote install -d -m 0700 "$REMOTE/release"
tar -C "$RELEASE" -cf - . | remote tar -C "$REMOTE/release" -xf -
copy_remote "$LOCAL/ssh/id_ed25519.pub" "$REMOTE/controller.pub"

remote bash -s -- "$REMOTE" "$PORT" "$VERSION" "$runtime_digest" <<'EOS'
set -euo pipefail
root="$1" port="$2" version="$3" digest="$4"
install -d -m 0700 "$root/home" "$root/config-home/yards" "$root/data" "$root/bin" "$root/sshd"
HOME="$root/home" SUBYARD_HOME="$root/data" SUBYARD_CONFIG_HOME="$root/config-home" \
  YARD_BIN_DIR="$root/bin" YARD_SHELL_RC="$root/home/.bashrc" YARD_LOGIN_RC="$root/home/.profile" \
  YARD_RELEASE_BASE_URL="file://$root/release" YARD_RELEASE_VERSION="$version" \
  "$root/release/subyard-install.sh" --yes >/dev/null
runtime="$(readlink -f "$root/bin/yard")"
[ "$runtime" = "$root/data/runtime/releases/$version-${digest:0:12}/bin/yard" ]
[ "$("$root/bin/yard" --version)" = "yard $version" ]
(cd "${runtime%/bin/yard}" && sha256sum -c runtime-files.sha256 >/dev/null)
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
export SUBYARD_CONFIG_HOME='$root/config-home'
export SUBYARD_HOME='$root/data'
export PATH=/usr/local/bin:/usr/bin:/bin
cd '$root'
exec '$root/bin/yard' rpc --stdio
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
  install -d -m 0700 "$fixture/home" "$fixture/config/yards" "$fixture/data" "$fixture/bin"
  if [ ! -x "$fixture/bin/yard" ]; then
    HOME="$fixture/home" SUBYARD_HOME="$fixture/data" SUBYARD_CONFIG_HOME="$fixture/config" \
      YARD_BIN_DIR="$fixture/bin" YARD_SHELL_RC="$fixture/home/.bashrc" YARD_LOGIN_RC="$fixture/home/.profile" \
      YARD_RELEASE_BASE_URL="file://$RELEASE" YARD_RELEASE_VERSION="$VERSION" \
      "$RELEASE/subyard-install.sh" --yes >/dev/null \
      || fail 'controller runtime installation failed'
    local runtime
    runtime="$(readlink -f "$fixture/bin/yard")"
    [ "$runtime" = "$fixture/data/runtime/releases/$VERSION-${runtime_digest:0:12}/bin/yard" ] \
      || fail 'controller launcher is outside the verified installed runtime'
    [ "$("$fixture/bin/yard" --version)" = "yard $VERSION" ] || fail 'controller installed version mismatch'
    (cd "${runtime%/bin/yard}" && sha256sum -c runtime-files.sha256 >/dev/null) \
      || fail 'controller installed runtime checksum mismatch'
  fi
  printf 'controller-%s\n' "$(basename "$fixture")" > "$fixture/config/host-id"
  chmod 0600 "$fixture/config/host-id"
  printf 'SSH_PORT=3223\n' > "$fixture/config/yards/default.env"
  env HOME="$fixture/home" SUBYARD_OPERATOR_HOME="$fixture/home" \
    SUBYARD_CONFIG_HOME="$fixture/config" \
    SUBYARD_HOME="$fixture/data" PATH="$LOCAL/bin:/usr/local/bin:/usr/bin:/bin" \
    "$fixture/bin/yard" "$@"
}

controller_state_hash() {
  local root="$1" owner="$2"
  (
    cd "$root/data/owner-inventory"
    [ -f "connections/$owner.json" ] && [ -f "owners/$owner.json" ] \
      && [ "$(cat "routing/$owner/default/projects/project-a")" = preserved ] \
      || fail 'controller hash is missing protected identity, inventory or project state'
    local -a files=()
    mapfile -d '' -t files < <(find . -path ./tmp -prune -o -type f -print0 | sort -z)
    [ "${#files[@]}" -ge 3 ] || fail 'controller hash has an empty or incomplete protected file set'
    sha256sum -- "${files[@]}" | sha256sum | awk '{print $1}'
  )
}

expected_fingerprint="$(remote ssh-keygen -lf "$REMOTE/sshd/key-one.pub" -E sha256 | awk '{print $2}')"
for case_name in direct alias proxy-jump proxy-command; do
  fixture="$LOCAL/$case_name"
  case "$case_name" in
    direct) endpoint="dev@$REMOTE_HOST" ;;
    alias) endpoint="owner-alias" ;;
    proxy-jump) endpoint="owner-proxy-jump" ;;
    proxy-command) endpoint="owner-proxy-command" ;;
  esac
  output="$fixture.add.out"
  yard_env "$fixture" host add "$endpoint" --yes >"$output" 2>&1 \
    || { sed -n '1,120p' "$output" >&2; fail "$case_name host add failed"; }
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
ROUTING="$FIXTURE/data/owner-inventory/routing/owner-a/default/projects/project-a"
install -d -m 0700 "$(dirname "$ROUTING")"
printf 'preserved\n' > "$ROUTING"
remote_dev env HOME="$REMOTE/home" SUBYARD_OPERATOR_HOME="$REMOTE/home" \
  SUBYARD_CONFIG_HOME="$REMOTE/config-home" SUBYARD_HOME="$REMOTE/data" \
  "$REMOTE/bin/yard" host rename owner-b --yes >/dev/null
expire_owner_inventory "$FIXTURE/data/owner-inventory/owners/owner-a.json" owner-a
yard_env "$FIXTURE" status >"$LOCAL/renamed-summary.out" 2>"$LOCAL/renamed-summary.err" \
  || fail 'same-key status refresh failed with expired cache'
grep -Fq 'owner-b/default' "$LOCAL/renamed-summary.out" \
  && ! grep -Fq 'owner-a/' "$LOCAL/renamed-summary.out" \
  && [ ! -s "$LOCAL/renamed-summary.err" ] || fail 'status omitted the renamed owner or reported a warning'
[ -f "$FIXTURE/data/owner-inventory/connections/owner-b.json" ] \
  && [ -f "$FIXTURE/data/owner-inventory/owners/owner-b.json" ] \
  && [ -f "$FIXTURE/data/owner-inventory/routing/owner-b/default/projects/project-a" ] \
  && [ ! -e "$FIXTURE/data/owner-inventory/connections/owner-a.json" ] \
  && [ ! -e "$FIXTURE/data/owner-inventory/owners/owner-a.json" ] \
  && [ ! -e "$FIXTURE/data/owner-inventory/routing/owner-a" ] \
  || fail 'same-key rename did not atomically move controller state'
before="$(controller_state_hash "$FIXTURE" owner-b)"
yard_env "$FIXTURE" status >"$LOCAL/fresh-summary.out" 2>"$LOCAL/fresh-summary.err" \
  || fail 'repeated status failed'
grep -Fq 'owner-b/default' "$LOCAL/fresh-summary.out" \
  && [ ! -s "$LOCAL/fresh-summary.err" ] || fail 'status omitted renamed owner or reported a warning'
after="$(controller_state_hash "$FIXTURE" owner-b)"
[ "$before" = "$after" ] || fail 'repeated status changed fresh controller metadata'

# The direct controller exercises first refresh without a cached old inventory.
MISSING="$LOCAL/direct"
find "$MISSING/data/owner-inventory/owners" -name 'owner-a.json' -delete
yard_env "$MISSING" status >"$LOCAL/missing-summary.out" 2>"$LOCAL/missing-summary.err" \
  || fail 'same-key status refresh failed with missing cache'
grep -Fq 'owner-b/default' "$LOCAL/missing-summary.out" \
  && [ ! -s "$LOCAL/missing-summary.err" ] \
  && [ -f "$MISSING/data/owner-inventory/connections/owner-b.json" ] \
  && [ ! -e "$MISSING/data/owner-inventory/connections/owner-a.json" ] \
  || fail 'missing-cache status did not adopt the authoritative owner'

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
expire_owner_inventory "$FIXTURE/data/owner-inventory/owners/owner-b.json" owner-b
before="$(controller_state_hash "$FIXTURE" owner-b)"
if yard_env "$FIXTURE" status >"$LOCAL/changed-key.out" 2>&1; then
  fail 'changed SSH key was accepted'
fi
grep -Fq "$expected_fingerprint" "$LOCAL/changed-key.out" \
  && grep -Fq 'yard host repair' "$LOCAL/changed-key.out" \
  || fail 'changed-key rejection omitted the pinned fingerprint or repair action'
after="$(controller_state_hash "$FIXTURE" owner-b)"
[ "$before" = "$after" ] || fail 'changed-key rejection mutated controller state'

yard_env "$FIXTURE" host repair owner-b --yes >/dev/null || fail 'explicit key-only repair failed'
key_two_fingerprint="$(remote ssh-keygen -lf "$REMOTE/sshd/key-two.pub" -E sha256 | awk '{print $2}')"
jq -e --arg fp "$key_two_fingerprint" '.hostId=="owner-b" and .trust.fingerprint==$fp' \
  "$FIXTURE/data/owner-inventory/connections/owner-b.json" >/dev/null \
  || fail 'key-only repair changed HostID or stored the wrong key'
[ -f "$FIXTURE/data/owner-inventory/routing/owner-b/default/projects/project-a" ] \
  || fail 'key-only repair lost routing state'

remote_dev env HOME="$REMOTE/home" SUBYARD_OPERATOR_HOME="$REMOTE/home" \
  SUBYARD_CONFIG_HOME="$REMOTE/config-home" SUBYARD_HOME="$REMOTE/data" \
  "$REMOTE/bin/yard" host rename owner-c --yes >/dev/null
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
[ -f "$FIXTURE/data/owner-inventory/routing/owner-c/default/projects/project-a" ] \
  || fail 'repair lost routing state'

remote kill "$(remote cat "$REMOTE/sshd.pid")"
expire_owner_inventory "$FIXTURE/data/owner-inventory/owners/owner-c.json" owner-c
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

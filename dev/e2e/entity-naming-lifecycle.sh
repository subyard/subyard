#!/usr/bin/env bash
# Real-OpenSSH HostID lifecycle acceptance. Run on VM1 through dev/agent-e2e.sh.
set -Eeuo pipefail
PHASE='fixture-setup'
report_failure() { printf 'entity-naming-lifecycle: phase=%s line=%s exit=%s\n' "$PHASE" "$2" "$1" >&2; }
trap 'report_failure "$?" "$LINENO"' ERR
phase() { PHASE="$1"; printf 'evidence: phase=%s\n' "$PHASE"; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=dev/e2e/lib-owner-inventory-fixture.sh
. "$ROOT/dev/e2e/lib-owner-inventory-fixture.sh"
cd "$ROOT"
RUN_ID="${SUBYARD_E2E_RUN_ID:?run through dev/agent-e2e.sh}"
[ "${SUBYARD_E2E_VM:?}" = 1 ] || { printf 'entity-naming-lifecycle: requires VM1\n' >&2; exit 2; }
# Isolated homes render real credential units; the account's user manager uses its real home.
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1

fail() { printf 'entity-naming-lifecycle: %s\n' "$*" >&2; exit 1; }
REMOTE="/tmp/subyard-hostid-$RUN_ID"
LOCAL="$(mktemp -d "/var/tmp/subyard-hostid-controller-$RUN_ID.XXXXXX")"
REMOTE_DATA=''
chmod 0700 "$LOCAL"
printf '%s\n' "$RUN_ID" > "$LOCAL/.fixture-run"
chmod 0600 "$LOCAL/.fixture-run"
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
  remote /usr/sbin/runuser -u dev -- env SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 \
    bash -c 'cd "$1" || exit; shift; exec "$@"' _ "$REMOTE" "$@"
}
copy_remote() {
  remote dd "of=$2" status=none < "$1"
}
cleanup() {
  if [ "$(remote_dev incus config get yard user.subyard.entity-fixture --project subyard 2>/dev/null || true)" = "$RUN_ID" ]; then
    remote_dev env HOME="$REMOTE/home" SUBYARD_OPERATOR_HOME="$REMOTE/home" \
      SUBYARD_CONFIG_HOME="$REMOTE/config-home" SUBYARD_HOME="$REMOTE_DATA" \
      "$REMOTE/bin/yard" teardown --yes >/dev/null 2>&1 || true
  fi
  remote bash -s -- "$REMOTE" "$REMOTE_DATA" "$RUN_ID" <<'EOS' >/dev/null 2>&1 || true
root="$1" data="$2" run="$3"
if [ "$root" = "/tmp/subyard-hostid-$run" ] && [ ! -L "$root" ] \
  && [ "$(cat "$root/.fixture-run" 2>/dev/null)" = "$run" ]; then
  if [ -r "$root/sshd.pid" ]; then kill "$(cat "$root/sshd.pid")" 2>/dev/null || true; fi
  find "$root" -depth -delete 2>/dev/null || true
fi
case "$data" in /var/tmp/subyard-hostid-owner-data-"$run".*)
  if [ ! -L "$data" ] && [ "$(cat "$data/.fixture-run" 2>/dev/null)" = "$run" ]; then
    find "$data" -depth -delete 2>/dev/null || true
  fi ;;
esac
EOS
  case "$LOCAL" in /var/tmp/subyard-hostid-controller-"$RUN_ID".*)
    if [ ! -L "$LOCAL" ] && [ "$(cat "$LOCAL/.fixture-run" 2>/dev/null)" = "$RUN_ID" ]; then
      find "$LOCAL" -depth -delete 2>/dev/null || true
    fi ;;
  esac
}
trap cleanup EXIT

# Runtime assets and storage use private persistent roots; Git fixtures stay /tmp.
# Match the product's new-yard minimum without weakening its later native preflight.
persistent_storage_preflight() {
  local path="$1" filesystem free_blocks block_bytes
  [ -d "$path" ] && [ ! -L "$path" ] && [ -O "$path" ] \
    && [ "$(realpath -e "$path")" = "$path" ] || return 1
  read -r filesystem free_blocks block_bytes < <(stat -f -c '%t %a %S' "$path")
  case "$filesystem" in 1021994|858458f6) return 1 ;; esac # tmpfs / ramfs
  [[ "$free_blocks" =~ ^[0-9]+$ && "$block_bytes" =~ ^[0-9]+$ ]] || return 1
  [ "$((free_blocks * block_bytes))" -ge "$((2 * 1024 * 1024 * 1024))" ] || return 1
  printf 'evidence: persistent fixture storage preflight passed fs=0x%s free_gib=%s\n' \
    "$filesystem" "$((free_blocks * block_bytes / 1024 / 1024 / 1024))"
}
persistent_storage_preflight "$LOCAL" || fail 'controller artifacts require persistent storage with at least 2 GiB free'
REMOTE_DATA="$(remote /usr/sbin/runuser -u dev -- bash -c 'cd /var/tmp && exec mktemp -d "$1"' _ "/var/tmp/subyard-hostid-owner-data-$RUN_ID.XXXXXX")"
# Run the actual filesystem and writable-root checks as the fixture execution user.
{ declare -f persistent_storage_preflight; cat <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
data="$1" root="$2" run="$3"
cd /var/tmp
chmod 0700 "$data"
printf '%s\n' "$run" > "$data/.fixture-run"
chmod 0600 "$data/.fixture-run"
persistent_storage_preflight "$data" || { printf 'entity-naming-lifecycle: owner data requires persistent storage with at least 2 GiB free\n' >&2; exit 1; }
[ ! -e "$root" ] && [ ! -L "$root" ]
install -d -m 0700 "$root"
printf '%s\n' "$run" > "$root/.fixture-run"
chmod 0600 "$root/.fixture-run"
EOS
} | remote /usr/sbin/runuser -u dev -- bash -s -- "$REMOTE_DATA" "$REMOTE" "$RUN_ID"

VERSION=hostid-e2e
phase package-runtime
RELEASE="$LOCAL/release"
"$ROOT/dev/package-engine.sh" --output-dir "$RELEASE" --version "$VERSION" >/dev/null
runtime_bundle="$(find "$RELEASE" -maxdepth 1 -name 'subyard-*-linux-*.tar.gz' -print)"
[ -f "$runtime_bundle" ] || fail 'expected one current-worktree runtime bundle'
runtime_digest="$(sha256sum "$runtime_bundle" | cut -d' ' -f1)"
printf 'evidence: current_worktree_runtime_sha256=%s version=%s\n' "$runtime_digest" "$VERSION"
install -d -m 0700 "$LOCAL/ssh" "$LOCAL/bin"
ssh-keygen -q -t ed25519 -N '' -C "subyard-hostid-$RUN_ID" -f "$LOCAL/ssh/id_ed25519"
remote install -d -m 0700 "$REMOTE"
remote install -d -m 0700 "$REMOTE_DATA/release"
tar -C "$RELEASE" -cf - . | remote tar -C "$REMOTE_DATA/release" -xf -
copy_remote "$LOCAL/ssh/id_ed25519.pub" "$REMOTE/controller.pub"

remote bash -s -- "$REMOTE" "$PORT" "$VERSION" "$runtime_digest" "$REMOTE_DATA" "$RUN_ID" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1" port="$2" version="$3" digest="$4" data="$5" run="$6"
[ "$root" = "/tmp/subyard-hostid-$run" ] && [ ! -L "$root" ] \
  && [ "$(realpath -e "$root")" = "$root" ] \
  && [ "$(cat "$root/.fixture-run")" = "$run" ]
case "$data" in /var/tmp/subyard-hostid-owner-data-"$run".*) ;; *) exit 1 ;; esac
[ ! -L "$data" ] && [ "$(realpath -e "$data")" = "$data" ] \
  && [ "$(cat "$data/.fixture-run")" = "$run" ]
install -d -m 0700 "$root/home" "$root/config-home/yards/default" "$data" "$root/bin" "$root/sshd"
# Release activation validates every runtime ancestor against its execution owner.
# Keep installation and all later owner commands under the same dev identity.
chown -R dev:dev "$root" "$data"
# runuser retains the admin SSH working directory, which dev cannot traverse.
cd "$root"
/usr/sbin/runuser -u dev -- bash -c '
  [ "$(id -u)" = "$2" ] && [ "$(pwd -P)" = "$1" ] && [ -r . ] && [ -x . ]
' _ "$root" "$(id -u dev)"
printf 'evidence: dev execution UID and traversable fixture CWD preflight passed\n'
/usr/sbin/runuser -u dev -- env HOME="$root/home" SUBYARD_OPERATOR_HOME="$root/home" \
  SUBYARD_HOME="$data" SUBYARD_CONFIG_HOME="$root/config-home" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 \
  YARD_BIN_DIR="$root/bin" YARD_SHELL_RC="$root/home/.bashrc" YARD_LOGIN_RC="$root/home/.profile" \
  YARD_RELEASE_BASE_URL="file://$data/release" YARD_RELEASE_VERSION="$version" \
  "$data/release/subyard-install.sh" --yes >/dev/null
runtime="$(readlink -f "$root/bin/yard")"
[ "$runtime" = "$data/runtime/releases/$version-${digest:0:12}/bin/yard" ]
[ "$("$root/bin/yard" --version)" = "yard $version" ]
(cd "${runtime%/bin/yard}" && sha256sum -c runtime-files.sha256 >/dev/null)
/usr/sbin/runuser -u dev -- env HOME="$root/home" SUBYARD_OPERATOR_HOME="$root/home" \
  SUBYARD_HOME="$data" SUBYARD_CONFIG_HOME="$root/config-home" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 \
  "$root/bin/yard" migrate --check --json > "$data/installed-readiness.json"
printf 'evidence: dev-owned installed runtime migration preflight passed\n'
printf 'owner-a\n' > "$root/config-home/host-id"
chmod 0600 "$root/config-home/host-id"
printf 'SSH_PORT=3222\n' > "$root/config-home/yards/default/config.env"
chmod 0600 "$root/config-home/yards/default/config.env"
ssh-keygen -q -t ed25519 -N '' -C hostid-key-one -f "$root/sshd/key-one"
ssh-keygen -q -t ed25519 -N '' -C hostid-key-two -f "$root/sshd/key-two"
ssh-keygen -q -t ed25519 -N '' -C hostid-key-three -f "$root/sshd/key-three"
cat > "$root/yard-rpc" <<EOF
#!/usr/bin/env bash
export HOME='$root/home'
export SUBYARD_OPERATOR_HOME='$root/home'
export SUBYARD_CONFIG_HOME='$root/config-home'
export SUBYARD_HOME='$data'
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
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
chown -R dev:dev "$root" "$data"
nohup /usr/sbin/sshd -D -e -f "$root/sshd/config" >"$root/sshd.log" 2>&1 &
for _ in $(seq 1 50); do [ -s "$root/sshd.pid" ] && exit 0; sleep 0.1; done
cat "$root/sshd.log" >&2
exit 1
EOS
remote cat "$REMOTE_DATA/installed-readiness.json" > "$LOCAL/owner-installed-readiness.json"
phase owner-source-and-key-prerequisites

# Keep registered Git settings and local ports on the installed owner runtime.
remote_dev bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1"
install -d -m 0700 "$root/source/shared" "$root/source/hosts/owner-a/yards/demo" \
  "$root/source/hosts/owner-a/overrides/agents/codex/rules" "$root/config-home/yards/demo"
printf '{"schemaVersion":1}\n' > "$root/source/subyard-config.json"
printf 'YARD_IMAGE=images:debian/13\n' > "$root/source/shared/config.env"
printf "CODING_TOOL_INTEGRATIONS=codex\nENVIRONMENT_PROFILES=''\n" > "$root/source/hosts/owner-a/config.env"
printf 'SSH_PORT=3224\n' > "$root/source/hosts/owner-a/yards/demo/config.env"
printf 'prefix_rule(pattern=["git"], decision="allow")\n' > "$root/source/hosts/owner-a/overrides/agents/codex/rules/repo.rules"
printf 'SSH_PORT=3225\n' > "$root/config-home/yards/demo/config.env"
chmod 0600 "$root/config-home/yards/demo/config.env"
find "$root/source" -type f -exec chmod 0600 {} +
git init --bare -q "$root/origin.git"
git -C "$root/source" init -q
git -C "$root/source" config user.name 'Subyard Fixture'
git -C "$root/source" config user.email 'fixture@invalid'
git -C "$root/source" add -A
git -C "$root/source" commit -q -m 'Initial owner settings'
git -C "$root/source" remote add origin "$root/origin.git"
git -C "$root/source" push -q -u origin HEAD
EOS
owner_yard() {
  remote_dev env HOME="$REMOTE/home" SUBYARD_OPERATOR_HOME="$REMOTE/home" \
    SUBYARD_CONFIG_HOME="$REMOTE/config-home" SUBYARD_HOME="$REMOTE_DATA" \
    "$REMOTE/bin/yard" "$@"
}
# Selected Git writers use an isolated candidate and the operator's Git identity.
remote_dev env HOME="$REMOTE/home" git config --global user.name 'Subyard Fixture'
remote_dev env HOME="$REMOTE/home" git config --global user.email 'fixture@invalid'
remote_dev chmod 0600 "$REMOTE/home/.gitconfig"
owner_yard config sync connect "$REMOTE/origin.git" --checkout "$REMOTE/source" --host-id owner-a --yes >/dev/null
[ "$(owner_yard config sync path)" = "$REMOTE/source" ] || fail 'owner Git source was not registered'
remote_dev bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: source registration line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1"
jq -e --arg checkout "$root/source" --arg origin "$root/origin.git" \
  '. == {schemaVersion:1, checkout:$checkout, origin:$origin}' "$root/config-home/.sync/source.json" >/dev/null
[ "$(stat -c %a "$root/config-home/.sync/source.json")" = 600 ]
[ "$(cat "$root/config-home/host-id")" = owner-a ]
printf 'evidence: exact owner Git source registration preflight passed\n'
EOS
source_before="$(remote_dev git -C "$REMOTE/source" rev-parse HEAD)"
# Check the installed timer adapter in this isolated home before live initialization.
# These bounded adapter context values describe the existing default fixture yard.
remote_dev env HOME="$REMOTE/home" SUBYARD_OPERATOR_HOME="$REMOTE/home" \
  SUBYARD_CONFIG_HOME="$REMOTE/config-home" SUBYARD_HOME="$REMOTE_DATA" \
  bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1" runtime="$(readlink -f "$1/bin/yard")"
runtime="${runtime%/bin/yard}"
export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1
export SUBYARD_CONFIG_DIR="$runtime/config" STORAGE_PATH="$SUBYARD_HOME/incus/storage"
export HOST_BASE="$SUBYARD_HOME/host" RESTRICTED_DISK_PATHS="$SUBYARD_HOME/host"
export ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME=yard INCUS_PROJECT=subyard INCUS_BRIDGE=incusbr0
export SSH_HOST=yard DEV_USER=dev DEV_UID=1000 DEV_SUDO=0 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0
"$runtime/scripts/install-keys-auto-sync.sh" --yes
"$runtime/scripts/install-keys-auto-sync.sh" --check
[ "$(stat -c %a "$root/home/.config/systemd/user/subyard-keys-sync.service")" = 644 ]
[ "$(stat -c %a "$root/home/.config/systemd/user/subyard-keys-sync.timer")" = 644 ]
printf 'evidence: isolated-home credential unit rendering preflight passed (timer enable excluded)\n'
EOS
# One live fixture-owned yard proves consumers and physical runtime identity.
phase live-init
owner_yard init --yes >"$LOCAL/live-init.out" 2>&1 || { tail -n 80 "$LOCAL/live-init.out" >&2; fail 'minimal live yard initialization failed'; }
printf 'evidence: live init completed\n'
phase live-start
owner_yard start --yes >"$LOCAL/live-start.out" 2>&1 || { tail -n 80 "$LOCAL/live-start.out" >&2; fail 'minimal live yard start failed'; }
printf 'evidence: live yard started\n'
remote_dev incus config set yard user.subyard.entity-fixture "$RUN_ID" --project subyard
phase project-sync
remote_dev bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1"
install -d -m 0700 "$root/project"
printf 'project sentinel\n' > "$root/project/sentinel"
git -C "$root/project" init -q
EOS
owner_yard sync "$REMOTE/project" --name rename-project --yes >/dev/null
printf 'evidence: authoritative fixture project sync completed\n'
runtime_identity() {
  remote_dev bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
snapshot_phase=uuid
trap 'rc=$?; printf "entity-naming-lifecycle: snapshot=%s line=%s exit=%s\n" "$snapshot_phase" "$LINENO" "$rc" >&2' ERR
root="$1"
uuid="$(incus config get yard volatile.uuid --project subyard)"
[ -n "$uuid" ]
printf '%s\n' "$uuid"
snapshot_phase=running-pid
incus query '/1.0/instances/yard/state?project=subyard' | jq -er '.pid | select(.>0)'
snapshot_phase=root-device
incus query '/1.0/instances/yard?project=subyard' \
  | jq -ecS '.expanded_devices.root | select(type=="object" and .type=="disk" and .path=="/")'
snapshot_phase=guest-rule
incus exec yard --project subyard -- sha256sum /home/dev/.codex/rules/repo.rules
snapshot_phase=credential-actor
jq -er '.actorId | select(test("^host-[0-9a-f]+$"))' "$root/config-home/keys/identity.json"
snapshot_phase=project-registry
[ -f "$root/config-home/projects/rename-project.json" ]
(cd "$root/config-home/projects" && find . -type f -exec sha256sum {} + | sort)
snapshot_phase=ssh-listener
python3 -c 'import socket; s=socket.create_connection(("127.0.0.1",3222),3); assert s.recv(255).startswith(b"SSH-"); s.close()'
EOS
}
phase live-snapshot
live_before="$(runtime_identity)"
printf 'evidence: live preservation snapshot captured\n'

# The OS hostname is only the initial identity snapshot, never ongoing authority.
remote hostnamectl set-hostname subyard-hostid-os-renamed
[ "$(remote_dev cat "$REMOTE/config-home/host-id")" = owner-a ] || fail 'OS hostname rewrote persisted HostID'
owner_yard config validate --all-local >/dev/null

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
  install -d -m 0700 "$fixture/home" "$fixture/config/yards/default" "$fixture/data" "$fixture/bin"
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
  printf 'SSH_PORT=3223\n' > "$fixture/config/yards/default/config.env"
  chmod 0600 "$fixture/config/yards/default/config.env"
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
phase peer-registration
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
  SUBYARD_CONFIG_HOME="$REMOTE/config-home" SUBYARD_HOME="$REMOTE_DATA" \
  "$REMOTE/bin/yard" host rename owner-b --yes >/dev/null
phase source-rename-preservation
# Verify the installed rename commit, cache provenance and local overrides.
remote_dev bash -s -- "$REMOTE" "$source_before" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1" before="$2"
[ "$(git -C "$root/origin.git" rev-parse HEAD)" = "$before" ]
[ -z "$(git -C "$root/source" status --porcelain)" ]
head="$(git -C "$root/source" rev-parse HEAD)"
[ "$head" != "$before" ]
[ "$(git -C "$root/source" rev-parse HEAD^)" = "$before" ]
[ "$(git -C "$root/source" rev-parse "$before:hosts/owner-a")" = "$(git -C "$root/source" rev-parse HEAD:hosts/owner-b)" ]
git -C "$root/source" diff --name-status -M100% "$before" HEAD | LC_ALL=C sort > "$root/rename-paths.out"
printf 'R100\thosts/owner-a/%s\thosts/owner-b/%s\n' \
  config.env config.env \
  overrides/agents/codex/rules/repo.rules overrides/agents/codex/rules/repo.rules \
  yards/demo/config.env yards/demo/config.env | LC_ALL=C sort | diff - "$root/rename-paths.out"
[ ! -e "$root/source/hosts/owner-a" ]
[ "$(cat "$root/config-home/yards/demo/config.env")" = SSH_PORT=3225 ]
[ "$(cat "$root/config-home/yards/default/config.env")" = SSH_PORT=3222 ]
cmp "$root/source/hosts/owner-b/overrides/agents/codex/rules/repo.rules" "$root/config-home/.sync/settings/overrides/host/agents/codex/rules/repo.rules"
[ "$(stat -c %a "$root/config-home/.sync/settings/overrides/host/agents/codex/rules/repo.rules")" = 644 ]
jq -e --arg head "$head" '.hostId=="owner-b" and .sourceCommit==$head' "$root/config-home/.sync/manifest.json" >/dev/null
printf 'evidence: one scoped source rename commit preserved exact owner tree and origin HEAD\n'
EOS
owner_yard config sync --yes >/dev/null
owner_yard config validate --all-local >/dev/null
# Simulate the historical defective identity-only rename with deleted cache.
phase source-mismatch-recovery
remote_dev bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1"
printf 'owner-mismatch\n' > "$root/config-home/host-id"
jq '.hostId="owner-mismatch"' "$root/config-home/.sync/manifest.json" > "$root/config-home/.sync/manifest.tmp"
chmod 0600 "$root/config-home/.sync/manifest.tmp"
mv "$root/config-home/.sync/manifest.tmp" "$root/config-home/.sync/manifest.json"
find "$root/config-home/.sync/settings" -type f -delete
EOS
if owner_yard config set YARD_IMAGE images:ubuntu/24.04 --scope host --git --yes >"$LOCAL/mismatch-writer.out" 2>&1; then
  fail 'selected writer admitted a mismatched empty owner source'
fi
owner_yard host rename owner-mismatch --source-host-id owner-b --yes >/dev/null
owner_yard config validate --all-local >/dev/null
owner_yard host rename owner-b --yes >/dev/null
owner_yard config sync --yes >/dev/null
owner_yard config set YARD_IMAGE images:debian/13 --scope shared --git --yes >/dev/null
owner_yard config validate --all-local >/dev/null
[ "$(runtime_identity)" = "$live_before" ] || fail 'rename/recovery changed live UUID, process, root volume, project registry, guest rule hash or credential actor'
expire_owner_inventory "$FIXTURE/data/owner-inventory/owners/owner-a.json" owner-a
phase peer-rename-adoption
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
yard_env "$MISSING" yards >"$LOCAL/missing-summary.out" 2>"$LOCAL/missing-summary.err" \
  || fail 'same-key status refresh failed with missing cache'
grep -Fq 'owner-b/default' "$LOCAL/missing-summary.out" \
  && [ ! -s "$LOCAL/missing-summary.err" ] \
  && [ -f "$MISSING/data/owner-inventory/connections/owner-b.json" ] \
  && [ ! -e "$MISSING/data/owner-inventory/connections/owner-a.json" ] \
  || fail 'missing-cache status did not adopt the authoritative owner'

remote bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1"
kill "$(cat "$root/sshd.pid")"
for _ in $(seq 1 50); do [ ! -e "$root/sshd.pid" ] && break; sleep 0.1; done
sed -i "s#HostKey $root/sshd/key-one#HostKey $root/sshd/key-two#" "$root/sshd/config"
nohup /usr/sbin/sshd -D -e -f "$root/sshd/config" >"$root/sshd.log" 2>&1 &
for _ in $(seq 1 50); do [ -s "$root/sshd.pid" ] && kill -0 "$(cat "$root/sshd.pid")" 2>/dev/null && exit 0; sleep 0.1; done
cat "$root/sshd.log" >&2; exit 1
EOS
expire_owner_inventory "$FIXTURE/data/owner-inventory/owners/owner-b.json" owner-b
phase peer-key-repair
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
  SUBYARD_CONFIG_HOME="$REMOTE/config-home" SUBYARD_HOME="$REMOTE_DATA" \
  "$REMOTE/bin/yard" host rename owner-c --yes >/dev/null
remote bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
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
phase peer-offline-reconnect
expire_owner_inventory "$FIXTURE/data/owner-inventory/owners/owner-c.json" owner-c
if yard_env "$FIXTURE" yards --json >"$LOCAL/offline.out" 2>&1; then
  fail 'offline owner reported a fresh successful refresh'
fi
grep -qi 'stale' "$LOCAL/offline.out" || fail 'offline owner omitted the stale marker'

owner_yard host rename owner-d --yes >/dev/null
# Routing project references guard removal even after the owner comes back.
remote bash -s -- "$REMOTE" <<'EOS'
set -Eeuo pipefail
trap 'rc=$?; printf "entity-naming-lifecycle: remote fixture line=%s exit=%s\n" "$LINENO" "$rc" >&2' ERR
root="$1"
nohup /usr/sbin/sshd -D -e -f "$root/sshd/config" >"$root/sshd.log" 2>&1 &
for _ in $(seq 1 50); do [ -s "$root/sshd.pid" ] && kill -0 "$(cat "$root/sshd.pid")" 2>/dev/null && exit 0; sleep 0.1; done
exit 1
EOS
yard_env "$FIXTURE" yards --json >"$LOCAL/reconnect.out" 2>"$LOCAL/reconnect.err" || fail 'offline rename did not recover on active query'
jq -e '.hostId=="owner-d" and .destination=="owner-alias"' "$FIXTURE/data/owner-inventory/connections/owner-d.json" >/dev/null
[ -f "$FIXTURE/data/owner-inventory/routing/owner-d/default/projects/project-a" ] || fail 'offline reconnect lost project routing'
owner_yard remove rename-project --yes >/dev/null || fail 'owner fixture project removal failed'
phase peer-removal
install -d -m 0700 "$FIXTURE/data/owner-inventory/routing/owner-d/default/projects"
printf 'guard\n' > "$FIXTURE/data/owner-inventory/routing/owner-d/default/projects/project-a"
if yard_env "$FIXTURE" host remove owner-d --yes >"$LOCAL/remove-guard.out" 2>&1; then
  fail 'host removal ignored controller project routing state'
fi
grep -qi 'project routing state' "$LOCAL/remove-guard.out" || fail 'remove guard was not actionable'
find "$FIXTURE/data/owner-inventory/routing/owner-d/default/projects" -depth -delete
yard_env "$FIXTURE" host remove owner-d --yes >/dev/null || fail 'guard-free host removal failed'
[ ! -e "$FIXTURE/data/owner-inventory/connections/owner-d.json" ] \
  && [ ! -e "$FIXTURE/data/owner-inventory/owners/owner-d.json" ] \
  && [ ! -e "$FIXTURE/data/owner-inventory/routing/owner-d" ] \
  || fail 'host removal left controller state'

printf 'ok: real OpenSSH direct, alias, proxy and full HostID lifecycle verified\n'

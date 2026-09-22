#!/usr/bin/env bash
# Real OpenSSH regression for first trust of a registered remote yard.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
[ -n "${SUBYARD_E2E_VM:-}" ] || { printf 'ssh-unknown-host: run through dev/agent-e2e.sh\n' >&2; exit 2; }
for tool in jq script ss ssh sshd ssh-keygen ssh-keyscan; do
  command -v "$tool" >/dev/null 2>&1 || { printf 'ssh-unknown-host: %s is required\n' "$tool" >&2; exit 2; }
done

fail() { printf 'ssh-unknown-host: %s\n' "$*" >&2; exit 1; }
free_port() {
  local port=$((36000 + RANDOM % 12000))
  while ss -Hln "sport = :$port" 2>/dev/null | grep -q .; do port=$((port + 1)); done
  printf '%s\n' "$port"
}

RUN_ID="${SUBYARD_E2E_RUN_ID:-$$}"
YARD_NAME="ssh-trust-${RUN_ID%%-*}"
REMOTE_NAME="trust-${RUN_ID%%-*}"
STATE="${SUBYARD_SSH_TRUST_RESUME:-}"
if [ -n "$STATE" ]; then
  case "$STATE" in /var/tmp/subyard-ssh-unknown.*) ;; *) fail 'invalid fixture resume path' ;; esac
  [ ! -L "$STATE" ] && [ "$(cat "$STATE/.marker")" = "$RUN_ID" ] \
    || fail 'fixture resume marker differs'
else
  STATE="$(mktemp -d /var/tmp/subyard-ssh-unknown.XXXXXX)"
  printf '%s\n' "$RUN_ID" >"$STATE/.marker"
fi
ORIGINAL_HOME="$HOME"
PLATFORM_ROOT="$ORIGINAL_HOME/.cache/subyard-e2e-platform"
PLATFORM_MARKER="$PLATFORM_ROOT/.subyard-e2e-platform-marker"
OWNER_HOME="$STATE/owner-home"
OWNER_CONFIG_HOME="$STATE/owner-config"
OWNER_DATA_HOME="$STATE/owner-data"
CONTROLLER_HOME="$STATE/controller-home"
CONTROLLER_CONFIG_HOME="$STATE/controller-config"
CONTROLLER_DATA_HOME="$STATE/controller-data"
SSHD_PID=''
REMOTE_ADDED=0
HOST_ID=''
cleanup() {
  local rc=$? cleanup_failed=0
  set +e
  if [ "$REMOTE_ADDED" = 1 ]; then "$ROOT/.build/yard" remote remove "$REMOTE_NAME" --yes >&2 || cleanup_failed=1; fi
  [ -z "$SSHD_PID" ] || { kill -TERM "$SSHD_PID" 2>/dev/null; wait "$SSHD_PID" 2>/dev/null; }
  env HOME="$OWNER_HOME" SUBYARD_OPERATOR_HOME="$OWNER_HOME" \
    SUBYARD_CONFIG_HOME="$OWNER_CONFIG_HOME" SUBYARD_HOME="$OWNER_DATA_HOME" \
    SUBYARD_NO_AUDIT=1 "$ROOT/.build/yard" -Y "$YARD_NAME" teardown --yes >&2 || cleanup_failed=1
  if [ "$cleanup_failed" = 1 ]; then
    printf 'ssh-unknown-host: cleanup failed; fixture retained at %s\n' "$STATE" >&2
    exit 3
  fi
  # Controller registrations are local to this disposable fixture directory.
  case "$STATE" in /var/tmp/subyard-ssh-unknown.*) find "$STATE" -depth -delete ;; esac
  exit "$rc"
}
trap cleanup EXIT

if [ -z "${SUBYARD_SSH_TRUST_RESUME:-}" ]; then
  # Keep the shared platform root writable by the operator when the product
  # installer creates root-owned storage ancestors on a fresh VM.
  [ ! -L "$ORIGINAL_HOME/.cache" ] && [ ! -L "$PLATFORM_ROOT" ] \
    || fail 'shared platform root must not be a symlink'
  if [ -e "$PLATFORM_ROOT" ]; then
    [ -d "$PLATFORM_ROOT" ] && [ -O "$PLATFORM_ROOT" ] \
      || fail 'shared platform root must belong to the operator'
    if [ -e "$PLATFORM_MARKER" ] || [ -L "$PLATFORM_MARKER" ]; then
      [ -f "$PLATFORM_MARKER" ] && [ ! -L "$PLATFORM_MARKER" ] \
        && [ "$(cat "$PLATFORM_MARKER")" = subyard-e2e-platform-v1 ] \
        || fail 'unexpected shared E2E platform marker'
    elif find "$PLATFORM_ROOT" -mindepth 1 -print -quit | grep -q .; then
      fail 'refusing a non-empty unmarked shared platform root'
    fi
  else
    install -d -m 0711 "$PLATFORM_ROOT"
  fi
  bash "$ROOT/dev/build-engine.sh" --force >/dev/null
  install -d -m 0700 "$OWNER_HOME" "$OWNER_CONFIG_HOME/yards/$YARD_NAME" \
    "$OWNER_DATA_HOME" "$CONTROLLER_HOME" "$CONTROLLER_CONFIG_HOME" \
    "$CONTROLLER_DATA_HOME" "$STATE/host"
  printf 'owner-%s\n' "$RUN_ID" >"$OWNER_CONFIG_HOME/host-id"
  printf 'controller-%s\n' "$RUN_ID" >"$CONTROLLER_CONFIG_HOME/host-id"
  chmod 0600 "$OWNER_CONFIG_HOME/host-id" "$CONTROLLER_CONFIG_HOME/host-id"

  yard_port="$(free_port)"
  cat >"$OWNER_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
SSH_PORT=$yard_port
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host
HOST_MOUNTS=
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
  chmod 0600 "$OWNER_CONFIG_HOME/yards/$YARD_NAME/config.env"
  env HOME="$OWNER_HOME" SUBYARD_OPERATOR_HOME="$OWNER_HOME" \
    SUBYARD_CONFIG_HOME="$OWNER_CONFIG_HOME" SUBYARD_HOME="$OWNER_DATA_HOME" \
    STORAGE_PATH="$PLATFORM_ROOT/incus/incus/storage" \
    SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1 \
    "$ROOT/.build/yard" -Y "$YARD_NAME" init --yes >/dev/null
fi
# init can install Incus and enroll dev, but cannot change its parent shell's
# groups. Resume the same fixture with the membership needed by owner SSH too.
if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin; then
  id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin \
    || fail 'product initialization did not grant Incus access'
  printf -v resume_command 'exec env SUBYARD_SSH_TRUST_RESUME=%q bash %q' \
    "$STATE" "$ROOT/tests/real-host/ssh-unknown-host.sh"
  exec sg incus-admin -c "$resume_command"
fi
# Retain the verified shared Incus baseline for subsequent acceptance runs.
incus info >/dev/null
for platform_path in "$ORIGINAL_HOME" "$ORIGINAL_HOME/.cache" "$PLATFORM_ROOT" \
  "$PLATFORM_ROOT/incus" "$PLATFORM_ROOT/incus/incus" "$PLATFORM_ROOT/incus/incus/storage"; do
  [ -d "$platform_path" ] && [ ! -L "$platform_path" ] \
    || fail 'shared E2E storage path must contain only plain directories'
done
[ "$(incus storage get default source --project default)" = "$PLATFORM_ROOT/incus/incus/storage" ] \
  || fail 'Incus default storage is outside the shared E2E platform'
[ "$(incus storage list --project default --format json | jq -r '.[] | select(.name == "default") | .driver')" = dir ] \
  || fail 'unexpected shared E2E storage driver'
incus network show incusbr0 --project default >/dev/null
if [ -e "$PLATFORM_MARKER" ] || [ -L "$PLATFORM_MARKER" ]; then
  [ -f "$PLATFORM_MARKER" ] && [ ! -L "$PLATFORM_MARKER" ] \
    && [ "$(cat "$PLATFORM_MARKER")" = subyard-e2e-platform-v1 ] \
    || fail 'unexpected shared E2E platform marker'
else
  platform_marker_temp="$(mktemp "$PLATFORM_ROOT/.platform-marker.XXXXXX")"
  printf '%s\n' subyard-e2e-platform-v1 >"$platform_marker_temp"
  chmod 0600 "$platform_marker_temp"
  mv -f -- "$platform_marker_temp" "$PLATFORM_MARKER"
fi
env HOME="$OWNER_HOME" SUBYARD_OPERATOR_HOME="$OWNER_HOME" \
  SUBYARD_CONFIG_HOME="$OWNER_CONFIG_HOME" SUBYARD_HOME="$OWNER_DATA_HOME" \
  SUBYARD_NO_AUDIT=1 "$ROOT/.build/yard" -Y "$YARD_NAME" start --yes >/dev/null
command -v incus >/dev/null 2>&1 || fail 'product initialization did not install Incus'
owner_port="$(free_port)"

export HOME="$CONTROLLER_HOME"
export SUBYARD_OPERATOR_HOME="$CONTROLLER_HOME"
export SUBYARD_CONFIG_HOME="$CONTROLLER_CONFIG_HOME"
export SUBYARD_HOME="$CONTROLLER_DATA_HOME"
export SUBYARD_NO_AUDIT=1

ssh-keygen -q -t ed25519 -N '' -f "$STATE/owner-host-key"
ssh-keygen -q -t ed25519 -N '' -f "$STATE/controller-key"
cat >"$STATE/owner-bash-env" <<EOF
export PATH='$ROOT/bin':\$PATH
export YARD_ENGINE_PATH='$ROOT/.build/yard'
EOF
chmod 0600 "$STATE/owner-bash-env"
cat >"$STATE/owner-command" <<EOF
#!/usr/bin/env bash
export HOME='$OWNER_HOME' SUBYARD_OPERATOR_HOME='$OWNER_HOME' SUBYARD_CONFIG_HOME='$OWNER_CONFIG_HOME'
export SUBYARD_HOME='$OWNER_DATA_HOME' SUBYARD_NO_AUDIT=1 BASH_ENV='$STATE/owner-bash-env'
export PATH='$ROOT/bin':\$PATH YARD_ENGINE_PATH='$ROOT/.build/yard'
exec /bin/bash -lc "\${SSH_ORIGINAL_COMMAND:?missing SSH command}"
EOF
chmod 0700 "$STATE/owner-command"
# ProxyJump needs local forwarding to the yard loopback port. Keep every other
# restriction and force owner commands into the isolated fixture environment.
printf 'restrict,port-forwarding,command="%s" %s\n' "$STATE/owner-command" "$(cat "$STATE/controller-key.pub")" >"$STATE/authorized_keys"
chmod 0600 "$STATE/authorized_keys"
cat >"$STATE/sshd_config" <<EOF
Port $owner_port
ListenAddress 127.0.0.1
HostKey $STATE/owner-host-key
PidFile $STATE/sshd.pid
AuthorizedKeysFile $STATE/authorized_keys
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PermitRootLogin no
AllowUsers $(id -un)
EOF
/usr/sbin/sshd -t -f "$STATE/sshd_config"
/usr/sbin/sshd -D -e -f "$STATE/sshd_config" >"$STATE/sshd.log" 2>&1 &
SSHD_PID=$!
for _ in $(seq 1 50); do
  ssh-keyscan -T 1 -p "$owner_port" 127.0.0.1 >"$STATE/owner-known-hosts" 2>/dev/null && break
  sleep 0.1
done
[ -s "$STATE/owner-known-hosts" ] || fail 'owner sshd did not become ready'
install -d -m 0700 "$HOME/.ssh" "$STATE/bin"
cat >"$HOME/.ssh/config" <<EOF
Include $HOME/.ssh/subyard-*.config

Host fixture-owner
    HostName 127.0.0.1
    Port $owner_port
    User $(id -un)
    IdentityFile $STATE/controller-key
    IdentitiesOnly yes
    StrictHostKeyChecking yes
    UserKnownHostsFile $STATE/owner-known-hosts
    GlobalKnownHostsFile /dev/null
EOF
chmod 0600 "$HOME/.ssh/config"
cat >"$STATE/bin/ssh" <<'EOF'
#!/usr/bin/env bash
exec /usr/bin/ssh -F "$SSH_UNKNOWN_CONFIG" -o ControlMaster=no -o ControlPath=none "$@"
EOF
chmod 0755 "$STATE/bin/ssh"
export SSH_UNKNOWN_CONFIG="$HOME/.ssh/config"
export PATH="$STATE/bin:$PATH"

ssh fixture-owner -- \
  "test \"\$YARD_ENGINE_PATH\" = '$ROOT/.build/yard' && test \"\$(command -v yard)\" = '$ROOT/bin/yard'" \
  || fail 'owner login shell did not resolve the candidate engine and launcher'

"$ROOT/.build/yard" remote add "$REMOTE_NAME" fixture-owner --yard "$YARD_NAME" --yes >/dev/null
REMOTE_ADDED=1
known="$SUBYARD_HOME/ssh/known_hosts"
namespace="subyard-remote-$REMOTE_NAME"
grep -Fq "$namespace " "$known" || fail 'remote add did not record the yard key'
printf 'unrelated.example %s\n' "$(cat "$STATE/owner-host-key.pub")" >>"$known"
sed -i "/^$namespace /d" "$known"

install -d -m 0700 "$STATE/source"
printf 'first trust regression\n' >"$STATE/source/proof.txt"
set +e
"$ROOT/.build/yard" -Y "$REMOTE_NAME" sync "$STATE/source" --name SSHUnknownHost --yes \
  >"$STATE/sync.out" 2>"$STATE/sync.err"
sync_rc=$?
set -e

if [ "${SUBYARD_EXPECT_UNKNOWN_HOST_FAILURE:-0}" = 1 ]; then
  [ "$sync_rc" -ne 0 ] || fail 'baseline unexpectedly accepted an unknown yard key'
  grep -Fq 'Host key verification failed' "$STATE/sync.err" \
    || fail 'baseline did not fail at strict OpenSSH host-key verification'
  grep -Fq 'unrelated.example ' "$known" || fail 'baseline changed unrelated trust'
  printf 'ok: reproduced strict unknown SSH host-key failure before project mutation\n'
  exit 0
fi

# Automation consent above proves the non-interactive path. Reset the pin and
# separately exercise the real TTY refusal/acceptance contract.
[ "$sync_rc" -eq 0 ] || { sed -n '1,120p' "$STATE/sync.err" >&2; fail 'automation consent did not continue sync after first trust'; }
"$ROOT/.build/yard" -Y "$REMOTE_NAME" remove SSHUnknownHost --yes >/dev/null
sed -i "/^$namespace /d" "$known"

set +e
printf 'n\n' | script -qfec \
  "'$ROOT/.build/yard' -Y '$REMOTE_NAME' sync '$STATE/source' --name SSHUnknownHost" \
  "$STATE/decline.typescript" >/dev/null
decline_rc=$?
set -e
[ "$decline_rc" -ne 0 ] || fail 'declined first trust unexpectedly ran sync'
! grep -Fq "$namespace " "$known" || fail 'declined first trust persisted a yard key'
[ ! -e "$SUBYARD_CONFIG_HOME/yards/$REMOTE_NAME/projects/SSHUnknownHost.json" ] \
  || fail 'declined first trust created the project registration'

printf 'y\ny\n' | script -qfec \
  "'$ROOT/.build/yard' -Y '$REMOTE_NAME' sync '$STATE/source' --name SSHUnknownHost" \
  "$STATE/accept.typescript" >/dev/null
grep -Fq "$namespace " "$known" || fail 'accepted yard key was not persisted in its exact namespace'
grep -Fq 'unrelated.example ' "$known" || fail 'first trust removed unrelated known_hosts content'
"$ROOT/.build/yard" -Y "$REMOTE_NAME" shell SSHUnknownHost --yes -- grep -Fxq 'first trust regression' proof.txt
"$ROOT/.build/yard" -Y "$REMOTE_NAME" remove SSHUnknownHost --yes >/dev/null

# Remove both external pins before owner registration. One legacy ProxyJump
# command must assess and publish the owner and yard keys together.
sed -i "/^$namespace /d" "$known"
: >"$STATE/owner-known-hosts"
"$ROOT/.build/yard" -Y "$REMOTE_NAME" sync "$STATE/source" \
  --name OwnerAndYardTrust --yes >/dev/null
grep -Fq "$namespace " "$known" || fail 'legacy ProxyJump did not restore yard trust'
[ -s "$STATE/owner-known-hosts" ] || fail 'legacy ProxyJump did not restore owner trust'
"$ROOT/.build/yard" -Y "$REMOTE_NAME" shell OwnerAndYardTrust --yes -- \
  grep -Fxq 'first trust regression' proof.txt
"$ROOT/.build/yard" -Y "$REMOTE_NAME" remove OwnerAndYardTrust --yes >/dev/null

# Register the same owner under its authoritative identity. A canonical route
# must use that controller-managed owner pin even with no ambient owner entry,
# assess only the missing yard key and continue the original command.
"$ROOT/.build/yard" host add fixture-owner --yes >/dev/null
HOST_ID="$("$ROOT/.build/yard" host list | awk '$2 == "fixture-owner" { print $1 }')"
[ -n "$HOST_ID" ] || fail 'host add did not expose the authoritative HostID'
"$ROOT/.build/yard" yards --json | jq -e \
  --arg route "$HOST_ID/$YARD_NAME" '.[] | select(.yardRef.hostId + "/" + .yardRef.yardName == $route)' \
  >/dev/null || fail 'registered owner did not expose the canonical yard route'

sed -i "/^$namespace /d" "$known"
: >"$STATE/owner-known-hosts"
"$ROOT/.build/yard" -Y "$HOST_ID/$YARD_NAME" sync "$STATE/source" \
  --name CanonicalSSHTrust --yes >/dev/null
grep -Fq "$namespace " "$known" || fail 'canonical route did not restore the exact yard namespace'
[ ! -s "$STATE/owner-known-hosts" ] \
  || fail 'canonical route unexpectedly copied managed owner trust into the ambient store'
"$ROOT/.build/yard" -Y "$HOST_ID/$YARD_NAME" shell CanonicalSSHTrust --yes -- \
  grep -Fxq 'first trust regression' proof.txt
"$ROOT/.build/yard" -Y "$HOST_ID/$YARD_NAME" remove CanonicalSSHTrust --yes >/dev/null

# The canonical project store retains its ordinary lock after the last removal.
# That housekeeping file must not prevent removal of an empty owner registration.
routing="$CONTROLLER_DATA_HOME/owner-inventory/routing/$HOST_ID"
[ -f "$routing/$YARD_NAME/projects/.lock" ] || fail 'canonical cleanup did not exercise the retained project lock'
"$ROOT/.build/yard" remote remove "$REMOTE_NAME" --yes >/dev/null
REMOTE_ADDED=0
"$ROOT/.build/yard" host remove "$HOST_ID" --yes >/dev/null
[ ! -e "$routing" ] || fail 'host removal retained canonical routing'
[ ! -e "$CONTROLLER_DATA_HOME/owner-inventory/connections/$HOST_ID.json" ] \
  || fail 'host removal retained the controller registration'

printf 'ok: unknown SSH keys covered legacy and canonical routes, owner ProxyJump, refusal, automation, interactive continuation, and empty host removal\n'

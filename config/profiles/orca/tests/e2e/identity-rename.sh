#!/usr/bin/env bash
# Registration-only Orca identity recovery on a broker-owned disposable VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
if [ "${1:-}" = --preflight ]; then
  bash -n "$0"
  python3 -B "$ROOT/config/profiles/orca/tests/identity-test.py"
  python3 -B "$ROOT/config/profiles/orca/tests/release-observation-test.py"
  exit
fi
[ "${SUBYARD_E2E_VM:-}" = 1 ]
[ -f "$ROOT/.subyard-acceptance/candidate.json" ]
STATE="$(mktemp -d /var/tmp/subyard-orca-identity.XXXXXX)"
RUN_ID="${SUBYARD_E2E_RUN_ID:?broker lease required}"
printf '%s\n' "$RUN_ID" > "$STATE/.fixture-run"
name="orca-identity-${STATE##*.}"
name="${name,,}"
project="subyard-$name" instance="yard-$name" launcher=''
incus() { sudo -n /usr/bin/incus "$@"; }
owner() {
  local command
  command=$(python3 -c 'import shlex,sys; print(shlex.join(sys.argv[1:]))' "$@")
  sg incus-admin -c "exec $command"
}
yard() { owner "$launcher" -Y "$name" "$@"; }
guest() { incus exec "$instance" --project "$project" -- "$@"; }
cleanup() {
  local original=$? cleanup_exit=0
  trap - EXIT
  set +e
  if [ "$original" != 0 ]; then
    for diagnostic in "$STATE"/identity-*.log; do
      [ -f "$diagnostic" ] || continue
      printf 'identity diagnostic: %s\n' "${diagnostic##*/}"
      grep -E '^yard:|^Error|^telemetry:|^git-fetch:|identity transition|identity prerequisite|changed since|pending identity|native identity' "$diagnostic" | tail -12
    done
  fi
  if [ -n "$launcher" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64997\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    yard teardown --yes || cleanup_exit=3
    if incus project show "$project" >/dev/null 2>&1; then cleanup_exit=3; fi
  fi
  if [ "$cleanup_exit" = 0 ] && [ ! -L "$STATE" ] && [ "$(cat "$STATE/.fixture-run")" = "$RUN_ID" ]; then
    sudo -n find "$STATE" -depth -delete || cleanup_exit=3
  fi
  printf 'orca-identity: original_exit=%s cleanup_exit=%s\n' "$original" "$cleanup_exit"
  [ "$original" != 0 ] || original=$cleanup_exit
  exit "$original"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
sudo -n true
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
export SUBYARD_OPERATOR_HOME="$STATE/home" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/candidate/runtime" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH
STORAGE_PATH="$(incus storage get default source)"
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
launcher="$(owner bash -c '
  set -euo pipefail
  . "$1/tests/helpers/release-candidate.sh"
  release_candidate_install "$1" "$2"
' _ "$ROOT" "$STATE/candidate")"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
port=$((35000 + $$ % 10000))
cat > "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
SSH_PORT=$port
YARD_KIND=container
YARD_IMAGE=images:debian/13/cloud
YARD_IMAGE_FALLBACK=images:debian/13/cloud
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=orca
DEV_USER=dev
DEV_UID=1001
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
LIMITS_CPU=1
LIMITS_MEMORY=1GiB
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
yard init --yes
yard start --yes
orca_runtime() {
  owner env SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1 \
    SUBYARD_CONFIG_DIR="$YARD_RUNTIME_ROOT/current/config" \
    HOST_BASE="$STATE/host" RESTRICTED_DISK_PATHS="$STATE/host" \
    ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME="$instance" \
    INCUS_PROJECT="$project" INCUS_BRIDGE=incusbr0 SSH_HOST=127.0.0.1 \
    DEV_USER="$1" DEV_UID=1001 DEV_SUDO=1 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0 \
    ENVIRONMENT_PROFILES=orca SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS= \
    "$YARD_RUNTIME_ROOT/current/config/profiles/orca/runtime.sh" "${@:2}"
}
# Selected init keeps an absent Orca runtime absent. A partial installation takes
# the production refresh a yard gets on update: registration helpers and lingering,
# with no application or ingress.
guest install -D -m 0644 /dev/null /usr/local/libexec/subyard/orca-registration.lock
observation="$(orca_runtime dev observe </dev/null)"
jq -e '.state == "stale"' <<<"$observation" >/dev/null
orca_runtime dev apply fixture-seed "$(jq -r .actual <<<"$observation")" "$(jq -r .desired <<<"$observation")" </dev/null \
  | jq -e '.state == "current"' >/dev/null
[ "$(guest loginctl show-user dev --property=Linger --value)" = yes ]
guest test ! -e /etc/systemd/system/subyard-orca.service
guest test ! -e /usr/local/libexec/subyard/orca-contract.sha256
guest systemctl is-active --quiet subyard-orca-discovery.timer
guest systemctl is-enabled --quiet subyard-orca-discovery.timer
printf 'PASS: Orca registration fixture installed through owning primitive; main application remains absent\n'
mkdir -p "$STATE/source"
printf 'retained project data\n' > "$STATE/source/content.txt"
yard sync "$STATE/source" --name identity-project --yes
guest python3 - <<'PYG'
import hashlib,json,os,pwd
from pathlib import Path
u=pwd.getpwnam('dev')
state=Path('/srv/agents/orca');state.mkdir(parents=True,exist_ok=True)
p=state/'routes';p.write_bytes(b'retained registration-only route state\n');p.chmod(0o600);os.chown(p,u.pw_uid,u.pw_gid)
project=Path('/srv/workspaces/identity-project')
v=dict(uid=u.pw_uid,gid=u.pw_gid,home=Path(u.pw_dir).stat().st_ino,route=p.stat().st_ino,project=project.stat().st_ino,metadata=hashlib.sha256((project/'.subyard-meta.json').read_bytes()).hexdigest())
Path('/var/lib/subyard-orca-identity-fixture.json').write_text(json.dumps(v))
PYG
old_user=dev
for user in renamed-dev dev; do
  # Exercise the real owning transport read-only. Its diagnostic source token
  # authorizes no mutation; ordinary init captures its own source-bound recipe below.
  orca_runtime "$old_user" identity-observe "$user" \
    "$(jq -r .runtime_sha256 "$ROOT/.subyard-acceptance/candidate.json")" </dev/null \
    | jq -e '.installed == true and .full == false' >/dev/null
  yard config set DEV_USER "$user" --scope yard --yes
  # Live work in the user manager refuses, with nothing renamed and nothing killed.
  guest runuser -u "$old_user" -- env XDG_RUNTIME_DIR=/run/user/1001 \
    systemd-run --user --quiet --scope --unit=orca_pty_orca_daemon_fixture.scope sleep 600 >/dev/null 2>&1 &
  work=$!
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if guest pgrep -u 1001 -x sleep >/dev/null; then break; fi
    sleep 1
  done
  if yard init --yes >"$STATE/identity-refusal-$user.log" 2>&1; then
    printf 'rename proceeded with a live agent terminal\n' >&2
    exit 1
  fi
  grep -q 'developer user services are running' "$STATE/identity-refusal-$user.log"
  guest pgrep -u 1001 -x sleep >/dev/null
  [ "$(guest id -un 1001)" = "$old_user" ]
  guest pkill -u 1001 -x sleep
  wait "$work" || :
  yard init --yes
  [ "$(guest loginctl show-user "$user" --property=Linger --value)" = yes ]
  guest test ! -e "/var/lib/systemd/linger/$old_user"
  guest python3 - "$user" <<'PYG'
import hashlib,json,pwd,subprocess,sys
from pathlib import Path
u=pwd.getpwnam(sys.argv[1]);v=json.loads(Path('/var/lib/subyard-orca-identity-fixture.json').read_text())
assert (u.pw_uid,u.pw_gid)==(v['uid'],v['gid'])
assert Path(u.pw_dir).stat().st_ino==v['home']
project=Path('/srv/workspaces/identity-project')
assert project.stat().st_ino==v['project'] and (project/'src/content.txt').read_bytes()==b'retained project data\n'
assert hashlib.sha256((project/'.subyard-meta.json').read_bytes()).hexdigest()==v['metadata']
p=Path('/srv/agents/orca/routes');assert p.stat().st_ino==v['route'] and p.read_bytes()==b'retained registration-only route state\n'
assert not Path('/etc/systemd/system/subyard-orca.service').exists()
assert not Path('/usr/local/libexec/subyard/orca-contract.sha256').exists()
fields=dict(line.split('=',1) for line in subprocess.check_output(['systemctl','show','subyard-orca-discovery.service','--property=User,Group,Environment,FragmentPath'],text=True).splitlines())
assert fields['User']==fields['Group']==u.pw_name
assert 'HOME='+u.pw_dir in fields['Environment'].split()
assert fields['FragmentPath']=='/etc/systemd/system/subyard-orca-discovery.service'
PYG
  guest systemctl is-active --quiet subyard-orca-discovery.timer
  guest systemctl is-enabled --quiet subyard-orca-discovery.timer
  old_user="$user"
  [ "$(yard shell -- id -un)" = "$user" ]
  yard init --yes
done
# Until init applies a rename, a shutdown still addresses the installed account.
yard config set DEV_USER next-dev --scope yard --yes
if ! yard orca down --yes >"$STATE/identity-down.log" 2>&1; then
  tail -n 40 "$STATE/identity-down.log" >&2
  exit 1
fi
grep -q "/user/$old_user/orca-daemon-and-terminal-scopes" "$STATE/identity-down.log"
[ "$(guest id -un 1001)" = "$old_user" ]
printf 'PASS: Orca shutdown addresses the installed account before init applies a rename\n'
printf 'PASS: Orca registration-only rename preserves absent app, discovery identity, project-hook continuation and retained state\n'

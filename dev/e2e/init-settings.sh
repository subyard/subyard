#!/usr/bin/env bash
# Installed-runtime configuration drift on one broker-owned disposable VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [ "${1:-}" = --preflight ]; then
  bash -n "$0"
  go test ./internal/config ./internal/cli ./internal/adapters/reconcileruntime -run 'TestDeveloperIdentity|TestStoppedIdentityInspection|TestAppliedIdentityHint|TestIdentityPrerequisite|TestIdentityContinuation|TestPhysicalLifecycleOwner|TestTeardownPhysicalOwner|TestTeardownSessionGuard|TestGenericProfileCompleted|TestIdentityAuthoring|TestAppliedGuestIdentity|TestCompletedReleaseKeepsPendingConfiguration|TestReleaseMaterialization|TestIntegrationRetryAdmission|TestInstalledRelease|TestAppliedIntegration|TestNativeAppliedIntegrationCapture|TestIntegrationSettingsPublicationRetryAndLateFailure|TestLegacyIntegrationSettings|TestHostIntegrationSettings' -count=1
  exit
fi
[ "${SUBYARD_E2E_VM:-}" = 1 ]
[ -f "$ROOT/.subyard-acceptance/candidate.json" ]
STATE="$(mktemp -d /var/tmp/subyard-init-settings.XXXXXX)"
RUN_ID="${SUBYARD_E2E_RUN_ID:?broker lease required}"
printf '%s\n' "$RUN_ID" > "$STATE/.fixture-run"
name="init-settings-${STATE##*.}"
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
    guest python3 - <<'PYG'
import json,pwd
from pathlib import Path
path=Path('/var/lib/subyard/developer-identity/transition.json')
result={'journal_phase':json.loads(path.read_text()).get('phase') if path.is_file() else None}
for name in ('dev','fixture-other'):
    try:
        u=pwd.getpwnam(name);result[name]={'uid':u.pw_uid,'gid':u.pw_gid,'home':u.pw_dir}
    except KeyError:result[name]=None
print('IDENTITY_FAILURE_STATE='+json.dumps(result,sort_keys=True))
PYG
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
  printf 'init-settings: original_exit=%s cleanup_exit=%s\n' "$original" "$cleanup_exit"
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
cat > "$STATE/provision.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for command in fixture-agent fixture-check; do
  printf '#!/bin/sh\nexit 0\n' > "/usr/local/bin/$command"
  chmod 0755 "/usr/local/bin/$command"
done
if [ -e /var/lib/subyard-fixture-next ]; then
  for command in fixture-agent-next fixture-check-next; do
    printf '#!/bin/sh\nexit 0\n' > "/usr/local/bin/$command"
    chmod 0755 "/usr/local/bin/$command"
  done
fi
printf '#!/bin/sh\n[ ! -e /var/lib/subyard-fixture-hook-fail ]\n' > /usr/local/bin/fixture-hook
chmod 0755 /usr/local/bin/fixture-hook
SH
chmod 0600 "$STATE/provision.sh"
printf '{"fixture":"before"}\n' > "$STATE/claude.json"
chmod 0600 "$STATE/claude.json"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
port=$((35000 + $$ % 10000))
cat > "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
SSH_PORT=$port
YARD_KIND=container
YARD_IMAGE=images:debian/13/cloud
YARD_IMAGE_FALLBACK=images:debian/13/cloud
CODING_TOOL_INTEGRATIONS=
AGENT_claude_PROVISION=$STATE/provision.sh
AGENT_claude_CONFIG=$STATE/claude.json
AGENT_claude_COMMAND=fixture-agent
AGENT_claude_CHECK=fixture-check
AGENT_claude_PERSIST=
ENVIRONMENT_PROFILES=
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
before="$(guest sha256sum /etc/subyard/preview.json | cut -d' ' -f1)"
preview_port=$((port + 1))
yard config set WEB_PREVIEW_HOST_PORT "$preview_port" --scope yard --yes
yard stop --yes
yard start --yes
yard init --yes
guest python3 -c 'import json,sys; value=json.load(open("/etc/subyard/preview.json")); assert value["host"] != "127.0.0.1", "fixture requires a routed preview endpoint"; assert value["port"] == int(sys.argv[1])' "$preview_port"
after="$(guest sha256sum /etc/subyard/preview.json | cut -d' ' -f1)"
[ "$before" != "$after" ]
yard init --yes
printf 'PASS: installed candidate config set -> stop/start -> init converges preview port without migrate\n'
# A yard with no coding integrations can establish the owner from core receipts
# before its legacy applied-input snapshot is captured.
guest python3 -c 'import json; p="/var/lib/subyard/integrations/inventory.json"; value=json.load(open(p)); value.pop("applied_settings",None); open(p,"w").write(json.dumps(value))'
yard config set DEV_USER fixture-other --scope yard --yes
yard stop --yes
yard start --yes
yard config set DEV_USER dev --scope yard --yes
yard init --yes
printf 'PASS: no coding integrations or applied snapshot retains the verified core owner\n'
yard config set CODING_TOOL_INTEGRATIONS claude --scope yard --yes
yard stop --yes
yard start --yes
# Exercise first publication with an established inventory and no diagnostic baseline.
guest python3 -c 'import json; p="/var/lib/subyard/integrations/inventory.json"; value=json.load(open(p)); value.pop("diagnostics",None); open(p,"w").write(json.dumps(value))'
yard init --yes
printf '{"fixture":"after"}\n' > "$STATE/claude.json"
yard config status > "$STATE/config-drift.log" 2>&1 || true
if ! grep -F 'AGENT_claude_CONFIG  content changed' "$STATE/config-drift.log" >/dev/null; then
  printf '{"fixture":"before"}\n' > "$STATE/claude.json"
  cat "$STATE/config-drift.log" >&2
  printf 'init-settings: first config publication did not record source content\n' >&2; exit 1
fi
printf '{"fixture":"before"}\n' > "$STATE/claude.json"
printf 'PASS: first config publication records same-path content changes\n'
# Seed the protected legacy inventory shape, preserving its native ownership.
guest python3 -c 'import json; p="/var/lib/subyard/integrations/inventory.json"; value=json.load(open(p)); value.pop("applied_settings",None); open(p,"w").write(json.dumps(value))'
yard config set AGENT_claude_COMMAND fixture-agent-next --scope yard --yes
yard stop --yes
yard start --yes
yard config set AGENT_claude_CHECK fixture-check-next --scope yard --yes
guest touch /var/lib/subyard-fixture-next
yard init --yes
guest fixture-agent-next
guest fixture-check-next
printf 'PASS: legacy capture preserves applied custom command/check until ordinary init\n'
yard config set AGENT_claude_PROJECTS_CHANGED fixture-hook --scope yard --yes
yard stop --yes
yard start --yes
printf 'PASS: first authored hook remains pending during ordinary stop/start\n'
guest touch /var/lib/subyard-fixture-hook-fail
if yard integration enable claude --yes > "$STATE/failed-hook.log" 2>&1; then
  printf 'init-settings: fixture hook failure was accepted\n' >&2
  exit 1
fi
guest rm -- /var/lib/subyard-fixture-hook-fail
yard integration enable claude --yes
printf 'PASS: direct integration enable retries a failed hook without migrate\n'
guest python3 -c 'import json; p="/var/lib/subyard/integrations/inventory.json"; value=json.load(open(p)); value.pop("applied_settings",None); open(p,"w").write(json.dumps(value))'
yard integration enable claude --yes
guest python3 -c 'import json; value=json.load(open("/var/lib/subyard/integrations/inventory.json")); assert value["applied_settings"]["AGENT_claude_COMMAND"] == "fixture-agent-next"'
printf 'PASS: converged integration retry restores the applied baseline\n'
guest mv /usr/local/libexec/subyard/projects-changed /usr/local/libexec/subyard/projects-changed.fixture-backup
if yard stop --yes > "$STATE/missing-owned-file.log" 2>&1; then
  printf 'init-settings: applied baseline concealed a missing owned dispatcher\n' >&2
  exit 1
fi
guest mv /usr/local/libexec/subyard/projects-changed.fixture-backup /usr/local/libexec/subyard/projects-changed
yard init --yes
printf 'PASS: applied settings retain missing-owned-file release refusal\n'
# Install the relevant credential-free owners before exercising account recovery.
yard config set ENVIRONMENT_PROFILES 'git-fetch telemetry' --scope yard --yes
yard config set DEV_SUDO 1 --scope yard --yes
yard stop --yes
yard start --yes
printf 'PASS: pending profile selection preserves ordinary stop/start before provisioning\n'
yard init --yes
# A registered project binds the hook scope a stopped rename continuation must keep.
mkdir -p "$STATE/source"
printf 'retained project data\n' > "$STATE/source/content.txt"
yard sync "$STATE/source" --name identity-project --yes
guest python3 - <<'PYG'
import json,os,pwd
from pathlib import Path
u=pwd.getpwnam('dev'); home=Path(u.pw_dir)
p=home/'retained-identity-data';p.write_text('preserved identity data\n');os.chown(p,u.pw_uid,u.pw_gid);p.chmod(0o600)
value={'uid':u.pw_uid,'gid':u.pw_gid,'home_inode':home.stat().st_ino,'data_inode':p.stat().st_ino,
       'subids':{name:[line.split(':',1)[1] for line in Path('/etc',name).read_text().splitlines() if line.startswith('dev:')] for name in ('subuid','subgid')}}
assert u.pw_uid!=u.pw_gid, 'retain the product-created distinct UID/GID baseline'
Path('/var/lib/subyard-identity-fixture.json').write_text(json.dumps(value))
PYG
yard config set DEV_USER fixture-other --scope yard --yes
yard config set AGENT_claude_COMMAND fixture-agent --scope yard --yes
# A foreign active session must refuse rather than being killed. This process is
# fixture-owned and is the only process this fixture explicitly terminates.
guest python3 - <<'PYG'
import os,pwd,subprocess
from pathlib import Path
u=pwd.getpwnam('dev')
def identity():os.setgid(u.pw_gid);os.setuid(u.pw_uid)
p=subprocess.Popen(['sleep','300'],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,preexec_fn=identity,start_new_session=True)
Path('/run/subyard-identity-fixture.pid').write_text(str(p.pid))
PYG
if yard init --yes > "$STATE/identity-active.log" 2>&1; then
  printf 'init-settings: live developer process was accepted\n' >&2; exit 1
fi
guest test -d /home/dev
guest test -f /var/lib/subyard/developer-identity/transition.json
if yard config set DEV_USER foreign --scope yard --yes > "$STATE/identity-admission.log" 2>&1; then
  printf 'init-settings: pending identity admitted configuration mutation\n' >&2; exit 1
fi
guest sh -c 'kill "$(cat /run/subyard-identity-fixture.pid)"'
# Cut after native usermod has changed its databases but before the core journal
# advances. Preserve and restore the actual native binary around this one cut.
guest sh -c 'mv /usr/sbin/usermod /usr/sbin/usermod.identity-fixture'
guest sh -c 'cat > /usr/sbin/usermod; chmod 0755 /usr/sbin/usermod' <<'SH'
#!/bin/sh
/usr/sbin/usermod.identity-fixture "$@" || exit $?
printf 'native-usermod-complete\n' > /run/subyard-identity-fixture.cut
chmod 0600 /run/subyard-identity-fixture.cut
exit 75
SH
if yard init --yes > "$STATE/identity-cut.log" 2>&1; then
  printf 'init-settings: injected account cut did not interrupt init\n' >&2; exit 1
fi
guest mv /usr/sbin/usermod.identity-fixture /usr/sbin/usermod
guest python3 - <<'PYG'
import json,pwd
from pathlib import Path
assert Path('/run/subyard-identity-fixture.cut').read_text()=='native-usermod-complete\n'
assert pwd.getpwnam('fixture-other').pw_dir=='/home/fixture-other'
assert json.loads(Path('/var/lib/subyard/developer-identity/transition.json').read_text())['phase']=='home'
PYG
printf 'PASS: native usermod completed before exit75 with new passwd and unreconciled home journal\n'
# An external shutdown may happen while the journal is pending. Enabled owned
# units can restart at boot; recovery must re-quiesce the ORIGINAL recipe.
guest touch /var/lib/subyard-fixture-hook-fail
incus stop "$instance" --project "$project"
[ "$(incus list "$instance" --project "$project" --format csv -c s)" = STOPPED ]

# Finish the identity while a later ordinary hook fails. Identity must stay
# usable and other applied settings must remain at their last successful state.
if yard init --yes > "$STATE/identity-hook.log" 2>&1; then
  printf 'init-settings: late hook failure was accepted\n' >&2; exit 1
fi
guest python3 - <<'PYG'
import json,pwd
from pathlib import Path
v=json.loads(Path('/var/lib/subyard-identity-fixture.json').read_text());u=pwd.getpwnam('fixture-other');home=Path(u.pw_dir)
assert (u.pw_uid,u.pw_gid,u.pw_dir)==(v['uid'],v['gid'],'/home/fixture-other')
try:pwd.getpwnam('dev');raise AssertionError('old login remains')
except KeyError:pass
assert home.stat().st_ino==v['home_inode']
p=home/'retained-identity-data';assert p.stat().st_ino==v['data_inode'] and p.read_text()=='preserved identity data\n'
assert (p.stat().st_uid,p.stat().st_gid)==(v['uid'],v['gid'])
for name,ranges in v['subids'].items():
 rows=Path('/etc',name).read_text().splitlines();assert not any(row.startswith('dev:') for row in rows)
 assert [row.split(':',1)[1] for row in rows if row.startswith('fixture-other:')]==ranges
assert not Path('/var/lib/subyard/developer-identity/transition.json').exists()
assert not Path('/etc/sudoers.d/90-subyard-dev').exists()
assert Path('/etc/sudoers.d/90-subyard-fixture-other').read_text()=='fixture-other ALL=(ALL) NOPASSWD:ALL\n'
value=json.loads(Path('/var/lib/subyard/integrations/inventory.json').read_text())
assert value['applied_settings']['DEV_USER']=='fixture-other'
assert value['applied_settings']['AGENT_claude_COMMAND']=='fixture-agent-next'
PYG
yard stop --yes
yard start --yes
guest rm -- /var/lib/subyard-fixture-hook-fail
yard init --yes
yard shell -- id -un | grep -Fx fixture-other
guest python3 -c 'import json; from pathlib import Path; value=json.loads(Path("/var/lib/subyard/integrations/inventory.json").read_text()); assert value["applied_settings"]["AGENT_claude_COMMAND"]=="fixture-agent"'
guest bash -s < "$ROOT/config/profiles/telemetry/tests/e2e/identity-check.sh"
yard init --yes
guest grep -Fxq 'retained project data' /srv/workspaces/identity-project/src/content.txt
printf 'PASS: same UID/GID rename preserves data and owners, recovers native cut, and survives later hook failure\n'
yard config set DEV_USER dev --scope yard --yes
yard stop --yes
yard init --yes
[ "$(incus list "$instance" --project "$project" --format csv -c s)" = STOPPED ]
[ "$(incus config get "$instance" --project "$project" user.subyard.desired_power)" = stopped ]
python3 - "$SUBYARD_CONFIG_HOME/reconcile/applied-identities/$name.json" <<'PYH'
import json,sys
assert json.load(open(sys.argv[1]))['user']=='dev'
PYH
yard init --yes > "$STATE/identity-stopped-repeat.log" 2>&1
if grep -q 'temporarily start stopped yard' "$STATE/identity-stopped-repeat.log"; then
  printf 'init-settings: matching applied identity hint repeated inspection boot\n' >&2; exit 1
fi
# Legacy installations have no hint. Inspect once without changing stopped
# power intent, then cache only the verified guest identity.
rm -- "$SUBYARD_CONFIG_HOME/reconcile/applied-identities/$name.json"
yard init --yes
[ "$(incus list "$instance" --project "$project" --format csv -c s)" = STOPPED ]
[ -f "$SUBYARD_CONFIG_HOME/reconcile/applied-identities/$name.json" ]
yard init --yes > "$STATE/identity-stopped-repeat.log" 2>&1
if grep -q 'temporarily start stopped yard' "$STATE/identity-stopped-repeat.log"; then
  printf 'init-settings: legacy inspection did not retain its verified hint\n' >&2; exit 1
fi
yard start --yes
yard shell -- id -un | grep -Fx dev
printf 'PASS: stopped init renames and restores desired power; legacy inspection caches its verified identity\n'
printf 'PASS: retired identity authority permits a second rename and ordinary repeat init\n'

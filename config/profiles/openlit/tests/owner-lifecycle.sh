#!/usr/bin/env bash
# Verify bounded owner transport and refusal before guest effects.
set -euo pipefail
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
[ "$#" -le 1 ] || { printf 'usage: owner-lifecycle.sh [installed-runtime]\n' >&2; exit 2; }
if [ "$#" = 1 ]; then root="$(cd "$1" && pwd)"; fi
state="$(mktemp -d /tmp/subyard-openlit-owner.XXXXXX)"
trap 'rm -rf -- "$state"' EXIT
mkdir "$state/bin"
cat > "$state/bin/incus" <<'MOCK'
#!/usr/bin/env python3
import json,os,sys,tarfile,subprocess
from pathlib import Path
args=sys.argv[1:]
path=Path(os.environ['OPENLIT_OWNER_FIXTURE_CALLS']+'.route')
state=json.loads(path.read_text()) if path.exists() else {'config':{},'devices':{}}
if args[:2]==['list','--all-projects']: print('[]'); sys.exit()
if args[0]=='list': print(os.environ.get('OPENLIT_OWNER_FIXTURE_STATE','RUNNING')); sys.exit()
if args[0]=='query':
    print(json.dumps([] if 'recursion=1' in args[1] else state)); sys.exit()
if args[0]=='config':
    if args[1]=='set': state['config'][args[3]]=args[4]
    elif args[1]=='unset': state['config'].pop(args[3])
    elif args[2]=='add': state['devices'][args[4]]={'type':args[5],**dict(arg.split('=',1) for arg in args[6:] if '=' in arg)}
    elif args[2]=='remove': state['devices'].pop(args[4])
    path.write_text(json.dumps(state)); sys.exit()
assert args[0]=='exec'
names=[]
guest_code=''
with tarfile.open(fileobj=sys.stdin.buffer,mode='r|') as archive:
    for member in archive:
        names.append(member.name)
        if member.name=='guest.sh': guest_code=archive.extractfile(member).read().decode()
names.sort()
Path(os.environ['OPENLIT_OWNER_FIXTURE_FILES']).write_text(''.join(name+'\n' for name in names))
position=args.index('subyard')
mode,selected,user,agents,*bindings=args[position+1:]
if mode!='observe':
    with open(os.environ['OPENLIT_OWNER_FIXTURE_CALLS'],'a') as output: output.write('\n'.join((mode,selected,user,agents))+'\n')
if mode=='pause' and os.environ.get('OPENLIT_OWNER_FIXTURE_REAL_GUEST')=='1':
    directory=Path(os.environ['OPENLIT_OWNER_FIXTURE_CALLS']).parent/'guest'
    state=directory/'state'; state.mkdir(parents=True,mode=0o700,exist_ok=True)
    (state/'marker').write_text('subyard-openlit-v1\n')
    unit=directory/'unit'; unit.write_text('# Managed by Subyard OpenLIT profile.\n')
    guest_code=guest_code.replace('state=/var/lib/subyard/openlit','state='+str(state)).replace('unit=/etc/systemd/system/subyard-openlit.service','unit='+str(unit)).replace('/run/lock/subyard-openlit.lock',str(directory/'lock'))
    guest_code=guest_code.replace('cutover_unit=/etc/systemd/system/subyard-openlit-trace-cutover.service','cutover_unit='+str(directory/'cutover-unit')).replace('cutover_timer=/etc/systemd/system/subyard-openlit-trace-cutover.timer','cutover_timer='+str(directory/'cutover-timer'))
    (directory/'guest.sh').write_text(guest_code)
    (directory/'trace_cutover.py').write_text('import os,sys\nif sys.argv[1] == "locked-guest": os.execv("/bin/bash", ["bash", sys.argv[2], "--runtime-lock-fd", "9", *sys.argv[3:]])\n')
    result=subprocess.run(['bash',str(directory/'guest.sh'),mode,selected,user,agents])
    sys.exit(result.returncode)
elif mode=='pause': print('paused')
elif mode in ('observe','refresh'):
    print(json.dumps({'state':'current','actual':'a'*64,'desired':'a'*64}))
MOCK
chmod 755 "$state/bin/incus"
export PATH="$state/bin:$PATH"
export OPENLIT_OWNER_FIXTURE_FILES="$state/files" OPENLIT_OWNER_FIXTURE_CALLS="$state/calls"
export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1
export SUBYARD_OPERATOR_HOME="$state" SUBYARD_CONFIG_DIR="$root/config" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export STORAGE_PATH="$state/storage" HOST_BASE="$state/host" RESTRICTED_DISK_PATHS="$state/host"
export ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME=yard-fixture INCUS_PROJECT=subyard-fixture INCUS_BRIDGE=fixture SSH_HOST=127.0.0.1
export DEV_USER=dev DEV_UID=1000 DEV_SUDO=0 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0
export CODING_TOOL_INTEGRATIONS='claude codex'
export AGENT_claude_CONFIG="$root/config/agents/claude/settings.json" AGENT_claude_CONFIG_DEST=.claude/settings.json
export AGENT_codex_CONFIG="$root/config/agents/codex/config.toml" AGENT_codex_CONFIG_DEST=.codex/config.toml
export OPENLIT_HOST_PORT=22222
export SUBYARD_PROFILE_SELECTED=0
bash "$root/config/profiles/openlit/owner.sh" --check
[ ! -s "$state/calls" ]
# Provision observes installed profiles even when they are not selected.
observation="$(ENVIRONMENT_PROFILES='' CODING_TOOL_INTEGRATIONS='' "$root/config/profiles/openlit/runtime.sh" observe)"
python3 - "$observation" <<'PYOBSERVE'
import json,sys
record=json.loads(sys.argv[1])
assert record['state']=='current' and record['actual']==record['desired']
PYOBSERVE
[ ! -s "$state/calls" ]
export SUBYARD_PROFILE_SELECTED=1
: > "$state/calls"
bash "$root/config/profiles/openlit/owner.sh" --yes
[ ! -s "$state/calls" ]
printf 'account.py\ncollector.yaml\ncompose.yaml\nguest.sh\nnative.py\nopenlit-collector.yaml\nruntime.py\nsubyard-openlit-trace-cutover.service\nsubyard-openlit-trace-cutover.timer\nsubyard-openlit.service\ntomli_w.py\ntrace_cutover.py\n' > "$state/expected"
cmp "$state/expected" "$state/files"
: > "$state/calls"
bash "$root/config/profiles/openlit/owner.sh" --remove
[ ! -s "$state/calls" ]
[ "$(bash "$root/config/profiles/openlit/owner.sh" --pause)" = paused ]
: > "$state/calls"
OPENLIT_OWNER_FIXTURE_STATE=STOPPED bash "$root/config/profiles/openlit/owner.sh" --yes
[ ! -s "$state/calls" ]
printf '[otel]\nexporter="none"\n' > "$state/conflict.toml"
if AGENT_codex_CONFIG="$state/conflict.toml" bash "$root/config/profiles/openlit/owner.sh" --yes >/dev/null 2>&1; then
  echo 'FAIL: template conflict accepted' >&2; exit 1
fi
[ ! -s "$state/calls" ]

# Released-updater observations use this profile's exact transport, with no guest execution.
# Retain the owning guest double for the independent elevated-pause scenario.
cp "$state/bin/incus" "$state/incus-owner-fixture"
python3 - "$root" "$state" <<'PY'
import json, os, pathlib, runpy, subprocess, sys

root, state = map(pathlib.Path, sys.argv[1:])
control, rejected = state/'control.json', state/'rejected'
control.write_text(json.dumps({'instances': [{'info': {'status': 'Running'}}]}))
control.chmod(0o600)
harness = runpy.run_path(str(root/'dev/verify-release-upgrades.py'))
harness['write_observation_incus'](state/'bin', control, rejected)
profile = root/'config/profiles/openlit'
environment = dict(os.environ, INCUS_PROJECT='subyard', YARD_INSTANCE_NAME='yard',
                   ENVIRONMENT_PROFILES='')
for agents in ('', 'claude', 'codex', 'claude codex'):
    result = subprocess.run(['bash', str(profile/'runtime.sh'), 'observe'],
                            env=dict(environment, CODING_TOOL_INTEGRATIONS=agents),
                            capture_output=True, text=True, timeout=30)
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout) == {'state': 'absent', 'actual': '', 'desired': ''}
    assert not rejected.exists()
before = control.read_bytes()
result = subprocess.run(['bash', str(profile/'runtime.sh'), 'apply', 'fixture-operation',
                         'a'*64, 'b'*64], env=environment,
                        capture_output=True, timeout=30)
# The dashboard's fresh observation refuses stale bindings before any host write.
assert result.returncode != 0 and not result.stdout
assert b'dashboard/runtime conflict or failed verification' in result.stderr
assert control.read_bytes() == before and not rejected.exists()
commands = runpy.run_path(str(profile/'tests/release-observation.py'))['read_only_commands'](profile)
command = commands[0][0]
changed_script = command.copy(); changed_script[9] += '\n'
apply = command.copy(); apply[11] = 'refresh'; apply += ['a'*64, 'b'*64]
selected = command.copy(); selected[12] = '1'
foreign = command.copy(); foreign[1] = 'foreign'
for arguments in (changed_script, apply, selected, foreign):
    rejected.unlink(missing_ok=True)
    result = subprocess.run([str(state/'bin/incus'), *arguments], input=b'',
                            capture_output=True, timeout=10)
    assert result.returncode == 99 and rejected.exists()
    assert not result.stdout
print('ok: released-updater observes OpenLIT absence and rejects changed transport, apply, selected and foreign calls')
PY
cp "$state/incus-owner-fixture" "$state/bin/incus"
chmod 0755 "$state/bin/incus"
printf 'ok: OpenLIT lifecycle transport, stopped preservation and template-conflict refusal\n'

# Exercise the real elevation projection, owner controller and tar-delivered guest
# pause script. Root lock handoff and systemctl/identity are platform doubles;
# trace-cutover-test.py owns real protected file/FD/flock boundaries.
cat > "$state/bin/systemctl" <<'SH'
#!/bin/sh
case "$1" in
  is-active) [ ! -f "$OPENLIT_OWNER_FIXTURE_CALLS.stopped" ] ;;
  stop) touch "$OPENLIT_OWNER_FIXTURE_CALLS.stopped" ;;
  *) exit 99 ;;
esac
SH
cat > "$state/bin/stat" <<'SH'
#!/bin/sh
case "$2" in %u:%g:%a) printf '0:0:700\n' ;; %u) printf '0\n' ;; *) exit 99 ;; esac
SH
cat > "$state/bin/id" <<'SH'
#!/bin/sh
case "$1:${2-}" in -u:) printf '0\n' ;; -u:dev|-g:dev) printf '1000\n' ;; *) exec /usr/bin/id "$@" ;; esac
SH
cat > "$state/bin/getent" <<'SH'
#!/bin/sh
if [ "$1:${2-}" = passwd:dev ]; then
  printf 'dev:x:1000:1000::%s:/bin/bash\n' "$OPENLIT_OWNER_FIXTURE_HOME"
else exec /usr/bin/getent "$@"; fi
SH
chmod 755 "$state/bin/"{systemctl,stat,id,getent}
# shellcheck source=scripts/lib/engine-context.sh
. "$root/scripts/lib/engine-context.sh"
subyard_elevated_context
for expected in paused ''; do
  result="$(env -i "${SUBYARD_ELEVATED_ENV[@]}" PATH="$PATH" \
    SUBYARD_PROFILE_SELECTED=1 OPENLIT_OWNER_FIXTURE_REAL_GUEST=1 \
    OPENLIT_OWNER_FIXTURE_HOME="$state/home" \
    OPENLIT_OWNER_FIXTURE_FILES="$state/files" OPENLIT_OWNER_FIXTURE_CALLS="$state/calls" \
    bash -euc '[ -z "${OPENLIT_HOST_PORT+x}" ]; bash "$1" --pause' fixture "$root/config/profiles/openlit/owner.sh")"
  [ "$result" = "$expected" ]
done
printf 'ok: elevated owner/guest pause with absent desired port and exact paused/empty stdout\n'

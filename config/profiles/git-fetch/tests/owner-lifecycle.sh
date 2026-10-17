#!/usr/bin/env bash
# Exercise the actual profile transport with a bounded fake Incus guest boundary.
set -euo pipefail
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
state="$(mktemp -d /tmp/subyard-git-fetch-owner.XXXXXX)"
trap 'rm -rf -- "$state"' EXIT
mkdir "$state/bin"
cat > "$state/bin/incus" <<'PY'
#!/usr/bin/env python3
import json,os,sys,tarfile
from pathlib import Path
args=sys.argv[1:]
if os.environ.get('GIT_FETCH_FIXTURE_FAIL')==args[0]: sys.exit(1)
if args[0]=='list': print(os.environ.get('GIT_FETCH_FIXTURE_STATE','RUNNING')); sys.exit()
assert args[0]=='exec'
Path(os.environ['GIT_FETCH_FIXTURE_CALLS']+'.argv').write_text(json.dumps(args))
with tarfile.open(fileobj=sys.stdin.buffer,mode='r|') as archive:
    names=sorted(member.name for member in archive)
assert names==['fetch.py','guest.py','subyard-git-fetch.service.in','subyard-git-fetch.timer']
arguments=args[args.index('subyard')+1:]
Path(os.environ['GIT_FETCH_FIXTURE_CALLS']).write_text(json.dumps(arguments))
if arguments[0]=='check' and os.environ.get('GIT_FETCH_FIXTURE_DRIFT')=='1': print('git-fetch-drift')
if arguments[0]=='pause': print('paused')
if arguments[0] in ('observe','refresh'): print(json.dumps({'state':'current','actual':'a'*64,'desired':'a'*64}))
PY
chmod 755 "$state/bin/incus"
export PATH="$state/bin:$PATH" GIT_FETCH_FIXTURE_CALLS="$state/calls"
export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1
export SUBYARD_OPERATOR_HOME="$state" SUBYARD_CONFIG_DIR="$root/config" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export STORAGE_PATH="$state/storage" HOST_BASE="$state/host" RESTRICTED_DISK_PATHS="$state/host"
export ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME=yard-fixture INCUS_PROJECT=subyard-fixture INCUS_BRIDGE=fixture SSH_HOST=127.0.0.1
export DEV_USER=dev DEV_UID=1000 DEV_SUDO=0 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0
export SUBYARD_PROFILE_SELECTED=1 ENVIRONMENT_PROFILES=git-fetch
bash "$root/config/profiles/git-fetch/owner.sh" --yes
python3 - "$state/calls" <<'PY'
import json,sys
assert json.load(open(sys.argv[1]))==['apply','1','dev','0']
PY
bash "$root/config/profiles/git-fetch/owner.sh" --remove
python3 - "$state/calls" <<'PY'
import json,sys
assert json.load(open(sys.argv[1]))==['apply','0','dev','0']
PY
[ "$(bash "$root/config/profiles/git-fetch/owner.sh" --pause)" = paused ]
GIT_FETCH_FIXTURE_STATE=STOPPED bash "$root/config/profiles/git-fetch/owner.sh" --check
expect_exit() {
  local expected=$1 rc=0
  shift
  "$@" >/dev/null 2>&1 || rc=$?
  [ "$rc" = "$expected" ] || { printf 'unexpected lifecycle exit %s (expected %s)\n' "$rc" "$expected" >&2; exit 1; }
}
expect_exit 1 env GIT_FETCH_FIXTURE_DRIFT=1 bash "$root/config/profiles/git-fetch/owner.sh" --check
expect_exit 2 env GIT_FETCH_FIXTURE_FAIL=list bash "$root/config/profiles/git-fetch/owner.sh" --check
expect_exit 2 env GIT_FETCH_FIXTURE_FAIL=exec bash "$root/config/profiles/git-fetch/owner.sh" --check
expect_exit 2 env GIT_FETCH_FIXTURE_STATE=ERROR bash "$root/config/profiles/git-fetch/owner.sh" --check
bash "$root/config/profiles/git-fetch/runtime.sh" observe > "$state/observation"
python3 - "$state/calls" "$state/observation" <<'PY'
import json,re,sys
args=json.load(open(sys.argv[1]))
assert args[:4]==['observe','1','dev','0'] and len(args)==5 and re.fullmatch('[a-f0-9]{64}',args[4])
assert json.load(open(sys.argv[2]))['state']=='current'
PY
expected=$(printf '%064d' 0)
ENVIRONMENT_PROFILES='' bash "$root/config/profiles/git-fetch/runtime.sh" apply operation "$expected" "$expected" > /dev/null
python3 - "$state/calls" "$expected" <<'PY'
import json,sys
args=json.load(open(sys.argv[1]))
assert args[:4]==['refresh','0','dev','0'] and args[5:]==['operation',sys.argv[2],sys.argv[2]]
PY
[ "$(GIT_FETCH_FIXTURE_STATE=STOPPED bash "$root/config/profiles/git-fetch/runtime.sh" observe)" = '{"state":"deferred"}' ]
[ "$(GIT_FETCH_FIXTURE_STATE='' bash "$root/config/profiles/git-fetch/runtime.sh" observe)" = '{"state":"absent"}' ]
expect_exit 2 env GIT_FETCH_FIXTURE_FAIL=exec bash "$root/config/profiles/git-fetch/runtime.sh" observe
ENVIRONMENT_PROFILES='' SUBYARD_YARD=default bash "$root/config/profiles/git-fetch/runtime.sh" observe > /dev/null
python3 - "$root/config/profiles/git-fetch" "$state/calls.argv" <<'PY'
import json,pathlib,runpy,sys
profile=pathlib.Path(sys.argv[1])
fixture=runpy.run_path(str(profile/'tests/release-observation.py'))['read_only_commands']
commands=fixture(profile,project='subyard-fixture',instance='yard-fixture')
assert len(commands)==1
arguments,response=commands[0]
assert json.loads(pathlib.Path(sys.argv[2]).read_text())==arguments
assert json.loads(response)=={'state':'absent'}
assert fixture(profile,yard='named',project='subyard-fixture',instance='yard-fixture')[0][0][-1]!=arguments[-1]
PY
expect_exit 2 env GIT_FETCH_FIXTURE_STATE=STOPPED bash "$root/config/profiles/git-fetch/runtime.sh" apply operation "$expected" "$expected"
if SUBYARD_ENGINE_CONTEXT=0 bash "$root/config/profiles/git-fetch/owner.sh" --yes >/dev/null 2>&1; then
  echo 'FAIL: unprepared owner invocation accepted' >&2; exit 1
fi
printf 'ok: git-fetch owner transport, lifecycle and release activation selection/absence/stopped/errors\n'

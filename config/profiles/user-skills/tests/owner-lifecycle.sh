#!/usr/bin/env bash
# Run owner.sh and its real guest script plus sync.py through a fake Incus
# boundary: the guest runs locally as the current user with stubbed identity tools.
set -euo pipefail
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
owner="$root/config/profiles/user-skills/owner.sh"
state="$(mktemp -d /tmp/subyard-user-skills-owner.XXXXXX)"
trap 'chmod -R u+rwX -- "$state" 2>/dev/null || :; rm -rf -- "$state"' EXIT
mkdir -p "$state/bin" "$state/guest-bin" "$state/git-bin" "$state/home"
cat > "$state/bin/incus" <<'PY'
#!/usr/bin/env python3
import io, json, os, subprocess, sys, tarfile
args = sys.argv[1:]
state = os.environ['FIXTURE_STATE']
if args[0] == 'list':
    print(args[1] + ',' + os.environ.get('FIXTURE_POWER', 'RUNNING'))
    sys.exit()
assert args[0] == 'exec' and args[1:4] == ['yard-fixture', '--project', 'subyard-fixture'], args
script, arguments = args[args.index('-c') + 1], args[args.index('subyard') + 1:]
with open(state + '/calls', 'a') as calls:
    calls.write(json.dumps(arguments) + '\n')
source = io.BytesIO(sys.stdin.buffer.read())
patched = io.BytesIO()
with tarfile.open(fileobj=source, mode='r:') as archive, tarfile.open(fileobj=patched, mode='w') as output:
    names = sorted(member.name for member in archive.getmembers() if member.isfile())
    assert names == ['agent-skills.py', 'skills/subyard-user-skills/SKILL.md', 'skills/subyard-user-skills/sync.py'], names
    for member in archive.getmembers():
        data = archive.extractfile(member).read() if member.isfile() else None
        if member.name == 'agent-skills.py':
            # Core delivery has its own contracts; the stub records the verb.
            data = ('import os,sys\nopen(os.environ["FIXTURE_STATE"]+"/skills","a").write(sys.argv[1]+"\\n")\n'
                    'sys.exit(int(os.environ.get("FIXTURE_SKILLS_"+sys.argv[1].replace("-","_").upper(),"0")))\n').encode()
        if member.name.endswith('sync.py'):
            data = data.replace(b'LOCK_WAIT = 330', b'LOCK_WAIT = 1')
            data = data.replace(b"SAFE_PATH = '/usr/local/sbin:", ("SAFE_PATH = '" + state + "/git-bin:").encode())
        if data is not None:
            member.size = len(data)
        output.addfile(member, io.BytesIO(data) if data is not None else None)
environment = dict(os.environ, PATH=os.environ['FIXTURE_GUEST_PATH'])
result = subprocess.run(['bash', '-euo', 'pipefail', '-c', script, 'subyard', *arguments],
                        input=patched.getvalue(), env=environment, stdout=subprocess.PIPE)
sys.stdout.buffer.write(result.stdout)
sys.exit(result.returncode)
PY
cat > "$state/guest-bin/setpriv" <<'SH'
#!/bin/bash
# Drop the identity options; the fixture guest already runs as the developer.
while [ "$1" != env ]; do shift; done
exec "$@"
SH
cat > "$state/guest-bin/getent" <<SH
#!/bin/bash
[ -z "\${FIXTURE_NO_DEV:-}" ] || exit 2
[ "\$1 \$2" = "passwd dev" ] && printf 'dev:x:1000:1000::%s:/bin/bash\n' "$state/home"
SH
chmod 755 "$state/bin/incus" "$state/guest-bin/setpriv" "$state/guest-bin/getent"
cat > "$state/guest-bin/id" <<'SH'
#!/bin/bash
[ "$2" = dev ] || exit 1
exec /usr/bin/id "$1"
SH
chmod 755 "$state/guest-bin/id"
# The sync helper's Git: the source answers only once access is granted.
cat > "$state/git-bin/git" <<SH
#!/bin/bash
[ "\$1 \$2" = "clone --" ] && [ -e "$state/access" ] && [ "\${3%skills.git}" != "\$3" ] || exit 128
mkdir -p "\$4/skills/alpha"
printf -- '---\nname: alpha\ndescription: a\n---\n' > "\$4/skills/alpha/SKILL.md"
SH
chmod 755 "$state/git-bin/git"
export FIXTURE_STATE="$state" FIXTURE_GUEST_PATH="$state/guest-bin:$PATH"
export PATH="$state/bin:$PATH"
export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1
export SUBYARD_OPERATOR_HOME="$state" SUBYARD_CONFIG_DIR="$root/config" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export STORAGE_PATH="$state/storage" HOST_BASE="$state/host" RESTRICTED_DISK_PATHS="$state/host"
export ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME=yard-fixture INCUS_PROJECT=subyard-fixture INCUS_BRIDGE=fixture SSH_HOST=127.0.0.1
export DEV_USER=dev DEV_UID=1000 DEV_SUDO=no FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0
export SUBYARD_PROFILE_SELECTED=1 ENVIRONMENT_PROFILES=user-skills
# Connection refused at once: the source is unavailable without network access.
export USER_SKILL_SOURCES='https://127.0.0.1:9/example/skills.git'
skills_root="$state/home/.local/share/subyard-user-skills"
fail() { printf 'user-skills owner: %s\n' "$*" >&2; exit 1; }
expect() { # <exit> <stderr> -- owner arguments
  local want="$1" want_err="$2" rc=0
  shift 3
  bash "$owner" "$@" 2>"$state/stderr" >"$state/stdout" || rc=$?
  [ "$rc" = "$want" ] || fail "$* exit $rc, want $want: $(cat "$state/stderr")"
  [ "$(cat "$state/stderr")" = "$want_err" ] || fail "$* stderr: $(cat "$state/stderr")"
  [ ! -s "$state/stdout" ] || fail "$* wrote stdout"
}
record='subyard-profile-readiness-v1 source-unavailable'

# Before apply: the skill is unregistered and sources.json is absent.
FIXTURE_SKILLS_CHECK_REGISTERED=10 expect 1 '' -- --check
# Apply delivers the skill even without access; the check then reports only access.
expect 0 '' -- --yes
[ "$(cat "$state/skills")" = $'check-registered\napply' ] || fail "skill verbs: $(cat "$state/skills")"
python3 -c 'import json,sys; assert [s["name"][:7] for s in json.load(open(sys.argv[1]))] == ["skills-"]' "$skills_root/sources.json"
grep -Fq 127.0.0.1 "$state/calls" || fail 'sources were not passed to the guest'
expect 1 "$record" -- --check
# Access appears: a check still reports the missing clone; the next init-side apply clones it.
clone="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[0]["dir"])' "$skills_root/sources.json")"
touch "$state/access"
expect 1 "$record" -- --check
[ ! -e "$clone" ] || fail 'a check cloned'
expect 0 '' -- --yes
[ -f "$clone/skills/alpha/SKILL.md" ] || fail 'apply did not clone once access appeared'
[ "$(readlink "$state/home/.claude/skills/alpha")" = "$clone/skills/alpha" ] || fail 'skill link missing'
expect 0 '' -- --check
# A registered-skill failure is a transport failure, never drift.
FIXTURE_SKILLS_CHECK_REGISTERED=1 expect 2 '' -- --check
# A held lock makes apply busy without changes; check answers at once.
exec {lock}>"$skills_root/.lock"
flock "$lock"
expect 1 'user-skills: another user-skills sync holds the lock; retry' -- --yes
expect 0 '' -- --check
exec {lock}>&-
# Deselection: links and sources.json are work; --yes removes them, keeps the clone.
SUBYARD_PROFILE_SELECTED=0 FIXTURE_SKILLS_CHECK_REMOVED=10 expect 1 '' -- --check
SUBYARD_PROFILE_SELECTED=0 expect 0 '' -- --yes
[ ! -e "$skills_root/sources.json" ] && [ ! -L "$state/home/.claude/skills/alpha" ] || fail 'deselection left links'
[ -f "$clone/skills/alpha/SKILL.md" ] || fail 'deselection removed the clone'
SUBYARD_PROFILE_SELECTED=0 expect 0 '' -- --check
# A multi-line value (one URL per line) passes every URL and never globs.
two=$'https://127.0.0.1:9/example/skills.git\n\thttps://127.0.0.1:9/example/other.git\n'
USER_SKILL_SOURCES="$two" expect 0 '' -- --yes
tail -n 1 "$state/calls" | grep -Fxq '["apply", "1", "dev", "https://127.0.0.1:9/example/skills.git", "https://127.0.0.1:9/example/other.git"]' \
  || fail "multi-line arguments: $(tail -n 1 "$state/calls")"
python3 -c 'import json,sys; assert len(json.load(open(sys.argv[1]))) == 2' "$skills_root/sources.json"
[ -L "$state/home/.claude/skills/alpha" ] || fail 'multi-line apply left no link'
(cd "$state" && USER_SKILL_SOURCES='*' expect 2 '' -- --check)
tail -n 1 "$state/calls" | grep -Fxq '["check", "1", "dev", "*"]' || fail "glob arguments: $(tail -n 1 "$state/calls")"
# A selected empty list converges like a deselection: no sources.json, no links, clones kept.
USER_SKILL_SOURCES='' expect 1 '' -- --check
USER_SKILL_SOURCES='' expect 0 '' -- --yes
tail -n 1 "$state/calls" | grep -Fxq '["apply", "1", "dev"]' || fail "empty-list arguments: $(tail -n 1 "$state/calls")"
[ ! -e "$skills_root/sources.json" ] && [ ! -L "$state/home/.claude/skills/alpha" ] || fail 'empty list left links'
[ -f "$clone/skills/alpha/SKILL.md" ] || fail 'empty list removed the clone'
USER_SKILL_SOURCES='' expect 0 '' -- --check
expect 0 '' -- --remove
tail -n 1 "$state/calls" | grep -Fxq '["apply", "0", "dev"]' || fail "remove arguments: $(tail -n 1 "$state/calls")"
# Without the developer account a deselected profile is current; a selected one is work.
FIXTURE_NO_DEV=1 SUBYARD_PROFILE_SELECTED=0 expect 0 '' -- --check
FIXTURE_NO_DEV=1 expect 1 '' -- --check
FIXTURE_NO_DEV=1 SUBYARD_PROFILE_SELECTED=0 expect 0 '' -- --yes
FIXTURE_NO_DEV=1 expect 2 'user-skills: skill delivery or source sync failed' -- --yes
# Stopped yards and never-provisioned removal stay outside the guest.
: > "$state/calls"
FIXTURE_POWER=STOPPED expect 0 '' -- --check
SUBYARD_GUEST_NEVER_PROVISIONED=1 expect 0 '' -- --remove
expect 0 '' -- --pause
expect 0 '' -- --resume
[ ! -s "$state/calls" ] || fail 'no-op verbs entered the guest'
expect 2 '' -- --refresh
printf 'user-skills owner lifecycle: ok\n'

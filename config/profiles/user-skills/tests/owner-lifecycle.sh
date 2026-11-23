#!/usr/bin/env bash
# Run the clone/link owner hook and its real guest script through a fake Incus
# boundary: the guest runs locally as the current user with stubbed identity tools.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
owner="$root/config/profiles/user-skills/owner.sh"
state="$(mktemp -d "${TMPDIR:-/tmp}/subyard-user-skills-owner.XXXXXX")"
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
    assert names == ['clone.py'], names
    for member in archive.getmembers():
        data = archive.extractfile(member).read() if member.isfile() else None
        if member.name == 'clone.py':
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
  if [ "$want_err" = warning ]; then
    grep -F >/dev/null 'could not be cloned; authorize Git access or correct USER_SKILL_SOURCES, then run yard init' "$state/stderr" \
      || fail "missing retry warning: $(cat "$state/stderr")"
    if grep -F >/dev/null 127.0.0.1 "$state/stderr"; then fail 'warning leaked the URL'; fi
  else
    [ "$(cat "$state/stderr")" = "$want_err" ] || fail "$* stderr: $(cat "$state/stderr")"
  fi
  [ ! -s "$state/stdout" ] || fail "$* wrote stdout"
}
# Missing sources schedule init, but a failed clone only warns and is retried.
expect 1 '' -- --check
expect 0 warning -- --yes
expect 1 '' -- --check
SUBYARD_PROFILE_POST_APPLY=1 expect 0 '' -- --check
clone="$skills_root/skills-git-$(python3 -c 'import hashlib,os; print(hashlib.sha256(os.environ["USER_SKILL_SOURCES"].encode()).hexdigest()[:8])')"
[ ! -e "$clone" ] || fail 'failed clone left its directory'
touch "$state/access"
expect 0 '' -- --yes
[ -f "$clone/skills/alpha/SKILL.md" ] || fail 'apply did not clone once access appeared'
[ "$(readlink "$state/home/.claude/skills/alpha")" = "$clone/skills/alpha" ] || fail 'skill link missing'
expect 0 '' -- --check
# Deselection removes owned links and keeps the clone.
SUBYARD_PROFILE_SELECTED=0 expect 1 '' -- --check
SUBYARD_PROFILE_SELECTED=0 expect 0 '' -- --yes
[ ! -L "$state/home/.claude/skills/alpha" ] || fail 'deselection left links'
[ -f "$clone/skills/alpha/SKILL.md" ] || fail 'deselection removed the clone'
SUBYARD_PROFILE_SELECTED=0 expect 0 '' -- --check
# A multi-line value (one URL per line) passes every URL and never globs.
two=$'https://127.0.0.1:9/example/skills.git\n\thttps://127.0.0.1:9/example/other.git\n'
USER_SKILL_SOURCES="$two" expect 0 warning -- --yes
tail -n 1 "$state/calls" | grep -Fx >/dev/null '["apply", "1", "dev", "0", "https://127.0.0.1:9/example/skills.git", "https://127.0.0.1:9/example/other.git"]' \
  || fail "multi-line arguments: $(tail -n 1 "$state/calls")"
[ -L "$state/home/.claude/skills/alpha" ] || fail 'multi-line apply left no link'
# An empty list converges like a deselection: owned links removed, clones kept.
USER_SKILL_SOURCES='' expect 1 '' -- --check
USER_SKILL_SOURCES='' expect 0 '' -- --yes
tail -n 1 "$state/calls" | grep -Fx >/dev/null '["apply", "1", "dev", "0"]' || fail "empty-list arguments: $(tail -n 1 "$state/calls")"
[ ! -L "$state/home/.claude/skills/alpha" ] || fail 'empty list left links'
[ -f "$clone/skills/alpha/SKILL.md" ] || fail 'empty list removed the clone'
USER_SKILL_SOURCES='' expect 0 '' -- --check
expect 0 '' -- --remove
tail -n 1 "$state/calls" | grep -Fx >/dev/null '["apply", "0", "dev", "0"]' || fail "remove arguments: $(tail -n 1 "$state/calls")"
# Without the developer account a deselected profile is current; a selected one is work.
FIXTURE_NO_DEV=1 SUBYARD_PROFILE_SELECTED=0 expect 0 '' -- --check
FIXTURE_NO_DEV=1 expect 1 '' -- --check
FIXTURE_NO_DEV=1 SUBYARD_PROFILE_SELECTED=0 expect 0 '' -- --yes
FIXTURE_NO_DEV=1 expect 2 'user-skills: clone or link operation failed' -- --yes
# Stopped yards and never-provisioned removal stay outside the guest.
: > "$state/calls"
FIXTURE_POWER=STOPPED expect 0 '' -- --check
SUBYARD_GUEST_NEVER_PROVISIONED=1 expect 0 '' -- --remove
expect 0 '' -- --pause
expect 0 '' -- --resume
[ ! -s "$state/calls" ] || fail 'no-op verbs entered the guest'
expect 2 '' -- --refresh
printf 'user-skills owner lifecycle: ok\n'

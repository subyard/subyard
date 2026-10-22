#!/usr/bin/env bash
# Execute actual owner network-only dispatch; double only external platform calls.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d /tmp/subyard-openlit-network.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
root="$tmp/source"
install -d -m 0700 "$root/config/profiles/openlit/tests/e2e" "$tmp/bin" "$tmp/state"
cp "$ROOT/config/profiles/openlit/tests/e2e/"{owner.sh,network-only.sh,network-facts.py} "$root/config/profiles/openlit/tests/e2e/"
cp "$ROOT/config/profiles/openlit/compose.yaml" "$root/config/profiles/openlit/"
# Bind host-only absolute paths to this fixture, never the test host's Incus.
python3 - "$root/config/profiles/openlit/tests/e2e/owner.sh" "$tmp" <<'PY'
from pathlib import Path
import sys
path=Path(sys.argv[1]); root=sys.argv[2]
path.write_text(path.read_text().replace('/usr/bin/incus',root+'/bin/incus')
    .replace('/usr/sbin/sshd',root+'/bin/driver')
    .replace('/var/tmp/subyard-openlit.XXXXXX',root+'/state/yard.XXXXXX'))
PY
export OPENLIT_NETWORK_FIXTURE_ROOT="$tmp"
cat > "$root/config/profiles/openlit/tests/e2e/candidate.sh" <<'CANDIDATE'
openlit_candidate_install() { printf '%s/bin/yard\n' "$OPENLIT_NETWORK_FIXTURE_ROOT"; }
openlit_candidate_command() {
  if [ "$1" = /usr/bin/incus ]; then shift; "$OPENLIT_NETWORK_FIXTURE_ROOT/bin/incus" "$@"; else "$@"; fi
}
CANDIDATE
cat > "$tmp/bin/driver" <<'PY'
#!/usr/bin/python3
import json,os,pathlib,sys
root=pathlib.Path(os.environ['OPENLIT_NETWORK_FIXTURE_ROOT'])
name=pathlib.Path(sys.argv[0]).name; args=sys.argv[1:]
with (root/'calls').open('a') as stream: stream.write(json.dumps([name,*args])+'\n')
if name=='id':
    print('dev' if args==['-un'] else 'dev incus-admin'); sys.exit()
if name=='sudo':
    assert args[:2] in (['-n','true'],['-n','unshare']); sys.exit()
if name=='yard':
    assert args[0]=='-Y' and args[2:] in (['init','--yes'],['start','--yes'],['teardown','--yes'])
    config=pathlib.Path(os.environ['SUBYARD_CONFIG_HOME'])/'yards'/args[1]/'config.env'
    assert 'CODING_TOOL_INTEGRATIONS=""\n' in config.read_text() and '\nENVIRONMENT_PROFILES=\n' in config.read_text()
    sys.exit()
if name=='incus':
    if args[0]=='query': assert args==['query','/1.0/instances?project=default']; print('{}')
    elif args[0]=='storage': assert args==['storage','show','default','--project','default']
    elif args[0]=='list': print('STOPPED')
    elif args[0]=='config': assert args[1]=='get' and args[3]=='user.subyard.desired_power'; print('stopped')
    else:
        assert args[0]=='exec'; command=args[args.index('--')+1:]
        if command==['test','!','-e','/var/lib/subyard/openlit']: pass
        else:
            assert command==['python3','-','probe','--location','guest','--registry-ip','44.1.2.3']
            os.execvp(command[0],command)
    sys.exit()
if name=='getent': assert args==['ahostsv4','registry-1.docker.io']; print('44.1.2.3 STREAM synthetic')
elif name=='curl': print('401 0.01 0.02 0.03 0.04 44.1.2.3'); print('PRIVATE_NETWORK_SENTINEL',file=sys.stderr)
elif name=='ip': print('[]')
else: sys.exit(99)
PY
chmod 0700 "$tmp/bin/driver"
for name in id sudo yard incus getent curl ip go sg ssh ssh-keygen unshare nsenter docker; do ln -s driver "$tmp/bin/$name"; done
PATH="$tmp/bin:$PATH" SUBYARD_E2E_VM=1 SUBYARD_E2E_MEMORY_PER_VM=8GiB \
  bash "$root/config/profiles/openlit/tests/e2e/owner.sh" container network-only > "$tmp/output" 2> "$tmp/error"
grep -Fxq 'openlit_e2e_network_diagnostic=completed connectivity=not-asserted image-delivery=not-tested' "$tmp/output"
! grep -q PRIVATE_NETWORK_SENTINEL "$tmp/output" "$tmp/error"
python3 - "$tmp" <<'PY'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1]); calls=[json.loads(line) for line in (root/'calls').read_text().splitlines()]
assert [row[3:] for row in calls if row[0]=='yard']==[['init','--yes'],['start','--yes'],['teardown','--yes']]
rows=[json.loads(line.split('=',1)[1]) for line in (root/'output').read_text().splitlines() if line.startswith('openlit_e2e_network=')]
for location in ('owner','guest'):
    assert {row['mode'] for row in rows if row['location']==location} >= {'registry-default','registry-ipv4','registry-direct-ipv4','registry-direct-resolved-ipv4','dockerhub-auth','ghcr','ghcr-auth'}
state=next((root/'state').glob('yard.*'))
assert json.loads((state/'network-addresses.json').read_text())==['44.1.2.3']
assert (state/'network-addresses.json').stat().st_mode & 0o777==0o600
assert not any(row[0]=='docker' for row in calls)
PY
printf 'ok: owner network-only empty selection, init/start and shared probes exit before image/CLI/profile actions\n'

#!/usr/bin/env bash
# Pinned managed predecessor -> packaged retirement, interruption, stopped state and rollback.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
request_mode=${1:-live}
case "$request_mode" in live|stopped) ;; *) exit 2 ;; esac
[[ "${SUBYARD_E2E_RUN_ID:-}" =~ ^[0-9a-f]{8}$ ]] || exit 2
fixture_token=$((16#$SUBYARD_E2E_RUN_ID))
set -- run "$fixture_token"
# Reuse only the existing marker-owned operator/runtime snapshot and cleanup leaf.
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
OLD_VERSION=0.14.0
OLD_INSTALLER_SHA256=a78c10910c0886a8d01c0a2e77718b21ce58e45fa7f0de6064bc1f351d56028b
INSTALLER="$STATE_ROOT/subyard-install-v0.14.0.sh"
metadata="$ROOT/.subyard-acceptance/candidate.json"
[ -f "$metadata" ] && [ ! -L "$metadata" ] || die 'packaged current-worktree candidate is required'
CANDIDATE_VERSION="$(jq -er .version "$metadata")"
prepare_fixture
# Use exact prepared release assets; do not build another candidate inside the VM.
install -d -m 0755 "$RELEASE_ROOT"
cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
chmod -R a+rX "$RELEASE_ROOT"
operator_env bash -c 'cat > "$1" <<EOF
AGENTS="codex aiobserver"
AI_OBSERVER_HOST_PORT=22222
ENVIRONMENT_PROFILES=
BASE_IMAGE=subyard-e2e-debian-13-cloud-container
BASE_IMAGE_FALLBACK=images:debian/13/cloud
DEV_UID=2000
FORWARD_SSH_AGENT=0
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
EOF
chmod 0600 "$1"' _ "$OPERATOR_HOME/.config/subyard/config.env"
curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
  "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" -o "$INSTALLER"
[ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] || die 'predecessor installer checksum changed'
chmod 0755 "$INSTALLER"
operator_env "$INSTALLER" --version "$OLD_VERSION" --yes
printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
incus project create "$PROJECT" -c features.images=false -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
operator_yard init --yes
operator_yard start --yes
history=/srv/agents/ai-observer/data/.retirement-history
incus exec "$INSTANCE" --project "$PROJECT" -- sh -eu -c '
  systemctl is-active --quiet subyard-ai-observer.service
  [ -f /usr/local/bin/ai-observer ]
  [ -f /srv/agents/ai-observer/data/ai-observer.duckdb ]
  printf "%s\n" "$2" > "$1"
' subyard "$history" "$MARKER"
database_identity="$(incus exec "$INSTANCE" --project "$PROJECT" -- stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)"
history_image="$(incus exec "$INSTANCE" --project "$PROJECT" -- docker inspect --format '{{.Image}}' subyard-ai-observer)"
history_user="$(incus exec "$INSTANCE" --project "$PROJECT" -- docker inspect --format '{{.Config.User}}' subyard-ai-observer)"
# Seed one genuine native-format record; this is predecessor history, not OpenLIT import.
history_code="$(cat <<'HISTORY'
import datetime,json,sys
from pathlib import Path
stamp=datetime.datetime.now(datetime.timezone.utc).isoformat()
root=Path('/home/dev/.codex/sessions')
# The managed predecessor creates and watches this root. A newly created date
# directory can be registered after its first file event, losing that ingestion.
if not root.is_dir():
    raise SystemExit('predecessor Codex sessions root is missing')
records=[{'timestamp':stamp,'type':'session_meta','payload':{'id':'retirement-fixture','timestamp':stamp,'cwd':'/synthetic/retirement','originator':'retirement-fixture','cli_version':'0.0.0-test','model_provider':'openai','model':'gpt-5'}},
{'timestamp':stamp,'type':'response_item','payload':{'type':'message','role':'user','content':[{'type':'input_text','text':sys.argv[1]}]}}]
path=root/'rollout-retirement-fixture.jsonl'
with path.open('x') as stream:
    stream.write(''.join(json.dumps(record)+'\n' for record in records))
path.chmod(0o644)
HISTORY
)"
incus exec "$INSTANCE" --project "$PROJECT" -- python3 -c "$history_code" "$MARKER"
api_history() {
  local response
  for _ in {1..60}; do
    response="$(incus exec "$INSTANCE" --project "$PROJECT" -- curl -fsS --max-time 3 -G --data-urlencode "search=$MARKER" http://127.0.0.1:8080/api/logs?limit=100 2>/dev/null || true)"
    if jq -e --arg marker "$MARKER" '.. | strings | select(contains($marker))' <<< "$response" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  return 1
}
api_history || die 'predecessor did not ingest the synthetic native history record'
config_before="$(operator_env sha256sum "$OPERATOR_HOME/.config/subyard/config.env")"
assert_history() {
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -eu -c '
    [ "$(cat "$1")" = "$2" ]
    [ -f /srv/agents/ai-observer/data/ai-observer.duckdb ]
  ' subyard "$history" "$MARKER"
  [ "$(incus exec "$INSTANCE" --project "$PROJECT" -- stat -c '%d:%i' /srv/agents/ai-observer/data/ai-observer.duckdb)" = "$database_identity" ] || die 'retirement replaced the persistent database'
}
assert_retired() {
  assert_history
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -eu -c '
    [ ! -e /usr/local/bin/ai-observer ]
    [ ! -e /etc/systemd/system/subyard-ai-observer.service ]
    ! docker container ls -a --format "{{.Names}}" | grep -Fxq subyard-ai-observer
    [ ! -e /var/lib/subyard/openlit ]
  '
  for key in user.subyard.ai_observer_provision user.subyard.ai_observer_proxy; do
    [ -z "$(incus config get "$INSTANCE" "$key" --project "$PROJECT")" ] || die 'retirement retained an Observer receipt'
  done
  ! incus config device list "$INSTANCE" --project "$PROJECT" | grep -Fxq ai-observer || die 'retirement retained an Observer route'
  operator_yard integration status --json | jq -e '.observed=="ready" and .selection.effective==["codex"]' >/dev/null
  [ "$(operator_env sha256sum "$OPERATOR_HOME/.config/subyard/config.env")" = "$config_before" ] || die 'retirement rewrote source-owned settings'
}
if [ "$request_mode" = live ]; then
  printf 'observer_retirement_stage=owned-partial-control-interruption\n'
  # Inject one native adapter failure after its durable control receipt exists.
  # Only the exact marked predecessor cleanup argv can consume this fault.
  fault_root="$OPERATOR_HOME/.subyard-observer-retirement-fixture"
  operator_env install -d -m 0700 "$fault_root"
  operator_env python3 - "$OPERATOR_HOME/.local/bin/incus" "$fault_root" <<'PY'
import pathlib,sys
path,state=sys.argv[1:]
program='#!/usr/bin/python3\nimport os,pathlib,sys\nSTATE='+repr(state)+'\n'
program+='''
arguments=sys.argv[1:]
armed=pathlib.Path(STATE+'/retirement-armed')
if arguments[:8]==['exec','yard','--project','subyard','--','sh','-eu','-c'] and len(arguments)==9 and armed.exists():
    target='rm -f -- "$wrapper" "$marker" "$unit" "$check" "$installed" "$health"'
    if target in arguments[8] and '# subyard-ai-observer-retirement-v1' in arguments[8]:
        armed.unlink()
        pathlib.Path(STATE+'/retirement-interrupted').write_text('owned partial controls')
        arguments[8]=arguments[8].replace(target,'rm -f -- "$wrapper"; exit 79',1)
os.execv('/usr/bin/incus',['incus']+arguments)
'''
pathlib.Path(path).write_text(program)
pathlib.Path(path).chmod(0o755)
pathlib.Path(state+'/retirement-armed').write_text('armed')
PY
  if operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes > "$STATE_ROOT/retirement-interruption.log" 2>&1; then
    die 'owned retirement fault did not interrupt activation'
  fi
  operator_env test -f "$fault_root/retirement-interrupted" || die 'retirement did not reach its owned interruption boundary'
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -eu -c '
    [ -f /var/lib/subyard/ai-observer-retirement ]
    [ ! -e /usr/local/bin/ai-observer ]
    [ -f /etc/systemd/system/subyard-ai-observer.service ]
  '
  assert_history
else
  printf 'observer_retirement_stage=stopped-predecessor-update\n'
  operator_yard stop --yes
fi
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
if [ "$request_mode" = stopped ]; then
  [ "$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)" = STOPPED ] || die 'update started the stopped predecessor'
  operator_yard integration status --json | jq -e '.selection.effective==["codex"]' >/dev/null
  operator_yard start --yes
  operator_yard init --yes
fi
assert_retired
# Read genuine retained records with the exact predecessor binary, isolated from
# all host routes and original data. Its database opener may initialize schema,
# so only a private copy of the quiescent DB and WAL is writable by this assay.
history_assay="$(cat <<'ASSAY'
import hashlib,json,os,re,shutil,signal,subprocess,sys,tempfile,time
from pathlib import Path
image,user,marker,token=sys.argv[1:]
if not re.fullmatch(r'sha256:[0-9a-f]{64}',image) or not re.fullmatch(r'[0-9]+:[0-9]+',user) or not re.fullmatch(r'[0-9a-f]{8}',token):
    raise SystemExit('invalid predecessor history identity')
uid,gid=map(int,user.split(':'))
database=Path('/srv/agents/ai-observer/data/ai-observer.duckdb')
paths=[database]
wal=Path(str(database)+'.wal')
if wal.exists(): paths.append(wal)
def identities():
    result=[]
    for path in paths:
        if path.is_symlink() or not path.is_file(): raise RuntimeError('invalid retained database')
        info=path.stat()
        result.append((info.st_dev,info.st_ino,info.st_mode,hashlib.sha256(path.read_bytes()).hexdigest()))
    return result
def run(argv,seconds=10):
    return subprocess.run(argv,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=seconds,check=True).stdout
before=identities();container=None;cleanup_confirmed=False
def interrupted(_signal,_frame): raise SystemExit('history assay interrupted')
signal.signal(signal.SIGTERM,interrupted)
temporary=tempfile.mkdtemp(prefix='subyard-observer-history-'+token+'-',dir='/tmp')
try:
    root=Path(temporary);os.chmod(root,0o700)
    name='subyard-history-'+root.name
    nonce=root.name
    (root/'owner').write_text(marker);os.chmod(root/'owner',0o600)
    data=root/'data';data.mkdir(mode=0o700)
    for path in paths:
        target=data/path.name;shutil.copyfile(path,target);os.chmod(target,0o600);os.chown(target,uid,gid)
    os.chown(data,uid,gid)
    try:
        container=run(['docker','create','--name',name,'--network','none','--user',user,'--label','org.subyard.retirement-history='+marker,'--label','org.subyard.history-nonce='+nonce,'--mount','type=bind,src='+str(data)+',dst=/app/data','--env','AI_OBSERVER_DATABASE_PATH=/app/data/ai-observer.duckdb',image,'serve']).decode().strip()
        if not re.fullmatch(r'[0-9a-f]{64}',container): raise RuntimeError('invalid assay container identity')
        run(['docker','start',container])
        pid=int(run(['docker','inspect','--format','{{.State.Pid}}',container]))
        if pid<=0: raise RuntimeError('history assay did not start')
        found=False;deadline=time.monotonic()+60
        for _ in range(20):
            if time.monotonic()>=deadline: break
            try:
                payload=run(['nsenter','-t',str(pid),'-n','curl','-fsS','--max-time','3','-G','--data-urlencode','search='+marker,'http://127.0.0.1:8080/api/logs?limit=100'],5)
                value=json.loads(payload)
                found=isinstance(value,dict) and isinstance(value.get('logs'),list) and any(isinstance(row,dict) and isinstance(row.get('body'),str) and marker in row['body'] for row in value['logs'])
                if found: break
            except (subprocess.CalledProcessError,ValueError): pass
            time.sleep(0.2)
        if not found: raise RuntimeError('retained native history marker was not readable')
        print('observer_retained_native_history=verified')
    finally:
        signal.signal(signal.SIGTERM,signal.SIG_IGN)
        try:
            # Creation can succeed in the daemon even if the client is interrupted
            # before returning a CID. Recover only the exact private-copy owner.
            fmt='{"id":{{json .Id}},"image":{{json .Image}},"user":{{json .Config.User}},"nonce":{{json (index .Config.Labels "org.subyard.history-nonce")}},"mounts":{{json .Mounts}}}'
            owned=json.loads(run(['docker','inspect','--format',fmt,name]))
            mounts=owned.get('mounts',[])
            if not (re.fullmatch(r'[0-9a-f]{64}',owned.get('id','')) and owned.get('image')==image and owned.get('user')==user and owned.get('nonce')==nonce and len(mounts)==1 and mounts[0].get('Type')=='bind' and mounts[0].get('Source')==str(data) and mounts[0].get('Destination')=='/app/data'):
                raise RuntimeError('history assay cleanup ownership is unconfirmed')
            run(['docker','rm','--force',owned['id']],20)
            cleanup_confirmed=True
        finally:
            if identities()!=before: raise RuntimeError('history assay changed retained data')
            if wal.exists()!=(wal in paths): raise RuntimeError('history assay changed retained WAL')
finally:
    # Preserve the marked copy if ownership recovery/removal failed.
    if cleanup_confirmed: shutil.rmtree(temporary)
ASSAY
)"
incus exec "$INSTANCE" --project "$PROJECT" -- timeout --signal=TERM --kill-after=45 180 python3 -c "$history_assay" "$history_image" "$history_user" "$MARKER" "$SUBYARD_E2E_RUN_ID"
operator_yard init --yes
assert_retired
printf 'observer_retirement_stage=actual-candidate-rollback-refusal\n'
database_before_rollback="$(incus exec "$INSTANCE" --project "$PROJECT" -- sha256sum /srv/agents/ai-observer/data/ai-observer.duckdb)"
rollback_refusal_code="$(cat "$ROOT/config/retired-integrations/aiobserver/tests/rollback-refusal.py")"
operator_env python3 -c "$rollback_refusal_code" \
  "$OPERATOR_HOME/.config/subyard" "$OPERATOR_HOME/.subyard/runtime" \
  "$OPERATOR_HOME/.local/bin/yard" update --rollback --yes
[ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] || die 'rollback refusal lost the actual candidate'
[ "$(incus exec "$INSTANCE" --project "$PROJECT" -- sha256sum /srv/agents/ai-observer/data/ai-observer.duckdb)" = "$database_before_rollback" ] || die 'rollback refusal changed retained database history'
assert_retired
assert_history
# Actual epoch-2 runtime cannot activate the epoch-1 predecessor. Matching-epoch
# rollback/forward is checked separately by dev/verify-release-upgrades.py using
# its sealed power1-fixture bridge, never reported as actual candidate acceptance.
printf 'observer_retirement_stage=actual-candidate-forward-fixed-point\n'
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
assert_retired
assert_history
printf 'ok: managed Observer retirement %s, retained database/history, repeat, epoch-1 rollback refusal, forward fixed point\n' "$request_mode"

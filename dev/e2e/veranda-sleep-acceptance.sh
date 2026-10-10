#!/usr/bin/env bash
# Two RTC sleeps and a real awake owner share one fresh disposable pair.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
native=''
slot_seen=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] && [ "$slot_seen" = 0 ] || die 'expected one --slot N'
      set_requested_slot "$2" --slot; slot_seen=1; shift 2 ;;
    --native)
      [ "$#" -ge 2 ] && [ -z "$native" ] || die 'expected one --native relative-artifact'
      native="$2"; shift 2 ;;
    -h|--help)
      printf 'Usage: dev/e2e/veranda-sleep-acceptance.sh --slot N --native .build/ARTIFACT\n'
      exit 0 ;;
    *) die 'unknown argument' ;;
  esac
done
[ "$slot_seen" = 1 ] || die '--slot N is required'
[[ "$native" =~ ^\.build/[a-zA-Z0-9._-]+$ ]] || die '--native must name a frozen native test artifact'
LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
chmod 0700 "$LOCAL_TEMP"
EVIDENCE_DIR="$(mktemp -d "$ROOT/.build/veranda-sleep-evidence.XXXXXX")"
chmod 0700 "$EVIDENCE_DIR"
sleep_token="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
# shellcheck disable=SC2034
LEASE_PURPOSE=veranda-sleep
owner_setup=0 owner_ready=0 client_started=0
evidence_failed=0

payload() {
  local vm="$1"; shift
  guest "$vm" env SUBYARD_E2E_RUN_ID="$LEASE_RUN" SUBYARD_E2E_SLOT="$LEASE_SLOT" \
    SUBYARD_E2E_VM="$vm" SUBYARD_E2E_TYPE=subyard-pair SUBYARD_E2E_PURPOSE="$LEASE_PURPOSE" \
    SUBYARD_E2E_SLEEP_TOKEN="$sleep_token" \
    /usr/bin/python3 "${GUEST_DIRS[$vm]}/src/dev/e2e/veranda-sleep.py" "$@"
}
preview() {
  local phase="$1"
  guest 2 bash -c '
    log="$1/$2.log"; : > "$log"; chmod 0600 "$log"
    exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev \
      SUBYARD_E2E_RUN_ID="$3" SUBYARD_E2E_VM=2 SUBYARD_E2E_TYPE=subyard-pair \
      bash -c '\''cd "$1"; shift; exec bash "$@"'\'' subyard \
      "$1/src" "$1/src/dev/e2e/preview-lifecycle.sh" "$2" > "$log" 2>&1
  ' subyard "${GUEST_DIRS[2]}" "$phase" "$LEASE_RUN"
}
retain_evidence() {
  local vm="$1" export_rc retain_rc
  install -m 0600 /dev/null "$EVIDENCE_DIR/vm-$vm.frame.json"
  install -m 0600 /dev/null "$EVIDENCE_DIR/vm-$vm.export.log"
  payload "$vm" --evidence > "$EVIDENCE_DIR/vm-$vm.frame.json" 2> "$EVIDENCE_DIR/vm-$vm.export.log"
  export_rc=$?
  python3 - "$EVIDENCE_DIR" "$vm" "$export_rc" <<'PY'
import base64,hashlib,json,os,pathlib,stat,sys
root=pathlib.Path(sys.argv[1]); vm=int(sys.argv[2]); export_rc=int(sys.argv[3])
metadata={'schema_version':1,'vm':vm,'export_exit_code':export_rc,'result':'failed','reason':'invalid_frame','artifacts':[]}
def write(name,data):
    fd=os.open(root/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    os.fchmod(fd,0o600)
    with os.fdopen(fd,'wb') as stream: stream.write(data)
try:
    info=root.lstat()
    assert stat.S_ISDIR(info.st_mode) and info.st_uid==os.geteuid() and stat.S_IMODE(info.st_mode)==0o700
    frame=root/('vm-'+str(vm)+'.frame.json'); info=frame.lstat()
    assert stat.S_ISREG(info.st_mode) and info.st_uid==os.geteuid() and info.st_nlink==1 and stat.S_IMODE(info.st_mode)==0o600 and info.st_size<=24*1024*1024
    value=json.loads(frame.read_bytes())
    assert type(value) is dict and set(value)=={'schema_version','vm','result','artifacts'}
    assert type(value['schema_version']) is int and value['schema_version']==1 and type(value['vm']) is int and value['vm']==vm
    assert value['result'] in {'complete','incomplete'} and type(value['artifacts']) is list
    names=[item['name'] for item in value['artifacts']]
    assert names in ([['receipt.json'],['receipt.json','native.log']] if vm==1 else [['owner-setup.log']])
    complete=True
    for artifact in value['artifacts']:
        assert type(artifact) is dict and set(artifact)=={'name','original_bytes','retained_bytes','truncated','stable','sha256','data_base64'}
        assert type(artifact['original_bytes']) is int and artifact['original_bytes']>=0
        assert type(artifact['retained_bytes']) is int and 0<=artifact['retained_bytes']<=8*1024*1024
        assert type(artifact['truncated']) is bool and type(artifact['stable']) is bool
        assert artifact['truncated']==(artifact['retained_bytes']<artifact['original_bytes'])
        data=base64.b64decode(artifact['data_base64'],validate=True)
        assert len(data)==artifact['retained_bytes'] and hashlib.sha256(data).hexdigest()==artifact['sha256']
        name='vm-'+str(vm)+'-'+artifact['name']; write(name,data)
        metadata['artifacts'].append({key:item for key,item in artifact.items() if key!='data_base64'} | {'path':name})
        complete &= not artifact['truncated'] and artifact['stable']
    assert value['result']==('complete' if complete else 'incomplete')
    metadata.update(result='complete' if complete and export_rc==0 else 'incomplete',reason='retained' if complete and export_rc==0 else 'export_incomplete')
except (OSError,ValueError,TypeError,KeyError,AssertionError):
    pass
write('vm-'+str(vm)+'.summary.json',(json.dumps(metadata,sort_keys=True)+'\n').encode())
sys.exit(0 if metadata['result']=='complete' else 1)
PY
  retain_rc=$?
  printf 'sleep-evidence: phase=retention vm=%s exit_code=%s summary=%s/vm-%s.summary.json\n' "$vm" "$retain_rc" "$EVIDENCE_DIR" "$vm"
  return "$retain_rc"
}
cleanup_acceptance() {
  local original=$? failed=0 vm rc
  trap - EXIT INT TERM
  set +e
  phase_end "$original"
  for vm in 1 2; do
    [ -n "${GUEST_DIRS[$vm]:-}" ] || continue
    if { [ "$vm" = 1 ] && [ "$client_started" = 1 ]; } || { [ "$vm" = 2 ] && [ "$owner_setup" = 1 ]; }; then
      retain_evidence "$vm" || evidence_failed=1
    fi
    phase_start guest-cleanup "$vm"
    rc=0
    if { [ "$vm" = 1 ] && [ "$client_started" = 1 ]; } || { [ "$vm" = 2 ] && [ "$owner_ready" = 1 ]; }; then
      payload "$vm" --cleanup || rc=1
    fi
    if [ "$vm" = 2 ] && [ "$owner_setup" = 1 ]; then
      preview owner-cleanup || rc=1
    fi
    cleanup_guest "$vm" || rc=1
    phase_end "$rc"
    [ "$rc" = 0 ] || failed=1
  done
  phase_start cleanup/release
  if [ -n "${LEASE_KEEPER_PID:-}" ]; then
    kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
    wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
  fi
  release_lease || failed=1
  case "$LOCAL_TEMP" in /tmp/subyard-agent-e2e.*|"${TMPDIR:-/tmp}"/subyard-agent-e2e.*)
    find "$LOCAL_TEMP" -depth -delete || failed=1 ;; *) failed=1 ;;
  esac
  phase_end "$failed"
  python3 - "$EVIDENCE_DIR" "$original" "$failed" "$evidence_failed" <<'PY'
import json,os,pathlib,sys
root=pathlib.Path(sys.argv[1])
value={'schema_version':1,'original_exit_code':int(sys.argv[2]),'cleanup_failed':sys.argv[3]=='1',
       'evidence_result':'failed' if sys.argv[4]=='1' else 'complete',
       'vm_summaries':[path.name for path in sorted(root.glob('vm-*.summary.json'))]}
fd=os.open(root/'summary.json',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600); os.fchmod(fd,0o600)
with os.fdopen(fd,'w') as stream: json.dump(value,stream,sort_keys=True);stream.write('\n')
PY
  [ "$?" = 0 ] || evidence_failed=1
  printf 'sleep-controller: original_exit_code=%s cleanup_failed=%s evidence_failed=%s evidence_summary=%s/summary.json\n' "$original" "$failed" "$evidence_failed" "$EVIDENCE_DIR"
  [ "$original" != 0 ] || { [ "$failed" = 0 ] && [ "$evidence_failed" = 0 ] || original=3; }
  exit "$original"
}
trap cleanup_acceptance EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

phase_start allocation
acquire_lease
phase_end 0
start_lease_keeper
phase_start packing
bundle="$LOCAL_TEMP/worktree.tar.gz"
build_bundle "$ROOT" "$bundle"
bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
phase_end 0
for vm in 1 2; do
  phase_start transport "$vm"
  prepare_guest "$vm" "$bundle" "$bundle_hash"
  if [ "$vm" = 2 ]; then guest "$vm" chown -R dev:dev "${GUEST_DIRS[$vm]}"; fi
  phase_end 0
done
guest_root="${GUEST_DIRS[1]}"
guest 1 dd "of=$guest_root/peer-key" status=none < "$GUEST_IDENTITY"
guest 1 dd "of=$guest_root/peer-known-hosts" status=none < "$GUEST_KNOWN_HOSTS"
cat > "$LOCAL_TEMP/peer-config" <<EOF
Host peer-admin
    HostName ${VM_IP[2]}
    Port 22
    User root
    IdentityFile $guest_root/peer-key
    IdentitiesOnly yes
    BatchMode yes
    StrictHostKeyChecking yes
    HostKeyAlias e2e-vm-2
    UserKnownHostsFile $guest_root/peer-known-hosts
    GlobalKnownHostsFile /dev/null
    ConnectTimeout 5
    LogLevel ERROR
EOF
chmod 0600 "$LOCAL_TEMP/peer-config"
guest 1 dd "of=$guest_root/peer-config" status=none < "$LOCAL_TEMP/peer-config"
printf '%s\n' "${GUEST_DIRS[2]}/src" > "$LOCAL_TEMP/peer-source"
chmod 0600 "$LOCAL_TEMP/peer-source"
guest 1 dd "of=$guest_root/peer-source" status=none < "$LOCAL_TEMP/peer-source"
guest 1 chmod 0600 "$guest_root/peer-key" "$guest_root/peer-known-hosts" "$guest_root/peer-config" "$guest_root/peer-source"
phase_start guest 2
# The controller freeze supplies the current engine as .build/yard; no guest build.
guest 2 test -x "${GUEST_DIRS[2]}/src/.build/yard"
owner_setup=1
preview owner-setup
payload 2 --owner-start
owner_ready=1
phase_end 0
phase_start guest 1
client_started=1
payload 1 --acceptance-client --native "$native" --peer-config "$guest_root/peer-config" \
  --destination "root@${VM_IP[2]}:22"
phase_end 0

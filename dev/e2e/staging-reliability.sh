#!/usr/bin/env bash
# Observe one real cold base and one singleton across two boots.
# Controller: timeout --signal=TERM --kill-after=120 7200 dev/agent-e2e.sh \
#   --slot N --type android-test --purpose staging-reliability --vm 1 -- \
#   bash dev/e2e/staging-reliability.sh
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$ROOT"
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ "${SUBYARD_E2E_TYPE:-}" = android-test ] \
  && sudo -n test -f /run/subyard-e2e-lease.json \
  || { echo 'staging-reliability: requires allocated Android VM1' >&2; exit 2; }
readonly -a OUTER_E2E_CONTEXT=("${SUBYARD_E2E_PROJECT:-}" \
  "${SUBYARD_E2E_RUN_ID:-}" "${SUBYARD_E2E_SLOT:-}" "${SUBYARD_E2E_PURPOSE:-}")
TOKEN=''
SUPPORTED_UPGRADE=0
for argument in "$@"; do
  case "$argument" in
    --supported-upgrade) SUPPORTED_UPGRADE=1 ;;
    --*) echo 'staging-reliability: unknown option' >&2; exit 2 ;;
    *) [ -z "$TOKEN" ] || { echo 'staging-reliability: unexpected argument' >&2; exit 2; }; TOKEN="$argument" ;;
  esac
done
TOKEN="${TOKEN:-$(date +%s)$$}"
# Reuse the existing marker-owned owner bootstrap without its broad dispatcher.
# shellcheck disable=SC1090
. <(sed '/^  case "$MODE" in/,$d; /^ROOT=/d' "$ROOT/dev/e2e/p0-guest.sh") staging-reliability "$TOKEN"
PROJECT=subyard-test-yard
INSTANCE=yard-test-yard
YARD=test-yard
RUNNER="$ROOT/dev/agent-e2e.sh"
STATE_PARENT=''
HOLDER_PID=''
HOLDER_STARTED_PID=''
SAMPLER_PID=''
SECOND_BOOT_ACTIVE=0
REAPER_PAUSED=0
REAPER_TIMER_STOPPED=0
BROKER_INSTALLED=0
RELEASE_STAGED=0
MINIMAL_OWNER=0
SUPPORTED_UPGRADE_BOUND=0
OWNER_BASE_IMAGE_FINGERPRINT=''
FAULT_INSTALLED=0
FAULT_ROOT=/run/subyard-staging-incus-fault
# Consumed by existing holder and monotonic recovery helpers.
# shellcheck disable=SC2034
HOLDER_STOP_GRACE_SECONDS=30
# shellcheck disable=SC2034
HOLDER_KILL_GRACE_SECONDS=10
# shellcheck disable=SC1090
. <(sed -n '/^hold_lease() (/,/^)/p; /^start_holder_child() {/,/^}/p; /^stop_holder_child() {/,/^}/p; /^recovery_monotonic_seconds() {/,/^}/p; /^reaper_pause() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh" | sed '/^  acquire_lease /i\
  VM_COUNT_REQUESTED=1\n  resolve_vm_count')

die() { printf 'staging-reliability: %s\n' "$*" >&2; exit 2; }
outer_exec() { timeout --signal=TERM --kill-after=10 90 incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
runtime_yard() { "$SUBYARD_HOME/runtime/current/bin/yard" "$@"; }
status() { timeout --signal=TERM --kill-after=10 "${1:-30}" env SUBYARD_E2E_STATE_DIR="$STATE_PARENT/probe" "$RUNNER" --yard "$YARD" --status --json; }
observe_cold_resources() {
  local phase="$1" expected_base="${2:-}" observation
  case "$phase" in before_acquire|released) ;; *) return 1 ;; esac
  observation="$(status | jq -ce --arg phase "$phase" '
    def bytes: type=="number" and .>=0 and floor==.;
    select(.status=="ok" and (.pool.slots|type)=="array" and (.pool.slots|length)==1) |
    select(.pool.slots[0].slot_id=="slot-001" and (.pool.slots[0].state|type)=="string") |
    .resources as $r | select(($r|type)=="object") |
    select(($r.bases|type)=="array" and all($r.bases[];
      type=="object" and (.type|type)=="string" and
      (.fingerprint|type)=="string" and (.fingerprint|test("^[a-f0-9]{64}$")) and (.current|type)=="boolean")) |
    select(($r.memory_admission|type)=="object" and $r.memory_admission.physical_available_known==true) |
    select([$r.memory_admission.pending_builder_bytes,$r.memory_admission.pending_vm_bytes,$r.reserved_vm_memory_bytes] | all(.[]; bytes)) |
    select(($r|has("errors")|not) or (($r.errors|type)=="array" and all($r.errors[]; type=="string"))) |
    {phase:$phase,slot_state:.pool.slots[0].state,
     bases:[$r.bases[]|{type,fingerprint,current}],builder_present:($r|has("builder")),
     pending_builder_bytes:$r.memory_admission.pending_builder_bytes,
     pending_vm_bytes:$r.memory_admission.pending_vm_bytes,reserved_vm_memory_bytes:$r.reserved_vm_memory_bytes,
     diagnostics_complete:((($r.errors // [])|length)==0)}
  ')" || return
  printf 'STAGING_RESOURCE_OBSERVATION=%s\n' "$observation"
  jq -e --arg expected "$expected_base" '
    .diagnostics_complete and .slot_state=="available" and (.builder_present|not) and
    .pending_builder_bytes==0 and .pending_vm_bytes==0 and .reserved_vm_memory_bytes==0 and
    (if .phase=="before_acquire" then (.bases|length)==0
     else any(.bases[]; .type=="subyard-pair" and .fingerprint==$expected and .current) end)
  ' <<<"$observation" >/dev/null
}
reexec_with_incus_group() {
  local command mode=''
  [ "$SUPPORTED_UPGRADE" = 0 ] || mode=--supported-upgrade
  printf -v command 'exec bash %q %q %s' "$ROOT/dev/e2e/staging-reliability.sh" "$TOKEN" "$mode"
  exec sg incus-admin -c "$command"
}
stop_sampler() {
  [ -n "$SAMPLER_PID" ] || return 0
  # Incus exec propagates TERM to its native child; wait before console capture.
  kill -TERM "$SAMPLER_PID" 2>/dev/null || true
  local rc=0
  wait "$SAMPLER_PID" || rc=$?
  SAMPLER_PID=''
  printf 'STAGING_OBSERVER_EXIT={"exit_code":%d}\n' "$rc"
  [ "$rc" = 0 ] || [ "$rc" = 143 ]
}
builder_incident_receipt() {
  outer_exec python3 - <<'PYBUILDERRECEIPT'
import json,pathlib,re,stat
def read_owned(path,mode):
    if path.resolve()!=path: raise RuntimeError('noncanonical builder evidence path')
    info=path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid!=0 or stat.S_IMODE(info.st_mode)!=mode:
        raise RuntimeError('unsafe builder evidence file')
    if info.st_size>2*1024*1024: raise RuntimeError('builder evidence exceeds bound')
    return path.read_text()
def receipt(root,config):
    lines=read_owned(config,0o644).splitlines()
    for expected in ('E2E_VM_STATE_DIR=/var/lib/subyard/test-vms','E2E_BROKER_SOURCE=test-yard'):
        key=expected.split('=',1)[0]+'='
        if [line for line in lines if line.startswith(key)]!=[expected]:
            raise RuntimeError('unexpected builder evidence configuration')
    info=root.lstat()
    if root.resolve()!=root or not stat.S_ISDIR(info.st_mode) or info.st_uid!=0 or stat.S_IMODE(info.st_mode)!=0o700:
        raise RuntimeError('unsafe builder evidence root')
    path=root/'images.json'
    if not path.exists() and not path.is_symlink(): return {}
    registry=json.loads(read_owned(path,0o600))
    if registry.get('schema_version')!=1 or not re.fullmatch('[0-9a-f]{32}',registry.get('owner','')):
        raise RuntimeError('invalid builder evidence registry')
    build=registry.get('build')
    if build is None: return {}
    failure=build.get('readiness_failure')
    if failure is None: return {}
    if (build.get('project')!='subyard-e2e-vms-builder' or failure.get('Project')!=build['project']
        or failure.get('VM')!='e2e-base-1' or not re.fullmatch('[0-9a-f]{64}',build.get('key',''))
        or not re.fullmatch('[0-9]{20}-[0-9a-f]{16}',build.get('incident_id',''))
        or build.get('incident_slot',{}).get('slot_id')!='slot-000'):
        raise RuntimeError('failed builder lacks a matching durable incident receipt')
    return {'incident_id':build['incident_id'],'owner':registry['owner'],'build_key':build['key']}
print(json.dumps(receipt(pathlib.Path('/var/lib/subyard/test-vms'),pathlib.Path('/etc/subyard/test-vms.env'))))
PYBUILDERRECEIPT
}
collect_builder_incident() {
  local receipt="$1" id owner
  [ "$receipt" != '{}' ] || return 0
  id="$(jq -er '.incident_id' <<<"$receipt")" || return 1
  owner="$(jq -er '.owner' <<<"$receipt")" || return 1
  [ "$(outer_exec incus project get subyard-e2e-vms-builder user.subyard.managed)" = test-vms-v1 ] \
    && [ "$(outer_exec incus project get subyard-e2e-vms-builder user.subyard.base_owner)" = "$owner" ] || return 1
  python3 - "$SUBYARD_HOME" "$receipt" <<'PYBUILDERCAPTURE'
import datetime,json,pathlib,re,stat,sys
def capture(home,receipt):
    incident_id=receipt['incident_id']
    if not re.fullmatch('[0-9]{20}-[0-9a-f]{16}',incident_id): raise RuntimeError('invalid builder incident ID')
    root=home/'logs/test-vms-broker-incidents'
    def read(path):
        if path.resolve()!=path: raise RuntimeError('noncanonical host capture path')
        for directory in (root,root/'host'):
            info=directory.lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid!=0 or stat.S_IMODE(info.st_mode)!=0o750:
                raise RuntimeError('unsafe host capture root')
        info=path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid!=0 or stat.S_IMODE(info.st_mode)!=0o640 or info.st_size>2*1024*1024:
            raise RuntimeError('unsafe host capture file')
        return json.loads(path.read_text())
    incident=read(root/(incident_id+'.json'))
    host=read(root/'host'/(incident_id+'.json'))
    resource=json.loads(incident.get('diagnostics',{}).get('resource','{}'))
    if (incident.get('schema_version')!=1 or incident.get('incident_id')!=incident_id
        or incident.get('source')!='test-yard' or incident.get('slot_id')!='slot-000'
        or resource!={'resource_kind':'base_builder','project':'subyard-e2e-vms-builder','vm':'e2e-base-1','build_key':receipt['build_key']}):
        raise RuntimeError('foreign builder incident')
    parse=lambda value: datetime.datetime.fromisoformat(value.replace('Z','+00:00'))
    if parse(host['observed_at'])<parse(incident['created_at']): raise RuntimeError('host capture predates incident')
    if host.get('timing')!='after incident, at host spool collection; kernel window is the preceding 15 minutes':
        raise RuntimeError('unexpected host capture provenance')
    for key in ('host_memory','allocation_memory'):
        memory=host.get(key)
        if memory is not None:
            if (memory.get('outer_host_evidence')!='collected by physical-owner host sink'
                or type(memory.get('available_bytes')) is not int or memory['available_bytes']<0
                or type(memory.get('limit_available')) is not bool):
                raise RuntimeError('invalid physical host memory evidence')
    return incident,host
incident,host=capture(pathlib.Path(sys.argv[1]),json.loads(sys.argv[2]))
print('STAGING_BUILDER_INCIDENT='+json.dumps(incident,separators=(',',':')))
print('STAGING_HOST_OBSERVATION='+json.dumps(host,separators=(',',':')))
PYBUILDERCAPTURE
}
collect_evidence() {
  [ -n "$STATE_PARENT" ] && [ -d "$STATE_PARENT" ] || return 0
  stop_sampler || return 1
  if [ "$BROKER_INSTALLED" != 1 ]; then
    printf 'STAGING_EVIDENCE_UNAVAILABLE=broker_not_verified\n'
    return 0
  fi
  verify_broker_installation || return 1
  # Export durable emergency spool before reaper restoration or owner deletion.
  outer_exec /usr/local/libexec/subyard/test-vms-inner _test-vms-worker spool-export > "$STATE_PARENT/spool.json" || return 1
  printf 'STAGING_SPOOL='
  jq -c . "$STATE_PARENT/spool.json" || return 1
  local builder_receipt
  builder_receipt="$(builder_incident_receipt)" || return 1
  sudo -n systemctl start "$(basename "${SUBYARD_TEST_VMS_SINK_SERVICE_PATH:-subyard-test-vms-host-sink.service}")" || return 1
  # Native Ack removes pending spool entries, not the builder's durable receipt.
  collect_builder_incident "$builder_receipt" || return 1
  local id host
  while IFS= read -r id; do
    [[ "$id" =~ ^[0-9]{20}-[0-9a-f]{16}$ ]] || return 1
    [ "$id" != "$(jq -r '.incident_id // ""' <<<"$builder_receipt")" ] || continue
    host="$SUBYARD_HOME/logs/test-vms-broker-incidents/host/$id.json"
    [ -f "$host" ] && [ ! -L "$host" ] || return 1
    printf 'STAGING_HOST_OBSERVATION='
    jq -c . "$host" || return 1
  done < <(jq -r '(.incidents // [])[].incident_id' "$STATE_PARENT/spool.json")
  printf 'STAGING_BOOT_SAMPLES_BEGIN\n'
  if [ -f "$STATE_PARENT/boot-samples.jsonl" ]; then
    cat "$STATE_PARENT/boot-samples.jsonl" || return 1
  else
    printf '{"unavailable":"boot_observer_not_started"}\n'
  fi
  printf 'STAGING_BOOT_SAMPLES_END\n'
  if [ -f "$STATE_PARENT/${HOLDER_CLIENT:-holder}.log" ]; then
    printf 'STAGING_HOLDER_LOG_BEGIN\n'; cat "$STATE_PARENT/${HOLDER_CLIENT:-holder}.log"; printf 'STAGING_HOLDER_LOG_END\n'
  fi
}
cleanup() {
  local rc=$? cleanup_rc=0 owner_cleanup_allowed=1
  trap - EXIT INT TERM
  set +e
  stop_sampler || cleanup_rc=3
  if [ "$SECOND_BOOT_ACTIVE" = 1 ]; then
    printf 'STAGING_SECOND_BOOT_FAILURE_CONSOLE_BEGIN\n'
    assert_owned_vm && outer_exec incus console e2e-vm-1 --show-log --project subyard-e2e-vms-slot-1 || cleanup_rc=3
    printf 'STAGING_SECOND_BOOT_FAILURE_CONSOLE_END\n'
  fi
  if [ -n "$HOLDER_PID" ]; then
    : > "$STATE_PARENT/${HOLDER_CLIENT:-holder}.release"
    stop_holder_child "$HOLDER_PID" || cleanup_rc=3
  fi
  stop_sampler || cleanup_rc=3
  restore_inventory_fault || { cleanup_rc=3; owner_cleanup_allowed=0; }
  collect_evidence || cleanup_rc=3
  if [ "$REAPER_PAUSED" = 1 ] && [ "$owner_cleanup_allowed" = 1 ]; then
    reaper_pause outer_exec restore "$MARKER" || { cleanup_rc=3; owner_cleanup_allowed=0; }
  fi
  if [ "$REAPER_TIMER_STOPPED" = 1 ]; then
    if [ "$owner_cleanup_allowed" = 1 ]; then
      outer_exec systemctl start subyard-test-vms-lease-reaper.timer || cleanup_rc=3
      outer_exec systemctl start --no-block subyard-test-vms-lease-reaper.service || cleanup_rc=3
    fi
  fi
  if [ "$owner_cleanup_allowed" = 1 ]; then
    retire_minimal_owner || { cleanup_rc=3; owner_cleanup_allowed=0; }
  fi
  [ "$rc" != 0 ] || rc="$cleanup_rc"
  printf 'STAGING_CLEANUP={"fixture_exit_code":%d,"cleanup_exit_code":%d}\n' "$rc" "$cleanup_rc"
  [ "$owner_cleanup_allowed" = 1 ] || exit "$rc"
  (exit "$rc")
  owner_cleanup
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

retire_minimal_owner() {
  [ "$MINIMAL_OWNER" = 1 ] || return 0
  [ "$(incus project get "$PROJECT" user.subyard.p0-image-cache)" = "$MARKER" ] || return 1
  if [ "$BROKER_INSTALLED" = 1 ]; then
    verify_broker_installation && outer_exec /usr/local/libexec/subyard/test-vms-inner _test-vms-worker drain-all || return 1
  fi
  # Shared reclamation verifies the exact instance and volume inventory too.
  reclaim_owner_project_residue "$PROJECT" "$MARKER" || return 1
  MINIMAL_OWNER=0
}

wait_state() {
  local expected="$1" seconds="$2" until now body remaining request_seconds
  until=$(( $(recovery_monotonic_seconds) + seconds ))
  while true; do
    now="$(recovery_monotonic_seconds)"
    remaining=$((until-now))
    [ "$remaining" -gt 10 ] || die "nested slot did not reach $expected within ${seconds}s"
    request_seconds=30
    [ "$request_seconds" -le "$((remaining-10))" ] || request_seconds=$((remaining-10))
    body="$(status "$request_seconds")" || return
    if jq -e --arg state "$expected" '.pool.slots[0].state == $state' <<<"$body" >/dev/null; then
      printf '%s\n' "$body"
      return
    fi
    sleep 2
  done
}
assert_owned_vm() {
  [ "$(outer_exec incus project get subyard-e2e-vms-slot-1 user.subyard.managed)" = test-vms-v1 ] \
    && [ "$(outer_exec incus config get e2e-vm-1 user.subyard.managed --project subyard-e2e-vms-slot-1)" = test-vms-v1 ] \
    && [ "$(outer_exec incus config get e2e-vm-1 user.subyard.generation --project subyard-e2e-vms-slot-1)" = "$HOLDER_GENERATION" ] \
    && [ "$(outer_exec incus config get e2e-vm-1 user.subyard.lease-epoch --project subyard-e2e-vms-slot-1)" = "$HOLDER_EPOCH" ] \
    || die 'nested VM ownership mismatch'
}

verify_broker_installation() {
  local expected actual
  [ "$(timeout 5 incus project get "$PROJECT" user.subyard.p0-image-cache 2>/dev/null)" = "$MARKER" ] \
    && [ "$(timeout 5 incus config get "$INSTANCE" user.subyard.managed --project "$PROJECT" 2>/dev/null)" = true ] \
    && [ "$(timeout 5 incus config get "$INSTANCE" user.subyard.name --project "$PROJECT" 2>/dev/null)" = "$YARD" ] || return 1
  expected="$(sha256sum "$SUBYARD_HOME/runtime/current/bin/yard-engine" | cut -d ' ' -f1)" || return
  actual="$(timeout --signal=TERM --kill-after=2 5 incus exec "$INSTANCE" --project "$PROJECT" -- \
    sha256sum /usr/local/libexec/subyard/test-vms-inner 2>/dev/null | cut -d ' ' -f1)" || return
  [ "$actual" = "$expected" ]
}

sample_cold_capacity() {
  local phase="$1" disk total used extra broker_known=0
  disk="$(outer_exec df -B1 --output=size,used,avail /srv/incus-e2e/storage | awk 'NR==2 {print $1,$2,$3}')" || disk=''
  before="$(status)" || before=''
  if jq -e 'type=="object" and (.resources | type=="object")' <<<"$before" >/dev/null 2>&1; then
    broker_known=1
    printf 'STAGING_COLD_CAPACITY='
    jq -c --arg phase "$phase" '{phase:$phase,memory:.resources.memory,memory_admission:.resources.memory_admission,budgets:.resources.budgets,storage:.resources.storage}' <<<"$before"
  else
    printf 'STAGING_COLD_CAPACITY={"phase":"%s","error_kind":"telemetry_unavailable"}\n' "$phase"
  fi
  read -r total used free extra <<<"$disk"
  if [[ "$total" =~ ^[0-9]{1,18}$ && "$used" =~ ^[0-9]{1,18}$ && "$free" =~ ^[0-9]{1,18}$ ]] && [ -z "$extra" ]; then
    printf 'STAGING_STORAGE_CAPACITY={"phase":"%s","total_bytes":%s,"used_bytes":%s,"available_bytes":%s,"builder_peak_bytes":32212254720,"reserve_bytes":5368709120,"required_bytes":37580963840}\n' "$phase" "$total" "$used" "$free"
  else
    printf 'STAGING_STORAGE_CAPACITY={"phase":"%s","error_kind":"telemetry_unavailable","required_bytes":37580963840}\n' "$phase"
    die 'cold builder storage telemetry unavailable'
  fi
  [ "$broker_known" = 1 ] || die 'cold builder broker telemetry unavailable'
}

sample_disposable_capacity() {
  local phase="$1" measure
  measure="$(cat <<'PYDISPOSABLE'
import json,os,pathlib,shutil,subprocess,sys
phase,scope=sys.argv[1:3]
if scope=='outer_vm':
    paths=dict(zip(('default_go_build','marked_go_build','go_modules','release_staging','release_cache'),sys.argv[3:]))
else:
    paths={'root_go_build':'/root/.cache/go-build','root_go_modules':'/root/go/pkg/mod',
        'dev_go_build':'/home/dev/.cache/go-build','dev_go_modules':'/home/dev/go/pkg/mod',
        'srv_go_build':'/srv/cache/go-build','srv_go_modules':'/srv/cache/go-mod',
        'apt_archives':'/var/cache/apt/archives','apt_lists':'/var/lib/apt/lists',
        'worker_staging':'/usr/local/libexec/subyard/test-vms-inner.new'}
result={'phase':phase,'scope':scope,'go_available':shutil.which('go') is not None}
for key,name in paths.items():
    path=pathlib.Path(name); value=None
    if not path.exists() and not path.is_symlink(): value=0
    elif not path.is_symlink():
        try:
            raw=subprocess.check_output(['du','-sx','-B1','--',str(path)],stderr=subprocess.DEVNULL,timeout=2).split()[0]
            if raw.isdigit() and len(raw)<=18: value=int(raw)
        except (OSError,subprocess.SubprocessError,IndexError): pass
    result[key+'_bytes']=value
print('STAGING_DISPOSABLE_CAPACITY='+json.dumps(result))
PYDISPOSABLE
)"
  python3 - "$phase" outer_vm "$P0_CAPACITY_DEFAULT_BUILD_CACHE" "$P0_CAPACITY_BUILD_CACHE" \
    "$P0_CAPACITY_MODULE_CACHE" "$ROOT/.build/p0-owner-release" "$SUBYARD_HOME/releases/$P0_OWNER_VERSION" <<<"$measure"
  verify_broker_installation || die 'owner changed before disposable category measurement'
  outer_exec python3 - "$phase" owner_container <<<"$measure"
}

reclaim_release_staging() {
  [ "$RELEASE_STAGED" = 1 ] && [ "$BROKER_INSTALLED" = 1 ] || return 1
  case "$ROOT" in /var/tmp/subyard-worktree.*/src|/tmp/subyard-worktree.*/src) ;; *) return 1 ;; esac
  local staged="$ROOT/.build/p0-owner-release"
  [ -d "$staged" ] && [ ! -L "$ROOT/.build" ] && [ ! -L "$staged" ] || return 1
  find "$staged" -depth -delete
  RELEASE_STAGED=0
}

reclaim_supported_upgrade_inputs() {
  [ "$SUPPORTED_UPGRADE" = 1 ] && [ "$SUPPORTED_UPGRADE_BOUND" = 1 ] \
    && p0_capacity_assert_root_marker && verify_broker_installation || return 1
  case "$ROOT" in /var/tmp/subyard-worktree.*/src|/tmp/subyard-worktree.*/src) ;; *) return 1 ;; esac
  local actual='' OWNER_BASE_IMAGE
  if [ "$OWNER_BASE_IMAGE_CREATED" = 1 ]; then
    [[ "$OWNER_BASE_IMAGE_FINGERPRINT" =~ ^[a-f0-9]{64}$ ]] || return 1
    actual="$(incus image list --project default --format json | jq -er \
      --arg alias "${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-subyard-e2e-debian-13-cloud-container}" \
      '.[] | select(any(.aliases[]; .name==$alias)) | .fingerprint')" || return
    [ "$actual" = "$OWNER_BASE_IMAGE_FINGERPRINT" ] || return 1
    ! grep -Fxq "$actual" <<<"$OWNER_BASELINE_IMAGES" || return 1
  fi
  # These transfers have been consumed by extraction and the successful native
  # update/binding. Keep installed current/previous, release caches and all source.
  python3 - "$ROOT" "$OWNER_ROOT" <<'PYCONSUMED' || return
import hashlib,json,os,pathlib,re,runpy,shutil,stat,sys
root,owner=map(pathlib.Path,sys.argv[1:])
targets=[root/'.subyard-acceptance/runtime',root/'.subyard-acceptance/release',
         root.parent/'worktree.tar.gz',owner/'baseline-0.19.0']
def owned(path):
    info=path.lstat()
    if path.resolve()!=path or info.st_uid!=os.geteuid():
        raise RuntimeError('consumed input ancestry is not canonical and owned')
    if not (stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode)):
        raise RuntimeError('consumed input is not a plain directory/file')
    if stat.S_ISREG(info.st_mode) and info.st_nlink!=1:
        raise RuntimeError('consumed input is hardlinked')
    return info
mounts=[]
for line in pathlib.Path('/proc/self/mountinfo').read_text().splitlines():
    mounts.append(pathlib.Path(re.sub(r'\\([0-7]{3})',lambda m:chr(int(m[1],8)),line.split()[4])))
allocated={}
for target in targets:
    boundary=owner if target==targets[-1] else root.parent
    for parent in [target,*target.parents]:
        owned(parent)
        if parent in mounts: raise RuntimeError('consumed input ancestry is mounted')
        if parent==boundary: break
    if any(mount==target or target in mount.parents for mount in mounts):
        raise RuntimeError('consumed input contains a mount')
    if target.is_dir()!=(target!=targets[2]):
        raise RuntimeError('consumed input has an unexpected type')
    entries=[target,*target.rglob('*')] if target.is_dir() else [target]
    allocated[target.name]=sum(owned(path).st_blocks*512 for path in entries)
# Recheck authentic baseline and candidate bytes before any deletion. The native
# runtime installations are separate from these downloaded/extracted inputs.
verifier=runpy.run_path(str(root/'dev/verify-release-upgrades.py'))
verifier['baseline_assets'](owner,targets[-1],verifier['DIRECT_BASELINE'],verifier['DIRECT_BASELINE_SHA256'],'amd64')
metadata_path=root/'.subyard-acceptance/candidate.json'
if not stat.S_ISREG(owned(metadata_path).st_mode):
    raise RuntimeError('candidate metadata is not a plain file')
metadata=json.loads(metadata_path.read_text())
runtime=metadata['runtime_file']
if pathlib.Path(runtime).name!=runtime or runtime in ('','.','..'):
    raise RuntimeError('unsafe candidate runtime name')
with (targets[1]/runtime).open('rb') as stream:
    if hashlib.file_digest(stream,'sha256').hexdigest()!=metadata['runtime_sha256']:
        raise RuntimeError('consumed candidate checksum changed')
for target in targets:
    if target.is_dir(): shutil.rmtree(target)
    else: target.unlink()
print('STAGING_CONSUMED_INPUTS='+json.dumps({'allocated_bytes':allocated,'total_allocated_bytes':sum(allocated.values())}))
PYCONSUMED
  # Reuse the existing recovery reclaim, addressing the exact captured image
  # fingerprint so alias drift cannot delete another image.
  if [ "$OWNER_BASE_IMAGE_CREATED" = 1 ]; then
    OWNER_BASE_IMAGE="$actual"
    reclaim_broker_recovery_capacity || return
    printf 'STAGING_CONSUMED_OWNER_IMAGE={"fingerprint":"%s","created_by_run":true}\n' "$actual"
  fi
}

reclaim_owner_apt_indexes() {
  { [ "$MINIMAL_OWNER" = 1 ] || [ "$SUPPORTED_UPGRADE" = 1 ]; } \
    && p0_capacity_assert_root_marker && verify_broker_installation || return 1
  local scope="$1" executor
  case "$scope" in
    owner_container) executor=(outer_exec) ;;
    outer_vm) executor=(sudo -n timeout --signal=TERM --kill-after=5 30) ;;
    *) return 1 ;;
  esac
  # Installation is complete. These downloaded indexes are reproduced by
  # apt-get update; keep sources, package databases and all native lock inodes.
  "${executor[@]}" python3 - "$scope" "$P0_CAPACITY_STATE_ROOT" "$MARKER" "$(id -u)" \
    "${OUTER_E2E_CONTEXT[@]}" <<'PYAPTINDEX'
import fcntl,json,os,pathlib,pwd,shlex,stat,subprocess,sys
scope,state,marker,uid,project,run,slot,purpose=sys.argv[1:]
root=pathlib.Path('/var/lib/apt/lists')
locks=['/var/lib/dpkg/lock-frontend','/var/lib/dpkg/lock','/var/lib/apt/lists/lock']
def canonical(path):
    if path.resolve()!=path or not path.exists(): raise RuntimeError('unsafe APT metadata path')
    return path.lstat()
def private_file(path,owner,mode):
    item=canonical(path)
    if not stat.S_ISREG(item.st_mode) or item.st_uid!=owner or stat.S_IMODE(item.st_mode)!=mode or item.st_nlink!=1 or item.st_size>4096:
        raise RuntimeError('unsafe APT actor identity file')
    return path.read_text()
if scope=='outer_vm':
    if os.geteuid()!=0: raise RuntimeError('APT actor is not privileged L1')
    lease=json.loads(private_file(pathlib.Path('/run/subyard-e2e-lease.json'),0,0o444))
    if (type(lease.get('schema_version')) is not int or lease['schema_version']!=2
        or any(lease.get(key)!=value or not value for key,value in
            (('project',project),('run',run),('slot',slot),('purpose',purpose)))
        or purpose not in ('staging-reliability','broker-cold-upgrade')): raise RuntimeError('foreign APT actor lease')
    state=pathlib.Path(state);uid=int(uid);item=canonical(state)
    if uid<=0 or not stat.S_ISDIR(item.st_mode) or item.st_uid!=uid or stat.S_IMODE(item.st_mode)!=0o711:
        raise RuntimeError('unsafe APT actor fixture root')
    if private_file(state/'.subyard-p0-marker',uid,0o600)!=marker+'\n': raise RuntimeError('foreign APT actor fixture marker')
elif scope!='owner_container': raise RuntimeError('invalid APT actor scope')
body=subprocess.check_output(['apt-config','shell','LISTS','Dir::State::lists/d'],stderr=subprocess.DEVNULL,timeout=2)
rows=shlex.split(body.decode()) if len(body)<=4096 else []
if len(rows)!=1 or rows[0] not in ('LISTS='+str(root),'LISTS='+str(root)+'/'):
    raise RuntimeError('foreign APT lists configuration')
info=canonical(root)
if not stat.S_ISDIR(info.st_mode) or info.st_uid!=os.geteuid(): raise RuntimeError('foreign APT metadata owner')
held=[]
for name in locks:
    path=pathlib.Path(name); before=canonical(path)
    fd=os.open(path,os.O_RDWR|os.O_NOFOLLOW)
    info=os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.geteuid() or info.st_nlink!=1 or (info.st_dev,info.st_ino)!=(before.st_dev,before.st_ino):
        raise RuntimeError('unsafe APT lock')
    fcntl.lockf(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)
    held.append(fd)
owners={os.geteuid(),pwd.getpwnam('_apt').pw_uid}
indexes=[]
for parent,dirs,files in os.walk(root,followlinks=False):
    for name in dirs+files:
        path=pathlib.Path(parent)/name; canonical(path)
        info=path.lstat()
        if info.st_uid not in owners or not (stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode)):
            raise RuntimeError('foreign APT metadata entry')
        if stat.S_ISREG(info.st_mode):
            if info.st_nlink!=1: raise RuntimeError('linked APT metadata entry')
            if path!=root/'lock': indexes.append(path)
def measure(phase):
    value=None
    try:
        fields=subprocess.check_output(['du','-sx','-B1','--',str(root)],stderr=subprocess.DEVNULL,timeout=2).split()
        if fields and fields[0].isdigit() and len(fields[0])<=18: value=int(fields[0])
    except (OSError,subprocess.SubprocessError): pass
    print('STAGING_DISPOSABLE_CAPACITY='+json.dumps({'phase':phase,'scope':scope,'apt_lists_bytes':value}))
# Preflight the entire tree before deleting anything. Native APT locks stay held.
measure('before_reclaim')
for path in indexes: path.unlink()
measure('after_reclaim')
PYAPTINDEX
}

inventory_fault_config() {
  local action="$1" real_incus=''
  verify_broker_installation || return 1
  if [ "$action" = install ]; then
    real_incus="$(outer_exec sh -eu -c 'command -v incus')" || return
    case "$real_incus" in /usr/bin/incus|/usr/local/bin/incus) ;; *) return 1 ;; esac
  fi
  outer_exec python3 - "$action" "$MARKER" "$real_incus" <<'PYINVENTORYFAULT'
import hashlib,json,os,pathlib,re,stat,sys
action,marker,real=sys.argv[1:]
root=pathlib.Path('/run/subyard-staging-incus-fault')
config=pathlib.Path('/etc/subyard/test-vms.env')
if not re.fullmatch(r'subyard-p0-[0-9]+',marker): raise RuntimeError('invalid fixture marker')
def regular(path):
    if path.resolve()!=path or not stat.S_ISREG(path.lstat().st_mode) or path.stat().st_uid!=os.geteuid():
        raise RuntimeError('unsafe fault file')
def fingerprint(path):
    regular(path); s=path.stat()
    return {'sha256':hashlib.sha256(path.read_bytes()).hexdigest(),'mode':stat.S_IMODE(s.st_mode),'uid':s.st_uid,'gid':s.st_gid}
regular(config)
parent=config.parent.lstat()
if parent.st_uid!=os.geteuid() or stat.S_IMODE(parent.st_mode)&0o022:
    raise RuntimeError('unsafe configuration directory')
def replace_config(body,original):
    # Atomic replacement must stage on the configuration filesystem, not /run.
    target=config.with_name('.'+config.name+'.'+marker+'.candidate')
    fd=os.open(target,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'wb') as stream:
        os.fchmod(stream.fileno(),original['mode']); os.fchown(stream.fileno(),original['uid'],original['gid'])
        stream.write(body); stream.flush(); os.fsync(stream.fileno())
    os.replace(target,config)
backup=root/'backup'; metadata=root/'metadata'; wrapper=root/'incus'; trigger=root/'trigger'
if action=='install':
    if root.exists() or root.is_symlink() or root.parent.resolve()!=root.parent:
        raise RuntimeError('fault staging already exists or is unsafe')
    original=fingerprint(config); body=config.read_bytes()
    root.mkdir(mode=0o700); root.chmod(0o700); (root/'.marker').write_text(marker+'\n'); (root/'.marker').chmod(0o600)
    backup.write_bytes(body); backup.chmod(original['mode']); os.chown(backup,original['uid'],original['gid'])
    rows=body.decode().splitlines()
    if sum(row.startswith('SUBYARD_INNER_INCUS=') for row in rows)>1: raise RuntimeError('ambiguous Incus setting')
    changed=[row for row in rows if not row.startswith('SUBYARD_INNER_INCUS=')]
    candidate=('\n'.join(changed)+'\nSUBYARD_INNER_INCUS='+str(wrapper)+'\n').encode()
    metadata.write_text(json.dumps({'original':original,'candidate_sha256':hashlib.sha256(candidate).hexdigest()})); metadata.chmod(0o600)
    wrapper.write_text('''#!/bin/sh
set -eu
root=/run/subyard-staging-incus-fault
[ "$(cat "$root/.marker")" = "'''+marker+'''" ]
if [ -f "$root/trigger" ] && [ ! -L "$root/trigger" ] && [ "$#" -eq 6 ] &&
  [ "$1" = project ] && [ "$2" = list ] && [ "$3" = --format ] &&
  [ "$4" = csv ] && [ "$5" = -c ] && [ "$6" = n ]; then
  [ "$(cat "$root/trigger")" = "'''+marker+'''" ]
  mv "$root/trigger" "$root/consumed"
  printf '%s\\n' 'Error: Failed to connect to local daemon: p0 targeted inventory fault' >&2
  exit 1
fi
exec '''+real+''' "$@"
'''); wrapper.chmod(0o700)
    trigger.write_text(marker+'\n'); trigger.chmod(0o600)
    if fingerprint(config)!=original or fingerprint(backup)!=original: raise RuntimeError('configuration changed during fault setup')
    replace_config(candidate,original)
else:
    if root.resolve()!=root or not root.is_dir() or root.stat().st_uid!=os.geteuid(): raise RuntimeError('unsafe fault staging')
    regular(root/'.marker')
    if (root/'.marker').read_text()!=marker+'\n': raise RuntimeError('foreign fault marker')
    for path in root.iterdir():
        if path.name not in {'.marker','backup','metadata','incus','trigger','consumed','candidate'}: raise RuntimeError('foreign fault staging entry')
        regular(path)
    if action=='consumed':
        regular(root/'consumed')
        if trigger.exists() or (root/'consumed').read_text()!=marker+'\n': raise RuntimeError('fault was not consumed exactly once')
    elif action=='restore':
        saved=json.loads(metadata.read_text()); original=saved['original']; current=fingerprint(config)
        if fingerprint(backup)!=original or any(current[k]!=original[k] for k in ('mode','uid','gid')):
            raise RuntimeError('fault backup or configuration mode/owner changed')
        if current['sha256'] not in {original['sha256'],saved['candidate_sha256']}: raise RuntimeError('foreign configuration change')
        replace_config(backup.read_bytes(),original)
        if fingerprint(config)!=original: raise RuntimeError('configuration restoration failed')
        for path in root.iterdir(): path.unlink()
        root.rmdir()
    else: raise RuntimeError('invalid fault action')
PYINVENTORYFAULT
}

restore_inventory_fault() {
  [ "$FAULT_INSTALLED" = 1 ] || return 0
  inventory_fault_config restore || return 1
  FAULT_INSTALLED=0
}

verify_singleton_recovery() {
  local held quarantined available uuid lease_project incident_id incident host captured incident_hash host_hash logs release_rc=0 until
  verify_broker_installation || die 'owner changed before recovery acceptance'
  [ "$REAPER_PAUSED" = 1 ] && [ "$REAPER_TIMER_STOPPED" = 1 ] \
    || die 'recovery capture requires the fixture-owned reaper pause'
  reaper_pause outer_exec verify "$MARKER" || die 'reaper pause changed before recovery acceptance'
  stop_sampler
  HOLDER_CLIENT=recovery
  SUBYARD_E2E_STATE_DIR="$STATE_PARENT/$HOLDER_CLIENT" "$RUNNER" --yard "$YARD" --prepare >/dev/null
  start_holder_child hold_lease "$HOLDER_CLIENT" staging-inventory-failure slot-001 \
    >"$STATE_PARENT/$HOLDER_CLIENT.log" 2>&1
  HOLDER_PID="$HOLDER_STARTED_PID"
  until=$(( $(recovery_monotonic_seconds) + 600 ))
  while [ ! -s "$STATE_PARENT/$HOLDER_CLIENT.ready" ]; do
    kill -0 "$HOLDER_PID" && [ ! -e "$STATE_PARENT/$HOLDER_CLIENT.failed" ] || die 'recovery singleton acquire failed'
    [ "$(recovery_monotonic_seconds)" -lt "$until" ] || die 'recovery singleton acquire watchdog expired'
    sleep 1
  done
  IFS=$'\t' read -r _ HOLDER_CONFIG _ _ _ HOLDER_GENERATION HELD_BASE < "$STATE_PARENT/$HOLDER_CLIENT.ready"
  [ "$HELD_BASE" = "$base" ] || die 'recovery allocation changed the validated base'
  held="$(status)"
  HOLDER_EPOCH="$(jq -er '.pool.slots[0].lease_epoch' <<<"$held")"
  lease_project="$(jq -er '.pool.slots[0].project | select(type=="string" and length>0)' <<<"$held")"
  jq -e --argjson generation "$HOLDER_GENERATION" '
    (.pool.slots | length)==1 and (.pool.slots[0] |
      .state=="held" and .purpose=="staging-inventory-failure" and .resource_generation==$generation)
  ' <<<"$held" >/dev/null || die 'recovery lease identity mismatch'
  assert_owned_vm
  uuid="$(outer_exec incus config get e2e-vm-1 volatile.uuid --project subyard-e2e-vms-slot-1)"
  [[ "$uuid" =~ ^[0-9a-f-]{36}$ ]] || die 'recovery VM UUID missing'
  # No status/inventory probe runs while the one-shot trigger is armed.
  FAULT_INSTALLED=1
  inventory_fault_config install || die 'inventory fault setup refused'
  : > "$STATE_PARENT/$HOLDER_CLIENT.release"
  wait "$HOLDER_PID" || release_rc=$?
  HOLDER_PID=''
  [ "$release_rc" -ge 1 ] && [ "$release_rc" -le 255 ] || die 'faulted release unexpectedly succeeded'
  inventory_fault_config consumed || die 'inventory fault was not consumed exactly once'
  quarantined="$(wait_state quarantined 30)"
  jq -e --argjson generation "$HOLDER_GENERATION" --argjson epoch "$HOLDER_EPOCH" --arg project "$lease_project" '
    .pool.slots[0] | .state=="quarantined" and .resource_generation==$generation and .lease_epoch==$epoch and .project==$project and
    (.incident_id | test("^[0-9]{20}-[0-9a-f]{16}$")) and
    (.last_failure_event_id | test("^[0-9]{20}-[0-9a-f]{16}$")) and (has("failure_reason")|not)
  ' <<<"$quarantined" >/dev/null || die 'quarantine identity or incident missing'
  assert_owned_vm
  [ "$(outer_exec incus config get e2e-vm-1 volatile.uuid --project subyard-e2e-vms-slot-1)" = "$uuid" ] \
    || die 'failed release changed VM identity'
  printf 'STAGING_RECOVERY_IDENTITY={"resource_generation":%s,"lease_epoch":%s,"vm_uuid":"%s"}\n' "$HOLDER_GENERATION" "$HOLDER_EPOCH" "$uuid"
  outer_exec sh -eu -c '
    key=/var/lib/subyard/e2e-slots/1/.ssh/authorized_keys
    [ "$(realpath -m "$key")" = "$key" ] && [ -f "$key" ] && [ ! -L "$key" ] && [ ! -s "$key" ]
  ' || die 'quarantined agent was not fenced'
  incident_id="$(jq -er '.pool.slots[0].incident_id' <<<"$quarantined")"
  # Emergency spool and physical-owner observation must exist before recovery.
  collect_evidence || die 'quarantine capture failed'
  incident="$SUBYARD_HOME/logs/test-vms-broker-incidents/$incident_id.json"
  host="$SUBYARD_HOME/logs/test-vms-broker-incidents/host/$incident_id.json"
  for captured in "$incident" "$host"; do
    [ "$(realpath -m "$captured")" = "$captured" ] && [ -f "$captured" ] && [ ! -L "$captured" ] \
      || die 'native incident or host observation missing'
  done
  jq -e --arg id "$incident_id" --arg command "$FAULT_ROOT/incus project list --format csv -c n" \
    --argjson generation "$HOLDER_GENERATION" --argjson epoch "$HOLDER_EPOCH" --arg yard "$YARD" --arg project "$lease_project" '
    .incident_id==$id and .slot_id=="slot-001" and .resource_generation==$generation and .lease_epoch==$epoch and
    (.failure_reason | contains("p0 targeted inventory fault")) and .context.schema_version==2 and .context.yard==$yard and .context.project==$project and
    .command.command==$command and .command.exit_code==1 and (.diagnostics|type)=="object"
  ' "$incident" >/dev/null || die 'incident lost the original inventory failure'
  jq -e '
    .outer_state=="running" and (.observed_at|type)=="string" and
    .host_memory.outer_host_evidence=="collected by physical-owner host sink" and (.host_memory.available_bytes|type)=="number" and
    .allocation_memory.outer_host_evidence=="collected by physical-owner host sink" and (.allocation_memory.available_bytes|type)=="number" and
    (.allocation_memory.limit_available|type)=="boolean" and
    ([.unavailable[]? | select(.=="host_memory" or .=="allocation_cgroup" or .=="allocation_state_and_cgroup")]|length)==0
  ' "$host" >/dev/null || die 'physical host observation was unavailable'
  python3 - "$incident" "$host" <<'PYCAPTURETIME'
import datetime,json,sys
incident,host=(json.load(open(path)) for path in sys.argv[1:])
parse=lambda value: datetime.datetime.fromisoformat(value.replace('Z','+00:00'))
if parse(host['observed_at'])<parse(incident['created_at']): raise RuntimeError('host observation predates incident')
PYCAPTURETIME
  incident_hash="$(sha256sum "$incident")"; host_hash="$(sha256sum "$host")"
  sudo -n systemctl start "$(basename "${SUBYARD_TEST_VMS_SINK_SERVICE_PATH:-subyard-test-vms-host-sink.service}")"
  [ "$(sha256sum "$incident")" = "$incident_hash" ] && [ "$(sha256sum "$host")" = "$host_hash" ] \
    || die 'sink replay changed immutable captured evidence'
  restore_inventory_fault || die 'inventory configuration restoration refused'
  reaper_pause outer_exec restore "$MARKER"
  REAPER_PAUSED=0
  outer_exec systemctl start subyard-test-vms-lease-reaper.timer
  REAPER_TIMER_STOPPED=0
  outer_exec systemctl start --no-block subyard-test-vms-lease-reaper.service
  # Keep the existing recovery deadline; the whole controller cap still applies.
  available="$(wait_state available 6000)"
  jq -e --arg id "$incident_id" --argjson generation "$HOLDER_GENERATION" --argjson epoch "$HOLDER_EPOCH" '
    .pool.slots[0] | .state=="available" and .resource_generation==($generation+1) and .lease_epoch==$epoch and .incident_id==$id
  ' <<<"$available" >/dev/null || die 'native recovery did not advance exactly one generation'
  [ "$(outer_exec incus project get subyard-e2e-vms-slot-1 user.subyard.managed)" = test-vms-v1 ] \
    || die 'recovered project lost its ownership marker'
  outer_exec incus list --project subyard-e2e-vms-slot-1 --format json | jq -e 'length==0' >/dev/null \
    || die 'recovery retained allocation instances'
  outer_exec incus storage volume list default --project subyard-e2e-vms-slot-1 --format json \
    | jq -e 'all(.[]; .type!="virtual-machine" and .type!="container")' >/dev/null \
    || die 'recovery retained allocation root volumes'
  sudo -n systemctl start "$(basename "${SUBYARD_TEST_VMS_SINK_SERVICE_PATH:-subyard-test-vms-host-sink.service}")"
  logs="$(runtime_yard test-vms logs -n 100000 --slot 1)"
  jq -s -e --arg id "$incident_id" --argjson generation "$HOLDER_GENERATION" --argjson epoch "$HOLDER_EPOCH" '
    [.[] | select(.incident_id==$id)] as $events |
    ([$events[] | select(.kind=="slot.quarantined" and .resource_generation==$generation and .lease_epoch==$epoch)]|length)==1 and
    ([$events[] | select(.kind=="recovery.start" and .recovery_attempt==1 and .resource_generation==$generation and .lease_epoch==$epoch)]|length)==1 and
    ([$events[] | select(.kind=="recovery.available" and .resource_generation==($generation+1) and .lease_epoch==$epoch)]|length)==1 and
    all($events[]; .kind!="rebuild.create" and .kind!="recovery.failed") and
    ([$events[]|select(.kind=="slot.quarantined")][0].event_id < [$events[]|select(.kind=="recovery.start")][0].event_id) and
    ([$events[]|select(.kind=="recovery.start")][0].event_id < [$events[]|select(.kind=="recovery.available")][0].event_id)
  ' <<<"$logs" >/dev/null || die 'native quarantine/recovery event sequence missing'
  printf 'STAGING_RECOVERY_EVENTS='
  jq -cs --arg id "$incident_id" '[.[]|select(.incident_id==$id)|{event_id,kind,incident_id,resource_generation,lease_epoch,recovery_attempt}]' <<<"$logs"
  printf 'STAGING_RECOVERY={"quarantine_observed":true,"agent_fenced":true,"incident_immutable":true,"host_observation_verified":true,"recovery_available":true,"allocation_absent":true,"original_release_exit_code":%d}\n' "$release_rc"
}

sample_disk_footprint() {
  local measure
  measure="$(cat <<'PYFOOTPRINT'
import json,pathlib,subprocess,sys
scope=sys.argv[1]
# Fixed totals only. Parent and child categories overlap; never sum them.
paths={'usr':'/usr','usr_lib':'/usr/lib','usr_share':'/usr/share','usr_local':'/usr/local',
    'var':'/var','var_lib':'/var/lib','apt_lists':'/var/lib/apt/lists','var_log':'/var/log',
    'var_cache':'/var/cache','var_tmp':'/var/tmp','home':'/home','root':'/root',
    'srv':'/srv','opt':'/opt','boot':'/boot','incus_images':'/var/lib/incus/images',
    'incus_default_pool':'/var/lib/incus/storage-pools/default'}
if scope=='outer_vm':
    paths.update(source=sys.argv[2],platform=sys.argv[3],owner_state=sys.argv[4])
else:
    paths.update(nested_storage='/srv/incus-e2e/storage')
result={'phase':'after_reclaim','scope':scope}
for key,name in paths.items():
    path=pathlib.Path(name); value=None
    try:
        # Reject symlink ancestors too, rather than attribute foreign content.
        if path.is_absolute() and path.resolve()==path:
            if not path.exists(): value=0
            else:
                raw=subprocess.check_output(['du','-sx','-B1','--',str(path)],stderr=subprocess.DEVNULL,timeout=2).split()[0]
                if raw.isdigit() and len(raw)<=18: value=int(raw)
    except (OSError,RuntimeError,subprocess.SubprocessError,IndexError): pass
    result[key+'_bytes']=value
print('STAGING_DISK_FOOTPRINT='+json.dumps(result))
PYFOOTPRINT
)"
  sudo -n python3 - outer_vm "$ROOT" "$P0_CAPACITY_PLATFORM_ROOT" "$OWNER_ROOT" <<<"$measure"
  verify_broker_installation || die 'owner changed before disk footprint measurement'
  outer_exec python3 - owner_container <<<"$measure"
}

prepare_owner_audit() {
  local logs="$SUBYARD_HOME/logs" path
  [ -d "$SUBYARD_HOME" ] && [ ! -L "$SUBYARD_HOME" ] && [ -O "$SUBYARD_HOME" ] \
    && [ "$(realpath -m -- "$SUBYARD_HOME")" = "$SUBYARD_HOME" ] || return 1
  if [ -e "$logs" ] || [ -L "$logs" ]; then
    [ -d "$logs" ] && [ ! -L "$logs" ] && [ -O "$logs" ] || return 1
  fi
  for path in "$logs/yard.log" "$logs/.yard.lock"; do
    if [ -e "$path" ] || [ -L "$path" ]; then
      [ -f "$path" ] && [ ! -L "$path" ] && [ -O "$path" ] || return 1
    fi
  done
  # Normal init audits as the operator before enabling the root host sink.
  # Use the installed CLI writer; the sink preserves an existing user log root.
  timeout --signal=TERM --kill-after=10 30 env SUBYARD_NO_AUDIT= \
    "$SUBYARD_HOME/runtime/current/bin/yard" help >/dev/null || return 1
  [ -d "$logs" ] && [ ! -L "$logs" ] && [ -O "$logs" ] \
    && [ "$(stat -c '%u:%a' "$logs")" = "$(id -u):700" ] || return 1
  for path in "$logs/yard.log" "$logs/.yard.lock"; do
    [ -f "$path" ] && [ ! -L "$path" ] && [ -O "$path" ] \
      && [ "$(stat -c '%u:%a' "$path")" = "$(id -u):600" ] || return 1
  done
}

owner_adapter() {
  env ASSUME_YES=1 SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1 \
    SUBYARD_USER="$(id -un)" SUBYARD_OPERATOR_HOME="$HOME" \
    SUBYARD_CONFIG_DIR="$OWNER_CONFIG_HOME" SUBYARD_CONFIG_HOME="$OWNER_CONFIG_HOME" SUBYARD_HOME="$SUBYARD_HOME" \
    STORAGE_PATH="$SUBYARD_HOME/incus/storage" HOST_BASE=/srv/subyard-test-yard RESTRICTED_DISK_PATHS=/srv/subyard-test-yard \
    ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME="$INSTANCE" YARD_NAME="$YARD" \
    INCUS_PROJECT="$PROJECT" INCUS_BRIDGE=incusbr0 SSH_HOST='' \
    DEV_USER=dev DEV_UID="$OWNER_DIAGNOSTIC_DEV_UID" DEV_SUDO=0 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=1 \
    SUBYARD_POWER_DESIRED=running STORAGE_POOL=default ROOT_POOL=default INCUS_NETWORK=incusbr0 \
    SRV_POOL=default SRV_VOLUME=yard-srv-test-yard SRV_VOLUME_TYPE=filesystem ROOT_DISK_SIZE='' \
    LIMITS_CPU='' LIMITS_MEMORY='' VM_FREE_PAGE_REPORTING=0 VM_PIN_IPV4=0 \
    YARD_IMAGE="$OWNER_BASE_IMAGE" YARD_IMAGE_FALLBACK="$OWNER_BASE_IMAGE" \
    SUBYARD_SUDO_PREAUTHORIZED=1 \
    SUBYARD_PROFILE_GUEST_ENVIRONMENT_HOOKS='' \
    SUBYARD_TEST_VMS_SINK_ENGINE_SOURCE="$SUBYARD_HOME/runtime/current/bin/yard-engine" \
    bash "$SUBYARD_HOME/runtime/current/scripts/$1" --yes
}

prepare_minimal_owner() {
  local bootstrap="$ROOT/.build/p0-staging-owner" userdata bootstrap_rc=0 recipes="$SUBYARD_HOME/runtime/current"
  [ ! -L "$ROOT/.build" ] || die 'owner driver parent is a symlink'
  [ ! -e "$bootstrap" ] && [ ! -L "$bootstrap" ] || die 'owner driver staging already exists'
  install -d -m 0700 "$bootstrap"
  printf '%s\n' "$MARKER" > "$bootstrap/.subyard-p0-marker"
  chmod 0600 "$bootstrap/.subyard-p0-marker"
  cat > "$bootstrap/main.go" <<'GOSTAGINGOWNER'
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Subyard/Subyard/internal/adapters/testvmsruntime"
)

func main() {
	if len(os.Args) != 6 {
		panic("owner driver requires runtime, data home, CPU, fixture marker and recipes")
	}
	root, home, cpu, marker := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	runner := testvmsruntime.ProcessRunner{}
	actual, _, err := runner.Run(context.Background(), "incus", []string{"project", "get", "subyard-test-yard", "user.subyard.p0-image-cache"}, nil, nil)
	if err != nil || string(actual) != marker+"\n" {
		panic("owner driver project ownership mismatch")
	}
	for key, want := range map[string]string{"user.subyard.managed": "true", "user.subyard.name": "test-yard"} {
		actual, _, err := runner.Run(context.Background(), "incus", []string{"config", "get", "yard-test-yard", key, "--project", "subyard-test-yard"}, nil, nil)
		if err != nil || string(actual) != want+"\n" {
			panic("owner driver instance ownership mismatch")
		}
	}
	backend := testvmsruntime.Backend{
		RepositoryRoot: os.Args[5], DataHome: home, Dispatcher: root + "/bin/yard-engine",
		Project: "subyard-test-yard", Instance: "yard-test-yard", YardName: "test-yard", DesiredPower: "running", Output: os.Stderr,
		Environment: map[string]string{"NESTED_E2E_VMS": "1", "DEV_USER": "dev", "E2E_VM_IMAGE": "images:debian/13/cloud",
			"E2E_VM_CPU": cpu, "E2E_VM_MEMORY": "2GiB", "E2E_VM_DISK": "10GiB", "E2E_VM_SLOT_COUNT": "1", "E2E_VM_BOOT_TIMEOUT": "300"},
	}
	if err := backend.Apply(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if ready, err := backend.Verify(context.Background()); err != nil || !ready {
		fmt.Fprintln(os.Stderr, "owner backend readiness failed", err)
		os.Exit(1)
	}
}
GOSTAGINGOWNER
  go build -o "$bootstrap/owner" "$bootstrap/main.go"
  owner_adapter 02-create-project.sh
  [ "$(incus project get "$PROJECT" user.subyard.p0-image-cache)" = "$MARKER" ] || die 'owner project ownership changed'
  [ -z "$(incus list --project "$PROJECT" --format csv -c n)" ] || die 'owner project was not empty'
  MINIMAL_OWNER=1
  # Cloud-init supplies only SSH/user and updater Git prerequisites.
  # Incus, QEMU and broker installation remain in the product Backend.Apply.
  [ "$OWNER_DIAGNOSTIC_DEV_UID" = 1001 ] || die 'minimal owner requires its fixed disposable UID'
  userdata="$(cat <<'CLOUDOWNER'
#cloud-config
users:
  - name: dev
    uid: 1001
    lock_passwd: true
    shell: /bin/bash
packages:
  - openssh-server
  - git
CLOUDOWNER
)"
  incus init "$OWNER_BASE_IMAGE" "$INSTANCE" --project "$PROJECT" \
    -c boot.autostart=false -c user.subyard.managed=true -c user.subyard.name="$YARD" \
    -c user.subyard.bridge=incusbr0 -c user.subyard.desired_power=running \
    -c user.subyard.initialized=false -c cloud-init.user-data="$userdata" >/dev/null
  owner_adapter 03-create-subyard.sh
  timeout --signal=TERM --kill-after=10 300 incus exec "$INSTANCE" --project "$PROJECT" -- cloud-init status --wait --long
  outer_exec sh -eu -c '
    test "$(id -u dev)" = 1001
    command -v python3 >/dev/null
    command -v sshd >/dev/null
    sshd -t
    systemctl is-active --quiet ssh.service
  '
  "$bootstrap/owner" "$SUBYARD_HOME/runtime/current" "$SUBYARD_HOME" "$diagnostic_cpu" "$MARKER" "$recipes" || bootstrap_rc=$?
  if [ "$bootstrap_rc" != 0 ]; then
    if verify_broker_installation; then BROKER_INSTALLED=1; fi
    return "$bootstrap_rc"
  fi
  verify_broker_installation || die 'minimal owner broker differs from installed candidate'
  BROKER_INSTALLED=1
  # Provisioning owns this private root; a legacy worker-key marker is optional.
  builder_incident_receipt >/dev/null || die 'native builder evidence baseline is unsafe'
  prepare_owner_audit || die 'normal owner audit context is unavailable'
  owner_adapter install-test-vms-host-sink.sh
  # Run-created driver staging is unused after native installation/readiness.
  [ ! -L "$ROOT/.build" ] && [ ! -L "$bootstrap" ] \
    && [ "$(realpath -m -- "$bootstrap")" = "$bootstrap" ] \
    && [ -f "$bootstrap/.subyard-p0-marker" ] && [ ! -L "$bootstrap/.subyard-p0-marker" ] \
    && [ "$(cat "$bootstrap/.subyard-p0-marker")" = "$MARKER" ] || die 'owner driver staging became unsafe'
  find "$bootstrap" -depth -delete
}

# This history uses authentic release bytes and the predecessor's own updater.
# The normal staging fixture retains its independent synthetic owner history.
prepare_supported_upgrade_owner() {
  local baseline assets installer name suffix baseline_owner candidate_engine before_owner after_owner before_state after_state
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  candidate_engine="$(release_candidate_prepare "$ROOT")" || die 'verified frozen candidate required'
  P0_OWNER_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
  [ "$("$candidate_engine" --version)" = "yard-engine $P0_OWNER_VERSION" ] || die 'candidate engine version mismatch'
  baseline=0.19.0
  assets="$(python3 - "$ROOT" "$OWNER_ROOT" <<'PYBASELINE'
import pathlib,runpy,sys
source,output=map(pathlib.Path,sys.argv[1:])
verifier=runpy.run_path(str(source/'dev/verify-release-upgrades.py'))
print(verifier['baseline_assets'](output,None,verifier['DIRECT_BASELINE'],verifier['DIRECT_BASELINE_SHA256'],'amd64'))
PYBASELINE
)"
  # baseline_assets also prints download progress; its path is the final line.
  assets="${assets##*$'\n'}"
  installer="$assets/subyard-install.sh"
  for name in subyard-install.sh subyard-install-runtime-release.sh subyard-install-runtime-release.sh.sha256; do
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 \
      "https://github.com/Subyard/Subyard/releases/download/v$baseline/$name" -o "$assets/$name"
  done
  [ "$(sha256sum "$installer" | cut -d' ' -f1)" = eeacda9ded1cc4c095ecc1e0569cf6fd96a737b3af6ab30ce7804833e3d8f1d1 ] \
    || die 'published baseline installer checksum mismatch'
  env YARD_RELEASE_BASE_URL="file://$assets" YARD_BIN_DIR="$SUBYARD_HOME/bin" \
    YARD_SHELL_RC="$OWNER_CONFIG_HOME/bootstrap.bashrc" YARD_LOGIN_RC="$OWNER_CONFIG_HOME/bootstrap.profile" \
    bash "$installer" --version "$baseline" --yes
  baseline_owner="$SUBYARD_HOME/runtime/current/bin/yard"
  [ "$("$baseline_owner" --version)" = "yard $baseline" ] || die 'authentic predecessor was not installed'
  # Reuse the focused owner bootstrap with the authentic predecessor assets.
  # Its shipped provision hook invokes the predecessor's native reconcile-pool;
  # unrelated core/Docker tooling is outside this broker acceptance contract.
  prepare_minimal_owner
  [ "$(runtime_yard --version)" = "yard $baseline" ] || die 'owner bootstrap replaced authentic predecessor'
  verify_broker_installation || die 'predecessor broker does not match its runtime'
  outer_exec /usr/local/libexec/subyard/test-vms-inner _test-vms-worker status \
    | jq -e '.status=="ok" and .pool.resource_type=="agent-e2e" and
      .pool.resource_id=="test-vms" and (.pool.slots | length)==1 and
      .pool.slots[0].state=="available"' >/dev/null || die 'native predecessor broker state is not available'
  printf 'STAGING_BASELINE_BROKER={"version":"%s","native_owner_state":true}\n' "$baseline"
  # Run the exact candidate Git configuration read as its real guest user.
  # The production leaf drops root to dev; a version string would miss this dependency.
  outer_exec python3 - /home/dev off check \
    < "$ROOT/.subyard-acceptance/runtime/config/profiles/github/git_wiring.py" \
    || die 'candidate Git configuration operation failed before supported update'
  printf 'STAGING_UPDATER_PREREQUISITE={"git_configuration_operation":true,"execution_user":"dev"}\n'
  before_owner="$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")"
  [[ "$before_owner" =~ ^[a-f0-9-]{36}$ ]] || die 'predecessor broker owner missing'
  before_state="$(outer_exec sha256sum /var/lib/subyard/test-vms/leases.json | cut -d' ' -f1)"
  [[ "$before_state" =~ ^[a-f0-9]{64}$ ]] || die 'predecessor lease state missing'
  runtime_yard -Y test-yard test-vms status --json \
    | jq -e '(.resources.bases // []) | length == 0' >/dev/null || die 'predecessor broker already has a cached base'
  # Stage only verified frozen assets into the normal release cache. Keep the
  # authentic caller and installed broker state intact until its supported update.
  install -d -m 0700 "$SUBYARD_HOME/releases/$P0_OWNER_VERSION"
  name="$(jq -er .runtime_file "$ROOT/.subyard-acceptance/candidate.json")"
  for suffix in '' .sha256 .manifest.json .provenance.json; do
    install -m 0600 "$ROOT/.subyard-acceptance/release/$name$suffix" "$SUBYARD_HOME/releases/$P0_OWNER_VERSION/$name$suffix"
  done
  env YARD_RELEASE_BASE_URL="file://$ROOT/.subyard-acceptance/release" \
    "$baseline_owner" update --version "$P0_OWNER_VERSION" --offline --yes
  [ "$(runtime_yard --version)" = "yard $P0_OWNER_VERSION" ] || die 'candidate runtime was not activated'
  verify_broker_installation || die 'activated broker differs from candidate'
  [ "$(sha256sum "$candidate_engine" | cut -d' ' -f1)" = "$(sha256sum "$SUBYARD_HOME/runtime/current/bin/yard-engine" | cut -d' ' -f1)" ] \
    || die 'installed engine differs from frozen candidate'
  after_owner="$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")"
  after_state="$(outer_exec sha256sum /var/lib/subyard/test-vms/leases.json | cut -d' ' -f1)"
  [ "$before_owner" = "$after_owner" ] && [ "$before_state" = "$after_state" ] \
    || die 'upgrade replaced retained broker owner or lease state'
  printf 'STAGING_SUPPORTED_UPGRADE={"baseline":"%s","candidate":"%s","broker_owner":"%s","lease_state_sha256":"%s","retained_owner":true}\n' \
    "$baseline" "$P0_OWNER_VERSION" "$after_owner" "$after_state"
  outer_exec cat /usr/local/libexec/subyard/e2e-recipes/manifest.sha256 \
    | (cd "$ROOT/.subyard-acceptance/runtime" && sha256sum -c - >/dev/null) \
    || die 'installed recipe bytes differ from frozen candidate'
  printf 'STAGING_RECIPE_BINDING='
  outer_exec sh -eu -c 'engine=$(sha256sum /usr/local/libexec/subyard/test-vms-inner | cut -d" " -f1); recipe=$(sha256sum /usr/local/libexec/subyard/e2e-recipes/manifest.sha256 | cut -d" " -f1); printf "{\"engine_sha256\":\"%s\",\"recipe_manifest_sha256\":\"%s\"}\n" "$engine" "$recipe"'
  SUPPORTED_UPGRADE_BOUND=1
}

prepare_owner_go_cache
STATE_PARENT="$OWNER_ROOT/staging-reliability"
p0_capacity_prepare_subtree "$STATE_PARENT"
YARD_BUILD_VERSION="$P0_OWNER_VERSION" dev/build-engine.sh --force >/dev/null
ensure_owner_incus
# These fields are consumed by the sourced marker-owned cleanup and registration.
# shellcheck disable=SC2034
OWNER_BASELINE_IMAGES="$(incus image list --project default --format csv -c f)"
# shellcheck disable=SC2034
OWNER_BASELINE_CAPTURED=1
ensure_owner_base_image
if [ "$OWNER_BASE_IMAGE_CREATED" = 1 ]; then
  OWNER_BASE_IMAGE_FINGERPRINT="$(incus image list --project default --format json | jq -er \
    --arg alias "$OWNER_BASE_IMAGE" '.[] | select(any(.aliases[]; .name==$alias)) | .fingerprint')"
  [[ "$OWNER_BASE_IMAGE_FINGERPRINT" =~ ^[a-f0-9]{64}$ ]] || die 'created owner image fingerprint missing'
fi
# shellcheck disable=SC2034
OWNER_DIAGNOSTIC_VM_MEMORY=2GiB
# Keep the product boot budget and admission reserves, never widen for PASS.
# shellcheck disable=SC2034
OWNER_DIAGNOSTIC_VM_BOOT_TIMEOUT=300
[ ! -e "$ROOT/.build/p0-owner-release" ] && [ ! -L "$ROOT/.build/p0-owner-release" ] \
  || die 'release staging already exists before fixture installation'
if [ "$SUPPORTED_UPGRADE" = 0 ]; then
  install_owner_runtime
  RELEASE_STAGED=1
fi
prepare_owner_image_cache_project "$PROJECT"
diagnostic_cpu=1
[ "$(nproc)" -lt 4 ] || diagnostic_cpu=4
# This focused setup has no whole-init registration migration phase.
registration="$OWNER_YARD_DIR/test-yard/config.env"
[ ! -e "$OWNER_YARD_DIR/test-yard" ] && [ ! -L "$OWNER_YARD_DIR/test-yard" ] \
  || die 'minimal owner registration already exists'
install -d -m 0700 "$OWNER_YARD_DIR/test-yard"
printf '# %s\n' "$MARKER" > "$registration"
chmod 0600 "$registration"
write_owner_registration test-yard test-vms 2224 1 "$diagnostic_cpu"
if [ "$SUPPORTED_UPGRADE" = 1 ]; then
  prepare_supported_upgrade_owner
else
  prepare_minimal_owner
fi
printf 'STAGING_CANDIDATE='
python3 - "$ROOT" "$SUBYARD_HOME/runtime/current/bin/yard-engine" <<'PY'
import hashlib,json,pathlib,runpy,sys
root=pathlib.Path(sys.argv[1]); helper=runpy.run_path(str(root/'dev/release-acceptance.py'))
print(json.dumps({'source_fingerprint':helper['source_fingerprint'](root),'installed_engine_sha256':hashlib.file_digest(open(sys.argv[2],'rb'),'sha256').hexdigest()}))
PY
export SUBYARD_E2E_TEST_MODE=1 SUBYARD_E2E_PROJECT_LABEL=Subyard SUBYARD_E2E_PROJECT_YARD=test-yard SUBYARD_E2E_YARD=test-yard
export SUBYARD_E2E_WORKSPACES_ROOT
SUBYARD_E2E_WORKSPACES_ROOT="$(dirname "$(dirname "$ROOT")")"
export SUBYARD_E2E_ROUTE_REGISTRY="$SUBYARD_HOME/e2e/routes"
SUBYARD_E2E_STATE_DIR="$STATE_PARENT/holder" "$RUNNER" --yard test-yard --prepare >/dev/null
# Compilation and controller preparation are complete. Reclaim only supported
# disposable Go caches, never runtime images, packages or arbitrary user data.
sample_cold_capacity before_reclaim
sample_disposable_capacity before_reclaim
p0_capacity_reclaim_default_go_build_cache
p0_capacity_reclaim_go_module_cache
p0_capacity_remove_build_cache
verify_broker_installation || die 'owner changed before disposable cache cleanup'
outer_exec apt-get clean
reclaim_owner_apt_indexes owner_container || die 'owner APT index cleanup refused'
reclaim_owner_apt_indexes outer_vm || die 'allocated VM APT index cleanup refused'
if [ "$RELEASE_STAGED" = 1 ]; then
  reclaim_release_staging || die 'release staging ownership not verified'
fi
if [ "$SUPPORTED_UPGRADE" = 1 ]; then
  reclaim_supported_upgrade_inputs || die 'consumed upgrade input ownership not verified'
fi
sample_disposable_capacity after_reclaim
sample_disk_footprint
sample_cold_capacity after_reclaim
# A cold builder reserves 30GiB plus the normal 5GiB physical reserve.
[ "$free" -ge $((35*1024*1024*1024)) ] \
  || die 'cold builder cannot preserve normal disk reserve'
jq -e '.resources.memory_admission | .physical_available_known==true and .headroom_bytes >= (2*1024*1024*1024+512*1024*1024)' <<<"$before" >/dev/null || die 'cold builder cannot preserve normal memory admission reserve'
printf 'STAGING_RESOURCE_CONTRACT={"nested_cpu":%s,"memory":"2GiB","disk":"10GiB","boot_timeout_seconds":300,"historical_resource_match":false}\n' "$diagnostic_cpu"
jq -e '(.resources.bases // []) | length == 0'  <<<"$before" >/dev/null || die 'cold base fixture already has a base'
# Keep actual failures intact until durable capture; restore only owned services.
outer_exec systemctl stop subyard-test-vms-lease-reaper.timer
REAPER_TIMER_STOPPED=1
REAPER_PAUSED=1
reaper_pause outer_exec install "$MARKER"
printf 'STAGING_NATIVE_VERSIONS_BEGIN\n'
outer_exec sh -eu -c 'incus version; qemu-system-x86_64 --version | head -n 1; dpkg-query -W -f="\${Package} \${Version}\\n" incus incus-agent qemu-system-x86 ovmf 2>/dev/null || true'
printf 'STAGING_NATIVE_VERSIONS_END\n'
# No QMP socket or consumptive console/ringbuf access while lifecycle is active.
# The watcher reads only verified QEMU proc state and parsed counters.
timeout --signal=TERM --kill-after=10 6400 incus exec "$INSTANCE" --project "$PROJECT" -- \
  python3 - <<'PYTHON' > "$STATE_PARENT/boot-samples.jsonl" 2> "$STATE_PARENT/boot-samples.stderr" &
import hashlib,json,math,pathlib,re,signal,subprocess,time
P=pathlib.Path
verified={}
digests={}
def digest(path):
    key=(str(path),path.stat().st_mtime_ns)
    if key not in digests:
        with open(path,"rb") as f: digests[key]=hashlib.file_digest(f,"sha256").hexdigest()
    return digests[key]
safe=re.compile(r'^[a-zA-Z0-9_.:/-]{1,128}$')
def native(args):
    try: return subprocess.check_output(['incus',*args],timeout=1,stderr=subprocess.DEVNULL).decode()
    except (OSError,subprocess.SubprocessError): return None
def text(path):
    try:
        with open(path) as f: return f.read(4096)
    except OSError: return None
def counters(path):
    body=text(path)
    if body is None: return {'unavailable':True}
    try:
        result={k:int(v) for k,v in (line.split() for line in body.splitlines()) if k in {'usage_usec','user_usec','system_usec','nr_periods','nr_throttled','throttled_usec','low','high','max','oom','oom_kill','oom_group_kill'}}
        if not result or any(v<0 for v in result.values()): raise ValueError()
        return result
    except ValueError: return {'unavailable':True}
def pressure(path):
    body=text(path)
    if body is None: return {'unavailable':True}
    try:
        result={}
        for line in body.splitlines():
            parts=line.split(); fields=dict(p.split('=') for p in parts[1:])
            if parts[0] not in {'some','full'} or set(fields)!={'avg10','avg60','avg300','total'}: raise ValueError()
            values={k:(int(v) if k=='total' else float(v)) for k,v in fields.items()}
            if parts[0] in result or values['total']<0 or any(not math.isfinite(values[k]) or not 0<=values[k]<=100 for k in ['avg10','avg60','avg300']): raise ValueError()
            result[parts[0]]=values
        return result if result else {'unavailable':True}
    except (ValueError,IndexError): return {'unavailable':True}
def qmp_events(project,name):
    records=[]
    for suffix in ['.old','']:
        found=False
        for root in ['/var/log/incus','/var/lib/incus/logs']:
            path=P(root)/(project+'_'+name)/('qemu.qmp.log'+suffix)
            try:
                with open(path,'rb') as f:
                    head=f.read(32768); f.seek(0,2); size=f.tell(); f.seek(max(32768,size-32768)); tail=f.read(32768)
                found=True
            except OSError: continue
            rows=[]
            for line in (head+b'\n'+tail).decode(errors='replace').splitlines():
                match=re.fullmatch(r'\[([^]]+)\] (QUERY|Event): (.+)',line)
                if not match: continue
                try: payload=json.loads(match[3])
                except ValueError: continue
                event=payload.get('execute') if match[2]=='QUERY' else payload.get('event')
                if event not in {'system_reset','cont','quit','RESET','SHUTDOWN','RESUME'}: continue
                if not re.fullmatch(r'[0-9TZ:+.\-]{10,40}',match[1]): continue
                item={'log':path.name,'time':match[1],'kind':match[2],'event':event}
                data=payload.get('data',{})
                if isinstance(data.get('guest'),bool): item['guest']=data['guest']
                if 'reason' in data: item['reason']=data['reason'] if data['reason'] in {'host-qmp-system-reset','guest-reset','host-qmp-quit','guest-shutdown','host-error','host-signal'} else 'unknown'
                stamp=payload.get('timestamp',{})
                if all(isinstance(stamp.get(k),int) for k in ['seconds','microseconds']): item['qmp_timestamp']={k:stamp[k] for k in ['seconds','microseconds']}
                if item not in rows: rows.append(item)
            records.extend(rows[:16]+rows[-16:] if len(rows)>32 else rows)
            break
        if not found: records.append({'log':'qemu.qmp.log'+suffix,'unavailable':True})
    return {'scope':'bounded logfile head/tail; PID is observation identity, not event attribution','records':records}
def sample(project,name):
    r={'project':project,'vm':name,'unavailable':['provided_guest_agent_identity_until_guest_reachable']}

    run=P('/run/incus')/(project+'_'+name); pidbody=text(run/'qemu.pid')
    if pidbody is None or not re.fullmatch(r'[1-9][0-9]*\s*',pidbody):
        r['unavailable'].append('qemu_pid_default_runpath'); return r
    pid=int(pidbody); proc=P('/proc')/str(pid)
    startbody=text(proc/'stat')
    try: start_ticks=int(startbody.rsplit(')',1)[1].split()[19])
    except (TypeError,ValueError,IndexError,AttributeError): r['unavailable'].append('qemu_starttime'); return r
    identity=(project,name,pid,start_ticks)
    if identity not in verified:
        body=native(['query',f'/1.0/instances/{name}?project={project}'])
        try: instance=json.loads(body); cfg=instance['expanded_config']
        except (TypeError,ValueError,KeyError): r['unavailable'].append('marked_instance'); return r
        marker=native(['project','get',project,'user.subyard.managed'])
        if marker is None or marker.strip()!='test-vms-v1' or cfg.get('user.subyard.managed')!='test-vms-v1':
            r['unavailable'].append('ownership'); return r
        if name=='e2e-base-1':
            owner=native(['project','get',project,'user.subyard.base_owner'])
            if owner is None or owner.strip()!=cfg.get('user.subyard.base_owner') or not re.fullmatch('[a-f0-9]{64}',cfg.get('user.subyard.base_key','')):
                r['unavailable'].append('builder_ownership'); return r
        elif not all(re.fullmatch('[1-9][0-9]*',cfg.get(k,'')) for k in ['user.subyard.generation','user.subyard.lease-epoch']):
            r['unavailable'].append('lease_ownership'); return r
        r['status']=instance.get('status','unknown')
        verified[identity]=True
    try:
        exe=(proc/'exe').resolve(); argv=(proc/'cmdline').read_bytes().split(b'\0')
        if not exe.name.startswith('qemu-system-') or argv[argv.index(b'-pidfile')+1]!=str(run/'qemu.pid').encode() or argv[argv.index(b'-readconfig')+1]!=str(run/'qemu.conf').encode(): raise ValueError()
        r['qemu_events']=qmp_events(project,name)
        r['qemu_binary_sha256']=digest(exe)
        conf=(run/'qemu.conf').read_bytes(); r['qemu_config_sha256']=hashlib.sha256(conf).hexdigest()
        r['firmware']=[]
        for filename in re.findall(rb'file\s*=\s*"([^"]+)"',conf):
            f=P(filename.decode())
            if any(str(f).startswith(prefix) for prefix in ['/usr/share/OVMF/','/usr/share/AAVMF/','/usr/share/qemu/']):
                try: r['firmware'].append({'file':f.name,'sha256':digest(f)})
                except OSError: r['unavailable'].append('firmware_file')
        stat=(proc/'stat').read_text().rsplit(')',1)[1].split()
        r.update(pid=pid,start_ticks=int(stat[19]),tasks=[])
        for task in sorted((proc/'task').iterdir(),key=lambda p:int(p.name))[:64]:
            v=(task/'stat').read_text().rsplit(')',1)[1].split(); w=(task/'wchan').read_text().strip(); comm=(task/'comm').read_text().strip()
            sched=[int(n) for n in (task/'schedstat').read_text().split()]
            r['tasks'].append({'tid':int(task.name),'comm':comm if safe.fullmatch(comm) or re.fullmatch(r'CPU [0-9]+/KVM',comm) else 'unavailable','state':v[0],'utime_ticks':int(v[11]),'stime_ticks':int(v[12]),'wchan':w if safe.fullmatch(w) else 'unavailable','schedstat':sched})
        group=next(line[3:] for line in (proc/'cgroup').read_text().splitlines() if line.startswith('0::'))
        cgroup=P('/sys/fs/cgroup')/group.lstrip('/')
        if not cgroup.resolve().is_relative_to(P('/sys/fs/cgroup')): raise ValueError()
        r['cgroup']={'cpu':counters(cgroup/'cpu.stat'),'memory':counters(cgroup/'memory.events'),'pressure':{k:pressure(cgroup/(k+'.pressure')) for k in ['cpu','io','memory']}}
    except (OSError,ValueError,IndexError,StopIteration): r['unavailable'].append('verified_qemu_proc')
    return r
running=True
def terminate(_signum,_frame):
    global running
    running=False
signal.signal(signal.SIGTERM,terminate)
until=time.monotonic()+6300
while running and time.monotonic()<until:
    result={'monotonic_seconds':time.monotonic(),'wall_seconds':time.time(),'allocation_pressure':{k:pressure('/proc/pressure/'+k) for k in ['cpu','io','memory']},'instances':[sample('subyard-e2e-vms-builder','e2e-base-1'),sample('subyard-e2e-vms-slot-1','e2e-vm-1')]}
    print(json.dumps(result,separators=(',',':')),flush=True); time.sleep(5)
# Incus debug logging may be absent; missing hook records are inconclusive.
try:
    journal=subprocess.check_output(['journalctl','-u','incus.service','--no-pager','-n','2000','-o','json'],timeout=3).decode()
    records=[]
    for line in journal.splitlines():
        row=json.loads(line); message=row.get('MESSAGE','')
        event=next((e for e in ['onStop hook started','onStop hook finished'] if e in message),None)
        if event is None: continue
        for project,name in [('subyard-e2e-vms-builder','e2e-base-1'),('subyard-e2e-vms-slot-1','e2e-vm-1')]:
            if re.search(r'(?:instance|name)="?'+re.escape(name)+r'"?(?:\s|$)',message) and re.search(r'project="?'+re.escape(project)+r'"?(?:\s|$)',message):
                stamp=row.get('__REALTIME_TIMESTAMP','')
                if re.fullmatch(r'[0-9]{1,20}',stamp): records.append({'project':project,'vm':name,'event':event,'wall_microseconds':int(stamp)})
    print(json.dumps({'daemon_hook_events':records[:32]+records[-32:] if len(records)>64 else records,'unavailable':[] if records else ['matching_incus_debug_hook_records']}),flush=True)
except (OSError,ValueError,subprocess.SubprocessError):
    print(json.dumps({'unavailable':['daemon_hook_events']}),flush=True)
PYTHON
SAMPLER_PID=$!
if [ "$SUPPORTED_UPGRADE" = 0 ]; then
  printf '  [ .. ] cold post-upgrade base validation\n'
  refresh_rc=0
  timeout --signal=TERM --kill-after=30 4500 "$SUBYARD_HOME/runtime/current/bin/yard" -Y test-yard test-vms refresh subyard-pair --yes || refresh_rc=$?
  [ "$refresh_rc" = 0 ] || exit "$refresh_rc"
  base="$(status | jq -er '.resources.bases[] | select(.type=="subyard-pair") | .fingerprint')"
  [[ "$base" =~ ^[a-f0-9]{64}$ ]] || die 'validated base fingerprint missing'
  outer_exec incus query "/1.0/images/$base?project=default" | jq -ec '
   .properties["user.subyard.base_validation"] == "cidata-seed-clean-v2" and .type == "virtual-machine"
  ' >/dev/null || die 'base lacks fresh-boot validation metadata'
  printf 'STAGING_BASE={"fingerprint":"%s","validation":"cidata-seed-clean-v2"}\n' "$base"

else
  # This acquire must execute the candidate recipe itself: never warm it with refresh.
  observe_cold_resources before_acquire \
    || die 'cold acquire resource evidence is incomplete or not idle'
  printf 'STAGING_COLD_ACQUIRE={"cached_bases_before_acquire":0}\n'
fi

start_holder_child hold_lease holder staging-reboot slot-001 >"$STATE_PARENT/holder.log" 2>&1
HOLDER_PID="$HOLDER_STARTED_PID"
acquire_budget=600
[ "$SUPPORTED_UPGRADE" = 0 ] || acquire_budget=5100
until=$(( $(recovery_monotonic_seconds) + acquire_budget ))
while [ ! -s "$STATE_PARENT/holder.ready" ]; do
  kill -0 "$HOLDER_PID" && [ ! -e "$STATE_PARENT/holder.failed" ] || die 'nested singleton acquire failed'
  [ "$(recovery_monotonic_seconds)" -lt "$until" ] || die 'nested singleton acquire watchdog expired'
  sleep 1
done
IFS=$'\t' read -r _ HOLDER_CONFIG _ _ _ HOLDER_GENERATION HELD_BASE < "$STATE_PARENT/holder.ready"
if [ "$SUPPORTED_UPGRADE" = 1 ]; then
  base="$(status | jq -er '.resources.bases[] | select(.type=="subyard-pair" and .current==true) | .fingerprint')"
  [[ "$base" =~ ^[a-f0-9]{64}$ ]] || die 'cold acquire did not publish a current base'
  outer_exec incus query "/1.0/images/$base?project=default" | jq -ec '
    .properties["user.subyard.base_validation"] == "cidata-seed-clean-v2" and .type == "virtual-machine"
  ' >/dev/null || die 'cold acquire base lacks fresh-boot validation'
  printf 'STAGING_BASE={"fingerprint":"%s","validation":"cidata-seed-clean-v2","built_by":"acquire"}\n' "$base"
fi
[ "$HELD_BASE" = "$base" ] || die 'singleton used a different base'
held="$(status)"
HOLDER_EPOCH="$(jq -er '.pool.slots[0].lease_epoch' <<<"$held")"
jq -e --argjson generation "$HOLDER_GENERATION" '.pool.slots[0] | .state=="held" and .purpose=="staging-reboot" and .resource_generation==$generation' <<<"$held" >/dev/null \
  || die 'nested holder lease changed'
assert_owned_vm
timeout --signal=TERM --kill-after=10 15 ssh -F "$HOLDER_CONFIG" -T -o ConnectTimeout=3 e2e-vm-1 -- true </dev/null
if [ "$SUPPORTED_UPGRADE" = 1 ]; then
  expected_recipe="$(outer_exec sha256sum /usr/local/libexec/subyard/e2e-recipes/manifest.sha256 | cut -d' ' -f1)"
  boot_recipe="$(timeout --signal=TERM --kill-after=10 15 ssh -F "$HOLDER_CONFIG" -T -o ConnectTimeout=3 e2e-vm-1 -- \
    "awk 'NR==2 {print \$1}' /var/lib/subyard/base-manifest.txt" </dev/null)"
  [ "$boot_recipe" = "$expected_recipe" ] || die 'acquired VM manifest does not bind candidate recipe'
  printf 'STAGING_BOOT_RECIPE={"base_fingerprint":"%s","recipe_manifest_sha256":"%s","ssh_true":true}\n' "$base" "$boot_recipe"
fi
printf 'STAGING_FIRST_BOOT_IDENTITY_BEGIN\n'
timeout --signal=TERM --kill-after=10 30 ssh -F "$HOLDER_CONFIG" -T -o ConnectTimeout=3 e2e-vm-1 -- \
  'uname -r; dpkg-query -W -f="\${Package} \${Version}\\n" incus-agent linux-image-amd64 2>/dev/null || true; agent=/run/incus_agent/incus-agent; [ -f "$agent" ] || agent=/usr/bin/incus-agent; if [ -f "$agent" ]; then sha256sum "$agent"; timeout 2 "$agent" --version || echo agent_version_unavailable; else echo agent_binary_unavailable; fi; systemctl show incus-agent.service -p ActiveState -p ActiveEnterTimestampMonotonic; journalctl -b -u incus-agent.service --no-pager -n 80' </dev/null
printf 'STAGING_FIRST_BOOT_IDENTITY_END\n'
boot_before="$(timeout --signal=TERM --kill-after=10 15 ssh -F "$HOLDER_CONFIG" -T -o ConnectTimeout=3 e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id </dev/null)"
SECOND_BOOT_ACTIVE=1
outer_exec incus stop e2e-vm-1 --project subyard-e2e-vms-slot-1 --timeout 60
assert_owned_vm
outer_exec incus start e2e-vm-1 --project subyard-e2e-vms-slot-1
# Preserve the same lease across reboot; readiness is bounded independently.
timeout --signal=TERM --kill-after=10 300 bash -c '
 while ! ssh -F "$1" -T -o ConnectTimeout=3 e2e-vm-1 -- sudo -n true </dev/null >/dev/null 2>&1; do sleep 2; done
' _ "$HOLDER_CONFIG"
timeout --signal=TERM --kill-after=10 310 incus exec "$INSTANCE" --project "$PROJECT" --   incus exec e2e-vm-1 --project subyard-e2e-vms-slot-1 -- timeout --kill-after=10 300 cloud-init status --wait --long >/dev/null
boot_after="$(timeout --signal=TERM --kill-after=10 15 ssh -F "$HOLDER_CONFIG" -T -o ConnectTimeout=3 e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id </dev/null)"
[ -n "$boot_before" ] && [ -n "$boot_after" ] && [ "$boot_before" != "$boot_after" ] || die 'singleton did not boot again'
printf 'STAGING_SECOND_BOOT_IDENTITY_BEGIN\n'
timeout --signal=TERM --kill-after=10 30 ssh -F "$HOLDER_CONFIG" -T -o ConnectTimeout=3 e2e-vm-1 -- \
  'uname -r; systemctl show incus-agent.service -p ActiveState -p ActiveEnterTimestampMonotonic; journalctl -b -u incus-agent.service --no-pager -n 80' </dev/null
printf 'STAGING_SECOND_BOOT_IDENTITY_END\n'
SECOND_BOOT_ACTIVE=0
stop_sampler
printf 'STAGING_SECOND_BOOT_CONSOLE_BEGIN\n'
outer_exec incus console e2e-vm-1 --show-log --project subyard-e2e-vms-slot-1
printf 'STAGING_SECOND_BOOT_CONSOLE_END\n'
: > "$STATE_PARENT/holder.release"
wait "$HOLDER_PID" || die 'singleton release failed'
HOLDER_PID=''
wait_state available 120 >/dev/null
printf 'STAGING_REBOOT={"same_lease":true,"release_available":true}\n'

if [ "$SUPPORTED_UPGRADE" = 1 ]; then
  observe_cold_resources released "$base" \
    || die 'released cold acquire resource evidence is incomplete or has pending work'
  [ -z "$(outer_exec incus list --all-projects --format csv -c n)" ] || die 'nested builder or VM remains after release'
  printf 'STAGING_SUPPORTED_COLD_TERMINAL={"cold_build":true,"post_upgrade_boot":true,"singleton_ssh":true,"release_available":true,"pending_builder":false,"pending_vm":false}\n'
  exit 0
fi
verify_singleton_recovery
printf 'STAGING_DIAGNOSIS={"cold_validation":true,"first_boot":true,"second_boot":true,"causal_result":"inconclusive_no_stall_observed"}\n'

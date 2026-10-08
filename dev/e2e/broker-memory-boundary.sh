#!/usr/bin/env bash
# Focused physical-memory mount acceptance on VM1 of a disposable allocation.
# Run: dev/agent-e2e.sh --slot N --type android-test --purpose broker-memory-boundary --vm 1 -- \
#        bash dev/e2e/broker-memory-boundary.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$ROOT"
[ "${SUBYARD_E2E_VM:-}" = 1 ] && sudo -n test -f /run/subyard-e2e-lease.json \
  || { printf 'broker-memory-boundary: run on allocated VM1 through dev/agent-e2e.sh\n' >&2; exit 2; }
TOKEN="${1:-$(date +%s)$$}"

# Reuse the marker-owned owner bootstrap without executing its P0 dispatcher.
# ROOT stays this checkout's root when the helper is sourced through /dev/fd.
# shellcheck disable=SC1090
. <(sed '/^  case "$MODE" in/,$d; /^ROOT=/d' "$ROOT/dev/e2e/p0-guest.sh") memory-boundary "$TOKEN"

PROJECT=subyard-test-yard
INSTANCE=yard-test-yard
DEVICE=subyard-host-memory
TELEMETRY=/var/lib/subyard/host-meminfo
ENGINE=/usr/local/libexec/subyard/test-vms-inner
HOLDER_PID=''
PRESSURE_PID=''
STATE_PARENT=''
HOLDER_STARTED_PID=''
# Consumed by the sourced holder cleanup functions.
# shellcheck disable=SC2034
HOLDER_STOP_GRACE_SECONDS=30
# shellcheck disable=SC2034
HOLDER_KILL_GRACE_SECONDS=10
RUNNER="$ROOT/dev/agent-e2e.sh"
# shellcheck disable=SC2034
YARD=test-yard

# These functions own the keeper, release capability and bounded process cleanup.
# shellcheck disable=SC1090
. <(sed -n '/^hold_lease() (/,/^)/p; /^start_holder_child() {/,/^}/p; /^stop_holder_child() {/,/^}/p; /^recovery_monotonic_seconds() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh" | sed '/^  start_lease_keeper$/a\
  printf "%s\\t%s\\n" "$BASHPID" "$LEASE_KEEPER_PID" > "$STATE_PARENT/$client.processes"')

die() { printf 'broker-memory-boundary: %s\n' "$*" >&2; exit 2; }
outer_exec() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
runtime_yard() { "$SUBYARD_HOME/runtime/current/bin/yard" "$@"; }

# An installer that grants incus-admin membership must resume this narrow fixture,
# never the shared helper's broader owner lane. Keep its existing ownership token.
reexec_with_incus_group() {
  local command
  printf -v command 'exec bash %q %q' "$ROOT/dev/e2e/broker-memory-boundary.sh" "$TOKEN"
  exec sg incus-admin -c "$command"
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$PRESSURE_PID" ]; then
    kill "$PRESSURE_PID" >/dev/null 2>&1
    wait "$PRESSURE_PID" >/dev/null 2>&1
  fi
  if [ -n "$HOLDER_PID" ]; then
    : > "$STATE_PARENT/holder.release"
    stop_holder_child "$HOLDER_PID" || rc=3
  fi
  # owner_cleanup performs exact marker checks before teardown and file removal.
  (exit "$rc")
  owner_cleanup
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

assert_owned_device() {
  [ "$(incus config get "$INSTANCE" user.subyard.host_memory --project "$PROJECT")" = v1 ] \
    || die 'host-memory ownership marker did not converge'
  local key want
  for key in type source path readonly; do
    case "$key" in type) want=disk ;; source) want=/proc/meminfo ;; path) want="$TELEMETRY" ;; readonly) want=true ;; esac
    [ "$(incus config device get "$INSTANCE" "$DEVICE" "$key" --project "$PROJECT")" = "$want" ] \
      || die "host-memory device $key did not converge"
  done
  outer_exec "$ENGINE" _test-vms-host-memory-check
  # The checker verifies the opened descriptor's procfs mount and readonly flag.
  # A write can still fail with EACCES before EROFS in an unprivileged container.
  outer_exec sh -eu -c '
    if output=$(LC_ALL=C sh -c '\''printf test >> "$1"'\'' _ "$1" 2>&1); then
      printf "owner meminfo unexpectedly accepted a write\n" >&2
      exit 1
    fi
    printf "  [ ok ] verified readonly owner meminfo rejected a write: %s\n" "$output"
  ' _ "$TELEMETRY" || die 'guest telemetry write check failed'
}

remove_owned_device() {
  assert_owned_device
  incus config device remove "$INSTANCE" "$DEVICE" --project "$PROJECT" >/dev/null
}

assert_unverified_source_rejected() {
  local status
  if outer_exec "$ENGINE" _test-vms-host-memory-check > "$STATE_PARENT/rejected-source.log" 2>&1; then
    die 'installed engine accepted an unverified physical-memory source'
  fi
  status="$(runtime_yard -Y test-yard test-vms status --json)"
  jq -e '.resources.memory == null and
    ((.resources.errors // []) | index("memory_telemetry_unavailable") != null)' \
    <<< "$status" >/dev/null \
    || die 'public status silently fell back to container memory for an unverified source'
}

reject_snapshot_source() {
  local digest
  # Incus may leave its empty, root-owned file mountpoint after hot removal.
  # Refuse every other pre-existing object before writing this marked snapshot.
  outer_exec sh -eu -c '
    test ! -L "$1"
    if test -e "$1"; then
      test -f "$1" && test ! -s "$1"
      test "$(stat -c %u:%h "$1")" = 0:1
      test "$(stat -f -c %T "$1")" != proc
      find "$1" -maxdepth 0 -type f -delete
    fi
    set -C
    { printf "# %s\n" "$2"; cat /proc/meminfo; } > "$1"
    chmod 0600 "$1"
  ' _ "$TELEMETRY" "$MARKER"
  digest="$(outer_exec sha256sum "$TELEMETRY" | awk '{print $1}')"
  assert_unverified_source_rejected
  outer_exec sh -eu -c '
    test ! -L "$1" && test -f "$1"
    test "$(head -n 1 "$1")" = "# $2"
    test "$(sha256sum "$1" | cut -d " " -f 1)" = "$3"
    find "$1" -maxdepth 0 -type f -delete
  ' _ "$TELEMETRY" "$MARKER" "$digest"
}

outer_identity() {
  local pid desired start
  pid="$(incus query "/1.0/instances/$INSTANCE/state?project=$PROJECT" | jq -er '.pid | select(. > 0)')"
  desired="$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")"
  start="$(outer_exec awk '{print $22}' /proc/1/stat)"
  [ "$desired" = running ] || die 'fixture desired power changed'
  printf '%s:%s:%s\n' "$pid" "$start" "$desired"
}

assert_outer_unchanged() {
  [ "$(outer_identity)" = "$OUTER_BEFORE" ] \
    || die 'telemetry reconciliation restarted the fixture yard or changed desired power'
}

sample_available() {
  local first mounted last low high tolerance=$((16 * 1024))
  first="$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)"
  mounted="$(outer_exec awk '/^MemAvailable:/ {print $2}' "$TELEMETRY")"
  last="$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)"
  [[ "$first:$mounted:$last" =~ ^[0-9]+:[0-9]+:[0-9]+$ ]] \
    || die 'missing numeric memory counters'
  low="$first"; high="$last"
  if [ "$first" -gt "$last" ]; then low="$last"; high="$first"; fi
  [ "$mounted" -ge "$((low - tolerance))" ] && [ "$mounted" -le "$((high + tolerance))" ] \
    || die 'mounted memory does not track its immediate physical owner'
  printf '%s\n' "$mounted"
}

assert_memory_accounting() {
  local status before after
  before="$(awk '/^MemAvailable:/ {print $2 * 1024}' /proc/meminfo)"
  status="$(runtime_yard -Y test-yard test-vms status --json)"
  after="$(awk '/^MemAvailable:/ {print $2 * 1024}' /proc/meminfo)"
  jq -e --argjson before "$before" --argjson after "$after" '
    .resources as $r |
    $r.broker_memory.ram_limit_bytes == 2147483648 and
    $r.broker_memory.swap_limit_bytes == 2147483648 and
    $r.broker_memory.effective_ram_limit_bytes >= 2147483648 and
    $r.broker_memory.allocation_scope_verified == true and
    $r.memory.physical_available_known == true and
    $r.memory.physical_available_bytes >= ([$before, $after] | min) - 67108864 and
    $r.memory.physical_available_bytes <= ([$before, $after] | max) + 67108864 and
    $r.memory_admission.headroom_bytes ==
      ([0, ($r.memory_admission.effective_available_bytes -
        $r.memory_admission.reserve_bytes - $r.memory_admission.pending_vm_bytes -
        $r.memory_admission.pending_builder_bytes)] | max)
  ' <<< "$status" >/dev/null || die 'broker budget or admission arithmetic did not match live bounds'
  # Keep only safe numeric counters and public source classifications in evidence.
  printf '%s\n' "$status" | jq -c '
    .resources | {memory, memory_admission, broker_memory,
      reserved_vm_memory_bytes, slots: [.slots[] | {slot_id, memory_commitment_bytes}]}'
  printf 'owner-memory '
  awk '/^(MemTotal|MemFree|MemAvailable|Buffers|Cached|SReclaimable|SwapTotal|SwapFree):/ {printf "%s%s_kib=%s", sep, substr($1,1,length($1)-1), $2; sep=" "} END {print ""}' /proc/meminfo
  printf 'visible-memory '
  outer_exec awk '/^(MemTotal|MemFree|MemAvailable|Buffers|Cached|SReclaimable|SwapTotal|SwapFree):/ {printf "%s%s_kib=%s", sep, substr($1,1,length($1)-1), $2; sep=" "} END {print ""}' /proc/meminfo
}

assert_owned_pool() {
  outer_exec python3 - <<'PYTHON'
import json, pathlib, stat
def owned_pool(root, config, marker, owner=(0, 0)):
    for path, mode, directory in ((root, 0o700, True), (config, 0o644, False),
                                  (marker, 0o644, False), (root / "leases.json", 0o600, False),
                                  (root / "leases.json.lock", 0o600, False)):
        assert not any(p.is_symlink() for p in (path, *path.parents)), "pool fixture path is a symlink"
        info = path.stat()
        assert (info.st_uid, info.st_gid) == owner and stat.S_IMODE(info.st_mode) == mode, "pool fixture ownership or mode differs"
        assert (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)), "pool fixture file type differs"
    expected = {"E2E_VM_STATE_DIR": str(root), "E2E_VM_PROJECT": "subyard-e2e-vms", "E2E_VM_PREFIX": "e2e-vm"}
    seen = set()
    for line in config.read_text().splitlines():
        key, separator, value = line.partition("=")
        if key in expected:
            assert separator and key not in seen and value == expected[key], "pool fixture configuration differs"
            seen.add(key)
    assert "E2E_VM_STATE_DIR" in seen, "pool fixture state root is not configured"
    # Provisioning owns the slot-account marker before any VM is allocated.
    # The pool root has no marker until a runtime explicitly creates one.
    assert marker.read_text().strip() == "test-vms-v1", "slot-account marker differs"
    pool = json.loads((root / "leases.json").read_text())
    assert pool["schema_version"] == 2 and pool["resource_type"] == "agent-e2e" and pool["resource_id"] == "test-vms", "pool fixture identity differs"
    assert pool["slots"] and any(s["slot_id"] == "slot-001" for s in pool["slots"]), "pool fixture slot is missing"
owned_pool(pathlib.Path("/var/lib/subyard/test-vms"), pathlib.Path("/etc/subyard/test-vms.env"),
           pathlib.Path("/var/lib/subyard/e2e-slots/1/.subyard-managed"))
PYTHON
}

assert_native_worker_lifetime() {
  printf '  [ .. ] checking native worker requester and transport lifetime\n'
  assert_owned_device
  assert_owned_pool
  outer_exec python3 - "$ENGINE" <<'PYTHON'
import fcntl, json, os, pathlib, signal, subprocess, sys, time
engine = sys.argv[1]
root = pathlib.Path("/var/lib/subyard/test-vms")
assert all(s["state"] == "available" for s in json.loads((root / "leases.json").read_text())["slots"])
lockpath = root / "leases.json.lock"
assert not lockpath.is_symlink() and lockpath.stat().st_uid == 0 and lockpath.stat().st_mode & 0o777 == 0o600
show = lambda unit, prop: subprocess.check_output(["systemctl", "show", unit, "--property=" + prop, "--value"], stderr=subprocess.DEVNULL).decode().strip()
start = lambda pid: pathlib.Path("/proc", str(pid), "stat").read_text().rsplit(") ", 1)[1].split()[19]
request = "renew slot-001 memory-boundary-invalid 1 memory-boundary-synthetic-capability"
with lockpath.open("r+") as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    for victim in ("requester", "transport"):
        owner = subprocess.Popen([sys.executable, "-c", "import subprocess,sys,time; p=subprocess.Popen([sys.argv[1], '_test-vms-facade'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL); print(p.pid, flush=True); time.sleep(60)", engine],
                                 env=dict(os.environ, SSH_ORIGINAL_COMMAND=request), stdout=subprocess.PIPE)
        unit = ""
        bridge = 0
        identity = ""
        try:
            bridge = int(owner.stdout.readline())
            identity = start(bridge)
            unit = "subyard-test-vms-worker-" + str(bridge) + "-" + identity + ".service"
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                worker = int(show(unit, "MainPID") or "0")
                source = pathlib.Path("/run/subyard-test-vms-workers", unit.removesuffix(".service") + ".json")
                if worker > 1 and not source.exists():
                    break
                assert owner.poll() is None, "native requester exited before credential restoration"
                time.sleep(0.1)
            else:
                raise AssertionError("native worker did not restore its credential")
            assert pathlib.Path("/proc", str(worker), "cgroup").read_text().strip() == "0::/subyardtestvms.slice/" + unit
            assert pathlib.Path("/proc", str(worker), "root/run/credentials", unit, "subyard-broker-request").is_file()
            # Inspect privately: neither raw unit environment nor request inputs enter evidence.
            for prop in ("Environment", "ExecStart"):
                value = show(unit, prop)
                assert "SSH_ORIGINAL_COMMAND=" not in value and "memory-boundary-synthetic-capability" not in value, "unit properties exposed request inputs"
            if victim == "requester":
                owner.kill()
                owner.wait(timeout=5)
            else:
                assert start(bridge) == identity
                os.kill(bridge, signal.SIGKILL)
                assert owner.poll() is None
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                if show(unit, "LoadState") == "not-found" and not pathlib.Path("/sys/fs/cgroup/subyardtestvms.slice", unit).exists():
                    break
                time.sleep(0.2)
            else:
                raise AssertionError("dead " + victim + " retained native worker service")
            assert not source.exists(), "native request source remained after cleanup"
        finally:
            if owner.poll() is None:
                owner.kill()
            owner.wait(timeout=5)
            if unit and show(unit, "LoadState") != "not-found":
                subprocess.run(["systemctl", "stop", unit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20, check=True)
            if bridge and pathlib.Path("/proc", str(bridge)).exists() and start(bridge) == identity:
                os.kill(bridge, signal.SIGKILL)
print("  [ ok ] native credentials hide inputs; requester and transport death collect bounded workers")
PYTHON
}

assert_native_namespace_waiter_lifetime() {
  printf '  [ .. ] checking direct Incus namespace waiter lifetime\n'
  assert_owned_device
  assert_owned_pool
  python3 - "$INSTANCE" "$PROJECT" "$ENGINE" <<'PYTHON'
import json, select, subprocess, sys, time
instance, project, engine = sys.argv[1:]
base = ["incus", "exec", instance, "--project", project]
locker_code = '''import fcntl,json,os,pathlib,time
root=pathlib.Path("/var/lib/subyard/test-vms")
assert all(s["state"]=="available" for s in json.loads((root/"leases.json").read_text())["slots"])
path=root/"leases.json.lock"
assert not path.is_symlink() and path.stat().st_uid==0 and path.stat().st_mode&0o777==0o600
with path.open("r+") as lock:
 fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
 assert all(s["state"]=="available" for s in json.loads((root/"leases.json").read_text())["slots"])
 start=pathlib.Path("/proc/self/stat").read_text().rsplit(") ",1)[1].split()[19]
 print(os.getpid(),start,flush=True)
 time.sleep(60)
'''
inspect_code = '''import json,os,pathlib,re,subprocess
request="renew slot-001 memory-boundary-invalid 1 memory-boundary-synthetic-capability"
stat=lambda pid:pathlib.Path("/proc",str(pid),"stat").read_text().rsplit(") ",1)[1].split()
show=lambda unit,prop:subprocess.check_output(["systemctl","show",unit,"--property="+prop,"--value"],stderr=subprocess.DEVNULL,timeout=3).decode().strip()
result=None
for leaf in pathlib.Path("/sys/fs/cgroup/subyardtestvms.slice").glob("subyard-test-vms-worker-*.service"):
 unit=leaf.name
 assert re.fullmatch(r"subyard-test-vms-worker-[1-9][0-9]*-[1-9][0-9]*\\.service",unit)
 try:
  worker=int(show(unit,"MainPID") or "0")
  source=pathlib.Path("/run/subyard-test-vms-workers",unit.removesuffix(".service")+".json")
  if worker<=1 or source.exists(): continue
  credential=pathlib.Path("/proc",str(worker),"root/run/credentials",unit,"subyard-broker-request")
  if not credential.is_file(): continue
  info=credential.stat()
  assert info.st_uid==0 and info.st_nlink==1 and info.st_mode&0o077==0
  inputs=json.loads(credential.read_text())
 except (FileNotFoundError,subprocess.CalledProcessError):
  continue
 if inputs["environment"].get("SSH_ORIGINAL_COMMAND")!=request: continue
 bridge,owner=int(inputs["bridge_pid"]),int(inputs["requester_pid"])
 assert bridge>1 and owner>1 and unit=="subyard-test-vms-worker-"+str(bridge)+"-"+inputs["bridge_start"]+".service"
 assert stat(bridge)[19]==inputs["bridge_start"] and int(stat(bridge)[1])==owner
 assert stat(owner)[19]==inputs["requester_start"] and stat(owner)[1]=="0"
 assert pathlib.Path("/proc",str(owner),"comm").read_text().strip()=="sh"
 assert pathlib.Path("/proc",str(worker),"cgroup").read_text().strip()=="0::/subyardtestvms.slice/"+unit
 assert (leaf.parent/"memory.max").read_text().strip()=="2147483648"
 assert (leaf.parent/"memory.swap.max").read_text().strip()=="2147483648"
 for prop in ("Environment","ExecStart"):
  value=show(unit,prop)
  assert "SSH_ORIGINAL_COMMAND=" not in value and "memory-boundary-synthetic-capability" not in value
 result={"unit":unit,"owner":owner,"start":inputs["requester_start"]}
 break
print(json.dumps(result))
'''
def inner(code, *args):
    return subprocess.check_output(base+["--","python3","-c",code,*map(str,args)], stderr=subprocess.DEVNULL, timeout=25)
def kill_identity(pid, identity):
    inner('import os,pathlib,signal,sys; p=sys.argv[1]; f=pathlib.Path("/proc",p,"stat"); assert int(p)>1; s=f.read_text().rsplit(") ",1)[1].split() if f.exists() else []; assert not s or s[19]==sys.argv[2]; os.kill(int(p),signal.SIGKILL) if s else None', pid, identity)
locker = subprocess.Popen(base+["--","python3","-c",locker_code], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
owner = None
identity = None
locked = None
try:
    assert select.select([locker.stdout], [], [], 10)[0], "native lock fixture did not become ready"
    locked = locker.stdout.readline().decode().split()
    assert len(locked)==2 and all(v.isdigit() for v in locked) and int(locked[0])>1 and int(locked[1])>0
    request="renew slot-001 memory-boundary-invalid 1 memory-boundary-synthetic-capability"
    owner = subprocess.Popen(base+["--env","SSH_ORIGINAL_COMMAND="+request,"--",engine,"_test-vms-facade"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    deadline=time.monotonic()+20
    while time.monotonic()<deadline:
        identity=json.loads(inner(inspect_code))
        if identity: break
        assert owner.poll() is None, "direct Incus native worker exited before credential restoration"
        time.sleep(0.1)
    else:
        raise AssertionError("direct Incus native waiter did not become ready")
    kill_identity(identity["owner"], identity["start"])
    owner.wait(timeout=20)
    inner('''import pathlib,subprocess,sys,time
unit=sys.argv[1]
deadline=time.monotonic()+20
while time.monotonic()<deadline:
 state=subprocess.check_output(["systemctl","show",unit,"--property=LoadState","--value"],stderr=subprocess.DEVNULL,timeout=3).decode().strip()
 if state=="not-found" and not pathlib.Path("/sys/fs/cgroup/subyardtestvms.slice",unit).exists(): break
 time.sleep(.2)
else: raise AssertionError("namespace requester death retained native service")
assert not pathlib.Path("/run/subyard-test-vms-workers",unit.removesuffix(".service")+".json").exists()
''', identity["unit"])
finally:
    try:
        if identity:
            kill_identity(identity["owner"], identity["start"])
            subprocess.run(base+["--","systemctl","stop",identity["unit"]], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
    finally:
        try:
            if owner and owner.poll() is None: owner.kill()
            if owner: owner.wait(timeout=5)
        finally:
            try:
                if locked: kill_identity(*locked)
            finally:
                if locker.poll() is None: locker.kill()
                locker.wait(timeout=5)
print("  [ ok ] direct Incus namespace waiter preserves private native scope and collects after requester death")
PYTHON
}

assert_launch_memory_bound() {
  local finite="${1:-no}"
  printf '  [ .. ] checking native Incus launch memory bound and admission\n'
  assert_owned_device
  assert_owned_pool
  outer_exec python3 - "$ENGINE" "$finite" <<'PYTHON'
import json, os, pathlib, subprocess, sys, tempfile
engine, finite = sys.argv[1:]
run = lambda *args: subprocess.check_output(args, stderr=subprocess.DEVNULL)
def launch():
    pid = int(run("systemctl", "show", "incus.service", "--property=MainPID", "--value"))
    assert pid > 1
    lines = pathlib.Path("/proc", str(pid), "cgroup").read_text().splitlines()
    paths = [line[3:] for line in lines if line.startswith("0::/")]
    assert len(paths) == 1 and "/subyardtestvms.slice" not in paths[0]
    current = pathlib.Path("/sys/fs/cgroup" + paths[0])
    leaf, bounds = current, []
    while True:
        limit = current / "memory.max"
        if limit.exists() and limit.read_text().strip() != "max":
            maximum = int(limit.read_text())
            used = int((current / "memory.current").read_text())
            bounds.append((max(0, maximum-used), maximum))
        if current == pathlib.Path("/sys/fs/cgroup"):
            break
        current = current.parent
    return leaf, min(bounds) if bounds else None

def status_bound():
    leaf, before = launch()
    status = json.loads(run(engine, "_test-vms-worker", "status"))
    _, after = launch()
    memory = status["resources"]["memory"]
    assert memory["limit_available"] == (before is not None)
    if before is not None:
        assert after is not None and memory["cgroup_limit_bytes"] in (before[1], after[1])
        assert min(before[0], after[0])-67108864 <= memory["cgroup_headroom_bytes"] <= max(before[0], after[0])+67108864
    bounds = [memory["visible_available_bytes"], memory["physical_available_bytes"]]
    if memory["limit_available"]:
        bounds.append(memory["cgroup_headroom_bytes"])
    assert memory["available_bytes"] == min(bounds)
    return leaf, status

leaf, status = status_bound()
if finite == "yes":
    # This boot has no nested leases or workloads; never lower a running VM/builder's ceiling.
    assert all(s["state"] == "available" for s in status["pool"]["slots"])
    assert json.loads(run("incus", "list", "--all-projects", "--format=json")) == [], "foreign or running inner workload prevents finite-bound fixture"
    assert status["resources"]["memory"]["available_bytes"] >= 536870912, "insufficient safe RAM margin for finite-bound fixture"
    limit = leaf / "memory.max"
    original = limit.read_text()
    ceiling = int((leaf / "memory.current").read_text()) + 268435456
    assert original.strip() == "max" or int(original) > ceiling, "launch ceiling lacks safe room for finite-bound fixture"
    try:
        limit.write_text(str(ceiling))
        _, bounded = status_bound()
        assert bounded["resources"]["memory"]["cgroup_limit_bytes"] == ceiling
        with tempfile.TemporaryDirectory(prefix="subyard-memory-boundary-", dir="/tmp") as temporary:
            key = str(pathlib.Path(temporary, "lease-key"))
            subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key], check=True)
            keytype, blob, *_ = pathlib.Path(key + ".pub").read_text().split()
            request = " ".join(["acquire-v3", "subyard-pair", "memory-boundary", "memory-boundary", "test-yard", "Subyard-2", "memory-boundary", "memory-boundary", keytype, blob, "slot-001", "2"])
            refusal = json.loads(subprocess.check_output([engine, "_test-vms-facade"], env=dict(os.environ, SSH_ORIGINAL_COMMAND=request), stderr=subprocess.DEVNULL))
        assert set(refusal) == {"schema_version", "status", "code", "reason", "message"}
        assert refusal["status"] == "error" and refusal["code"] == "capacity" and refusal["reason"] == "memory"
        admission = json.loads(refusal["message"].split("; admission=", 1)[1])
        assert admission["headroom_bytes"] == max(0, admission["effective_available_bytes"]-admission["reserve_bytes"]-admission["pending_vm_bytes"]-admission["pending_builder_bytes"])
        assert admission["headroom_bytes"] <= 268435456 and admission["required_bytes"] == 5368709120
        assert admission["slot_state"] == "provisioning" and admission["vm_count"] == 2
        print("native-memory-refusal " + json.dumps(admission, separators=(",", ":")))
        assert json.loads(run("incus", "list", "--all-projects", "--format=json")) == [], "refused request created a guest or builder"
        assert all(s["state"] == "available" for s in json.loads(run(engine, "_test-vms-worker", "status"))["pool"]["slots"])
    finally:
        limit.write_text(original)
        assert limit.read_text().strip() == original.strip()
    status_bound()
    print("  [ ok ] restored finite Incus launch ceiling proves native typed memory refusal without allocation")
print("  [ ok ] status RAM bound matches real Incus MainPID ancestry")
PYTHON
}

live_pressure() {
  local before after restored attempt
  before="$(sample_available)"
  python3 - "$STATE_PARENT/pressure.ready" <<'PY' &
import pathlib, signal, sys
memory = bytearray(128 * 1024 * 1024)
for offset in range(0, len(memory), 4096):
    memory[offset] = 1
pathlib.Path(sys.argv[1]).write_text("ready\n")
signal.pause()
PY
  PRESSURE_PID=$!
  for ((attempt=0; attempt<30; attempt++)); do
    [ ! -f "$STATE_PARENT/pressure.ready" ] || break
    kill -0 "$PRESSURE_PID" || die 'bounded pressure process exited'
    sleep 1
  done
  [ -f "$STATE_PARENT/pressure.ready" ] || die 'bounded pressure process did not become ready'
  after="$(sample_available)"
  [ "$((before - after))" -ge $((64 * 1024)) ] \
    || die '128MiB physical pressure was not reflected in live telemetry'
  assert_memory_accounting
  kill "$PRESSURE_PID"
  wait "$PRESSURE_PID" || true
  PRESSURE_PID=''
  restored="$(sample_available)"
  [ "$((restored - after))" -ge $((64 * 1024)) ] \
    || die 'released physical pressure was not reflected in live telemetry'
  printf '  [ ok ] live owner memory before_kib=%s pressured_kib=%s released_kib=%s\n' \
    "$before" "$after" "$restored"
}

held_identity() {
  # Hash only the stable lease fields inside L1; no capability or controller data
  # leaves the protected store, and no lease ID is printed in fixture evidence.
  outer_exec python3 -c '
import hashlib, json, pathlib
pool = json.loads(pathlib.Path("/var/lib/subyard/test-vms/leases.json").read_text())
slot, = [slot for slot in pool["slots"] if slot["slot_id"] == "slot-001"]
assert slot["state"] == "held" and slot["lease_id"]
fields = ("slot_id", "state", "lease_id", "resource_generation", "lease_epoch", "run", "purpose", "project", "base_fingerprint", "environment")
print(hashlib.sha256(json.dumps({key: slot[key] for key in fields}, sort_keys=True).encode()).hexdigest())
'
}

held_heartbeat() {
  outer_exec python3 -c '
import json, pathlib
pool = json.loads(pathlib.Path("/var/lib/subyard/test-vms/leases.json").read_text())
slot, = [slot for slot in pool["slots"] if slot["slot_id"] == "slot-001"]
print(slot["last_heartbeat_at"])
'
}

assert_held_unchanged() {
  kill -0 "$HOLDER_PID" || die 'held lease keeper exited'
  [ "$(held_identity)" = "$HELD_BEFORE" ] || die 'update changed the held lease identity or allocation'
  assert_outer_unchanged
  local guest
  for guest in 1 2; do
    [ "$(ssh -F "$HELD_CONFIG" -T -o ConnectTimeout=10 "e2e-vm-$guest" -- \
      cat /proc/sys/kernel/random/boot_id </dev/null)" = "${GUEST_BOOT[$guest]}" ] \
      || die "held guest $guest restarted or lost access"
  done
  assert_held_ram_released
}

assert_held_ram_released() {
  runtime_yard -Y test-yard test-vms status --json | jq -e '
    .resources.reserved_vm_memory_bytes == 0 and
    (.resources.slots | length == 1) and
    .resources.slots[0].memory_commitment_bytes == 0 and
    .resources.slots[0].remaining_memory_growth_bytes == 0' >/dev/null \
    || die 'held allocation retained a startup RAM promise'
}

assert_guests_outside_broker_budget() {
  outer_exec python3 - <<'PY'
import json, pathlib, subprocess
for name in ("e2e-vm-1", "e2e-vm-2"):
    state = json.loads(subprocess.check_output([
        "incus", "query", "/1.0/instances/" + name + "/state?project=subyard-e2e-vms-slot-1"
    ]))
    assert state["status"] == "Running" and state["pid"] > 0
    path, = [line[3:] for line in pathlib.Path("/proc", str(state["pid"]), "cgroup").read_text().splitlines() if line.startswith("0::/")]
    assert "/subyardtestvms.slice" not in path, "guest was charged to broker process budget"
    daemon = int(subprocess.check_output(["systemctl", "show", "incus.service", "--property=MainPID", "--value"]))
    launch, = [line[3:] for line in pathlib.Path("/proc", str(daemon), "cgroup").read_text().splitlines() if line.startswith("0::/")]
    assert path == launch or path.startswith(launch.rstrip("/") + "/"), "guest escaped sampled Incus launch ancestry"
print("  [ ok ] both running guests retain Incus launch ancestors outside the broker process cgroup")
PY
}

assert_orphan_expiry() {
  local owner keeper heartbeat expiry now state attempt generation prior_epoch stale_rc guest fresh_config
  prior_epoch="$(runtime_yard -Y test-yard test-vms status --json | jq -er '.pool.slots[] | select(.slot_id == "slot-001") | .lease_epoch')"
  for guest in 1 2; do
    ssh -F "$HELD_CONFIG" -T -o ConnectTimeout=10 "e2e-vm-$guest" -- touch /root/subyard-owner-death-marker
  done
  IFS=$'\t' read -r owner keeper < "$STATE_PARENT/holder.processes"
  [[ "$owner:$keeper" =~ ^[1-9][0-9]*:[1-9][0-9]*$ ]] || die 'holder process identities unavailable'
  kill -0 "$owner" && kill -0 "$keeper" || die 'holder or keeper exited before the owner-death check'
  kill -KILL "$owner"
  wait "$HOLDER_PID" 2>/dev/null || true
  HOLDER_PID=''
  for ((attempt=0; attempt<10; attempt++)); do
    [ ! -r "/proc/$keeper/stat" ] || [ "$(awk '{print $3}' "/proc/$keeper/stat")" = Z ] || { sleep 1; continue; }
    break
  done
  if [ -r "/proc/$keeper/stat" ] && [ "$(awk '{print $3}' "/proc/$keeper/stat")" != Z ]; then
    die 'orphan keeper survived requester death'
  fi
  heartbeat="$(held_heartbeat)"
  expiry="$(runtime_yard -Y test-yard test-vms status --json | python3 -c '
import datetime, json, sys
slot, = [s for s in json.load(sys.stdin)["pool"]["slots"] if s["slot_id"] == "slot-001"]
stamp = lambda s: datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
assert 1199 <= stamp(slot["expires_at"]) - stamp(slot["last_heartbeat_at"]) <= 1201
print(int(stamp(slot["expires_at"])))
')" || die 'held lease did not advertise the 20-minute renewal timeout'
  printf '  [ .. ] requester stopped; waiting for natural lease expiry\n'
  for ((attempt=0; ; attempt++)); do
    state="$(runtime_yard -Y test-yard test-vms status --json | jq -er '.pool.slots[] | select(.slot_id == "slot-001") | .state')"
    now="$(date +%s)"
    if [ "$state" = available ]; then
      [ "$now" -ge "$((expiry - 2))" ] || die 'orphan allocation was freed before its advertised expiry'
      break
    fi
    [ "$now" -le "$((expiry + 180))" ] || die 'expired orphan allocation was not cleaned up'
    if [ "$state" = held ]; then
      [ "$(held_heartbeat)" = "$heartbeat" ] || die 'orphan requester lease was renewed'
    fi
    [ "$((attempt % 12))" -ne 0 ] || printf '  [ .. ] orphan state=%s expiry_remaining_seconds=%s\n' "$state" "$((expiry - now))"
    sleep 5
  done
  assert_memory_accounting
  outer_exec incus list --project subyard-e2e-vms-slot-1 --format json | jq -e 'length == 0' >/dev/null \
    || die 'expired allocation retained a disposable guest'
  stale_rc=0
  timeout 15 ssh -F "$HELD_CONFIG" -N -o BatchMode=yes -o ConnectTimeout=5 subyard-e2e-data \
    >/dev/null 2>"$STATE_PARENT/stale-data.log" || stale_rc=$?
  [ "$stale_rc" = 255 ] && grep -Fq 'Permission denied (publickey)' "$STATE_PARENT/stale-data.log" \
    || die 'expired lease data account did not reject its former key'
  printf '  [ ok ] requester death stopped renew; natural expiry removed both disposable guests\n'

  SUBYARD_E2E_STATE_DIR="$STATE_PARENT/reuse" "$RUNNER" --yard test-yard --prepare >/dev/null
  start_holder_child hold_lease reuse memory-boundary-reuse slot-001 > "$STATE_PARENT/reuse.log" 2>&1
  HOLDER_PID="$HOLDER_STARTED_PID"
  for ((attempt=0; attempt<600; attempt++)); do
    [ ! -s "$STATE_PARENT/reuse.ready" ] || break
    kill -0 "$HOLDER_PID" || die 'fresh lease failed after orphan cleanup'
    sleep 1
  done
  [ -s "$STATE_PARENT/reuse.ready" ] || die 'fresh lease did not become ready'
  IFS=$'\t' read -r _ fresh_config _ _ _ generation _ < "$STATE_PARENT/reuse.ready"
  runtime_yard -Y test-yard test-vms status --json | jq -e --argjson previous "$prior_epoch" --argjson generation "$generation" '
    .pool.slots[] | select(.slot_id == "slot-001") |
      .state == "held" and .lease_epoch > $previous and .resource_generation == $generation' >/dev/null \
    || die 'fresh allocation did not advance the lease epoch'
  for guest in 1 2; do
    ssh -F "$fresh_config" -T -o ConnectTimeout=10 "e2e-vm-$guest" -- test ! -e /root/subyard-owner-death-marker \
      || die 'fresh allocation retained former guest data'
  done
  assert_memory_accounting
  : > "$STATE_PARENT/reuse.release"
  wait "$HOLDER_PID" || die 'fresh allocation release failed'
  HOLDER_PID=''
  printf '  [ ok ] clean reacquire advanced epoch; disposable data absent; release succeeded\n'
}

holder_failure() {
  # Preserve bounded controller diagnostics before marker-owned cleanup. Never
  # print the private lease/config store or arbitrary provisioning output.
  if [ -f "$STATE_PARENT/holder.log" ]; then
    awk '
      /^agent-e2e: retryable capacity refusal for slot-[0-9][0-9][0-9]: resource=(memory|disk); retry after resources become available$/ { print; next }
      /^agent-e2e: broker capability probe failed$/ { print; next }
      /^agent-e2e: test environment unavailable: no pinned host key for / { print "holder has no pinned SSH host key"; next }
      /^agent-e2e: (cannot canonicalize|E2E runner must execute|managed workspace|project metadata)/ { print "holder workspace attribution failed"; next }
      /Permission denied \(publickey\)/ { print "holder SSH authentication failed"; next }
      /^ssh:/ { print "holder SSH transport failed"; next }
      /: unbound variable$/ { print "holder shell variable was unset" }
    ' \
      "$STATE_PARENT/holder.log" >&2
  fi
  die "$1"
}

printf '  [ .. ] preparing marker-owned broker on disposable VM1\n'
prepare_owner_go_cache
STATE_PARENT="$OWNER_ROOT/memory-boundary"
p0_capacity_prepare_subtree "$STATE_PARENT"
YARD_BUILD_VERSION="$P0_OWNER_VERSION" dev/build-engine.sh --force >/dev/null
ensure_owner_incus
# Consumed by the sourced marker-owned cleanup and registration functions.
# shellcheck disable=SC2034
OWNER_BASELINE_IMAGES="$(incus image list --project default --format csv -c f)"
# shellcheck disable=SC2034
OWNER_BASELINE_CAPTURED=1
ensure_owner_base_image
# shellcheck disable=SC2034
OWNER_DIAGNOSTIC_VM_MEMORY=2GiB
install_owner_runtime
prepare_broker_recovery_update
prepare_owner_image_cache_project "$PROJECT"
write_owner_registration test-yard test-vms 2224 1
REGISTRATION="$OWNER_YARD_DIR/test-yard.env"
if [ -e "$OWNER_YARD_DIR/test-yard/config.env" ]; then
  REGISTRATION="$OWNER_YARD_DIR/test-yard/config.env"
fi
cat >> "$REGISTRATION" <<'CONFIG'
# Small budgets belong exclusively to this disposable physical-boundary fixture.
E2E_MEMORY_RESERVE=128MiB
E2E_VM_OVERHEAD=512MiB
E2E_DISK_RESERVE=512MiB
E2E_CACHE_BUDGET=4GiB
CONFIG
p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes
assert_owned_device
OUTER_BEFORE="$(outer_identity)"
assert_memory_accounting
remove_owned_device
assert_unverified_source_rejected
reject_snapshot_source
printf '  [ ok ] missing and ordinary snapshot sources fail closed in engine and public status\n'
p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes
assert_owned_device
assert_outer_unchanged
live_pressure
assert_memory_accounting
printf '  [ ok ] init and live hotplug preserve yard power and process identity\n'

# Only this nested, marker-owned fixture is restarted, before any nested lease.
runtime_yard -Y test-yard stop --yes >/dev/null
runtime_yard -Y test-yard start --yes >/dev/null
wait_for_outer_default_route "$INSTANCE" "$PROJECT"
assert_owned_device
OUTER_BEFORE="$(outer_identity)"
assert_memory_accounting
printf '  [ ok ] telemetry survives fixture container boot\n'
assert_native_worker_lifetime
assert_native_namespace_waiter_lifetime
assert_launch_memory_bound yes

# Free compiler cache before reserving the nested pair's disks on the allocated VM.
p0_capacity_remove_build_cache
nested_free="$(outer_exec df -B1 --output=avail /srv/incus-e2e/storage | awk 'NR == 2 {print $1}')"
[[ "$nested_free" =~ ^[0-9]+$ ]] || die 'nested disk headroom unavailable'
# A fresh base builder reserves 3x the 10GiB guest disk, plus the fixture reserve.
nested_required=$((30 * 1024 * 1024 * 1024 + 512 * 1024 * 1024))
[ "$nested_free" -ge "$nested_required" ] \
  || die "fixture disk too small: available_bytes=$nested_free required_bytes=$nested_required"
printf '  [ ok ] cold nested builder disk headroom available_bytes=%s required_bytes=%s\n' \
  "$nested_free" "$nested_required"
export SUBYARD_E2E_TEST_MODE=1
export SUBYARD_E2E_WORKSPACES_ROOT
SUBYARD_E2E_WORKSPACES_ROOT="$(dirname "$(dirname "$ROOT")")"
export SUBYARD_E2E_PROJECT_LABEL=Subyard-2
export SUBYARD_E2E_PROJECT_YARD=test-yard
export SUBYARD_E2E_YARD=test-yard
SUBYARD_E2E_STATE_DIR="$STATE_PARENT/holder" "$RUNNER" --yard test-yard --prepare >/dev/null
start_holder_child hold_lease holder memory-boundary slot-001 > "$STATE_PARENT/holder.log" 2>&1
HOLDER_PID="$HOLDER_STARTED_PID"
for ((attempt=0; attempt<1800; attempt++)); do
  [ ! -s "$STATE_PARENT/holder.ready" ] || break
  kill -0 "$HOLDER_PID" 2>/dev/null || holder_failure 'nested holder failed before publishing readiness'
  [ "$((attempt % 60))" -ne 0 ] || printf '  [ .. ] preparing nested pair (%ss elapsed)\n' "$attempt"
  sleep 1
done
[ -s "$STATE_PARENT/holder.ready" ] || holder_failure 'nested holder did not become ready within 30 minutes'
IFS=$'\t' read -r _slot HELD_CONFIG _project _run _purpose _generation _base \
  < "$STATE_PARENT/holder.ready"
HELD_BEFORE="$(held_identity)"
declare -A GUEST_BOOT=()
for guest in 1 2; do
  GUEST_BOOT[$guest]="$(ssh -F "$HELD_CONFIG" -T -o ConnectTimeout=10 "e2e-vm-$guest" -- \
    cat /proc/sys/kernel/random/boot_id </dev/null)"
done
assert_held_ram_released
assert_memory_accounting
assert_guests_outside_broker_budget
assert_launch_memory_bound
runtime_yard -Y test-yard test-vms status --json | jq -e '
  .pool.slots[] | select(.slot_id == "slot-001") |
    .environment.vm_count == 2 and .environment.memory_per_vm == "2GiB"' >/dev/null \
  || die 'guest allocation exceeding the broker process budget was not held'
printf '  [ ok ] ready guests release startup RAM promises to host accounting\n'

release="$(dirname "$P0_BROKER_RECOVERY_UPDATE_ARTIFACT")"
runtime_root="$SUBYARD_HOME/runtime"
old_hash="$(outer_exec sha256sum "$ENGINE" | awk '{print $1}')"
remove_owned_device
YARD_RELEASE_BASE_URL="file://$release" runtime_yard update \
  --runtime-root "$runtime_root" --version p0-broker-recovery-update --check >/dev/null
YARD_RELEASE_BASE_URL="file://$release" runtime_yard update \
  --runtime-root "$runtime_root" --version p0-broker-recovery-update --yes >/dev/null
assert_owned_device
assert_held_unchanged
assert_memory_accounting
new_hash="$(outer_exec sha256sum "$ENGINE" | awk '{print $1}')"
expected_hash="$(sha256sum "$runtime_root/current/bin/yard-engine" | awk '{print $1}')"
[ "$new_hash" = "$expected_hash" ] && [ "$new_hash" != "$old_hash" ] \
  || die 'ordinary update did not install the distinct candidate engine'
printf '  [ ok ] ordinary update restores telemetry and preserves held lease, guests and yard\n'
runtime_yard migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null \
  || die 'ordinary update did not reach release readiness'

# Keep the completed release ready: removing its required device creates a new
# repair transaction, whose protected caller correctly pins the current target.
runtime_yard update --runtime-root "$runtime_root" --rollback --yes >/dev/null
assert_owned_device
assert_held_unchanged
assert_memory_accounting
[ "$(outer_exec sha256sum "$ENGINE" | awk '{print $1}')" = "$old_hash" ] \
  || die 'ordinary rollback did not restore the original engine'
runtime_yard migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null \
  || die 'ordinary rollback did not reach release readiness'
printf '  [ ok ] ordinary rollback preserves telemetry, held lease, guests and yard\n'

HEARTBEAT_BEFORE="$(held_heartbeat)"
for ((attempt=0; attempt<165; attempt++)); do
  [ "$(held_heartbeat)" = "$HEARTBEAT_BEFORE" ] || break
  kill -0 "$HOLDER_PID" || die 'held keeper exited before renewing the lease'
  sleep 2
done
[ "$(held_heartbeat)" != "$HEARTBEAT_BEFORE" ] \
  || die 'held heartbeat did not advance across update and rollback'
assert_held_unchanged
printf '  [ ok ] held lease renewal continues after update and rollback\n'

assert_orphan_expiry
printf 'evidence: original_engine_sha256=%s candidate_engine_sha256=%s held_identity_sha256=%s\n' \
  "$old_hash" "$new_hash" "$HELD_BEFORE"
printf 'ok: broker memory bounds, process budget, boot, held update/rollback and orphan expiry\n'

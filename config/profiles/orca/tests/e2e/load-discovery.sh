#!/usr/bin/env bash
# Sourced only after guarded disposable Orca project setup.
[ "${SUBYARD_E2E_ORCA_LOAD_DIAGNOSTIC:-}" = 1 ] || die 'explicit load opt-in required'
load_roots="${SUBYARD_E2E_ORCA_LOAD_ROOTS:-1000}"
case "$load_roots" in 1000|1400) ;; *) die 'load roots must be 1000 or 1400' ;; esac
load_lane="${SUBYARD_E2E_ORCA_LOAD_LANE:-load}"
case "$load_lane" in load|cleanup) ;; *) die 'load lane must be load or cleanup' ;; esac
# All probes and generated fixtures live in the nested disposable guest.
incus --project "$PROJECT" file push "$ROOT/config/profiles/orca/tests/e2e/load-probe.py" \
  "$INSTANCE/tmp/load-probe.py" --mode 0755
# Native diagnostics are enabled only in this marker-owned disposable yard.
guest_root bash -se <<'YARD'
install -d -m 0755 /etc/systemd/system/subyard-orca.service.d
printf '[Service]\nEnvironment=ORCA_MAIN_THREAD_DIAGNOSTICS=1\n' > /etc/systemd/system/subyard-orca.service.d/load-diagnostic.conf
chmod 0644 /etc/systemd/system/subyard-orca.service.d/load-diagnostic.conf
systemctl daemon-reload
systemctl restart subyard-orca.service
YARD
guest_dev python3 -B - <<'PYREADY'
import importlib.util, time
spec = importlib.util.spec_from_file_location('fixture', '/tmp/orca-projects-helper.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)
deadline = time.monotonic() + 120
while time.monotonic() < deadline:
    try:
        fixture.call_any_result('/srv/agents/orca/config/orca/orca-runtime.json', 'repo.list', None)
        break
    except fixture.SafeRpcError:
        time.sleep(2)
else:
    raise SystemExit('diagnostic runtime did not become reachable')
PYREADY
orca_load_measure() {
  local pid rc=0
  pid="$(guest_root systemctl show subyard-orca.service --property MainPID --value)"
  guest_root python3 -B /tmp/load-probe.py --phase "$1" --telemetry-start || true
  guest_dev python3 -B /tmp/load-probe.py --phase "$1" --pid "$pid" \
    --roots "$load_roots" --root "$load_root" --state /tmp/orca-load-state.json || rc=$?
  # Telemetry cannot turn an original failed phase into a pass.
  guest_root python3 -B /tmp/load-probe.py --phase "$1" --telemetry-finish || true
  return "$rc"
}

load_sync() {
  local output category remaining current batches=0 lock_deadline=0 deadline=$((SECONDS + 600))
  remaining="$(guest_dev python3 -B /tmp/load-probe.py --phase cleaned --remaining \
    --root "$load_root" --state /tmp/orca-load-state.json)" || die 'load sync progress unavailable'
  [[ "$remaining" =~ ^[0-9]+$ ]] || die 'invalid load sync progress'
  while [ "$SECONDS" -lt "$deadline" ]; do
    if output="$(yard orca sync --yes 2>&1)"; then return 0; fi
    category="$(python3 -B "$ROOT/config/profiles/orca/tests/e2e/load-probe.py" \
      --phase cleaned --classify-sync-failure <<< "$output")" || die 'load sync classification unavailable'
    case "$category" in
      lock)
        [ "$lock_deadline" -ne 0 ] || lock_deadline=$((SECONDS + 120))
        [ "$SECONDS" -lt "$lock_deadline" ] || die 'load sync lock wait exceeded 120 seconds'
        sleep 2 ;;
      budget)
        current="$(guest_dev python3 -B /tmp/load-probe.py --phase cleaned --remaining \
          --root "$load_root" --state /tmp/orca-load-state.json)" || die 'load sync progress unavailable'
        [[ "$current" =~ ^[0-9]+$ ]] || die 'invalid load sync progress'
        [ "$current" -lt "$remaining" ] || die 'load sync budget exhausted without pruning progress'
        batches=$((batches + 1))
        stage "load sync budget batch $batches: remaining fixtures $remaining -> $current"
        [ "$batches" -lt 6 ] || die 'load sync exhausted its pruning batch budget'
        remaining="$current"
        lock_deadline=0 ;;
      rpc-timeout:*) die "load sync failed: $category" ;;
      mixed|unknown) die "load sync failed: $category" ;;
      *) die 'invalid load sync failure category' ;;
    esac
  done
  die 'load sync exceeded its 600-second convergence budget'
}
: "${clone_root:?disposable clone root is required}"
load_root="$clone_root/.build/load-diagnostic"
load_marker=subyard-orca-load-diagnostic-v1
load_cleanup() {
  guest_dev python3 - "$load_root" "$load_marker" <<'PY'
import pathlib, shutil, sys
root = pathlib.Path(sys.argv[1])
if root.exists():
    if root.is_symlink() or (root / '.marker').read_text().strip() != sys.argv[2]:
        raise SystemExit('load cleanup ownership mismatch')
    shutil.rmtree(root)
PY
}
load_exit() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  load_cleanup || { [ "$rc" -ne 0 ] || rc=3; }
  ( exit "$rc" )
  cleanup
}
trap load_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
orca_load_measure baseline
stage "extended load: generating $load_roots Git roots and 100001 flat files in disposable guest"
guest_dev python3 - "$load_root" "$load_marker" "$load_roots" <<'PY'
import os, pathlib, shutil, subprocess, sys
root = pathlib.Path(sys.argv[1])
root.mkdir(parents=True, exist_ok=False)
(root / '.marker').write_text(sys.argv[2] + '\n')
seed = root / 'seed'
seed.mkdir()
subprocess.run(['git', '-C', str(seed), 'init', '-q'], check=True, timeout=15)
(seed / 'fixture.txt').write_text('load fixture\n')
subprocess.run(['git', '-C', str(seed), 'add', 'fixture.txt'], check=True, timeout=15)
subprocess.run(['git', '-C', str(seed), '-c', 'user.name=Fixture', '-c',
                'user.email=fixture@example.invalid', 'commit', '-qm', 'initial'],
               check=True, timeout=15)
for index in range(int(sys.argv[3])):
    target = root / ('checkout-' + str(index))
    target.mkdir()
    shutil.copytree(seed / '.git', target / '.git', ignore=shutil.ignore_patterns('hooks', 'info'))
flat = root / 'flat-cache'
flat.mkdir()
fd = os.open(flat, os.O_RDONLY | os.O_DIRECTORY)
try:
    for index in range(100001):
        os.close(os.open(str(index), os.O_WRONLY | os.O_CREAT, 0o600, dir_fd=fd))
finally:
    os.close(fd)
PY
load_deadline=$((SECONDS + 600))
load_admitted=0
while [ "$SECONDS" -lt "$load_deadline" ]; do
  load_admitted="$(guest_dev python3 -B /tmp/load-probe.py --phase stock --admission-ready \
    --roots "$load_roots" --root "$load_root" --state /tmp/orca-load-state.json)" \
    || die 'native admission observation unavailable'
  case "$load_admitted" in 0) sleep 4 ;; 1) break ;; *) die 'invalid native admission observation' ;; esac
done
[ "$load_admitted" = 1 ] || die 'load roots did not reach the bounded native admission count'
if [ "$load_lane" = load ]; then
  orca_load_measure stock
  yard orca restart --yes >/dev/null
  orca_load_measure restarted
  orca_load_measure repeated
else
  orca_load_measure loaded
fi
load_cleanup
load_sync
orca_load_measure cleaned
# Observe another timer cycle after removal; IDs and tabs must remain preserved.
sleep 35
orca_load_measure cleaned
stage 'controlled interruption cleans its marker-owned guest fixture'
set +e
guest_dev python3 -B - "$load_root" "$load_marker" <<'PYINTERRUPT'
import importlib.util, os, pathlib, shutil, signal, subprocess, sys, time
root = pathlib.Path(sys.argv[1])
spec = importlib.util.spec_from_file_location('fixture', '/tmp/orca-projects-helper.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)
def terminate(_signal, _frame):
    raise SystemExit(143)
signal.signal(signal.SIGTERM, terminate)
root.mkdir(parents=True, exist_ok=False)
(root / '.marker').write_text(sys.argv[2] + '\n')
try:
    for index in range(3):
        subprocess.run(['git', 'init', '-q', str(root / ('checkout-' + str(index)))],
                       check=True, timeout=15)
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline:
        repos = fixture.call_any_result('/srv/agents/orca/config/orca/orca-runtime.json', 'repo.list', None)
        if sum(repo['path'].startswith(str(root) + '/checkout-') for repo in repos['repos']) == 3:
            os.kill(os.getpid(), signal.SIGTERM)
        time.sleep(3)
    raise SystemExit('interrupt fixture did not register before deadline')
finally:
    if root.is_symlink() or (root / '.marker').read_text().strip() != sys.argv[2]:
        raise SystemExit('interrupt cleanup ownership mismatch')
    shutil.rmtree(root)
PYINTERRUPT
interrupt_rc=$?
set -e
[ "$interrupt_rc" = 143 ] || die 'controlled interrupt did not complete its cleanup'
guest_dev test ! -e "$load_root" || die 'interrupted fixture leaked files'
load_sync
orca_load_measure interrupted-cleaned
trap cleanup EXIT INT TERM
printf 'ok: stock Orca %s admission and cleanup checks; RPC and CPU measurements are diagnostic\n' "$load_lane"

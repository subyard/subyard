#!/usr/bin/env bash
# Authentic published predecessor plus two installed consumer classes on one VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
cd "$ROOT"
die() { printf 'release-activation: %s\n' "$*" >&2; exit 2; }
stage() { printf 'release_activation_stage=%s elapsed_seconds=%s\n' "$1" "$SECONDS"; }
# Changing an activation input leaves ordinary mutations gated until a fresh,
# authorized repair has reconciled the installed runtime.
set_git_mode() {
  local mode="$1" log_prefix="$2"
  yard config set GITHUB_GIT_MODE "$mode" --scope yard --yes > "$log_prefix-setting.log" 2>&1
  yard migrate --yes > "$log_prefix-apply.log" 2>&1
}
trace_deferred_migration() {
  local original_python source before native_exit capture_exit=0 unchanged=0
  original_python="$(command -v python3)"
  source="$STATE/runtime/current/config/profiles/github/runtime.py"
  before="$(hash "$source")"
  install -m 0700 "$ROOT/config/profiles/github/tests/e2e/runtime-observer-trace.py" "$STATE/wrappers/python3"
  # Trace only this invocation. The callback observes original Python returns;
  # installed source and functions, native status and stdout stay untouched.
  if PATH="$STATE/wrappers:$PATH" SUBYARD_TRACE_REAL_PYTHON="$original_python" \
      SUBYARD_TRACE_GITHUB_SOURCE="$source" SUBYARD_TRACE_GITHUB_LOG="$STATE/native-github-observations.log" \
      yard migrate --yes > "$STATE/deferred-migrate.log" 2>&1; then native_exit=0; else native_exit=$?; fi
  [ "$before" != "$(hash "$source")" ] || unchanged=1
  if ! "$original_python" "$ROOT/config/profiles/github/tests/e2e/runtime-observer-trace.py" \
      --verify "$STATE/native-github-observations.log" "$NAMED" > "$STATE/native-github-capture.log" 2>&1; then capture_exit=1; fi
  printf 'release_activation_observation_terminal native_exit=%s capture_exit=%s installed_source_unchanged=%s\n' \
    "$native_exit" "$capture_exit" "$unchanged"
  [ "$native_exit" = 0 ] || return "$native_exit"
  [ "$capture_exit" = 0 ] && [ "$unchanged" = 1 ] || die 'native observation diagnostic was incomplete'
}
# Shared with the controller so both select the same authentic baseline and synthetic B.
candidate_identity() {
  python3 - "$1" "$2" <<'PY'
import json,re,sys
candidate=json.load(open(sys.argv[1]))
baseline=sys.argv[2]
if baseline not in ('0.19.0','0.20.0'):
    raise SystemExit('unsupported authentic activation predecessor')
version=candidate['version']
match=re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?',version)
if not match:
    raise SystemExit('invalid activation candidate version')
major,minor,patch=map(int,match.groups())
if (major,minor,patch)<=tuple(map(int,baseline.split('.'))):
    raise SystemExit('activation candidate must be newer than its authentic predecessor')
if candidate['runtime_file'] not in (f'subyard-{version}-linux-amd64.tar.gz',f'subyard-{version}-linux-arm64.tar.gz'):
    raise SystemExit('activation candidate archive differs from its version')
if not re.fullmatch('[0-9a-f]{64}',candidate['runtime_sha256']):
    raise SystemExit('invalid activation candidate checksum')
print(version,f'{major}.{minor}.{patch+1}-activation-fixture',
      'eeacda9ded1cc4c095ecc1e0569cf6fd96a737b3af6ab30ce7804833e3d8f1d1',sep='\t')
PY
}
if [ "${1:-}" = --candidate-identity ]; then
  [ "$#" = 3 ] || die 'candidate identity requires metadata and baseline'
  candidate_identity "$2" "$3"
  exit
fi
BASELINE=0.19.0
NATIVE_LOSS=''
MODE="${1:-normal}"
if [ "$MODE" != --local-probe ]; then
  MODE=normal
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --baseline) [ "$#" -ge 2 ] || exit 2; BASELINE="$2"; shift 2 ;;
      --native-loss) [ "$#" -ge 2 ] && [ -z "$NATIVE_LOSS" ] || exit 2; NATIVE_LOSS="$2"; shift 2 ;;
      *) die 'guest takes --local-probe or --baseline VERSION [--native-loss owner|helper|group]' ;;
    esac
  done
else [ "$#" = 1 ] || exit 2; fi
case "$NATIVE_LOSS" in ''|owner|helper|group) ;; *) exit 2 ;; esac
if [ "$MODE" = normal ]; then
  [ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] || die 'allocated disposable VM1 required'
  [ "$(id -un)" = dev ] || die 'allocated dev user required'
fi
for command in curl git go jq openssl python3 sha256sum shellcheck sudo timeout; do
  command -v "$command" >/dev/null || die "missing prerequisite: $command"
done
[ "$MODE" = --local-probe ] || sudo -n true || die 'disposable VM passwordless sudo unavailable'
# Keep disposable source/Git fixtures outside managed workspaces, even with TMPDIR set.
STATE="$(mktemp -d /tmp/subyard-release-activation.XXXXXX)"
chmod 0711 "$STATE"
MARKER=subyard-release-activation-v1
printf '%s\n' "$MARKER" > "$STATE/.marker"
chmod 0600 "$STATE/.marker"
TOKEN="$(date +%s)$$"
NAMED="activation-$TOKEN"
PROJECT="activation-$TOKEN-live"
INSTANCE="activation-$TOKEN-live"
NAMED_PROJECT="subyard-$NAMED"
NAMED_INSTANCE="yard-$NAMED"
IMAGE_PROBE_PROJECT="activation-$TOKEN-uid-probe"
IMAGE_PROBE_CLAIMED=0
FIXTURE_DEV_UID=2000
UNIT=subyard-github-default.service
NAMED_UNIT="subyard-github-$NAMED.service"
UNIT_DIR="$HOME/.config/systemd/user"
UNITS_AUTHORIZED=0
# Bind this fixture's creator session; GID is provenance, not a generic product
# readiness requirement. Native leaves run through fresh() with the same UID.
CREATION_GID="$(id -g)"
CREATED_YARDS=()
STARTED="$(date +%s)"
READY_STATE=not-started READY_EXIT=unknown READY_STARTED=$SECONDS
YARD_BIN=''
unit_recipe_owned() {
  local path="$1" unit="$2" yard_name=default
  [ "$UNITS_AUTHORIZED" = 1 ] && [ -f "$path" ] && [ ! -L "$path" ] \
    && [ "$(stat -c '%a:%u:%g' "$path")" = "644:$(id -u):$CREATION_GID" ] || return 1
  case "$unit" in
    "$UNIT"|"$NAMED_UNIT")
      [ "$unit" = "$UNIT" ] || yard_name="$NAMED"
      [ "$(head -n 1 "$path")" = '# Managed by Subyard GitHub broker' ] \
        && grep -Fxq "ExecStart=\"$STATE/data/github-broker/$yard_name-engine\" _github-broker \"$STATE/data/github-broker/$yard_name.json\"" "$path" ;;
    subyard-keys-sync.service) grep -Fxq "ExecStart=$STATE/runtime/current/bin/yard _keys-auto-sync --if-due" "$path" ;;
    subyard-keys-sync.timer)
      [ -f "$STATE/unit-subyard-keys-sync.service.sha" ] \
        && cmp -s "$path" "$STATE/runtime/current/config/systemd/subyard-keys-sync.timer.in" ;;
    *) return 1 ;;
  esac
}
claim_units() {
  local unit path
  if [ "$MODE" = normal ]; then
    [ "$(fresh id -g)" = "$CREATION_GID" ] || die 'fixture creator group changed after the captured session'
  fi
  for unit in "$UNIT" "$NAMED_UNIT" subyard-keys-sync.service subyard-keys-sync.timer; do
    path="$UNIT_DIR/$unit"
    [ -e "$path" ] || [ -L "$path" ] || continue
    unit_recipe_owned "$path" "$unit" || die "fixture unit recipe changed: $unit"
    sha256sum "$path" > "$STATE/unit-$unit.sha"
    stat -c '%d:%i:%u:%g:%a:%s:%Y:%Z' "$path" > "$STATE/unit-$unit.stat"
  done
}
unit_owned() {
  local path="$1" unit="$2" reason=''
  if ! unit_recipe_owned "$path" "$unit"; then reason=recipe;
  elif [ ! -f "$STATE/unit-$unit.sha" ] || [ ! -f "$STATE/unit-$unit.stat" ]; then reason=missing-claim;
  elif ! cmp -s "$STATE/unit-$unit.sha" <(sha256sum "$path"); then reason=content;
  elif ! cmp -s "$STATE/unit-$unit.stat" <(stat -c '%d:%i:%u:%g:%a:%s:%Y:%Z' "$path"); then reason=metadata;
  fi
  [ -z "$reason" ] || {
    printf 'release_activation_cleanup_guard unit=%s reason=%s\n' "$unit" "$reason" >&2
    return 1
  }
}
cleanup_image_probe() {
  [ "$IMAGE_PROBE_CLAIMED" = 1 ] || return 0
  local marker instances
  marker="$(incus project get "$IMAGE_PROBE_PROJECT" user.subyard.activation-fixture)" || return 1
  [ "$marker" = "$STATE" ] || return 1
  instances="$(incus list --project "$IMAGE_PROBE_PROJECT" -f json)" || return 1
  jq -e --arg marker "$STATE" 'type=="array" and length<=1 and
    all(.[]; .name=="uid-probe" and .config["user.subyard.activation-fixture"]==$marker)' \
    <<< "$instances" >/dev/null || return 1
  if [ "$(jq length <<< "$instances")" = 1 ]; then
    incus delete uid-probe --project "$IMAGE_PROBE_PROJECT" --force || return 1
  fi
  incus project delete "$IMAGE_PROBE_PROJECT" || return 1
  IMAGE_PROBE_CLAIMED=0
}
cleanup() {
  local original=$? failed=0 yard project marker safe_units=1 unit path mutated_units=0
  local cleanup_loss="${NATIVE_LOSS:-}"
  trap - EXIT INT TERM
  set +e
  if [ "$original" != 0 ]; then
    # Fixed in-memory facts only; never repeat the command or print its private JSON.
    printf 'release_activation_ready_failure elapsed_seconds=%s state=%s native_exit=%s trigger_exit=%s\n' \
      "$SECONDS" "${READY_STATE:-not-started}" "${READY_EXIT:-unknown}" "$original" >&2 || :
  fi
  # An interrupted update is a fixture-owned process group, never the user manager.
  if [ -f "$STATE/updater.json" ]; then
    python3 - "$STATE/updater.json" <<'PY'
import json, os, signal, sys
value=json.load(open(sys.argv[1]))
for group in value.get('groups', [])+[value]:
    try:
        pid=group['pid']
        if os.getpgid(pid)==pid and open('/proc/'+str(pid)+'/stat').read().split()[21]==group['start']:
            os.killpg(pid, signal.SIGKILL)
    except (ProcessLookupError, FileNotFoundError):
        pass
PY
    [ "$?" = 0 ] || failed=1
  fi
  cleanup_image_probe || failed=1
  if [ "$UNITS_AUTHORIZED" = 1 ]; then
    for unit in "$UNIT" "$NAMED_UNIT" subyard-keys-sync.service subyard-keys-sync.timer; do
      path="$UNIT_DIR/$unit"
      [ -e "$path" ] || [ -L "$path" ] || continue
      unit_owned "$path" "$unit" || { safe_units=0; failed=1; }
    done
  fi
  for yard in "${CREATED_YARDS[@]}"; do
    [ "$safe_units" = 1 ] || continue
    project="$PROJECT"; [ "$yard" = default ] || project="$NAMED_PROJECT"
    if incus project show "$project" >/dev/null 2>&1; then
      marker="$(incus project get "$project" user.subyard.activation-fixture 2>/dev/null)"
      if [ "$marker" != "$STATE" ]; then
        printf 'release-activation: cleanup refused unmarked project %s\n' "$project" >&2
        failed=1
        continue
      fi
      if [ -n "$cleanup_loss" ] && [ "$cleanup_loss" != owner ] && [ -f "$STATE/native-loss/after-cut.json" ]; then
        printf 'release_activation_pending_fixture_cleanup=allocation-release yard=%s\n' "$yard"
      else
        fresh timeout --signal=TERM --kill-after=10 75 "$YARD_BIN" -Y "$yard" teardown --yes \
          > "$STATE/cleanup-$yard.log" 2>&1 || failed=1
      fi
    fi
  done
  for unit in "$UNIT" "$NAMED_UNIT" subyard-keys-sync.timer subyard-keys-sync.service; do
    [ "$UNITS_AUTHORIZED" = 1 ] || continue
    path="$UNIT_DIR/$unit"
    if [ -f "$path" ] && [ ! -L "$path" ]; then
      unit_owned "$path" "$unit" || { failed=1; continue; }
      mutated_units=1
      systemctl_user stop "$unit" || failed=1
      systemctl_user disable "$unit" >/dev/null 2>&1 || failed=1
      rm -- "$path" || failed=1
    fi
  done
  [ "$mutated_units" = 0 ] || systemctl_user daemon-reload || failed=1
  printf 'release_activation_terminal original_exit=%s cleanup_exit=%s duration_seconds=%s\n' \
    "$original" "$failed" "$(( $(date +%s) - STARTED ))"
  # Preserve full native logs before the standard runner deletes its source/VM.
  for log in "$STATE"/*.log; do
    [ -f "$log" ] || continue
    printf 'release_activation_log=%s\n' "${log##*/}"
    cat "$log"
  done
  if [ "$failed" = 0 ] && { [ -z "$cleanup_loss" ] || [ "$cleanup_loss" = owner ] || [ ! -f "$STATE/native-loss/after-cut.json" ]; }; then
    [[ "$STATE" = /tmp/subyard-release-activation.* ]] && [ ! -L "$STATE" ] \
      && [ "$(cat "$STATE/.marker")" = "$MARKER" ] || { [ "$original" != 0 ] && exit "$original"; exit 3; }
    find "$STATE" -depth -delete || { [ "$original" != 0 ] && exit "$original"; exit 3; }
  fi
  [ "$failed" = 0 ] || [ "$original" != 0 ] || exit 3
  exit "$original"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
incus() { sudo -n /usr/bin/incus "$@"; }
fresh() {
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$@"
    sg incus-admin -c "exec $command"
  else
    "$@"
  fi
}
yard() { fresh "$YARD_BIN" "$@"; }
systemctl_user() {
  XDG_RUNTIME_DIR="/run/user/$(id -u)" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$(id -u)/bus" \
    timeout --signal=TERM --kill-after=2 15 systemctl --user "$@"
}
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
guest_dev() { guest runuser -u dev -- "$@"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }
cat > "$STATE/check-dev-uid.sh" <<'SH'
set -eu
command -v getent >/dev/null
if entry="$(getent passwd dev)"; then
  uid="${entry#*:*:}"; uid="${uid%%:*}"
  [ "$uid" = "$1" ]
else
  [ "$?" = 2 ] || exit 1
  if getent passwd "$1" >/dev/null; then exit 1; else [ "$?" = 2 ]; fi
fi
SH
setting() { yard config show "$1" | sed -n 's/^effective: //p'; }
ready() {
  READY_STATE=running READY_EXIT=unknown READY_STARTED=$SECONDS
  printf 'release_activation_ready state=start elapsed_seconds=%s\n' "$SECONDS" || :
  if yard migrate --check --json > "$STATE/readiness.json"; then
    READY_EXIT=0
  else
    READY_EXIT=$?
  fi
  READY_STATE=returned
  printf 'release_activation_ready state=returned elapsed_seconds=%s native_exit=%s operation_elapsed_seconds=%s\n' \
    "$SECONDS" "$READY_EXIT" "$((SECONDS - READY_STARTED))" || :
  [ "$READY_EXIT" = 0 ] || return "$READY_EXIT"
  jq -e '.outcome.status == "ready"' "$STATE/readiness.json" >/dev/null || return $?
  jq -c '.outcome | {status,code,warnings}' "$STATE/readiness.json" || return $?
  READY_STATE=complete
}
update() {
  local version="$1" release="$2"; shift 2
  fresh env YARD_RELEASE_BASE_URL="file://$release" "$YARD_BIN" update --version "$version" --offline "$@"
}
interrupt_driver() {
  fresh python3 - "$@" <<'PY'
import hashlib,json,os,signal,subprocess,sys,time
from pathlib import Path
root=Path(sys.argv[1]); probe=sys.argv[2:]==['--probe']
pending=0
process=None
ownership=None
foreign=None
probe_cancelled=False

def received(signum, _frame):
    global pending
    pending=signum

for signum in (signal.SIGTERM, signal.SIGINT):
    signal.signal(signum,received)

def info(pid):
    path=Path('/proc/'+str(pid)+'/stat')
    text=path.read_text(); fields=text.rsplit(') ',1)[1].split()
    return {'pid':pid,'parent':int(fields[1]),'group':int(fields[2]),
            'start':fields[19],'state':fields[0],'uid':path.stat().st_uid}

def same(record):
    try:
        now=info(record['pid'])
        return now['start']==record['start'] and now['group']==record['pid'] and now['uid']==os.getuid()
    except (ProcessLookupError,FileNotFoundError):
        return False

def save():
    target=root/'updater.json'; temporary=root/'updater.json.next'
    temporary.write_text(json.dumps(ownership)); temporary.chmod(0o600); temporary.replace(target)

def capture():
    if not same(ownership): return
    for path in Path('/proc').glob('[0-9]*/stat'):
        try:
            item=info(int(path.parent.name))
            if item['pid']!=item['group'] or item['pid']==process.pid or item['uid']!=os.getuid(): continue
            ancestor=item['pid']
            while ancestor>1 and ancestor!=process.pid:
                ancestor=info(ancestor)['parent']
            if ancestor==process.pid and not any(v['pid']==item['pid'] for v in ownership['groups']):
                ownership['groups'].append({'pid':item['pid'],'start':item['start']})
        except (ProcessLookupError,FileNotFoundError,PermissionError):
            continue
    save()

def cancel():
    if pending: raise SystemExit(128+pending)

def drain():
    if process is None: return
    # Freeze the proven updater first; then capture/freeze its separate native
    # groups before the parent disappears. A second pass closes the fork window.
    if same(ownership):
        os.killpg(process.pid,signal.SIGSTOP)
        for _ in range(2):
            capture()
            for group in ownership['groups']:
                if same(group): os.killpg(group['pid'],signal.SIGSTOP)
    for group in ownership['groups']:
        if same(group): os.killpg(group['pid'],signal.SIGKILL)
    if same(ownership): os.killpg(process.pid,signal.SIGKILL)
    process.wait(timeout=10)
    deadline=time.monotonic()+5
    while time.monotonic()<deadline:
        live=[]
        for group in ownership['groups']:
            if same(group) and info(group['pid'])['state']!='Z': live.append(group)
        if not live: break
        time.sleep(.05)
    else: raise RuntimeError('owned native process group did not drain')
    (root/'updater.json').unlink(missing_ok=True)

try:
    if probe:
        # Same ancestry/cancellation machinery, with a separate-session child
        # before the delivery marker and an unrelated process that must survive.
        foreign=subprocess.Popen(['sleep','30'],start_new_session=True)
        child_code="import pathlib,subprocess,time,sys; p=subprocess.Popen(['sleep','30'],start_new_session=True); pathlib.Path(sys.argv[1]).write_text(str(p.pid)); time.sleep(30)"
        command=[sys.executable,'-c',child_code,str(root/'probe-child')]
        env=os.environ.copy()
    else:
        state,engine,release,version,unit,oldpid,oldhash,wanted=sys.argv[1:]
        env=dict(os.environ,PATH=str(root/'wrappers')+':'+os.environ['PATH'],YARD_RELEASE_BASE_URL='file://'+release)
        command=[engine,'update','--offline','--version',version,'--yes']
    with (root/'interrupted-update.log').open('wb') as log:
        process=subprocess.Popen(command,env=env,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
        record=info(process.pid)
        ownership={'pid':process.pid,'start':record['start'],'groups':[]}
        save()
        # The signal handler only records cancellation, so even a signal during
        # Popen/receipt creation reaches this finally block with the exact child.
        deadline=time.monotonic()+(10 if probe else 180)
        while not (root/'delivery-paused').exists():
            capture(); cancel()
            if process.poll() is not None: raise RuntimeError('updater exited before native delivery checkpoint')
            if time.monotonic()>deadline: raise RuntimeError('native delivery checkpoint deadline')
            if probe and (root/'probe-child').exists():
                os.kill(os.getpid(),signal.SIGTERM)
            time.sleep(.1)
        capture(); cancel()
        journal=json.loads((root/'config/release-transition/v2/journal.json').read_text())
        assert journal['checkpoint']=='reconciling'
        def sha(path): return hashlib.sha256(Path(path).read_bytes()).hexdigest()
        assert sha(root/'data/github-broker/default-engine')==wanted
        assert sha('/proc/'+oldpid+'/exe')==oldhash
        wrapper=int((root/'delivery-paused').read_text()); native=info(wrapper)['group']
        assert any(group['pid']==native and same(group) for group in ownership['groups'])
        assert same(ownership) and process.poll() is None
        (root/'interrupted-journal.json').write_text(json.dumps(journal))
        drain()
        assert process.returncode<0
        print('release_activation_interruption=after-delivery-before-restart old_loaded_identity=verified journal=reconciling')
except SystemExit as error:
    if not probe or error.code!=143: raise
    probe_cancelled=True
finally:
    if process is not None and process.poll() is None: drain()
    if foreign is not None:
        assert foreign.poll() is None, 'cancellation killed an unrelated process'
        foreign.terminate(); foreign.wait(timeout=5)
if probe:
    assert probe_cancelled
    print('ok: TERM before delivery marker drained separate owned child group; unrelated process survived')
PY
}
verify_native_receipts() {
  local arch usage_pin age_pin sops_pin tools
  arch="$(guest dpkg --print-architecture)"
  case "$arch" in amd64|arm64) ;; *) die 'unexpected native receipt architecture' ;; esac
  usage_pin="$(setting "CCUSAGE_SHA256_${arch^^}")"
  guest python3 - "$(setting CCUSAGE_VERSION)" "$usage_pin" <<'PY'
import hashlib,json,stat,sys
from pathlib import Path
binary=Path('/usr/local/bin/ccusage'); receipt=Path(str(binary)+'.subyard-runtime.json')
for path,mode in ((binary,0o755),(receipt,0o600)):
    info=path.lstat()
    assert stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode)==mode and (info.st_uid,info.st_gid)==(0,0)
expected={'schema':1,'version':sys.argv[1],'archive':sys.argv[2],'binary':hashlib.sha256(binary.read_bytes()).hexdigest()}
assert json.loads(receipt.read_text())==expected
print('ok: native ccusage receipt matches installed bytes, exact version/archive pin and protected modes')
PY
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die 'unexpected owner architecture' ;; esac
  age_pin="$(setting "SUBYARD_AGE_SHA256_${arch^^}")"
  sops_pin="$(setting "SUBYARD_SOPS_SHA256_${arch^^}")"
  tools="$(setting SUBYARD_KEYS_TOOLS_DIR)"
  [ "$tools" = "$SUBYARD_CONFIG_HOME/tools" ] || die 'fixture key tools escaped their owned root'
  python3 - "$tools/bin" "$(setting SUBYARD_AGE_VERSION)" "$(setting SUBYARD_SOPS_VERSION)" "$age_pin" "$sops_pin" "$CREATION_GID" <<'PY'
import hashlib,json,os,stat,subprocess,sys
from pathlib import Path
root=Path(sys.argv[1]); owner=(os.getuid(),int(sys.argv[6])); values={}
for name,key,version in (('age','age_binary',sys.argv[2]),('age-keygen','keygen_binary',sys.argv[2]),('sops','sops_binary',sys.argv[3])):
    path=root/name; info=path.lstat()
    assert stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode)==0o755 and (info.st_uid,info.st_gid)==owner
    output=subprocess.check_output([str(path),'--version'],stderr=subprocess.STDOUT,timeout=5).decode().splitlines()[0]
    assert version in [part.removeprefix('v') for part in output.split()]
    values[key]=hashlib.sha256(path.read_bytes()).hexdigest()
receipt=root/'.subyard-native-tools.json'; info=receipt.lstat()
assert stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode)==0o600 and (info.st_uid,info.st_gid)==owner
expected=dict(schema=1,age=sys.argv[2],sops=sys.argv[3],age_archive=sys.argv[4],sops_archive=sys.argv[5],**values)
assert json.loads(receipt.read_text())==expected
print('ok: age/SOPS receipts match installed bytes, exact versions/archive pins and protected modes')
PY
}
verify_git_wiring() {
  guest_dev /usr/local/bin/subyard-github status --git | jq -e '.mode=="broker-first" and .state=="current"' >/dev/null
  guest_dev git config --get subyard.githubGitMode | grep -Fxq broker-first
  [ "$(guest stat -c '%a:%u:%g' /usr/local/bin/git-remote-subyard-github)" = 755:0:0 ] || die 'Git remote helper permissions differ'
  [ "$(guest stat -c '%a:%u:%g' /home/dev/.config/subyard/github-git.conf)" = "644:$FIXTURE_DEV_UID:$(guest id -g dev)" ] || die 'Git fragment permissions differ'
  guest_dev git config --file /home/dev/.config/git/config --get-all include.path | grep -Fxq /home/dev/.config/subyard/github-git.conf
}
verify_runtime() {
  local source="$1" wanted preview ccusage
  if [ "$(setting GITHUB_GIT_MODE)" = broker-first ]; then verify_git_wiring; fi
  wanted="$(hash "$source/config/profiles/github/bin/broker")"
  guest_dev /usr/local/bin/subyard-github status --runtime \
    | jq -e --arg wanted "$wanted" '.protocol==1 and .broker_sha256==$wanted and .client_sha256==$wanted' >/dev/null
  [ "$(hash "$SUBYARD_HOME/github-broker/default-engine")" = "$wanted" ] || die 'owner disk binary differs'
  pid="$(systemctl_user show "$UNIT" -p MainPID --value)"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] && [ "$(hash "/proc/$pid/exe")" = "$wanted" ] || die 'loaded owner binary differs'
  [ "$(guest sha256sum /usr/local/libexec/subyard/github-client | cut -d' ' -f1)" = "$wanted" ] || die 'guest disk binary differs'
  preview="$(hash "$source/config/preview/subyard-preview")"
  [ "$(guest sha256sum /usr/local/bin/subyard-preview | cut -d' ' -f1)" = "$preview" ] || die 'preview helper differs'
  [ "$(guest stat -c '%a:%u:%g' /usr/local/bin/subyard-preview)" = 755:0:0 ] || die 'preview helper permissions differ'
  [ "$(guest stat -c '%a:%u:%g' /etc/subyard/preview.json)" = 644:0:0 ] || die 'preview metadata permissions differ'
  [ "$(guest sha256sum /etc/subyard/preview.json | cut -d' ' -f1)" = \
    "$(incus config get "$INSTANCE" user.subyard.preview_endpoint_sha256 --project "$PROJECT")" ] || die 'preview endpoint receipt differs'
  guest python3 -c 'import json; v=json.load(open("/etc/subyard/preview.json")); assert v["version"]==1 and v["host"] and 0<int(v["port"])<65536'
  ccusage="$(setting CCUSAGE_VERSION)"
  [ "$(guest /usr/local/bin/ccusage --version)" = "ccusage $ccusage" ] || die 'native usage reporter version differs'
  guest sh -eu -c 'test "$(stat -c "%a:%u:%g" /usr/local/bin/ccusage)" = 755:0:0; tail -n 1 /var/lib/subyard/ccusage-ownership | sha256sum -c - >/dev/null'
  verify_native_receipts
  python3 - "$source" "$STATE/runtime/current/bin/yard" "$UNIT_DIR" "$CREATION_GID" <<'PY'
import json,os,stat,sys
from pathlib import Path
source=Path(sys.argv[1]); yard=sys.argv[2]; units=Path(sys.argv[3])
for name,mode in (('subyard-keys-sync.service',0o644),('subyard-keys-sync.timer',0o644),('.subyard-keys-sync.json',0o600)):
    path=units/name; info=path.lstat()
    assert stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode)==mode
    assert (info.st_uid,info.st_gid)==(os.getuid(),int(sys.argv[4]))
assert json.loads((units/'.subyard-keys-sync.json').read_text())==dict(schema=1,yard=yard)
assert (units/'subyard-keys-sync.service').read_bytes()==(source/'config/systemd/subyard-keys-sync.service.in').read_bytes().replace(b'@YARD_BIN@',yard.encode())
assert (units/'subyard-keys-sync.timer').read_bytes()==(source/'config/systemd/subyard-keys-sync.timer.in').read_bytes()
print('ok: owned timer receipt and installed unit bytes match the active release')
PY
  guest_dev /usr/local/bin/subyard-github status | jq -e '.configured==true' >/dev/null
  guest_dev sha256sum /home/dev/operator-opaque | cmp -s - "$STATE/opaque.sha" || die 'opaque user data changed'
  cmp -s "$STATE/connection.sha" <(sha256sum "$SUBYARD_KEYS_CONSUMER_ROOT/github/connection.json") || die 'credentials changed'
  cmp -s "$STATE/selection-live" "$SUBYARD_CONFIG_HOME/yards/default/config.env" || die 'live selection changed'
  cmp -s "$STATE/selection-stopped" "$SUBYARD_CONFIG_HOME/yards/$NAMED/config.env" || die 'named yard selection changed'
  [ "$(systemctl_user is-enabled subyard-keys-sync.timer 2>/dev/null || true)" = disabled ] || die 'disabled credential timer was enabled'
  [ "$(systemctl_user is-active subyard-keys-sync.timer 2>/dev/null || true)" = inactive ] || die 'inactive credential timer was started'
  cmp -s "$STATE/selection-absent" "$SUBYARD_CONFIG_HOME/yards/$ABSENT/config.env" || die 'absent yard selection changed'
  [ "$(yard -Y "$ABSENT" config show ENVIRONMENT_PROFILES | sed -n 's/^effective: //p')" = '<unset>' ] || die 'absent yard did not retain empty effective selection'
  ! incus project show "subyard-$ABSENT" >/dev/null 2>&1 || die 'absent unselected yard was created'
  printf 'release_activation_selection live=github named=github absent=empty\n'
}

if [ "$MODE" = --local-probe ]; then
  (
    # Exercise the same sequence used by the physical fixture. A setting write
    # creates pending activation; ordinary profile mutations must remain gated.
    pending="$STATE/git-mode-pending"
    yard() {
      case "$*" in
        'config set GITHUB_GIT_MODE broker-first --scope yard --yes'|'config set GITHUB_GIT_MODE off --scope yard --yes')
          touch "$pending" ;;
        'migrate --yes') [ -f "$pending" ] && rm -- "$pending" ;;
        'profile enable github --yes') [ ! -e "$pending" ] ;;
        *) return 99 ;;
      esac
    }
    for mode in broker-first off broker-first; do
      set_git_mode "$mode" "$STATE/git-mode-probe"
      yard profile enable github --yes
    done
  ) || die 'Git mode change did not authorize runtime repair before profile mutation'
  printf 'ok: Git mode changes reconcile activation through fresh migrate before profile mutations\n'
  mkdir "$STATE/uid-commands"
  cat > "$STATE/uid-commands/getent" <<'SH'
#!/bin/sh
case "$1 $2" in
  'passwd dev')
    [ "${PROBE_NAME_LOOKUP:-2}" = 2 ] || exit "$PROBE_NAME_LOOKUP"
    [ "${PROBE_DEV_UID:-}" ] || exit 2
    printf 'dev:x:%s:2000:fixture:/home/dev:/bin/sh\n' "$PROBE_DEV_UID" ;;
  'passwd 2000') exit "${PROBE_UID_LOOKUP:-2}" ;;
  *) exit 99 ;;
esac
SH
  chmod 0755 "$STATE/uid-commands/"*
  PATH="$STATE/uid-commands:$PATH" sh "$STATE/check-dev-uid.sh" "$FIXTURE_DEV_UID"
  PROBE_DEV_UID=2000 PATH="$STATE/uid-commands:$PATH" sh "$STATE/check-dev-uid.sh" "$FIXTURE_DEV_UID"
  for lookup in 0 1; do
    if PROBE_UID_LOOKUP="$lookup" PATH="$STATE/uid-commands:$PATH" sh "$STATE/check-dev-uid.sh" "$FIXTURE_DEV_UID"; then die 'UID collision or unavailable lookup accepted'; fi
  done
  if PROBE_DEV_UID=1000 PATH="$STATE/uid-commands:$PATH" sh "$STATE/check-dev-uid.sh" "$FIXTURE_DEV_UID"; then die 'existing different dev UID accepted'; fi
  if PROBE_NAME_LOOKUP=1 PATH="$STATE/uid-commands:$PATH" sh "$STATE/check-dev-uid.sh" "$FIXTURE_DEV_UID"; then die 'unavailable dev name lookup accepted'; fi
  printf 'ok: free or matching dev UID accepted; occupied, mismatched and unavailable identities refused\n'
  for probe_case in foreign-project foreign-instance unavailable owned; do
    (
      IMAGE_PROBE_CLAIMED=1
      mutations="$STATE/probe-$probe_case-mutations"
      incus() {
        case "$1 $2" in
          'project get')
            if [ "$probe_case" = foreign-project ]; then printf 'foreign\n'; else printf '%s\n' "$STATE"; fi ;;
          'list --project')
            [ "$probe_case" != unavailable ] || return 99
            probe_name=uid-probe; [ "$probe_case" != foreign-instance ] || probe_name=foreign
            jq -n --arg name "$probe_name" --arg marker "$STATE" '[{name:$name,config:{"user.subyard.activation-fixture":$marker}}]' ;;
          'delete uid-probe'|'project delete') printf '%s\n' "$*" >> "$mutations" ;;
          *) return 99 ;;
        esac
      }
      if cleanup_image_probe; then
        [ "$probe_case" = owned ] && [ "$IMAGE_PROBE_CLAIMED" = 0 ] && [ "$(wc -l < "$mutations")" = 2 ] || exit 1
      else
        [ "$probe_case" != owned ] && [ ! -e "$mutations" ] || exit 1
      fi
    ) || die 'cached-image probe ownership guard failed'
  done
  printf 'ok: cached-image probe cleanup accepts exact ownership and preserves foreign or unavailable state\n'
  # Exercise cleanup itself against a pre-existing managed-looking foreign unit.
  PROBE_UNITS="$(mktemp -d /tmp/subyard-activation-unit-probe.XXXXXX)"
  UNIT_DIR="$PROBE_UNITS"
  printf '# Managed by Subyard GitHub broker\nExecStart=/foreign/operator/broker\n' > "$UNIT_DIR/$UNIT"
  foreign_before="$(hash "$UNIT_DIR/$UNIT")"
  interrupt_driver "$STATE" --probe
  trap - EXIT INT TERM
  # Even identical fixture-looking bytes cannot authorize a replacement inode.
  UNITS_AUTHORIZED=1
  original_gid="$(id -g)"
  alternate_gid=''
  for group_gid in $(id -G); do
    [ "$group_gid" = "$original_gid" ] || { alternate_gid="$group_gid"; break; }
  done
  if [ -n "$alternate_gid" ]; then
    group_name="$(getent group "$alternate_gid" | cut -d: -f1)"
    [ -n "$group_name" ] || die 'local alternate primary group name unavailable'
    CREATION_GID="$(sg "$group_name" -c 'id -g')"
    [ "$CREATION_GID" = "$alternate_gid" ] || die 'refreshed session did not select expected primary group'
    printf -v create_command '%q ' bash -c \
      'printf "# Managed by Subyard GitHub broker\nExecStart=\"%s/data/github-broker/default-engine\" _github-broker \"%s/data/github-broker/default.json\"\n" "$1" "$1" > "$2"; chmod 0644 "$2"' \
      _ "$STATE" "$UNIT_DIR/$UNIT"
    # Replace the foreign probe first so file creation gets the new primary GID.
    rm -- "$UNIT_DIR/$UNIT"
    sg "$group_name" -c "$create_command"
    unit_recipe_owned "$UNIT_DIR/$UNIT" "$UNIT" || die 'known changed-primary-group unit was rejected'
    CREATION_GID="$original_gid"
    if unit_recipe_owned "$UNIT_DIR/$UNIT" "$UNIT"; then die 'incorrect original-group provenance was accepted'; fi
    CREATION_GID="$alternate_gid"
    printf 'ok: changed primary GID creation accepted; incorrect original GID rejected\n'
  else
    printf 'local altered-primary-GID probe unavailable; physical creation-context preflight remains required\n'
    printf '# Managed by Subyard GitHub broker\nExecStart="%s/data/github-broker/default-engine" _github-broker "%s/data/github-broker/default.json"\n' \
      "$STATE" "$STATE" > "$UNIT_DIR/$UNIT"
  fi
  chmod 0644 "$UNIT_DIR/$UNIT"
  claim_units
  cp "$UNIT_DIR/$UNIT" "$UNIT_DIR/replacement"
  chgrp "$CREATION_GID" "$UNIT_DIR/replacement"
  chmod 0644 "$UNIT_DIR/replacement"
  mv -f -- "$UNIT_DIR/replacement" "$UNIT_DIR/$UNIT"
  replaced_hash="$(hash "$UNIT_DIR/$UNIT")"
  printf 'release_activation_probe=replacement-inode expected_cleanup_exit=1 expected_native_exit=3\n'
  if (cleanup) > "$STATE/replacement-cleanup.probe" 2>&1; then die 'replacement unit was accepted for cleanup'; else probe_rc=$?; fi
  [ "$probe_rc" = 3 ] && [ "$(hash "$UNIT_DIR/$UNIT")" = "$replaced_hash" ] || die 'replacement unit was mutated'
  grep -Fq "release_activation_cleanup_guard unit=$UNIT reason=metadata" "$STATE/replacement-cleanup.probe" \
    || die 'replacement cleanup refusal did not classify its guard'
  printf 'ok: replacement unit inode was refused and preserved\n'
  UNITS_AUTHORIZED=0
  printf '# Managed by Subyard GitHub broker\nExecStart=/foreign/operator/broker\n' > "$UNIT_DIR/$UNIT"
  (cleanup)
  [ "$(hash "$UNIT_DIR/$UNIT")" = "$foreign_before" ] || die 'collision cleanup changed pre-existing foreign unit'
  [[ "$PROBE_UNITS" = /tmp/subyard-activation-unit-probe.* ]] && [ ! -L "$PROBE_UNITS" ] || exit 3
  find "$PROBE_UNITS" -depth -delete
  printf 'ok: early unit collision cleanup preserved the pre-existing managed-looking unit\n'
  exit 0
fi

stage capacity-and-platform
bash "$ROOT/dev/e2e/p0-guest.sh" capacity-preflight "$TOKEN"
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$TOKEN"
fresh id -nG | tr ' ' '\n' | grep -Fxq incus-admin || die 'platform did not establish a usable refreshed Incus operator session'
CREATION_GID="$(fresh id -g)"
printf 'release_activation_creation_context uid=%s gid=%s validation_gid=%s\n' "$(id -u)" "$CREATION_GID" "$(id -g)"
# The cached cloud image already uses UID 1000. Prove the configured fixture UID
# against that exact image before the authentic predecessor performs provisioning.
stage cached-image-dev-uid
! incus project show "$IMAGE_PROBE_PROJECT" >/dev/null 2>&1 || die 'UID probe project already exists'
IMAGE_PROBE_CLAIMED=1
incus project create "$IMAGE_PROBE_PROJECT" -c features.images=false -c "user.subyard.activation-fixture=$STATE"
incus launch subyard-e2e-debian-13-cloud-container uid-probe --project "$IMAGE_PROBE_PROJECT" --storage default \
  -c "user.subyard.activation-fixture=$STATE" -c limits.memory=512MiB -c limits.cpu=1
incus exec uid-probe --project "$IMAGE_PROBE_PROJECT" -- sh -s -- "$FIXTURE_DEV_UID" < "$STATE/check-dev-uid.sh" \
  || die 'configured fixture DEV_UID is occupied, mismatched or unavailable in the cached image'
cleanup_image_probe || die 'UID probe cleanup failed'
printf 'release_activation_dev_uid=%s cached_image_availability=verified\n' "$FIXTURE_DEV_UID"
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
FINAL_ENGINE="$(release_candidate_prepare "$ROOT")" || die 'frozen transported candidate is required'
read -r CANDIDATE SYNTHETIC BASELINE_INSTALLER_SHA < <(candidate_identity "$ROOT/.subyard-acceptance/candidate.json" "$BASELINE") \
  || die 'frozen candidate identity is invalid'
[ "$("$FINAL_ENGINE" --version)" = "yard-engine $CANDIDATE" ] || die 'frozen candidate engine version differs from metadata'
printf 'release_activation_candidate=%s authentic_predecessor=%s synthetic_successor=%s\n' "$CANDIDATE" "$BASELINE" "$SYNTHETIC"
FINAL_SOURCE="${FINAL_ENGINE%/bin/yard-engine}"
FINAL_RELEASE="$ROOT/.subyard-acceptance/release"
for unit in "$UNIT" "$NAMED_UNIT" subyard-keys-sync.service subyard-keys-sync.timer .subyard-keys-sync.json; do
  [ ! -e "$UNIT_DIR/$unit" ] && [ ! -L "$UNIT_DIR/$unit" ] || die "fixture unit already exists: $unit"
done
UNITS_AUTHORIZED=1
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export SUBYARD_KEYS_CONSUMER_ROOT="$STATE/consumers" SUBYARD_NO_AUDIT=1 MIN_DISK_GIB=1
# This setting is shipped/command-only; use its canonical root consistently.
export SUBYARD_KEYS_TOOLS_DIR="$SUBYARD_CONFIG_HOME/tools"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export YARD_RUNTIME_ROOT="$STATE/runtime" YARD_RELEASE_CACHE="$STATE/releases" YARD_BIN_DIR="$STATE/bin"
export YARD_SHELL_RC="$STATE/bashrc" YARD_LOGIN_RC="$STATE/profile"
XDG_RUNTIME_DIR="/run/user/$(id -u)"
DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
export XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS
unset YARD_ENGINE_PATH ENVIRONMENT_PROFILES SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/default" "$SUBYARD_CONFIG_HOME/yards/$NAMED"
ABSENT="absent-$TOKEN"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$ABSENT"
printf 'SSH_PORT=%s\nENVIRONMENT_PROFILES=\n' "$((35000 + ($$ % 10000) + 2))" > "$SUBYARD_CONFIG_HOME/yards/$ABSENT/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$ABSENT/config.env"
cp "$SUBYARD_CONFIG_HOME/yards/$ABSENT/config.env" "$STATE/selection-absent"
SSH_PORT=$((35000 + ($$ % 10000)))
cat > "$SUBYARD_CONFIG_HOME/config.env" <<EOF
CODING_TOOL_INTEGRATIONS=
SUBYARD_KEYS_CONSUMER_ROOT=$SUBYARD_KEYS_CONSUMER_ROOT
STORAGE_PATH=$STORAGE_PATH
DEV_UID=$FIXTURE_DEV_UID
HOST_BASE=$STATE/host
HOST_MOUNTS=
HOST_LINKS=
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
BASE_IMAGE=subyard-e2e-debian-13-cloud-container
BASE_IMAGE_FALLBACK=subyard-e2e-debian-13-cloud-container
EOF
cat > "$SUBYARD_CONFIG_HOME/yards/default/config.env" <<EOF
INCUS_PROJECT=$PROJECT
YARD_INSTANCE_NAME=$INSTANCE
SSH_PORT=$SSH_PORT
ENVIRONMENT_PROFILES=github
EOF
cat > "$SUBYARD_CONFIG_HOME/yards/$NAMED/config.env" <<EOF
SSH_PORT=$((SSH_PORT+1))
ENVIRONMENT_PROFILES=
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env" "$SUBYARD_CONFIG_HOME"/yards/*/config.env
stage native-operator-fd-readiness
install -d -m 0700 "$STATE/empty-reader-source"
fresh python3 - "$FINAL_SOURCE" "$SUBYARD_CONFIG_HOME" "$STATE/empty-reader-source" "$CREATION_GID" "$CANDIDATE" <<'PY'
import os, pwd, subprocess, sys
source, config, empty, creator_gid, version = sys.argv[1:]
assert os.getgid() == int(creator_gid)
operator = pwd.getpwuid(os.getuid())
source_fd = os.open(source, os.O_RDONLY | os.O_DIRECTORY)
empty_fd = os.open(empty, os.O_RDONLY | os.O_DIRECTORY)
anchor = f'/proc/{os.getpid()}/fd/'
script = r'''set -euo pipefail
exec {repository}< "$1"
exec {engine}< "$2/bin/yard-engine"
runuser -u "$3" -- env -i PATH=/usr/bin:/bin HOME="$7" \
  SUBYARD_REPOSITORY_ROOT="/proc/self/fd/$repository" SUBYARD_CONFIG_HOME="$4" \
  bash -c '[ "$(id -u)" = "$1" ] && [ "$(id -g)" = "$2" ] || exit 89; shift 2; exec -a yard "$@"' \
  fd-reader "$5" "$6" "/proc/self/fd/$engine" --version
'''
try:
    results = []
    for descriptor in (source_fd, empty_fd):
        result = subprocess.run(['sudo', '-n', 'bash', '-c', script, 'fd-readiness',
            anchor+str(descriptor), anchor+str(source_fd), operator.pw_name, config,
            str(operator.pw_uid), str(operator.pw_gid), operator.pw_dir],
            capture_output=True, text=True, timeout=30)
        results.append(result)
    assert results[0].returncode == 0 and results[0].stdout == f'yard {version}\n' and not results[0].stderr
    assert results[1].returncode == 2 and not results[1].stdout and results[1].stderr.startswith('yard: initialize engine:')
    print('release_activation_fd_readiness engine=verified repository=verified invalid_repository=refused operator_identity=verified')
finally:
    os.close(empty_fd)
    os.close(source_fd)
PY
stage authentic-published-predecessor
curl --fail --silent --show-error --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 \
  "https://api.github.com/repos/Subyard/Subyard/releases/tags/v$BASELINE" > "$STATE/published-release.json"
jq -e --arg pin "sha256:$BASELINE_INSTALLER_SHA" --arg tag "v$BASELINE" \
  '.immutable==true and .tag_name==$tag and ([.assets[] | select(.name=="subyard-install.sh") | .digest] == [$pin])' "$STATE/published-release.json" >/dev/null
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 \
  "https://github.com/Subyard/Subyard/releases/download/v$BASELINE/subyard-install.sh" > "$STATE/baseline-install.sh"
[ "$(hash "$STATE/baseline-install.sh")" = "$BASELINE_INSTALLER_SHA" ] || die 'published installer pin differs'
bash "$STATE/baseline-install.sh" --version "$BASELINE" --yes > "$STATE/baseline-install.log" 2>&1
YARD_BIN="$STATE/bin/yard"
[ "$(yard --version)" = "yard $BASELINE" ] || die 'published predecessor not active'
for target in default "$NAMED"; do
  project="$PROJECT"; [ "$target" = default ] || project="$NAMED_PROJECT"
  ! incus project show "$project" >/dev/null 2>&1 || die 'fixture project already exists'
  CREATED_YARDS+=("$target")
  # Claim exact random fixture project before product init; native product retains it.
  incus project create "$project" -c features.images=false -c "user.subyard.activation-fixture=$STATE"
  yard -Y "$target" init --yes > "$STATE/init-$target.log" 2>&1
  instance="$INSTANCE"; [ "$target" = default ] || instance="$NAMED_INSTANCE"
  claim_units
  yard -Y "$target" start --yes > "$STATE/start-$target.log" 2>&1
  [ "$(incus exec "$instance" --project "$project" -- id -u dev)" = "$FIXTURE_DEV_UID" ] || die 'published baseline created an unexpected dev UID'
done
incus config set "$INSTANCE" user.subyard.activation-fixture "$STATE" --project "$PROJECT"
incus config set "$NAMED_INSTANCE" user.subyard.activation-fixture "$STATE" --project "$NAMED_PROJECT"
guest_dev /usr/local/bin/subyard-github status | jq -e '.configured==false' >/dev/null
incus exec "$NAMED_INSTANCE" --project "$NAMED_PROJECT" -- test ! -e /usr/local/libexec/subyard/github-client
stage synthetic-credentials-and-intent
openssl genrsa -out "$STATE/synthetic-app.pem" 2048 >/dev/null 2>&1
chmod 0600 "$STATE/synthetic-app.pem"
printf '{"app_id":"123456","installation_id":42}\n' > "$SUBYARD_CONFIG_HOME/github-app.json"
chmod 0600 "$SUBYARD_CONFIG_HOME/github-app.json"
yard keys import "$STATE/synthetic-app.pem" --label release-activation-fixture --consumer github-app-key --yes > "$STATE/key-import.log" 2>&1
yard keys materialize global --yes > "$STATE/key-materialize.log" 2>&1
yard profile setup github --yes > "$STATE/profile-setup.log" 2>&1
guest_dev /usr/local/bin/subyard-github status | jq -e '.configured==true' >/dev/null
sha256sum "$SUBYARD_KEYS_CONSUMER_ROOT/github/connection.json" > "$STATE/connection.sha"
guest_dev sh -c 'printf "%s\n" retained-user-data > /home/dev/operator-opaque; chmod 0600 /home/dev/operator-opaque'
guest_dev sha256sum /home/dev/operator-opaque > "$STATE/opaque.sha"
cp "$SUBYARD_CONFIG_HOME/yards/default/config.env" "$STATE/selection-live"
systemctl_user disable --now subyard-keys-sync.timer
touch "$STATE/timer-owned"
# Preserve the genuine legacy recipe until ordinary activation establishes ownership.
claim_units
yard -Y "$NAMED" config set ENVIRONMENT_PROFILES github --scope yard --yes > "$STATE/select-stopped.log" 2>&1
yard -Y "$NAMED" stop --yes > "$STATE/stop-named.log" 2>&1
cp "$SUBYARD_CONFIG_HOME/yards/$NAMED/config.env" "$STATE/selection-stopped"
stopped_preview="$(incus config get "$NAMED_INSTANCE" user.subyard.preview_sha256 --project "$NAMED_PROJECT")"
[ ! -e "$UNIT_DIR/$NAMED_UNIT" ] || die 'stopped fixture unexpectedly already has an owner broker'
stage marker-owned-live-preview-drift
baseline_preview="$(guest sha256sum /usr/local/bin/subyard-preview | cut -d' ' -f1)"
[ "$baseline_preview" = "$(incus config get "$INSTANCE" user.subyard.preview_sha256 --project "$PROJECT")" ] || die 'predecessor preview marker does not own installed bytes'
guest sh -eu -c 'test ! -L /usr/local/bin/subyard-preview; test "$(stat -c "%a:%u:%g" /usr/local/bin/subyard-preview)" = 755:0:0; printf "\n# Marker-owned release activation fixture drift.\n" >> /usr/local/bin/subyard-preview'
drifted_preview="$(guest sha256sum /usr/local/bin/subyard-preview | cut -d' ' -f1)"
[ "$baseline_preview" != "$drifted_preview" ] && [ "$drifted_preview" != "$(hash "$FINAL_SOURCE/config/preview/subyard-preview")" ] || die 'preview fixture did not produce meaningful installed drift'
printf 'release_activation_preview_drift baseline=%s installed=%s candidate=%s\n' "$baseline_preview" "$drifted_preview" "$(hash "$FINAL_SOURCE/config/preview/subyard-preview")"
stage publish-exact-candidate
fresh env YARD_RELEASE_BASE_URL="file://$FINAL_RELEASE" bash "$FINAL_RELEASE/subyard-install.sh" \
  --version "$CANDIDATE" --publish-only --yes > "$STATE/publish.log" 2>&1

stage interrupt-after-native-delivery
install -d -m 0700 "$STATE/wrappers"
python3 - "$STATE" "$UNIT" <<'PY'
from pathlib import Path
import shlex,sys
state,unit=sys.argv[1:]
script='#!/bin/sh\nset -eu\nstate='+shlex.quote(state)+'\nunit='+shlex.quote(unit)+'\n'+r'''
if [ "${1:-}" = --user ] && [ "${2:-}" = restart ] && [ "${3:-}" = "$unit" ] &&
   [ ! -e "$state/allow-restart" ] &&
   /usr/bin/jq -e '.checkpoint=="reconciling"' "$state/config/release-transition/v2/journal.json" >/dev/null 2>&1; then
  printf '%s\n' "$$" > "$state/delivery-paused"
  while [ ! -e "$state/allow-restart" ]; do sleep 0.1; done
fi
exec /usr/bin/systemctl "$@"
'''
path=Path(state)/'wrappers/systemctl'; path.write_text(script); path.chmod(0o700)
PY
old_pid="$(systemctl_user show "$UNIT" -p MainPID --value)"
old_hash="$(hash "/proc/$old_pid/exe")"
wanted="$(hash "$FINAL_SOURCE/config/profiles/github/bin/broker")"
[ "$old_hash" != "$wanted" ] || die 'predecessor fixture does not have old loaded broker code'
if [ -f "$FINAL_SOURCE/config/release-owner.json" ]; then
  # release_candidate_prepare has checked this explicit target declaration and
  # its entire manifest. Actual Rust routing is proved by the separate loss case.
  stage rust-normal-upgrade-without-opaque-interruption
  touch "$STATE/allow-restart"
  update "$CANDIDATE" "$FINAL_RELEASE" --yes > "$STATE/recovery.log" 2>&1
else
  interrupt_driver "$STATE" "$YARD_BIN" "$FINAL_RELEASE" "$CANDIDATE" "$UNIT" "$old_pid" "$old_hash" "$wanted"
  touch "$STATE/allow-restart"
  claim_units
  stage historical-go-native-recovery-with-fresh-consent
  PATH="$STATE/wrappers:$PATH" update "$CANDIDATE" "$FINAL_RELEASE" --yes > "$STATE/recovery.log" 2>&1
fi
claim_units
verify_runtime "$FINAL_SOURCE"
ready
[ "$(incus list "$NAMED_INSTANCE" --project "$NAMED_PROJECT" --format csv -c s)" = STOPPED ] || die 'activation started stopped yard'
[ "$(incus config get "$NAMED_INSTANCE" user.subyard.desired_power --project "$NAMED_PROJECT")" = stopped ] || die 'stopped intent changed'
cmp -s "$STATE/selection-stopped" "$SUBYARD_CONFIG_HOME/yards/$NAMED/config.env" || die 'stopped selection changed'
[ "$(incus config get "$NAMED_INSTANCE" user.subyard.preview_sha256 --project "$NAMED_PROJECT")" = "$stopped_preview" ] || die 'stopped guest delivery marker changed'
[ ! -e "$UNIT_DIR/$NAMED_UNIT" ] || die 'activation installed the stopped guest broker'
printf 'release_activation_deferred_yard=%s state=stopped selection=github guest_delivery=unchanged\n' "$NAMED"
ledger="$SUBYARD_CONFIG_HOME/release-transition/v2/ledger.json"
[ -f "$ledger" ] || die 'native migration ledger unavailable'
cp "$ledger" "$STATE/ledger-retained"
stable="$(systemctl_user show "$UNIT" -p MainPID --value):$(stat -c '%i' "$SUBYARD_HOME/github-broker/default-engine"):$(guest stat -c '%i' /usr/local/bin/subyard-preview)"
update "$CANDIDATE" "$FINAL_RELEASE" --yes > "$STATE/noop.log" 2>&1
[ "$stable" = "$(systemctl_user show "$UNIT" -p MainPID --value):$(stat -c '%i' "$SUBYARD_HOME/github-broker/default-engine"):$(guest stat -c '%i' /usr/local/bin/subyard-preview)" ] || die 'no-op activation changed PID/inodes'
cmp -s "$STATE/ledger-retained" "$ledger" || die 'runtime retry changed completed migrations'

stage broker-first-owned-wiring-lifecycle
set_git_mode broker-first "$STATE/git-mode"
yard profile enable github --yes > "$STATE/git-enable.log" 2>&1
verify_git_wiring
# Configuration delivery may replace .gitconfig; the independent XDG include survives.
guest_dev sh -c 'printf "[user]\n\tname = Delivered fixture\n" > /home/dev/.gitconfig'
verify_git_wiring
wiring_inode="$(guest stat -c '%i' /home/dev/.config/subyard/github-git.conf)"
yard profile enable github --yes > "$STATE/git-repeat.log" 2>&1
[ "$wiring_inode" = "$(guest stat -c '%i' /home/dev/.config/subyard/github-git.conf)" ] || die 'repeated Git enable rewrote current fragment'
guest_dev sh -c 'printf "# subyard-managed-github-git-v1\n# fixture drift\n" > /home/dev/.config/subyard/github-git.conf'
yard profile status github --json | jq -e '.runtime=="pending"' >/dev/null
yard migrate --yes > "$STATE/git-repair.log" 2>&1
verify_git_wiring
set_git_mode off "$STATE/git-off"
guest test ! -e /home/dev/.config/subyard/github-git.conf
guest_dev git config --global --get user.name | grep -Fxq 'Delivered fixture'
set_git_mode broker-first "$STATE/git-on"
verify_git_wiring
yard profile disable github --yes > "$STATE/git-disable.log" 2>&1
guest test ! -e /home/dev/.config/subyard/github-git.conf
guest test ! -e /usr/local/bin/git-remote-subyard-github
guest_dev git config --global --get user.name | grep -Fxq 'Delivered fixture'
yard profile enable github --yes > "$STATE/git-reenable.log" 2>&1
claim_units
verify_git_wiring
cp "$SUBYARD_CONFIG_HOME/yards/default/config.env" "$STATE/selection-live"

stage authentic-predecessor-rollback-refusal
links="$(readlink "$STATE/runtime/current")|$(readlink "$STATE/runtime/previous")"
if yard update --rollback --yes > "$STATE/predecessor-rollback.log" 2>&1; then die "incompatible $BASELINE predecessor rollback succeeded"; fi
grep -Eq 'rollback-incompatible|cannot verify selected installed consumers|compatible installed-consumer' "$STATE/predecessor-rollback.log" || die 'rollback failed for an unexpected reason'
[ "$links" = "$(readlink "$STATE/runtime/current")|$(readlink "$STATE/runtime/previous")" ] || die 'refused rollback changed runtime links'
cmp -s "$STATE/ledger-retained" "$ledger" || die 'refused rollback changed migration ledger'

stage explicit-stopped-yard-start-and-migrate
yard -Y "$NAMED" start --yes > "$STATE/start-deferred.log" 2>&1
trace_deferred_migration
claim_units
incus exec "$NAMED_INSTANCE" --project "$NAMED_PROJECT" -- runuser -u dev -- \
  /usr/local/bin/subyard-github status --runtime | jq -e --arg wanted "$wanted" '.broker_sha256==$wanted and .client_sha256==$wanted' >/dev/null
ready

stage owned-timer-drift-current-repair
SYNTHETIC_RELEASE="$ROOT/.subyard-acceptance/fixture-b"
[ -f "$SYNTHETIC_RELEASE/subyard-install.sh" ] || die 'controller-packaged synthetic B unavailable'
# Repair owned drift in the current release before requesting a different target.
[ -f "$UNIT_DIR/.subyard-keys-sync.json" ] || die 'ordinary update did not establish timer ownership'
printf '\n# Fixture template drift.\n' >> "$UNIT_DIR/subyard-keys-sync.service"
claim_units
systemctl_user daemon-reload
update "$CANDIDATE" "$FINAL_RELEASE" --yes > "$STATE/timer-repair.log" 2>&1
claim_units
verify_runtime "$FINAL_SOURCE"
ready
cmp -s "$STATE/ledger-retained" "$ledger" || die 'timer runtime repair replayed migrations'

stage publish-exact-synthetic-candidate
links="$(readlink "$STATE/runtime/current")|$(readlink "$STATE/runtime/previous")"
fresh env YARD_RELEASE_BASE_URL="file://$SYNTHETIC_RELEASE" bash "$SYNTHETIC_RELEASE/subyard-install.sh" \
  --version "$SYNTHETIC" --publish-only --yes > "$STATE/publish-b.log" 2>&1
[ "$links" = "$(readlink "$STATE/runtime/current")|$(readlink "$STATE/runtime/previous")" ] || die 'synthetic publication changed runtime links'
cmp -s "$STATE/ledger-retained" "$ledger" || die 'synthetic publication changed migration ledger'

stage synthetic-same-source-supported-rollback
update "$SYNTHETIC" "$SYNTHETIC_RELEASE" --yes > "$STATE/update-b.log" 2>&1
claim_units
source_b="$(readlink -f "$STATE/runtime/current")"
verify_runtime "$source_b"
[ "$(hash "$source_b/config/preview/subyard-preview")" != "$(hash "$FINAL_SOURCE/config/preview/subyard-preview")" ] || die 'synthetic preview delta absent'
ready
yard update --rollback --yes > "$STATE/rollback-a.log" 2>&1
claim_units
[ "$(yard --version)" = "yard $CANDIDATE" ] || die 'supported rollback did not restore candidate A'
verify_runtime "$FINAL_SOURCE"
ready
cmp -s "$STATE/ledger-retained" "$ledger" || die 'supported runtime rollback replayed migrations'
stage absent-installed-timer-intent
grep -Fq "$STATE/runtime/current/bin/yard" "$UNIT_DIR/subyard-keys-sync.service" || die 'credential timer ownership differs'
[ -f "$STATE/timer-owned" ] || die 'credential timer fixture ownership missing'
rm -- "$UNIT_DIR/subyard-keys-sync.service" "$UNIT_DIR/subyard-keys-sync.timer"
systemctl_user daemon-reload
update "$CANDIDATE" "$FINAL_RELEASE" --yes > "$STATE/absent-timer.log" 2>&1
[ ! -e "$UNIT_DIR/subyard-keys-sync.service" ] && [ ! -L "$UNIT_DIR/subyard-keys-sync.service" ] \
  && [ ! -e "$UNIT_DIR/subyard-keys-sync.timer" ] && [ ! -L "$UNIT_DIR/subyard-keys-sync.timer" ] \
  || die 'release activation recreated absent timer units'
ready
printf 'ok: authentic %s upgrade/recovery/refusal and synthetic B-to-A rollback; broker loaded identities, core helpers, deferred yard, credentials and disabled timer verified\n' "$BASELINE"
if [ -n "$NATIVE_LOSS" ]; then
  stage "separate-final-rust-native-loss-$NATIVE_LOSS"
  loss_case="$STATE/native-loss"
  install -d -m 0700 "$loss_case" "$loss_case/bin"
  printf 'subyard-profile-native-loss-v1\n' > "$loss_case/.marker"
  python3 - "$loss_case" "$UNIT" <<'PY'
from pathlib import Path
import shlex,sys
case,unit=sys.argv[1:]
script='#!/bin/bash\nset -euo pipefail\ncase='+shlex.quote(case)+'\nunit='+shlex.quote(unit)+'\n'+r'''
if [ "${1:-}" = --user ] && [ "${2:-}" = restart ] && [ "${3:-}" = "$unit" ]; then
  printf 'restart\n' >> "$case/calls"
  printf '%s\n' "$$" > "$case/paused"
  while [ ! -e "$case/release" ]; do sleep .05; done
fi
exec /usr/bin/systemctl "$@"
'''
path=Path(case)/'bin/systemctl';path.write_text(script);path.chmod(0o700)
PY
  # Owned delivered bytes drift while the loaded broker remains usable. The
  # ordinary runtime handler must restore them before its actual restart call.
  printf '\nfixture-owned loss drift\n' >> "$SUBYARD_HOME/github-broker/default-engine"
  loss_observe() {
    fresh /usr/bin/python3 -B "$ROOT/tests/helpers/profile-native-loss.py" \
      --runtime "$(readlink -f "$STATE/runtime/current")" \
      --manifest-sha256 "$(hash "$FINAL_SOURCE/runtime-files.sha256")" \
      --case-root "$loss_case" --mode "$NATIVE_LOSS" --successor "$SYNTHETIC_RELEASE" \
      --current-version "$CANDIDATE" --activation-id github-runtime "$@"
  }
  PATH="$loss_case/bin:$PATH" loss_observe > "$STATE/native-loss.log" 2>&1
  claim_units
  loss_resources() {
    hash "$SUBYARD_HOME/github-broker/default-engine"
    hash "$UNIT_DIR/$UNIT"
    systemctl_user show "$UNIT" --property=MainPID,ActiveState
    readlink "$STATE/runtime/current"
    readlink "$STATE/runtime/previous"
    guest sha256sum /usr/local/libexec/subyard/github-client
  }
  loss_resources > "$loss_case/resources.before"
  if [ "$NATIVE_LOSS" = owner ]; then
    cp "$loss_case/calls" "$loss_case/calls.drained"
    PATH="$loss_case/bin:$PATH" yard migrate --yes > "$STATE/native-loss-recover.log" 2>&1
    cmp "$loss_case/calls.drained" "$loss_case/calls" || die 'owner recovery replayed the drained GitHub mutation'
    loss_resources > "$loss_case/resources.after"
    cmp "$loss_case/resources.before" "$loss_case/resources.after" || die 'owner recovery changed the drained profile resources'
    ready
    verify_runtime "$FINAL_SOURCE"
  else
    loss_observe --verify-unknown >> "$STATE/native-loss.log" 2>&1
    if PATH="$loss_case/bin:$PATH" yard migrate --yes > "$STATE/native-loss-same-goal.log" 2>&1; then
      die 'opaque native loss replayed the original goal'
    fi
    loss_observe --verify-unchanged
    loss_resources > "$loss_case/resources.after"
    cmp "$loss_case/resources.before" "$loss_case/resources.after" || die 'same-goal refusal changed GitHub resources'
    if PATH="$loss_case/bin:$PATH" update "$SYNTHETIC" "$SYNTHETIC_RELEASE" --yes > "$STATE/native-loss-new-goal.log" 2>&1; then
      die 'opaque native loss admitted a replacement goal'
    fi
    loss_observe --verify-unchanged
    loss_resources > "$loss_case/resources.after"
    cmp "$loss_case/resources.before" "$loss_case/resources.after" || die 'replacement refusal changed GitHub resources'
    loss_observe --verify-unknown >> "$STATE/native-loss.log" 2>&1
  fi
  printf 'ok: separate Rust GitHub %s loss kept native exit, original custody, no replay and exact final outcome\n' "$NATIVE_LOSS"
fi

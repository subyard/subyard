#!/usr/bin/env bash
# Real outer+nested teardown boundary acceptance. Run only on a disposable leased VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
YARD_BIN="$ROOT/.build/yard"
# shellcheck source=scripts/lib-power.sh
. "$ROOT/scripts/lib-power.sh"
STATE=''
OUTER_YARD=''
OUTER_PROJECT=''
OUTER_INSTANCE=''
OUTER_POOL=''
OUTER_BRIDGE=''
DEFAULT_POOL_BEFORE=''
HOST_DEFAULT_ROUTE_BEFORE=''
HOST_GLOBAL_IPV4_BEFORE=''
BASELINE_BRIDGE=incusbr0
FIXTURE_STARTED="$(date -u '+%Y-%m-%d %H:%M:%S UTC')"

die() { printf 'nested-teardown-boundary: %s\n' "$*" >&2; exit 2; }

[ -n "${SUBYARD_E2E_VM:-}" ] || die 'run through dev/agent-e2e.sh'
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] \
  || die 'freeze and pass the current packaged candidate before allocation'
for command in go incus jq sudo timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
COMMAND_TIMEOUT="${NESTED_TEARDOWN_COMMAND_TIMEOUT:-3600}"
COMMAND_KILL_AFTER_SECONDS="${NESTED_TEARDOWN_COMMAND_KILL_AFTER_SECONDS:-10}"
[[ "$COMMAND_TIMEOUT" =~ ^[1-9][0-9]{0,3}$ ]] && [ "$COMMAND_TIMEOUT" -le 3600 ] \
  || die 'nested command timeout must be an integer from 1 through 3600 seconds'
[[ "$COMMAND_KILL_AFTER_SECONDS" =~ ^[1-9][0-9]?$ ]] \
  && [ "$COMMAND_KILL_AFTER_SECONDS" -le 60 ] \
  || die 'nested command kill grace must be an integer from 1 through 60 seconds'
if ! command -v qemu-system-x86_64 >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || die 'qemu-system-x86_64 or apt-get is required'
  printf '  [ .. ] installing QEMU for the outer VM fixture\n'
  sudo -n apt-get update >/dev/null
  sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-system-x86 >/dev/null
fi

# The synced development candidate includes every manifest-declared native profile.
make -C "$ROOT" build

bounded_command() {
  local label="$1" rc
  shift
  if timeout --signal=TERM --kill-after="$COMMAND_KILL_AFTER_SECONDS" \
    "$COMMAND_TIMEOUT" "$@"; then
    return 0
  else
    rc=$?
  fi
  case "$rc" in
    124|137)
      printf 'nested-teardown-boundary: %s exceeded the %ss command deadline\n' \
        "$label" "$COMMAND_TIMEOUT" >&2
      ;;
  esac
  return "$rc"
}

yard() {
  bounded_command yard "$YARD_BIN" -Y "$OUTER_YARD" "$@"
}

setting() {
  local key="$1" value
  value="$(yard config show "$key" | sed -n 's/^effective: //p')"
  [ -n "$value" ] || die "could not resolve $key"
  printf '%s\n' "$value"
}

default_pool_snapshot() {
  local state rc inventory
  set +e
  state="$(incus storage show default --project default 2>/dev/null)"
  rc=$?
  set -e
  if [ "$rc" = 0 ]; then
    printf 'present\n%s\n' "$state"
    return
  fi
  inventory="$(incus storage list --project default --format csv -c n)" \
    || die 'could not inventory the default Incus pool'
  ! grep -Fxq default <<<"$inventory" \
    || die 'default Incus pool inventory and state query disagree'
  printf 'absent\n'
}

assert_default_pool_unchanged() {
  local after before_hash after_hash
  after="$(default_pool_snapshot)"
  [ "$after" = "$DEFAULT_POOL_BEFORE" ] && return
  before_hash="$(printf '%s' "$DEFAULT_POOL_BEFORE" | sha256sum | awk '{print $1}')"
  after_hash="$(printf '%s' "$after" | sha256sum | awk '{print $1}')"
  die "candidate lifecycle changed the default Incus pool: before=$before_hash after=$after_hash"
}

assert_host_network_unchanged() {
  local routes addresses
  routes="$(ip -4 route show default | sort)"
  addresses="$(ip -4 -o address show scope global | awk '{print $2, $4}' | sort)"
  [ "$routes" = "$HOST_DEFAULT_ROUTE_BEFORE" ] \
    || die 'candidate lifecycle changed the allocated host default route'
  [ "$addresses" = "$HOST_GLOBAL_IPV4_BEFORE" ] \
    || die 'candidate lifecycle changed the allocated host global IPv4 addresses'
  incus network show "$BASELINE_BRIDGE" --project default >/dev/null 2>&1 \
    || die "candidate teardown removed surviving bridge $BASELINE_BRIDGE"
  power_nm_prepare_reader \
    || die "cannot prepare NetworkManager safety check: $POWER_ERROR"
  power_host_safe "$BASELINE_BRIDGE" \
    || die "allocated host is unsafe after candidate teardown: $POWER_ERROR"
}

outer_data_home_metadata() {
  [ -d "$SUBYARD_HOME" ] && [ ! -L "$SUBYARD_HOME" ] \
    || die "outer data home is not a real directory: $SUBYARD_HOME"
  stat -c '%u:%g:%a' -- "$SUBYARD_HOME"
}

assert_outer_data_home_metadata() {
  local stage="$1"
  [ "$(outer_data_home_metadata)" = "$OUTER_DATA_HOME_METADATA" ] \
    || die "outer data home metadata changed after $stage"
}

nested_decimal_at_most() {
  local value="$1" limit="$2"
  [ "${#value}" -lt "${#limit}" ] \
    || { [ "${#value}" -eq "${#limit}" ] \
      && [[ "$value" == "$limit" || "$value" < "$limit" ]]; }
}

nested_monotonic_seconds() {
  local uptime
  read -r uptime _ < /proc/uptime
  [[ "${uptime%%.*}" =~ ^[0-9]+$ ]] || die 'could not inspect monotonic uptime'
  printf '%s\n' "${uptime%%.*}"
}

nested_memory_available_bytes() {
  awk '/MemAvailable:/ { printf "%.0f\n", $2 * 1024; exit }' /proc/meminfo
}

capture_outer_docker_failure() {
  local errors="$1" snapshot owner rc deadline now remaining export_rc=0
  now="$(nested_monotonic_seconds)" || return
  deadline=$((now + 45))
  python3 - "$errors" <<'PY' || export_rc=$?
import base64, decimal, hashlib, json, os, re, stat, sys

def emit(probe, **fields):
    print('NESTED_DOCKER_DIAGNOSTIC ' + json.dumps(dict(probe=probe, **fields)), flush=True)

def artifact(name, data, size):
    data = data[:4096]
    encoded = base64.b64encode(data).decode('ascii')
    print('NESTED_DOCKER_ARTIFACT ' + json.dumps(dict(name=name, source_bytes=size,
        retained_bytes=len(data), truncated=len(data) < size, sha256=hashlib.sha256(data).hexdigest(),
        chunks=(len(encoded) + 2047) // 2048)), flush=True)
    for index, offset in enumerate(range(0, len(encoded), 2048)):
        print('NESTED_DOCKER_DATA ' + json.dumps(dict(name=name, index=index,
            base64=encoded[offset:offset + 2048])), flush=True)

with open(sys.argv[1], 'rb') as stream:
    info = os.fstat(stream.fileno())
    assert stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600
    assert info.st_uid == os.getuid() and info.st_nlink == 1
    data = stream.read(8192)
    tail_offset = max(0, info.st_size - 8192)
    stream.seek(tail_offset)
    tail = stream.read(8192)
    artifact('readiness-exec-stderr', data, info.st_size)
    begin = b'NESTED_DOCKER_PROBE_BEGIN\n'
    matches = list(re.finditer(rb'\nNESTED_DOCKER_TIMING start=([0-9]+\.[0-9]+) end=([0-9]+\.[0-9]+) native_exit=([0-9]+)\n', tail))
    if data.count(begin) == 1 and len(matches) == 1:
        match = matches[0]
        start = data.index(begin) + len(begin)
        size = tail_offset + match.start() - start
        assert size >= 0
        stream.seek(start)
        error = stream.read(min(size, 4096))
        elapsed = int((decimal.Decimal(match[2].decode()) - decimal.Decimal(match[1].decode())) * 1000)
        emit('container-enumeration', native_exit=int(match[3]), elapsed_ms=elapsed,
             clock='realtime', budget_ms=10000, scope='single-native-probe', timing_available=True)
        artifact('enumeration-stderr', error, size)
    else:
        emit('container-enumeration', timing_available=False, scope='single-native-probe')
PY
  printf 'NESTED_DOCKER_DIAGNOSTIC {"probe":"stderr-export","scope":"failure-only","native_exit":%s}\n' "$export_rc"
  # A failure-only followup has its own budget and cannot authorize an unsafe target.
  now="$(nested_monotonic_seconds)" || return
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] || return 124
  [ "$remaining" -le 3 ] || remaining=3
  snapshot="$(timeout --signal=TERM --kill-after=1 "$remaining" incus query \
    "/1.0/instances/$OUTER_INSTANCE?project=$OUTER_PROJECT" 2>/dev/null)" || return
  jq -e --arg instance "$OUTER_INSTANCE" --arg pool "$OUTER_POOL" '
    .name == $instance and .type == "virtual-machine" and .status == "Running"
    and .expanded_config["user.subyard.managed"] == "true"
    and .expanded_devices.root.pool == $pool
  ' >/dev/null 2>&1 <<<"$snapshot" || return 2
  now="$(nested_monotonic_seconds)" || return
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] || return 124
  [ "$remaining" -le 3 ] || remaining=3
  owner="$(timeout --signal=TERM --kill-after=1 "$remaining" incus storage get "$OUTER_POOL" \
    user.subyard.owner --project default 2>/dev/null)" || return
  [ "$owner" = nested-teardown-e2e-v1 ] || return 2
  now="$(nested_monotonic_seconds)" || return
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] || return 124
  if timeout --signal=TERM --kill-after=1 "$remaining" incus exec "$OUTER_INSTANCE" \
    --project "$OUTER_PROJECT" -- python3 - 2>/dev/null <<'PY'
import base64, hashlib, json, os, re, subprocess, time

def emit(probe, **fields):
    print('NESTED_DOCKER_DIAGNOSTIC ' + json.dumps(dict(probe=probe, scope='failure-only', **fields)), flush=True)

def artifact(name, data, limit):
    retained = data[:limit]
    encoded = base64.b64encode(retained).decode('ascii')
    print('NESTED_DOCKER_ARTIFACT ' + json.dumps(dict(name=name, source_bytes=len(data),
        retained_bytes=len(retained), truncated=len(retained) < len(data),
        sha256=hashlib.sha256(retained).hexdigest(), chunks=(len(encoded) + 2047) // 2048)), flush=True)
    for index, offset in enumerate(range(0, len(encoded), 2048)):
        print('NESTED_DOCKER_DATA ' + json.dumps(dict(name=name, index=index,
            base64=encoded[offset:offset + 2048])), flush=True)

def run(probe, arguments, budget):
    started = time.monotonic()
    try:
        result = subprocess.run(['timeout', '--signal=TERM', '--kill-after=1', str(budget), *arguments],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except OSError:
        result = subprocess.CompletedProcess(arguments, None, stdout=b'', stderr=b'')
    emit(probe, native_exit=result.returncode, elapsed_ms=int((time.monotonic() - started) * 1000),
         budget_ms=budget * 1000)
    if result.returncode != 0:
        artifact(probe + '-stderr', result.stderr, 4096)
    return result

# Passive diagnostics cannot socket-activate a daemon that has stopped since the probe.
# Context metadata is local; never add another server API request here.
context = run('context', ['docker', 'context', 'show'], 3)
value = (context.stdout or b'').strip()
emit('context-class', value=('default' if value == b'default' else 'custom') if context.returncode == 0 else 'unavailable',
     docker_host_set=bool(os.environ.get('DOCKER_HOST')), docker_context_set=bool(os.environ.get('DOCKER_CONTEXT')))
endpoint = run('endpoint', ['docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'], 3)
value = (endpoint.stdout or b'').strip()
emit('context-endpoint-class', value=('unix' if value.startswith(b'unix://') else 'tcp' if value.startswith(b'tcp://') else 'other')
     if endpoint.returncode == 0 else 'unavailable')
enums = {
    'Type': {'simple', 'exec', 'forking', 'oneshot', 'dbus', 'notify', 'notify-reload', 'idle'},
    'LoadState': {'loaded', 'not-found', 'error', 'masked', 'bad-setting', 'merged', 'stub'},
    'ActiveState': {'active', 'reloading', 'inactive', 'failed', 'activating', 'deactivating', 'maintenance', 'refreshing'},
    'SubState': {'dead', 'start-pre', 'start', 'start-post', 'running', 'listening', 'exited', 'reload', 'stop',
                 'stop-sigterm', 'stop-sigkill', 'stop-post', 'failed', 'auto-restart', 'auto-restart-queued', 'cleaning'},
    'Result': {'success', 'resources', 'timeout', 'exit-code', 'signal', 'core-dump', 'watchdog',
               'start-limit-hit', 'oom-kill', 'protocol', 'exec-condition', 'assert'},
}  # Native property allowlist.
numbers = {'MainPID', 'ControlPID', 'ExecMainStatus', 'NRestarts', 'ExecMainStartTimestampMonotonic',
           'InactiveExitTimestampMonotonic', 'ActiveEnterTimestampMonotonic'}
properties = ','.join([*enums, *sorted(numbers)])
for unit in ('docker.service', 'containerd.service', 'docker.socket'):
    result = run(unit, ['systemctl', 'show', unit, '--property=' + properties], 3)
    state = {}
    if result.returncode == 0 and len(result.stdout) <= 8192:
        for line in result.stdout.decode('utf-8', errors='replace').splitlines():
            key, separator, value = line.partition('=')
            if separator and key in enums:
                state[key] = value if value in enums[key] else 'unknown'
            elif separator and key in numbers:
                state[key] = value if value.isascii() and value.isdigit() and len(value) <= 20 else 'unknown'
    emit('unit-state', unit=unit, state=state, projection_complete=len(state) == len(enums) + len(numbers))
journal = run('journal', ['journalctl', '-u', 'docker.service', '-u', 'containerd.service',
    '--no-pager', '-n', '80', '--output=short-iso'], 5)
artifact('unit-journal', journal.stdout or b'', 32768)
PY
  then rc=0; else rc=$?; fi
  printf 'NESTED_DOCKER_DIAGNOSTIC {"probe":"followup","scope":"failure-only","native_exit":%s,"budget_ms":45000}\n' "$rc"
  [ "$export_rc" = 0 ] || return "$export_rc"
  return "$rc"
}

require_outer_docker_ready() {
  local deadline remaining snapshot owner rc now observations errors capture_rc
  docker_ready_report() {
    printf '{"phase":"%s","reason":"%s","native_exit":%d}\n' "$1" "$2" "$3" >&2
  }
  [[ "$OUTER_PROJECT" =~ ^subyard-nested-e2e-[a-z0-9]{6}$ ]] \
    && [ "$OUTER_POOL" = "${OUTER_PROJECT#subyard-}" ] \
    && [ "$OUTER_INSTANCE" = "yard-$OUTER_POOL" ] \
    || { docker_ready_report ownership unsafe-target 0; die 'Docker prerequisite target identity is unsafe'; }
  now="$(nested_monotonic_seconds)" \
    || { docker_ready_report clock unavailable "$?"; die 'Docker prerequisite clock is unavailable'; }
  deadline=$((now + 60))
  if snapshot="$(timeout --signal=TERM --kill-after=1 3 incus query \
    "/1.0/instances/$OUTER_INSTANCE?project=$OUTER_PROJECT" 2>/dev/null)"; then
    jq -e --arg instance "$OUTER_INSTANCE" --arg pool "$OUTER_POOL" '
      .name == $instance and .type == "virtual-machine" and .status == "Running"
      and .expanded_config["user.subyard.managed"] == "true"
      and .expanded_devices.root.pool == $pool
    ' >/dev/null 2>&1 <<<"$snapshot" \
      || { docker_ready_report ownership rejected-vm 0; die 'Docker prerequisite VM ownership is unavailable'; }
  else
    rc=$?; docker_ready_report ownership vm-query-failed "$rc"
    die 'Docker prerequisite VM ownership is unavailable'
  fi
  now="$(nested_monotonic_seconds)" \
    || { docker_ready_report clock unavailable "$?"; die 'Docker prerequisite clock is unavailable'; }
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] \
    || { docker_ready_report clock deadline 0; die 'Docker prerequisite exceeded its 60s deadline'; }
  [ "$remaining" -le 3 ] || remaining=3
  if owner="$(timeout --signal=TERM --kill-after=1 "$remaining" incus storage get "$OUTER_POOL" \
    user.subyard.owner --project default 2>/dev/null)"; then
    [ "$owner" = nested-teardown-e2e-v1 ] \
      || { docker_ready_report ownership rejected-pool 0; die 'Docker prerequisite pool ownership is unavailable'; }
  else
    rc=$?; docker_ready_report ownership pool-query-failed "$rc"
    die 'Docker prerequisite pool ownership is unavailable'
  fi
  now="$(nested_monotonic_seconds)" \
    || { docker_ready_report clock unavailable "$?"; die 'Docker prerequisite clock is unavailable'; }
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] \
    || { docker_ready_report clock deadline 0; die 'Docker prerequisite exceeded its 60s deadline'; }
  # start guarantees the Incus agent, not boot completion of Docker. Observe only;
  # never start/restart the daemon or retry init to satisfy this fixture prerequisite.
  errors="$(mktemp "$STATE/docker-ready.stderr.XXXXXX")"
  chmod 0600 "$errors"
  if observations="$(timeout --signal=TERM --kill-after=1 "$remaining" \
    incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
    timeout --signal=TERM --kill-after=1 "$remaining" bash -eu -c '
      report() {
        printf "{\"phase\":\"%s\",\"reason\":\"%s\",\"native_exit\":%d,\"load\":\"%s\",\"active\":\"%s\",\"sub\":\"%s\"}\n" \
          "$1" "$2" "$3" "$load" "$active" "$sub"
      }
      observed=0
      while :; do
        load=unknown active=unknown sub=unknown
        if state=$(timeout 3 systemctl show docker.service \
          --property=LoadState --property=ActiveState --property=SubState); then :; else
          rc=$?
          reason=unavailable; [ "$rc" != 124 ] || reason=timeout
          report service-query "$reason" "$rc"; exit 2
        fi
        seen_load=0 seen_active=0 seen_sub=0 malformed=0
        [ "$(
          LC_ALL=C
          printf "%s" "${#state}"
        )" -le 8192 ] || { report service-state malformed 0; exit 2; }
        while IFS="=" read -r field value; do
          case "$field" in
            LoadState)
              [ "$seen_load" = 0 ] || malformed=1; seen_load=1
              case "$value" in loaded|not-found|error|masked|bad-setting) load=$value ;; *) malformed=1 ;; esac ;;
            ActiveState)
              [ "$seen_active" = 0 ] || malformed=1; seen_active=1
              case "$value" in active|activating|inactive|failed|deactivating|reloading) active=$value ;; *) malformed=1 ;; esac ;;
            SubState)
              [ "$seen_sub" = 0 ] || malformed=1; seen_sub=1
              case "$value" in running|start|start-pre|start-post|dead|failed|exited|stop|stop-sigterm|stop-sigkill|auto-restart) sub=$value ;; *) malformed=1 ;; esac ;;
            *) malformed=1 ;;
          esac
        done <<<"$state"
        if [ "$malformed:$seen_load:$seen_active:$seen_sub" != 0:1:1:1 ]; then
          report service-state malformed 0; exit 2
        fi
        case "$load:$active:$sub" in
          loaded:active:running)
            started=$EPOCHREALTIME
            printf "NESTED_DOCKER_PROBE_BEGIN\n" >&2
            if timeout 10 docker container ls -a --format "{{.Names}}" >/dev/null; then rc=0; else rc=$?; fi
            ended=$EPOCHREALTIME
            printf "\nNESTED_DOCKER_TIMING start=%s end=%s native_exit=%s\n" "$started" "$ended" "$rc" >&2
            report enumeration completed "$rc"; exit "$rc"
            ;;
          loaded:activating:start|loaded:activating:start-pre|loaded:activating:start-post)
            if [ "$observed" = 0 ]; then report service-state waiting 0; observed=1; fi
            sleep 1
            ;;
          *) report service-state rejected 0; exit 2 ;;
        esac
      done
    ' 2>"$errors")"; then rc=0; else rc=$?; fi
  # Only this closed schema reaches the operation boundary; native output stays private.
  if [ "$(
    LC_ALL=C
    printf '%s' "${#observations}"
  )" -le 2048 ] && observations="$(jq -c -s --argjson rc "$rc" '
    if (length <= 2 and (length > 0 or $rc != 0)
      and ($rc != 0 or (.[-1].phase == "enumeration" and .[-1].native_exit == 0)) and all(.[];
      type == "object" and keys == ["active","load","native_exit","phase","reason","sub"]
      and (.native_exit | type == "number" and . >= 0 and . <= 255 and floor == .)
      and (.load | IN("unknown","loaded","not-found","error","masked","bad-setting"))
      and (.active | IN("unknown","active","activating","inactive","failed","deactivating","reloading"))
      and (.sub | IN("unknown","running","start","start-pre","start-post","dead","failed","exited","stop","stop-sigterm","stop-sigkill","auto-restart"))
      and ((.phase == "service-query" and (.reason | IN("timeout","unavailable")))
        or (.phase == "service-state" and (.reason | IN("malformed","waiting","rejected")))
        or (.phase == "enumeration" and .reason == "completed"))))
    then .[] | {phase,reason,native_exit,load,active,sub}
    else error("invalid Docker prerequisite diagnostics") end
  ' 2>/dev/null <<<"$observations")"; then
    [ -z "$observations" ] || printf '%s\n' "$observations" >&2
    if [ "$rc" = 124 ]; then docker_ready_report boundary timeout "$rc";
    elif [ "$rc" = 0 ]; then docker_ready_report boundary completed "$rc";
    else docker_ready_report boundary native-failure "$rc"; fi
  else
    docker_ready_report boundary malformed-diagnostics "$rc"
    [ "$rc" != 0 ] || rc=2
  fi
  [ "$rc" != 0 ] || return 0
  if capture_outer_docker_failure "$errors" >&2; then capture_rc=0; else capture_rc=$?; fi
  printf 'NESTED_DOCKER_DIAGNOSTIC {"probe":"capture","scope":"failure-only","native_exit":%s}\n' "$capture_rc" >&2
  printf 'nested-teardown-boundary: Docker prerequisite failed within its 60s budget\n' >&2
  return "$rc"
}

require_nested_memory_reserve() {
  local vm_budget="${NESTED_TEARDOWN_VM_MEMORY_BYTES:-2147483648}"
  local host_reserve="${NESTED_TEARDOWN_POST_LAUNCH_RESERVE_BYTES:-1073741824}"
  local wait_seconds="${NESTED_TEARDOWN_MEMORY_WAIT_SECONDS:-300}"
  local poll_seconds="${NESTED_TEARDOWN_MEMORY_POLL_SECONDS:-5}"
  local signed_max=9223372036854775807 minimum available deadline now remaining delay warned=0
  [[ "$vm_budget" =~ ^[1-9][0-9]*$ ]] \
    || die 'nested VM memory budget must be a positive integer'
  [[ "$host_reserve" =~ ^[1-9][0-9]*$ ]] \
    || die 'post-launch host memory reserve must be a positive integer'
  [[ "$wait_seconds" =~ ^[0-9]+$ ]] && [ "$wait_seconds" -le 900 ] \
    || die 'nested memory wait must be an integer from 0 through 900 seconds'
  [[ "$poll_seconds" =~ ^[1-9][0-9]*$ ]] && [ "$poll_seconds" -le 30 ] \
    || die 'nested memory poll must be an integer from 1 through 30 seconds'
  nested_decimal_at_most "$vm_budget" "$signed_max" \
    && nested_decimal_at_most "$host_reserve" "$((signed_max - vm_budget))" \
    || die 'nested memory requirement exceeds the supported range'
  minimum=$((vm_budget + host_reserve))
  now="$(nested_monotonic_seconds)"
  deadline=$((now + wait_seconds))
  while :; do
    available="$(nested_memory_available_bytes)"
    [[ "$available" =~ ^[0-9]+$ ]] || die 'could not inspect available host memory'
    [ "$available" -lt "$minimum" ] || return 0
    now="$(nested_monotonic_seconds)"
    [ "$now" -lt "$deadline" ] \
      || die "nested fixture needs $vm_budget bytes for its VM plus $host_reserve bytes host reserve; have $available after waiting ${wait_seconds}s"
    if [ "$warned" = 0 ]; then
      printf '  [ .. ] waiting up to %ss for nested fixture memory reserve (have %s, need %s)\n' \
        "$wait_seconds" "$available" "$minimum"
      warned=1
    fi
    remaining=$((deadline - now))
    delay="$poll_seconds"
    [ "$delay" -le "$remaining" ] || delay="$remaining"
    sleep "$delay"
  done
}

assert_outer_vm_ssh_address() {
  local stage="$1" primary_interface eth0 foreign pinned relay_unit relay_service listeners
  primary_interface="$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
    ip -4 route show default \
    | awk 'NR == 1 { for (i=1; i<=NF; i++) if ($i == "dev") { print $(i+1); exit } }')"
  eth0="$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
    ip -4 -o address show dev "$primary_interface" scope global \
    | awk 'NR == 1 { split($4, address, "/"); print address[1] }')"
  foreign="$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
    ip -4 -o address show dev aaa0 scope global \
    | awk 'NR == 1 { split($4, address, "/"); print address[1] }')"
  pinned="$(incus config device get "$OUTER_INSTANCE" eth0 ipv4.address \
    --project "$OUTER_PROJECT")"
  [ -n "$eth0" ] || die "$stage: outer VM eth0 has no global IPv4"
  [ -n "$foreign" ] || die "$stage: foreign IPv4 fixture is missing"
  [ "$eth0" != "$foreign" ] || die "$stage: primary and foreign interfaces share an IPv4"
  [ "$pinned" = "$eth0" ] || die "$stage: eth0 pinned $pinned instead of $eth0"
  ! incus config device list "$OUTER_INSTANCE" --project "$OUTER_PROJECT" \
    | grep -qx ssh || die "$stage: unsupported Incus VM proxy device is still attached"
  relay_unit="subyard-ssh-relay-$ssh_port.socket"
  relay_service="/etc/systemd/system/subyard-ssh-relay-$ssh_port.service"
  systemctl is-active --quiet "$relay_unit" \
    || die "$stage: VM SSH loopback relay is not active"
  grep -qxF "ExecStart=/usr/lib/systemd/systemd-socket-proxyd $eth0:22" "$relay_service" \
    || grep -qxF "ExecStart=/lib/systemd/systemd-socket-proxyd $eth0:22" "$relay_service" \
    || die "$stage: VM SSH relay does not target $eth0:22"
  listeners="$(ss -H -ltn "sport = :$ssh_port")"
  grep -Eq "[[:space:]]127[.]0[.]0[.]1:${ssh_port}[[:space:]]" <<<"$listeners" \
    || die "$stage: VM SSH relay is not listening on IPv4 loopback"
  ! grep -Eq "[[:space:]](0[.]0[.]0[.]0|\*|\[::\]):${ssh_port}[[:space:]]" <<<"$listeners" \
    || die "$stage: VM SSH relay escaped the loopback boundary"
  incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- true >/dev/null \
    || die "$stage: outer VM agent is unreachable"
}

ensure_outer_foreign_ipv4() {
  incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- sh -eu -c '
    ip link show aaa0 >/dev/null 2>&1 || ip link add aaa0 type dummy
    ip -4 address show dev aaa0 | grep -q "172.31.255.1/24" \
      || ip address add 172.31.255.1/24 dev aaa0
    ip link set aaa0 up
  '
}

remove_owned_outer_backend() {
  local failed=0 owner used_by
  if [ -n "$OUTER_POOL" ] \
    && incus storage show "$OUTER_POOL" --project default >/dev/null 2>&1; then
    owner="$(incus storage get "$OUTER_POOL" user.subyard.owner \
      --project default 2>/dev/null)"
    if [ "$owner" != nested-teardown-e2e-v1 ]; then
      printf 'nested-teardown-boundary: refusing non-owned pool %s\n' \
        "$OUTER_POOL" >&2
      failed=1
    elif ! incus storage delete "$OUTER_POOL" --project default >/dev/null; then
      failed=1
    fi
  fi
  if [ -n "$OUTER_BRIDGE" ] \
    && incus network show "$OUTER_BRIDGE" --project default >/dev/null 2>&1; then
    owner="$(incus network get "$OUTER_BRIDGE" user.subyard.owner \
      --project default 2>/dev/null)"
    used_by="$(incus query "/1.0/networks/$OUTER_BRIDGE?project=default" \
      | jq -er '.used_by | arrays | length')"
    if [ "$owner" != nested-teardown-e2e-v1 ]; then
      printf 'nested-teardown-boundary: refusing non-owned network %s\n' \
        "$OUTER_BRIDGE" >&2
      failed=1
    elif [ "$used_by" != 0 ]; then
      printf 'nested-teardown-boundary: refusing network %s with active users\n' \
        "$OUTER_BRIDGE" >&2
      failed=1
    elif ! incus network delete "$OUTER_BRIDGE" --project default >/dev/null; then
      failed=1
    fi
  fi
  return "$failed"
}

cleanup() {
  local rc=$? cleanup_failed=0 oom_state
  trap - EXIT INT TERM
  set +e
  if [ "$rc" != 0 ] && [ -n "$OUTER_INSTANCE" ] \
    && [ "$(incus config get "$OUTER_INSTANCE" user.subyard.managed \
      --project "$OUTER_PROJECT" 2>/dev/null)" = true ]; then
    if oom_state="$(timeout 10 sudo -n journalctl -k --no-pager --since "$FIXTURE_STARTED" |
      awk '/Out of memory: Killed process [0-9]+ \(qemu/ {found=1}
        END {print found ? "present" : "absent"}')"; then
      printf 'nested-teardown-boundary: fixture qemu_oom_kill=%s\n' "$oom_state" >&2
    else
      printf 'nested-teardown-boundary: fixture qemu_oom_kill=unknown\n' >&2
    fi
    printf '\n== failed nested yard: instance and console ==\n' >&2
    timeout 10 incus info "$OUTER_INSTANCE" --project "$OUTER_PROJECT" --show-log >&2
    timeout 10 incus console "$OUTER_INSTANCE" --project "$OUTER_PROJECT" --show-log \
      | tail -n 100 >&2
  fi
  if [ -n "$OUTER_PROJECT" ] && incus project show "$OUTER_PROJECT" >/dev/null 2>&1; then
    yard teardown --yes >/dev/null 2>&1 || { rc=3; cleanup_failed=1; }
  fi
  remove_owned_outer_backend || { rc=3; cleanup_failed=1; }
  if [ "$cleanup_failed" = 0 ] && [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-nested-teardown.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = nested-teardown-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-nested-teardown.XXXXXX)"
printf '%s\n' nested-teardown-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
OUTER_YARD="nested-e2e-$token"
OUTER_PROJECT="subyard-$OUTER_YARD"
OUTER_INSTANCE="yard-$OUTER_YARD"
OUTER_POOL="nested-e2e-$token"
OUTER_BRIDGE="ne${token:0:8}br0"
DEFAULT_POOL_BEFORE="$(default_pool_snapshot)"
HOST_DEFAULT_ROUTE_BEFORE="$(ip -4 route show default | sort)"
HOST_GLOBAL_IPV4_BEFORE="$(ip -4 -o address show scope global | awk '{print $2, $4}' | sort)"
incus network show "$BASELINE_BRIDGE" --project default >/dev/null 2>&1 \
  || die "baseline Incus bridge $BASELINE_BRIDGE is missing"

incus project show "$OUTER_PROJECT" >/dev/null 2>&1 \
  && die "refusing existing project $OUTER_PROJECT"
incus storage show "$OUTER_POOL" --project default >/dev/null 2>&1 \
  && die "refusing existing pool $OUTER_POOL"
incus network show "$OUTER_BRIDGE" --project default >/dev/null 2>&1 \
  && die "refusing existing network $OUTER_BRIDGE"
incus storage create "$OUTER_POOL" dir user.subyard.owner=nested-teardown-e2e-v1 \
  --project default >/dev/null

export SUBYARD_OPERATOR_HOME="$STATE/operator"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/runtime"
export STORAGE_PATH="$STATE/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
export HOST_CLAUDE_MD=
export HOST_CODEX_AGENTS_MD=
export HOST_OPENCODE_AGENTS_MD=
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME/yards/$OUTER_YARD"

# Commit the current release's protected reader boundary before any yard effects.
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
YARD_BIN="$(release_candidate_install "$ROOT" "$STATE")"

ssh_port=$((32000 + ($$ % 10000)))
while ss -H -ltn "sport = :$ssh_port" 2>/dev/null | grep -q .; do
  ssh_port=$((ssh_port + 1))
done
cat > "$SUBYARD_CONFIG_HOME/yards/$OUTER_YARD/config.env" <<EOF
SSH_PORT=$ssh_port
CODING_TOOL_INTEGRATIONS=
YARD_KIND=vm
LIMITS_CPU=2
LIMITS_MEMORY=2GiB
SRV_POOL=$OUTER_POOL
INCUS_BRIDGE=$OUTER_BRIDGE
HOST_MOUNTS=
HOST_LINKS=
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
DEV_SUDO=1
NESTED_E2E_VMS=0
EOF

require_nested_memory_reserve
# Publish ownership with creation so cleanup also works when yard init fails.
incus network create "$OUTER_BRIDGE" ipv4.address=auto ipv6.address=none \
  user.subyard.owner=nested-teardown-e2e-v1 --project default >/dev/null
printf '  [ .. ] creating the outer yard\n'
yard init --yes
OUTER_DATA_HOME_METADATA="$(outer_data_home_metadata)"
yard start --yes
[ "$(incus config get "$OUTER_INSTANCE" user.subyard.managed --project "$OUTER_PROJECT")" = true ] \
  || die 'outer instance is not marker-owned'
require_outer_docker_ready
ensure_outer_foreign_ipv4
assert_outer_vm_ssh_address fresh-init
canonical_identity="$SUBYARD_HOME/ssh/id_ed25519"
canonical_fingerprint="$(ssh-keygen -lf "$canonical_identity.pub" | awk '{print $2}')"
legacy_identity="$STATE/legacy-operator-key"
ssh-keygen -q -t ed25519 -N '' -C legacy-operator -f "$legacy_identity"
legacy_public="$(awk 'NF >= 2 { print $1 " " $2; exit }' "$legacy_identity.pub")"
incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" \
  --env LEGACY_PUBLIC="$legacy_public" -- sh -eu -c '
    grep -qxF "$LEGACY_PUBLIC" /home/dev/.ssh/authorized_keys \
      || printf "%s\n" "$LEGACY_PUBLIC" >> /home/dev/.ssh/authorized_keys
  '
outer_snippet="$SUBYARD_OPERATOR_HOME/.ssh/subyard-$OUTER_YARD.config"
cat > "$outer_snippet" <<EOF
# Legacy fixture replaced by yard init.
Host yard-$OUTER_YARD
    HostName 127.0.0.1
    Port $ssh_port
    User dev
    IdentityFile $legacy_identity
    IdentitiesOnly yes
    StrictHostKeyChecking yes
    UserKnownHostsFile $SUBYARD_HOME/ssh/known_hosts
    ForwardAgent no
EOF
chmod 0600 "$outer_snippet"
SSH_AUTH_SOCK='' ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes \
  -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$SUBYARD_HOME/ssh/known_hosts" \
  -o GlobalKnownHostsFile=/dev/null -o IdentityFile="$legacy_identity" \
  -p "$ssh_port" dev@127.0.0.1 true \
  || die 'legacy named-yard access fixture is not usable before migration'
yard init --yes
assert_outer_data_home_metadata named-yard-init
grep -qxF "    IdentityFile \"$canonical_identity\"" "$outer_snippet" \
  || die 'named-yard migration did not switch to the canonical identity'
incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" \
  --env LEGACY_PUBLIC="$legacy_public" -- sh -eu -c \
  'grep -qxF "$LEGACY_PUBLIC" /home/dev/.ssh/authorized_keys' \
  || die 'named-yard migration removed the previous authorized key'
[ "$(ssh-keygen -lf "$canonical_identity.pub" | awk '{print $2}')" = "$canonical_fingerprint" ] \
  || die 'named-yard migration unexpectedly replaced the host-scoped identity'
docker_pid_before="$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
  systemctl show docker.service --property=MainPID --value)"
incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
  iptables -P FORWARD DROP
yard init --yes
assert_outer_data_home_metadata repeated-init
assert_outer_vm_ssh_address repeated-init
[ "$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
  iptables -S FORWARD | head -n 1)" = '-P FORWARD ACCEPT' ] \
  || die 'repeated init retained stale FORWARD DROP'
[ "$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
  systemctl show docker.service --property=MainPID --value)" = "$docker_pid_before" ] \
  || die 'repeated init restarted Docker without config drift'
yard stop --yes
yard start --yes
require_outer_docker_ready
ensure_outer_foreign_ipv4
yard init --yes
assert_outer_data_home_metadata restart-init
assert_outer_vm_ssh_address restart-init

printf '  [ .. ] syncing the exact candidate into the outer yard\n'
yard sync "$ROOT" --name NestedBoundary --target yard --yes >/dev/null
project_state="$(find "$SUBYARD_CONFIG_HOME/yards/$OUTER_YARD/projects" \
  -maxdepth 1 -type f -name '*.json' -print -quit)"
[ -n "$project_state" ] || die 'controller project state is missing'
project_id="$(jq -r '.projectId' "$project_state")"
outer_source="/srv/workspaces/$project_id/src"

yard code "$project_id" --yes > "$STATE/code.out"
outer_ssh="$(setting SSH_HOST)"
outer_ssh_namespace="$(printf '%s' "$outer_ssh" | base64 -w0 | tr '+/' '-_' | tr -d '=')"
descriptor="$SUBYARD_CONFIG_HOME/workspaces/$outer_ssh_namespace.$project_id/NestedBoundary.code-workspace"
[ -f "$descriptor" ] || die 'controller-local workspace descriptor is missing'
IFS= read -r outer_host_id < "$SUBYARD_CONFIG_HOME/host-id"
[ -n "$outer_host_id" ] || die 'controller HostID is missing'
expected_title="\${rootNameShort} — Yard SSH: $outer_host_id/$OUTER_YARD"
jq -e \
  --arg authority "ssh-remote+$outer_ssh" \
  --arg uri "vscode-remote://ssh-remote+$outer_ssh$outer_source" \
  --arg title "$expected_title" \
  '.remoteAuthority == $authority and .folders == [{name:"NestedBoundary", uri:$uri}] and
    .settings["window.title"] == $title' \
  "$descriptor" >/dev/null \
  || die 'workspace descriptor does not select the expected Remote-SSH workspace'

outer_dev() {
  bounded_command outer-dev incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" \
    --user 1000 --group 1000 --env HOME=/home/dev --env USER=dev --env LOGNAME=dev -- "$@"
}

printf '  [ .. ] creating and tearing down a current-candidate inner yard\n'
outer_dev bash -euc '
  source=$1
  candidate_state="$HOME/.cache/subyard-nested-teardown-candidate"
  export YARD_RUNTIME_ROOT="$candidate_state/runtime"
  export HOST_MOUNTS= HOST_LINKS= FORWARD_SSH_AGENT=0 DEV_SUDO=0
  export NESTED_E2E_VMS=0 SSH_PORT=23222 MIN_DISK_GIB=1
  export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
  export HOST_CLAUDE_MD= HOST_CODEX_AGENTS_MD= HOST_OPENCODE_AGENTS_MD=
  # The synced source carries the same authenticated prebuilt candidate archive.
  jq -e --arg sha "$2" ".runtime_sha256 == \$sha" \
    "$source/.subyard-acceptance/candidate.json" >/dev/null
  . "$source/tests/helpers/release-candidate.sh"
  launcher=$(release_candidate_install "$source" "$candidate_state")
  printf -v teardown_command "%q " "$launcher" teardown --yes
  install -d "$HOME/.subyard/workspaces"
  install -d -m 0700 "$HOME/.config/subyard/yards/default"
  printf "CODING_TOOL_INTEGRATIONS=\nSSH_PORT=23222\n" > "$HOME/.config/subyard/yards/default/config.env"
  printf "outer sentinel\n" > "$HOME/.subyard/workspaces/active.code-workspace"
  cd "$source"
  "$launcher" init --yes
  sudo -n incus project show subyard >/dev/null
  sg incus-admin -c "exec $teardown_command"
  ! sudo -n incus project show subyard >/dev/null 2>&1
  [ -f "$HOME/.subyard/workspaces/active.code-workspace" ]
  [ ! -e "$HOME/.config/subyard/projects" ]
  sg incus-admin -c "exec $teardown_command" >/dev/null
' _ "$outer_source" "$(jq -er .runtime_sha256 "$ROOT/.subyard-acceptance/candidate.json")"

incus info "$OUTER_INSTANCE" --project "$OUTER_PROJECT" >/dev/null \
  || die 'inner teardown removed the outer instance'
[ -f "$descriptor" ] || die 'inner teardown removed the controller descriptor'
outer_dev sudo -n rm -rf -- /home/dev/.subyard
[ -f "$descriptor" ] || die 'agent data deletion removed the controller descriptor'
incus info "$OUTER_INSTANCE" --project "$OUTER_PROJECT" >/dev/null \
  || die 'agent data deletion stopped the outer instance'
yard shell "$project_id" --yes -- true \
  || die 'inner teardown or agent data deletion broke outer SSH transport'

printf '  [ .. ] tearing down the outer yard and checking the default pool\n'
outer_config="$SUBYARD_OPERATOR_HOME/.ssh/config"
outer_include="Include subyard-$OUTER_YARD.config"
grep -qxF "$outer_include" "$outer_config" \
  || die 'outer SSH config is missing the managed Include before teardown'
teardown_canary="$STATE/teardown-config-tmp-canary"
sudo -n install -o root -g root -m 0600 /dev/null "$teardown_canary"
printf 'teardown config.tmp canary\n' | sudo -n tee "$teardown_canary" >/dev/null
teardown_canary_digest="$(sudo -n sha256sum "$teardown_canary" | awk '{print $1}')"
teardown_canary_metadata="$(stat -c '%u:%g:%a' "$teardown_canary")"
operator_user="$(stat -c '%U' "$outer_config")"
[ "$operator_user" != UNKNOWN ] || die 'could not resolve the outer SSH config owner'
legacy_config_tmp="$outer_config.tmp"
sudo -n -u "$operator_user" -- ln -s "$teardown_canary" "$legacy_config_tmp"

yard teardown --yes
! incus project show "$OUTER_PROJECT" >/dev/null 2>&1 \
  || die 'outer project remains after teardown'
[ "$(sudo -n sha256sum "$teardown_canary" | awk '{print $1}')" = "$teardown_canary_digest" ] \
  && [ "$(stat -c '%u:%g:%a' "$teardown_canary")" = "$teardown_canary_metadata" ] \
  || die 'outer teardown changed the root-owned config.tmp canary'
! grep -qxF "$outer_include" "$outer_config" \
  || die 'outer teardown retained the managed SSH Include'
[ "$(stat -c %u "$outer_config")" != 0 ] \
  && [ "$(stat -c %u "$SUBYARD_OPERATOR_HOME/.ssh/.subyard-config.lock")" != 0 ] \
  || die 'outer teardown left the SSH config or lock root-owned'
[ -L "$legacy_config_tmp" ] && [ "$(readlink "$legacy_config_tmp")" = "$teardown_canary" ] \
  || die 'outer teardown changed the legacy config.tmp symlink'
sudo -n -u "$operator_user" -- rm -f -- "$legacy_config_tmp"
remove_owned_outer_backend || die 'could not remove the marker-owned outer backend'
assert_default_pool_unchanged
assert_host_network_unchanged

printf 'ok: nested teardown preserves the outer yard boundary, foreign data and default pool\n'

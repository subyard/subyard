#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-docker-forwarding.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
export TEST_LOG="$TMP/log"

cat >"$TMP/bin/systemctl" <<'SH'
#!/bin/sh
printf 'systemctl %s\n' "$*" >>"$TEST_LOG"
if [ "$*" = 'enable --now ssh docker' ]; then
  [ "${TEST_SERVICE_FAIL:-0}" = 0 ] || exit 1
  : >"$TEST_STARTED"
fi
if [ "${1:-}" = show ]; then
  printf '%s\n' 'Type=notify' 'LoadState=loaded' 'ActiveState=active' \
    'SubState=running' 'Result=success' 'MainPID=123' 'ExecMainStatus=0' \
    'ControlPID=PRIVATE_PAYLOAD' 'NRestarts=１２３' \
    'ActiveEnterTimestampMonotonic=100' 'ExecMainStartTimestampMonotonic=99' \
    'InactiveExitTimestampMonotonic=98' 'Environment=PRIVATE_PAYLOAD' \
    'PRIVATE_PAYLOAD' >&1
  printf '%s\n' 'PRIVATE_STDERR' >&2
  if [ "$2" = containerd.service ]; then
    printf '%s\n' 'ActiveState=PRIVATE_PAYLOAD'
  fi
  exit "${TEST_COMPARE_EXIT:-0}"
fi
SH
cat >"$TMP/bin/docker" <<'SH'
#!/bin/sh
[ -f "$TEST_STARTED" ] || exit 99
printf 'docker %s\n' "$*" >>"$TEST_LOG"
printf '%s\n' 'PRIVATE_PAYLOAD'
printf '%s\n' 'PRIVATE_STDERR' >&2
case "$*" in
  'container ls -a --format {{.Names}}')
    if [ "${TEST_API_HANG:-0}" = 1 ]; then
      printf '%s\n' "$$" >"$TEST_API_PID"
      exec sleep 30
    fi
    exit "${TEST_API_EXIT:-0}" ;;
  info|'version --format {{.Server.Version}}')
    exit "${TEST_COMPARE_EXIT:-0}" ;;
  *) exit 99 ;;
esac
SH
cat >"$TMP/bin/iptables" <<'SH'
#!/bin/sh
case "$*" in
  '-S FORWARD') printf '%s\n' '-P FORWARD DROP' ;;
  *) printf 'iptables %s\n' "$*" >>"$TEST_LOG" ;;
esac
SH
cat >"$TMP/bin/timeout" <<'SH'
#!/bin/sh
printf 'timeout %s\n' "$*" >>"$TEST_LOG"
exec /usr/bin/timeout "$@"
SH
chmod +x "$TMP/bin/systemctl" "$TMP/bin/iptables" "$TMP/bin/docker" "$TMP/bin/timeout"
PATH="$TMP/bin:/usr/bin:/bin"
export PATH

docker_function="$TMP/docker-network.sh"
sed -n '/^docker_forwarding_converge()/,/^}/p' \
  "$ROOT/scripts/04-provision-subyard.sh" >"$docker_function"
# shellcheck disable=SC1090
. "$docker_function"

docker_forwarding_converge 0
grep -Fxq 'iptables -P FORWARD ACCEPT' "$TEST_LOG" \
  || { printf 'FAIL: stale FORWARD DROP was not repaired\n' >&2; exit 1; }
! grep -Fq 'systemctl restart docker' "$TEST_LOG" \
  || { printf 'FAIL: unchanged Docker config restarted active workloads\n' >&2; exit 1; }

: >"$TEST_LOG"
docker_forwarding_converge 1
grep -Fxq 'systemctl restart docker' "$TEST_LOG" \
  || { printf 'FAIL: Docker config drift did not trigger restart\n' >&2; exit 1; }

# Execute the owning guest service boundary, rather than checking its source text.
services="$TMP/yard-services.sh"
sed -n '/^# Services live /,/^EOS/{ /^# Services live /d; /^EOS/d; p; }' \
  "$ROOT/scripts/04-provision-subyard.sh" >"$services"
export TEST_STARTED="$TMP/started"
export TEST_API_PID="$TMP/api.pid"
for TEST_SERVICE_FAIL in 0 1; do
  for TEST_API_EXIT in 0 1 124; do
   for TEST_COMPARE_EXIT in 0 1 124; do
    export TEST_SERVICE_FAIL TEST_API_EXIT TEST_COMPARE_EXIT
    rm -f "$TEST_STARTED"
    : >"$TEST_LOG"
    status=0
    bash -euo pipefail "$services" >"$TMP/stdout" 2>"$TMP/stderr" || status=$?
    if [ "$TEST_SERVICE_FAIL" = 0 ] && [ "$TEST_API_EXIT" = 0 ]; then
      [ "$status" = 0 ] || { cat "$TMP/stderr" >&2; exit 1; }
    else
      [ "$status" = 1 ] || { printf 'FAIL: original provisioning failure changed\n' >&2; exit 1; }
    fi
    if [ "$TEST_SERVICE_FAIL" = 0 ]; then
      [ "$(grep -Fxc 'docker container ls -a --format {{.Names}}' "$TEST_LOG")" = 1 ] \
        || { printf 'FAIL: service start did not issue one inventory query\n' >&2; exit 1; }
    else
      ! grep -Fq 'docker ' "$TEST_LOG" \
        || { printf 'FAIL: failed service start reached Docker API\n' >&2; exit 1; }
    fi
    python3 - "$TMP" <<'PY'
import json
import os
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
stdout, stderr = (root / "stdout").read_text(), (root / "stderr").read_text()
assert not stdout, stdout
assert "PRIVATE" not in stderr, stderr
records = [json.loads(line) for line in stderr.splitlines() if line.startswith("{")]
failed_api = os.environ["TEST_SERVICE_FAIL"] == "0" and os.environ["TEST_API_EXIT"] != "0"
commands = (root / "log").read_text()
if not failed_api:
    assert not records, records
    assert "docker info" not in commands and "docker version" not in commands and "systemctl show" not in commands
    sys.exit(0)
assert stderr.splitlines()[-1] == "Docker API is unavailable after service startup"
probes = {r['probe']: r for r in records if r['probe'] != 'service-state'}
primary = probes['container-enumeration']
assert primary['exit'] == int(os.environ['TEST_API_EXIT'])
assert primary['clock'] == 'realtime' and primary['budget_ms'] == 10000
assert primary['kill_after_ms'] == 1000
comparison_exit = int(os.environ["TEST_COMPARE_EXIT"])
for record in records:
    assert record["diagnostic"] == "docker-provision-startup"
    assert isinstance(record["elapsed_ms"], int) and record["elapsed_ms"] >= 0
for probe in ('info', 'server-version'):
    assert probes[probe]['exit'] == comparison_exit
    assert probes[probe]['budget_ms'] == 10000 and probes[probe]['kill_after_ms'] == 1000
services = {r['unit']: r for r in records if r['probe'] == 'service-state'}
for unit in ('docker.service', 'containerd.service'):
    record = services[unit]
    assert record["budget_ms"] == 3000 and record["kill_after_ms"] == 1000
    if comparison_exit:
        assert not record["state"]
    else:
        state = record["state"]
        expected_active = "active" if record["unit"] == "docker.service" else "unknown"
        assert state["ActiveState"] == expected_active and state["SubState"] == "running"
        assert state["Type"] == "notify" and state["MainPID"] == "123"
        assert state["ControlPID"] == "unknown" and state["NRestarts"] == "unknown"
        assert "Environment" not in state
PY
   done
  done
done

# A real inventory timeout stays fatal even when later diagnostics succeed.
export TEST_SERVICE_FAIL=0 TEST_API_EXIT=0 TEST_COMPARE_EXIT=0 TEST_API_HANG=1
rm -f "$TEST_STARTED"
: >"$TEST_LOG"
status=0
bash -euo pipefail "$services" >"$TMP/stdout" 2>"$TMP/stderr" || status=$?
[ "$status" = 1 ] || { printf 'FAIL: inventory timeout did not fail provisioning\n' >&2; exit 1; }
! kill -0 "$(cat "$TEST_API_PID")" 2>/dev/null \
  || { printf 'FAIL: inventory query survived its timeout\n' >&2; exit 1; }
python3 - "$TMP" <<'PY'
import json
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
stderr = (root / 'stderr').read_text()
assert not (root / 'stdout').read_text() and 'PRIVATE' not in stderr
probes = {r['probe']: r for line in stderr.splitlines() if line.startswith('{')
          for r in [json.loads(line)] if r['probe'] != 'service-state'}
assert probes['container-enumeration']['exit'] == 124
assert probes['info']['exit'] == 0 and probes['server-version']['exit'] == 0
assert (root / 'log').read_text().splitlines().count('docker container ls -a --format {{.Names}}') == 1
PY

# Execute the real installer branch with the shared atomic download policy.
installer="$TMP/docker-installer.sh"
sed -n '/^# Docker Engine /,/^# Groups /{ /^# Groups /d; p; }' \
  "$ROOT/scripts/04-provision-subyard.sh" > "$installer"
cat > "$TMP/bin/curl" <<'PY'
#!/usr/bin/python3
import os,pathlib,stat,sys
args=sys.argv[1:];base=pathlib.Path(os.environ['TEST_LOG']).parent
counter=base/'fetch-count';attempt=int(counter.read_text())+1 if counter.exists() else 1
counter.write_text(str(attempt))
output=pathlib.Path(args[args.index('--output')+1]) if '--output' in args else None
if output:
    assert stat.S_IMODE(output.stat().st_mode)==0o600
    with (base/'fetch-paths').open('a') as log:log.write(str(output)+'\n')
scenario=os.environ['TEST_DOWNLOAD']
code=6 if scenario=='dns' or (scenario=='dns-recovery' and attempt==1) else 18 if scenario=='partial' or (scenario=='partial-recovery' and attempt==1) else 0
body='printf "complete\\n" >> "$TEST_INSTALLED"\nexit "${TEST_INSTALL_EXIT:-0}"\n'
if code==18:body='printf "partial\\n" >> "$TEST_INSTALLED"\n'
if code!=6:
    if output:output.write_text(body)
    else:sys.stdout.write(body)
if '--write-out' in args:sys.stdout.write('000' if code==6 else '200')
sys.exit(code)
PY
cat > "$TMP/bin/sh" <<'SH'
#!/bin/sh
if [ "$#" -gt 0 ]; then
  [ "$(stat -c %a "$1")" = 600 ] || exit 99
  printf '%s\n' "$1" >> "$TEST_LOG.installer-paths"
fi
exec /bin/sh "$@"
SH
cat > "$TMP/bin/chown" <<'SH'
#!/bin/sh
[ "$1" = root:root ] || exit 99
SH
cat > "$TMP/bin/sleep" <<'SH'
#!/bin/sh
exit 0
SH
chmod 0755 "$TMP/bin/curl" "$TMP/bin/sh" "$TMP/bin/chown" "$TMP/bin/sleep"
command() {
  if [ "$#" = 2 ] && [ "$1" = -v ] && [ "$2" = docker ]; then
    [ "$TEST_DOWNLOAD" = installed ]; return
  fi
  builtin command "$@"
}
export -f command docker_forwarding_converge
export TEST_INSTALLED="$TMP/installer-executed"
for TEST_DOWNLOAD in dns-recovery partial-recovery dns partial installer-failure installed; do
  export TEST_DOWNLOAD TEST_INSTALL_EXIT=0
  [ "$TEST_DOWNLOAD" != installer-failure ] || export TEST_INSTALL_EXIT=42
  rm -f "$TMP/fetch-count" "$TMP/fetch-paths" "$TEST_INSTALLED" "$TEST_LOG.installer-paths"
  status=0
  bash -euo pipefail -c '. "$1"; docker_config_changed=0; . "$2"' \
    subyard "$ROOT/scripts/lib/download.sh" "$installer" > "$TMP/stdout" 2> "$TMP/stderr" || status=$?
  case "$TEST_DOWNLOAD" in
    dns-recovery|partial-recovery) [ "$status" = 0 ] && [ "$(cat "$TMP/fetch-count")" = 2 ] ;;
    dns|partial) [ "$status" = 1 ] && [ "$(cat "$TMP/fetch-count")" = 4 ] ;;
    installer-failure) [ "$status" = 42 ] && [ "$(cat "$TMP/fetch-count")" = 1 ] ;;
    installed) [ "$status" = 0 ] && [ ! -e "$TMP/fetch-count" ] ;;
  esac || { printf 'FAIL: Docker installer download outcome %s exit=%s\n' "$TEST_DOWNLOAD" "$status" >&2; exit 1; }
  case "$TEST_DOWNLOAD" in
    dns-recovery|partial-recovery|installer-failure)
      [ "$(cat "$TEST_INSTALLED")" = complete ]
      [ "$(wc -l < "$TEST_LOG.installer-paths")" = 1 ] ;;
    *) [ ! -e "$TEST_INSTALLED" ] && [ ! -e "$TEST_LOG.installer-paths" ] ;;
  esac
  for paths in "$TMP/fetch-paths" "$TEST_LOG.installer-paths"; do
    [ ! -e "$paths" ] || while IFS= read -r path; do [ ! -e "$path" ]; done < "$paths"
  done
done

printf 'ok: Docker forwarding, service readiness and atomic installer download\n'

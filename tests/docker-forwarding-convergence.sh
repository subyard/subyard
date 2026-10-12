#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
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
  info) exit "${TEST_API_EXIT:-0}" ;;
  'version --format {{.Server.Version}}'|'container ls -a --format {{.Names}}')
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
      [ "$(grep -Fxc 'docker info' "$TEST_LOG")" = 1 ] \
        || { printf 'FAIL: service start did not verify Docker API\n' >&2; exit 1; }
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
    assert "docker version" not in commands and "systemctl show" not in commands
    sys.exit(0)
assert stderr.splitlines()[-1] == "Docker API is unavailable after service startup"
assert len(records) == 5, records
assert [r["probe"] for r in records] == ["info", "server-version", "container-enumeration",
                                         "service-state", "service-state"]
assert records[0]["exit"] == int(os.environ["TEST_API_EXIT"])
assert records[0]["clock"] == "realtime" and records[0]["budget_ms"] == 10000
assert "timeout 10 docker info\n" in commands
assert "docker version --format {{.Server.Version}}\n" in commands
assert "docker container ls -a --format {{.Names}}\n" in commands
assert "timeout --kill-after=1 10 docker version --format {{.Server.Version}}\n" in commands
assert "timeout --kill-after=1 10 docker container ls -a --format {{.Names}}\n" in commands
assert "timeout --kill-after=1 3 systemctl show docker.service --property=" in commands
assert "timeout --kill-after=1 3 systemctl show containerd.service --property=" in commands
comparison_exit = int(os.environ["TEST_COMPARE_EXIT"])
for record in records:
    assert record["diagnostic"] == "docker-provision-startup"
    assert isinstance(record["elapsed_ms"], int) and record["elapsed_ms"] >= 0
for record in records[1:]:
    assert record["exit"] == comparison_exit
assert [r["unit"] for r in records[3:]] == ["docker.service", "containerd.service"]
for record in records[3:]:
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

printf 'ok: Docker forwarding and service readiness convergence\n'

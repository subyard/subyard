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
SH
cat >"$TMP/bin/docker" <<'SH'
#!/bin/sh
[ "$*" = info ] && [ -f "$TEST_STARTED" ] || exit 99
printf 'docker API checked\n' >>"$TEST_LOG"
exit "${TEST_API_EXIT:-0}"
SH
cat >"$TMP/bin/iptables" <<'SH'
#!/bin/sh
case "$*" in
  '-S FORWARD') printf '%s\n' '-P FORWARD DROP' ;;
  *) printf 'iptables %s\n' "$*" >>"$TEST_LOG" ;;
esac
SH
chmod +x "$TMP/bin/systemctl" "$TMP/bin/iptables" "$TMP/bin/docker"
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
  for TEST_API_EXIT in 0 1; do
    export TEST_SERVICE_FAIL TEST_API_EXIT
    rm -f "$TEST_STARTED"
    : >"$TEST_LOG"
    status=0
    bash -euo pipefail "$services" >"$TMP/stdout" 2>"$TMP/stderr" || status=$?
    if [ "$TEST_SERVICE_FAIL" = 0 ] && [ "$TEST_API_EXIT" = 0 ]; then
      [ "$status" = 0 ] || { cat "$TMP/stderr" >&2; exit 1; }
    else
      [ "$status" != 0 ] || { printf 'FAIL: unavailable Docker accepted\n' >&2; exit 1; }
    fi
    if [ "$TEST_SERVICE_FAIL" = 0 ]; then
      grep -Fxq 'docker API checked' "$TEST_LOG" \
        || { printf 'FAIL: service start did not verify Docker API\n' >&2; exit 1; }
    else
      ! grep -Fxq 'docker API checked' "$TEST_LOG" \
        || { printf 'FAIL: failed service start reached Docker API\n' >&2; exit 1; }
    fi
  done
done

printf 'ok: Docker forwarding and service readiness convergence\n'

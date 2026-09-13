#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/config/agents/aiobserver/provision.sh"
TMP="$(mktemp -d)"
cleanup() { chmod -R u+w "$TMP" 2>/dev/null || true; find "$TMP" -depth -delete; }
trap cleanup EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_file() { [ -f "$1" ] || fail "missing file: $1"; }
assert_log_absent() {
  ! grep -Eq "$1" "$FAKE_LOG" || fail "unexpected external action matching: $1"
}

mkdir -p "$TMP/bin"
export AI_OBSERVER_FAKE_ROOT="$TMP/fake"
export FAKE_LOG="$TMP/external.log"
mkdir -p "$AI_OBSERVER_FAKE_ROOT/containers" "$AI_OBSERVER_FAKE_ROOT/images"
: >"$FAKE_LOG"

cat >"$TMP/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root="${AI_OBSERVER_FAKE_ROOT:?}"
printf 'docker' >>"${FAKE_LOG:?}"
printf ' %q' "$@" >>"$FAKE_LOG"
printf '\n' >>"$FAKE_LOG"
[ -z "${AI_OBSERVER_FAKE_DOCKER_DELAY:-}" ] || sleep "$AI_OBSERVER_FAKE_DOCKER_DELAY"
container_dir() { printf '%s/containers/%s' "$root" "$1"; }
case "${1:-}" in
  info) exit 0 ;;
  image)
    [ "${2:-}" = inspect ] || exit 91
    [ -f "$root/images/image" ]
    ;;
  pull)
    [ "${AI_OBSERVER_FAKE_PULL_FAIL:-0}" != 1 ] || exit 92
    printf '%s\n' "${2:?}" >"$root/images/image"
    ;;
  container)
    [ "${2:-}" = inspect ] || exit 93
    [ -d "$(container_dir "${3:?}")" ]
    ;;
  inspect)
    [ "${2:-}" = -f ] || exit 94
    format="${3:?}" dir="$(container_dir "${4:?}")"
    [ -d "$dir" ] || exit 1
    if [ -f "$root/inspect-fail-once" ] &&
      [ "$(cat "$root/inspect-fail-once")" = "$format" ]; then
      rm -f "$root/inspect-fail-once"
      exit 86
    fi
    case "$format" in
      '{{ index .Config.Labels "org.subyard.managed" }}') cat "$dir/owner" ;;
      '{{ index .Config.Labels "org.subyard.ai-observer.spec" }}') cat "$dir/spec" ;;
      '{{.Config.Image}}') cat "$dir/image" ;;
      '{{.Config.User}}') cat "$dir/user" ;;
      '{{json .Config.Cmd}}') cat "$dir/cmd" ;;
      '{{range .Config.Env}}{{println .}}{{end}}') cat "$dir/env" ;;
      '{{json .HostConfig.Binds}}') cat "$dir/binds" ;;
      '{{json .HostConfig.PortBindings}}') cat "$dir/ports" ;;
      '{{.HostConfig.RestartPolicy.Name}}') cat "$dir/restart" ;;
      '{{.State.Running}}') cat "$dir/running" ;;
      *) printf 'unsupported inspect format: %s\n' "$format" >&2; exit 95 ;;
    esac
    ;;
  create)
    shift
    name='' owner='' spec='' user='' publish='' restart='no'
    volumes=() envs=()
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --name) name="$2"; shift 2 ;;
        --label)
          case "$2" in
            org.subyard.managed=*) owner="${2#*=}" ;;
            org.subyard.ai-observer.spec=*) spec="${2#*=}" ;;
          esac
          shift 2
          ;;
        --user) user="$2"; shift 2 ;;
        --publish) publish="$2"; shift 2 ;;
        --restart) restart="$2"; shift 2 ;;
        --volume) volumes+=("$2"); shift 2 ;;
        --env) envs+=("$2"); shift 2 ;;
        --*) printf 'unsupported create option: %s\n' "$1" >&2; exit 96 ;;
        *) image="$1"; shift; break ;;
      esac
    done
    [ "$#" -eq 3 ] && [ "$1 $2 $3" = 'watch all --backfill' ] || exit 97
    [ -n "$name" ] && [ ! -e "$(container_dir "$name")" ] || exit 98
    dir="$(container_dir "$name")"; mkdir "$dir"
    printf '%s\n' "$owner" >"$dir/owner"
    printf '%s\n' "$spec" >"$dir/spec"
    printf '%s\n' "$image" >"$dir/image"
    printf '%s\n' "$user" >"$dir/user"
    printf '%s\n' '["watch","all","--backfill"]' >"$dir/cmd"
    if [ "${AI_OBSERVER_FAKE_CREATE_BIND_MODE_DRIFT:-0}" = 1 ]; then
      volumes[1]="${volumes[1]%:ro}:rw"
    fi
    printf '["%s","%s","%s"]\n' "${volumes[0]}" "${volumes[1]}" "${volumes[2]}" >"$dir/binds"
    if [ -n "${AI_OBSERVER_FAKE_CREATE_INSPECT_FAIL_FORMAT:-}" ]; then
      printf '%s\n' "$AI_OBSERVER_FAKE_CREATE_INSPECT_FAIL_FORMAT" >"$root/inspect-fail-once"
    fi
    [ "$publish" = 127.0.0.1:8080:8080 ] || exit 99
    printf '%s\n' '{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}' >"$dir/ports"
    printf '%s\n' "$restart" >"$dir/restart"
    printf '%s\n' false >"$dir/running"
    printf '%s\n' "${envs[@]}" >"$dir/env"
    if [ -f "$root/partial-create-once" ]; then
      rm -f "$root/partial-create-once"
      exit 87
    fi
    ;;
  rename)
    mv "$(container_dir "${2:?}")" "$(container_dir "${3:?}")"
    ;;
  rm)
    shift
    [ "${1:-}" != -f ] || shift
    find "$(container_dir "${1:?}")" -depth -delete
    ;;
  stop)
    shift
    [ "${1:-}" != --time ] || shift 2
    printf '%s\n' false >"$(container_dir "${1:?}")/running"
    ;;
  start)
    shift
    [ "${1:-}" != --attach ] || shift
    printf '%s\n' true >"$(container_dir "${1:?}")/running"
    ;;
  exec)
    shift
    name="${1:?}"; shift
    [ "$(cat "$(container_dir "$name")/running")" = true ] || exit 1
    [ "$*" = '/app/ai-observer --version' ] || exit 90
    printf 'ai-observer 0.5.0\n'
    ;;
  logs) exit 0 ;;
  *) printf 'unsupported docker call: %s\n' "$*" >&2; exit 89 ;;
esac
SH

cat >"$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root="${AI_OBSERVER_FAKE_ROOT:?}"
printf 'systemctl' >>"${FAKE_LOG:?}"
printf ' %q' "$@" >>"$FAKE_LOG"
printf '\n' >>"$FAKE_LOG"
case "${1:-}" in
  daemon-reload) ;;
  is-enabled) [ -f "$root/enabled" ] ;;
  is-active)
    [ -f "$root/active" ] &&
      [ "$(cat "$root/containers/subyard-ai-observer/running" 2>/dev/null)" = true ]
    ;;
  enable)
    touch "$root/enabled"
    if [ "${2:-}" = --now ]; then set -- start "${3:?}"; else exit 0; fi
    ;&
  start|restart)
    if [ -f "$root/fail-start-once" ]; then
      rm -f "$root/fail-start-once"
      exit 1
    fi
    touch "$root/active"
    printf '%s\n' true >"$root/containers/subyard-ai-observer/running"
    ;;
  stop)
    rm -f "$root/active"
    [ ! -d "$root/containers/subyard-ai-observer" ] ||
      printf '%s\n' false >"$root/containers/subyard-ai-observer/running"
    ;;
  disable)
    rm -f "$root/enabled" "$root/active"
    [ ! -d "$root/containers/subyard-ai-observer" ] ||
      printf '%s\n' false >"$root/containers/subyard-ai-observer/running"
    ;;
  *) printf 'unsupported systemctl call: %s\n' "$*" >&2; exit 88 ;;
esac
SH

cat >"$TMP/bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl' >>"${FAKE_LOG:?}"
printf ' %q' "$@" >>"$FAKE_LOG"
printf '\n' >>"$FAKE_LOG"
if [ "${AI_OBSERVER_FAKE_HTTP_FAIL:-0}" = 1 ]; then
  printf 'probe-output-should-stay-private\n' >&2
  exit 22
fi
if [ -f "${AI_OBSERVER_FAKE_ROOT:?}/http-failures-left" ]; then
  failures=$(cat "$AI_OBSERVER_FAKE_ROOT/http-failures-left")
  if [ "$failures" -gt 0 ]; then
    printf '%s\n' "$((failures - 1))" >"$AI_OBSERVER_FAKE_ROOT/http-failures-left"
    exit 52
  fi
fi
[ "$(cat "${AI_OBSERVER_FAKE_ROOT:?}/containers/subyard-ai-observer/running" 2>/dev/null)" = true ]
[ "${*: -1}" = http://127.0.0.1:8080/health ]
printf '{"status":"ok"}\n'
SH
chmod +x "$TMP/bin/docker" "$TMP/bin/systemctl" "$TMP/bin/curl"
export PATH="$TMP/bin:$PATH"

run_hook() {
  local test_root="$1" dev_home="$2" integrations="$3" context="${4:-}"
  AI_OBSERVER_TEST_ROOT="$test_root" \
    AI_OBSERVER_STARTUP_TIMEOUT_SECONDS="${AI_OBSERVER_STARTUP_TIMEOUT_SECONDS:-3}" \
    AI_OBSERVER_TEST_ALLOW_NON_ROOT=1 \
    AI_OBSERVER_TEST_DEV_HOME="$dev_home" \
    AI_OBSERVER_CONTEXT="$context" \
    DEV_USER="$(id -un)" \
    CODING_TOOL_INTEGRATIONS="$integrations" \
    bash "$HOOK"
}

test_root="$TMP/root"
dev_home="$TMP/dev-home"
mkdir -p "$test_root" "$dev_home/.claude" "$dev_home/.codex/sessions" "$TMP/shared/claude"
ln -s "$TMP/shared/claude" "$dev_home/.claude/projects"
printf 'claude history\n' >"$TMP/shared/claude/session.jsonl"
printf 'codex history\n' >"$dev_home/.codex/sessions/session.jsonl"

run_hook "$test_root" "$dev_home" 'claude codex aiobserver'

wrapper="$test_root/usr/local/bin/ai-observer"
check="$test_root/usr/local/bin/ai-observer-check"
unit="$test_root/etc/systemd/system/subyard-ai-observer.service"
state="$test_root/srv/agents/ai-observer"
managed_marker="$test_root/etc/subyard/ai-observer/managed"
container="$AI_OBSERVER_FAKE_ROOT/containers/subyard-ai-observer"
for file in "$wrapper" "$check" "$unit" "$managed_marker"; do assert_file "$file"; done
[ "$(stat -c %a "$wrapper")" = 755 ] || fail 'wrapper mode is not 0755'
[ "$(stat -c %a "$check")" = 755 ] || fail 'check mode is not 0755'
[ "$(stat -c %a "$unit")" = 644 ] || fail 'unit mode is not 0644'
[ "$(stat -c %a "$managed_marker")" = 644 ] || fail 'managed marker mode is not 0644'
[ "$(cat "$managed_marker")" = subyard-ai-observer-v1 ] || fail 'managed marker content drifted'
[ -d "$state/data" ] || fail 'persistent data directory missing'
[ "$(cat "$container/image")" = 'tobilg/ai-observer:0.5.0@sha256:e2d8f8fdf5e0b55b2cbb1f0db84288eca4d8c3727f9ec569a92704c3a2ecc30f' ] \
  || fail 'container image is not the pinned release'
[ "$(cat "$container/user")" = "$(id -u):$(id -g)" ] || fail 'container does not use developer UID:GID'
[ "$(cat "$container/binds")" = "[\"$state/data:/app/data:rw\",\"$TMP/shared/claude:/sessions/claude:ro\",\"$dev_home/.codex/sessions:/sessions/codex:ro\"]" ] \
  || fail 'container mounts do not expose only resolved, read-only session trees'
[ "$(cat "$container/ports")" = '{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}' ] \
  || fail 'dashboard port is not loopback-only'
[ "$(cat "$container/cmd")" = '["watch","all","--backfill"]' ] || fail 'watch/backfill command drifted'
grep -Fxq 'AI_OBSERVER_CLAUDE_PATH=/sessions/claude' "$container/env" || fail 'Claude path env missing'
grep -Fxq 'AI_OBSERVER_CODEX_PATH=/sessions/codex' "$container/env" || fail 'Codex path env missing'
grep -Fxq 'AI_OBSERVER_DATABASE_PATH=/app/data/ai-observer.duckdb' "$container/env" || fail 'database env missing'
grep -Fxq 'AI_OBSERVER_API_PORT=8080' "$container/env" || fail 'API port env missing'
! grep -Eqi 'docker[.]sock|4318|0[.]0[.]0[.]0' "$container/binds" "$container/ports" "$container/env" \
  || fail 'container exposes a credential, OTLP, Docker, or LAN boundary'
grep -Fq 'ExecStart='"$wrapper"' run' "$unit" || fail 'unit does not use the managed wrapper'
grep -Fq 'ExecStop='"$wrapper"' stop' "$unit" || fail 'unit stop is not controlled'
"$check" >/dev/null || fail 'installed readiness check failed'
[ "$("$wrapper" --version)" = 'ai-observer 0.5.0' ] || fail 'wrapper version command failed'
status_output="$("$wrapper" status)" || fail 'wrapper status command failed'
[[ "$status_output" == *'ai-observer 0.5.0 ready'* ]] || fail 'wrapper status omitted readiness'
[[ "$status_output" != *"$TMP"* ]] && [[ "$status_output" != *'AI_OBSERVER_'* ]] \
  || fail 'wrapper status exposed container paths or environment'
grep -Eq 'curl .*--connect-timeout 2 .*--max-time 5 .*http://127[.]0[.]0[.]1:8080/health' "$FAKE_LOG" \
  || fail 'HTTP readiness request is not bounded'

spec_inspect_format='{{ index .Config.Labels "org.subyard.ai-observer.spec" }}'
printf '%s\n' "$spec_inspect_format" >"$AI_OBSERVER_FAKE_ROOT/inspect-fail-once"
set +e
"$check" >"$TMP/transient-inspect-check.log" 2>&1
transient_check_status=$?
set -e
[ "$transient_check_status" = 1 ] \
  || fail "transient specification inspect returned $transient_check_status instead of retryable status 1"
grep -Fxq 'ai-observer-check: container specification unavailable' \
  "$TMP/transient-inspect-check.log" \
  || fail 'transient specification inspect was misreported as static drift'

transient_expected_spec="$(cat "$container/spec")"
printf 'transient-inspect-old-spec\n' >"$container/spec"
printf 'persistent transient inspect data\n' >"$state/data/transient-inspect-sentinel"
: >"$FAKE_LOG"
AI_OBSERVER_FAKE_CREATE_INSPECT_FAIL_FORMAT="$spec_inspect_format" \
  run_hook "$test_root" "$dev_home" 'claude codex aiobserver' \
  >"$TMP/transient-inspect-provision.log" 2>&1 \
  || fail 'outer provision did not retry a transient specification inspect failure'
[ "$(cat "$container/spec")" = "$transient_expected_spec" ] \
  || fail 'transient specification inspect rolled back the replacement container'
[ "$(cat "$container/running")" = true ] \
  || fail 'transient specification inspect left the replacement service stopped'
[ "$(cat "$state/data/transient-inspect-sentinel")" = 'persistent transient inspect data' ] \
  || fail 'transient specification inspect damaged persistent data'
! grep -Fq 'previous runtime restored' "$TMP/transient-inspect-provision.log" \
  || fail 'transient specification inspect triggered rollback'
[ -z "$(find "$AI_OBSERVER_FAKE_ROOT/containers" -mindepth 1 -maxdepth 1 \
  ! -name subyard-ai-observer -print -quit)" ] \
  || fail 'transient specification inspect stranded a rollback container'

printf 'persistent outer drift data\n' >"$state/data/outer-drift-sentinel"
printf 'outer-static-old-spec\n' >"$container/spec"
: >"$FAKE_LOG"
SECONDS=0
set +e
AI_OBSERVER_STARTUP_TIMEOUT_SECONDS=10 AI_OBSERVER_FAKE_CREATE_BIND_MODE_DRIFT=1 \
  run_hook "$test_root" "$dev_home" 'claude codex aiobserver' \
  >"$TMP/outer-static-drift.log" 2>&1
outer_static_status=$?
set -e
[ "$outer_static_status" -ne 0 ] \
  || fail 'outer provision accepted create-time mount drift'
[ "$SECONDS" -lt 5 ] \
  || fail 'outer provision waited for startup timeout on static mount drift'
grep -Fxq 'ai-observer-check: container mounts drifted' "$TMP/outer-static-drift.log" \
  || fail 'outer provision omitted the static mount predicate'
! grep -Fq 'startup readiness timed out' "$TMP/outer-static-drift.log" \
  || fail 'outer provision misreported static mount drift as startup timeout'
[ "$(cat "$container/spec")" = outer-static-old-spec ] \
  || fail 'static mount drift did not restore the previous container'
[ "$(cat "$container/running")" = true ] \
  || fail 'static mount drift did not restore the previous running service'
[ "$(cat "$state/data/outer-drift-sentinel")" = 'persistent outer drift data' ] \
  || fail 'static mount drift damaged persistent data'
[ -z "$(find "$AI_OBSERVER_FAKE_ROOT/containers" -mindepth 1 -maxdepth 1 \
  ! -name subyard-ai-observer -print -quit)" ] \
  || fail 'static mount drift stranded a rollback container'
run_hook "$test_root" "$dev_home" 'claude codex aiobserver' >/dev/null

canonical_binds="$(cat "$container/binds")"
canonical_spec="$(cat "$container/spec")"
permuted_binds="[\"$dev_home/.codex/sessions:/sessions/codex:ro\",\"$state/data:/app/data:rw\",\"$TMP/shared/claude:/sessions/claude:ro\"]"
printf '%s\n' "$permuted_binds" >"$container/binds"
"$check" >/dev/null \
  || fail 'readiness check rejected semantically identical permuted mounts'
: >"$FAKE_LOG"
run_hook "$test_root" "$dev_home" 'claude codex aiobserver'
assert_log_absent '^docker (create|rename|rm|stop)'
assert_log_absent '^systemctl (start|restart|stop|disable)'

assert_mount_drift_rejected() {
  local binds="$1" description="$2" output="$TMP/mount-drift.log" status
  printf '%s\n' "$binds" >"$container/binds"
  [ "$(cat "$container/spec")" = "$canonical_spec" ] \
    || fail "$description mount fixture changed the expected-spec marker"
  SECONDS=0
  set +e
  "$check" >"$output" 2>&1
  status=$?
  set -e
  if [ "$status" = 0 ]; then
    fail "readiness check accepted $description mount drift"
  fi
  [ "$status" = 2 ] \
    || fail "$description mount drift returned $status instead of static-drift status 2"
  [ "$SECONDS" -le 2 ] || fail "$description mount drift did not fail fast"
  grep -Fxq 'ai-observer-check: container mounts drifted' "$output" \
    || fail "$description mount drift omitted its exact predicate"
}

assert_mount_drift_rejected \
  "[\"$state/data:/app/data:rw\",\"$TMP/shared/claude:/sessions/claude:ro\"]" \
  missing
assert_mount_drift_rejected \
  "[\"$state/data:/app/data:rw\",\"$TMP/shared/claude:/sessions/claude:ro\",\"$dev_home/.codex/sessions:/sessions/codex:ro\",\"$state/empty:/sessions/extra:ro\"]" \
  extra
assert_mount_drift_rejected \
  "[\"$state/data:/app/data:rw\",\"$TMP/shared/claude:/sessions/claude:rw\",\"$dev_home/.codex/sessions:/sessions/codex:ro\"]" \
  mode
assert_mount_drift_rejected \
  "[\"$state/data:/app/data:rw\",\"$TMP/shared/claude:/sessions/claude:ro\",\"$dev_home/.codex/sessions:/sessions/codex:ro\",\"$dev_home/.codex/sessions:/sessions/codex:ro\"]" \
  duplicate
assert_mount_drift_rejected \
  "[\"$state/other-data:/app/data:rw\",\"$TMP/shared/claude:/sessions/claude:ro\",\"$dev_home/.codex/sessions:/sessions/codex:ro\"]" \
  source
assert_mount_drift_rejected \
  "[\"$state/data:/app/other-data:rw\",\"$TMP/shared/claude:/sessions/claude:ro\",\"$dev_home/.codex/sessions:/sessions/codex:ro\"]" \
  destination
printf '%s\n' "$permuted_binds" >"$container/binds"
[ "$canonical_binds" != "$permuted_binds" ] \
  || fail 'mount permutation fixture did not change Docker inspect order'

SECONDS=0
if AI_OBSERVER_CHECK_TIMEOUT_SECONDS=1 AI_OBSERVER_FAKE_DOCKER_DELAY=0.3 \
  "$check" >"$TMP/check-timeout.log" 2>&1; then
  fail 'delayed readiness check unexpectedly succeeded'
fi
[ "$SECONDS" -le 2 ] || fail 'readiness check exceeded its total bounded invocation'
grep -Fxq 'ai-observer-check: readiness check timed out' "$TMP/check-timeout.log" \
  || fail 'readiness timeout omitted its diagnostic'

: >"$FAKE_LOG"
run_hook "$test_root" "$dev_home" 'claude codex aiobserver'
assert_log_absent '^docker (create|rename|rm|stop)'
assert_log_absent '^systemctl (start|restart|stop|disable)'

spec_without_context="$(cat "$container/spec")"
: >"$FAKE_LOG"
run_hook "$test_root" "$dev_home" 'claude codex aiobserver' \
  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >/dev/null
spec_with_context="$(cat "$container/spec")"
[ "$spec_with_context" != "$spec_without_context" ] \
  || fail 'convergence context did not change the managed container specification'
grep -Eq '^docker (rename|create)' "$FAKE_LOG" \
  || fail 'new convergence context did not recreate the managed container'
: >"$FAKE_LOG"
run_hook "$test_root" "$dev_home" 'claude codex aiobserver' \
  aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >/dev/null
assert_log_absent '^docker (create|rename|rm|stop)'
if run_hook "$test_root" "$dev_home" 'claude codex aiobserver' invalid >/dev/null 2>&1; then
  fail 'invalid convergence context was accepted'
fi

run_hook "$test_root" "$dev_home" 'claude codex aiobserver' >/dev/null

rm -f "$AI_OBSERVER_FAKE_ROOT/active"
: >"$FAKE_LOG"
run_hook "$test_root" "$dev_home" 'claude codex aiobserver' >/dev/null
grep -Eq '^docker stop --time 10 subyard-ai-observer$' "$FAKE_LOG" \
  || fail 'recovery did not stop a container running outside systemd ownership'
[ -f "$AI_OBSERVER_FAKE_ROOT/active" ] || fail 'recovery did not reactivate the unit'

original_spec="$(cat "$container/spec")"
printf 'persistent data\n' >"$state/data/sentinel"
printf '# local drift\n' >>"$unit"
touch "$AI_OBSERVER_FAKE_ROOT/fail-start-once"
: >"$FAKE_LOG"
if run_hook "$test_root" "$dev_home" 'claude codex aiobserver' >/dev/null 2>&1; then
  fail 'file-only reconciliation unexpectedly survived a failed restart'
fi
[ -d "$container" ] || fail 'file-only rollback removed the original container'
[ "$(cat "$container/spec")" = "$original_spec" ] || fail 'file-only rollback replaced the original container'
[ "$(cat "$state/data/sentinel")" = 'persistent data' ] || fail 'file-only rollback damaged persistent data'
assert_log_absent '^docker rm (-f )?subyard-ai-observer$'
run_hook "$test_root" "$dev_home" 'claude codex aiobserver' >/dev/null

printf 'partial-create-old-spec\n' >"$container/spec"
touch "$AI_OBSERVER_FAKE_ROOT/partial-create-once"
if run_hook "$test_root" "$dev_home" 'claude codex aiobserver' >/dev/null 2>&1; then
  fail 'partial Docker create failure unexpectedly succeeded'
fi
[ "$(cat "$container/spec")" = partial-create-old-spec ] \
  || fail 'partial Docker create failure did not restore the previous container'
[ "$(cat "$container/running")" = true ] \
  || fail 'partial Docker create failure did not recover the previous service'
[ -z "$(find "$AI_OBSERVER_FAKE_ROOT/containers" -mindepth 1 -maxdepth 1 ! -name subyard-ai-observer -print -quit)" ] \
  || fail 'partial Docker create failure stranded a rollback container'

printf 'stale-spec\n' >"$container/spec"
touch "$AI_OBSERVER_FAKE_ROOT/fail-start-once"
if run_hook "$test_root" "$dev_home" 'claude codex aiobserver' >/dev/null 2>&1; then
  fail 'replacement unexpectedly survived a failed service start'
fi
[ "$(cat "$container/spec")" = stale-spec ] || fail 'failed replacement did not restore old container'
[ "$(cat "$state/data/sentinel")" = 'persistent data' ] || fail 'failed replacement damaged persistent data'
[ "$(cat "$container/running")" = true ] || fail 'failed replacement did not recover old service'

printf 'tampered\n' >"$container/spec"
if "$check" >/dev/null 2>&1; then fail 'check accepted container configuration drift'; fi
printf 'stale-spec\n' >"$container/spec"

cat >"$TMP/bin/sleep" <<'SH'
#!/usr/bin/env bash
# Keep the failed-readiness retry regression independent of wall-clock waiting.
exit 0
SH
chmod +x "$TMP/bin/sleep"
SECONDS=0
if AI_OBSERVER_FAKE_HTTP_FAIL=1 run_hook "$test_root" "$dev_home" \
  'claude codex aiobserver' >"$TMP/readiness-failure.log" 2>&1; then
  fail 'unhealthy replacement unexpectedly passed readiness'
fi
[ "$SECONDS" -le 5 ] || fail 'unhealthy startup exceeded its total wait budget'
grep -Fq 'startup readiness timed out after 3 seconds' "$TMP/readiness-failure.log" \
  || fail 'failed startup omitted its deadline diagnostic'
grep -Fxq 'ai-observer-check: HTTP readiness failed' "$TMP/readiness-failure.log" \
  || fail 'failed provision omitted the final readiness predicate before rollback'
! grep -Fq 'probe-output-should-stay-private' "$TMP/readiness-failure.log" \
  || fail 'failed provision exposed raw probe output'
[ "$(cat "$container/spec")" = stale-spec ] \
  || fail 'readiness failure did not restore the previous container'
[ "$(cat "$state/data/sentinel")" = 'persistent data' ] \
  || fail 'readiness failure damaged persistent data'

# A healthy first backfill can outlast the old thirty-probe startup window.
printf '35\n' >"$AI_OBSERVER_FAKE_ROOT/http-failures-left"
if ! AI_OBSERVER_STARTUP_TIMEOUT_SECONDS=15 run_hook "$test_root" "$dev_home" \
  'claude codex aiobserver' >"$TMP/slow-startup.log" 2>&1; then
  fail 'slow initial backfill was rolled back before HTTP became ready'
fi
"$check" >/dev/null || fail 'slow initial backfill did not become ready'
[ "$(cat "$state/data/sentinel")" = 'persistent data' ] \
  || fail 'slow initial backfill damaged persistent data'

printf 'foreign\n' >"$container/owner"
: >"$FAKE_LOG"
if "$wrapper" disable >/dev/null 2>&1; then fail 'disable accepted a foreign same-name container'; fi
assert_log_absent '^systemctl disable'
printf 'ai-observer-v1\n' >"$container/owner"
"$wrapper" disable
[ ! -e "$AI_OBSERVER_FAKE_ROOT/enabled" ] || fail 'disable left unit enabled'
[ ! -e "$AI_OBSERVER_FAKE_ROOT/active" ] || fail 'disable left unit active'
[ -d "$container" ] || fail 'disable removed managed container'
[ -f "$state/data/sentinel" ] || fail 'disable removed persistent data'

empty_root="$TMP/empty-root"
empty_home="$TMP/empty-home"
empty_fake="$TMP/empty-fake"
mkdir -p "$empty_root" "$empty_home" "$empty_fake/containers" "$empty_fake/images"
AI_OBSERVER_FAKE_ROOT="$empty_fake" run_hook "$empty_root" "$empty_home" aiobserver >/dev/null
[ ! -e "$empty_home/.claude" ] && [ ! -e "$empty_home/.codex" ] \
  || fail 'unselected integrations caused agent home creation'
empty_state="$empty_root/srv/agents/ai-observer"
[ "$(cat "$empty_fake/containers/subyard-ai-observer/binds")" = "[\"$empty_state/data:/app/data:rw\",\"$empty_state/empty/claude:/sessions/claude:ro\",\"$empty_state/empty/codex:/sessions/codex:ro\"]" ] \
  || fail 'unselected integrations were not isolated behind empty read-only mounts'

foreign_root="$TMP/foreign-root"
foreign_home="$TMP/foreign-home"
foreign_fake="$TMP/foreign-fake"
mkdir -p "$foreign_root" "$foreign_home" "$foreign_fake/containers/subyard-ai-observer" "$foreign_fake/images"
printf 'foreign\n' >"$foreign_fake/containers/subyard-ai-observer/owner"
: >"$FAKE_LOG"
if AI_OBSERVER_FAKE_ROOT="$foreign_fake" run_hook "$foreign_root" "$foreign_home" aiobserver >/dev/null 2>&1; then
  fail 'provision accepted a foreign same-name container'
fi
assert_log_absent '^docker (rename|rm|stop)'

printf 'ok aiobserver provision\n'

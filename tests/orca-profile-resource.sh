#!/usr/bin/env bash
# Host-free contract for the minimal profile-owned Orca lifecycle.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home" SUBYARD_NO_AUDIT=1 PATH="$TMP/bin:$PATH"
export ORCA_TEST_LOG="$TMP/commands.log" ORCA_TEST_ROUTE="$TMP/route"
export ORCA_TEST_CAPTURE="$TMP/capture" ORCA_TEST_GUEST="$TMP/guest"
export ORCA_TEST_STAGE_COUNTER="$TMP/stage-counter"
export ORCA_TEST_PUSH_COUNTER="$TMP/push-counter"
export ORCA_TEST_SERVICE="$TMP/service" ORCA_TEST_INGRESS="$TMP/ingress"
export ORCA_TEST_CLEANUP_FAILED="$TMP/cleanup-failed"
mkdir -p "$HOME" "$TMP/bin" "$ORCA_TEST_CAPTURE" "$ORCA_TEST_GUEST/tmp"
touch "$ORCA_TEST_GUEST/tmp/unrelated"

cat >"$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
log="$state_root/commands.log"
route="$state_root/route"
capture="$state_root/capture"
guest="$state_root/guest"
stage_counter_file="$state_root/stage-counter"
push_counter_file="$state_root/push-counter"
service="$state_root/service"
ingress="$state_root/ingress"
cleanup_failed="$state_root/cleanup-failed"
printf '%s\n' "$*" >> "$log"
case "${1:-}" in
  info) exit 0 ;;
  list) printf 'RUNNING\n' ;;
  file)
    if [ "${2:-}" = push ]; then
      push_counter="$(cat "$push_counter_file" 2>/dev/null || printf 0)"
      push_counter=$((push_counter + 1))
      printf '%s\n' "$push_counter" >"$push_counter_file"
      guest_path="/${4#*/}"
      target="$guest$guest_path"
      if [ -e "$state_root/fail-push" ]; then
        printf 'injected push failure for %s\n' "$guest_path" >&2
        exit 1
      fi
      if [ -e "$target" ]; then
        printf 'Failed to open target file "%s": permission denied\n' "$guest_path" >&2
        exit 1
      fi
      mkdir -p "${target%/*}"
      cp "$3" "$target"
      case "$guest_path" in
        */orca-ingress|*/subyard-orca-ingress) cp "$3" "$capture/orca-ingress" ;;
        */orca-capture-ready|*/subyard-orca-capture-ready)
          cp "$3" "$capture/orca-capture-ready"
          ;;
        */orca-sync|*/subyard-orca-sync) cp "$3" "$capture/orca-sync" ;;
        */subyard-orca.service) cp "$3" "$capture/subyard-orca.service" ;;
      esac
    fi
    ;;
  config)
    case "${2:-} ${3:-}" in
      'device list')
        [ -f "$route" ] && printf 'orca-server\n'
        ;;
      'device get')
        [ -f "$route" ] || exit 1
        case "${6:-}" in
          listen) sed -n '1p' "$route" ;;
          connect) sed -n '2p' "$route" ;;
        esac
        ;;
      'device add')
        [ ! -e "$state_root/fail-route" ] || exit 1
        listen= connect=
        for argument in "$@"; do
          case "$argument" in
            listen=*) listen="${argument#listen=}" ;;
            connect=*) connect="${argument#connect=}" ;;
          esac
        done
        printf '%s\n%s\n' "$listen" "$connect" > "$route"
        ;;
      'device remove') rm -f "$route" ;;
    esac
    ;;
  exec)
    case " $* " in
      *' mktemp -d /tmp/subyard-orca.XXXXXX '*)
        counter="$(cat "$stage_counter_file" 2>/dev/null || printf 0)"
        counter=$((counter + 1))
        printf '%s\n' "$counter" >"$stage_counter_file"
        guest_path="$(printf '/tmp/subyard-orca.%06d' "$counter")"
        mkdir "$guest$guest_path"
        printf '%s\n' "$guest_path"
        ;;
      *' rm -rf -- /tmp/subyard-orca.'*)
        guest_path="${*: -1}"
        case "$guest_path" in
          /tmp/subyard-orca.[0-9][0-9][0-9][0-9][0-9][0-9])
            if [ -e "$state_root/fail-cleanup-once" ] &&
              [ ! -e "$cleanup_failed" ]; then
              touch "$cleanup_failed"
              exit 1
            fi
            rm -rf -- "$guest$guest_path"
            ;;
          *) exit 1 ;;
        esac
        ;;
      *' cmp -s /tmp/subyard-orca.'*)
        source_path="${*: -2:1}"
        target_path="${*: -1}"
        cmp -s "$guest$source_path" "$guest$target_path"
        ;;
      *' install -m '*' /tmp/subyard-orca.'*)
        mode="${*: -3:1}"
        source_path="${*: -2:1}"
        target_path="${*: -1}"
        install -D -m "$mode" \
          "$guest$source_path" "$guest$target_path"
        ;;
      *' rm '*|*' rm -'*)
        printf 'unexpected destructive guest command: %s\n' "$*" >&2
        exit 1
        ;;
      *' systemctl is-active --quiet subyard-orca.service '*)
        [ -f "$service" ]
        ;;
      *' systemctl start subyard-orca.service '*|*' systemctl restart subyard-orca.service '*)
        touch "$service" "$ingress"
        ;;
      *' systemctl disable --now subyard-orca.service '*)
        rm -f "$service" "$ingress"
        ;;
      *' /usr/local/libexec/subyard/orca-ingress down '*)
        rm -f "$ingress"
        ;;
      *' dpkg --print-architecture '*) printf 'amd64\n' ;;
      *' dpkg-query -W '*orca-ide*) printf '1.4.159\n' ;;
      *' nft list chain inet subyard_orca input '*)
        [ -f "$ingress" ] || exit 1
        printf 'chain input { comment "subyard-orca-managed"; }\n'
        ;;
      *' jq -e '*) [ ! -e "$state_root/fail-service-ready" ] ;;
      *' jq -er '*) printf 'orca://pair?code=test-fixture\n' ;;
      *' bash -se -- dev /usr/bin/orca-ide /srv/agents/orca ')
        if [ -e "$state_root/project-counts-fail" ]; then
          exit 1
        elif [ -e "$state_root/project-counts-drift" ]; then
          printf '1 2\n'
        else
          printf '2 2\n'
        fi
        ;;
    esac
    ;;
esac
MOCK

cat >"$TMP/bin/tailscale" <<'MOCK'
#!/usr/bin/env bash
[ "${1:-} ${2:-}" = 'ip -4' ] && printf '100.64.1.20\n'
MOCK

cat >"$TMP/bin/getent" <<'MOCK'
#!/usr/bin/env bash
[ "${1:-}" = ahostsv4 ] && printf '100.64.1.20 STREAM %s\n' "${2:-}"
MOCK

cat >"$TMP/bin/ip" <<'MOCK'
#!/usr/bin/env bash
printf 'tailscale0 UP 100.64.1.20/32\n'
MOCK

cat >"$TMP/bin/ss" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK

cat >"$TMP/bin/curl" <<'MOCK'
#!/usr/bin/env bash
state_root="$(cd "$(dirname "$0")/.." && pwd)"
printf 'curl %s\n' "$*" >> "$state_root/commands.log"
MOCK

cat >"$TMP/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod 0755 "$TMP/bin/"*

run_orca() {
  ORCA_ADVERTISE_HOST="${ORCA_TEST_ADVERTISE:-owner.example-tailnet.ts.net}" \
    ORCA_HOST_PORT=17678 "$ROOT/bin/yard" orca "$@"
}

assert_down() {
  [ ! -e "$ORCA_TEST_ROUTE" ] || fail "$1 left the owned route attached"
  [ ! -e "$ORCA_TEST_SERVICE" ] || fail "$1 left the candidate service active"
  [ ! -e "$ORCA_TEST_INGRESS" ] || fail "$1 left the ingress guard active"
}

assert_guest_staging_clean() {
  if find "$ORCA_TEST_GUEST/tmp" -mindepth 1 -maxdepth 1 -type d \
    -name 'subyard-orca.*' | grep -q .; then
    fail "$1 left a guest staging directory"
  fi
  [ -f "$ORCA_TEST_GUEST/tmp/unrelated" ] \
    || fail "$1 removed an unrelated temporary file"
}

count_log() {
  grep -Fc -- "$1" "$ORCA_TEST_LOG" || true
}

run_orca up --yes >"$TMP/up.out"
grep -Fxq 'tcp:100.64.1.20:17678' "$ORCA_TEST_ROUTE" \
  || fail 'Tailscale mode did not bind the exact owner address'
grep -Fxq 'tcp:127.0.0.1:6768' "$ORCA_TEST_ROUTE" \
  || fail 'owner route did not target yard loopback'
grep -Fq 'ExecStart=/usr/local/libexec/subyard/orca-capture-ready' \
  "$ORCA_TEST_CAPTURE/subyard-orca.service" \
  || fail 'service does not isolate readiness JSON'
grep -Fq 'ExecStart=/usr/local/libexec/subyard/orca-capture-ready /srv/agents/orca/ready.json /usr/bin/orca-ide serve' \
  "$ORCA_TEST_CAPTURE/subyard-orca.service" \
  || fail 'service does not use the packaged Orca CLI'
grep -Fq -- '--pairing-address owner.example-tailnet.ts.net:17678 --json' \
  "$ORCA_TEST_CAPTURE/subyard-orca.service" \
  || fail 'service does not advertise the owner endpoint'
cmp -s "$ORCA_TEST_CAPTURE/subyard-orca.service" \
  "$ORCA_TEST_GUEST/etc/systemd/system/subyard-orca.service" \
  || fail 'service candidate was not installed at the canonical guest path'
if grep -Fq -- '--no-pairing' "$ORCA_TEST_CAPTURE/subyard-orca.service"; then
  fail 'service disabled stock startup pairing'
fi
grep -Fq '.result.repos[]?' "$ORCA_TEST_CAPTURE/orca-sync" \
  || fail 'project hook does not use the stock repo list contract'
grep -Fq 'repo add --path "$checkout" --json' "$ORCA_TEST_CAPTURE/orca-sync" \
  || fail 'project hook does not add canonical roots'
grep -Fq '<<<"$add_result"' "$ORCA_TEST_CAPTURE/orca-sync" \
  || fail 'project hook does not validate the stock repo add result'
if grep -Eqi 'nodejs|npm|AppImage|squashfs|APPDIR|SHA512' \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" "$ROOT/config/profiles/orca/release.env"; then
  fail 'removed SSH/AppImage dependencies returned'
fi

restart_count="$(count_log 'systemctl restart subyard-orca.service')"
run_orca up --yes >/dev/null
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq "$restart_count" ] \
  || fail 'identical up restarted an unchanged runtime'
if grep -Fq 'apt-get install -y -qq /tmp/subyard-orca' "$ORCA_TEST_LOG"; then
  fail 'repeated up reinstalled the already verified Orca release'
fi

pairing_field_read_count="$(count_log '.pairing.url')"
ENVIRONMENT_PROFILES=orca run_orca status >"$TMP/status.out"
grep -Fq 'Orca profile selected for yard init' "$TMP/status.out" \
  || fail 'status did not confirm the selected Orca profile'
grep -Fq 'automatic project hook ready' "$TMP/status.out" \
  || fail 'status did not report automatic project hook readiness'
grep -Fq 'projects registered: 2/2' "$TMP/status.out" \
  || fail 'status did not report bounded project registration counts'
if grep -Fq 'orca://pair?' "$TMP/status.out"; then
  fail 'status exposed a pairing capability'
fi
ENVIRONMENT_PROFILES='' run_orca status >"$TMP/status-unselected.out"
grep -Fq 'Orca profile is not selected in ENVIRONMENT_PROFILES' "$TMP/status-unselected.out" \
  || fail 'status did not warn that yard init will omit the Orca profile'
[ "$(count_log '.pairing.url')" -eq "$pairing_field_read_count" ] \
  || fail 'status read a pairing capability from readiness state'
grep -Fq 'for hook in /usr/local/libexec/subyard/projects-changed.d/*; do' \
  "$ORCA_TEST_LOG" \
  || fail 'status did not verify automatic project hook wiring'
touch "$TMP/project-counts-fail"
ENVIRONMENT_PROFILES=orca run_orca status >"$TMP/status-counts-fail.out"
grep -Fq 'project registration counts unavailable' "$TMP/status-counts-fail.out" \
  || fail 'status did not report unavailable project registration counts'
if grep -Fq 'while service is not ready' "$TMP/status-counts-fail.out"; then
  fail 'status blamed service readiness for an independent project count failure'
fi
rm -f "$TMP/project-counts-fail"

restart_count="$(count_log 'systemctl restart subyard-orca.service')"
run_orca restart --yes >"$TMP/restart.out"
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq $((restart_count + 1)) ] \
  || fail 'restart did not restart the existing Orca service'
grep -Fq 'Orca service restarted' "$TMP/restart.out" \
  || fail 'restart did not confirm recovery'
if grep -Fq 'orca://pair?' "$TMP/restart.out"; then
  fail 'restart exposed a pairing capability'
fi
[ "$(count_log '.pairing.url')" -eq "$pairing_field_read_count" ] \
  || fail 'restart read a pairing capability from readiness state'

run_orca logs >"$TMP/logs.out"
grep -Fq 'journalctl --no-pager -u subyard-orca.service -n 18000' "$ORCA_TEST_LOG" \
  || fail 'logs did not bound journal output to the latest 18000 lines'
run_orca logs --follow >"$TMP/logs-follow.out"
grep -Fq 'journalctl --no-pager -u subyard-orca.service -n 18000 --follow' \
  "$ORCA_TEST_LOG" \
  || fail 'logs --follow did not request bounded history followed by live output'
if run_orca logs --tail >"$TMP/logs-invalid.out" 2>&1; then
  fail 'logs accepted an unsupported option'
fi

touch "$TMP/project-counts-drift"
if run_orca pair --yes >"$TMP/pair-drift.out" 2>&1; then
  fail 'pair returned a link while canonical project registration was incomplete'
fi
if grep -Fq 'orca://pair?' "$TMP/pair-drift.out"; then
  fail 'pair exposed a capability before project registration converged'
fi
rm -f "$TMP/project-counts-drift"

sync_count="$(count_log 'runuser -u dev -- /usr/local/libexec/subyard/projects-changed.d/orca')"
pairing="$(run_orca pair --yes | tail -n1)"
[ "$pairing" = 'orca://pair?code=test-fixture' ] \
  || fail 'pair did not return only the stock startup link'
grep -Fq 'systemctl restart subyard-orca.service' "$ORCA_TEST_LOG" \
  || fail 'pair did not mint a fresh startup offer'
[ "$(count_log 'runuser -u dev -- /usr/local/libexec/subyard/projects-changed.d/orca')" \
  -eq $((sync_count + 1)) ] \
  || fail 'pair did not reconcile canonical project roots before returning the link'
run_orca sync >/dev/null
grep -Fq 'runuser -u dev -- /usr/local/libexec/subyard/projects-changed.d/orca' "$ORCA_TEST_LOG" \
  || fail 'sync did not invoke the resource-owned project hook as dev'

mkdir -p "$ORCA_TEST_GUEST/srv/agents/orca/state"
touch "$ORCA_TEST_GUEST/srv/agents/orca/state/persisted-grant"
run_orca down --yes >/dev/null
assert_down 'down'
grep -Fq 'systemctl disable --now subyard-orca.service' "$ORCA_TEST_LOG" \
  || fail 'down did not stop the profile-owned service'

ORCA_TEST_ADVERTISE=127.0.0.1 run_orca up --yes >/dev/null
grep -Fxq 'tcp:127.0.0.1:17678' "$ORCA_TEST_ROUTE" \
  || fail 'SSH mode was not limited to owner loopback'
[ -f "$ORCA_TEST_GUEST/srv/agents/orca/state/persisted-grant" ] \
  || fail 'down/up removed persistent Orca state'
assert_guest_staging_clean 'successful up'

pairing="$(ORCA_TEST_ADVERTISE=127.0.0.1 run_orca pair --yes | tail -n1)"
[ "$pairing" = 'orca://pair?code=test-fixture' ] \
  || fail 'repeated pair did not return the stock startup link'

run_orca down --yes >/dev/null
assert_down 'second down'

touch "$TMP/fail-cleanup-once"
if ORCA_TEST_ADVERTISE=127.0.0.1 run_orca up --yes >"$TMP/cleanup-failure.out" 2>&1; then
  fail 'guest staging cleanup failure was accepted'
fi
rm -f "$TMP/fail-cleanup-once"
assert_down 'cleanup failure'
assert_guest_staging_clean 'cleanup failure retry'

touch "$TMP/fail-push"
if ORCA_TEST_ADVERTISE=127.0.0.1 run_orca up --yes >"$TMP/push-failure.out" 2>&1; then
  fail 'injected guest push failure was accepted'
fi
rm -f "$TMP/fail-push"
assert_down 'push failure'
assert_guest_staging_clean 'push failure'

touch "$TMP/fail-route"
if ORCA_TEST_ADVERTISE=127.0.0.1 run_orca up --yes >"$TMP/route-failure.out" 2>&1; then
  fail 'injected owner route failure was accepted'
fi
rm -f "$TMP/fail-route"
assert_down 'route failure rollback'
assert_guest_staging_clean 'route failure rollback'

touch "$TMP/fail-service-ready"
if ORCA_TEST_ADVERTISE=127.0.0.1 run_orca up --yes >"$TMP/readiness-failure.out" 2>&1; then
  fail 'injected service readiness failure was accepted'
fi
rm -f "$TMP/fail-service-ready"
assert_down 'readiness failure rollback'
assert_guest_staging_clean 'readiness failure rollback'

if ORCA_ADVERTISE_HOST='https://bad/path' ORCA_HOST_PORT=17678 \
  "$ROOT/bin/yard" orca up --yes >"$TMP/invalid.out" 2>&1; then
  fail 'unsafe advertised hostname was accepted'
fi
grep -Fq 'without scheme, path or port' "$TMP/invalid.out" \
  || fail 'unsafe hostname failure was not actionable'

grep -Fq 'projects-changed.d/*' "$ROOT/scripts/04-provision-subyard.sh" \
  || fail 'project lifecycle dispatcher does not include shared-resource hooks'

printf 'ok: Orca repeatably stages, pairs, rolls back and preserves state across exact routes\n'

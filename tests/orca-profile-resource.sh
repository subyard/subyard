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
printf '%s\n' "$*" >> "$ORCA_TEST_LOG"
case "${1:-}" in
  info) exit 0 ;;
  list) printf 'RUNNING\n' ;;
  file)
    if [ "${2:-}" = push ]; then
      push_counter="$(cat "$ORCA_TEST_PUSH_COUNTER" 2>/dev/null || printf 0)"
      push_counter=$((push_counter + 1))
      printf '%s\n' "$push_counter" >"$ORCA_TEST_PUSH_COUNTER"
      guest_path="/${4#*/}"
      target="$ORCA_TEST_GUEST$guest_path"
      if [ "${ORCA_TEST_FAIL_PUSH:-0}" = 1 ]; then
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
        */orca-ingress|*/subyard-orca-ingress) cp "$3" "$ORCA_TEST_CAPTURE/orca-ingress" ;;
        */orca-capture-ready|*/subyard-orca-capture-ready)
          cp "$3" "$ORCA_TEST_CAPTURE/orca-capture-ready"
          ;;
        */orca-sync|*/subyard-orca-sync) cp "$3" "$ORCA_TEST_CAPTURE/orca-sync" ;;
        */subyard-orca.service) cp "$3" "$ORCA_TEST_CAPTURE/subyard-orca.service" ;;
      esac
    fi
    ;;
  config)
    case "${2:-} ${3:-}" in
      'device list')
        [ -f "$ORCA_TEST_ROUTE" ] && printf 'orca-server\n'
        ;;
      'device get')
        [ -f "$ORCA_TEST_ROUTE" ] || exit 1
        case "${6:-}" in
          listen) sed -n '1p' "$ORCA_TEST_ROUTE" ;;
          connect) sed -n '2p' "$ORCA_TEST_ROUTE" ;;
        esac
        ;;
      'device add')
        [ "${ORCA_TEST_FAIL_ROUTE:-0}" != 1 ] || exit 1
        listen= connect=
        for argument in "$@"; do
          case "$argument" in
            listen=*) listen="${argument#listen=}" ;;
            connect=*) connect="${argument#connect=}" ;;
          esac
        done
        printf '%s\n%s\n' "$listen" "$connect" > "$ORCA_TEST_ROUTE"
        ;;
      'device remove') rm -f "$ORCA_TEST_ROUTE" ;;
    esac
    ;;
  exec)
    case " $* " in
      *' mktemp -d /tmp/subyard-orca.XXXXXX '*)
        counter="$(cat "$ORCA_TEST_STAGE_COUNTER" 2>/dev/null || printf 0)"
        counter=$((counter + 1))
        printf '%s\n' "$counter" >"$ORCA_TEST_STAGE_COUNTER"
        guest_path="$(printf '/tmp/subyard-orca.%06d' "$counter")"
        mkdir "$ORCA_TEST_GUEST$guest_path"
        printf '%s\n' "$guest_path"
        ;;
      *' rm -rf -- /tmp/subyard-orca.'*)
        guest_path="${*: -1}"
        case "$guest_path" in
          /tmp/subyard-orca.[0-9][0-9][0-9][0-9][0-9][0-9])
            if [ "${ORCA_TEST_FAIL_CLEANUP_ONCE:-0}" = 1 ] &&
              [ ! -e "$ORCA_TEST_CLEANUP_FAILED" ]; then
              touch "$ORCA_TEST_CLEANUP_FAILED"
              exit 1
            fi
            rm -rf -- "$ORCA_TEST_GUEST$guest_path"
            ;;
          *) exit 1 ;;
        esac
        ;;
      *' cmp -s /tmp/subyard-orca.'*)
        source_path="${*: -2:1}"
        target_path="${*: -1}"
        cmp -s "$ORCA_TEST_GUEST$source_path" "$ORCA_TEST_GUEST$target_path"
        ;;
      *' install -m '*' /tmp/subyard-orca.'*)
        mode="${*: -3:1}"
        source_path="${*: -2:1}"
        target_path="${*: -1}"
        install -D -m "$mode" \
          "$ORCA_TEST_GUEST$source_path" "$ORCA_TEST_GUEST$target_path"
        ;;
      *' rm '*|*' rm -'*)
        printf 'unexpected destructive guest command: %s\n' "$*" >&2
        exit 1
        ;;
      *' systemctl is-active --quiet subyard-orca.service '*)
        [ -f "$ORCA_TEST_SERVICE" ]
        ;;
      *' systemctl start subyard-orca.service '*|*' systemctl restart subyard-orca.service '*)
        touch "$ORCA_TEST_SERVICE" "$ORCA_TEST_INGRESS"
        ;;
      *' systemctl disable --now subyard-orca.service '*)
        rm -f "$ORCA_TEST_SERVICE" "$ORCA_TEST_INGRESS"
        ;;
      *' /usr/local/libexec/subyard/orca-ingress down '*)
        rm -f "$ORCA_TEST_INGRESS"
        ;;
      *' dpkg --print-architecture '*) printf 'amd64\n' ;;
      *' dpkg-query -W '*orca-ide*) printf '1.4.159\n' ;;
      *' nft list chain inet subyard_orca input '*)
        [ -f "$ORCA_TEST_INGRESS" ] || exit 1
        printf 'chain input { comment "subyard-orca-managed"; }\n'
        ;;
      *' jq -e '*) [ "${ORCA_TEST_FAIL_SERVICE_READY:-0}" != 1 ] ;;
      *' jq -er '*) printf 'orca://pair?code=test-fixture\n' ;;
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
printf 'curl %s\n' "$*" >> "$ORCA_TEST_LOG"
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

pairing="$(run_orca pair --yes | tail -n1)"
[ "$pairing" = 'orca://pair?code=test-fixture' ] \
  || fail 'pair did not return only the stock startup link'
grep -Fq 'systemctl restart subyard-orca.service' "$ORCA_TEST_LOG" \
  || fail 'pair did not mint a fresh startup offer'
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

if ORCA_TEST_ADVERTISE=127.0.0.1 ORCA_TEST_FAIL_CLEANUP_ONCE=1 \
  run_orca up --yes >"$TMP/cleanup-failure.out" 2>&1; then
  fail 'guest staging cleanup failure was accepted'
fi
assert_down 'cleanup failure'
assert_guest_staging_clean 'cleanup failure retry'

if ORCA_TEST_ADVERTISE=127.0.0.1 ORCA_TEST_FAIL_PUSH=1 \
  run_orca up --yes >"$TMP/push-failure.out" 2>&1; then
  fail 'injected guest push failure was accepted'
fi
assert_down 'push failure'
assert_guest_staging_clean 'push failure'

if ORCA_TEST_ADVERTISE=127.0.0.1 ORCA_TEST_FAIL_ROUTE=1 \
  run_orca up --yes >"$TMP/route-failure.out" 2>&1; then
  fail 'injected owner route failure was accepted'
fi
assert_down 'route failure rollback'
assert_guest_staging_clean 'route failure rollback'

if ORCA_TEST_ADVERTISE=127.0.0.1 ORCA_TEST_FAIL_SERVICE_READY=1 \
  run_orca up --yes >"$TMP/readiness-failure.out" 2>&1; then
  fail 'injected service readiness failure was accepted'
fi
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

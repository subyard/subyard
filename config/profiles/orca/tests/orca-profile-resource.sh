#!/usr/bin/env bash
# Host-free contract for the minimal profile-owned Orca lifecycle.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
if [ "${1:-}" = --prepare-parser-fixture ] || [ "${1:-}" = --startup-binding-fixture ]; then
  export INCUS_PROJECT=subyard-orca-bootstrap-5b9fe2b7ea30
  export YARD_INSTANCE_NAME=yard-orca-bootstrap-5b9fe2b7ea30
fi
# shellcheck source=config/profiles/orca/release.env
. "$ROOT/config/profiles/orca/release.env"
export ORCA_TEST_VERSION="$ORCA_VERSION"
printf 'owner-host\n' >"$SUBYARD_CONFIG_HOME/host-id"
chmod 0600 "$SUBYARD_CONFIG_HOME/host-id"
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
  info) [ ! -e "$state_root/missing-yard" ] ;;
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
          type) printf 'proxy\n' ;;
          bind) printf 'host\n' ;;
          listen) sed -n '1p' "$route" ;;
          connect) sed -n '2p' "$route" ;;
        esac
        ;;
      'device show')
        if [ -f "$route" ]; then
          printf 'orca-server:\n  type: proxy\n  bind: host\n  listen: %s\n  connect: %s\n' \
            "$(sed -n '1p' "$route")" "$(sed -n '2p' "$route")"
        fi
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
    if [ -e "$state_root/drain-exec-stdin" ]; then
      # Native Incus forwards stdin even when the guest command does not read it.
      # Explicit script execution keeps its intended script input.
      case " $* " in *' bash -s'*) ;; *) cat >/dev/null ;; esac
    fi
    case " $* " in
      *' bash -se -- '*'orca-registration.sha256'*)
        arguments=("$@")
        command=()
        for index in "${!arguments[@]}"; do
          [ "${arguments[$index]}" = -- ] && command=("${arguments[@]:$((index + 1))}")
        done
        mapped=()
        if [[ "${command[0]}" = /tmp/subyard-orca.* ]]; then
          for index in 0 1 2 3 4; do mapped+=("$guest${command[$index]}"); done
          mapped+=("${command[5]}" "$guest${command[6]}" "$guest${command[7]}")
        else
          mapped+=("${command[0]}")
          for index in 1 2 3 4 5 6 7 8; do mapped+=("$guest${command[$index]}"); done
          mapped+=("${command[@]:9}")
        fi
        export ORCA_TEST_GUEST_ROOT="$guest" ORCA_TEST_STATE_ROOT="$state_root"
        stat() {
          if [ "${1:-}" = -c ] && { [ "${2:-}" = %u ] || [ "${2:-}" = %g ]; }; then
            printf '0\n'
          else
            command stat "$@"
          fi
        }
        chown() { :; }
        mv() {
          local target="${!#}"
          if [ -e "$ORCA_TEST_STATE_ROOT/fail-registration-marker" ] &&
            [[ "$target" = "$ORCA_TEST_GUEST_ROOT/usr/local/libexec/subyard/orca-registration.sha256" ]]; then
            return 1
          fi
          command mv "$@"
        }
        systemctl() {
          if [ "${1:-}" = is-enabled ] && [[ "${*: -1}" = *subyard-orca-discovery.timer ]]; then
            [ -f "$ORCA_TEST_STATE_ROOT/discovery-enabled" ]
          else return 0; fi
        }
        export -f stat chown mv systemctl
        /bin/bash -se -- "${mapped[@]}"
        ;;
      *' test -x /usr/local/libexec/subyard/projects-changed '*)
        [ ! -e "$state_root/missing-dispatcher" ]
        ;;
      *' runuser -u dev -- /usr/local/libexec/subyard/projects-changed.d/orca '*|*' runuser -u dev -- env SUBYARD_ORCA_REGISTRATION_SCOPE='*' /usr/local/libexec/subyard/projects-changed.d/orca '*)
        [ ! -e "$state_root/project-sync-fail" ] || exit 1
        rm -f "$state_root/codex-defaults-drift"
        ;;
      *' runuser -u dev -- bash -c '*)
        arguments=("$@")
        for index in "${!arguments[@]}"; do
          if [ "${arguments[$index]}" = bash ] && [ "${arguments[$((index + 1))]:-}" = -c ]; then
            script="${arguments[$((index + 2))]}"
            target="$guest${arguments[$((index + 4))]}"
            operation="${arguments[$((index + 5))]:-}"
            mkdir -p "${target%/*}"
            case "$script" in
              *mktemp*'ln -T'*)
                [ ! -e "$state_root/fail-mobile-request" ] || exit 1
                bash -c "$script" _ "$target" "$operation"
                [ ! -e "$state_root/signal-mobile-request" ] || kill -TERM "$PPID"
                [ ! -e "$state_root/fail-after-mobile-request" ] || exit 1
                ;;
              *) bash -c "$script" _ "$target" "$operation" ;;
            esac
            exit
          fi
        done
        exit 1
        ;;
      *' runuser -u dev -- rm -f -- /srv/agents/orca/ready.json.mobile-request '*)
        rm -f "$guest${*: -1}"
        ;;
      *' /usr/bin/python3 -B /usr/local/libexec/subyard/orca-registration/settings.py --check '*)
        [ ! -e "$state_root/codex-defaults-drift" ]
        ;;
      *' /usr/bin/python3 -B /usr/local/libexec/subyard/orca-registration/settings.py '*) rm -f "$state_root/codex-defaults-drift" ;;
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
      *' systemctl show -p InvocationID --value subyard-orca.service '*)
        printf '%032x\n' "$(cat "$state_root/invocation" 2>/dev/null || printf 1)"
        ;;
      *' systemctl is-active --quiet subyard-orca.service '*)
        [ -f "$service" ]
        ;;
      *' systemctl enable --now subyard-orca-discovery.timer '*)
        touch "$state_root/discovery-enabled" "$state_root/discovery-active"
        ;;
      *' systemctl is-enabled --quiet subyard-orca-discovery.timer '*)
        [ -f "$state_root/discovery-enabled" ]
        ;;
      *' systemctl is-active --quiet subyard-orca-discovery.timer '*)
        [ -f "$state_root/discovery-active" ]
        ;;
      *' systemctl is-active --quiet subyard-orca-discovery.service '*) exit 1 ;;
      *' systemctl start subyard-orca-discovery.timer '*) touch "$state_root/discovery-active" ;;
      *' systemctl stop subyard-orca-discovery.timer '*) rm -f "$state_root/discovery-active" ;;
      *' systemctl start subyard-orca.service '*|*' systemctl restart subyard-orca.service '*)
        [ ! -e "$state_root/fail-restart" ] || exit 1
        invocation="$(cat "$state_root/invocation" 2>/dev/null || printf 1)"
        printf '%s\n' "$((invocation+1))" > "$state_root/invocation"
        touch "$service" "$ingress"
        ready="$guest/srv/agents/orca/ready.json"
        mkdir -p "${ready%/*}"
        "$guest/usr/local/libexec/subyard/orca-capture-ready" "$ready" \
          "$state_root/bin/orca-ide" serve --port 6768 --pairing-address fixture:17678 --json
        ;;
      *' systemctl disable --now subyard-orca.service '*)
        rm -f "$service" "$ingress"
        ;;
      *' /usr/local/libexec/subyard/orca-ingress down '*)
        rm -f "$ingress"
        ;;
      *' dpkg --print-architecture '*) printf 'amd64\n' ;;
      *' dpkg-query -W '*orca-ide*) printf '%s\n' "$ORCA_TEST_VERSION" ;;
      *' nft list chain inet subyard_orca input '*)
        nft list chain inet subyard_orca input
        ;;
      *' jq -e '*|*' jq -er '*)
        [ ! -e "$state_root/fail-service-ready" ] || exit 1
        arguments=("$@")
        command=()
        for index in "${!arguments[@]}"; do
          [ "${arguments[$index]}" = -- ] && command=("${arguments[@]:$((index + 1))}")
        done
        for index in "${!command[@]}"; do
          case "${command[$index]}" in
            /srv/agents/orca/*) command[$index]="$guest${command[$index]}" ;;
          esac
        done
        "${command[@]}"
        ;;
      *' /usr/bin/python3 -B /usr/local/libexec/subyard/orca-registration/main.py status --host-name owner-host ')
        if [ -e "$state_root/project-counts-fail" ]; then
          exit 1
        elif [ -e "$state_root/project-counts-drift" ]; then
          printf '%s\n' '{"ready":false,"registered":1,"total":2,"errors":["nested checkout has the wrong group"],"warnings":[],"scopeDigest":"0000000000000000000000000000000000000000000000000000000000000001","catalogDigest":"0000000000000000000000000000000000000000000000000000000000000002"}'
          exit 1
        else
          printf '%s\n' '{"ready":true,"registered":2,"total":2,"errors":[],"warnings":[],"scopeDigest":"0000000000000000000000000000000000000000000000000000000000000001","catalogDigest":"0000000000000000000000000000000000000000000000000000000000000002"}'
        fi
        ;;
    esac
    ;;
esac
MOCK

cat >"$TMP/bin/nft" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
printf 'nft %s\n' "$*" >>"$state_root/commands.log"
case "${1:-} ${2:-}" in
  'list table') [ -f "$state_root/ingress" ] ;;
  'list chain')
    [ -f "$state_root/ingress" ] || exit 1
    mode="$(cat "$state_root/nft-mode" 2>/dev/null || printf owned)"
    # A finite stream larger than the pipe buffer exposes early-closing readers.
    python3 - "$mode" <<'PY'
import os
import signal
import sys

signal.signal(signal.SIGPIPE, signal.SIG_DFL)
marker = "foreign" if sys.argv[1] == "foreign" else "subyard-orca-managed"
os.write(1, f'chain input {{ comment "{marker}"; }}\n'.encode())
for _ in range(64):
    os.write(1, b"# trailing rules\n" * 4096)
PY
    [ "$mode" != error ] || exit 42
    ;;
  'delete table') rm -f "$state_root/ingress" ;;
  'add table') touch "$state_root/ingress" ;;
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

cat >"$TMP/bin/orca-ide" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state_root="$(cd "$(dirname "$0")/.." && pwd)"
mobile=0
for argument in "$@"; do
  [ "$argument" = --mobile-pairing ] && mobile=$((mobile + 1))
done
printf 'orca-ide mobile=%s args=%s\n' "$mobile" "$*" >> "$state_root/commands.log"
[ ! -e "$state_root/fail-orca-cli" ] || exit 1
if [ -e "$state_root/unready-orca" ]; then
  printf '%s\n' '{}'
  exit 0
fi
scope=runtime
[ "$mobile" -eq 0 ] || scope=mobile
[ ! -e "$state_root/wrong-pairing-scope" ] || scope=runtime
printf '{"type":"orca_server_ready","schemaVersion":1,"pairing":{"available":true,"scope":"%s","url":"orca://pair?code=%s-fixture"}}\n' \
  "$scope" "$scope"
MOCK

cat >"$TMP/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod 0755 "$TMP/bin/"*

run_orca() {
  # Exercise the physical adapter contract in isolation. Native CLI bootstrap,
  # confirmation and init composition have separate CLI and real-host coverage.
  local verb="$1" assessment action argument
  shift
  local -a arguments=()
  for argument in "$@"; do
    [ "$argument" = --yes ] || arguments+=("$argument")
  done
  assessment="$(ORCA_ADVERTISE_HOST="${ORCA_TEST_ADVERTISE:-owner.example-tailnet.ts.net}" \
    ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" "$verb" "${arguments[@]}")" || return
  action="$(jq -er '.action' <<<"$assessment")" || return
  ORCA_ADVERTISE_HOST="${ORCA_TEST_ADVERTISE:-owner.example-tailnet.ts.net}" \
    ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION="$action" \
    SUBYARD_RESOURCE_BINDING="$(jq -r '.binding // ""' <<<"$assessment")" \
    SUBYARD_RESOURCE_STEPS="$(jq -c 'if .steps then .steps else empty end' <<<"$assessment")" \
    SUBYARD_OPERATION_ID=orca-handler-test \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" "$verb" "${arguments[@]}" || return
  if [ "$(jq -r .schema <<<"$assessment")" = yard.resource-action-assessment.v2 ]; then
    local verified
    verified="$(ORCA_ADVERTISE_HOST="${ORCA_TEST_ADVERTISE:-owner.example-tailnet.ts.net}" ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=verify SUBYARD_RESOURCE_BINDING="$(jq -r .binding <<<"$assessment")" SUBYARD_RESOURCE_STEPS="$(jq -c .steps <<<"$assessment")" "$ROOT/config/profiles/orca/resources/orca/handler.sh" "$verb" "${arguments[@]}")" || return
    jq -e '.changed == false and all(.steps[]; .observed == .desired and .decision == "skip")' <<<"$verified" >/dev/null \
      || fail 'native Orca verifier did not prove stopped/absent exact targets'
    [ "$(jq -r .binding <<<"$assessment")" = "$(jq -r .binding <<<"$verified")" ] \
      || fail 'native Orca verifier changed desired/target scope'
  fi
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

touch "$TMP/missing-yard"
prepare_mode=prepare
[ "${1:-}" != --startup-binding-fixture ] || prepare_mode=prepare-start
ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE="$prepare_mode" \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" up >"$TMP/bootstrap-plan.json" \
  || fail 'first bring-up cannot be assessed before the yard exists'
jq -e '.action == "up" and .changed == true' "$TMP/bootstrap-plan.json" >/dev/null \
  || fail 'absent yard did not produce an up assessment'
[ ! -e "$ORCA_TEST_SERVICE" ] || fail 'bootstrap assessment started the service'
rm -f "$TMP/missing-yard"
if [ "${1:-}" = --startup-binding-fixture ]; then
  cp "$TMP/bootstrap-plan.json" "$2/cold.json"
  touch "$TMP/drain-exec-stdin"
  export ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT=17678
  SUBYARD_RESOURCE_BINDING="$(jq -r .binding "$2/cold.json")"
  SUBYARD_RESOURCE_STEPS="$(jq -c .steps "$2/cold.json")"
  export SUBYARD_RESOURCE_BINDING SUBYARD_RESOURCE_STEPS
  SUBYARD_RESOURCE_MODE=prepare "$ROOT/config/profiles/orca/resources/orca/handler.sh" up > "$2/running.json"
  SUBYARD_RESOURCE_MODE=prepare "$3" up > "$2/relocated.json"
  SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=orca-startup-fixture \
    "$3" up > "$TMP/startup-output"
  SUBYARD_RESOURCE_MODE=verify "$3" up > "$2/verified.json"
  stages="$(count_log 'mktemp -d /tmp/subyard-orca.')"
  if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=orca-startup-fixture \
    "$4" up > "$TMP/source-drift-output" 2>&1; then fail 'changed native source bytes accepted'; fi
  grep -Fq 'plan_stale: Orca native desired scope changed' "$TMP/source-drift-output" \
    || fail 'native source byte guard did not reject before effects'
  if ORCA_HOST_PORT=17679 SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=orca-startup-fixture \
    "$3" up > "$TMP/target-drift-output" 2>&1; then fail 'changed native desired endpoint accepted'; fi
  grep -Fq 'plan_stale: Orca native desired scope changed' "$TMP/target-drift-output" \
    || fail 'native desired endpoint guard did not reject before effects'
  [ "$stages" = "$(count_log 'mktemp -d /tmp/subyard-orca.')" ] || fail 'stale native input started target staging'
  exit 0
fi
if [ "${1:-}" = --prepare-parser-fixture ]; then
  cp "$TMP/bootstrap-plan.json" "$2/up.json"
  ORCA_TEST_ADVERTISE=127.0.0.1 run_orca up --yes > "$TMP/up-output"
  printf '3735928559\n' > "$TMP/invocation"
  SUBYARD_RESOURCE_MODE=prepare "$ROOT/config/profiles/orca/resources/orca/handler.sh" restart > "$2/restart.json"
  printf 'tcp:127.0.0.1:17678\ntcp:127.0.0.1:6768\n' > "$ORCA_TEST_ROUTE"
  SUBYARD_RESOURCE_MODE=prepare "$ROOT/config/profiles/orca/resources/orca/handler.sh" down > "$2/down.json"
  SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=restart SUBYARD_OPERATION_ID=orca-parser-fixture \
    SUBYARD_RESOURCE_BINDING="$(jq -r .binding "$2/restart.json")" \
    SUBYARD_RESOURCE_STEPS="$(jq -c .steps "$2/restart.json")" \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" restart > "$TMP/restart-output"
  SUBYARD_RESOURCE_MODE=verify SUBYARD_RESOURCE_STEPS="$(jq -c .steps "$2/restart.json")" \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" restart > "$2/restart-verified.json"
  touch "$TMP/codex-defaults-drift"
  SUBYARD_RESOURCE_MODE=prepare "$ROOT/config/profiles/orca/resources/orca/handler.sh" sync > "$2/sync.json"
  rm -f "$TMP/codex-defaults-drift"
  SUBYARD_RESOURCE_MODE=verify SUBYARD_RESOURCE_STEPS="$(jq -c .steps "$2/sync.json")" \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" sync > "$2/sync-verified.json"
  ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" up > "$2/up-noop.json"
  ORCA_TEST_ADVERTISE=127.0.0.1 run_orca down --yes > "$TMP/down-output"
  SUBYARD_RESOURCE_MODE=prepare "$ROOT/config/profiles/orca/resources/orca/handler.sh" down > "$2/down-noop.json"
  exit 0
fi
for mode in prepare apply; do
  for port in '' 17678; do
    if ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT="$port" \
      SUBYARD_RESOURCE_MODE="$mode" SUBYARD_RESOURCE_ACTION=pair \
      SUBYARD_OPERATION_ID=orca-handler-test \
      "$ROOT/config/profiles/orca/resources/orca/handler.sh" pair >"$TMP/pair-stopped.out" 2>&1; then
      fail "pair $mode accepted a stopped Orca service"
    fi
    grep -Fq "Orca is not running; run 'yard orca up' first" "$TMP/pair-stopped.out" \
      || fail "pair $mode did not explain that Orca must be started first (port='$port')"
    if ORCA_ADVERTISE_HOST=127.0.0.1 ORCA_HOST_PORT="$port" \
      SUBYARD_RESOURCE_MODE="$mode" SUBYARD_RESOURCE_ACTION=pair \
      SUBYARD_OPERATION_ID=orca-handler-test \
      "$ROOT/config/profiles/orca/resources/orca/handler.sh" pair --mobile \
      >"$TMP/pair-mobile-stopped.out" 2>&1; then
      fail "mobile pair $mode accepted a stopped Orca service"
    fi
    grep -Fq "Orca is not running; run 'yard orca up' first" "$TMP/pair-mobile-stopped.out" \
      || fail "mobile pair $mode did not explain that Orca must be started first (port='$port')"
    assert_down "pair $mode while stopped"
  done
done
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq 0 ] \
  || fail 'pair restarted a stopped Orca service'
[ "$(count_log '.pairing.url')" -eq 0 ] \
  || fail 'pair read a pairing capability while Orca was stopped'
observation="$("$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract observe)"
jq -e '.state == "absent" and .actual == "" and .desired == ""' <<<"$observation" >/dev/null \
  || fail 'runtime contract observation did not distinguish an absent Orca installation'
run_orca up --yes >"$TMP/up.out"
ready_file="$ORCA_TEST_GUEST/srv/agents/orca/ready.json"
mobile_request="$ready_file.mobile-request"
[ "$(stat -c %a "$ready_file")" = 600 ] || fail 'captured readiness file is not mode 0600'
[ "$(count_log 'orca-ide mobile=0')" -ge 1 ] \
  || fail 'staged capture wrapper did not execute the normal stock command'
grep -Fxq 'Environment=SSH_AUTH_SOCK=/home/dev/.ssh/subyard-agent.sock' \
  "$ORCA_TEST_CAPTURE/subyard-orca.service" \
  || fail 'service lacks the fixed yard SSH agent environment'
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
for source in "$ROOT"/config/profiles/orca/resources/orca/registration/*.py; do
  cmp -s "$source" "$ORCA_TEST_GUEST/usr/local/libexec/subyard/orca-registration/${source##*/}" \
    || fail 'registration component was not installed intact'
done
grep -Fq -- "sync --host-name 'owner-host'" \
  "$ORCA_TEST_GUEST/usr/local/libexec/subyard/projects-changed.d/orca" \
  || fail 'installed project hook did not receive the owner HostID'
observation="$("$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract observe)"
jq -e '.state == "current" and (.actual | test("^[0-9a-f]{64}$")) and .actual == .desired' \
  <<<"$observation" >/dev/null || fail 'installed runtime contract was not observed as current'
rm -f "$ORCA_TEST_GUEST/usr/local/libexec/subyard/orca-registration.sha256"
observation="$("$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract observe)"
jq -e '.state == "stale" and .actual != .desired' <<<"$observation" >/dev/null \
  || fail 'runtime contract observation accepted a missing completion marker'
printf 'legacy-contract\n' >"$ORCA_TEST_GUEST/usr/local/libexec/subyard/orca-registration.sha256"
observation="$("$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract observe)"
jq -e '.state == "stale" and .actual != .desired' <<<"$observation" >/dev/null \
  || fail 'runtime contract observation accepted an old completion marker'
push_count="$(cat "$ORCA_TEST_PUSH_COUNTER")"
printf '# drift\n' >>"$ORCA_TEST_GUEST/usr/local/libexec/subyard/orca-registration/main.py"
chmod 0644 "$ORCA_TEST_GUEST/usr/local/libexec/subyard/projects-changed.d/orca"
rm -f "$ORCA_TEST_GUEST/usr/local/libexec/subyard/orca-registration.lock"
printf 'legacy-contract\n' >"$ORCA_TEST_GUEST/usr/local/libexec/subyard/orca-registration.sha256"
observation="$("$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract observe)"
jq -e '.state == "stale" and .actual != .desired' <<<"$observation" >/dev/null \
  || fail 'runtime contract observation missed helper drift'
[ "$(cat "$ORCA_TEST_PUSH_COUNTER")" -eq "$push_count" ] \
  || fail 'runtime contract observation wrote guest files'
if run_orca sync >"$TMP/stale-sync.out" 2>&1; then
  fail 'sync assessment accepted a stale installed helper'
fi
grep -Fq 'yard init' "$TMP/stale-sync.out" \
  || fail 'stale sync assessment did not provide an init recovery hint'
if ORCA_ADVERTISE_HOST=owner.example-tailnet.ts.net ORCA_HOST_PORT=17678 \
  SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=sync SUBYARD_OPERATION_ID=stale-sync \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" sync >"$TMP/stale-sync-apply.out" 2>&1; then
  fail 'sync apply executed a stale installed helper'
fi
grep -Fq 'yard init' "$TMP/stale-sync-apply.out" \
  || fail 'stale sync apply did not provide an init recovery hint'
if "$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract apply \
  orca-refresh wrong "$(jq -r .desired <<<"$observation")" >"$TMP/runtime-stale.out" 2>&1; then
  fail 'runtime contract apply accepted a stale assessment'
fi
restart_count="$(count_log 'systemctl restart subyard-orca.service')"
unit_hash="$(sha256sum "$ORCA_TEST_GUEST/etc/systemd/system/subyard-orca.service")"
touch "$TMP/fail-registration-marker"
if "$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract apply orca-refresh \
  "$(jq -r .actual <<<"$observation")" "$(jq -r .desired <<<"$observation")" \
  >"$TMP/runtime-interrupted.out" 2>&1; then
  fail 'runtime contract apply concealed an interruption before marker publication'
fi
rm -f "$TMP/fail-registration-marker"
observation="$("$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract observe)"
jq -e '.state == "stale" and .actual != .desired' <<<"$observation" >/dev/null \
  || fail 'interrupted runtime contract apply appeared current without its marker'
"$ROOT/config/profiles/orca/resources/orca/handler.sh" _runtime-contract apply orca-refresh \
  "$(jq -r .actual <<<"$observation")" "$(jq -r .desired <<<"$observation")" \
  >"$TMP/runtime-applied.json"
jq -e '.state == "current" and .actual == .desired' "$TMP/runtime-applied.json" >/dev/null \
  || fail 'runtime contract apply did not verify the repaired helper'
[ "$(stat -c %a "$ORCA_TEST_GUEST/usr/local/libexec/subyard/projects-changed.d/orca")" = 755 ] \
  || fail 'runtime contract repair did not restore the hook mode'
[ "$(stat -c %a "$ORCA_TEST_GUEST/usr/local/libexec/subyard/orca-registration.lock")" = 644 ] \
  || fail 'runtime contract repair did not restore the persistent lock'
grep -Fq 'orca-registration.sha256' "$ORCA_TEST_GUEST/usr/local/libexec/subyard/projects-changed.d/orca" \
  || fail 'project hook does not guard against an interrupted helper generation'
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq "$restart_count" ] \
  || fail 'registration-only runtime repair restarted Orca'
[ "$(sha256sum "$ORCA_TEST_GUEST/etc/systemd/system/subyard-orca.service")" = "$unit_hash" ] \
  || fail 'registration-only runtime repair changed the installed service unit'
if grep -Eqi 'nodejs|npm|AppImage|squashfs|APPDIR|SHA512' \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" "$ROOT/config/profiles/orca/release.env"; then
  fail 'removed SSH/AppImage dependencies returned'
fi

restart_count="$(count_log 'systemctl restart subyard-orca.service')"
ORCA_ADVERTISE_HOST=owner.example-tailnet.ts.net ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" pair --mobile >"$TMP/mobile-plan.json"
jq -e '.action == "pair" and .changed == true' "$TMP/mobile-plan.json" >/dev/null \
  || fail 'mobile pair was not assessed through the normal resource action'
[ ! -e "$mobile_request" ] || fail 'mobile pairing prepare created a request'
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq "$restart_count" ] \
  || fail 'mobile pairing prepare restarted the service'
for invalid in '--unknown' '--mobile --mobile' '--mobile extra'; do
  status=0
  # shellcheck disable=SC2086 # Deliberate argument cases for the resource parser.
  if ORCA_ADVERTISE_HOST=owner.example-tailnet.ts.net ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" pair $invalid >"$TMP/pair-invalid.out" 2>&1; then
    fail "pair prepare accepted invalid arguments: $invalid"
  else
    status=$?
  fi
  [ "$status" -eq 2 ] || fail "pair prepare did not reject invalid arguments with code 2: $invalid"
  [ ! -e "$mobile_request" ] || fail "invalid pair arguments created a request: $invalid"
  [ "$(count_log 'systemctl restart subyard-orca.service')" -eq "$restart_count" ] \
    || fail "invalid pair arguments restarted the service: $invalid"
done

restart_count="$(count_log 'systemctl restart subyard-orca.service')"
ORCA_ADVERTISE_HOST=owner.example-tailnet.ts.net ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" up >"$TMP/nft-plan.json"
jq -e '.changed == false' "$TMP/nft-plan.json" >/dev/null \
  || fail 'owned ingress was reported as drift after its marker matched'
for verb in up down; do
  bash "$ORCA_TEST_CAPTURE/orca-ingress" "$verb" 6768 \
    || fail "$verb rejected owned ingress after its marker matched"
  touch "$ORCA_TEST_INGRESS"
done
for mode in foreign error; do
  printf '%s\n' "$mode" >"$TMP/nft-mode"
  ORCA_ADVERTISE_HOST=owner.example-tailnet.ts.net ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" up >"$TMP/nft-plan.json"
  jq -e '.changed == true' "$TMP/nft-plan.json" >/dev/null \
    || fail "ingress assessment accepted a $mode nft listing"
  mutations="$(count_log 'nft delete table') $(count_log 'nft add')"
  for verb in up down; do
    if bash "$ORCA_TEST_CAPTURE/orca-ingress" "$verb" 6768 >"$TMP/nft-refusal.out" 2>&1; then
      fail "$verb accepted a $mode nft listing"
    fi
    [ -f "$ORCA_TEST_INGRESS" ] || fail "$verb deleted an unverified ingress table"
  done
  [ "$(count_log 'nft delete table') $(count_log 'nft add')" = "$mutations" ] \
    || fail "ingress guards mutated the table after a $mode nft listing"
done
rm -f "$TMP/nft-mode"
run_orca up --yes >/dev/null
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq "$restart_count" ] \
  || fail 'identical up restarted an unchanged runtime'
if grep -Fq 'apt-get install -y -qq /tmp/subyard-orca' "$ORCA_TEST_LOG"; then
  fail 'repeated up reinstalled the already verified Orca release'
fi

grep -Fq '/usr/bin/python3 -B /usr/local/libexec/subyard/orca-registration/settings.py' \
  "$ORCA_TEST_CAPTURE/orca-sync" || fail 'project/init hook omitted Codex default convergence'
grep -Fxq 'Environment=SUBYARD_ORCA_CODEX_CONFIG=1' "$ORCA_TEST_CAPTURE/subyard-orca.service" \
  || fail 'Orca service omitted its scoped Codex launch environment'
dash -n "$ORCA_TEST_GUEST/etc/profile.d/subyard-orca-codex.sh" \
  || fail 'Codex shell integration is not valid for ordinary POSIX login shells'
env -u SUBYARD_ORCA_CODEX_CONFIG bash --noprofile --norc -c \
  '. "$1"; ! declare -F codex' _ "$ORCA_TEST_GUEST/etc/profile.d/subyard-orca-codex.sh" \
  || fail 'Codex shell integration changed a non-Orca shell'
touch "$TMP/codex-defaults-drift"
ORCA_ADVERTISE_HOST=owner.example-tailnet.ts.net ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" up >"$TMP/codex-plan.json"
jq -e '.changed == true' "$TMP/codex-plan.json" >/dev/null \
  || fail 'Codex default drift was not assessed'
[ -e "$TMP/codex-defaults-drift" ] || fail 'assessment changed Codex settings'
run_orca up --yes >/dev/null
[ ! -e "$TMP/codex-defaults-drift" ] || fail 'up did not repair Codex defaults'
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq "$restart_count" ] \
  || fail 'Codex settings repair unnecessarily restarted Orca'

touch "$TMP/missing-dispatcher"
if run_orca up --yes >"$TMP/missing-dispatcher.out" 2>&1; then
  fail 'up reported full readiness with a missing automatic dispatcher'
fi
grep -Fq 'init' "$TMP/missing-dispatcher.out" || fail 'missing dispatcher recovery was not actionable'
rm -f "$TMP/missing-dispatcher"

touch "$TMP/project-sync-fail"
if run_orca sync >"$TMP/sync-failure.out" 2>&1; then
  fail 'explicit sync skipped the hook and concealed its failure'
fi
rm -f "$TMP/project-sync-fail"

pairing_field_read_count="$(count_log '.pairing.url')"
ENVIRONMENT_PROFILES=orca run_orca status >"$TMP/status.out"
grep -Fq 'Orca profile selected for yard init' "$TMP/status.out" \
  || fail 'status did not confirm the selected Orca profile'
grep -Fq 'automatic project hook ready' "$TMP/status.out" \
  || fail 'status did not report automatic project hook readiness'
grep -Fq 'checkouts registered: 2/2' "$TMP/status.out" \
  || fail 'status did not report bounded project registration counts'
if grep -Fq 'orca://pair?' "$TMP/status.out"; then
  fail 'status exposed a pairing capability'
fi
ENVIRONMENT_PROFILES='' run_orca status >"$TMP/status-unselected.out"
grep -Fq 'Orca profile is not selected in ENVIRONMENT_PROFILES' "$TMP/status-unselected.out" \
  || fail 'status did not warn that yard init will omit the Orca profile'
[ "$(count_log '.pairing.url')" -eq "$pairing_field_read_count" ] \
  || fail 'status read a pairing capability from readiness state'
touch "$TMP/project-counts-fail"
ENVIRONMENT_PROFILES=orca run_orca status >"$TMP/status-counts-fail.out"
grep -Fq 'project registration status unavailable' "$TMP/status-counts-fail.out" \
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
mobile_pairing="$(run_orca pair --mobile --yes | tail -n1)"
[ "$mobile_pairing" = 'orca://pair?code=mobile-fixture' ] \
  || fail 'mobile pair did not return the stock mobile startup link'
[ ! -e "$mobile_request" ] || fail 'mobile pairing request survived a successful restart'
[ "$(count_log 'orca-ide mobile=1')" -eq 1 ] \
  || fail 'capture wrapper did not add mobile pairing exactly once'
[ "$(stat -c %a "$ready_file")" = 600 ] || fail 'mobile capture changed readiness mode'
[ "$(count_log 'runuser -u dev -- /usr/local/libexec/subyard/projects-changed.d/orca')" \
  -eq $((sync_count + 1)) ] \
  || fail 'mobile pair did not reconcile canonical project roots before returning the link'

pairing="$(run_orca pair --yes | tail -n1)"
[ "$pairing" = 'orca://pair?code=runtime-fixture' ] \
  || fail 'ordinary pair did not return the stock runtime startup link after mobile pairing'
[ "$(count_log 'orca-ide mobile=1')" -eq 1 ] \
  || fail 'ordinary pairing reused the mobile one-shot request'
grep -Fq 'systemctl restart subyard-orca.service' "$ORCA_TEST_LOG" \
  || fail 'pair did not mint a fresh startup offer'
[ "$(count_log 'runuser -u dev -- /usr/local/libexec/subyard/projects-changed.d/orca')" \
  -eq $((sync_count + 2)) ] \
  || fail 'pair did not reconcile canonical project roots before returning the link'

touch "$TMP/wrong-pairing-scope"
if run_orca pair --mobile --yes >"$TMP/pair-wrong-scope.out" 2>&1; then
  fail 'mobile pair accepted a runtime-scope readiness result'
fi
if grep -Fq 'orca://pair?' "$TMP/pair-wrong-scope.out"; then
  fail 'wrong-scope mobile pairing exposed a capability'
fi
[ ! -e "$mobile_request" ] || fail 'wrong-scope pairing retained a request'
rm -f "$TMP/wrong-pairing-scope"

touch "$TMP/fail-restart"
if run_orca pair --mobile --yes >"$TMP/pair-restart-failure.out" 2>&1; then
  fail 'mobile pair accepted a failed restart'
fi
if grep -Fq 'orca://pair?' "$TMP/pair-restart-failure.out"; then
  fail 'failed restart exposed a pairing capability'
fi
[ ! -e "$mobile_request" ] || fail 'failed restart retained a request'
rm -f "$TMP/fail-restart"

touch "$TMP/fail-orca-cli"
if run_orca pair --mobile --yes >"$TMP/pair-cli-failure.out" 2>&1; then
  fail 'mobile pair accepted a failed stock CLI'
fi
[ ! -e "$mobile_request" ] || fail 'failed stock CLI retained a request'
rm -f "$TMP/fail-orca-cli"
run_orca restart --yes >/dev/null

touch "$TMP/unready-orca"
restart_count="$(count_log 'systemctl restart subyard-orca.service')"
if run_orca pair --mobile --yes >"$TMP/pair-unready.out" 2>&1; then
  fail 'mobile pair accepted an unready restart'
fi
if grep -Fq 'orca://pair?' "$TMP/pair-unready.out"; then
  fail 'unready mobile pairing exposed a capability'
fi
[ ! -e "$mobile_request" ] || fail 'unready restart retained a request'
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq $((restart_count + 1)) ] \
  || fail 'unready mobile pairing did not reach the restart path'
rm -f "$TMP/unready-orca"
run_orca restart --yes >/dev/null

touch "$TMP/fail-mobile-request"
if run_orca pair --mobile --yes >"$TMP/pair-request-failure.out" 2>&1; then
  fail 'mobile pair accepted a failed request installation'
fi
[ ! -e "$mobile_request" ] || fail 'failed request installation retained a request'
rm -f "$TMP/fail-mobile-request"

touch "$TMP/fail-after-mobile-request"
restart_count="$(count_log 'systemctl restart subyard-orca.service')"
if run_orca pair --mobile --yes >"$TMP/pair-request-after-failure.out" 2>&1; then
  fail 'mobile pair accepted a request transport failure after creation'
fi
if grep -Fq 'orca://pair?' "$TMP/pair-request-after-failure.out"; then
  fail 'request transport failure exposed a pairing capability'
fi
[ ! -e "$mobile_request" ] || fail 'request transport failure retained a request'
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq "$restart_count" ] \
  || fail 'request transport failure restarted the service'
rm -f "$TMP/fail-after-mobile-request"

pairing="$(run_orca pair --yes | tail -n1)"
[ "$pairing" = 'orca://pair?code=runtime-fixture' ] \
  || fail 'ordinary pair did not recover after request transport failure'

touch "$TMP/signal-mobile-request"
restart_count="$(count_log 'systemctl restart subyard-orca.service')"
signal_status=0
if run_orca pair --mobile --yes >"$TMP/pair-request-signal.out" 2>&1; then
  fail 'mobile pair ignored a signal after request creation'
else
  signal_status=$?
fi
[ "$signal_status" -eq 143 ] || fail 'mobile pair did not return TERM status after request creation'
if grep -Fq 'orca://pair?' "$TMP/pair-request-signal.out"; then
  fail 'signaled mobile pairing exposed a capability'
fi
[ ! -e "$mobile_request" ] || fail 'signaled mobile pairing retained a request'
[ "$(count_log 'systemctl restart subyard-orca.service')" -eq "$restart_count" ] \
  || fail 'signaled mobile pairing restarted the service'
rm -f "$TMP/signal-mobile-request"

pairing="$(run_orca pair --yes | tail -n1)"
[ "$pairing" = 'orca://pair?code=runtime-fixture' ] \
  || fail 'ordinary pair did not recover after a signaled mobile request'

: >"$mobile_request"
if run_orca pair --mobile --yes >"$TMP/pair-request-pending.out" 2>&1; then
  fail 'mobile pair overwrote an existing request'
fi
[ -e "$mobile_request" ] || fail 'failed noclobber request installation removed the pending request'
rm -f "$mobile_request"

pairing="$(run_orca pair --yes | tail -n1)"
[ "$pairing" = 'orca://pair?code=runtime-fixture' ] \
  || fail 'ordinary pair did not recover after failed mobile pairing'
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
[ "$pairing" = 'orca://pair?code=runtime-fixture' ] \
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

if ORCA_ADVERTISE_HOST='https://bad/path' ORCA_HOST_PORT=17678 SUBYARD_RESOURCE_MODE=prepare \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" up >"$TMP/invalid.out" 2>&1; then
  fail 'unsafe advertised hostname was accepted'
fi
grep -Fq 'without scheme, path or port' "$TMP/invalid.out" \
  || fail 'unsafe hostname failure was not actionable'

# Run the profile component's filesystem, wire and reconciliation contract tests in the core gate.
python3 -B -m unittest discover -s "$ROOT/config/profiles/orca/tests/orca_registration"

printf 'ok: Orca repeatably stages, pairs, rolls back and preserves state across exact routes\n'

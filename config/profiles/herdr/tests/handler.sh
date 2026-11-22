#!/usr/bin/env bash
set -euo pipefail

test_root=$(mktemp -d /tmp/subyard-herdr-handler.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/fixture/config/profiles"
ln -s "$PWD/scripts" "$test_root/fixture/scripts"
cp -a config/profiles/herdr "$test_root/fixture/config/profiles/"
mkdir -p "$test_root/fixture/config/profiles/herdr/bin"
export PATH="$test_root/bin:$PATH" HERDR_TEST_CALLS="$test_root/calls"
export HERDR_TEST_DROPIN_PATH="$test_root/dropin"
export HERDR_TEST_PID_PATH="$test_root/pid"
printf '42\n' > "$HERDR_TEST_PID_PATH"
unset -f incus
cat > "$test_root/bin/incus" <<'INCUS'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  list) if [ "${HERDR_TEST_RUNNING:-no}" = yes ]; then printf 'fixture,RUNNING\n'; fi ;;
  info) [ "${HERDR_TEST_RUNNING:-no}" = yes ] ;;
  query) printf '%s\n' "$HERDR_TEST_ROUTE" ;;
  config) printf '%s\n' "$*" >> "$HERDR_TEST_CALLS" ;;
  exec)
    shift 5
    case "$1" in
      uname) printf 'x86_64\n' ;;
      cat) printf '%s\n' "${HERDR_TEST_ACTUAL:-$HERDR_TEST_DIGEST}" ;;
      cmp) cat >/dev/null ;;
      install) ;;
      test)
        if [[ "$2 $3" == '-x /usr/local/libexec/subyard/projects-changed.d/herdr' ]]; then
          [ "${HERDR_TEST_HOOK:-ready}" = ready ]
        fi ;;
      loginctl) printf 'yes\n' ;;
      journalctl) printf '%s\n' "$@" ;;
      sh)
        cat >/dev/null
        if [ "$2:$3:$4" = '-eu:-s:--' ]; then
          case "$5" in
            check) [ "${HERDR_TEST_DROPIN:-present}" = present ] || [ -e "$HERDR_TEST_DROPIN_PATH" ] ;;
            absent) [ "${HERDR_TEST_DROPIN:-present}" = missing ] && [ ! -e "$HERDR_TEST_DROPIN_PATH" ] ;;
            ensure)
              printf 'install SSH drop-in\n' >> "$HERDR_TEST_CALLS"
              if [ "${HERDR_TEST_POLICY:-safe}" = overridden ]; then
                printf 'Remove the overriding PasswordAuthentication directive\n' >&2
                exit 1
              fi
              touch "$HERDR_TEST_DROPIN_PATH" ;;
            remove) printf 'remove SSH drop-in\n' >> "$HERDR_TEST_CALLS"; rm -f -- "$HERDR_TEST_DROPIN_PATH" ;;
            *) exit 85 ;;
          esac
        fi ;;
      systemctl) printf '%s\n' "$*" >> "$HERDR_TEST_CALLS" ;;
      runuser)
        shift 4
        case "$1:$2" in
          bash:-s)
            cat >/dev/null
            printf 'core release %s\n' "$7" >> "$HERDR_TEST_CALLS"
            if [ "$7" = minimum ]; then printf 'upstream Core update completed\n'; fi
            if [ "$7" = installed ]; then printf '0.1.21\n'; fi
            if [ "$7" = latest ] || [ "$7" = force ]; then
              printf '%s\n' "${HERDR_TEST_SWAP:-true}"
              if [ "${HERDR_TEST_SWAP:-true}:${HERDR_TEST_STICK_PID:-no}" = true:no ]; then
                printf '43\n' > "$HERDR_TEST_PID_PATH"
              fi
            fi ;;
          */herdr-login:server)
            login_call="$*"
            login_call="${login_call#/usr/local/libexec/subyard/herdr-login }"
            case "$login_call" in
              'server restart --fresh'|'server restart') printf '%s\n' "$login_call" >> "$HERDR_TEST_CALLS" ;;
              *) exit 83 ;;
            esac
            if [ "${HERDR_TEST_STICK_PID:-no}" = no ]; then printf '43\n' > "$HERDR_TEST_PID_PATH"; fi ;;
          */herdr-profile:observe)
            if [ "${HERDR_TEST_CORE:-up}" = down ]; then exit 1; fi
            if [ "${HERDR_TEST_CORE:-up}" = stopped ]; then printf 'null\n'; exit 0; fi
            printf '{"PID":%s,"Live":2,"Version":"%s"}\n' "$(cat "$HERDR_TEST_PID_PATH")" "${HERDR_TEST_VERSION:-0.1.21}" ;;
          */herdr-profile:stop)
            printf 'stop Core\n' >> "$HERDR_TEST_CALLS"
            [ "${HERDR_TEST_CORE:-up}" = stopped ] ;;
          */herdr-profile:sync) printf 'sync projects\n' >> "$HERDR_TEST_CALLS" ;;
          */herdr-profile:registration-ready) [ "${HERDR_TEST_REGISTRATION:-ready}" = ready ] ;;
          */herdr-profile:keys)
            case "$3:$4" in
              apply:list) printf '1  fixture-client\n' ;;
              prepare:revoke) printf '{"changed":true,"digest":"%064d"}\n' 1 ;;
              apply:revoke) printf 'revoke client key\n' >> "$HERDR_TEST_CALLS" ;;
              *) exit 84 ;;
            esac ;;
          *) exit 89 ;;
        esac ;;
      *) exit 88 ;;
    esac ;;
  *) exit 87 ;;
esac
INCUS
printf '#!/bin/sh\nexit 86\n' > "$test_root/fixture/config/profiles/herdr/bin/herdr-profile"
cat > "$test_root/bin/timeout" <<'TIMEOUT'
#!/bin/sh
[ "$1:$2:$6:$7" = '5:bash:100.100.100.100:6768' ] && [ "${HERDR_TEST_LISTENER:-up}" = up ]
TIMEOUT
printf '#!/bin/sh\nprintf "256 SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fixture (ED25519)\\n"\n' > "$test_root/bin/ssh-keygen"
printf '#!/bin/sh\nprintf "100.100.100.100\\n"\n' > "$test_root/bin/tailscale"
printf '#!/bin/sh\nprintf "100.100.100.100 STREAM fixture.ts.net\\n"\n' > "$test_root/bin/getent"
printf '#!/bin/sh\nprintf "1: tailscale0 inet 100.100.100.100/32 scope global\\n"\n' > "$test_root/bin/ip"
printf '#!/bin/sh\nexit 0\n' > "$test_root/bin/ss"
printf '#!/bin/sh\nexit 0\n' > "$test_root/bin/sleep"
chmod 0755 "$test_root/bin/"* "$test_root/fixture/config/profiles/herdr/bin/herdr-profile"

for setting in SUBYARD_OPERATOR_HOME SUBYARD_CONFIG_DIR SUBYARD_CONFIG_HOME SUBYARD_HOME \
  STORAGE_PATH HOST_BASE RESTRICTED_DISK_PATHS INCUS_BRIDGE; do
  export "$setting="
done
export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1 SUBYARD_RESOURCE_MODE=prepare
export ACCESS_KIND=local YARD_INSTANCE_NAME=fixture INCUS_PROJECT=fixture SSH_HOST=fixture
export DEV_USER=dev DEV_UID=1000 DEV_SUDO=no FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0 YARD_KIND=container
handler="$test_root/fixture/config/profiles/herdr/resources/herdr/handler.sh"
"$handler" status | jq -e '.action == "status" and .changed == false' >/dev/null
"$handler" up | jq -e '.action == "up" and .changed and .steps[0].decision == "conditional"' >/dev/null
if result=$(YARD_KIND=vm "$handler" up 2>&1); then exit 1; fi
[[ "$result" == *'container yards only'* ]]
if result=$(NESTED_E2E_VMS=1 "$handler" up 2>&1); then exit 1; fi
[[ "$result" == *'unavailable in nested E2E mode'* ]]
if result=$(YARD_KIND=vm NESTED_E2E_VMS=1 "$handler" up 2>&1); then exit 1; fi
[[ "$result" == *'container yards only'*'unavailable in nested E2E mode'* ]]

export HERDR_TEST_RUNNING=yes HERDR_ADVERTISE_HOST=fixture.ts.net HERDR_HOST_PORT=6768
profile="$test_root/fixture/config/profiles/herdr"
config="$profile/resources/herdr"
HERDR_TEST_DIGEST=$(sha256sum "$profile/bin/herdr-profile" "$config/login.sh" "$config/handler.sh" \
  "$config/install-core.sh" "$config/projects-changed.sh" "$profile/release.env" "$profile/guest-ssh.sh" |
  awk '{print $1}' | sha256sum | cut -d ' ' -f1)
export HERDR_TEST_DIGEST
token="v1:$(printf 'v1\nbind=host\nconnect=tcp:127.0.0.1:22\nlisten=tcp:100.100.100.100:6768\ntype=proxy\n' | sha256sum | cut -d ' ' -f1)"
export HERDR_TEST_ROUTE
HERDR_TEST_ROUTE=$(jq -cn --arg token "$token" '{devices:{"herdr-ssh":{bind:"host",connect:"tcp:127.0.0.1:22",listen:"tcp:100.100.100.100:6768",type:"proxy"}},config:{"user.subyard.resource.herdr-ssh":$token}}')

"$handler" up | jq -e '.changed == false' >/dev/null
HERDR_TEST_REGISTRATION=missing "$handler" up | jq -e '.changed == true' >/dev/null
HERDR_TEST_HOOK=missing "$handler" up | jq -e '.changed == true' >/dev/null
HERDR_TEST_LISTENER=down "$handler" up | jq -e '.changed == true' >/dev/null
HERDR_TEST_DROPIN=missing "$handler" up | jq -e '.changed == true' >/dev/null
if HERDR_TEST_LISTENER=down "$handler" pair --key fixture >/dev/null 2>&1; then exit 1; fi
if HERDR_TEST_DROPIN=missing "$handler" pair --key fixture >/dev/null 2>&1; then exit 1; fi

SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=logs SUBYARD_OPERATION_ID=fixture \
  "$handler" logs --follow > "$test_root/journal-argv"
printf '%s\n' journalctl --no-pager -n 100 _SYSTEMD_UNIT=subyard-herdr-core.service + _UID=1000 _COMM=herdr-core --follow |
  cmp - "$test_root/journal-argv"

(
  export HERDR_TEST_SWAP=false HERDR_TEST_STICK_PID=yes SUBYARD_RESOURCE_MODE=apply
  export SUBYARD_RESOURCE_ACTION=update SUBYARD_OPERATION_ID=fixture
  : > "$HERDR_TEST_CALLS"
  "$handler" update
  "$handler" update --force
  [ "$(cat "$HERDR_TEST_PID_PATH")" = 42 ]
  if grep -q '^server restart' "$HERDR_TEST_CALLS"; then exit 1; fi
)
(
  # The installed release is current but Core still runs the older binary: a retry hands over.
  export HERDR_TEST_SWAP=false HERDR_TEST_VERSION=0.1.20 SUBYARD_RESOURCE_MODE=apply
  export SUBYARD_RESOURCE_ACTION=update SUBYARD_OPERATION_ID=fixture
  for argument in '' --force; do
    printf '42\n' > "$HERDR_TEST_PID_PATH"
    : > "$HERDR_TEST_CALLS"
    "$handler" update ${argument:+"$argument"}
    grep -Fxq 'server restart' "$HERDR_TEST_CALLS"
    [ "$(cat "$HERDR_TEST_PID_PATH")" = 43 ]
  done
  printf '42\n' > "$HERDR_TEST_PID_PATH"
  if result=$(HERDR_TEST_STICK_PID=yes "$handler" update 2>&1); then exit 1; fi
  [[ "$result" == *'new PID after 42'* ]]
)
printf '42\n' > "$HERDR_TEST_PID_PATH"
SUBYARD_RESOURCE_MODE=verify "$handler" update | jq -e '.changed == false' >/dev/null
HERDR_TEST_VERSION=0.1.20 SUBYARD_RESOURCE_MODE=verify "$handler" update | jq -e '.changed' >/dev/null
(
  export SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=update SUBYARD_OPERATION_ID=fixture
  if result=$(HERDR_TEST_STICK_PID=yes "$handler" update 2>&1); then exit 1; fi
  [[ "$result" == *'new PID after 42'* ]]
  "$handler" update
  [ "$(cat "$HERDR_TEST_PID_PATH")" = 43 ]
)
printf '42\n' > "$HERDR_TEST_PID_PATH"
steps=$("$handler" restart | jq -ce 'select(.changed) | .steps | select(.[0].desired == "running-after:42" and .[0].decision == "apply")')
SUBYARD_RESOURCE_MODE=verify SUBYARD_RESOURCE_STEPS="$steps" "$handler" restart |
  jq -e '.changed and .steps[0].observed == "running:42"' >/dev/null
(
  export SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=restart SUBYARD_OPERATION_ID=fixture
  if result=$(HERDR_TEST_STICK_PID=yes "$handler" restart 2>&1); then exit 1; fi
  [[ "$result" == *'new PID after 42'* ]]
  "$handler" restart
)
SUBYARD_RESOURCE_MODE=verify SUBYARD_RESOURCE_STEPS="$steps" "$handler" restart |
  jq -e '.changed == false and .steps[0].observed == "running-after:42"' >/dev/null

(
  export HERDR_TEST_DROPIN=missing SUBYARD_RESOURCE_MODE=apply SUBYARD_OPERATION_ID=fixture
  : > "$HERDR_TEST_CALLS"
  if result=$(HERDR_TEST_ROUTE='{}' HERDR_TEST_POLICY=overridden SUBYARD_RESOURCE_ACTION=up "$handler" up 2>&1); then exit 1; fi
  [[ "$result" == *'Remove the overriding PasswordAuthentication directive'* ]]
  if grep -q '^config device add ' "$HERDR_TEST_CALLS"; then exit 1; fi
  : > "$HERDR_TEST_CALLS"
  HERDR_TEST_ROUTE='{}' SUBYARD_RESOURCE_ACTION=up "$handler" up >/dev/null
  [ -f "$HERDR_TEST_DROPIN_PATH" ]
  grep -E '^(install SSH drop-in|config device add)' "$HERDR_TEST_CALLS" | cut -d ' ' -f1-3 > "$test_root/install-order"
  printf 'install SSH drop-in\nconfig device add\n' | cmp - "$test_root/install-order"
  HERDR_TEST_CORE=stopped SUBYARD_RESOURCE_ACTION=down "$handler" down
  [ ! -e "$HERDR_TEST_DROPIN_PATH" ] && grep -Fxq 'remove SSH drop-in' "$HERDR_TEST_CALLS"
)

(
  export HERDR_TEST_DROPIN=missing HERDR_TEST_LISTENER=down HERDR_TEST_ROUTE='{}'
  key=SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB
  "$handler" pair --list | jq -e '.action == "pair-list"' >/dev/null
  "$handler" pair --revoke "$key" | jq -e '.action == "pair-revoke" and .changed' >/dev/null
  export SUBYARD_CONFIG_HOME="$test_root/config" SUBYARD_RESOURCE_MODE=apply SUBYARD_OPERATION_ID=fixture
  mkdir -p "$SUBYARD_CONFIG_HOME/generated/integration-locks"
  install -m 0600 /dev/null "$SUBYARD_CONFIG_HOME/generated/integration-locks/default.lock"
  SUBYARD_RESOURCE_ACTION=pair-list "$handler" pair --list | grep -Fx '1  fixture-client' >/dev/null
  SUBYARD_RESOURCE_ACTION=pair-revoke "$handler" pair --revoke "$key"
  grep -Fxq 'revoke client key' "$HERDR_TEST_CALLS"
)

HERDR_TEST_CORE=down "$handler" down | jq -e '.changed and (.consequences[0] | contains("unavailable"))' >/dev/null
: > "$HERDR_TEST_CALLS"
if result=$(HERDR_TEST_CORE=down SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down SUBYARD_OPERATION_ID=fixture "$handler" down 2>&1); then exit 1; fi
[[ "$result" == *'Core shutdown could not be verified'* ]]
grep -n -e '^config device remove ' -e '^stop Core$' "$HERDR_TEST_CALLS" | cut -d: -f2- > "$test_root/order"
printf 'config device remove fixture herdr-ssh --project fixture\nstop Core\n' | cmp - "$test_root/order"

old=$(printf '%064d' 0)
: > "$HERDR_TEST_CALLS"
"$handler" _runtime observe | jq -e '.state == "current"' >/dev/null
[ ! -s "$HERDR_TEST_CALLS" ]
HERDR_TEST_ACTUAL="$old" "$handler" _runtime apply fixture "$old" "$HERDR_TEST_DIGEST" > "$test_root/runtime" 2> "$test_root/runtime.log"
jq -se --arg desired "$HERDR_TEST_DIGEST" 'length == 1 and .[0].state == "current" and .[0].actual == $desired' "$test_root/runtime" >/dev/null
printf 'systemctl daemon-reload\n' | cmp - "$HERDR_TEST_CALLS"
empty=$(printf '' | sha256sum | cut -d ' ' -f1)
for corrupted in stale "$(printf '%063d' 0)" "$(printf '%064d' 0 | tr 0 A)"; do
  HERDR_TEST_ACTUAL="$corrupted" "$handler" _runtime observe |
    jq -e --arg empty "$empty" '.state == "stale" and .actual == $empty' >/dev/null
done
: > "$HERDR_TEST_CALLS"
HERDR_TEST_ACTUAL=stale "$handler" _runtime apply fixture "$empty" "$HERDR_TEST_DIGEST" |
  jq -e '.state == "current"' >/dev/null
grep -Fxq 'systemctl daemon-reload' "$HERDR_TEST_CALLS"
printf '\n# New SSH policy revision\n' >> "$profile/guest-ssh.sh"
"$handler" _runtime observe | jq -e '.state == "stale" and .actual != .desired' >/dev/null

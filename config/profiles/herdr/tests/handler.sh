#!/usr/bin/env bash
set -euo pipefail

test_root=$(mktemp -d /tmp/subyard-herdr-handler.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/fixture/config/profiles"
ln -s "$PWD/scripts" "$test_root/fixture/scripts"
cp -a config/profiles/herdr "$test_root/fixture/config/profiles/"
mkdir -p "$test_root/fixture/config/profiles/herdr/bin"
export PATH="$test_root/bin:$PATH" HERDR_TEST_CALLS="$test_root/calls"
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
      bash) cat >/dev/null; printf 'context test\n' ;;
      cat) printf '%s\n' "${HERDR_TEST_ACTUAL:-$HERDR_TEST_DIGEST}" ;;
      cmp) cat >/dev/null ;;
      install) ;;
      test)
        if [[ "$2 $3" == '-x /usr/local/libexec/subyard/projects-changed.d/herdr' ]]; then
          [ "${HERDR_TEST_HOOK:-ready}" = ready ]
        fi ;;
      loginctl) printf 'yes\n' ;;
      sh) cat >/dev/null ;;
      systemctl) printf '%s\n' "$*" >> "$HERDR_TEST_CALLS" ;;
      runuser)
        shift 4
        case "$1:$2" in
          bash:-s)
            cat >/dev/null
            if [ "$7" = minimum ]; then printf 'upstream Core update completed\n'; fi ;;
          */herdr-profile:observe)
            if [ "${HERDR_TEST_CORE:-up}" = down ]; then exit 1; fi
            printf '{"PID":42,"Live":2}\n' ;;
          */herdr-profile:stop) printf 'stop Core\n' >> "$HERDR_TEST_CALLS"; exit 1 ;;
          */herdr-profile:sync) ;;
          */herdr-profile:registration-ready) [ "${HERDR_TEST_REGISTRATION:-ready}" = ready ] ;;
          *) exit 89 ;;
        esac ;;
      *) exit 88 ;;
    esac ;;
  *) exit 87 ;;
esac
INCUS
cat > "$test_root/fixture/config/profiles/herdr/bin/herdr-profile" <<'NATIVE'
#!/usr/bin/env bash
case "$1" in
  ssh-live) cat >/dev/null; [ "${HERDR_TEST_LISTENER:-up}" = up ] ;;
  ssh-policy) cat >/dev/null; [ "${HERDR_TEST_POLICY:-safe}" = safe ] ;;
  *) exit 86 ;;
esac
NATIVE
printf '#!/bin/sh\nprintf "100.100.100.100\\n"\n' > "$test_root/bin/tailscale"
printf '#!/bin/sh\nprintf "100.100.100.100 STREAM fixture.ts.net\\n"\n' > "$test_root/bin/getent"
printf '#!/bin/sh\nprintf "1: tailscale0 inet 100.100.100.100/32 scope global\\n"\n' > "$test_root/bin/ip"
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

export HERDR_TEST_RUNNING=yes HERDR_ADVERTISE_HOST=fixture.ts.net HERDR_HOST_PORT=6768
profile="$test_root/fixture/config/profiles/herdr"
config="$profile/resources/herdr"
HERDR_TEST_DIGEST=$(sha256sum "$profile/bin/herdr-profile" "$config/login.sh" "$config/handler.sh" \
  "$config/install-core.sh" "$config/projects-changed.sh" "$profile/release.env" | awk '{print $1}' | sha256sum | cut -d ' ' -f1)
export HERDR_TEST_DIGEST
token="v1:$(printf 'v1\nbind=host\nconnect=tcp:127.0.0.1:22\nlisten=tcp:100.100.100.100:6768\ntype=proxy\n' | sha256sum | cut -d ' ' -f1)"
export HERDR_TEST_ROUTE
HERDR_TEST_ROUTE=$(jq -cn --arg token "$token" '{devices:{"herdr-ssh":{bind:"host",connect:"tcp:127.0.0.1:22",listen:"tcp:100.100.100.100:6768",type:"proxy"}},config:{"user.subyard.resource.herdr-ssh":$token}}')

"$handler" up | jq -e '.changed == false' >/dev/null
HERDR_TEST_REGISTRATION=missing "$handler" up | jq -e '.changed == true' >/dev/null
HERDR_TEST_HOOK=missing "$handler" up | jq -e '.changed == true' >/dev/null
HERDR_TEST_LISTENER=down "$handler" up | jq -e '.changed == true' >/dev/null
if HERDR_TEST_POLICY=unsafe "$handler" up >/dev/null; then exit 1; fi
if HERDR_TEST_LISTENER=down "$handler" pair --list >/dev/null 2>&1; then exit 1; fi
if HERDR_TEST_POLICY=unsafe "$handler" pair --list >/dev/null; then exit 1; fi

HERDR_TEST_CORE=down "$handler" down | jq -e '.changed and (.consequences[0] | contains("unavailable"))' >/dev/null
: > "$HERDR_TEST_CALLS"
if result=$(HERDR_TEST_CORE=down SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down SUBYARD_OPERATION_ID=fixture "$handler" down 2>&1); then exit 1; fi
[[ "$result" == *'Core shutdown could not be verified'* ]]
grep -n -e '^config device remove ' -e '^stop Core$' "$HERDR_TEST_CALLS" | cut -d: -f2- > "$test_root/order"
printf 'config device remove fixture herdr-ssh --project fixture\nstop Core\n' | cmp - "$test_root/order"

old=$(printf '%064d' 0)
HERDR_TEST_ACTUAL="$old" "$handler" _runtime apply fixture "$old" "$HERDR_TEST_DIGEST" > "$test_root/runtime" 2> "$test_root/runtime.log"
jq -se --arg desired "$HERDR_TEST_DIGEST" 'length == 1 and .[0].state == "current" and .[0].actual == $desired' "$test_root/runtime" >/dev/null
grep -Fq 'upstream Core update completed' "$test_root/runtime.log"

#!/usr/bin/env bash
set -euo pipefail

test_root=$(mktemp -d "${TMPDIR:-/tmp}/subyard-herdr-handler.XXXXXX")
trap 'rm -rf -- "$test_root"' EXIT
keygen=$(command -v ssh-keygen)
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
      ssh-keygen) printf '256 SHA256:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC host (ED25519)\n' ;;
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
        if [ "$2:$3" = '-eu:-c' ]; then
          printf 'guest file %s %s\n' "$6" "$7" >> "$HERDR_TEST_CALLS"
          if [ -n "${HERDR_TEST_FILES:-}" ]; then cat > "$HERDR_TEST_FILES/${6##*/}"; fi
        fi
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
      systemctl)
        printf '%s\n' "$*" >> "$HERDR_TEST_CALLS"
        case "$2:${4:-}" in
          is-enabled:subyard-herdr-core.service) [ "${HERDR_TEST_CORE_UNIT:-enabled}" = enabled ] ;;
          is-enabled:subyard-herdr-discovery.timer) [ "${HERDR_TEST_TIMER:-active}" != disabled ] ;;
          is-active:subyard-herdr-discovery.timer) [ "${HERDR_TEST_TIMER:-active}" = active ] ;;
          is-active:subyard-herdr-discovery.service) [ "${HERDR_TEST_DISCOVERY:-idle}" = running ] ;;
        esac ;;
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
          env:SUBYARD_HERDR_MINIMUM=*) printf 'Core: fixture\n' ;;
          */herdr-profile:registration-ready) [ "${HERDR_TEST_REGISTRATION:-ready}" = ready ] ;;
          */herdr-profile:keys)
            case "$3:$4" in
              apply:list)
                if [ -n "${HERDR_TEST_GUEST_HOME:-}" ]; then
                  HOME="$HERDR_TEST_GUEST_HOME" "$HERDR_TEST_NATIVE" keys apply list "$5"
                else
                  printf '1  fixture-client\n'
                fi ;;
              prepare:revoke) printf '{"changed":true,"digest":"%064d"}\n' 1 ;;
              apply:revoke) printf 'revoke client key\n' >> "$HERDR_TEST_CALLS" ;;
              apply:add|apply:existing)
                printf '%s %s %s\n' "$4" "$6" "$(cat)" >> "$HERDR_TEST_CALLS"
                printf '{"changed":false}\n' ;;
              *) exit 84 ;;
            esac ;;
          *) exit 89 ;;
        esac ;;
      *) exit 88 ;;
    esac ;;
  *) exit 87 ;;
esac
INCUS
export HERDR_TEST_NATIVE="$test_root/fixture/config/profiles/herdr/bin/herdr-profile"
go build -o "$HERDR_TEST_NATIVE" ./config/profiles/herdr/cmd/herdr-profile
cat > "$test_root/bin/timeout" <<'TIMEOUT'
#!/bin/sh
[ "$1:$2:$6:$7" = '5:bash:100.100.100.100:6768' ] && [ "${HERDR_TEST_LISTENER:-up}" = up ]
TIMEOUT
cat > "$test_root/bin/ssh-keygen" <<'KEYGEN'
#!/bin/sh
case "${HERDR_TEST_OWNER_FP:-}" in
  '') printf '256 SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fixture (ED25519)\n' ;;
  fail) exit 1 ;;
  *) printf '256 %s owner (ED25519)\n' "$HERDR_TEST_OWNER_FP" ;;
esac
KEYGEN
printf '#!/bin/sh\nprintf "100.100.100.100\\n"\n' > "$test_root/bin/tailscale"
printf '#!/bin/sh\nprintf "100.100.100.100 STREAM fixture.ts.net\\n"\n' > "$test_root/bin/getent"
printf '#!/bin/sh\nprintf "1: tailscale0 inet 100.100.100.100/32 scope global\\n"\n' > "$test_root/bin/ip"
printf '#!/bin/sh\nexit 0\n' > "$test_root/bin/ss"
printf '#!/bin/sh\nexit 0\n' > "$test_root/bin/sleep"
printf '#!/bin/sh\nprintf "Fixture-Host.\\n"\n' > "$test_root/bin/hostname"
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
library="$test_root/fixture/scripts/lib/checkout-discovery"
HERDR_TEST_DIGEST=$(sha256sum "$profile/bin/herdr-profile" "$config/login.sh" "$config/handler.sh" \
  "$config/install-core.sh" "$config/projects-changed.sh" "$profile/release.env" "$profile/guest-ssh.sh" \
  "$library/checkouts.py" "$library/discovery.py" "$library/inventory.py" |
  awk '{print $1}' | sha256sum | cut -d ' ' -f1)
export HERDR_TEST_DIGEST
token="v1:$(printf 'v1\nbind=host\nconnect=tcp:127.0.0.1:22\nlisten=tcp:100.100.100.100:6768\ntype=proxy\n' | sha256sum | cut -d ' ' -f1)"
export HERDR_TEST_ROUTE
HERDR_TEST_ROUTE=$(jq -cn --arg token "$token" '{devices:{"herdr-ssh":{bind:"host",connect:"tcp:127.0.0.1:22",listen:"tcp:100.100.100.100:6768",type:"proxy"}},config:{"user.subyard.resource.herdr-ssh":$token}}')

"$handler" up | jq -e '.changed == false' >/dev/null
HERDR_TEST_REGISTRATION=missing "$handler" up | jq -e '.changed == true' >/dev/null
HERDR_TEST_HOOK=missing "$handler" up | jq -e '.changed == true' >/dev/null
HERDR_TEST_TIMER=disabled "$handler" up | jq -e '.changed == true' >/dev/null
HERDR_TEST_TIMER=inactive "$handler" up | jq -e '.changed == true' >/dev/null
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
  export HERDR_TEST_FILES="$test_root/guest-files"
  mkdir "$HERDR_TEST_FILES"
  HERDR_TEST_ROUTE='{}' SUBYARD_RESOURCE_ACTION=up "$handler" up >/dev/null
  [ -f "$HERDR_TEST_DROPIN_PATH" ]
  # The guest gets the shared library and a timer with Orca's intervals that runs discovery.
  for name in checkouts discovery inventory; do
    grep -Fxq "guest file /usr/local/libexec/subyard/herdr-discovery/$name.py 0644" "$HERDR_TEST_CALLS"
    cmp "$HERDR_TEST_FILES/$name.py" "$library/$name.py"
  done
  grep -Fxq 'ExecStart=/usr/local/libexec/subyard/herdr-profile discover' "$HERDR_TEST_FILES/subyard-herdr-discovery.service"
  grep -Fxq 'User=dev' "$HERDR_TEST_FILES/subyard-herdr-discovery.service"
  grep -Fxq 'OnBootSec=15s' "$HERDR_TEST_FILES/subyard-herdr-discovery.timer"
  grep -Fxq 'OnUnitInactiveSec=15s' "$HERDR_TEST_FILES/subyard-herdr-discovery.timer"
  grep -n -e '^sync projects$' -e '^systemctl enable --now --quiet subyard-herdr-discovery.timer$' \
    "$HERDR_TEST_CALLS" | cut -d: -f2- | cut -d ' ' -f1-2 > "$test_root/schedule-order"
  printf 'sync projects\nsystemctl enable\n' | cmp - "$test_root/schedule-order"
  unset HERDR_TEST_FILES
  grep -E '^(install SSH drop-in|config device add)' "$HERDR_TEST_CALLS" | cut -d ' ' -f1-3 > "$test_root/install-order"
  printf 'install SSH drop-in\nconfig device add\n' | cmp - "$test_root/install-order"
  : > "$HERDR_TEST_CALLS"
  HERDR_TEST_DISCOVERY=running HERDR_TEST_CORE=stopped SUBYARD_RESOURCE_ACTION=down "$handler" down
  [ ! -e "$HERDR_TEST_DROPIN_PATH" ] && grep -Fxq 'remove SSH drop-in' "$HERDR_TEST_CALLS"
  grep -o 'systemctl \(disable\|stop\) .*' "$HERDR_TEST_CALLS" > "$test_root/down-units"
  printf '%s\n' 'systemctl disable --now --quiet subyard-herdr-discovery.timer' 'systemctl stop subyard-herdr-discovery.service' \
    'systemctl disable --now subyard-herdr-core.service' | cmp - "$test_root/down-units"
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

(
  # Host keys: the owner key, two bare new keys, a granted key, an RSA key the yard
  # accepts outside Herdr and a key with options. The guest state comes from the real helper.
  export SUBYARD_OPERATOR_HOME="$test_root/operator" SUBYARD_CONFIG_HOME="$test_root/config"
  export HERDR_TEST_GUEST_HOME="$test_root/guest"
  host_keys="$SUBYARD_OPERATOR_HOME/.ssh/authorized_keys"
  mkdir -m 0700 "$test_root/keys" "$SUBYARD_OPERATOR_HOME" "$SUBYARD_OPERATOR_HOME/.ssh" \
    "$HERDR_TEST_GUEST_HOME" "$HERDR_TEST_GUEST_HOME/.ssh"
  mkdir -p "$SUBYARD_CONFIG_HOME/generated/integration-locks"
  install -m 0600 /dev/null "$SUBYARD_CONFIG_HOME/generated/integration-locks/default.lock"
  declare -A fp
  for name in owner laptop phone desk manual office; do
    type=ed25519
    if [ "$name" = manual ]; then type=rsa; fi
    "$keygen" -q -t "$type" -N '' -C "user@$name" -f "$test_root/keys/$name"
    fp[$name]=$("$keygen" -E sha256 -lf "$test_root/keys/$name.pub" | cut -d ' ' -f2)
  done
  export HERDR_TEST_OWNER_FP="${fp[owner]}"
  bare() { cut -d ' ' -f1-2 "$test_root/keys/$1.pub"; }
  { for name in owner laptop phone desk manual; do cat "$test_root/keys/$name.pub"; done
    printf 'restrict %s\nnot a key\n' "$(cat "$test_root/keys/office.pub")"; } > "$host_keys"
  chmod 0600 "$host_keys"
  { cat "$test_root/keys/owner.pub"
    printf 'from="127.0.0.1,::1",no-agent-forwarding %s subyard-herdr:desk-old\n' "$(bare desk)"
    cat "$test_root/keys/manual.pub"; } > "$HERDR_TEST_GUEST_HOME/.ssh/authorized_keys"
  chmod 0600 "$HERDR_TEST_GUEST_HOME/.ssh/authorized_keys"
  missing="SHA256:$(printf '%043d' 0)"

  "$handler" pair --from-host | jq -e '.action == "pair-list" and .changed == false' >/dev/null
  SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=pair-list SUBYARD_OPERATION_ID=fixture \
    "$handler" pair --from-host > "$test_root/preview"
  printf '%s\n' "${fp[owner]}  user-owner  skipped:owner" "${fp[laptop]}  user-laptop  new" "${fp[phone]}  user-phone  new" \
    "${fp[desk]}  desk-old  granted" "${fp[manual]}  user-manual  unmanaged" \
    "${fp[office]}  user-office  skipped:options" 'line 7  unrecognized' \
    'Pair new keys with yard herdr pair --from-host --all or --from-host SHA256:FINGERPRINT...' | cmp - "$test_root/preview"

  (
    # A guest list over the helper's limit reads as unknown instead of failing the preview.
    guest_keys="$HERDR_TEST_GUEST_HOME/.ssh/authorized_keys"
    awk 'BEGIN { for (line = 0; line < 200000; line++) print "x" }' > "$guest_keys"
    SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=pair-list SUBYARD_OPERATION_ID=fixture \
      "$handler" pair --from-host > "$test_root/oversize"
    grep -Fxq "${fp[laptop]}  user-laptop  unknown" "$test_root/oversize"
  )
  { cat "$test_root/keys/owner.pub"
    printf 'from="127.0.0.1,::1",no-agent-forwarding %s subyard-herdr:desk-old\n' "$(bare desk)"
    cat "$test_root/keys/manual.pub"; } > "$HERDR_TEST_GUEST_HOME/.ssh/authorized_keys"

  "$handler" pair --from-host --all | jq -e '.action == "pair-add" and .changed and
    ([.consequences[] | capture("^authorize Herdr public key fingerprint:[0-9a-f]{64} \\((?<label>[^)]+)\\) from the host account").label]
      == ["user-laptop", "user-phone"])' >/dev/null
  : > "$HERDR_TEST_CALLS"
  SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=pair-add SUBYARD_OPERATION_ID=fixture \
    "$handler" pair --from-host --all > "$test_root/granted"
  printf 'add %s %s\n' user-laptop "$(bare laptop)" user-phone "$(bare phone)" desk-old "$(bare desk)" |
    cmp - "$HERDR_TEST_CALLS"
  grep -Fxq "Paired ${fp[laptop]}  user-laptop" "$test_root/granted"
  grep -Fxq "Already paired ${fp[desk]}  desk-old" "$test_root/granted"
  grep -Fxq 'hc machine add dev@fixture.ts.net:6768 --name fixture-host-default' "$test_root/granted"
  printf 'SSH config:\nHost fixture-host-default\n  HostName fixture.ts.net\n  Port 6768\n  User dev\n' |
    cmp - <(sed -n '/^SSH config:$/,$p' "$test_root/granted")

  "$handler" pair --from-host "${fp[desk]}" | jq -e '.action == "pair-existing" and .changed == false' >/dev/null
  : > "$HERDR_TEST_CALLS"
  SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=pair-existing SUBYARD_OPERATION_ID=fixture \
    "$handler" pair --from-host "${fp[desk]}" >/dev/null
  printf 'existing desk-old %s\n' "$(bare desk)" | cmp - "$HERDR_TEST_CALLS"
  if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=pair-add SUBYARD_OPERATION_ID=fixture \
    "$handler" pair --from-host "${fp[desk]}" >/dev/null 2>&1; then exit 1; fi

  # Every independent refusal is reported together.
  if result=$(YARD_KIND=vm HERDR_TEST_LISTENER=down "$handler" pair --from-host "${fp[manual]}" "${fp[office]}" "$missing" 2>&1); then exit 1; fi
  for expected in 'container yards only' "${fp[manual]} is already authorized in the yard outside Herdr" \
    "${fp[office]} has authorization options" "$missing is not in the owner account"; do
    [[ "$result" == *"$expected"* ]]
  done
  if result=$(HERDR_TEST_LISTENER=down "$handler" pair --from-host "${fp[owner]}" "${fp[office]}" 2>&1); then exit 1; fi
  [[ "$result" == *'ingress is not ready'*"${fp[owner]} is the Subyard owner SSH key"* ]]
  if result=$(YARD_KIND=vm HERDR_TEST_RUNNING=no HERDR_TEST_OWNER_FP=fail "$handler" pair --from-host --all 2>&1); then exit 1; fi
  [[ "$result" == *'container yards only'*'could not read the managed owner public key fingerprint'* ]]
  if result=$(HERDR_TEST_LISTENER=down "$handler" pair --from-host "${fp[office]}" 2>&1); then exit 1; fi
  [[ "$result" == *'ingress is not ready'*"${fp[office]} has authorization options"* ]]
  if result=$(HERDR_TEST_RUNNING=no "$handler" pair --from-host --all 2>&1); then exit 1; fi
  [[ "$result" == *'yard is not running'* ]]
  chmod 0620 "$host_keys"
  if result=$(HERDR_TEST_LISTENER=down "$handler" pair --from-host --all 2>&1); then exit 1; fi
  [[ "$result" == *'ingress is not ready'*"could not read the owner account's authorized_keys"* ]]
  chmod 0600 "$host_keys"
  for arguments in '--all extra' 'not-a-fingerprint' '--name laptop'; do
    # shellcheck disable=SC2086 # Split the argument fixture.
    if "$handler" pair --from-host $arguments >/dev/null 2>&1; then exit 1; else [ "$?" -eq 2 ]; fi
  done
)

(
  # The machine label is <host-id>-<yard>; the persisted host ID wins over the hostname.
  export SUBYARD_CONFIG_HOME="$test_root/label" SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=status SUBYARD_OPERATION_ID=fixture
  mkdir -m 0700 "$SUBYARD_CONFIG_HOME"
  printf 'Owner-Host\n' > "$SUBYARD_CONFIG_HOME/host-id"
  "$handler" status > "$test_root/status"
  grep -Fxq 'SSH endpoint: dev@fixture.ts.net:6768' "$test_root/status"
  grep -Fxq 'hc machine add dev@fixture.ts.net:6768 --name owner-host-default' "$test_root/status"
  grep -Fxq 'Host owner-host-default' "$test_root/status"
  YARD_NAME=work "$handler" status > "$test_root/status"
  grep -Fxq 'Host owner-host-work' "$test_root/status"
  printf 'a-host-id-longer-than-the-limit\n' > "$SUBYARD_CONFIG_HOME/host-id"
  "$handler" status > "$test_root/status"
  grep -Fxq 'hc machine add dev@fixture.ts.net:6768' "$test_root/status"
  grep -q '^Machine label unavailable' "$test_root/status"
  if grep -q '^Host ' "$test_root/status"; then exit 1; fi
)

(
  # Down is complete only once checkout discovery is disabled too.
  export HERDR_TEST_CORE_UNIT=disabled HERDR_TEST_CORE=stopped HERDR_TEST_DROPIN=missing HERDR_TEST_ROUTE='{}'
  HERDR_TEST_TIMER=disabled "$handler" down | jq -e '.changed == false' >/dev/null
  "$handler" down | jq -e '.changed' >/dev/null
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
# An enabled Herdr keeps its checkout discovery timer across a runtime refresh.
grep -o 'systemctl .*' "$HERDR_TEST_CALLS" > "$test_root/runtime-units"
printf '%s\n' 'systemctl daemon-reload' 'systemctl is-enabled --quiet subyard-herdr-core.service' \
  'systemctl enable --now --quiet subyard-herdr-discovery.timer' | cmp - "$test_root/runtime-units"
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

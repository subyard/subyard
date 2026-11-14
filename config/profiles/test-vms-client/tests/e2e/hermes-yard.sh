#!/usr/bin/env bash
# Delivery into a real Hermes-preset yard on the leased owner VM: the supported installer,
# `provision test-vms-client`, client and skill without a checkout or coding tools, the
# Hermes link, retirement on deselection, and refusal in a role without host access.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARD_NAME=''
CANDIDATE_ENGINE=''
SKILL=/usr/local/share/subyard/skills/subyard-test-vms
LINKS='.agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills .codex/skills'

die() { printf 'test-vms-client-yard: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
[ "$(id -un)" = dev ] || die 'run as the allocated dev user'
for command in incus jq sg sudo timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
  && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
  printf -v reexec 'exec env SUBYARD_E2E_VM=%q bash %q' "$SUBYARD_E2E_VM" "$ROOT/config/profiles/test-vms-client/tests/e2e/hermes-yard.sh"
  exec sg incus-admin -c "$reexec"
fi

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard_named() {
  local name="$1" command
  shift
  # Init refreshes its child session; the fixture's parent keeps the old groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v command '%q ' "$CANDIDATE_ENGINE" -Y "$name" "$@"
    sg incus-admin -c "exec $command"
  else
    "$CANDIDATE_ENGINE" -Y "$name" "$@"
  fi
}
yard() { yard_named "$YARD_NAME" "$@"; }

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$YARD_NAME" ] && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ]; then
    yard teardown --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-test-vms-client.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = test-vms-client-yard-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  printf 'TEST_VMS_CLIENT_YARD_RESULT exit_code=%s\n' "$rc"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

STATE="$(mktemp -d /var/tmp/subyard-test-vms-client.XXXXXX)"
printf '%s\n' test-vms-client-yard-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="tvc-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME"

# A release run installs the frozen packaged candidate; development runs package the
# current worktree. Both go through the supported installer.
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$STATE/incus-foundation"
  export SUBYARD_USER=dev SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
  export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
  set -a
  # shellcheck source=config/host.env
  . "$ROOT/config/host.env"
  set +a
  bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
)
timeout 10 /usr/bin/incus query /1.0 >/dev/null
printf 'STORAGE_PATH=%s\n' "$STORAGE_PATH" > "$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
export YARD_RUNTIME_ROOT="$STATE/runtime"
unset YARD_ENGINE_PATH
CANDIDATE_ENGINE="$(release_candidate_install "$ROOT" "$STATE")"

# The shipped Hermes preset: no coding-tool integration, no sudo, no host links.
definition="$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
install -d -m 0700 "$(dirname "$definition")"
port=$((35000 + ($$ % 15000)))
{
  grep -v '^SSH_PORT=' "$ROOT/config/profiles/hermes/yard.env"
  printf 'SSH_PORT=%s\nHOST_BASE=%s\nRESTRICTED_DISK_PATHS=%s\n' "$port" "$STATE/host" "$STATE/host"
} > "$definition"
chmod 0600 "$definition"
printf 'test-vms-client_e2e_stage=init\n'
yard init --yes
case "$(yard config show CODING_TOOL_INTEGRATIONS | sed -n 's/^effective: //p')" in
  ''|'<unset>') ;;
  *) die 'the Hermes preset gained a coding-tool integration' ;;
esac

printf 'test-vms-client_e2e_stage=provision\n'
yard provision test-vms-client --yes
yard start --yes
profiles="$(yard config show ENVIRONMENT_PROFILES | sed -n 's/^effective: //p')"
case " $profiles " in *' test-vms-client '*) ;; *) die "provision did not select the profile: $profiles" ;; esac
instance="yard-$YARD_NAME"
project="subyard-$YARD_NAME"
dev_uid="$(incus exec "$instance" --project "$project" -- id -u dev)"
dev_gid="$(incus exec "$instance" --project "$project" -- id -g dev)"
as_dev() {
  incus exec "$instance" --project "$project" --user "$dev_uid" --group "$dev_gid" \
    --env HOME=/home/dev --env SKILL="$SKILL" --env LINKS="$LINKS" -- "$@"
}

# shellcheck disable=SC2016
as_dev sh -euc '
fail() { printf "delivery assertion failed: %s\n" "$*" >&2; exit 1; }
for file in SKILL.md client.sh service-route.py; do
  test -f "$SKILL/$file" && test ! -L "$SKILL/$file" || fail "missing $file"
  test "$(stat -c %a:%u "$SKILL/$file")" = 644:0 || fail "$file is not root-owned 0644"
done
for adapter in $LINKS; do
  [ "$adapter" = .codex/skills ] && continue
  test "$(readlink -f "$HOME/$adapter/subyard-test-vms")" = "$SKILL" || fail "link $adapter does not resolve"
done
test -f "$HOME/.hermes/skills/subyard-test-vms/SKILL.md" || fail "Hermes cannot read the skill"
if find /srv/workspaces -maxdepth 4 -path "*/dev/agent-e2e.sh" 2>/dev/null | grep -q .; then
  fail "the yard contains a Subyard checkout"
fi
if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then fail "dev has passwordless sudo"; fi
test -d /var/lib/subyard/e2e-routes || fail "the route registry mount is missing"
bash "$SKILL/client.sh" --help | grep -Fq "acquire --slot N" || fail "client help is unavailable"
'
# The nested owner has no registered broker, so status reports the documented diagnostic.
set +e
status_output="$(as_dev bash "$SKILL/client.sh" status 2>&1)"
status_rc=$?
set -e
[ "$status_rc" = 2 ] \
  && grep -Fq 'subyard-test-vms: test environment unavailable: owner service resolution failed' <<<"$status_output" \
  || die "client status without a registered broker: rc=$status_rc $status_output"
printf '  [ ok ] client and skill delivered into the Hermes yard without a checkout\n'

printf 'test-vms-client_e2e_stage=deselect\n'
yard config set ENVIRONMENT_PROFILES 'hermes github' --scope yard --yes
yard init --yes
# shellcheck disable=SC2016
as_dev sh -euc '
fail() { printf "retirement assertion failed: %s\n" "$*" >&2; exit 1; }
test ! -e "$SKILL" && test ! -L "$SKILL" || fail "skill directory survived deselection"
for adapter in $LINKS; do
  test ! -e "$HOME/$adapter/subyard-test-vms" && test ! -L "$HOME/$adapter/subyard-test-vms" \
    || fail "link $adapter survived deselection"
done
'
incus exec "$instance" --project "$project" -- test ! -e /var/lib/subyard/agent-skills/profile-test-vms-client.json \
  || die 'the provider receipt survived deselection'
printf '  [ ok ] deselection and init retired the client, skill and links\n'

# The shipped role without host access refuses the profile before any guest effect.
refused="vpn-$token"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$refused"
{
  grep -v '^ENVIRONMENT_PROFILES=' "$ROOT/config/profiles/amnezia/yard.env"
  printf 'ENVIRONMENT_PROFILES="amnezia test-vms-client"\n'
} > "$SUBYARD_CONFIG_HOME/yards/$refused/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$refused/config.env"
set +e
refusal="$(yard_named "$refused" config show ENVIRONMENT_PROFILES 2>&1)"
refusal_rc=$?
set -e
[ "$refusal_rc" != 0 ] && grep -Fq 'profile "test-vms-client" requires host access' <<<"$refusal" \
  || die "a role without host access accepted the profile: $refusal"
rm -rf -- "${SUBYARD_CONFIG_HOME:?}/yards/$refused"
printf 'ok: Hermes yard delivery, retirement and host-access refusal\n'

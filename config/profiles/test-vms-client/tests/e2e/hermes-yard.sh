#!/usr/bin/env bash
# Delivery into a real Hermes-preset yard on the leased owner VM: the supported installer,
# `provision test-vms-client`, client and skill without a checkout or coding tools, the
# Hermes link, retirement on deselection, and refusal in a role without host access.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARD_NAME=''
REFUSED_YARD=''
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
  local original=$? cleanup_rc=0 teardown_rc=0
  trap - EXIT INT TERM
  set +e
  # The refusal definition is never initialized; remove it so it cannot affect teardown.
  if [ -n "$REFUSED_YARD" ]; then
    rm -rf -- "${SUBYARD_CONFIG_HOME:?}/yards/$REFUSED_YARD" || cleanup_rc=3
  fi
  if [ -n "$YARD_NAME" ] && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ]; then
    # A registered sentinel keeps the last-yard teardown from retiring the shared base
    # platform (bridge, storage pool and host guards) used by later leases' fixtures.
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    # Label the outcome so a refused teardown is not read as the failed stage's output.
    printf 'test-vms-client_e2e_cleanup=teardown state=start\n' >&2
    yard teardown --yes >&2 || teardown_rc=$?
    printf 'test-vms-client_e2e_cleanup=teardown state=end exit_code=%s\n' "$teardown_rc" >&2
    [ "$teardown_rc" -eq 0 ] || cleanup_rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-test-vms-client.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = test-vms-client-yard-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || cleanup_rc=3
  fi
  printf 'TEST_VMS_CLIENT_YARD_RESULT original_exit_code=%s cleanup_exit_code=%s\n' "$original" "$cleanup_rc"
  [ "$original" -eq 0 ] || exit "$original"
  exit "$cleanup_rc"
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
if [ ! -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
  command -v go >/dev/null 2>&1 || die 'Go is required to package the development candidate'
fi
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
# Phase 1 grants this session socket access through the native dispatcher helper.
set +e
CANDIDATE_ENGINE="$(release_candidate_prepare "$ROOT")"
candidate_status=$?
set -e
case "$candidate_status" in
  0) ;;
  1) "$ROOT/dev/build-engine.sh" --force ;;
  *) die 'frozen release candidate is invalid' ;;
esac
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$STATE/incus-foundation"
  export SUBYARD_USER=dev SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
  export SUBYARD_DISPATCHER_PATH="${CANDIDATE_ENGINE:-$ROOT/.build/yard}"
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
# Retirement is asserted on the running guest; start is a no-op when init kept it running.
yard start --yes
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
REFUSED_YARD="vpn-$token"
refused="$REFUSED_YARD"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$refused"
cp "$ROOT/config/profiles/amnezia/yard.env" "$SUBYARD_CONFIG_HOME/yards/$refused/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$refused/config.env"
# The supported enabling command reports every independent refusal of the selection.
set +e
refusal="$(yard_named "$refused" provision test-vms-client --yes 2>&1)"
refusal_rc=$?
set -e
[ "$refusal_rc" != 0 ] \
  && grep -Fq 'profile "test-vms-client" requires host access' <<<"$refusal" \
  && grep -Fq 'selected yard role permits only environment profile "amnezia"' <<<"$refusal" \
  || die "a role without host access accepted the profile: $refusal"
rm -rf -- "${SUBYARD_CONFIG_HOME:?}/yards/$refused"
REFUSED_YARD=''
printf 'ok: Hermes yard delivery, retirement and host-access refusal\n'

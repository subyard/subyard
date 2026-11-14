#!/usr/bin/env bash
# Bounded integration lifecycle on one disposable agent VM; no model/API calls.
set -Eeuo pipefail
umask 022
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KIND="${1:-container}"
case "$KIND" in container|cold|vm|special|default|upgrade|local-first) ;; *) printf 'usage: %s [container|cold|vm|special|default|upgrade|local-first]\n' "$0" >&2; exit 2 ;; esac
die() { printf 'integration-selection: %s\n' "$*" >&2; exit 1; }
trap 'printf "integration-selection: failure at line %s (exit %s)\n" "$LINENO" "$?" >&2' ERR
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run through dev/agent-e2e.sh on an allocated VM'
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] || die 'a frozen candidate bundle is required'
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
for command in jq python3 sudo ss sg; do command -v "$command" >/dev/null || die "$command is required"; done
if [ "$KIND" = local-first ]; then command -v git >/dev/null || die 'git is required'; fi
sudo -n true || die 'passwordless sudo is required on the disposable VM'
# Enroll the operator before the installed engine first inspects Incus.
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
if [ "$KIND" = cold ]; then
  # Only a fresh allocated host with no Incus workloads may become a cold fixture.
  # Keep the baseline state aside; package removal alone can retain the database.
  [ -x /usr/bin/incus ] || die 'cold fixture requires the prepared baseline client to verify empty state'
  sudo -n systemctl start incus.socket incus.service
  sudo -n /usr/bin/incus list --all-projects --format json | jq -e 'length == 0' >/dev/null \
    || die 'cold fixture requires no existing instances'
  sudo -n /usr/bin/incus storage list --format json | jq -e 'length == 0' >/dev/null \
    || die 'cold fixture requires no existing storage pools'
  sudo -n systemctl stop incus.service incus.socket
  sudo -n env DEBIAN_FRONTEND=noninteractive apt-get remove -y incus incus-client >/dev/null
  baseline="$(sudo -n mktemp -d /var/tmp/subyard-cold-incus.XXXXXX)"
  for directory in /var/lib/incus /run/incus; do
    if sudo -n test -e "$directory" || sudo -n test -L "$directory"; then
      sudo -n mv -- "$directory" "$baseline/$(printf '%s' "$directory" | tr / _)"
    fi
  done
  ! command -v incus >/dev/null || die 'cold fixture still has an Incus client'
  printf '  [ ok ] cold fixture has no Incus client, socket or state\n'
  KIND=container
fi
# VM-mode product creation requires host QEMU. Match the nested-teardown fixture;
# the ordinary Incus installer deliberately does not install virtualization packages.
if [ "$KIND" = vm ] && ! command -v qemu-system-x86_64 >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || die 'qemu-system-x86_64 or apt-get is required'
  printf '  [ .. ] installing QEMU for the allocated VM acceptance host\n'
  sudo -n apt-get update >/dev/null
  sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-system-x86 >/dev/null
fi
STATE="$(mktemp -d /var/tmp/subyard-integration-selection.XXXXXX)"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
MARKER="subyard-integration-selection-v1-$token"
# Keep nested VM virtiofs socket paths below the Unix socket path limit.
YARD_NAME="is-$token"
PROJECT="subyard-$YARD_NAME"
INSTANCE="yard-$YARD_NAME"
[ "$KIND" != default ] || YARD_NAME=default
printf '%s\n' "$MARKER" > "$STATE/.marker"
export SUBYARD_OPERATOR_HOME="$STATE/home"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/candidate/runtime"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
incus_stopped=0
command_with_group() {
  # First init can enroll this user, but cannot update its parent shell's groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$@"
    sg incus-admin -c "exec $command"
  else
    "$@"
  fi
}
yard() { command_with_group "$launcher" -Y "$YARD_NAME" "$@"; }
host_yard() { command_with_group "$launcher" "$@"; }
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
write_default_native_target() {
  # Yard-local names keep the cleanup sentinel from inspecting this live guest.
  printf 'INCUS_PROJECT=%s\nYARD_INSTANCE_NAME=%s\n' "$PROJECT" "$INSTANCE" >> "$config"
}
cleanup() {
  local rc=$? managed='' cleanup_rc=0
  trap - EXIT INT TERM ERR
  set +e
  if [ "$incus_stopped" = 1 ]; then sudo -n systemctl start incus.socket incus.service; fi
  if command -v /usr/bin/incus >/dev/null; then
    managed="$(incus config get "$INSTANCE" user.subyard.managed --project "$PROJECT" 2>/dev/null)"
  fi
  if [ -f "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" ] \
    && grep -Fqx "# $MARKER" "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"; then
    if [ -z "$managed" ] || [ "$managed" = true ]; then
      install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
      printf 'SSH_PORT=64997\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
      chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
      yard teardown --yes >"$STATE/cleanup.log" 2>&1 || {
        cleanup_rc=$?
        printf 'integration-selection: cleanup native_exit=%s bounded_log=true\n' "$cleanup_rc" >&2
        rc=3
      }
    else
      printf 'integration-selection: refusing to teardown unowned target\n' >&2
      rc=3
    fi
  fi
  if [ "$rc" = 0 ] && [[ "$STATE" = /var/tmp/subyard-integration-selection.* ]] \
    && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  else
    printf 'integration-selection: retained evidence at %s\n' "$STATE" >&2
    python3 -B "$ROOT/tests/helpers/failure-tails.py" "$STATE" "$MARKER" || true
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
launcher="$(command_with_group bash -c \
  '. "$1/tests/helpers/release-candidate.sh"; release_candidate_install "$1" "$2"' _ "$ROOT" "$STATE/candidate")"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"
port=$((33000 + ($$ % 1000)))
while ss -Hln "sport = :$port" 2>/dev/null | grep -q .; do port=$((port + 1)); done
config="$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
cat > "$config" <<EOF
# $MARKER
SSH_PORT=$port
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
ENVIRONMENT_PROFILES=
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
EOF
if [ "$KIND" = default ]; then
  write_default_native_target
  printf 'YARD_KIND=container\n' >> "$config"
elif [ "$KIND" = upgrade ] || [ "$KIND" = local-first ]; then
  printf 'YARD_KIND=container\n' >> "$config"
elif [ "$KIND" = special ]; then
  printf 'YARD_TEMPLATE=test-vms\nE2E_VM_SLOT_COUNT=1\n' >> "$config"
elif [ "$KIND" = vm ]; then
  # Incus 6.0 cannot hot-add these virtiofs mounts after its balloon device starts.
  # Match the nested-VM fixture; host-backed link retirement is covered by containers.
  printf 'YARD_KIND=vm\nHOST_MOUNTS=\nHOST_LINKS=\n' >> "$config"
else
  printf 'YARD_KIND=%s\n' "$KIND" >> "$config"
fi
# Ordinary modes keep package work bounded through an inherited empty selection.
# It is deliberately not a per-yard override: init must consume this inherited intent.
if [ "$KIND" != default ] && [ "$KIND" != special ]; then
  printf 'AGENTS=none\n' > "$SUBYARD_CONFIG_HOME/config.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
fi
chmod 0600 "$config"
# Keep only the retained test platform directory operator-owned; Incus children stay root-owned.
platform_root="$HOME/.cache/subyard-e2e-platform"
platform_marker="$platform_root/.subyard-e2e-platform-marker"
[ ! -L "$HOME/.cache" ] || die 'platform parent must not be a symlink'
if [ -e "$platform_root" ] || [ -L "$platform_root" ]; then
  [ -d "$platform_root" ] && [ ! -L "$platform_root" ] || die 'platform root must be a plain directory'
fi
if [ -e "$platform_marker" ] || [ -L "$platform_marker" ]; then
  [ -f "$platform_marker" ] && [ ! -L "$platform_marker" ] || die 'platform marker must be a plain file'
  [ "$(sudo -n cat "$platform_marker")" = subyard-e2e-platform-v1 ] || die 'unexpected platform marker'
fi
sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0711 "$platform_root"
# Product init owns baseline installation, including an absent Incus substrate.
yard init --yes
yard start --yes
incus info >/dev/null
incus storage show default --project default >/dev/null
incus network show incusbr0 --project default >/dev/null
printf '%s\n' subyard-e2e-platform-v1 > "$STATE/platform-marker"
sudo -n install -o "$(id -u)" -g "$(id -g)" -m 0600 "$STATE/platform-marker" "$platform_marker.$token"
sudo -n mv -fT -- "$platform_marker.$token" "$platform_marker"
[ "$(cat "$platform_marker")" = subyard-e2e-platform-v1 ] || die 'unexpected platform marker'
yard integration status --json > "$STATE/status.json"
if [ "$KIND" = default ]; then
  jq -e '.selection.requested == ["claude","codex","opencode","pi"] and .observed == "ready"' "$STATE/status.json" >/dev/null \
    || die 'fresh default did not receive the four approved integrations'
  guest sh -eu -c 'command -v codex; command -v opencode; test -f /home/dev/.claude/settings.json; test -f /home/dev/.pi/agent/settings.json'
  yard init --yes
  yard integration status --json | jq -e '.observed == "ready"' >/dev/null
  printf 'ok: fresh default converges four integrations without optional profiles\n'
  exit 0
fi
if [ "$KIND" != special ]; then
  jq -e '.selection.present and (.selection.requested | length) == 0 and (.selection.effective | length) == 0 and .observed == "ready"' "$STATE/status.json" >/dev/null \
    || die 'fresh named yard did not preserve the inherited empty selection'
fi
if [ "$KIND" = local-first ]; then
  # This source belongs only to the marked fixture; never register operator Git.
  source="$STATE/settings-source"
  checkout="$STATE/settings-checkout"
  host_id="$(cat "$SUBYARD_CONFIG_HOME/host-id")"
  source_yard="$source/hosts/$host_id/yards/$YARD_NAME/config.env"
  cached="$SUBYARD_CONFIG_HOME/.sync/settings/yards/$YARD_NAME/config.env"
  install -d -m 0700 "${source_yard%/*}"
  printf '{"schemaVersion":1}\n' > "$source/subyard-config.json"
  printf 'CODING_TOOL_INTEGRATIONS=\n' > "$source_yard"
  chmod 0600 "$source/subyard-config.json" "$source_yard"
  git -C "$source" init -q
  git -C "$source" add -- subyard-config.json "hosts/$host_id/yards/$YARD_NAME/config.env"
  git -C "$source" -c user.name='Subyard Test' -c user.email=test@invalid commit -qm 'Seed isolated integration settings'
  yard config sync connect "$source" --checkout "$checkout" --yes
  cmp "$source_yard" "$cached" || die 'connect did not cache Git yard settings'
  yard config unset CODING_TOOL_INTEGRATIONS --scope host --yes
  yard integration status --json | jq -e --arg path "$cached" \
    '.selection.requested == [] and .selection.provenance.Path == $path and .observed == "ready"' >/dev/null \
    || die 'initialized yard did not consume cached Git integration selection'
  yard init --yes
  if grep -q '^CODING_TOOL_INTEGRATIONS=' "$config"; then die 'init copied inherited Git selection locally'; fi
  before="$(sha256sum "$cached" "$source_yard")"
  yard integration enable claude --yes
  yard integration status --json | jq -e --arg path "$config" \
    '.selection.requested == ["claude"] and .selection.provenance.Path == $path and .observed == "ready"' >/dev/null \
    || die 'registered source prevented local integration activation'
  [ "$(sha256sum "$cached" "$source_yard")" = "$before" ] || die 'local activation edited Git settings'
  local_before="$(sha256sum "$config")"
  printf 'CODING_TOOL_INTEGRATIONS=codex\n' > "$source_yard"
  git -C "$source" add -- "hosts/$host_id/yards/$YARD_NAME/config.env"
  git -C "$source" -c user.name='Subyard Test' -c user.email=test@invalid commit -qm 'Change isolated fallback selection'
  yard config sync pull --yes
  cmp "$source_yard" "$cached" || die 'pull did not update the cached fallback'
  yard config sync --yes
  [ "$(sha256sum "$config")" = "$local_before" ] || die 'sync overwrote local integration selection'
  yard integration status --json | jq -e '.selection.requested == ["claude"] and .observed == "ready"' >/dev/null \
    || die 'refreshed Git fallback displaced the local integration selection'
  yard integration disable claude --yes
  yard integration status --json | jq -e --arg path "$config" \
    '.selection.requested == [] and .selection.effective == [] and .selection.provenance.Path == $path and .observed == "ready"' >/dev/null \
    || die 'explicit empty local selection failed to mask the Git fallback'
  printf 'ok: initialized container uses cached Git settings; local activation and explicit empty survive sync\n'
  exit 0
fi
if [ "$KIND" = special ]; then
  jq -e '.selection.allows_coding_tools == false and (.selection.effective | length) == 0 and .observed == "ready"' "$STATE/status.json" >/dev/null
  guest sh -eu -c 'for file in /usr/local/bin/ccusage /usr/local/bin/codex /usr/local/bin/opencode; do [ ! -e "$file" ]; done'
  # A role-forbidden utility without an ownership receipt stays and surfaces a conflict.
  guest sh -eu -c 'printf "#!/bin/sh\n" > /usr/local/bin/ccusage; chmod 0755 /usr/local/bin/ccusage'
  yard integration status --json >"$STATE/ccusage-conflict-status.json"
  jq -e '.observed == "conflict" and (.detail | contains("/usr/local/bin/ccusage"))' "$STATE/ccusage-conflict-status.json" >/dev/null \
    || die 'unowned role-forbidden utility did not surface an ownership conflict'
  guest test -f /usr/local/bin/ccusage || die 'ownership conflict removed the utility'
  guest rm -- /usr/local/bin/ccusage
  before="$(sha256sum "$config")"
  if yard integration enable claude --yes > "$STATE/forbidden.log" 2>&1; then die 'special role accepted coding integration'; fi
  [ "$(sha256sum "$config")" = "$before" ] || die 'role rejection changed desired selection'
  printf 'ok: fresh special yard excludes coding tools and ccusage\n'
  exit 0
fi

yard integration enable claude --yes
yard integration status --json | jq -e '.observed == "ready" and .selection.requested == ["claude"]' >/dev/null
# A host-level legacy request remains inherited; init must not freeze it per yard.
sed -i '/^CODING_TOOL_INTEGRATIONS=/d' "$config"
printf 'AGENTS=claude\n' > "$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
if [ "$KIND" = container ]; then
  before="$(sha256sum "$config" "$SUBYARD_CONFIG_HOME/config.env")"
  incus_stopped=1
  sudo -n systemctl stop incus.service incus.socket
  if yard init --yes > "$STATE/stopped-incus-init.log" 2>&1; then die 'init accepted an unavailable existing daemon'; fi
  [ "$(sha256sum "$config" "$SUBYARD_CONFIG_HOME/config.env")" = "$before" ] \
    || die 'stopped-daemon init changed inherited integration intent'
  sudo -n systemctl start incus.socket incus.service
  incus_stopped=0
  printf 'ok: unavailable existing Incus preserves inherited integration intent\n'
fi
yard init --yes
if grep -q '^CODING_TOOL_INTEGRATIONS=' "$config"; then die 'init froze an inherited legacy selection per yard'; fi
yard integration status --json | jq -e '.selection.requested == ["claude"] and .observed == "ready"' >/dev/null
# A legacy alias explicitly authored in the yard remains the one migration case.
printf 'AGENTS=none\n' > "$SUBYARD_CONFIG_HOME/config.env"
printf 'AGENTS=claude\n' >> "$config"
yard init --yes
grep -Eq '^CODING_TOOL_INTEGRATIONS=.*claude' "$config" || die 'explicit local legacy selection was not canonicalized'
if grep -q '^AGENTS=' "$config"; then die 'canonicalization retained the local legacy alias'; fi
yard integration status --json | jq -e '.selection.requested == ["claude"] and .observed == "ready"' >/dev/null
inventory_before="$(guest sha256sum /var/lib/subyard/integrations/inventory.json)"
yard integration enable claude --yes
[ "$(guest sha256sum /var/lib/subyard/integrations/inventory.json)" = "$inventory_before" ] || die 'healthy repeated enable rewrote inventory'
history_path=/home/dev/.claude/projects/selection-history
preserved_history=/mnt/host/agent-sessions/claude/projects/selection-history
if [ "$KIND" = vm ]; then
  history_path=/home/dev/.claude/history.jsonl
  preserved_history="$history_path"
fi
guest sh -eu -c '
  printf "%s\n" synthetic-auth > /home/dev/.claude/auth.json
  printf "%s\n" synthetic-history > "$1"
  python3 -c '\''import json; p="/home/dev/.claude/settings.json"; x=json.load(open(p)); x["permissions"]["defaultMode"]="plan"; open(p,"w").write(json.dumps(x))'\''
' subyard "$history_path"
# Installed completed history permits only a source reduction here. The changed
# owned field no longer proves Subyard's values: init releases its ownership and
# leaves the document unchanged; the release repair must not restore it first.
guest sha256sum /home/dev/.claude/settings.json /var/lib/subyard/integrations/inventory.json \
  /home/dev/.claude/auth.json "$history_path" > "$STATE/drift-guest.before"
guest sha256sum /home/dev/.claude/settings.json > "$STATE/drift-settings.before"
guest sha256sum /home/dev/.claude/auth.json "$preserved_history" > "$STATE/retained-data.before"
yard integration disable claude --yes > "$STATE/drift.log" 2>&1
yard integration status --json > "$STATE/drift-status.json"
jq -e '.selection.requested == [] and .selection.effective == [] and .observed == "pending" and
  (.detail | contains("retiring claude releases ownership of /home/dev/.claude/settings.json: owned fields changed after Subyard applied them"))' \
  "$STATE/drift-status.json" >/dev/null || die 'source-only disable did not preview the ownership release'
guest sha256sum -c - < "$STATE/drift-guest.before" >/dev/null \
  || die 'source-only disable changed guest settings, inventory, auth or history'
before="$(sha256sum "$config")"
if host_yard migrate --yes > "$STATE/drift-migrate.log" 2>&1; then die 'release repair restored changed owned fields'; fi
host_yard migrate --check --json > "$STATE/drift-migrate.json"
jq -e --arg retry "'yard' '-Y' '$YARD_NAME' 'init'" '.outcome.status == "operator-action-required" and
  .outcome.code == "dependency-unavailable" and
  any(.blockers[]?; .resource == "activation.materialized-config.pending-configuration" and .retry == $retry)' \
  "$STATE/drift-migrate.json" >/dev/null || die 'release repair did not leave the ownership release to init'
[ "$(sha256sum "$config")" = "$before" ] || die 'refused repair changed desired selection'
guest sha256sum -c - < "$STATE/drift-guest.before" >/dev/null \
  || die 'refused repair changed guest settings, inventory, auth or history'
yard init --yes > "$STATE/drift-init.log" 2>&1 || { tail -n 20 "$STATE/drift-init.log" >&2; die 'init did not release changed owned fields'; }
grep -Fq "yard $YARD_NAME: released ownership of /home/dev/.claude/settings.json: owned fields changed" "$STATE/drift-init.log" \
  || die 'init did not report the ownership release'
guest sha256sum -c - < "$STATE/drift-settings.before" >/dev/null || die 'ownership release changed the document'
host_yard migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null || die 'released yard is not release-ready'
yard integration status --json | jq -e '.observed == "ready" and .selection.requested == [] and .selection.effective == []' >/dev/null \
  || die 'released owned configuration did not converge'
guest sha256sum -c - < "$STATE/retained-data.before" >/dev/null \
  || die 'retirement changed auth or history'
yard integration disable claude --yes
guest sha256sum -c - < "$STATE/retained-data.before" >/dev/null \
  || die 'repeated disable changed auth or history'
guest sha256sum -c - < "$STATE/drift-settings.before" >/dev/null || die 'repeated disable changed released fields'
# Init first: a fresh enable owns the shipped fields again; after another change
# and source-only disable, plain init releases them without a migrate.
yard integration enable claude --yes
guest python3 -c 'import json; p="/home/dev/.claude/settings.json"; x=json.load(open(p)); x["permissions"]["defaultMode"]="plan"; open(p,"w").write(json.dumps(x))'
guest sha256sum /home/dev/.claude/settings.json > "$STATE/drift-settings.before"
yard integration disable claude --yes > "$STATE/drift-disable-init-first.log" 2>&1
yard init --yes > "$STATE/drift-init-first.log" 2>&1 || { tail -n 20 "$STATE/drift-init-first.log" >&2; die 'init-first release failed'; }
grep -Fq "yard $YARD_NAME: released ownership of /home/dev/.claude/settings.json: owned fields changed" "$STATE/drift-init-first.log" \
  || die 'init-first release was not reported'
guest sha256sum -c - < "$STATE/drift-settings.before" >/dev/null || die 'init-first release changed the document'
host_yard migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null || die 'init-first released yard is not release-ready'
guest sh -eu -c '
  [ "$(cat /home/dev/.claude/auth.json)" = synthetic-auth ]
  [ ! -L /home/dev/.claude/projects ]
  [ "$(cat "$1")" = synthetic-history ]
  python3 -c '\''import json; assert json.load(open("/home/dev/.claude/settings.json"))["permissions"]["defaultMode"] == "plan"'\''
' subyard "$preserved_history"
# A package-local failure happens after persistent desired write, and retry resumes it.
printf '#!/bin/sh\nexit 71\n' > "$STATE/claude-provision.sh"
printf 'AGENT_claude_PROVISION=%s\nAGENT_claude_COMMAND=sh\n' "$STATE/claude-provision.sh" >> "$config"
if yard integration enable claude --yes > "$STATE/failure.log" 2>&1; then die 'synthetic package failure unexpectedly succeeded'; fi
yard integration status --json | jq -e '.selection.requested == ["claude"] and .observed == "pending"' >/dev/null
printf '#!/bin/sh\nexit 0\n' > "$STATE/claude-provision.sh"
yard integration enable claude --yes
yard integration disable claude --yes
# Stopped mutation refuses both changed and already-empty requests without writes/start.
yard stop --yes
before="$(sha256sum "$config")"
for verb in enable disable; do
  if yard integration "$verb" claude --yes > "$STATE/stopped-$verb.log" 2>&1; then die "stopped $verb was accepted"; fi
  [ "$(sha256sum "$config")" = "$before" ] || die 'stopped mutation wrote desired config'
  [ "$(incus list "$INSTANCE" --project "$PROJECT" --format json | jq -r '.[0].status')" = Stopped ] || die 'stopped mutation started yard'
done
yard start --yes
yard integration status --json | jq -e '.observed == "ready"' >/dev/null
if [ "$KIND" = upgrade ]; then
  yard integration enable claude --yes
  guest sh -eu -c 'install -d /srv/workspaces/selection-preserve/src; printf "%s\n" keep > /srv/workspaces/selection-preserve/src/data'
  # Inherited requested tools remain visible, while this role suppresses their effective set.
  sed -i '/^CODING_TOOL_INTEGRATIONS=/d; /^YARD_KIND=/d' "$config"
  printf 'AGENTS=claude\n' > "$SUBYARD_CONFIG_HOME/config.env"
  printf 'YARD_TEMPLATE=test-vms\nE2E_VM_SLOT_COUNT=1\n' >> "$config"
  yard init --yes
  yard integration status --json | jq -e '.selection.allows_coding_tools == false and .selection.requested == ["claude"] and (.selection.effective | length) == 0 and .observed == "ready"' >/dev/null
  guest sh -eu -c '
    [ ! -e /usr/local/bin/ccusage ]
    [ ! -L /home/dev/.claude/projects ]
    [ "$(cat /home/dev/.claude/auth.json)" = synthetic-auth ]
    [ "$(cat /srv/workspaces/selection-preserve/src/data)" = keep ]
    systemctl is-active --quiet subyard-test-vms-broker.service
    test -s /home/dev/.ssh/authorized_keys
  '
  yard init --yes
  printf 'ok: upgraded special role retires owned coding wiring and ccusage; broker, keys and workspace remain\n'
fi
printf 'ok: %s selection, no-op, safe retirement, changed-field ownership release, failure/retry and stopped rejection\n' "$KIND"

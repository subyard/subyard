#!/usr/bin/env bash
# Bounded integration lifecycle on one disposable agent VM; no model/API calls.
set -Eeuo pipefail
umask 022
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KIND="${1:-container}"
case "$KIND" in container|cold|vm|special|default|upgrade) ;; *) printf 'usage: %s [container|cold|vm|special|default|upgrade]\n' "$0" >&2; exit 2 ;; esac
die() { printf 'integration-selection: %s\n' "$*" >&2; exit 1; }
trap 'printf "integration-selection: failure at line %s (exit %s)\n" "$LINENO" "$?" >&2' ERR
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run through dev/agent-e2e.sh on an allocated VM'
for command in jq python3 sudo ss sg; do command -v "$command" >/dev/null || die "$command is required"; done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
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
[ -x "$ROOT/.build/yard" ] || "$ROOT/dev/build-engine.sh"
STATE="$(mktemp -d /var/tmp/subyard-integration-selection.XXXXXX)"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
MARKER="subyard-integration-selection-v1-$token"
# Keep nested VM virtiofs socket paths below the Unix socket path limit.
YARD_NAME="is-$token"
PROJECT="subyard-$YARD_NAME"
INSTANCE="yard-$YARD_NAME"
[ "$KIND" != default ] || YARD_NAME=default
printf '%s\n' "$MARKER" > "$STATE/.marker"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
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
yard() {
  # First init can enroll this user, but cannot update its parent shell's groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"
    sg incus-admin -c "exec $command"
  else
    "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"
  fi
}
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
cleanup() {
  local rc=$? managed=''
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
      yard teardown --yes >"$STATE/cleanup.log" 2>&1 || rc=3
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
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM
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
  printf 'INCUS_PROJECT=%s\nYARD_INSTANCE_NAME=%s\n' "$PROJECT" "$INSTANCE" > "$SUBYARD_CONFIG_HOME/config.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
  printf 'YARD_KIND=container\n' >> "$config"
elif [ "$KIND" = upgrade ]; then
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
  jq -e '.selection.requested == ["claude","codex","opencode","pi","aiobserver"] and .observed == "ready"' "$STATE/status.json" >/dev/null \
    || die 'fresh default did not receive the five approved integrations'
  guest sh -eu -c 'command -v codex; command -v opencode; command -v ai-observer; test -f /home/dev/.claude/settings.json; test -f /home/dev/.pi/agent/settings.json'
  yard init --yes
  yard integration status --json | jq -e '.observed == "ready"' >/dev/null
  sentinel="/srv/agents/ai-observer/data/.selection-preservation-$token"
  guest sh -eu -c '[ ! -e "$1" ]; printf "%s\n" "$2" > "$1"' subyard "$sentinel" "$MARKER"
  yard integration disable aiobserver --yes
  yard integration status --json > "$STATE/observer-disabled-status.json"
  jq -c '{observed, detail, effective: .selection.effective}' "$STATE/observer-disabled-status.json"
  jq -e '.observed == "ready" and (.selection.effective | index("aiobserver") | not)' "$STATE/observer-disabled-status.json" >/dev/null \
    || die 'observer disable did not converge to ready with observer excluded'
  guest sh -eu -c '
    active=$(systemctl show -p ActiveState --value subyard-ai-observer.service) || active=unavailable
    enabled=$(systemctl show -p UnitFileState --value subyard-ai-observer.service) || enabled=unavailable
    running=$(docker inspect -f "{{.State.Running}}" subyard-ai-observer 2>/dev/null) || running=unavailable
    preserved=false
    if [ "$(cat "$1" 2>/dev/null)" = "$2" ]; then preserved=true; fi
    printf "observer disabled: ActiveState=%s UnitFileState=%s dockerRunning=%s sentinelPreserved=%s\n" "$active" "$enabled" "$running" "$preserved"
    case "$active" in inactive|failed) ;; *) exit 1 ;; esac
    [ "$enabled" = disabled ] && [ "$running" = false ] && [ "$preserved" = true ]
  ' subyard "$sentinel" "$MARKER" || die 'observer disabled service/container/data assertion failed'
  for key in user.subyard.ai_observer_provision user.subyard.ai_observer_proxy; do
    [ -z "$(incus config get "$INSTANCE" "$key" --project "$PROJECT")" ] || die "observer disable retained $key"
  done
  if incus config device list "$INSTANCE" --project "$PROJECT" | grep -qx ai-observer; then
    die 'observer disable retained its dashboard proxy'
  fi
  yard integration enable aiobserver --yes
  yard integration status --json > "$STATE/observer-enabled-status.json"
  jq -c '{observed, detail, effective: .selection.effective}' "$STATE/observer-enabled-status.json"
  jq -e '.observed == "ready" and (.selection.effective | index("aiobserver") != null)' "$STATE/observer-enabled-status.json" >/dev/null \
    || die 'observer enable did not converge to ready with observer selected'
  guest sh -eu -c '
    [ "$(cat "$1")" = "$2" ]
    systemctl is-active --quiet subyard-ai-observer.service
  ' subyard "$sentinel" "$MARKER" \
    || die 'observer enable did not preserve data and activate its service'
  printf 'ok: fresh logical default converges five integrations; observer disable/enable preserves data and retires service/proxy\n'
  exit 0
fi
jq -e '.selection.present and (.selection.requested | length) == 0 and (.selection.effective | length) == 0 and .observed == "ready"' "$STATE/status.json" >/dev/null \
  || die 'fresh named yard did not converge to explicit empty selection'
if [ "$KIND" = container ]; then
  paseo_sentinel="/srv/agents/paseo/data/.cleanup-preservation-$token"
  guest sh -eu -c '
    [ ! -e /var/lib/subyard/paseo-ownership ]
    [ ! -e /etc/systemd/system/paseo.service ]
    [ ! -e /etc/systemd/system/paseo.service.subyard-retired ]
    install -d -o root -g root -m 0755 /srv/agents/paseo/data
    printf "%s\n" "$1" >"$2"
    chown root:root "$2"
    chmod 0600 "$2"
    printf "%s\n" \
      "[Unit]" \
      "Description=Synthetic Paseo cleanup fixture" \
      "[Service]" \
      "ExecStart=/usr/bin/sleep infinity" \
      "[Install]" \
      "WantedBy=multi-user.target" \
      >/etc/systemd/system/paseo.service
    chown root:root /etc/systemd/system/paseo.service
    chmod 0644 /etc/systemd/system/paseo.service
    systemctl daemon-reload
    systemctl enable --now paseo.service
  ' subyard "$MARKER" "$paseo_sentinel"
  yard integration status --json >"$STATE/paseo-conflict-status.json"
  jq -e '.selection.requested == [] and .selection.effective == [] and .observed == "conflict" and (.detail | contains("paseo.service"))' \
    "$STATE/paseo-conflict-status.json" >/dev/null \
    || die 'unselected unowned Paseo service did not surface an ownership conflict'
  paseo_before="$(guest sha256sum /etc/systemd/system/paseo.service "$paseo_sentinel")"
  yard integration cleanup paseo --check >"$STATE/paseo-cleanup-check.log"
  grep -Fq 'Integration cleanup: paseo' "$STATE/paseo-cleanup-check.log" \
    && grep -Fq 'Stop and disable paseo.service' "$STATE/paseo-cleanup-check.log" \
    && grep -Fq 'paseo.service.subyard-retired' "$STATE/paseo-cleanup-check.log" \
    || die 'Paseo cleanup check omitted its bounded consequences'
  [ "$(guest sha256sum /etc/systemd/system/paseo.service "$paseo_sentinel")" = "$paseo_before" ] \
    || die 'Paseo cleanup check changed the unit or preserved data'
  guest sh -eu -c '
    systemctl is-active --quiet paseo.service
    systemctl is-enabled --quiet paseo.service
    [ ! -e /etc/systemd/system/paseo.service.subyard-retired ]
    [ ! -e /var/lib/subyard/paseo-ownership ]
  ' subyard || die 'Paseo cleanup check changed service state or ownership evidence'
  if yard integration cleanup paseo </dev/null >"$STATE/paseo-cleanup-eof.log" 2>&1; then
    die 'EOF accepted Paseo cleanup'
  fi
  [ "$(guest sha256sum /etc/systemd/system/paseo.service "$paseo_sentinel")" = "$paseo_before" ] \
    || die 'declined Paseo cleanup changed the unit or preserved data'
  guest sh -eu -c '
    systemctl is-active --quiet paseo.service
    systemctl is-enabled --quiet paseo.service
    [ ! -e /etc/systemd/system/paseo.service.subyard-retired ]
    [ ! -e /var/lib/subyard/paseo-ownership ]
  ' subyard || die 'declined Paseo cleanup changed service state or ownership evidence'
  yard integration cleanup paseo --yes >"$STATE/paseo-cleanup-apply.log"
  guest sh -eu -c '
    [ ! -e /etc/systemd/system/paseo.service ]
    [ -f /etc/systemd/system/paseo.service.subyard-retired ]
    [ "$(stat -c "%u:%g:%a:%h" /etc/systemd/system/paseo.service.subyard-retired)" = 0:0:644:1 ]
    grep -Fqx "ExecStart=/usr/bin/sleep infinity" /etc/systemd/system/paseo.service.subyard-retired
    [ "$(cat "$1")" = "$2" ]
    [ ! -e /var/lib/subyard/paseo-ownership ]
    ! systemctl is-active --quiet paseo.service
    ! systemctl is-enabled --quiet paseo.service
  ' subyard "$paseo_sentinel" "$MARKER" \
    || die 'confirmed Paseo cleanup did not retire only the service unit'
  paseo_retired_before="$(guest sha256sum /etc/systemd/system/paseo.service.subyard-retired "$paseo_sentinel")"
  yard integration cleanup paseo --yes >"$STATE/paseo-cleanup-retry.log"
  [ "$(guest sha256sum /etc/systemd/system/paseo.service.subyard-retired "$paseo_sentinel")" = "$paseo_retired_before" ] \
    || die 'converged Paseo cleanup retry changed the backup or preserved data'
  grep -Fq 'No cleanup needed.' "$STATE/paseo-cleanup-retry.log" \
    || die 'converged Paseo cleanup retry did not report a no-op'
  yard integration status --json | jq -e '.observed == "ready" and .selection.requested == [] and .selection.effective == []' >/dev/null \
    || die 'Paseo cleanup did not restore ready unselected integration status'
fi
if [ "$KIND" = special ]; then
  jq -e '.selection.allows_coding_tools == false' "$STATE/status.json" >/dev/null
  guest sh -eu -c 'for file in /usr/local/bin/ccusage /usr/local/bin/codex /usr/local/bin/opencode /usr/local/bin/paseo; do [ ! -e "$file" ]; done'
  before="$(sha256sum "$config")"
  if yard integration enable claude --yes > "$STATE/forbidden.log" 2>&1; then die 'special role accepted coding integration'; fi
  [ "$(sha256sum "$config")" = "$before" ] || die 'role rejection changed desired selection'
  printf 'ok: fresh special yard excludes coding tools and ccusage\n'
  exit 0
fi

yard integration enable claude --yes
yard integration status --json | jq -e '.observed == "ready" and .selection.requested == ["claude"]' >/dev/null
# Adopt a trustworthy inherited legacy request into this existing yard only.
sed -i '/^CODING_TOOL_INTEGRATIONS=/d' "$config"
printf 'AGENTS=claude\n' > "$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
if [ "$KIND" = container ]; then
  before="$(sha256sum "$config" "$SUBYARD_CONFIG_HOME/config.env")"
  incus_stopped=1
  sudo -n systemctl stop incus.service incus.socket
  if yard init --yes > "$STATE/stopped-incus-init.log" 2>&1; then die 'init accepted an unavailable existing daemon'; fi
  grep -Fq 'cannot establish existing integration intent' "$STATE/stopped-incus-init.log" \
    || die 'stopped-daemon init failed outside the integration intent guard'
  [ "$(sha256sum "$config" "$SUBYARD_CONFIG_HOME/config.env")" = "$before" ] \
    || die 'stopped-daemon init changed inherited integration intent'
  sudo -n systemctl start incus.socket incus.service
  incus_stopped=0
  printf 'ok: unavailable existing Incus preserves inherited integration intent\n'
fi
yard init --yes
grep -Eq '^CODING_TOOL_INTEGRATIONS=.*claude' "$config" || die 'existing requested selection was not adopted'
printf '' > "$SUBYARD_CONFIG_HOME/config.env"
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
  cp /home/dev/.claude/settings.json /tmp/subyard-selection-settings.json
  python3 -c '\''import json; p="/home/dev/.claude/settings.json"; x=json.load(open(p)); x["permissions"]["defaultMode"]="plan"; open(p,"w").write(json.dumps(x))'\''
' subyard "$history_path"
before="$(sha256sum "$config")"
if yard integration disable claude --yes > "$STATE/drift.log" 2>&1; then die 'owned configuration drift was silently deleted'; fi
[ "$(sha256sum "$config")" = "$before" ] || die 'assessment conflict changed desired selection'
guest cp /tmp/subyard-selection-settings.json /home/dev/.claude/settings.json
yard integration disable claude --yes
guest sh -eu -c '
  [ "$(cat /home/dev/.claude/auth.json)" = synthetic-auth ]
  [ ! -L /home/dev/.claude/projects ]
  [ "$(cat "$1")" = synthetic-history ]
  python3 -c '\''import json; assert "permissions" not in json.load(open("/home/dev/.claude/settings.json"))'\''
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
  # Existing inherited tools are suppressed; authored forbidden nonempty is separately rejected.
  sed -i '/^CODING_TOOL_INTEGRATIONS=/d; /^YARD_KIND=/d' "$config"
  printf 'AGENTS=claude\n' > "$SUBYARD_CONFIG_HOME/config.env"
  printf 'YARD_TEMPLATE=test-vms\nE2E_VM_SLOT_COUNT=1\n' >> "$config"
  yard init --yes
  yard integration status --json | jq -e '.selection.allows_coding_tools == false and (.selection.effective | length) == 0 and .observed == "ready"' >/dev/null
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
printf 'ok: %s selection, no-op, safe retirement, conflict, failure/retry and stopped rejection\n' "$KIND"

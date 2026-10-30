#!/usr/bin/env bash
# One disposable VM: installed helper facts and native core skill discovery.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [ "${1:-}" = --preflight ]; then
  bash -n "$0"
  python3 -B "$root/config/skills/tests/guest-facts-test.py"
  printf 'guest environment fixture preflight=PASS; requires one standard VM, 14 GiB disk, 2 GiB available RAM, product platform preparation, frozen candidate\n'
  exit
fi
[ "$#" = 0 ] && [ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] || exit 2
[ "$(df -B1 --output=avail /var/tmp | tail -n 1)" -ge 15032385536 ]
[ "$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)" -ge 2097152 ]
# Reuse the product initializer and existing real-Incus image preflight.
bash "$root/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
[ "$(sudo -n cat "$HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker")" = subyard-e2e-platform-v1 ]
# shellcheck source=tests/helpers/release-candidate.sh
. "$root/tests/helpers/release-candidate.sh"
state="$(mktemp -d /var/tmp/subyard-guest-environment.XXXXXX)"
printf '%s\n' subyard-guest-environment-v1 > "$state/.marker"
export SUBYARD_OPERATOR_HOME="$state/home" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export YARD_RUNTIME_ROOT="$state/runtime" SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH
STORAGE_PATH="$(sudo -n /usr/bin/incus storage get default source)"
install -d -m 0700 "$SUBYARD_OPERATOR_HOME"
command_with_group() {
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local quoted
    printf -v quoted '%q ' "$@"
    sg incus-admin -c "exec $quoted"
  else "$@"; fi
}
name="facts-${SUBYARD_E2E_RUN_ID:?}"
launcher=''; project=''; instance=''; collision_injected=0
yard() { command_with_group "$launcher" -Y "$name" "$@"; }
guest() { sudo -n /usr/bin/incus exec "$instance" --project "$project" -- "$@"; }
cleanup() {
  result=$?; cleanup_result=0
  workload_result=$result
  trap - EXIT
  if [ -n "$launcher" ] && [ -n "$project" ]; then
    # Retire only this fixture's injected empty collision, then authorize any
    # remaining static repair before teardown's ordinary migration guard.
    if [ "$collision_injected" = 1 ]; then
      guest rmdir /home/dev/.claude/skills/subyard-environment || cleanup_result=3
    fi
    command_with_group "$launcher" migrate --yes || cleanup_result=3
    yard teardown --yes || cleanup_result=3
  fi
  if [ "$(cat "$state/.marker")" = subyard-guest-environment-v1 ]; then
    sudo -n find "$state" -depth -delete || cleanup_result=3
  else cleanup_result=3; fi
  [ "$workload_result" != 0 ] || result=$cleanup_result
  printf 'guest environment workload_exit=%s cleanup_exit=%s overall_exit=%s\n' "$workload_result" "$cleanup_result" "$result"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
launcher="$(command_with_group bash -c '. "$1/tests/helpers/release-candidate.sh"; release_candidate_install "$1" "$2"' _ "$root" "$state")"
version="$("$launcher" --version)"; version="${version#yard }"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
port=$((30000 + $$ % 10000))
while ss -H -ltn "sport = :$port" | grep -q .; do port=$((port+1)); done
printf 'ENVIRONMENT_PROFILES=\nCODING_TOOL_INTEGRATIONS=\nFORWARD_SSH_AGENT=0\nSSH_PORT=%s\n' "$port" > "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
yard init --yes
project="$(yard config show INCUS_PROJECT | sed -n 's/^effective: //p')"
instance="$(yard config show YARD_INSTANCE_NAME | sed -n 's/^effective: //p')"
yard start --yes
yard provision --yes
runtime="$(readlink -f "$state/runtime/current")"
reader=/usr/local/share/subyard/skills/subyard-environment/facts.py
# Plain nonlogin shell, arbitrary project, no source checkout or inherited context.
app="$(guest runuser -u dev -- mktemp -d /tmp/subyard-facts-project.XXXXXX)"
guest runuser -u dev -- git init --quiet "$app"
read_facts() {
  guest runuser -u dev -- env -i HOME=/home/dev PATH=/usr/bin:/bin sh -c 'cd "$1"; python3 "$2"' _ "$app" "$reader"
}
read_facts | python3 -c 'import json,sys; v=json.load(sys.stdin); assert v["last_success"]["version"] == sys.argv[1]; assert v["status"] == "current"; assert v["selected_profiles"] == []; assert v["target"]["yard"]["name"] == sys.argv[2]; assert v["capabilities"] == v["readiness"] == "unknown"' "$version" "$name"
yard provision --yes
read_facts >/dev/null
# Existing-yards refresh repairs absent facts and missing discovery links.
guest rm /etc/subyard/guest-runtime.json /home/dev/.agents/skills/subyard-environment
if read_facts >/dev/null; then printf 'missing facts unexpectedly readable\n' >&2; exit 1; fi
command_with_group "$launcher" migrate --yes
yard init --yes
read_facts >/dev/null
# Ordinary provisioning refuses drift before effects. A fresh host-wide repair
# plan authorizes the refresh, whose delivery then encounters our real collision.
guest cat /etc/subyard/guest-runtime.json > "$state/before-collision.json"
guest sh -eu -c 'rm /home/dev/.claude/skills/subyard-environment; mkdir /home/dev/.claude/skills/subyard-environment'
collision_injected=1
if yard provision --yes > "$state/collision-refusal.log" 2>&1; then
  cat "$state/collision-refusal.log"
  printf 'collision unexpectedly provisioned\n' >&2; exit 1
else refusal_exit=$?; fi
cat "$state/collision-refusal.log"
grep -Fq '"status":"migration-required"' "$state/collision-refusal.log"
guest cat /etc/subyard/guest-runtime.json > "$state/after-refusal.json"
cmp "$state/before-collision.json" "$state/after-refusal.json"
printf 'guest environment unapproved_drift_exit=%s facts_bytes=unchanged\n' "$refusal_exit"
if command_with_group "$launcher" migrate --yes > "$state/collision-refresh.log" 2>&1; then
  cat "$state/collision-refresh.log"
  printf 'collision unexpectedly refreshed\n' >&2; exit 1
else refresh_exit=$?; fi
cat "$state/collision-refresh.log"
if read_facts > "$state/incomplete.json"; then
  printf 'authorized collision repair did not enter guest refresh; expected incomplete facts\n' >&2; exit 1
fi
python3 - "$state/before-collision.json" "$state/incomplete.json" "$version" <<'PYFACTS'
import json,sys
before,after=(json.load(open(path)) for path in sys.argv[1:3])
assert after['status'] == 'incomplete', 'refresh did not publish pending facts'
assert after['last_success'] == before['last_success'], 'previous successful snapshot changed'
assert after['last_success']['version'] == sys.argv[3]
PYFACTS
printf 'guest environment authorized_refresh_exit=%s status=incomplete last_success=preserved\n' "$refresh_exit"
guest rmdir /home/dev/.claude/skills/subyard-environment
collision_injected=0
command_with_group "$launcher" migrate --yes
yard provision --yes
read_facts >/dev/null
for adapter in .agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills; do
  guest test -r "/home/dev/$adapter/subyard-environment/SKILL.md"
done
# Native loader probe with no accounts: clients are installed after skill delivery.
for client in codex opencode claude; do
  guest timeout --kill-after=5 300 bash -s < "$runtime/config/agents/$client/provision.sh"
done
metadata="$(python3 - "$runtime/config/skills/core/subyard-environment/SKILL.md" <<'PY'
from pathlib import Path
import sys
fields=dict(line.split(': ',1) for line in Path(sys.argv[1]).read_text().split('---',2)[1].splitlines() if ': ' in line)
print(fields['name']+'='+fields['description'])
PY
)"
guest runuser -u dev -- python3 - --cwd "$app" --skill "$metadata" < "$root/dev/e2e/agent-skills-discovery.py"
printf 'guest environment facts=PASS native_codex_claude_opencode=PASS pi_hermes_links=PASS optional_profiles=none\n'

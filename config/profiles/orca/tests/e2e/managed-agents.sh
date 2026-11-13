#!/usr/bin/env bash
# Sourced by the focused Orca bootstrap mode on a disposable lease VM.
# Run: bash config/profiles/orca/tests/e2e/acceptance.sh --slot N --lane managed-agents
# Variables such as pairing are owned by the sourcing bootstrap fixture.
# shellcheck disable=SC2154
set -Eeuo pipefail
# Errexit would otherwise end this lane without naming the failed command.
trap 'printf "managed-agents: command failed at line %s\n" "$LINENO" >&2' ERR

[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ "${MANAGED_AGENTS:-}" = 1 ] \
  && [ -f "${STATE:-}/.marker" ] \
  && [ "$(<"$STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ] \
  || { printf 'managed-agents-e2e: use the marked focused Orca bootstrap fixture\n' >&2; exit 2; }

helper=/tmp/subyard-managed-agents.py
codex_provider=/tmp/subyard-managed-codex-provider.py
workspace=/srv/workspaces/managed-agents/src
claude_policy=/etc/claude-code/managed-settings.d/50-subyard-git-approval.json
incus --project "$PROJECT" file push "$ROOT/config/profiles/orca/tests/e2e/managed-agents.py" \
  "$INSTANCE$helper" --mode 0644
incus --project "$PROJECT" file push "$ROOT/config/profiles/orca/tests/e2e/codex-permissions.py" \
  "$INSTANCE$codex_provider" --mode 0644
as_dev() { guest_root runuser -u dev -- env HOME=/home/dev PATH=/usr/local/bin:/usr/bin:/bin "$@"; }
login_dev() { guest_root runuser -l dev -c "$1"; }

orca_pid="$(guest_root systemctl show -p MainPID --value subyard-orca.service)"
orca_xdg="$(guest_root sh -c "tr '\\0' '\\n' < /proc/$orca_pid/environ | sed -n 's/^XDG_DATA_HOME=//p'")"
[ -n "$orca_xdg" ] || die 'Orca server exposes no XDG_DATA_HOME to its agents'

assert_managed_resolution() { # phase
  [ "$(guest_root readlink /usr/local/bin/codex)" = /opt/subyard/codex/codex ] || die "$1: Codex command left its path"
  codex_release="$(guest_root readlink /opt/subyard/codex/codex)"
  case "$codex_release" in releases/*/bin/codex) ;; *) die "$1: Codex command is not on a release" ;; esac
  codex_runtime="/opt/subyard/codex/$codex_release"
  [ "$(login_dev 'command -v claude')" = /usr/local/bin/claude ] || die "$1: login shell runs another Claude"
  [ "$(login_dev 'command -v codex')" = /usr/local/bin/codex ] || die "$1: login shell runs another Codex"
  guest_root test ! -e /home/dev/.local/bin/claude || die "$1: Claude shadow remains"
  guest_root test ! -L /home/dev/.local/bin/claude || die "$1: dangling Claude shadow remains"
  guest_root test ! -e /home/dev/.local/bin/codex || die "$1: Codex shadow remains"
  guest_root test ! -L /home/dev/.local/bin/codex || die "$1: dangling Codex shadow remains"
  guest_root claude-policy-check >/dev/null || die "$1: Claude readiness failed"
  guest_root codex-policy-check >/dev/null || die "$1: Codex readiness failed"
  # The Orca chat launcher and a login shell must settle on the managed process image.
  as_dev python3 -B "$helper" exe-of --expect "$codex_runtime" /usr/local/libexec/subyard/orca-codex/codex app-server \
    || die "$1: Orca Codex launcher reached another runtime"
  login_dev "python3 -B $helper exe-of --expect $codex_runtime bash -lc 'exec codex app-server'" \
    || die "$1: a login shell reached another Codex runtime"
}

stage 'checking fresh yard managed binaries and native update locks'
assert_managed_resolution fresh
guest_root jq -e '.env == {DISABLE_UPDATES: "1"}' "$claude_policy" >/dev/null \
  || die 'managed Claude update lock is missing'
guest_root grep -Fxq 'check_for_update_on_startup = false' /etc/codex/requirements.toml \
  || die 'managed Codex update check lock is missing'
codex_package="/opt/subyard/codex/${codex_release%/bin/codex}"
if [ -n "$(guest_root find "$codex_package" \( ! -user root -o ! -group root -o -perm /022 \) -print -quit)" ]; then
  die 'the Codex package tree is not root-owned and closed to group and other writes'
fi
for action in update install; do
  as_dev /usr/local/bin/claude "$action" >/dev/null 2>&1 </dev/null || true
  guest_root test ! -L /home/dev/.local/bin/claude || die "locked claude $action created a native link"
done
as_dev /usr/local/bin/codex update >/dev/null 2>&1 </dev/null || true
assert_managed_resolution after-update-commands

daemon_home=/home/dev/.codex/app-server-daemon
daemon_package=/home/dev/.codex/packages/app-server-daemon
daemon_control=/home/dev/.codex/app-server-control
stage 'routing plain and --no-daemon Codex clients from a login shell on a fresh yard'
as_dev python3 -B "$helper" codex-routing --provider "$codex_provider" --package absent \
  || die 'Codex client routing on a fresh yard changed'
guest_root test ! -e "$daemon_home/daemon.pid" || die 'the routing check left a daemon running'

guest_root rm -rf "$daemon_package" "$daemon_home" "$daemon_control"

stage 'launching agents under the Orca server environment without a self-install'
as_dev python3 -B "$helper" launch --cli /usr/local/bin/claude --xdg "$orca_xdg" \
  || die 'locked Claude launch under Orca XDG failed'
status=0
as_dev env XDG_DATA_HOME="$orca_xdg" timeout 15 /usr/local/bin/codex exec --skip-git-repo-check \
  -c 'model_provider="fixture"' \
  -c 'model_providers.fixture={name="Fixture",base_url="http://127.0.0.1:9/v1",wire_api="responses",requires_openai_auth=false,request_max_retries=0,stream_max_retries=0}' \
  fixture >/dev/null 2>&1 </dev/null || status=$?
[ "$status" != 0 ] || die 'Codex fixture unexpectedly reached a model'
guest_root test ! -e /home/dev/.codex/packages/standalone || die 'Codex self-installed a standalone package'
assert_managed_resolution after-launch

# The unlocked positive control proves the launch trigger, and leaves a real legacy link.
stage 'creating a real legacy Claude self-install under the Orca server environment'
guest_root sh -c "jq 'del(.env)' '$claude_policy' > /tmp/subyard-legacy-policy.json && cat /tmp/subyard-legacy-policy.json > '$claude_policy' && rm -f /tmp/subyard-legacy-policy.json"
as_dev python3 -B "$helper" launch --cli /usr/local/bin/claude --xdg "$orca_xdg" --expect-link --seconds 150 \
  || die 'unlocked Claude did not reproduce its native self-install'
claude_target="$(guest_root readlink /home/dev/.local/bin/claude)"
case "$claude_target" in "$orca_xdg"/claude/versions/*) ;; *) die 'native Claude link has an unexpected target' ;; esac
[ "$(login_dev 'command -v claude')" = /home/dev/.local/bin/claude ] \
  || die 'native Claude link does not shadow the managed runtime'
if guest_root claude-policy-check >/dev/null 2>&1; then die 'legacy Claude state passed readiness'; fi
claude_version_before="$(guest_root sha256sum "$claude_target")"

seed_legacy_codex() {
  guest_root runuser -u dev -- bash -euc '
    home=/home/dev
    standalone="$home/.codex/packages/standalone"
    install -d "$home/.local/bin" "$standalone/releases/0.1.0-fixture/bin"
    cp /usr/local/bin/codex "$standalone/releases/0.1.0-fixture/bin/codex"
    ln -sfn "$standalone/releases/0.1.0-fixture" "$standalone/current"
    printf "keep\n" > "$home/.codex/packages/unknown-fixture"
    ln -sfn "$standalone/current/bin/codex" "$home/.local/bin/codex"
  '
  guest_root sed -i '/^check_for_update_on_startup = false$/d' /etc/codex/requirements.toml
}
assert_legacy_preserved() { # phase
  [ "$claude_version_before" = "$(guest_root sha256sum "$claude_target")" ] \
    || die "$1: Claude version file changed"
  guest_root test -f /home/dev/.codex/packages/standalone/releases/0.1.0-fixture/bin/codex \
    || die "$1: standalone release was removed"
  guest_root test -f /home/dev/.codex/packages/unknown-fixture || die "$1: unknown Codex file was removed"
  guest_root jq -e '.env == {DISABLE_UPDATES: "1"}' "$claude_policy" >/dev/null \
    || die "$1: Claude update lock did not converge"
  guest_root grep -Fxq 'check_for_update_on_startup = false' /etc/codex/requirements.toml \
    || die "$1: Codex update check lock did not converge"
  [ "$(guest_root systemctl show -p MainPID --value subyard-orca.service)" = "$orca_pid" ] \
    || die "$1: convergence restarted Orca"
}
seed_legacy_codex
[ "$(login_dev 'command -v codex')" = /home/dev/.local/bin/codex ] \
  || die 'standalone Codex link does not shadow the managed runtime'
if guest_root codex-policy-check >/dev/null 2>"$STATE/codex-legacy-check.err"; then
  die 'legacy Codex state passed readiness'
fi
grep -Fq 'run yard init' "$STATE/codex-legacy-check.err" || die 'legacy Codex readiness lacks its repair'
report_status() { # phase
  local output="$STATE/managed-status-$1.out" status=0
  yard status >"$output" 2>&1 || status=$?
  printf 'managed-agents-status: %s: yard status exit %s\n' "$1" "$status"
  sed 's/^/managed-agents-status: /' "$output"
}
report_status legacy-agents

hold_codex() { # command expected-executable
  local pid_file=/tmp/subyard-held-codex.pid deadline=$((SECONDS + 30))
  guest_root rm -f "$pid_file"
  guest_root sh -c "setsid runuser -u dev -- env HOME=/home/dev PATH=/usr/local/bin:/usr/bin:/bin python3 -B $helper hold --pid-file $pid_file $1 app-server >/dev/null 2>&1 </dev/null &"
  until guest_root test -s "$pid_file"; do
    ((SECONDS < deadline)) || die 'held Codex session did not start'
    sleep 1
  done
  held_codex_pid="$(guest_root cat "$pid_file")"
  deadline=$((SECONDS + 15))
  until [ "$(guest_root readlink "/proc/$held_codex_pid/exe")" = "$2" ]; do
    ((SECONDS < deadline)) || die "held Codex session is not running $2"
    sleep 1
  done
}

stage 'holding sessions on the legacy binaries across convergence'
guest_root sh -c "setsid runuser -u dev -- env HOME=/home/dev PATH=/usr/local/bin:/usr/bin:/bin python3 -B $helper launch --cli /home/dev/.local/bin/claude --xdg $orca_xdg --seconds 8 --pid-file /tmp/subyard-held-claude.pid >/dev/null 2>&1 </dev/null &"
deadline=$((SECONDS + 30))
until guest_root test -s /tmp/subyard-held-claude.pid; do
  ((SECONDS < deadline)) || die 'held legacy Claude session did not start'
  sleep 1
done
held_pid="$(guest_root cat /tmp/subyard-held-claude.pid)"
held_codex_exe=/home/dev/.codex/packages/standalone/releases/0.1.0-fixture/bin/codex
hold_codex /home/dev/.local/bin/codex "$held_codex_exe"

stage 'converging legacy agents through release activation'
focused_release_ready agent-shadow-drift || die 'release activation did not repair legacy agents'
assert_managed_resolution release-activation
assert_legacy_preserved release-activation
[ "$(guest_root readlink "/proc/$held_pid/exe")" = "$claude_target" ] \
  || die 'running legacy Claude session did not survive convergence'
[ "$(guest_root readlink "/proc/$held_codex_pid/exe")" = "$held_codex_exe" ] \
  || die 'running legacy Codex session did not survive convergence'
printf 'ok: held legacy sessions survived convergence: claude %s, codex %s\n' "$claude_target" "$held_codex_exe"
guest_root kill "$held_pid" "$held_codex_pid" 2>/dev/null || true

stage 'converging legacy agents through yard init while provisioning is current'
guest_root runuser -u dev -- ln -s "$claude_target" /home/dev/.local/bin/claude
seed_legacy_codex
yard init --yes >"$STATE/managed-agents-init.out" 2>"$STATE/managed-agents-init.err" \
  || die 'yard init did not repair legacy agents'
assert_managed_resolution yard-init
assert_legacy_preserved yard-init
if grep -Eiq 'warn.*(claude|codex)|(claude|codex).*warn' \
  "$STATE/managed-agents-init.out" "$STATE/managed-agents-init.err"; then
  die 'expected agent drift was reported as a warning'
fi

stage 'keeping links outside the proven installer shape through yard init'
guest_root runuser -u dev -- sh -c 'printf "#!/bin/sh\n" > /home/dev/.local/bin/claude; chmod 0755 /home/dev/.local/bin/claude; ln -s /usr/bin/true /home/dev/.local/bin/codex'
yard init --yes >/dev/null 2>&1 || die 'yard init refused foreign agent links'
guest_root test -f /home/dev/.local/bin/claude || die 'foreign Claude link was removed'
[ "$(guest_root readlink /home/dev/.local/bin/codex)" = /usr/bin/true ] || die 'foreign Codex link was removed'
guest_root rm -f /home/dev/.local/bin/claude /home/dev/.local/bin/codex
assert_managed_resolution foreign-cleanup

stage 'keeping a real upstream Codex install and daemon through yard init'
as_dev sh -c 'cd && curl -fsSL https://chatgpt.com/codex/install.sh -o /tmp/subyard-codex-install.sh \
  && CODEX_NON_INTERACTIVE=1 sh /tmp/subyard-codex-install.sh >/tmp/subyard-codex-install.out 2>&1 </dev/null' \
  || { guest_root tail -n 20 /tmp/subyard-codex-install.out >&2 || true; die 'upstream Codex installer failed'; }
[ "$(guest_root readlink /home/dev/.local/bin/codex)" = /home/dev/.codex/packages/standalone/current/bin/codex ] \
  || die 'upstream Codex installer published another link shape'
# The updater stays off here only so that a selection cannot move the daemon during these assertions.
as_dev sh -c "install -d -m 0700 $daemon_home; printf '%s\n' '{\"updater\":{\"autoUpdateEnabled\":false}}' > $daemon_home/settings.json"
as_dev sh -c 'cd && timeout 150 /home/dev/.local/bin/codex app-server daemon bootstrap >/tmp/subyard-codex-bootstrap.out 2>&1 </dev/null' \
  || die 'upstream Codex daemon bootstrap failed'
daemon_pid="$(as_dev jq -er .pid "$daemon_home/daemon.pid")"
daemon_exe="$(guest_root readlink "/proc/$daemon_pid/exe")"
case "$daemon_exe" in "$daemon_package"/releases/*) ;; *) die 'Codex daemon runs another binary' ;; esac
yard init --yes >/dev/null 2>&1 || die 'yard init did not converge the real upstream Codex'
assert_managed_resolution real-codex-daemon
[ "$(guest_root readlink "/proc/$daemon_pid/exe")" = "$daemon_exe" ] || die 'convergence stopped the running Codex daemon'
stage 'routing Codex clients through a kept daemon package'
as_dev python3 -B "$helper" codex-routing --provider "$codex_provider" --package present \
  || die 'Codex client routing through a kept daemon package changed'
guest_root test ! -e "$daemon_home/daemon.pid" || die 'the routing check left a daemon running'
guest_root rm -f /tmp/subyard-codex-bootstrap.out /tmp/subyard-codex-install.sh /tmp/subyard-codex-install.out

stage 'preparing a registered Git workspace for Claude prompts'
mkdir -p "$STATE/managed-agents"
printf 'Managed agents fixture\n' > "$STATE/managed-agents/fixture.txt"
yard sync "$STATE/managed-agents" --name managed-agents --yes >/dev/null
guest_root runuser -u dev -- bash -euc '
  cd "$1"
  git init -q
  git config user.name Fixture
  git config user.email fixture@example.invalid
  git commit -q --allow-empty -m seed
  git remote add fixture /nonexistent/subyard-fixture.git
' subyard "$workspace"
yard orca sync --yes >/dev/null
workspace_head() { guest_root runuser -u dev -- git -C "$workspace" rev-parse --verify HEAD; }

workspace_commits() { guest_root runuser -u dev -- git -C "$workspace" rev-list --count HEAD; }
install -d -m 0700 "$STATE/managed-agents-client"
agent_client() {
  HOME="$STATE/managed-agents-client/home" \
    XDG_CONFIG_HOME="$STATE/managed-agents-client/config" \
    XDG_DATA_HOME="$STATE/managed-agents-client/data" \
    XDG_STATE_HOME="$STATE/managed-agents-client/state" \
    LIBGL_ALWAYS_SOFTWARE=1 ORCA_PAIRING_CODE="$pairing" \
    timeout 30 /usr/bin/orca-ide "$@"
}
terminal_handle=''
serve_pid_file=/tmp/subyard-managed-model.pid
terminal_close() {
  [ -z "$terminal_handle" ] || agent_client terminal close --terminal "$terminal_handle" --tab --json >/dev/null 2>&1 || true
  terminal_handle=''
}
terminal_cleanup() {
  terminal_close
  guest_root sh -c "[ ! -s $serve_pid_file ] || kill \"\$(cat $serve_pid_file)\"" >/dev/null 2>&1 || true
}
terminal_screen() {
  agent_client terminal read --terminal "$terminal_handle" --screen --json >"$STATE/managed-terminal.json" \
    2>"$STATE/managed-terminal.err" || return 1
  jq -r '(.result.terminal.tail // [])[]' "$STATE/managed-terminal.json"
}
terminal_fail() {
  # This terminal contains only synthetic fixture output.
  [ -z "$terminal_handle" ] || terminal_screen | tail -n 30 >&2 || true
  terminal_cleanup
  die "$1"
}
terminal_shows() { # text
  local screen
  screen="$(terminal_screen)" || return 1
  grep -Fq -- "$1" <<<"$screen"
}
terminal_wait() { # text seconds
  local deadline=$((SECONDS + $2))
  while ((SECONDS < deadline)); do
    if terminal_shows "$1"; then return 0; fi
    sleep 1
  done
  return 1
}
terminal_send() {
  local output
  output="$(agent_client terminal send --terminal "$terminal_handle" "$@" --json 2>&1)" \
    || terminal_fail "Orca terminal send failed: $output"
}
terminal_press() { # text
  terminal_send --text "$1"
  sleep 1
  terminal_send --text $'\r'
}
orca_terminal_open() { # command title
  agent_client terminal create --worktree "id:$repo_id::$workspace" --title "$2" \
    --command "$1" --json >"$STATE/managed-terminal-create.json" \
    2>"$STATE/managed-terminal-create.err" || die "paired Orca could not create terminal: $2"
  terminal_handle="$(jq -er 'select(.ok == true) | .result.terminal.handle |
    select(type == "string" and length > 0)' "$STATE/managed-terminal-create.json")" \
    || die "paired Orca returned no terminal handle: $2"
}
terminal_run() { # command choice expected-change commits-added
  local before after commits_before
  before="$(workspace_head)"
  commits_before="$(workspace_commits)"
  as_dev sh -c 'printf "%s\n" "$1" > /tmp/subyard-managed-command' subyard "$1"
  terminal_press 'Run the fixture command.'
  terminal_wait 'Do you want to proceed' 45 || terminal_fail "Orca Claude terminal did not prompt for: $1"
  terminal_shows "$1" || terminal_fail "Orca Claude terminal prompt names another command than: $1"
  [ "$(workspace_head)" = "$before" ] || terminal_fail 'Orca Claude terminal ran Git before approval'
  as_dev python3 -B "$helper" terminal-exe "$workspace" || terminal_fail 'Orca terminal ran another Claude executable'
  case "$2" in
    allow) terminal_press 1 ;;
    deny) terminal_send --text $'\x1b' ;;
  esac
  sleep 2
  local deadline=$((SECONDS + 30))
  while ((SECONDS < deadline)) && terminal_shows 'Do you want to proceed'; do sleep 1; done
  if terminal_shows 'Do you want to proceed'; then
    terminal_fail "Orca Claude terminal prompt did not close for: $1"
  fi
  # A completed tool run settles after the prompt closes.
  sleep 2
  after="$(workspace_head)"
  if [ "$3" = changed ]; then
    [ "$after" != "$before" ] || terminal_fail "approved command did not run: $1"
  else
    [ "$after" = "$before" ] || terminal_fail "declined command changed history: $1"
  fi
  [ "$(workspace_commits)" = "$((commits_before + $4))" ] \
    || terminal_fail "unexpected commit count after: $1"
  printf 'ok: Orca Claude terminal prompt: %s (%s)\n' "$1" "$2"
}

settings_file=/home/dev/.claude/settings.json
managed_settings() { guest_root jq -cS '{autoMemoryDirectory, permissions}' "$settings_file"; }
log_claude_settings() { # phase
  printf 'managed-agents: Claude settings after %s: %s\n' "$1" "$(guest_root jq -c '{
    keys: keys, orca_hook_commands: ([(.hooks // {}) | .. | .command? // empty | strings
      | select(test("orca"; "i"))] | length)}' "$settings_file")"
}
assert_managed_settings() { # phase
  log_claude_settings "$1"
  [ "$(managed_settings)" = "$managed_before" ] || {
    printf 'managed-agents: MATERIAL FINDING: %s changed managed Claude settings: %s\n' "$1" "$(managed_settings)" >&2
    die "$1 changed autoMemoryDirectory or permissions"
  }
}
agent_client_ready() {
  local deadline=$((SECONDS + 90))
  until agent_client repo list --json >"$STATE/managed-repos.json" 2>"$STATE/managed-repos.err" \
    && jq -e --arg path "$workspace" '.result.repos | any(.path == $path)' "$STATE/managed-repos.json" >/dev/null; do
    ((SECONDS < deadline)) || die 'paired Orca did not return after its service restart'
    sleep 2
  done
}

# These reads precede every fixture snapshot, so a real Orca rewrite cannot be restored away.
stage 'keeping managed Claude settings across a real Orca hook install and launch'
managed_before="$(managed_settings)"
jq -e '(.autoMemoryDirectory | type == "string") and (.permissions.defaultMode | type == "string")' <<<"$managed_before" >/dev/null \
  || die 'fresh yard settings do not hold the managed keys'
log_claude_settings fresh-yard
guest_root runuser -u dev -- sh -c "jq 'del(.hooks)' $settings_file > $settings_file.stage && cat $settings_file.stage > $settings_file && rm -f $settings_file.stage"
guest_root systemctl restart subyard-orca.service
agent_client_ready
orca_pid="$(guest_root systemctl show -p MainPID --value subyard-orca.service)"
assert_managed_settings orca-service-restart
[ "$(guest_root jq '[(.hooks // {}) | .. | .command? // empty | strings | select(test("orca"; "i"))] | length' "$settings_file")" -gt 0 ] \
  || die 'the Orca service restart did not reinstall its hooks, so this proves nothing'
repo_id="$(jq -er --arg path "$workspace" \
  '.result.repos | map(select(.path == $path)) | select(length == 1) | .[0].id' \
  "$STATE/managed-repos.json")" || die 'paired Orca did not register the managed agents workspace'
orca_terminal_open 'claude --dangerously-skip-permissions' subyard-managed-claude-launch
as_dev python3 -B "$helper" terminal-exe --agent claude --seconds 30 "$workspace" \
  || terminal_fail 'Orca terminal ran another Claude executable'
sleep 5
assert_managed_settings orca-claude-terminal-launch
terminal_close
assert_managed_settings orca-claude-terminal-close

stage 'running Claude agent view through a stock Orca terminal'
agent_view_plugin="$(guest_root find /srv/agents/orca -type d -name orca-agent-view -print -quit)"
[ -n "$agent_view_plugin" ] || die 'stock Orca agent-view plugin was not installed'
orca_terminal_open "claude --plugin-dir '$agent_view_plugin'" subyard-managed-claude-agent-view
as_dev python3 -B "$helper" terminal-exe --agent claude --seconds 30 --plugin-dir "$agent_view_plugin" "$workspace" \
  || terminal_fail 'Orca agent view ran another Claude executable or omitted its plugin'
sleep 5
assert_managed_settings orca-claude-agent-view
guest_root test ! -L /home/dev/.local/bin/claude || die 'agent view recreated the native Claude shadow'
terminal_close

stage 'reading managed Claude settings drift before repairing it'
guest_root runuser -u dev -- sh -c "jq 'del(.autoMemoryDirectory)' $settings_file > $settings_file.stage && cat $settings_file.stage > $settings_file && rm -f $settings_file.stage"
if yard config status >"$STATE/managed-drift-config-status.out" 2>&1; then
  die 'yard config status did not report managed Claude settings drift'
fi
sed 's/^/managed-agents-status: settings drift: yard config status: /' "$STATE/managed-drift-config-status.out"
report_status settings-drift
yard config apply --yes >/dev/null || die 'yard config apply did not repair managed Claude settings'
yard config status >/dev/null || die 'managed Claude settings stayed in drift after repair'
assert_managed_settings settings-repair

stage 'launching Codex from Orca terminal and native chat'
orca_terminal_open 'codex --dangerously-bypass-approvals-and-sandbox' subyard-managed-codex-launch
as_dev python3 -B "$helper" terminal-exe --agent codex --seconds 30 "$workspace" \
  || terminal_fail 'Orca terminal ran another Codex executable'
terminal_close
as_dev python3 -B "$helper" codex-chat --provider "$codex_provider" "$workspace" \
  || die 'stock Orca native Codex chat did not run the managed runtime'

guest_root test ! -e "$daemon_home/daemon.pid" \
  || die 'an Orca Codex launch started a daemon'
printf 'ok: Orca Codex terminal and chat ran the managed runtime and did not start a daemon\n'

stage 'running Git prompts in stock Orca native Claude chat'
as_dev python3 -B "$helper" chat "$workspace" \
  || die 'stock Orca native Claude chat approvals failed'

stage 'running Git prompts in a stock-argument Claude launched by a paired Orca terminal'
guest_root sh -c "setsid runuser -u dev -- env HOME=/home/dev PATH=/usr/local/bin:/usr/bin:/bin sh -c 'echo \$\$ > $serve_pid_file; exec python3 -B $helper serve --port-file /tmp/subyard-managed-model.port --command-file /tmp/subyard-managed-command' >/dev/null 2>&1 </dev/null &"
deadline=$((SECONDS + 20))
until guest_root test -s /tmp/subyard-managed-model.port; do
  ((SECONDS < deadline)) || die 'synthetic terminal model did not start'
  sleep 1
done
model_port="$(guest_root cat /tmp/subyard-managed-model.port)"
guest_root runuser -u dev -- python3 -c '
import json, pathlib, sys
workspace, port = sys.argv[1:3]
state = pathlib.Path.home() / ".claude.json"
data = json.loads(state.read_text()) if state.exists() else {}
data.update(hasCompletedOnboarding=True, lastOnboardingVersion="2.1.316")
data.setdefault("projects", {})[workspace] = {"hasTrustDialogAccepted": True, "hasCompletedProjectOnboarding": True}
state.write_text(json.dumps(data))
settings = pathlib.Path.home() / ".claude/settings.json"
value = json.loads(settings.read_text())
pathlib.Path("/tmp/subyard-managed-settings.saved").write_text(settings.read_text())
value["env"] = {**value.get("env", {}), "ANTHROPIC_AUTH_TOKEN": "synthetic-local-only",
                "ANTHROPIC_BASE_URL": "http://127.0.0.1:" + port}
settings.write_text(json.dumps(value))
' "$workspace" "$model_port"
claude_terminal_ready() { # title, accept|decline (default: decline)
  local choice="${2:-decline}" screen offered=0 accepted=0 ready_since=0 deadline=$((SECONDS + 65))
  orca_terminal_open 'claude --dangerously-skip-permissions' "$1"
  while ((SECONDS < deadline)); do
    screen="$(terminal_screen)" || terminal_fail 'cannot read Claude startup screen'
    # A status line may appear behind the dialog. Resolve the offer before sending any prompt.
    if [[ "$screen" = *'Make auto mode'* ]]; then
      if [ "$offered" = 0 ]; then
        case "$choice" in
          accept)
            printf 'managed-agents-status: auto mode offer: accepting through the real dialog\n'
            # Yes is preselected; Enter is intentional only in this acceptance case.
            terminal_send --text $'\r' ;;
          decline)
            printf 'managed-agents-status: auto mode offer: explicitly declining\n'
            terminal_press $'\x1b[B' ;;
          *) terminal_fail "invalid auto mode offer choice: $choice" ;;
        esac
        offered=1
      fi
    elif [[ "$screen" = *'Yes, I accept'* ]]; then
      if [ "$accepted" = 0 ]; then
        # The bypass disclaimer defaults to exit; select its accept option explicitly.
        terminal_press $'\x1b[B'
        accepted=1
      fi
    elif [ "$offered" = 1 ]; then
      if { [ "$choice" = accept ] && [[ "$screen" = *'auto mode on'* ]]; } \
        || { [ "$choice" = decline ] && [[ "$screen" = *'bypass permissions on'* ]]; }; then
        return 0
      fi
    elif [[ "$screen" = *'bypass permissions on'* ]]; then
      [ "$ready_since" != 0 ] || ready_since=$SECONDS
      # Older clients may not offer auto mode; allow the existing 15-second offer window.
      if [ "$choice" = decline ] && ((SECONDS - ready_since >= 15)); then return 0; fi
    else
      ready_since=0
    fi
    sleep 1
  done
  terminal_fail "Claude startup did not finish the $choice auto mode choice"
}
# bypassPermissions is the standard mode, so an accepted auto mode offer must read as drift.
yard config status >/dev/null 2>&1 || die 'the Claude terminal fixture settings already read as drift'
guest_root runuser -u dev -- sh -c 'cp /home/dev/.claude.json /tmp/subyard-managed-claude-state.saved && cp /home/dev/.claude/settings.json /tmp/subyard-managed-fixture-settings.saved'
claude_terminal_ready subyard-managed-claude-offer accept
terminal_close
[ "$(guest_root jq -r .permissions.defaultMode "$settings_file")" = auto ] || die 'the accepted offer did not set auto mode'
if yard config status >"$STATE/managed-offer-status.out" 2>&1; then
  die 'yard config status did not report an accepted auto mode offer'
fi
sed 's/^/managed-agents-status: accepted auto mode offer: yard config status: /' "$STATE/managed-offer-status.out"
yard config apply --yes >/dev/null || die 'yard config apply did not restore bypassPermissions'
yard config status >/dev/null || die 'an accepted auto mode offer stayed in drift after repair'
[ "$(guest_root jq -r .permissions.defaultMode "$settings_file")" = bypassPermissions ] \
  || die 'yard config apply restored another Claude mode'
guest_root runuser -u dev -- sh -c 'cat /tmp/subyard-managed-claude-state.saved > /home/dev/.claude.json; cat /tmp/subyard-managed-fixture-settings.saved > /home/dev/.claude/settings.json; rm -f /tmp/subyard-managed-claude-state.saved /tmp/subyard-managed-fixture-settings.saved'
claude_terminal_ready subyard-managed-claude decline
[ "$(guest_root jq -r .permissions.defaultMode "$settings_file")" = bypassPermissions ] \
  || die 'declining the auto mode offer changed the managed mode'
terminal_run 'git commit --allow-empty -m terminal-denied' deny same 0
terminal_run 'git commit --allow-empty -m terminal-first' allow changed 1
terminal_run 'git commit --allow-empty -m terminal-repeated' allow changed 1
terminal_run 'git commit --amend --allow-empty -m terminal-amended' allow changed 0
terminal_run 'git push' deny same 0
terminal_run 'git push' deny same 0
terminal_cleanup
assert_managed_settings orca-claude-terminal-prompts
yard config status >/dev/null || die 'a declined auto mode offer read as drift'
printf 'ok: a declined auto mode offer leaves yard config status clean\n'
guest_root runuser -u dev -- sh -c 'cat /tmp/subyard-managed-settings.saved > /home/dev/.claude/settings.json; rm -f /tmp/subyard-managed-settings.saved'
assert_managed_resolution after-orca-agents
guest_root test ! -e /home/dev/.local/bin/claude || die 'Orca-launched Claude self-installed'
guest_root rm -f -- "$helper" "$codex_provider"

printf 'ok: managed Claude and Codex binaries, Codex routing, legacy convergence, and Claude Git prompts in stock Orca terminal and chat\n'

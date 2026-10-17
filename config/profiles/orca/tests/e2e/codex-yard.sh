#!/usr/bin/env bash
# Sourced by the focused Orca bootstrap mode on a disposable lease VM.
# Run: SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 \
#   dev/agent-e2e.sh --slot N --purpose codex-permissions -- bash config/profiles/orca/tests/e2e/orca-bootstrap.sh
# Variables such as pairing are owned by the sourcing bootstrap fixture.
# shellcheck disable=SC2154
set -euo pipefail

[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ "${CODEX_PERMISSIONS:-}" = 1 ] \
  && [ -f "${STATE:-}/.marker" ] \
  && [ "$(<"$STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ] \
  || { printf 'codex-yard-e2e: use the marked focused Orca bootstrap fixture\n' >&2; exit 2; }

helper="$ROOT/config/profiles/orca/tests/e2e/codex-permissions.py"
guest_helper=/tmp/subyard-codex-permissions.py
workspace_one=/srv/workspaces/codex-policy-one/src
workspace_two=/srv/workspaces/codex-policy-two/src

[ -f "$helper" ] || die 'Codex permissions helper is unavailable'

stage 'creating two registered workspaces for Codex policy coverage'
for name in codex-policy-one codex-policy-two; do
  mkdir -p "$STATE/$name"
  printf 'Codex permissions fixture\n' > "$STATE/$name/fixture.txt"
  yard sync "$STATE/$name" --name "$name" --yes >/dev/null
done
yard orca sync --yes >/dev/null

incus --project "$PROJECT" file push "$helper" "$INSTANCE$guest_helper" --mode 0644
stage 'repairing a legacy Codex chat launch environment through yard init'
codex_orca_pid="$(guest_root systemctl show -p MainPID --value subyard-orca.service)"
guest_root python3 - <<'PY'
from pathlib import Path

Path('/usr/local/libexec/subyard/orca-codex/codex').unlink()
profile = Path('/etc/profile.d/subyard-orca-codex.sh')
profile.write_text('''if [ "${SUBYARD_ORCA_CODEX_CONFIG:-}" = 1 ]; then
  codex() { /usr/bin/python3 -B /usr/local/libexec/subyard/orca-registration/codex_launch.py "$@"; }
fi
''')
PY
yard init --yes >"$STATE/codex-launch-repair.out" 2>"$STATE/codex-launch-repair.err" \
  || die 'yard init did not repair the legacy Codex launch environment'
guest_root test -x /usr/local/libexec/subyard/orca-codex/codex \
  || die 'yard init omitted the Codex chat launcher'
guest_root grep -Fq 'export PATH=/usr/local/libexec/subyard/orca-codex:' /etc/profile.d/subyard-orca-codex.sh \
  || die 'yard init retained the legacy Codex login integration'
[ "$(guest_root systemctl show -p MainPID --value subyard-orca.service)" = "$codex_orca_pid" ] \
  || die 'Codex launcher repair restarted Orca'

stage 'running stock Orca native Codex chat start and resume'
chat_helper="$ROOT/config/profiles/orca/tests/e2e/codex-chat.py"
incus --project "$PROJECT" file push "$chat_helper" "$INSTANCE/tmp/subyard-codex-chat.py" --mode 0644
guest_root runuser -u dev -- env HOME=/home/dev \
  python3 -B /tmp/subyard-codex-chat.py "$guest_helper" "$workspace_two" \
  || die 'stock Orca native Codex chat permissions failed'

stage 'running Codex permission and Orca terminal coverage as the developer'
for surface in appserver terminal; do
  helper_args=()
  [ "$surface" != terminal ] || helper_args+=(--terminal)
  guest_root runuser -u dev -- env \
    HOME=/home/dev CODEX_HOME=/home/dev/.codex \
    PATH=/home/dev/.local/bin:/home/dev/.npm-global/bin:/usr/local/bin:/usr/bin:/bin \
    python3 -B "$guest_helper" "${helper_args[@]}" "$workspace_one" "$workspace_two" \
    || die 'Codex permissions helper failed'
done

codex_terminal=''
codex_client() {
  HOME="$STATE/codex-permissions-client/home" \
    XDG_CONFIG_HOME="$STATE/codex-permissions-client/config" \
    XDG_DATA_HOME="$STATE/codex-permissions-client/data" \
    XDG_STATE_HOME="$STATE/codex-permissions-client/state" \
    LIBGL_ALWAYS_SOFTWARE=1 ORCA_PAIRING_CODE="$pairing" \
    timeout 30 /usr/bin/orca-ide "$@"
}
codex_terminal_close() {
  [ -z "$codex_terminal" ] || codex_client terminal close --terminal "$codex_terminal" --tab --json \
    >/dev/null 2>&1 || true
  codex_terminal=''
}
codex_terminal_fail() {
  # This terminal contains only the isolated helper's synthetic fixture output.
  jq -r '.result.terminal.tail[-20:][]?' "$STATE/codex-permissions-terminal-read.json" >&2 2>/dev/null || true
  guest_root tail -n 25 "$workspace_one/codex-orca.log" >&2 2>/dev/null || true
  codex_terminal_close
  die 'paired Orca Codex permissions terminal failed'
}

stage 'running Codex permissions through an actual paired Orca terminal'
codex_client repo list --json >"$STATE/codex-permissions-repos.json" \
  2>"$STATE/codex-permissions-repos.err" || die 'paired Orca could not list Codex workspaces'
codex_repo_id="$(jq -er --arg path "$workspace_one" \
  '.result.repos | map(select(.path == $path)) | select(length == 1) | .[0].id' \
  "$STATE/codex-permissions-repos.json")" || die 'paired Orca did not register the first Codex workspace'
codex_worktree="$(jq -er --arg id "$codex_repo_id" \
  '.result.repos[] | select(.id == $id) | .path' "$STATE/codex-permissions-repos.json")" \
  || die 'paired Orca returned an invalid Codex worktree'
[ "$codex_worktree" = "$workspace_one" ] || die 'paired Orca selected an unexpected Codex worktree'
# Keep the terminal alive after the helper exits so the paired CLI can read its output.
codex_client terminal create --worktree "id:$codex_repo_id::$codex_worktree" \
  --title subyard-codex-permissions-e2e \
  --command "exec bash -c 'python3 -B $guest_helper --orca $workspace_one $workspace_two 2>&1 | tee $workspace_one/codex-orca.log; read -r -t 360'" --json \
  >"$STATE/codex-permissions-terminal-create.json" \
  2>"$STATE/codex-permissions-terminal-create.err" || die 'paired Orca could not create the Codex permissions terminal'
codex_terminal="$(jq -er 'select(.ok == true) | .result.terminal.handle |
  select(type == "string" and length > 0)' "$STATE/codex-permissions-terminal-create.json")" \
  || die 'paired Orca returned no Codex permissions terminal handle'

deadline=$((SECONDS + 300))
completed=0
while ((SECONDS < deadline)); do
  codex_client terminal read --terminal "$codex_terminal" --json \
    >"$STATE/codex-permissions-terminal-read.json" \
    2>"$STATE/codex-permissions-terminal-read.err" || codex_terminal_fail
  jq -e '.ok == true' "$STATE/codex-permissions-terminal-read.json" >/dev/null \
    || codex_terminal_fail
  if jq -e '(.result.terminal.tail | any(contains("Error:")))' \
    "$STATE/codex-permissions-terminal-read.json" >/dev/null; then
    codex_terminal_fail
  fi
  if jq -e '(.result.terminal.tail | any(contains("ok: codex-permissions complete")))' \
    "$STATE/codex-permissions-terminal-read.json" >/dev/null; then
    completed=1
    break
  fi
  sleep 1
done
[ "$completed" = 1 ] || codex_terminal_fail
codex_terminal_close
guest_root rm -f -- "$guest_helper"

stage 'checking native requirements drift denial and authorized release repair'
home_config_before="$(guest_root sha256sum /home/dev/.codex/config.toml | cut -d' ' -f1)" \
  || die 'Codex user configuration is unavailable before requirements repair'
guest_root /usr/local/bin/codex-policy-check >/dev/null \
  || die 'native Codex requirements did not pass their managed check'
guest_root sh -c 'printf "\n# e2e requirements drift\n" >> /etc/codex/requirements.toml'
if guest_root /usr/local/bin/codex-policy-check >/dev/null 2>&1; then
  die 'native Codex requirements drift passed its managed check'
fi
focused_release_ready requirements-drift || die 'native requirements release repair failed'
guest_root /usr/local/bin/codex-policy-check >/dev/null \
  || die 'authorized release repair did not restore native Codex requirements'
yard init --yes >/dev/null
guest_root /usr/local/bin/codex-policy-check >/dev/null \
  || die 'native Codex requirements did not remain ready after public yard init'
[ "$(guest_root sha256sum /home/dev/.codex/config.toml | cut -d' ' -f1)" = "$home_config_before" ] \
  || die 'native requirements repair changed the Codex user configuration'

printf 'ok: native Codex drift denied and repaired through release migration, init verified policy, user config preserved, and two workspaces passed direct and Orca coverage\n'

# shellcheck shell=bash
# shellcheck disable=SC2154 # release_version and the fixture state come from orca-bootstrap.sh
# shellcheck source=config/profiles/orca/tests/e2e/orca-lane-output.bash
. "$ROOT/config/profiles/orca/tests/e2e/orca-lane-output.bash"
# Stuck 0.29.0 recovery, sourced by orca-bootstrap.sh after the published predecessor runs Orca.
# A removed project keeps a saved tab that survived an Orca restart, an agent-session tab and a
# live terminal. Updating to published 0.29.0 must stop in recovery; the candidate, a newer
# version, must then complete through `yard update --version` with no manual step while the
# predecessor's Orca package is still installed.

stuck_tabs() {
  # Print the tab IDs of one exact worktree selector, one per line.
  orca_rpc session.tabs.listAll | jq -r --arg worktree "$1" \
    '.snapshots[] | select(.worktree == $worktree) | .tabs[] | .id'
}

stuck_agent_tab() {
  local worktree="$1" name="$2" before after
  before="$(stuck_tabs "$worktree")"
  guest_dev python3 -B - "$worktree" <<'YARD' >/dev/null || die "agent session could not be created: $name"
import secrets, sys, time
sys.path.insert(0, "/usr/local/libexec/subyard/orca-registration")
from transport import RuntimeRPC
rpc = RuntimeRPC("/srv/agents/orca/config/orca/orca-runtime.json", timeout=30)
rpc.call("terminal.createAgentSession", {
    "clientOperationId": f"{int(time.time() * 1000):013d}-{secrets.token_hex(16)}",
    "worktree": "id:" + sys.argv[1], "agent": "codex", "agentArgs": ""})
YARD
  for _ in $(seq 1 30); do
    after="$(stuck_tabs "$worktree")"
    comm -13 <(sort <<<"$before") <(sort <<<"$after") | head -n1 | grep . && return 0
    sleep 1
  done
  die "agent session tab did not appear: $name"
}

stuck_live_terminal() {
  # Print "TAB PTY" of a new live terminal in one exact worktree.
  guest_dev python3 -B /tmp/orca-projects-helper.py cleanup-terminal create "id:$1" \
    | jq -er '.tab_id + " " + .pty_id'
}

stuck_pty_live() {
  orca_rpc terminal.list '{"limit":1000,"requireFreshPtyLiveness":true,"includeVisualLayouts":false}' \
    | jq -e --arg pty "$1" '.terminals | any(.ptyId == $pty and .connected == true)' >/dev/null
}

stuck_orca_version() {
  guest_root dpkg-query -W -f '${Version}' orca-ide
}

run_stuck_recovery() {
  local removed_source neighbour_source removed_root neighbour_root removed_id neighbour_id
  local removed_wt neighbour_wt saved_tab agent_tab live_tab live_pty neighbour_agent
  local neighbour_live neighbour_pty orca_before hint started elapsed helper_target status_warnings
  stage 'seeding a removed project with saved, agent-session and live sessions on the predecessor'
  incus --project "$PROJECT" file push "$ROOT/config/profiles/orca/tests/e2e/orca-projects-helper.py" \
    "$INSTANCE/tmp/orca-projects-helper.py" --mode 0755
  guest_root sh -c 'printf "#!/bin/sh\nexec sleep 3600\n" >/usr/local/bin/codex && chmod 0755 /usr/local/bin/codex'
  removed_source="$STATE/host/stuck-removed" neighbour_source="$STATE/host/stuck-neighbour"
  mkdir -p "$removed_source" "$neighbour_source"
  printf 'removed project\n' >"$removed_source/README"
  printf 'neighbour project\n' >"$neighbour_source/README"
  yard sync "$removed_source" --name stuck-removed --yes >/dev/null
  yard sync "$neighbour_source" --name stuck-neighbour --yes >/dev/null
  removed_root=/srv/workspaces/stuck-removed/src neighbour_root=/srv/workspaces/stuck-neighbour/src
  yard orca sync --yes >/dev/null
  removed_id="$(orca_repo_id "$removed_root")" || die 'removed fixture was not registered'
  neighbour_id="$(orca_repo_id "$neighbour_root")" || die 'neighbour fixture was not registered'
  removed_wt="$removed_id::$removed_root" neighbour_wt="$neighbour_id::$neighbour_root"

  saved_tab="$(orca_rpc session.tabs.createTerminal "$(jq -cn --arg selector "id:$removed_wt" \
    '{worktree:$selector,activate:false,clientMutationId:"orca-stuck-saved"}')" | jq -er '.tab.id')" \
    || die 'saved tab was not created'
  yard orca restart --yes >/dev/null
  stuck_tabs "$removed_wt" | grep -Fxq "$saved_tab" || die 'saved tab did not survive the Orca restart'
  agent_tab="$(stuck_agent_tab "$removed_wt" removed)"
  read -r live_tab live_pty < <(stuck_live_terminal "$removed_wt") || die 'live terminal was not created'
  neighbour_agent="$(stuck_agent_tab "$neighbour_wt" neighbour)"
  read -r neighbour_live neighbour_pty < <(stuck_live_terminal "$neighbour_wt") \
    || die 'neighbour live terminal was not created'
  orca_before="$(stuck_orca_version)" || die 'predecessor Orca package version is unreadable'

  yard remove stuck-removed --yes >"$STATE/stuck-remove.out" 2>"$STATE/stuck-remove.err" \
    || die 'predecessor project removal failed'
  orca_catalog_has "$removed_root" || die 'predecessor unexpectedly removed the record with sessions'

  stage "updating to published $UPGRADE_TARGET, which must stop in recovery"
  if YARD_RELEASE_BASE_URL="https://github.com/subyard/subyard/releases/download/v$UPGRADE_TARGET" \
    yard update --version "$UPGRADE_TARGET" --yes >"$STATE/stuck-target.out" 2>"$STATE/stuck-target.err"; then
    die "published $UPGRADE_TARGET unexpectedly completed with a retained removed-project record"
  fi
  grep -Fq 'recovery-ambiguous' "$STATE/stuck-target.out" "$STATE/stuck-target.err" \
    || { show_output "$STATE/stuck-target.out" "$STATE/stuck-target.err"
         die "published $UPGRADE_TARGET did not stop in recovery-ambiguous"; }
  if yard update --yes >"$STATE/stuck-plain.out" 2>"$STATE/stuck-plain.err"; then
    die "plain update unexpectedly completed the stuck $UPGRADE_TARGET transition"
  fi
  # The hint names the newest published stable release, which this fixture does not control.
  hint=absent
  if grep -Fq 'yard update --version' "$STATE/stuck-plain.out" "$STATE/stuck-plain.err"; then hint=present; fi

  stage "recovering through yard update --version $release_version"
  [ "$(printf '%s\n%s\n' "$UPGRADE_TARGET" "$release_version" | sort -V | tail -n1)" = "$release_version" ] \
    && [ "$release_version" != "$UPGRADE_TARGET" ] \
    || die "candidate $release_version must sort above $UPGRADE_TARGET to replace its journal"
  started="$(date +%s)"
  YARD_RELEASE_BASE_URL="file://$STATE/release" yard update --version "$release_version" --yes \
    >"$STATE/stuck-recover.out" 2>"$STATE/stuck-recover.err" \
    || { report_config_failure "$STATE/stuck-recover.err"; show_output "$STATE/stuck-recover.out" "$STATE/stuck-recover.err"
         die 'candidate update did not recover the stuck transition'; }
  elapsed=$(( $(date +%s) - started ))
  [ "$(yard version)" = "yard $release_version" ] || die 'candidate is not active after recovery'
  INSTALLED_RELEASE="$(readlink "$SUBYARD_HOME/runtime/current")"
  INSTALLED_RELEASE="${INSTALLED_RELEASE#releases/}"
  release_ready stuck-recovered || { print_release_state stuck-recovered; die 'recovered release is not ready'; }
  helper_target="$(release_orca_helper_hash "$SUBYARD_HOME/runtime/$(readlink "$SUBYARD_HOME/runtime/current")")"
  [ "$(orca_registration_hash)" = "$helper_target" ] || die 'installed Orca helper is not the candidate helper'
  [ "$(stuck_orca_version)" = "$orca_before" ] || die 'the update changed the Orca package before the hook ran'

  stage 'checking that only the removed record and its sessions are gone'
  orca_catalog_has "$removed_root" && die 'recovered update retained the removed-project record'
  orca_rpc session.tabs.listAll | jq -e --arg prefix "$removed_id::" \
    '.snapshots | all(((.worktree | startswith($prefix)) and (.tabs | length > 0)) | not)' >/dev/null \
    || die 'removed-project tabs survived the recovery'
  stuck_pty_live "$live_pty" && die 'removed-project live terminal survived the recovery'
  stuck_pty_live "$neighbour_pty" || die 'recovery ended the neighbour live terminal'
  stuck_tabs "$neighbour_wt" | grep -Fxq "$neighbour_agent" || die 'recovery closed the neighbour agent-session tab'
  stuck_tabs "$neighbour_wt" | grep -Fxq "$neighbour_live" || die 'recovery closed the neighbour terminal tab'
  yard config status >"$STATE/stuck-config.out" 2>"$STATE/stuck-config.err" \
    || die 'materialized config is not converged after recovery'
  yard orca status >"$STATE/stuck-status.out" 2>"$STATE/stuck-status.err" \
    || { show_output "$STATE/stuck-status.out" "$STATE/stuck-status.err"; die 'Orca status failed after recovery'; }
  # The predecessor's Orca package and service stay until orca up; their status warnings are
  # expected here. Project registration and cleanup must already be clean.
  if grep -h -F '[warn]' "$STATE/stuck-status.out" "$STATE/stuck-status.err" \
    | grep -Eiq 'cleanup|registration|checkouts registered|orca registration'; then
    show_output "$STATE/stuck-status.out" "$STATE/stuck-status.err"
    die 'Orca status warns about project registration or cleanup after recovery'
  fi
  status_warnings="$(cat "$STATE/stuck-status.out" "$STATE/stuck-status.err" | grep -c -F '[warn]' || true)"
  yard orca up --yes >"$STATE/stuck-up.out" 2>"$STATE/stuck-up.err" \
    || { show_output "$STATE/stuck-up.out" "$STATE/stuck-up.err"; die 'orca up failed after recovery'; }
  yard orca status >"$STATE/stuck-status-up.out" 2>"$STATE/stuck-status-up.err" \
    || { show_output "$STATE/stuck-status-up.out" "$STATE/stuck-status-up.err"; die 'Orca status failed after orca up'; }
  if grep -Fq '[warn]' "$STATE/stuck-status-up.out" "$STATE/stuck-status-up.err"; then
    show_output "$STATE/stuck-status-up.out" "$STATE/stuck-status-up.err"
    die 'Orca status warns after orca up'
  fi
  started="$(date +%s)"
  guest_dev timeout 125 /usr/local/libexec/subyard/projects-changed >"$STATE/stuck-hook.out" 2>"$STATE/stuck-hook.err" \
    || { show_output "$STATE/stuck-hook.out" "$STATE/stuck-hook.err"; die 'installed project hooks fail after recovery'; }
  printf 'orca-stuck-recovery-evidence: from=%s stuck=%s candidate=%s orca=%s newer-release-hint=%s recover-seconds=%s steady-hook-seconds=%s pre-up-status-warnings=%s saved-tab=%s agent-tab=%s live-tab=%s\n' \
    "$UPGRADE_FROM" "$UPGRADE_TARGET" "$release_version" "$orca_before" "$hint" "$elapsed" \
    "$(( $(date +%s) - started ))" "$status_warnings" "$saved_tab" "$agent_tab" "$live_tab"
}

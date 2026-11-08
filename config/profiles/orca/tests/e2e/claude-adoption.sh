#!/usr/bin/env bash
# Sourced only by the marker-owned installed Orca fixture on a disposable VM.
# shellcheck disable=SC2154
set -Eeuo pipefail
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ "${MANAGED_AGENTS:-}" = 1 ] \
  && [ -f "${STATE:-}/.marker" ] \
  && [ "$(<"$STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ] \
  || die 'Claude adoption needs the marked disposable Orca fixture'

stage 'seeding Claude data created before integration selection'
adoption_orca_pid="$(guest_root systemctl show -p MainPID --value subyard-orca.service)"
adoption_xdg="$(guest_root sh -c "tr '\\0' '\\n' < /proc/$adoption_orca_pid/environ | sed -n 's/^XDG_DATA_HOME=//p'")"
[ -n "$adoption_xdg" ] || die 'Orca data home is unavailable'
guest_root curl -fsSL --max-time 120 https://claude.ai/install.sh -o /tmp/claude-native-install.sh
guest_root runuser -u dev -- env HOME=/home/dev XDG_DATA_HOME="$adoption_xdg" \
  bash /tmp/claude-native-install.sh stable
guest_root runuser -u dev -- bash -euc '
  install -d -m 0700 /home/dev/.claude/projects/orca-existing /home/dev/.claude-memory
  printf "retained session\\n" > /home/dev/.claude/projects/orca-existing/session.jsonl
  printf "retained memory\\n" > /home/dev/.claude-memory/MEMORY.md
  printf "{\"hooks\":{},\"model\":\"sonnet\",\"theme\":\"dark\"}\\n" > /home/dev/.claude/settings.json
  chmod 0644 /home/dev/.claude/settings.json
'
guest_root install -d -m 0755 /etc/claude-code/managed-settings.d
guest_root python3 - <<'PY_ADOPTION_POLICY'
import json
from pathlib import Path
policy = {"$comment": "Subyard Claude permission policy", "permissions": {
    "ask": ["Bash(git commit)", "Bash(git commit:*)", "Bash(git push)", "Bash(git push:*)"]}}
path = Path('/etc/claude-code/managed-settings.d/50-subyard-git-approval.json')
path.write_text(json.dumps(policy) + '\n')
path.chmod(0o644)
path = Path('/usr/local/bin/claude-policy-check')
path.write_text('#!/bin/sh\n# Subyard Claude permission policy\nexit 0\n')
path.chmod(0o755)
PY_ADOPTION_POLICY
# Every failure after a yard command prints that command's captured output.
fail_with() {
  local message=$1 file
  shift
  for file in "$@"; do
    [ -f "$file" ] || continue
    printf '==> %s <==\n' "$file" >&2
    cat "$file" >&2
  done
  die "$message"
}
unmanaged_logs=("$STATE/claude-unmanaged-status.json" "$STATE/claude-unmanaged-status.err")
yard integration status claude --json >"${unmanaged_logs[0]}" 2>"${unmanaged_logs[1]}" \
  || fail_with 'status of unselected Claude failed' "${unmanaged_logs[@]}"
jq -e '.observed == "unmanaged" and .unmanaged.claude' "${unmanaged_logs[0]}" >/dev/null \
  || fail_with 'unselected Orca Claude was not reported as unmanaged' "${unmanaged_logs[@]}"
yard_status_logs=("$STATE/claude-unmanaged-yard-status.out" "$STATE/claude-unmanaged-yard-status.err")
yard -Y default status >"${yard_status_logs[0]}" 2>"${yard_status_logs[1]}" \
  || fail_with 'detailed yard status failed' "${yard_status_logs[@]}"
grep -Eq 'claude.*unmanaged' "${yard_status_logs[0]}" \
  || fail_with 'detailed yard status omitted unmanaged Claude' "${yard_status_logs[@]}"
# A failure before adoption must preserve ordinary commands and a typed way back.
# shellcheck source=config/profiles/orca/tests/e2e/claude-recovery.sh
. "$ROOT/config/profiles/orca/tests/e2e/claude-recovery.sh"
[ "${CLAUDE_RECOVERY_ONLY:-0}" != 1 ] || exit 0
# Shows the plan on a terminal and declines it; --yes answers without printing it.
yard_declined() {
  local command
  printf -v command '%q ' "$YARD_BIN" "$@"
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    command="sg incus-admin -c $(printf '%q' "exec $command")"
  fi
  printf 'n\n' | timeout --signal=TERM --kill-after=5s 900 script -q -e -c "$command" /dev/null
}

stage 'disclosing target entries that win over the source'
guest_root install -d -o dev -g dev -m 0700 /mnt/host/agent-sessions/claude \
  /mnt/host/agent-sessions/claude/projects /mnt/host/agent-sessions/claude/projects/orca-existing \
  /mnt/host/agent-sessions/claude/memory
guest_root runuser -u dev -- sh -eu -c '
  printf "retained target session\n" > /mnt/host/agent-sessions/claude/projects/orca-existing/session.jsonl
  printf "retained target memory\n" > /mnt/host/agent-sessions/claude/memory/MEMORY.md
  chmod 0600 /mnt/host/agent-sessions/claude/projects/orca-existing/session.jsonl /mnt/host/agent-sessions/claude/memory/MEMORY.md
'
if yard_declined integration enable claude >"$STATE/claude-adoption-existing-plan.out" 2>&1; then
  fail_with 'declined Claude enable was applied' "$STATE/claude-adoption-existing-plan.out"
fi
grep -Fq 'already exist in the target and will not be transferred' "$STATE/claude-adoption-existing-plan.out" \
  && grep -Fq 'orca-existing/session.jsonl' "$STATE/claude-adoption-existing-plan.out" \
  && grep -Fq 'MEMORY.md' "$STATE/claude-adoption-existing-plan.out" \
  || fail_with 'plan did not disclose the entries the target keeps' "$STATE/claude-adoption-existing-plan.out"
guest_root grep -Fxq 'retained session' /home/dev/.claude/projects/orca-existing/session.jsonl
guest_root grep -Fxq 'retained memory' /home/dev/.claude-memory/MEMORY.md

stage 'seeding about 200 MB of session data'
guest_root runuser -u dev -- sh -eu -c '
  install -d -m 0700 /home/dev/.claude/projects/bulk
  i=0
  while [ "$i" -lt 2000 ]; do
    head -c 102400 /dev/urandom > /home/dev/.claude/projects/bulk/part-$i.bin
    i=$((i + 1))
  done
  cd /home/dev/.claude/projects/bulk && sha256sum part-*.bin > /tmp/subyard-claude-bulk.sha256
'
projects_inode="$(guest_root stat -c %i /home/dev/.claude/projects)"

stage 'interrupting a started adoption, then deselecting Claude'
# The guest-side watcher kills the transfer program once its descriptor exists.
guest_root sh -c 'rm -f /tmp/subyard-claude-interrupted; setsid sh -c "
  for _ in \$(seq 1 30000); do
    if [ -e /home/dev/.claude/projects.subyard-transfer.json ]; then
      pkill -KILL -f -- \"[.]subyard-stage-\" && touch /tmp/subyard-claude-interrupted
      exit 0
    fi
    sleep 0.01
  done" >/dev/null 2>&1 </dev/null &'
interrupt_logs=("$STATE/claude-adoption-interrupt.out" "$STATE/claude-adoption-interrupt.err")
if yard integration enable claude --yes >"${interrupt_logs[0]}" 2>"${interrupt_logs[1]}"; then
  fail_with 'interrupted adoption reported success' "${interrupt_logs[@]}"
fi
guest_root test -f /tmp/subyard-claude-interrupted \
  || fail_with 'adoption transfer was not interrupted' "${interrupt_logs[@]}"
guest_root test ! -L /home/dev/.claude/projects || fail_with 'interrupt landed after the exchange' "${interrupt_logs[@]}"
[ "$(guest_root stat -c %i /home/dev/.claude/projects)" = "$projects_inode" ] \
  || fail_with 'interrupted adoption replaced the source' "${interrupt_logs[@]}"
guest_root test -f /home/dev/.claude/projects.subyard-transfer.json \
  || fail_with 'interrupted adoption left no transfer record' "${interrupt_logs[@]}"
# The memory mapping sorts first, shares the target parent and is already exchanged.
[ "$(guest_root readlink /home/dev/.claude-memory)" = /mnt/host/agent-sessions/claude/memory ] \
  || fail_with 'memory mapping was not exchanged before the projects interruption' "${interrupt_logs[@]}"
pending_stage="$(guest_root sh -c 'ls -d /mnt/host/agent-sessions/claude/.subyard-stage-*')"
[ -n "$pending_stage" ] || fail_with 'interrupted adoption left no stage' "${interrupt_logs[@]}"
pending_logs=("$STATE/claude-adoption-pending-status.json" "$STATE/claude-adoption-pending-status.err")
yard integration status claude --json >"${pending_logs[0]}" 2>"${pending_logs[1]}" \
  || fail_with 'status failed while a projects adoption is pending' "${pending_logs[@]}"
jq -e --arg stage "$pending_stage" \
  '[(.persist_retained // [])[].path] | index($stage) == null' "${pending_logs[0]}" >/dev/null \
  || fail_with 'status reported the pending stage as retained' "${pending_logs[@]}"
# Disable plans and applies the runtime reconciliation for the deselected
# selection itself: its confirmed plan discloses and performs the cancellation
# (stage copies discarded, descriptor removed, the directory untouched).
deselect_logs=("$STATE/claude-adoption-deselect.out" "$STATE/claude-adoption-deselect.err")
yard integration disable claude --yes >"${deselect_logs[0]}" 2>"${deselect_logs[1]}" \
  || fail_with 'deselecting a pending adoption failed' "${deselect_logs[@]}"
grep -Fq 'cancel the interrupted adoption of /home/dev/.claude/projects' "${deselect_logs[@]}" \
  || fail_with 'disable plan did not disclose the cancellation' "${deselect_logs[@]}"
[ "$(guest_root stat -c %i /home/dev/.claude/projects)" = "$projects_inode" ] \
  || fail_with 'cancellation replaced the source' "${deselect_logs[@]}"
guest_root sh -c '! ls -d /home/dev/.claude/projects.subyard-* /mnt/host/agent-sessions/claude/.subyard-stage-* 2>/dev/null' \
  || fail_with 'cancellation left transfer state' "${deselect_logs[@]}"
guest_root runuser -u dev -- sh -c 'cd /home/dev/.claude/projects/bulk && sha256sum -c --quiet /tmp/subyard-claude-bulk.sha256' \
  || fail_with 'cancellation changed source data' "${deselect_logs[@]}"
# Nothing is left for init: it completes without asking for confirmation.
yard_declined init >"$STATE/claude-adoption-deselect-init.out" 2>&1 \
  || fail_with 'init after the cancellation was not a no-op' "$STATE/claude-adoption-deselect-init.out" "${deselect_logs[@]}"
if grep -Fq 'integration.persist' "$STATE/claude-adoption-deselect-init.out"; then
  fail_with 'init after the cancellation planned persist work' "$STATE/claude-adoption-deselect-init.out"
fi
guest_root grep -Fxq 'retained session' /home/dev/.claude/projects/orca-existing/session.jsonl

stage 'adopting with a real open-per-append writer and a held descriptor'
guest_root install -d -o dev -g dev -m 0700 /tmp/subyard-claude-adoption-writer
guest_root runuser -u dev -- sh -eu -c 'cat > /tmp/subyard-claude-adoption-writer/write.py; chmod 0600 /tmp/subyard-claude-adoption-writer/write.py' <<'PY_LIVE_WRITER'
import os, time
from pathlib import Path
root = Path('/tmp/subyard-claude-adoption-writer')
live = '/home/dev/.claude/projects/orca-existing/live.jsonl'
deadline = time.monotonic() + 900
with open('/home/dev/.claude/projects/orca-existing/held.jsonl', 'a') as held:
    held.write('held-before\n')
    held.flush()
    index = 0
    while not (root / 'stop').exists():
        if time.monotonic() > deadline:
            raise RuntimeError('fixture writer stop timed out')
        # Like a session log: open by path for every append.
        with open(live, 'a') as stream:
            stream.write(f'{index}\n')
        (root / 'progress.tmp').write_text(str(index))
        os.replace(root / 'progress.tmp', root / 'progress')
        index += 1
        time.sleep(0.002)
    # This descriptor was opened before the exchange and still names the backup.
    held.write('held-after\n')
(root / 'done').write_text(str(index - 1))
PY_LIVE_WRITER
guest_root sh -c 'setsid runuser -u dev -- python3 -B /tmp/subyard-claude-adoption-writer/write.py >/tmp/subyard-claude-adoption-writer/output 2>&1 </dev/null &'
writer_progress() { guest_root cat /tmp/subyard-claude-adoption-writer/progress 2>/dev/null || echo -1; }
for _ in $(seq 1 100); do [ "$(writer_progress)" -ge 50 ] && break; sleep 0.1; done
[ "$(writer_progress)" -ge 50 ] || die 'fixture writer did not start'
before_enable="$(writer_progress)"
enable_logs=("$STATE/claude-adoption-enable.out" "$STATE/claude-adoption-enable.err")
yard integration enable claude --yes >"${enable_logs[0]}" 2>"${enable_logs[1]}" \
  || fail_with 'Claude adoption enable failed with a live writer' "${enable_logs[@]}"
after_enable="$(writer_progress)"
for _ in $(seq 1 100); do [ "$(writer_progress)" -ge $((after_enable + 50)) ] && break; sleep 0.1; done
guest_root runuser -u dev -- touch /tmp/subyard-claude-adoption-writer/stop
for _ in $(seq 1 100); do guest_root test -f /tmp/subyard-claude-adoption-writer/done && break; sleep 0.1; done
guest_root test -f /tmp/subyard-claude-adoption-writer/done \
  || { guest_root cat /tmp/subyard-claude-adoption-writer/output >&2; die 'fixture writer did not finish'; }
[ "$(guest_root readlink /home/dev/.claude/projects)" = /mnt/host/agent-sessions/claude/projects ] \
  || fail_with 'Claude projects were not adopted' "${enable_logs[@]}"
[ "$(guest_root readlink /home/dev/.claude-memory)" = /mnt/host/agent-sessions/claude/memory ] \
  || fail_with 'Claude memory was not adopted' "${enable_logs[@]}"
# The backup is deleted after the switch; nothing is retained.
guest_root sh -c '! ls -d /home/dev/.claude/projects.subyard-* /home/dev/.claude-memory.subyard-* /mnt/host/agent-sessions/claude/.subyard-stage-* 2>/dev/null' \
  || fail_with 'successful adoption left a backup, stage or record' "${enable_logs[@]}"
# Lines written before the adoption started precede its catch-up pass and must
# be in the target; lines between the catch-up and the exchange are an accepted loss.
guest_root python3 - "$before_enable" <<'PY_NO_GAP' || fail_with 'writer lines from before the adoption are missing from the target' "${enable_logs[@]}"
import sys
first_pass = int(sys.argv[1])
last = int(open('/tmp/subyard-claude-adoption-writer/done').read())
target = set(open('/mnt/host/agent-sessions/claude/projects/orca-existing/live.jsonl').read().split())
missing = [index for index in range(first_pass + 1) if str(index) not in target]
if missing:
    raise SystemExit(f'missing lines {missing[:20]} of {first_pass + 1}')
if str(last) not in target:
    raise SystemExit('writes after the exchange did not reach the target')
PY_NO_GAP
guest_root grep -Fxq 'held-before' /mnt/host/agent-sessions/claude/projects/orca-existing/held.jsonl || fail_with 'held file was not transferred' "${enable_logs[@]}"
if guest_root grep -Fxq 'held-after' /mnt/host/agent-sessions/claude/projects/orca-existing/held.jsonl; then
  fail_with 'held descriptor unexpectedly wrote into the target' "${enable_logs[@]}"
fi
guest_root runuser -u dev -- sh -c 'cd /home/dev/.claude/projects/bulk && sha256sum -c --quiet /tmp/subyard-claude-bulk.sha256' \
  || fail_with 'adopted bulk data differs' "${enable_logs[@]}"
# The target won for entries it already had.
guest_root grep -Fxq 'retained target session' /home/dev/.claude/projects/orca-existing/session.jsonl
guest_root grep -Fxq 'retained target memory' /home/dev/.claude-memory/MEMORY.md
guest_root jq -e '.autoMemoryDirectory and .permissions and .hooks and .model == "sonnet" and .theme == "dark"' \
  /home/dev/.claude/settings.json >/dev/null || fail_with 'Claude settings lost foreign or managed fields' "${enable_logs[@]}"

stage 'reporting nothing retained after a successful adoption'
status_logs=("$STATE/claude-adoption-status.json" "$STATE/claude-adoption-status.err")
yard integration status claude --json >"${status_logs[0]}" 2>"${status_logs[1]}" \
  || fail_with 'status after the adoption failed' "${status_logs[@]}"
jq -e '(.persist_retained // []) | length == 0' "${status_logs[0]}" >/dev/null \
  || fail_with 'status listed retained data after success' "${status_logs[@]}"

stage 'repeating init without persist changes'
state_before="$(guest_root sh -c 'ls -d /home/dev/.claude/projects.subyard-* /home/dev/.claude-memory.subyard-* 2>/dev/null | sort; stat -c %i /mnt/host/agent-sessions/claude/projects')"
repeat_logs=("$STATE/claude-adoption-repeat.out" "$STATE/claude-adoption-repeat.err")
yard init --yes >"${repeat_logs[0]}" 2>"${repeat_logs[1]}" \
  || fail_with 'Claude adoption repeat init failed' "${repeat_logs[@]}"
[ "$(guest_root sh -c 'ls -d /home/dev/.claude/projects.subyard-* /home/dev/.claude-memory.subyard-* 2>/dev/null | sort; stat -c %i /mnt/host/agent-sessions/claude/projects')" = "$state_before" ] \
  || fail_with 'repeat init changed persist state' "${repeat_logs[@]}"

guest_root rm -rf /home/dev/.claude/projects/bulk

stage 'adopting a source that became a directory again, without configuration drift'
guest_root runuser -u dev -- sh -eu -c '
  rm /home/dev/.claude/projects
  install -d -m 0700 /home/dev/.claude/projects /home/dev/.claude/projects/late
  i=0
  while [ "$i" -lt 100 ]; do
    head -c 10240 /dev/urandom > /home/dev/.claude/projects/late/part-$i.bin
    i=$((i + 1))
  done
  cd /home/dev/.claude/projects/late && sha256sum part-*.bin > /tmp/subyard-claude-late.sha256
'
again_logs=("$STATE/claude-adoption-again.out" "$STATE/claude-adoption-again.err")
yard init --yes >"${again_logs[0]}" 2>"${again_logs[1]}" \
  || fail_with 'yard init did not adopt the recreated directory' "${again_logs[@]}"
[ "$(guest_root readlink /home/dev/.claude/projects)" = /mnt/host/agent-sessions/claude/projects ] \
  || fail_with 'recreated directory was not adopted' "${again_logs[@]}"
guest_root runuser -u dev -- sh -c 'cd /home/dev/.claude/projects/late && sha256sum -c --quiet /tmp/subyard-claude-late.sha256' \
  || fail_with 'adoption of the recreated directory lost data' "${again_logs[@]}"
guest_root sh -c '! ls -d /home/dev/.claude/projects.subyard-* /mnt/host/agent-sessions/claude/.subyard-stage-* 2>/dev/null' \
  || fail_with 'finished adoption left its backup, stage or record' "${again_logs[@]}"
yard init --yes >"$STATE/claude-adoption-again-repeat.out" 2>"$STATE/claude-adoption-again-repeat.err" \
  || fail_with 'repeat init after the adoption failed' "$STATE/claude-adoption-again-repeat.out" "$STATE/claude-adoption-again-repeat.err"

stage 'reporting a retained backup as state with a manual next step'
retained_backup=/home/dev/.claude/projects.subyard-backup-0123456789abcdef
guest_root runuser -u dev -- sh -eu -c 'install -d -m 0700 "$1" && printf "kept\n" > "$1/kept"' sh "$retained_backup"
retained_json_logs=("$STATE/claude-adoption-retained.json" "$STATE/claude-adoption-retained-json.err")
yard integration status claude --json >"${retained_json_logs[0]}" 2>"${retained_json_logs[1]}" \
  || fail_with 'status with a retained backup failed' "${retained_json_logs[@]}"
jq -e --arg path "$retained_backup" '[(.persist_retained // [])[] | select(.path == $path and .kind == "backup" and .bytes == 5)] | length == 1' \
  "$STATE/claude-adoption-retained.json" >/dev/null \
  || fail_with 'status omitted the retained backup' "${retained_json_logs[@]}"
retained_logs=("$STATE/claude-adoption-retained.out" "$STATE/claude-adoption-retained.err")
yard integration status claude >"${retained_logs[0]}" 2>"${retained_logs[1]}" \
  || fail_with 'status text with a retained backup failed' "${retained_logs[@]}"
grep -Fq "persist retained backup $retained_backup (5 bytes)" "$STATE/claude-adoption-retained.out" \
  && grep -Fq "'ls' '-la'" "$STATE/claude-adoption-retained.out" \
  || fail_with 'status text omitted the retained backup or its next step' "${retained_logs[@]}"
if grep -Eiq 'warn' "$STATE/claude-adoption-retained.out"; then
  fail_with 'retained backup was rendered as a warning' "${retained_logs[@]}"
fi
retained_init_logs=("$STATE/claude-adoption-retained-init.out" "$STATE/claude-adoption-retained-init.err")
yard init --yes >"${retained_init_logs[0]}" 2>"${retained_init_logs[1]}" \
  || fail_with 'a retained backup blocked yard init' "${retained_init_logs[@]}"
guest_root grep -Fxq kept "$retained_backup/kept" || fail_with 'init removed a retained backup' "${retained_init_logs[@]}"
guest_root rm -r -- "$retained_backup"
guest_root rm -rf /home/dev/.claude/projects/late

stage 'Claude adoption, interruption and cancel by disable, target-wins disclosure, backup deletion, repeat init, re-adoption and retained reporting passed'

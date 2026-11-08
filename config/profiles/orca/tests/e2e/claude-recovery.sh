#!/usr/bin/env bash
# Sourced within the marker-owned Orca adoption fixture on its leased VM.
# shellcheck disable=SC2154
set -Eeuo pipefail
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ "${MANAGED_AGENTS:-}" = 1 ] \
  && [ "$(<"$STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ] \
  || die 'Claude recovery requires the marked disposable adoption fixture'

stage 'checking ordinary commands and typed rollback after a refused Claude init'
yard --version
printf 'claude-recovery: fresh installed candidate; codex -> codex claude; foreign instructions force pre-apply refusal\n'
jq -c '{checkpoint, releases, migration_steps:(.steps | length)}' \
  "$SUBYARD_CONFIG_HOME/release-transition/v2/journal.json"
recovery_selection="$SUBYARD_CONFIG_HOME/yards/default/config.env"
cp -p "$recovery_selection" "$STATE/claude-recovery-selection-before.env"
recovery_failure() {
  local phase
  for phase in init inspect unlock rollback; do
    [ ! -f "$STATE/claude-recovery-$phase.err" ] || tail -n 25 "$STATE/claude-recovery-$phase.err" >&2
  done
  # Exact fixture-only desired-selection cleanup, never a product recovery claim.
  cp -p "$STATE/claude-recovery-selection-before.env" "$recovery_selection"
  die "$1"
}
for recovery_shape in applied legacy; do
stage "Claude recovery shape: $recovery_shape"
if [ "$recovery_shape" = legacy ]; then
  # Supported historical inventory: ownership is recorded, applied init settings
  # are unknown. This changes fixture integration evidence, never release history.
  guest_root python3 - <<'PY_LEGACY_APPLIED'
import json, os
path = '/var/lib/subyard/integrations/inventory.json'
with open(path) as stream:
    value = json.load(stream)
with open(path + '.fixture-applied-before', 'w') as stream:
    os.fchmod(stream.fileno(), 0o600)
    json.dump(value.get('applied_settings'), stream)
value.pop('applied_settings', None)
with open(path + '.fixture-new', 'w') as stream:
    os.fchmod(stream.fileno(), 0o600)
    json.dump(value, stream)
    stream.flush()
    os.fsync(stream.fileno())
os.replace(path + '.fixture-new', path)
PY_LEGACY_APPLIED
fi
guest_root runuser -u dev -- sh -eu -c 'printf "Foreign instructions retained.\n" > /home/dev/.claude/CLAUDE.md; chmod 0644 /home/dev/.claude/CLAUDE.md'
yard config set CODING_TOOL_INTEGRATIONS 'codex claude' --scope yard --local --yes \
  >"$STATE/claude-recovery-select.out" 2>"$STATE/claude-recovery-select.err" \
  || recovery_failure 'could not author Claude selection before the refused init'
if yard init --yes >"$STATE/claude-recovery-init.out" 2>"$STATE/claude-recovery-init.err"; then
  recovery_failure 'init unexpectedly replaced foreign Claude instructions'
fi
grep -Fq '/home/dev/.claude/CLAUDE.md' "$STATE/claude-recovery-init.err" \
  || recovery_failure 'init refused for an unrelated reason'
inspect_status=0
yard migrate --check --json >"$STATE/claude-recovery-inspect.json" 2>"$STATE/claude-recovery-inspect.err" || inspect_status=$?
printf 'claude-recovery: migrate check exit=%s\n' "$inspect_status"
jq -r '.outcome | "claude-recovery: readiness=\(.status) code=\(.code)"' "$STATE/claude-recovery-inspect.json"
if [ "$recovery_shape" = legacy ]; then
  if jq -e '.outcome.status == "ready"' "$STATE/claude-recovery-inspect.json" >/dev/null; then
    stage 'missing applied snapshot did not reproduce an ownership gate on this fresh history'
  else
    jq -e '.outcome.status == "operator-action-required" and
      any(.blockers[]?; .resource == "activation.materialized-config.pending-configuration")' \
      "$STATE/claude-recovery-inspect.json" >/dev/null \
      || recovery_failure 'legacy inventory has an unrelated readiness blocker'
    stage 'pending ownership gate reproduced without an applied snapshot'
  fi
fi
# No key is enrolled: reaching the key handler, rather than release refusal,
# establishes that the unrelated command is admitted without granting credentials.
unlock_status=0
yard ssh-agent unlock --key missing-claude-recovery-fixture --ttl 48h --yes \
  >"$STATE/claude-recovery-unlock.out" 2>"$STATE/claude-recovery-unlock.err" || unlock_status=$?
printf 'claude-recovery: unrelated unlock exit=%s\n' "$unlock_status"
ordinary_blocked=0
if grep -Eq 'activation\.materialized-config|operator-action-required|unowned selected artifact|integration ownership conflict' "$STATE/claude-recovery-unlock.err"; then
  ordinary_blocked=1
fi
rollback_status=0
yard config set CODING_TOOL_INTEGRATIONS codex --scope yard --local --yes \
  >"$STATE/claude-recovery-rollback.out" 2>"$STATE/claude-recovery-rollback.err" || rollback_status=$?
printf 'claude-recovery: typed rollback exit=%s\n' "$rollback_status"
if [ "$rollback_status" != 0 ]; then
  if grep -Fq '(V2 history)' "$STATE/claude-recovery-rollback.err"; then
    stage 'live V2-history refusal reproduced on the installed candidate'
  fi
  recovery_failure 'typed selection rollback refused after init ownership conflict'
fi
stage "V2-history reduce-selection refusal not reproduced: $recovery_shape; typed rollback passed"
[ "$ordinary_blocked" = 0 ] || recovery_failure 'ownership conflict blocked an unrelated SSH-agent command'
yard migrate --check --json >"$STATE/claude-recovery-restored.json" \
  || recovery_failure 'readiness query failed after typed rollback'
jq -e '.outcome.status == "ready" and .outcome.reachedGoal == true' "$STATE/claude-recovery-restored.json" >/dev/null \
  || recovery_failure 'typed rollback did not restore ready'
guest_root runuser -u dev -- mv /home/dev/.claude/CLAUDE.md "/home/dev/.claude/CLAUDE.md.foreign-preserved-$recovery_shape"
if [ "$recovery_shape" = legacy ]; then
  guest_root python3 - <<'PY_RESTORE_APPLIED'
import json, os
path = '/var/lib/subyard/integrations/inventory.json'
with open(path) as stream:
    value = json.load(stream)
with open(path + '.fixture-applied-before') as stream:
    value['applied_settings'] = json.load(stream)
with open(path + '.fixture-new', 'w') as stream:
    os.fchmod(stream.fileno(), 0o600)
    json.dump(value, stream)
    stream.flush()
    os.fsync(stream.fileno())
os.replace(path + '.fixture-new', path)
os.unlink(path + '.fixture-applied-before')
PY_RESTORE_APPLIED
fi
stage "ordinary command admission and typed Claude rollback passed: $recovery_shape"
done

#!/usr/bin/env bash
# shellcheck disable=SC2154 # Variables come from the generic host-free ledger harness.
# OpenClaw-owned consumer materialization and exclusive handoff assertions.
# Sourced by the encrypted-ledger host-free harness after fixture trust is established.
secret='super-secret-static-value'
printf '%s' "$secret" | yard_one keys add staging-file --kind file --zone canonical --consumer staging-env --yes >/dev/null
shared_id="$(yard_one keys list | awk -F '\t' '$8=="staging-file" {print $1}')"
[ -n "$shared_id" ] || fail 'shared credential id missing'
initial_revision="$(yard_one keys history "$shared_id" | awk -F '\t' -v id="$shared_id" '$1==id {print $2; exit}')"
[ -n "$initial_revision" ] || fail 'initial revision missing from history'
if grep -rFq -- "$secret" "$SUBYARD_KEYS_ROOT"; then fail 'plaintext landed in the credential store'; fi

# Production denylist matching examines protected payload values without printing them and cleans temp input.
blocked_prod='dummy-production-token'
printf '%s' "$blocked_prod" | sha256sum | cut -d' ' -f1 > "$TMP/prod-fingerprints"
export SUBYARD_KEYS_PROD_FINGERPRINTS="$TMP/prod-fingerprints"
if printf 'BOT_TOKEN=%s\n' "$blocked_prod" | yard_one keys add blocked-prod --yes >"$TMP/prod.out" 2>&1; then
  fail 'production fingerprint entered the ledger'
fi
grep -Fq 'production fingerprint' "$TMP/prod.out" || fail 'production fingerprint rejection was unclear'
if grep -rFq -- "$blocked_prod" "$TMPDIR" 2>/dev/null; then fail 'rejected production payload remained in a temp file'; fi
if printf 'traversal-dummy' | yard_one keys add bad-zone --zone .. --consumer staging-env --yes >"$TMP/zone.out" 2>&1; then
  fail 'consumer path traversal zone was accepted'
fi
grep -Fq 'invalid credential zone' "$TMP/zone.out" || fail 'invalid zone rejection was unclear'

printf 'openclaw-host-only-value' | yard_one keys add openclaw-host-only --local-only --yes >/dev/null
local_id="$(yard_one keys list | awk -F '\t' '$8=="openclaw-host-only" {print $1}')"
yard_one keys sync @two --now --yes >/dev/null
yard_two keys list | grep -Fq staging-file || fail 'shared credential did not reach peer'
if yard_two keys list | grep -Fq "$local_id"; then fail 'local-only credential reached peer'; fi

yard_one keys materialize canonical --yes >/dev/null
[ "$(cat "$TMP/consumer-one/staging/canonical.env")" = "$secret" ] || fail 'local materialized content differs'
[ "$(cat "$TMP/consumer-two/staging/canonical.env")" = "$secret" ] || fail 'peer did not materialize after sync refresh'
[ "$(stat -c '%a' "$TMP/consumer-one/staging/canonical.env")" = 600 ] || fail 'consumer mode is not 0600'

# Same-value divergent rotations converge automatically.
printf 'same-rotation' | yard_one keys rotate "$shared_id" --yes >/dev/null
printf 'same-rotation' | yard_two keys rotate "$shared_id" --yes >/dev/null
yard_one keys sync @two --now --yes >/dev/null
[ "$(yard_one keys list | awk -F '\t' -v id="$shared_id" '$1==id {print $3}')" = 1 ] \
  || fail 'same-value rotations did not auto-merge'

# Different rotations remain multi-head and never choose silently.
printf 'rotation-A' | yard_one keys rotate "$shared_id" --yes >/dev/null
printf 'rotation-B' | yard_two keys rotate "$shared_id" --yes >/dev/null
yard_one keys sync @two --now --yes >/dev/null
[ -r "$TMP/consumer-one/staging/canonical.env" ] || fail 'verified consumer disappeared before conflict test'
last_verified="$(cat "$TMP/consumer-one/staging/canonical.env")"
[ "$(yard_one keys list | awk -F '\t' -v id="$shared_id" '$1==id {print $3":"$4}')" = '2:conflict' ] \
  || fail 'different rotations were silently resolved'
if yard_one keys materialize canonical --yes >"$TMP/conflict.out" 2>&1; then
  fail 'multi-head materialization unexpectedly succeeded'
fi
[ "$(cat "$TMP/consumer-one/staging/canonical.env")" = "$last_verified" ] \
  || fail 'conflict changed the last verified consumer'

# Explicit resolve collapses every current head.
chosen="$(find "$SUBYARD_KEYS_ROOT/one/shared/records/$shared_id" -name '*.json' -printf '%f\n' | sed 's/\.json$//' | sort | tail -n1)"
yard_one keys resolve "$shared_id" --choose "$chosen" --yes >/dev/null
yard_one keys sync @two --now --yes >/dev/null
[ "$(yard_two keys list | awk -F '\t' -v id="$shared_id" '$1==id {print $3}')" = 1 ] || fail 'explicit resolve did not converge'

# Rollback is a visible new successor, never a Git/history rewrite.
before_rollback_count="$(yard_one keys history "$shared_id" | awk -F '\t' -v id="$shared_id" '$1==id {n++} END{print n+0}')"
yard_one keys rollback "$shared_id" "$initial_revision" --yes >/dev/null
yard_one keys materialize canonical --yes >/dev/null
[ "$(cat "$TMP/consumer-one/staging/canonical.env")" = "$secret" ] || fail 'rollback did not restore historical value'
after_rollback_count="$(yard_one keys history "$shared_id" | awk -F '\t' -v id="$shared_id" '$1==id {n++} END{print n+0}')"
[ "$after_rollback_count" -eq $((before_rollback_count + 1)) ] || fail 'rollback did not append exactly one successor'

# Concurrent revoke versus update is deterministic: revoke wins and cannot resurrect.
printf 'revive-base' | yard_one keys add openclaw-revoke-race --yes >/dev/null
revoke_id="$(yard_one keys list | awk -F '\t' '$8=="openclaw-revoke-race" {print $1}')"
yard_one keys sync @two --now --yes >/dev/null
yard_one keys revoke "$revoke_id" --yes >/dev/null
printf 'resurrection-attempt' | yard_two keys rotate "$revoke_id" --yes >/dev/null
yard_one keys sync @two --now --yes >/dev/null
[ "$(yard_one keys list | awk -F '\t' -v id="$revoke_id" '$1==id {print $4}')" = revoked ] \
  || fail 'revoke-vs-update did not converge to revoked'

# Status is read-only and exposes bounded-staleness telemetry.
before="$(git -C "$SUBYARD_KEYS_ROOT/one/shared" rev-parse HEAD)"
yard_one keys status > "$TMP/status.out"
after="$(git -C "$SUBYARD_KEYS_ROOT/one/shared" rev-parse HEAD)"
[ "$before" = "$after" ] || fail 'keys status mutated the ledger'
grep -Fq 'policy=automatic' "$TMP/status.out" || fail 'status omitted auto-sync policy'
grep -Fq 'next-retry=' "$TMP/status.out" || fail 'status omitted retry/backoff telemetry'
yard_one keys auto-sync pause @two --yes >/dev/null
jq -e '.manualOnly == true' "$SUBYARD_KEYS_ROOT/one/peers/two.json" >/dev/null || fail 'auto-sync pause failed'
yard_one keys auto-sync resume @two --yes >/dev/null

# Exclusive ciphertext may replicate, but only the assigned yard materializes it. A cooperative handoff
# removes the old consumer, publishes an authority epoch, and makes the old start guard fail closed.
printf 'exclusive-bot-token' | yard_one keys add exclusive-bot --kind telegram --zone exclusive \
  --consumer staging-env --exclusive --yes >/dev/null
exclusive_id="$(yard_one keys list | awk -F '\t' '$8=="exclusive-bot" {print $1}')"
exclusive_head="$(find "$SUBYARD_KEYS_ROOT/one/shared/records/$exclusive_id" -name '*.json' -print -quit)"
[ "$(jq -r '.assignedYard' "$exclusive_head")" = "$actor_one/one" ] \
  || fail 'exclusive assignment does not qualify the yard with its host identity'
yard_one keys materialize exclusive --yes >/dev/null
yard_one keys sync @two --now --yes >/dev/null
[ -r "$TMP/consumer-one/staging/exclusive.env" ] || fail 'assigned exclusive consumer was not materialized'
[ ! -e "$TMP/consumer-two/staging/exclusive.env" ] || fail 'unassigned peer materialized an exclusive consumer'
mkdir -p "$TMP/fake-bin"
cat > "$TMP/fake-bin/incus" <<'SH'
#!/usr/bin/env bash
case "${1:-}" in info) exit 0 ;; list) printf 'RUNNING\n' ;; exec) exit 1 ;; *) exit 1 ;; esac
SH
chmod +x "$TMP/fake-bin/incus"
export PATH="$TMP/fake-bin:$PATH"
mv "$SUBYARD_KEYS_ROOT/two/shared.git" "$SUBYARD_KEYS_ROOT/two/shared.git.handoff-offline"
if yard_one keys move "$exclusive_id" @two --yes >"$TMP/handoff-offline.out" 2>&1; then
  fail 'exclusive handoff reported success while its target was offline'
fi
[ ! -e "$TMP/consumer-one/staging/exclusive.env" ] || fail 'published handoff kept the old exclusive consumer'
[ ! -e "$TMP/consumer-two/staging/exclusive.env" ] || fail 'offline handoff materialized an unconfirmed target'
mv "$SUBYARD_KEYS_ROOT/two/shared.git.handoff-offline" "$SUBYARD_KEYS_ROOT/two/shared.git"
yard_one keys move "$exclusive_id" @two --yes >/dev/null
[ ! -e "$TMP/consumer-one/staging/exclusive.env" ] || fail 'handoff kept the old exclusive consumer'
[ "$(cat "$TMP/consumer-two/staging/exclusive.env")" = exclusive-bot-token ] || fail 'handoff did not materialize the target'
if yard_one keys check-exclusive exclusive >"$TMP/old-grant.out" 2>&1; then fail 'old exclusive owner passed the start guard'; fi
yard_two keys check-exclusive exclusive >/dev/null || fail 'new exclusive owner failed a fresh authority grant'
cp "$SUBYARD_KEYS_ROOT/two/state/one.json" "$TMP/two-state-one.json"
jq '.lastSuccess=1' "$TMP/two-state-one.json" > "$SUBYARD_KEYS_ROOT/two/state/one.json"
chmod 0600 "$SUBYARD_KEYS_ROOT/two/state/one.json"
if yard_two keys check-exclusive exclusive >"$TMP/stale-grant.out" 2>&1; then
  fail 'stale exclusive authority grant passed the start guard'
fi
grep -Fq 'authority grant' "$TMP/stale-grant.out" || fail 'stale authority rejection was unclear'
install -m 0600 "$TMP/two-state-one.json" "$SUBYARD_KEYS_ROOT/two/state/one.json"

# Two yard contexts on one physical host share crypto identity and history but keep distinct
# assignment/materialization targets; no self-trust record is needed.
printf 'same-host-exclusive' | yard_one keys add same-host-bot --kind telegram --zone same-host \
  --consumer staging-env --exclusive --yes >/dev/null
same_host_id="$(yard_one keys list | awk -F '\t' '$8=="same-host-bot" {print $1}')"
yard_one keys materialize same-host --yes >/dev/null
yard_one keys move "$same_host_id" @one_alt --yes >/dev/null
[ ! -e "$TMP/consumer-one/staging/same-host.env" ] || fail 'same-host handoff kept the old consumer'
[ "$(cat "$TMP/consumer-one-alt/staging/same-host.env")" = same-host-exclusive ] \
  || fail 'same-host handoff did not materialize the target context'
same_host_head="$(find "$SUBYARD_KEYS_ROOT/one/shared/records/$same_host_id" -name '*.json' -printf '%p\n' | sort | tail -n1)"
[ "$(jq -r '.assignedYard' "$same_host_head")" = "$actor_one/one_alt" ] \
  || fail 'same-host handoff lost the yard context'

yard_one keys delete "$shared_id" --yes >/dev/null
[ ! -e "$TMP/consumer-one/staging/canonical.env" ] || fail 'tombstone kept the local consumer copy'
yard_one keys sync @two --now --yes >/dev/null
[ ! -e "$TMP/consumer-two/staging/canonical.env" ] || fail 'tombstone kept the peer consumer copy'
[ "$(yard_two keys list | awk -F '\t' -v id="$shared_id" '$1==id {print $4}')" = tombstone ] \
  || fail 'tombstone did not synchronize'


while IFS= read -r output_file; do
  for leaked in "$secret" "$blocked_prod" exclusive-bot-token; do
    if grep -Fq -- "$leaked" "$output_file" 2>/dev/null; then fail "plaintext appeared in output file $output_file"; fi
  done
done < <(find "$TMP" -maxdepth 1 -type f -name '*.out' -print)

credential_scenario_values+=(exclusive-bot-token)

#!/usr/bin/env bash
# Host-free encrypted-ledger contract: trust, Git exchange, convergence, local-only and quarantine.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home"
export SUBYARD_NO_AUDIT=1
SUBYARD_KEYS_ROOT="$SUBYARD_CONFIG_HOME/key-hosts"
export SUBYARD_KEYS_TOOLS_DIR="$SUBYARD_CONFIG_HOME/tools"
export SUBYARD_KEYS_SYSTEMD_DIR="$TMP/systemd"
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export TMPDIR="$TMP/tmp"
mkdir -p "$HOME" "$TMPDIR" "$SUBYARD_CONFIG_HOME/yards" "$SUBYARD_KEYS_TOOLS_DIR/bin"

# A source checkout is a valid development/E2E runtime after its explicit build. This fallback must
# not require a release install, while a release runtime remains preferred as soon as one exists.
[ -x "$ROOT/.build/yard" ] || fail 'development engine must be built before credential tests'
YARD_RUNTIME_ROOT="$TMP/missing-runtime" \
SUBYARD_KEYS_SYSTEMD_DIR="$TMP/dev-systemd" \
ASSUME_YES=1 \
  "$ROOT/scripts/install-keys-auto-sync.sh" >/dev/null
grep -Fxq "ExecStart=$ROOT/.build/yard _keys-auto-sync --if-due" \
  "$TMP/dev-systemd/subyard-keys-sync.service" \
  || fail 'credential timer did not accept the explicit development candidate'

install -d "$SUBYARD_HOME/runtime/current/bin"
ln -s "$ROOT/bin/yard" "$SUBYARD_HOME/runtime/current/bin/yard"

# A forced/non-login SSH command has no ambient user-bus variables. The installer must address the
# lingered user manager explicitly so real-host initialization can enable its timer from that path.
mkdir -p "$TMP/timer-bin"
cat > "$TMP/timer-bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s|%s|%s\n' "${XDG_RUNTIME_DIR:-}" "${DBUS_SESSION_BUS_ADDRESS:-}" "$*" \
  >> "$SYSTEMCTL_CALLS"
SH
cat > "$TMP/timer-bin/loginctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'show-user '*'-p Linger --value') printf 'yes\n' ;;
  *) exit 2 ;;
esac
SH
chmod +x "$TMP/timer-bin/systemctl" "$TMP/timer-bin/loginctl"
SYSTEMCTL_CALLS="$TMP/systemctl-calls" \
SUBYARD_KEYS_SYSTEMD_DIR="$TMP/live-systemd" \
SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=0 \
PATH="$TMP/timer-bin:$PATH" \
ASSUME_YES=1 \
  "$ROOT/scripts/install-keys-auto-sync.sh" >/dev/null
expected_runtime="/run/user/$(id -u)"
grep -Fxq "$expected_runtime|unix:path=$expected_runtime/bus|--user show-environment" "$TMP/systemctl-calls" \
  || fail 'credential timer did not address the lingered user manager from a non-login command'
grep -Fxq "$expected_runtime|unix:path=$expected_runtime/bus|--user daemon-reload" "$TMP/systemctl-calls" \
  || fail 'credential timer reload lost the explicit user-bus environment'
grep -Fxq "$expected_runtime|unix:path=$expected_runtime/bus|--user enable --now subyard-keys-sync.timer" \
  "$TMP/systemctl-calls" || fail 'credential timer enable lost the explicit user-bus environment'

# Small deterministic test doubles preserve the CLI contract without CI network. Revision files
# still contain no plaintext and are signed with real per-host OpenSSH signing identities.
cat > "$SUBYARD_KEYS_TOOLS_DIR/bin/age" <<'SH'
#!/usr/bin/env bash
[ "${1:-}" = --version ] && { echo 'age 1.3.1'; exit 0; }
exit 2
SH
cat > "$SUBYARD_KEYS_TOOLS_DIR/bin/age-keygen" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  --version) echo 'age-keygen 1.3.1' ;;
  -o)
    recipient="age1$(printf '%s-%s-%s' "$2" "$$" "$RANDOM" | sha256sum | cut -c1-58)"
    printf 'FAKE:%s\n' "$recipient" > "$2"
    printf 'Public key: %s\n' "$recipient" >&2 ;;
  -y) sed -n 's/^FAKE://p' "$2" ;;
  *) exit 2 ;;
esac
SH
cat > "$SUBYARD_KEYS_TOOLS_DIR/bin/sops" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in --version) echo 'sops 3.13.2'; exit 0 ;; esac
mode="${1:-}"; shift || true
age_csv=''; input=''
while [ $# -gt 0 ]; do
  case "$1" in
    --age) age_csv="$2"; shift 2 ;;
    --encrypted-regex|--input-type|--output-type) shift 2 ;;
    -*) shift ;;
    *) input="$1"; shift ;;
  esac
done
[ -r "$input" ] || exit 2
case "$mode" in
  encrypt)
    payload="$(jq -r '.payload' "$input")"
    wrapped="$(printf '%s' "$payload" | base64 -w0)"
    jq --arg payload "ENC[$wrapped]" --arg recipients "$age_csv" '
      .payload=$payload |
      .sops={age:($recipients|split(",")|map({recipient:.,enc:"test-envelope"})),mac:"test-mac"}
    ' "$input" ;;
  decrypt)
    wrapped="$(jq -r '.payload' "$input")"
    case "$wrapped" in ENC\[*\]) ;; *) exit 1 ;; esac
    wrapped="${wrapped#ENC[}"; wrapped="${wrapped%]}"
    payload="$(printf '%s' "$wrapped" | base64 -d)"
    jq --arg payload "$payload" '.payload=$payload | del(.sops)' "$input" ;;
  *) exit 2 ;;
esac
SH
chmod +x "$SUBYARD_KEYS_TOOLS_DIR/bin/age" "$SUBYARD_KEYS_TOOLS_DIR/bin/age-keygen" "$SUBYARD_KEYS_TOOLS_DIR/bin/sops"

cat > "$SUBYARD_CONFIG_HOME/yards/one.env" <<EOF
SSH_PORT=3221
SUBYARD_KEYS_ROOT=$SUBYARD_KEYS_ROOT/one
SUBYARD_KEYS_CONSUMER_ROOT=$TMP/consumer-one
EOF
cat > "$SUBYARD_CONFIG_HOME/yards/two.env" <<EOF
SSH_PORT=3222
SUBYARD_KEYS_ROOT=$SUBYARD_KEYS_ROOT/two
SUBYARD_KEYS_CONSUMER_ROOT=$TMP/consumer-two
EOF
cat > "$SUBYARD_CONFIG_HOME/yards/one_alt.env" <<EOF
SSH_PORT=3226
SUBYARD_KEYS_ROOT=$SUBYARD_KEYS_ROOT/one
SUBYARD_KEYS_CONSUMER_ROOT=$TMP/consumer-one-alt
EOF

yard_one() { "$ROOT/bin/yard" -Y one "$@"; }
yard_two() { "$ROOT/bin/yard" -Y two "$@"; }
yard_one_alt() { "$ROOT/bin/yard" -Y one_alt "$@"; }
yard_three() { "$ROOT/bin/yard" -Y three "$@"; }
yard_four() { "$ROOT/bin/yard" -Y four "$@"; }
bootstrap_keys() {
  "$ROOT/bin/yard" -Y "$1" _keys-init
}

bootstrap_keys one >/dev/null
initial_actor_one="$(jq -r '.actorId' "$SUBYARD_KEYS_ROOT/one/identity.json")"
bootstrap_keys two >/dev/null
bootstrap_keys one_alt >/dev/null
ASSUME_YES=1 "$ROOT/scripts/install-keys-auto-sync.sh" >/dev/null
grep -Fxq "ExecStart=$SUBYARD_HOME/runtime/current/bin/yard _keys-auto-sync --if-due" \
  "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service" \
  || fail 'credential timer did not prefer the release runtime'
[ -r "$SUBYARD_KEYS_ROOT/one/identity/age.txt" ] || fail 'yard one age identity missing'
[ "$(stat -c '%a' "$SUBYARD_KEYS_ROOT/one/identity/age.txt")" = 600 ] || fail 'age identity mode is not 0600'
[ "$(jq -r '.actorId' "$SUBYARD_KEYS_ROOT/one/identity.json")" = "$initial_actor_one" ] \
  || fail 'second local yard did not reuse the host identity'
[ ! -e "$SUBYARD_KEYS_ROOT/one/one_alt" ] || fail 'host ledger created a per-yard child store'
if yard_one_alt keys trust @one --yes >"$TMP/same-host-trust.out" 2>&1; then
  fail 'two contexts on the same host enrolled duplicate cryptographic trust'
fi
grep -Fq 'same key identity' "$TMP/same-host-trust.out" || fail 'same-host trust rejection was unclear'
grep -Fq 'Persistent=true' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.timer" || fail 'persistent timer missing'
grep -Fq 'OnUnitActiveSec=5h30min' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.timer" || fail 'timer interval drifted'
grep -Fq '_keys-auto-sync --if-due' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service" \
  || fail 'timer service does not synchronize the host ledger'
if grep -Fq -- '--all-contexts' "$SUBYARD_KEYS_SYSTEMD_DIR/subyard-keys-sync.service"; then
  fail 'timer still assumes one ledger per yard context'
fi

# Recover XDG_RUNTIME_DIR for a lingering user manager.
mkdir -m 0700 "$TMP/runtime"
mkdir -p "$TMP/bin"
cat > "$TMP/bin/loginctl" <<'SH'
#!/usr/bin/env bash
case "$*" in *'show-user'*'-p Linger --value'*) printf 'yes\n' ;; *) exit 2 ;; esac
SH
cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "${XDG_RUNTIME_DIR:-}" = "$EXPECTED_RUNTIME_DIR" ] || exit 80
[ -d "$EXPECTED_RUNTIME_DIR" ] || exit 79
printf '%s\n' "$*" >> "$SYSTEMCTL_LOG"
case "$*" in
  *'is-enabled'*) [ -e "$SYSTEMCTL_ENABLED" ] ;;
  *'enable --now'*) touch "$SYSTEMCTL_ENABLED" ;;
esac
SH
chmod +x "$TMP/bin/loginctl" "$TMP/bin/systemctl"
unset XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS
SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=0 \
SUBYARD_KEYS_RUNTIME_DIR="$TMP/runtime" \
EXPECTED_RUNTIME_DIR="$TMP/runtime" \
SYSTEMCTL_LOG="$TMP/systemctl.log" \
SYSTEMCTL_ENABLED="$TMP/systemctl-enabled" \
PATH="$TMP/bin:$PATH" \
ASSUME_YES=1 "$ROOT/scripts/install-keys-auto-sync.sh" >/dev/null
grep -Fxq -- '--user daemon-reload' "$TMP/systemctl.log" \
  || fail 'credential timer did not rediscover the user systemd runtime'
grep -Fxq -- '--user enable --now subyard-keys-sync.timer' "$TMP/systemctl.log" \
  || fail 'credential timer was not enabled through the rediscovered user systemd runtime'
SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=0 \
SUBYARD_KEYS_RUNTIME_DIR="$TMP/runtime" \
EXPECTED_RUNTIME_DIR="$TMP/runtime" \
SYSTEMCTL_LOG="$TMP/systemctl.log" \
SYSTEMCTL_ENABLED="$TMP/systemctl-enabled" \
PATH="$TMP/bin:$PATH" \
"$ROOT/scripts/install-keys-auto-sync.sh" --check \
  || fail 'credential timer convergence check did not rediscover the user systemd runtime'

cat > "$TMP/bin/sudo" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" != -- ] || shift
[ "$1 $2" = 'systemctl start' ] || exit 81
mkdir -m 0700 "$EXPECTED_RUNTIME_DIR"
printf '%s\n' "$*" >> "$SUDO_LOG"
SH
chmod +x "$TMP/bin/sudo"
find "$TMP/runtime" -depth -delete
unset XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS
SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=0 \
SUBYARD_KEYS_RUNTIME_DIR="$TMP/runtime" \
EXPECTED_RUNTIME_DIR="$TMP/runtime" \
SYSTEMCTL_LOG="$TMP/systemctl.log" \
SYSTEMCTL_ENABLED="$TMP/systemctl-enabled" \
SUDO_LOG="$TMP/sudo.log" \
PATH="$TMP/bin:$PATH" \
ASSUME_YES=1 "$ROOT/scripts/install-keys-auto-sync.sh" >/dev/null
grep -Eq '^systemctl start user@[0-9]+\.service$' "$TMP/sudo.log" \
  || fail 'credential timer did not start a missing lingering user manager'

if yard_one keys init --yes >"$TMP/removed-init.out" 2>&1; then fail 'removed keys init command still succeeds'; fi
grep -Eq 'unknown keys command.*init' "$TMP/removed-init.out" || fail 'removed keys init command has unclear error'

yard_one keys trust @two --yes >/dev/null
jq -e '.manualOnly == false and .trusted == true' "$SUBYARD_KEYS_ROOT/one/peers/two.json" >/dev/null \
  || fail 'trust did not enable automatic sync by default'
actor_one="$(jq -r '.actorId' "$SUBYARD_KEYS_ROOT/one/identity.json")"
actor_two="$(jq -r '.actorId' "$SUBYARD_KEYS_ROOT/two/identity.json")"
[[ "$actor_one" = host-* ]] || fail 'host identity actor is not host-scoped'
jq -e '.identityScope == "host"' "$SUBYARD_KEYS_ROOT/one/identity.json" >/dev/null \
  || fail 'identity scope is not explicit'
jq -e '.transport == "local" and .manualOnly == false and .trusted == true' \
  "$SUBYARD_KEYS_ROOT/one/peers/two.json" >/dev/null || fail 'trust initiator is not active+automatic'
jq -e '.transport == "inbound" and .manualOnly == true and .trusted == true' \
  "$SUBYARD_KEYS_ROOT/two/peers/one.json" >/dev/null || fail 'reciprocal trust side is not passive+respond-only'
grep -q "^$actor_two " "$SUBYARD_KEYS_ROOT/one/allowed_signers" \
  || fail 'initiator did not trust the reciprocal signing identity'
grep -q "^$actor_one " "$SUBYARD_KEYS_ROOT/two/allowed_signers" \
  || fail 'passive side did not trust the initiator signing identity'
cp "$SUBYARD_KEYS_ROOT/two/identity.json" "$TMP/two-identity.json"
jq '.signingPublic="ssh-ed25519 AAAAchanged"' "$TMP/two-identity.json" \
  > "$SUBYARD_KEYS_ROOT/two/identity.json"
chmod 0600 "$SUBYARD_KEYS_ROOT/two/identity.json"
if yard_one keys trust @two --yes >"$TMP/identity-rotation.out" 2>&1; then
  fail 'known peer silently rotated its signing identity'
fi
grep -Eiq 'identity|signing' "$TMP/identity-rotation.out" \
  || fail 'peer identity rotation rejection was unclear'
install -m 0600 "$TMP/two-identity.json" "$SUBYARD_KEYS_ROOT/two/identity.json"
yard_one keys status > "$TMP/active-status.out"
yard_two keys status > "$TMP/passive-status.out"
grep -Eq 'peer +two +role=active +policy=automatic' "$TMP/active-status.out" \
  || fail 'status did not expose the active automatic initiator'
grep -Eq 'peer +one +role=passive +policy=respond-only .*last-success=n/a' "$TMP/passive-status.out" \
  || fail 'status did not expose the passive respond-only side'
if yard_two keys auto-sync resume @one --yes >"$TMP/passive-resume.out" 2>&1; then
  fail 'passive peer enabled auto-sync without a reverse route'
fi
grep -Fq 'passive (respond-only)' "$TMP/passive-resume.out" \
  || fail 'passive auto-sync rejection was unclear'
yard_two keys sync --all --now --yes >/dev/null \
  || fail 'sync --all tried to initiate through a passive peer'
if grep -Fq 'stale' "$TMP/passive-status.out"; then fail 'passive peer was reported as a stale automatic initiator'; fi

# If the reciprocal side later learns its own route, it becomes active too. Its trust-import back
# to the first side must preserve that side's existing outbound route instead of demoting it. The
# passive source name may differ from the local registry alias; promotion deduplicates by actor.
jq '.name="controller-alias"' "$SUBYARD_KEYS_ROOT/two/peers/one.json" \
  > "$SUBYARD_KEYS_ROOT/two/peers/controller-alias.json"
chmod 0600 "$SUBYARD_KEYS_ROOT/two/peers/controller-alias.json"
rm -f "$SUBYARD_KEYS_ROOT/two/peers/one.json"
yard_two keys trust @one --yes >/dev/null
jq -e '.transport == "local" and .manualOnly == false' "$SUBYARD_KEYS_ROOT/two/peers/one.json" >/dev/null \
  || fail 'known reciprocal route did not promote the passive peer to active'
[ ! -e "$SUBYARD_KEYS_ROOT/two/peers/controller-alias.json" ] \
  || fail 'active route promotion left a duplicate actor alias'
jq -e '.transport == "local" and .manualOnly == false' "$SUBYARD_KEYS_ROOT/one/peers/two.json" >/dev/null \
  || fail 'reciprocal inbound refresh demoted an existing active route'

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

printf 'host-only-value' | yard_one keys add host-only --local-only --yes >/dev/null
local_id="$(yard_one keys list | awk -F '\t' '$8=="host-only" {print $1}')"
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
printf 'revive-base' | yard_one keys add revoke-race --yes >/dev/null
revoke_id="$(yard_one keys list | awk -F '\t' '$8=="revoke-race" {print $1}')"
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

# Offline failure is telemetry, not false success; restoring the peer converges without manual merge.
last_success="$(jq -r '.lastSuccess' "$SUBYARD_KEYS_ROOT/one/state/two.json")"
mv "$SUBYARD_KEYS_ROOT/two/shared.git" "$SUBYARD_KEYS_ROOT/two/shared.git.offline"
if yard_one keys sync @two --now --yes >"$TMP/offline.out" 2>&1; then fail 'offline peer reported successful sync'; fi
[ "$(jq -r '.lastSuccess' "$SUBYARD_KEYS_ROOT/one/state/two.json")" = "$last_success" ] \
  || fail 'failed sync overwrote last successful exchange'
jq -e '.error != "" and .consecutiveFailures > 0 and .nextRetry > .lastAttempt' \
  "$SUBYARD_KEYS_ROOT/one/state/two.json" >/dev/null || fail 'offline backoff telemetry is incomplete'
mv "$SUBYARD_KEYS_ROOT/two/shared.git.offline" "$SUBYARD_KEYS_ROOT/two/shared.git"
yard_one keys sync @two --now --yes >/dev/null
jq -e '.error == "" and .consecutiveFailures == 0 and .lastSuccess == .lastAttempt' \
  "$SUBYARD_KEYS_ROOT/one/state/two.json" >/dev/null || fail 'reconnect did not clear failure telemetry'

# The SSH/OWNER_YARD_NAME path is exercised host-free with a protocol-preserving ssh shim. Git still
# invokes upload-pack/receive-pack, while owner-host helpers compose `yard -Y four` over control SSH.
cat > "$SUBYARD_CONFIG_HOME/yards/three.env" <<EOF
SSH_PORT=3223
SUBYARD_KEYS_ROOT=$SUBYARD_KEYS_ROOT/three
SUBYARD_KEYS_CONSUMER_ROOT=$TMP/consumer-three
EOF
cat > "$SUBYARD_CONFIG_HOME/yards/four.env" <<EOF
SSH_PORT=3224
SUBYARD_KEYS_ROOT=$SUBYARD_KEYS_ROOT/four
SUBYARD_KEYS_CONSUMER_ROOT=$TMP/consumer-four
EOF
cat > "$SUBYARD_CONFIG_HOME/yards/srv4.env" <<'EOF'
ACCESS_KIND=remote
OWNER_ENDPOINT=fake-owner
OWNER_YARD_NAME=four
SSH_PORT=3225
EOF
cat > "$HOME/.bash_profile" <<EOF
export PATH="$ROOT/bin:\$PATH"
EOF
cat > "$TMP/fake-bin/ssh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%q ' "$@" >> "${SUBYARD_TEST_SSH_LOG:?}"
printf '\n' >> "$SUBYARD_TEST_SSH_LOG"
args=("$@")
i=0
while [ "$i" -lt "${#args[@]}" ]; do
  case "${args[$i]}" in
    -o|-p|-i|-F|-J) i=$((i + 2)) ;;
    -G) exit 1 ;;
    -*) i=$((i + 1)) ;;
    *) break ;;
  esac
done
i=$((i + 1)) # destination
[ "${args[$i]:-}" != -- ] || i=$((i + 1))
rest=("${args[@]:$i}")
[ "${#rest[@]}" -gt 0 ] || exit 0
printf -v remote_command '%s ' "${rest[@]}"
exec bash -c "$remote_command"
SH
chmod +x "$TMP/fake-bin/ssh"
export SUBYARD_TEST_SSH_LOG="$TMP/ssh.log"
bootstrap_keys three >/dev/null
bootstrap_keys four >/dev/null
yard_three keys trust @srv4 --yes >/dev/null
printf 'remote-wire-dummy' | yard_three keys add remote-static --yes >/dev/null
yard_three keys sync @srv4 --now --yes >/dev/null
yard_four keys list | grep -Fq remote-static || fail 'OWNER_YARD_NAME owner-host exchange did not converge'
grep -Fq 'ConnectTimeout=8' "$SUBYARD_TEST_SSH_LOG" || fail 'SSH/Git exchange omitted its bounded timeout'
grep -Eq 'Y.*four' "$SUBYARD_TEST_SSH_LOG" || fail 'remote helper omitted OWNER_YARD_NAME composition'

# A correctly signed append containing corrupt ciphertext still fails decrypt/MAC validation and is
# quarantined; trusting an actor never means trusting arbitrary record bytes from that actor.
actor_four="$(jq -r '.actorId' "$SUBYARD_KEYS_ROOT/four/identity.json")"
bad_remote_rev="$actor_four-000000900001-deadbeef"
bad_remote_dir="$SUBYARD_KEYS_ROOT/four/shared/records/cred-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
mkdir -p "$bad_remote_dir"
remote_source="$(find "$SUBYARD_KEYS_ROOT/four/shared/records" -name '*.json' -print -quit)"
jq --arg actor "$actor_four" --arg revision "$bad_remote_rev" \
  '.credentialId="cred-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" | .revisionId=$revision | .actorId=$actor |
   .actorCounter=900001 | .parents=[] | .label="corrupt dummy" | .payload="CORRUPT"' \
  "$remote_source" > "$bad_remote_dir/$bad_remote_rev.json"
ssh-keygen -Y sign -q -f "$SUBYARD_KEYS_ROOT/four/identity/signing_ed25519" -n subyard-keys \
  "$bad_remote_dir/$bad_remote_rev.json" >/dev/null
git -C "$SUBYARD_KEYS_ROOT/four/shared" add "records/cred-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
git -C "$SUBYARD_KEYS_ROOT/four/shared" -c user.name="$actor_four" -c user.email="$actor_four@subyard.invalid" \
  -c gpg.format=ssh -c user.signingkey="$SUBYARD_KEYS_ROOT/four/identity/signing_ed25519" \
  -c commit.gpgsign=true commit -S -m 'corrupt ciphertext fixture' >/dev/null
git -C "$SUBYARD_KEYS_ROOT/four/shared" push -q origin main
if yard_three keys sync @srv4 --now --yes >"$TMP/corrupt.out" 2>&1; then fail 'corrupt signed ciphertext was accepted'; fi
find "$SUBYARD_KEYS_ROOT/three/quarantine" -name "$bad_remote_rev.json" -print -quit | grep -q . \
  || fail 'corrupt signed ciphertext was not quarantined'

# Removing a recipient emits successors without that actor and removes reciprocal signing trust.
actor_two="$(jq -r '.actorId' "$SUBYARD_KEYS_ROOT/two/identity.json")"
yard_one keys untrust @two --yes >/dev/null
[ ! -e "$SUBYARD_KEYS_ROOT/one/peers/two.json" ] || fail 'recipient peer metadata survived untrust'
while IFS= read -r cred; do
  heads="$(jq -s '[.[] as $r | select((([.[].parents] | flatten | index($r.revisionId)) == null)) | $r]' \
    "$SUBYARD_KEYS_ROOT/one/shared/records/$cred"/*.json)"
  printf '%s' "$heads" | jq -e --arg actor "$actor_two" 'all(.[]; (.recipientActors | index($actor)) == null)' >/dev/null \
    || fail "current head still authorizes removed recipient for $cred"
done < <(find "$SUBYARD_KEYS_ROOT/one/shared/records" -mindepth 1 -maxdepth 1 -type d -printf '%f\n')
yard_one keys trust @two --yes >/dev/null

yard_one keys delete "$shared_id" --yes >/dev/null
[ ! -e "$TMP/consumer-one/staging/canonical.env" ] || fail 'tombstone kept the local consumer copy'
yard_one keys sync @two --now --yes >/dev/null
[ ! -e "$TMP/consumer-two/staging/canonical.env" ] || fail 'tombstone kept the peer consumer copy'
[ "$(yard_two keys list | awk -F '\t' -v id="$shared_id" '$1==id {print $4}')" = tombstone ] \
  || fail 'tombstone did not synchronize'

# Broad coding-agent OAuth paths are rejected before their contents are read.
mkdir -p "$TMP/.codex"
printf 'oauth-must-not-import' > "$TMP/.codex/auth.json"; chmod 0600 "$TMP/.codex/auth.json"
if yard_one keys import "$TMP/.codex/auth.json" --yes >"$TMP/oauth.out" 2>&1; then
  fail 'coding-agent OAuth store was imported'
fi
grep -Fq 'cannot be imported' "$TMP/oauth.out" || fail 'OAuth rejection was unclear'

# A pair of individually valid signed records with cyclic parents is also rejected and quarantined.
# Otherwise a malicious trusted actor could create a credential with no derived head.
actor_two="$(jq -r '.actorId' "$SUBYARD_KEYS_ROOT/two/identity.json")"
cycle_cred='cred-cccccccccccccccccccccccccccccccc'
cycle_rev_a="$actor_two-000000900001-c0ffee01"
cycle_rev_b="$actor_two-000000900002-c0ffee02"
cycle_dir="$SUBYARD_KEYS_ROOT/two/shared/records/$cycle_cred"
mkdir -p "$cycle_dir"
source_record="$(find "$SUBYARD_KEYS_ROOT/two/shared/records/$shared_id" -name '*.json' -print -quit)"
jq --arg cred "$cycle_cred" --arg actor "$actor_two" --arg revision "$cycle_rev_a" --arg parent "$cycle_rev_b" \
  '.credentialId=$cred | .revisionId=$revision | .actorId=$actor | .actorCounter=900001 |
   .parents=[$parent] | .label="cycle dummy A"' "$source_record" > "$cycle_dir/$cycle_rev_a.json"
jq --arg cred "$cycle_cred" --arg actor "$actor_two" --arg revision "$cycle_rev_b" --arg parent "$cycle_rev_a" \
  '.credentialId=$cred | .revisionId=$revision | .actorId=$actor | .actorCounter=900002 |
   .parents=[$parent] | .label="cycle dummy B"' "$source_record" > "$cycle_dir/$cycle_rev_b.json"
ssh-keygen -Y sign -q -f "$SUBYARD_KEYS_ROOT/two/identity/signing_ed25519" -n subyard-keys \
  "$cycle_dir/$cycle_rev_a.json" >/dev/null
ssh-keygen -Y sign -q -f "$SUBYARD_KEYS_ROOT/two/identity/signing_ed25519" -n subyard-keys \
  "$cycle_dir/$cycle_rev_b.json" >/dev/null
git -C "$SUBYARD_KEYS_ROOT/two/shared" add "records/$cycle_cred"
git -C "$SUBYARD_KEYS_ROOT/two/shared" -c user.name="$actor_two" -c user.email="$actor_two@subyard.invalid" \
  -c gpg.format=ssh -c user.signingkey="$SUBYARD_KEYS_ROOT/two/identity/signing_ed25519" \
  -c commit.gpgsign=true commit -S -m 'cyclic parent fixture' >/dev/null
git -C "$SUBYARD_KEYS_ROOT/two/shared" push -q origin main
if yard_one keys sync @two --now --yes >"$TMP/cycle.out" 2>&1; then fail 'cyclic remote revision graph was accepted'; fi
find "$SUBYARD_KEYS_ROOT/one/quarantine" -name "$cycle_rev_a.json" -print -quit | grep -q . \
  || fail 'cyclic revision graph was not quarantined'

# No failed/interrupted operation may leave plaintext in command output or the private temp root.
while IFS= read -r output_file; do
  for leaked in "$secret" "$blocked_prod" exclusive-bot-token remote-wire-dummy; do
    if grep -Fq -- "$leaked" "$output_file" 2>/dev/null; then fail "plaintext appeared in output file $output_file"; fi
  done
done < <(find "$TMP" -maxdepth 1 -type f -name '*.out' -print)
if find "$TMPDIR" -mindepth 1 -print -quit | grep -q .; then fail 'key operations left files in the private temp root'; fi

printf 'ok: encrypted credential ledgers sync, converge safely, isolate local-only data and quarantine tampering\n'

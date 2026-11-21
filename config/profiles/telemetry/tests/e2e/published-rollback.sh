# shellcheck shell=bash
# Rollback scenario sourced by dev/e2e/published-upgrade.sh on the candidate, before its rollback.
# Telemetry installs runtime format 2 and the published release reads format 1. The rollback
# refuses while telemetry is enabled; a first command converts the disabled installation.
# shellcheck disable=SC2154 # The core lane defines runtime, STATE_ROOT and the candidate target.
[ "${BASH_SOURCE[0]}" != "$0" ] || { printf 'published-rollback.sh: run through dev/e2e/published-upgrade.sh\n' >&2; exit 2; }
info 'preparing installed telemetry for the rollback'
operator_yard profile enable telemetry --yes >"$STATE_ROOT/telemetry-enable.log" 2>&1 \
  || { cat "$STATE_ROOT/telemetry-enable.log" >&2; die 'telemetry did not enable on the candidate'; }
status=0
operator_yard update --rollback --yes >"$STATE_ROOT/telemetry-rollback-refusal.log" 2>&1 || status=$?
[ "$status" != 0 ] && grep -Fq 'telemetry is enabled' "$STATE_ROOT/telemetry-rollback-refusal.log" \
  && grep -Fq 'profile disable telemetry' "$STATE_ROOT/telemetry-rollback-refusal.log" \
  && [ "$(operator_env readlink "$runtime/current")" = "$CANDIDATE_RELEASE_TARGET" ] \
  || { cat "$STATE_ROOT/telemetry-rollback-refusal.log" >&2; die "rollback did not refuse enabled telemetry (exit=$status)"; }
operator_yard profile disable telemetry --yes >"$STATE_ROOT/telemetry-disable.log" 2>&1 \
  || { cat "$STATE_ROOT/telemetry-disable.log" >&2; die 'telemetry did not disable on the candidate'; }
guest test -f /etc/systemd/system/subyard-telemetry.service || die 'disabled telemetry left no collector unit'
status=0
operator_yard update --rollback --yes >"$STATE_ROOT/telemetry-rollback-prepare.log" 2>&1 || status=$?
[ "$status" = 0 ] && grep -Fq 'Profile runtimes prepared for rollback' "$STATE_ROOT/telemetry-rollback-prepare.log" \
  && grep -Fq 'Next: run yard update --rollback' "$STATE_ROOT/telemetry-rollback-prepare.log" \
  && [ "$(operator_env readlink "$runtime/current")" = "$CANDIDATE_RELEASE_TARGET" ] \
  && guest grep -Fq -- '--auth-file /var/lib/subyard/telemetry/auth.token' /etc/systemd/system/subyard-telemetry.service \
  && guest test -f /var/lib/subyard/telemetry/auth.token \
  && ! guest test -e /var/lib/subyard/telemetry-private/auth.token \
  || { cat "$STATE_ROOT/telemetry-rollback-prepare.log" >&2; die "rollback did not prepare telemetry (exit=$status)"; }
ok 'rollback refused enabled telemetry and prepared the disabled installation'

#!/usr/bin/env bash

# A changed init assessment is rejected before mutation. Real hosts can reach
# network convergence between the initial assessment and its pre-apply refresh,
# so the P0 harness may submit a fresh plan for that one exact safe rejection.
p0_retry_init_after_plan_stale() {
  local max_attempts="${P0_INIT_STALE_RETRY_ATTEMPTS:-3}"
  local delay_seconds="${P0_INIT_STALE_RETRY_DELAY_SECONDS:-1}"
  local attempt=1 command_rc tee_rc log errexit=0
  local -a statuses
  local stale_message='yard: init: operation plan is stale: action consequences changed after confirmation'

  [ "$#" -gt 0 ] || return 2
  [[ "$max_attempts" =~ ^[1-9][0-9]*$ ]] && [ "$max_attempts" -le 10 ] || return 2
  case "$delay_seconds" in
    [0-9]|[1-5][0-9]|60) ;;
    *) return 2 ;;
  esac
  log="$(mktemp "${TMPDIR:-/tmp}/subyard-p0-init-retry.XXXXXX")" || return 2
  case "$-" in *e*) errexit=1 ;; esac

  while [ "$attempt" -le "$max_attempts" ]; do
    : > "$log"
    set +e
    "$@" 2>&1 | tee "$log"
    statuses=("${PIPESTATUS[@]}")
    command_rc="${statuses[0]}"
    tee_rc="${statuses[1]}"
    [ "$errexit" = 0 ] || set -e
    if [ "$tee_rc" != 0 ]; then
      find "$log" -delete
      return "$tee_rc"
    fi
    if [ "$command_rc" = 0 ]; then
      find "$log" -delete
      return 0
    fi
    if [ "$command_rc" != 1 ] || ! grep -Fqx "$stale_message" "$log" \
      || [ "$attempt" -ge "$max_attempts" ]; then
      find "$log" -delete
      return "$command_rc"
    fi
    attempt=$((attempt + 1))
    printf '  [warn] init assessment changed before apply; retrying with a fresh plan (%s/%s)\n' \
      "$attempt" "$max_attempts"
    [ "$delay_seconds" = 0 ] || sleep "$delay_seconds"
  done
}

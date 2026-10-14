# shellcheck shell=bash
# One ordinary cold enable; a same-guest fallback is diagnostic, never acceptance.
openlit_pull_observe() {
  local phase="$1" rc=0 network_rc=0
  printf 'openlit_e2e_pull_observation=%s timing=before-or-after-command\n' "$phase"
  incus exec "$pull_instance" --project "$pull_project" -- python3 - context \
    "${pull_images[@]}" "$phase" < "$pull_root/config/profiles/openlit/tests/e2e/failure-facts.py" || rc=$?
  timeout --signal=TERM --kill-after=5 175 bash \
    "$pull_root/config/profiles/openlit/tests/e2e/network-only.sh" \
    "$pull_root" "$pull_state" "$pull_instance" "$pull_project" || network_rc=$?
  [ "$rc" -ne 0 ] || rc=$network_rc
  return "$rc"
}

openlit_pull_diagnostic() {
  [ "$#" = 7 ] || return 2
  local pull_root="$1" pull_state="$2" pull_instance="$3" pull_project="$4"
  local -a pull_images=("${@:5}")
  local ordinary_rc=0 observation_rc=0 fallback_rc=0 compose_payload
  openlit_pull_observe before || return 2
  yard profile enable openlit --yes || ordinary_rc=$?
  printf 'openlit_e2e_ordinary_enable_exit=%s\n' "$ordinary_rc"
  openlit_pull_observe after-ordinary || observation_rc=$?
  if [ "$ordinary_rc" -ne 0 ]; then
    printf 'openlit_e2e_pull_fallback=CACHE_REUSED quiet-causality=unknown cold-acceptance=not-passed\n'
    compose_payload="$(base64 -w0 "$pull_root/config/profiles/openlit/compose.yaml")" || return "$ordinary_rc"
    incus exec "$pull_instance" --project "$pull_project" -- python3 - dependency \
      "${pull_images[@]}" "$compose_payload" \
      < "$pull_root/config/profiles/openlit/tests/e2e/failure-facts.py" || fallback_rc=$?
    printf 'openlit_e2e_fallback_exit=%s original-enable-exit=%s\n' "$fallback_rc" "$ordinary_rc"
    openlit_pull_observe after-fallback || :
    return "$ordinary_rc"
  fi
  [ "$observation_rc" -eq 0 ] || return 2
  printf 'ok: ordinary cold OpenLIT enable ready; native-events-and-rendered-UI=not-tested\n'
}

# shellcheck shell=bash
# OpenClaw-only native resource consent projection; execution stays in each handler.
openclaw_file_fact() {
  local path="$1"
  if [ -L "$path" ]; then die "plan_stale: resource input is a symlink"; fi
  if [ -f "$path" ]; then
    stat -c '%f:%u:%g:%s' -- "$path"
    sha256sum -- "$path" | awk '{print $1}'
  elif [ -e "$path" ]; then die "plan_stale: resource input is not a regular file"
  else printf 'absent\n'; fi
}

openclaw_emit_exact() {
  local action="$1" changed="$2" target desired observed decision binding consequence
  shift 2
  target="$(openclaw_target "$action")"
  binding="$(openclaw_binding "$action")"
  desired="converged $action on $target"
  case "$action" in
    destroy-purge) consequence="irreversibly delete the prepared persistent resource data and remove its runtime and staged inputs" ;;
    destroy) consequence="remove the prepared resource runtime and staged inputs while retaining persistent data" ;;
    down) consequence="stop the prepared resource runtime while retaining persistent data" ;;
    stop) consequence="stop the prepared gateway and release only its owned bot lease" ;;
    *) consequence="converge the prepared OpenClaw $action resource within its approved native scope" ;;
  esac
  decision=apply
  observed="native state digest $(openclaw_observation "$action")"
  if [ "$changed" = false ]; then decision=skip; observed="$desired"; fi
  jq -cn --arg action "$action" --argjson changed "$changed" --arg binding "$binding" \
    --arg consequence "$consequence" --arg target "$target" --arg desired "$desired" --arg observed "$observed" --arg decision "$decision" \
    --args '{schema:"yard.resource-action-assessment.v2",action:$action,changed:$changed,consequences:$ARGS.positional,binding:$binding,
      steps:[{id:"openclaw.native",target:$target,observed:$observed,desired:$desired,decision:$decision,
        preconditions:["unchanged native input fingerprints, resource identity and bounded paths"],
        verify:"native read-only postcondition observation",consequence:$consequence}]}' "$@"
}

openclaw_recheck_exact() {
  local action="$1" fresh
  # Dedicated native callers without an outer prepared operation retain their
  # existing typed action gate. Exact operations always provide both fields.
  [ -n "${SUBYARD_RESOURCE_STEPS:-}" ] || return 0
  fresh="$(openclaw_prepare_fresh)"
  [ "$(jq -r .action <<<"$fresh")" = "$action" ] && \
    [ "$(jq -r .binding <<<"$fresh")" = "${SUBYARD_RESOURCE_BINDING:-}" ] \
    || die "plan_stale: OpenClaw native input or target changed"
  jq -en --argjson approved "$SUBYARD_RESOURCE_STEPS" --argjson current "$(jq -c .steps <<<"$fresh")" '
    ($approved|length)==($current|length) and all(range(0;$approved|length); . as $i |
      $approved[$i] as $a | $current[$i] as $c |
      $a.id==$c.id and $a.target==$c.target and $a.desired==$c.desired and
      ($c.decision=="skip" or ($a.decision=="apply" and $a.observed==$c.observed)))' >/dev/null \
    || die "plan_stale: OpenClaw native observed work changed"
  [ "$(jq -r .changed <<<"$fresh")" = true ] || exit 0
}

openclaw_lock() {
  local key="$1" directory="$SUBYARD_CONFIG_HOST_DIR/.openclaw-resource-locks"
  [ ! -L "$directory" ] || die "unsafe OpenClaw resource lock directory"
  mkdir -p "$directory"
  chmod 0700 "$directory"
  [ ! -L "$directory/$key.lock" ] || die "unsafe OpenClaw resource lock"
  (umask 077; touch "$directory/$key.lock")
  chmod 0600 "$directory/$key.lock"
  exec 8>"$directory/$key.lock"
  flock 8
}

openclaw_recheck_inputs() {
  [ -n "${SUBYARD_RESOURCE_BINDING:-}" ] || return 0
  [ "$(openclaw_binding "$1")" = "$SUBYARD_RESOURCE_BINDING" ] || die "plan_stale: OpenClaw prepared input changed before write"
}

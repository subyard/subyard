#!/usr/bin/env bash
# Existing profile lifecycle transports the bounded package into a running yard.
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$profile/../../.." && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$root/scripts/lib/engine-context.sh"
subyard_require_engine_context
export SUBYARD_REPOSITORY_ROOT="$root"
mode=apply
selected=${SUBYARD_PROFILE_SELECTED:-0}
runtime_args=()
if [ "${1-}" = _runtime-contract ]; then
  selected=0
  [[ " ${ENVIRONMENT_PROFILES:-} " != *" openlit "* ]] || selected=1
  shift
  case "${1-}" in
    observe) [ "$#" = 1 ] || exit 2; mode=observe ;;
    apply)
      [ "$#" = 4 ] && [[ "$2" =~ ^[a-zA-Z0-9_-]+$ ]] && [[ "$3" =~ ^[a-f0-9]{64}$ ]] && [[ "$4" =~ ^[a-f0-9]{64}$ ]] || exit 2
      mode=refresh
      runtime_args=("$3" "$4")
      ;;
    *) exit 2 ;;
  esac
else
case "${1-}" in
  --yes) ;;
  --check) mode=check ;;
  --remove) selected=0 ;;
  --pause) mode=pause ;;
  --resume) mode=resume ;;
  *) exit 2 ;;
esac
[ "$#" = 1 ] || exit 2
fi
state="$(incus list "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -f csv -c s)"
case "$state" in
  RUNNING) ;;
  ''|STOPPED)
    if [ "$mode" = observe ]; then
      [ -n "$state" ] && printf '{"state":"deferred","actual":"","desired":""}\n' || printf '{"state":"absent","actual":"","desired":""}\n'
    fi
    exit 0
    ;;
  *) echo 'openlit: yard is not ready for profile reconciliation' >&2; exit 1 ;;
esac
agents=''
for agent in claude codex; do
  if [[ " ${CODING_TOOL_INTEGRATIONS:-} " = *" $agent "* ]]; then
    agents="${agents:+$agents }$agent"
    if [ "$selected" = 1 ] && { [ "$mode" = apply ] || [ "$mode" = check ] || [ "$mode" = observe ] || [ "$mode" = refresh ]; }; then
      destination="AGENT_${agent}_CONFIG_DEST"
      expected=".claude/settings.json"
      [ "$agent" != codex ] || expected=".codex/config.toml"
      [ "${!destination}" = "$expected" ] || { echo 'openlit: native telemetry requires the supported home configuration destination' >&2; exit 1; }
      name="AGENT_${agent}_CONFIG"
      python3 "$profile/native.py" template-check --agent "$agent" --template "${!name}" --source-root "$root" || exit 1
    fi
  fi
done
# Route and guest observations share one assessed activation binding.
exec python3 "$profile/dashboard.py" "$mode" "$selected" "$DEV_USER" "$agents" "${runtime_args[@]}"

#!/usr/bin/env bash
# Bounded integration package and project-hook adapter. Core substrate is a precondition.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/runtime.sh
. "$SCRIPT_DIR/lib/runtime.sh"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/ai-observer-proxy.sh
. "$SCRIPT_DIR/lib/ai-observer-proxy.sh"
PROJ=(--project "$INCUS_PROJECT")
[ "$(incus list "$YARD_INSTANCE_NAME" "${PROJ[@]}" --format json | jq -r '.[0].status')" = Running ] \
  || die "integration reconcile requires a running yard"
[ "${ALLOWS_CODING_TOOLS:-true}" != false ] || [ -z "${CODING_TOOL_INTEGRATIONS:-}" ] \
  || die "coding integrations are forbidden by the yard role"
# The special role retires the owned reporter in Go before clearing its old marker.
if [ "${ALLOWS_CODING_TOOLS:-true}" = false ]; then
  _ccusage_marker="$(incus config get "$YARD_INSTANCE_NAME" user.subyard.ccusage_version "${PROJ[@]}")"
  [ -z "$_ccusage_marker" ] || incus config unset "$YARD_INSTANCE_NAME" user.subyard.ccusage_version "${PROJ[@]}"
  unset _ccusage_marker
fi

# Explicit operator links stay outside integration ownership and survive selection changes.
if [ -z "${INTEGRATION_HOST_LINKS:-}" ] && [ -n "${HOST_LINKS:-}" ]; then
  incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" --env DEV_USER="$DEV_USER" \
    --env HOST_LINKS="$HOST_LINKS" -- bash -euo pipefail -s <<'EOS'
home="$(getent passwd "$DEV_USER" | cut -d: -f6)"
for entry in $HOST_LINKS; do
  IFS=: read -r name target kind <<<"$entry"
  case "$name" in ''|/*|..|../*|*/../*) exit 1 ;; esac
  case "$target" in /*) ;; *) exit 1 ;; esac
  mount="/$(printf '%s' "$target" | cut -d/ -f2-4)"
  [ -d "$mount" ] || continue
  link="$home/$name"
  [ ! -e "$link" ] && [ ! -L "$link" ] || continue
  if [ "${kind:-}" = file ]; then directory="$(dirname "$target")"; else directory="$target"; fi
  runuser -u "$DEV_USER" -- mkdir -p "$directory" "$(dirname "$link")"
  runuser -u "$DEV_USER" -- ln -s "$target" "$link"
done
EOS
fi

# --- 3. provision enabled agent CLIs ----------------------------------------
# Hooks live under config/agents/<name> and run as root in the yard.
echo "Agent CLIs:"
_aiobserver_selected=0
for _agent in ${CODING_TOOL_INTEGRATIONS:-}; do
  [ "$_agent" != aiobserver ] || _aiobserver_selected=1
  _provision_var="AGENT_${_agent}_PROVISION"
  _provision="${!_provision_var:-}"
  [ -n "$_provision" ] || continue
  [ -r "$_provision" ] || die "$_agent provision hook missing: $_provision"
  if [ "$_agent" = aiobserver ]; then
    [[ "${AI_OBSERVER_CONTEXT:-}" =~ ^[0-9a-f]{64}$ ]] \
      || die "prepared AI Observer context is required"
    incus config set "$YARD_INSTANCE_NAME" user.subyard.ai_observer_provision pending "${PROJ[@]}" \
      || die "could not invalidate AI Observer convergence"
  fi
  info "provisioning $_agent CLI in $YARD_INSTANCE_NAME"
  _agent_env=(
    --env DEV_USER="$DEV_USER"
    --env CODING_TOOL_INTEGRATIONS="${CODING_TOOL_INTEGRATIONS:-}"
    --env AI_OBSERVER_CONTEXT="${AI_OBSERVER_CONTEXT:-}"
    --env YARD_VERSION="${YARD_VERSION:-}"
  )
  incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" "${_agent_env[@]}" \
    -- bash -euo pipefail -s < "$_provision" \
    || die "$_agent CLI provisioning failed"
  _check_var="AGENT_${_agent}_CHECK"
  _check="${!_check_var:-}"
  if [ -n "$_check" ]; then
    case "$_check" in *[!A-Za-z0-9._/-]*|'') die "$_agent check command is invalid" ;; esac
    incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- timeout 90 "$_check" \
      || die "$_agent package check failed"
  fi
  ok "$_agent CLI ready"
done
unset _agent _agent_env _check_var _check _provision_var _provision

# Stop a previously managed observer when it is removed from the exact agent list.
if [ "$_aiobserver_selected" = 0 ]; then
  incus exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- sh -eu -c '
    if [ -f /etc/subyard/ai-observer/managed ]; then
      /usr/local/bin/ai-observer disable
    fi
  ' || die "could not disable the deselected AI Observer"
fi
subyard_ai_observer_proxy "$_aiobserver_selected" || die "AI Observer dashboard route did not converge"
subyard_ai_observer_provision_marker \
  "$_aiobserver_selected" "${AI_OBSERVER_CONTEXT:-}" \
  || die "could not update AI Observer convergence marker"
unset _aiobserver_selected

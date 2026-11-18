#!/usr/bin/env bash
# Bounded integration package and project-hook adapter. Core substrate is a precondition.
set -euo pipefail
# Activation consumes only the package ID written by this script. Keep all
# command output private and reserve fd 3 for the fixed failure record.
activation_diagnostics=false
if [ "${1:-}" = --activation-diagnostics ]; then
  activation_diagnostics=true
  exec 3>&1 >/dev/null 2>&1
fi
package_failure() {
  if [ "$activation_diagnostics" = true ]; then
    printf '%s\n' "$_agent" >&3
  fi
  die "$1"
}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/runtime.sh
. "$SCRIPT_DIR/lib/runtime.sh"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/native-installed-stage.sh
. "$SCRIPT_DIR/lib/native-installed-stage.sh"
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib-power.sh
. "$SCRIPT_DIR/lib-power.sh"
PROJ=(--project "$INCUS_PROJECT")
[ "$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" 3>&-)" = RUNNING ] \
  || die "integration reconcile requires a running yard"
[ "${ALLOWS_CODING_TOOLS:-true}" != false ] || [ -z "${CODING_TOOL_INTEGRATIONS:-}" ] \
  || die "coding integrations are forbidden by the yard role"
# The special role retires the owned reporter in Go before clearing its old marker.
if [ "${ALLOWS_CODING_TOOLS:-true}" = false ]; then
  _ccusage_marker="$(incus 3>&- config get "$YARD_INSTANCE_NAME" user.subyard.ccusage_version "${PROJ[@]}")"
  [ -z "$_ccusage_marker" ] || incus 3>&- config unset "$YARD_INSTANCE_NAME" user.subyard.ccusage_version "${PROJ[@]}"
  unset _ccusage_marker
fi

# Explicit operator links stay outside integration ownership and survive selection changes.
if [ -z "${INTEGRATION_HOST_LINKS:-}" ] && [ -n "${HOST_LINKS:-}" ]; then
  incus 3>&- exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" --env DEV_USER="$DEV_USER" \
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
# Resolve the Git approval default here so installed prompt package hashes stay valid.
echo "Coding integrations:"
for _agent in ${CODING_TOOL_INTEGRATIONS:-}; do
  _provision_var="AGENT_${_agent}_PROVISION"
  _provision="${!_provision_var:-}"
  [ -n "$_provision" ] || continue
  [ -r "$_provision" ] || package_failure "$_agent provision hook missing: $_provision"
  info "provisioning $_agent integration in $YARD_INSTANCE_NAME"
  _agent_env=(
    --env DEV_USER="$DEV_USER"
    --env CODING_TOOL_INTEGRATIONS="${CODING_TOOL_INTEGRATIONS:-}"
    --env CODING_TOOL_GIT_APPROVAL="${CODING_TOOL_GIT_APPROVAL:-off}"
    --env YARD_VERSION="${YARD_VERSION:-}"
  )
  incus 3>&- exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" "${_agent_env[@]}" \
    -- bash -euo pipefail -s < "$_provision" \
    || package_failure "$_agent CLI provisioning failed"
  _check_var="AGENT_${_agent}_CHECK"
  _check="${!_check_var:-}"
  if [ -n "$_check" ]; then
    case "$_check" in *[!A-Za-z0-9._/-]*|'') package_failure "$_agent check command is invalid" ;; esac
    incus 3>&- exec "$YARD_INSTANCE_NAME" "${PROJ[@]}" -- timeout 90 "$_check" \
      || package_failure "$_agent package check failed"
  fi
  _health_var="AGENT_${_agent}_HEALTH"
  if [ -n "${!_health_var:-}" ]; then
    ok "$_agent package installed; service health is reported by yard status"
  else
    ok "$_agent integration ready"
  fi
done
unset _agent _agent_env _check_var _check _health_var _provision_var _provision

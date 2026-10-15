# Exact producer capture and drain shared by lifecycle and both teardown scopes.
# shellcheck shell=bash
teardown_capture_producer() {
  local guard="$SCRIPT_DIR/lib/teardown-plan.py"
  SUBYARD_TEARDOWN_SERVICES="$(python3 -B "$guard" capture-producer "$1" "${2:-0}" "$SCRIPT_DIR/..")" || return
  export SUBYARD_TEARDOWN_SERVICES
  SUBYARD_TEARDOWN_ARTIFACTS="$(python3 -B "$guard" producer-artifacts-json)" || return
  export SUBYARD_TEARDOWN_ARTIFACTS
}

teardown_stop_services() {
require_root "removing the approved host producers requires root"
guard="$SCRIPT_DIR/lib/teardown-plan.py"
python3 "$guard" guard-artifacts || exit 75

service_inventory="$(python3 "$guard" service-inventory)"

operator="${SUBYARD_USER:-${SUDO_USER:-${USER:-root}}}"
operator_uid="$(id -u "$operator")"
user_runtime="/run/user/$operator_uid"
control() {
  local scope="$1"; shift
  if [ "$scope" = user ]; then
    sudo -n -u "$operator" -- env "XDG_RUNTIME_DIR=$user_runtime" \
      "DBUS_SESSION_BUS_ADDRESS=unix:path=$user_runtime/bus" systemctl --user "$@"
  else
    systemctl "$@"
  fi
}

while IFS=$'\t' read -r scope unit; do
  [ -n "$unit" ] || continue
  load_state="$(control "$scope" show "$unit" --property=LoadState --value)"
  [ "$load_state" != not-found ] || continue
  fragment="$(control "$scope" show "$unit" --property=FragmentPath --value)"
  python3 "$guard" service-fragment "$fragment" \
    || die "approved unit name now resolves to an unapproved service artifact"
done <<< "$service_inventory"

# Disable triggering units first. The following stop is synchronous, including
# an already-running oneshot and its cgroup; never delete a producer's data first.
while IFS=$'\t' read -r scope unit; do
  [ -n "$unit" ] || continue
  [ "$(control "$scope" show "$unit" --property=LoadState --value)" != not-found ] || continue
  case "$unit" in
    *.timer|*.socket) control "$scope" disable --now "$unit" ;;
  esac
done <<< "$service_inventory"
while IFS=$'\t' read -r scope unit; do
  [ -n "$unit" ] || continue
  [ "$(control "$scope" show "$unit" --property=LoadState --value)" != not-found ] || continue
  control "$scope" stop "$unit" || die "could not stop approved producer $unit"
  state="$(control "$scope" show "$unit" --property=ActiveState --value)"
  case "$state" in inactive|failed) ;; *) die "approved producer is still active: $unit" ;; esac
  for property in MainPID ControlPID; do
    pid="$(control "$scope" show "$unit" --property="$property" --value)"
    [ -z "$pid" ] || [ "$pid" = 0 ] || die "approved producer still has a process: $unit"
  done
done <<< "$service_inventory"

# A producer may have written another incident while its stop was draining.
# Recheck private ownership before removing even its unit/helper artifacts.
[ -z "${SUBYARD_TEARDOWN_PLAN:-}" ] || python3 -B "$SCRIPT_DIR/lib/teardown-profile-checks.py"

while IFS= read -r artifact; do
  [ -n "$artifact" ] || continue
  python3 "$guard" remove-artifact "$artifact" || exit 75
done < <(python3 "$guard" producer-artifacts)
systemctl daemon-reload
if [[ "$service_inventory" == *$'user\t'* ]]; then
  control user daemon-reload
fi

}

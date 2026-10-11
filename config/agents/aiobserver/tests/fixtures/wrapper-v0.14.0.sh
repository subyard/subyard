#!/usr/bin/env bash
# Generated dispatch/control proof from checksum-verified published v0.14.0.
# Only provision-time paths/identity are adapted for the disposable fixture.
# shellcheck disable=SC2034,SC2015 # Preserve the exact legacy generated idioms/metadata.
# Managed by Subyard AI Observer provision.
set -euo pipefail
image='owned-image'
version='0.5.0'
container='subyard-ai-observer'
unit='subyard-ai-observer.service'
owner_label='ai-observer-v1'
managed_marker="${FIXTURE_ROOT:?}/managed"
unit_path="$FIXTURE_ROOT/unit"
check_path="$FIXTURE_ROOT/check"
expected_file_owner="$(id -u):$(id -g)"
test_mode=1
die() { printf 'ai-observer: %s\n' "$*" >&2; exit 1; }
container_value() { timeout 10 docker inspect -f "$1" "$container" 2>/dev/null; }
container_exists() { timeout 10 docker container inspect "$container" >/dev/null 2>&1; }
container_owned() {
  [ "$(container_value '{{ index .Config.Labels "org.subyard.managed" }}')" = "$owner_label" ]
}
verify_control_files() {
  [ -f "${BASH_SOURCE[0]}" ] && [ ! -L "${BASH_SOURCE[0]}" ] \
    || die 'managed wrapper is not a regular file'
  [ "$(stat -c '%u:%g' "${BASH_SOURCE[0]}")" = "$expected_file_owner" ] \
    || die 'managed wrapper ownership drifted'
  grep -Fxq '# Managed by Subyard AI Observer provision.' "${BASH_SOURCE[0]}" || die 'managed wrapper marker is missing'
  [ -f "$managed_marker" ] && [ ! -L "$managed_marker" ] \
    || die 'managed state marker is missing'
  [ "$(stat -c '%u:%g' "$managed_marker")" = "$expected_file_owner" ] \
    || die 'managed state marker ownership drifted'
  grep -Fxq 'subyard-ai-observer-v1' "$managed_marker" || die 'managed state marker drifted'
  [ -f "$unit_path" ] && [ ! -L "$unit_path" ] \
    || die 'managed unit is missing'
  [ "$(stat -c '%u:%g' "$unit_path")" = "$expected_file_owner" ] \
    || die 'managed unit ownership drifted'
  grep -Fxq '# Managed by Subyard AI Observer provision.' "$unit_path" || die 'managed unit marker is missing'
  [ -f "$check_path" ] && [ ! -L "$check_path" ] \
    || die 'managed readiness check is missing'
  [ "$(stat -c '%u:%g' "$check_path")" = "$expected_file_owner" ] \
    || die 'managed readiness check ownership drifted'
  grep -Fxq '# Managed by Subyard AI Observer provision.' "$check_path" || die 'managed readiness check marker is missing'
}
verify_owned_container() {
  container_exists || die 'managed container is missing'
  container_owned || die 'same-name container is not managed by Subyard'
}
case "${1:-}" in
  run)
    verify_control_files
    verify_owned_container
    exec docker start --attach "$container"
    ;;
  stop)
    verify_control_files
    verify_owned_container
    [ "$(container_value '{{.State.Running}}')" != true ] \
      || timeout 20 docker stop --time 10 "$container" >/dev/null
    ;;
  disable)
    [ "$(id -u)" -eq 0 ] || [ "$test_mode" = 1 ] || die 'disable must run as root'
    verify_control_files
    if container_exists; then container_owned || die 'refusing to disable a foreign same-name container'; fi
    timeout 30 systemctl disable --now "$unit"
    ;;
  status)
    verify_control_files
    verify_owned_container
    "$check_path"
    printf 'dashboard http://127.0.0.1:8080/\n'
    ;;
  logs)
    shift
    verify_control_files
    verify_owned_container
    exec docker logs "$@" "$container"
    ;;
  --version)
    verify_control_files
    verify_owned_container
    if [ "$(container_value '{{.State.Running}}')" = true ]; then
      exec timeout 10 docker exec "$container" /app/ai-observer --version
    fi
    printf 'ai-observer %s\n' "$version"
    ;;
  *)
    printf 'Usage: ai-observer {status|logs|disable|--version}\n' >&2
    exit 2
    ;;
esac

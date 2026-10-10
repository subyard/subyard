#!/usr/bin/env bash
# Run one Go-selected profile hook inside the yard.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
mode=apply
if [ "${1:-}" = --check ]; then
  mode=check
  shift
fi
profile="${1:-}"
shift || true
case "$profile" in '' | *[!a-zA-Z0-9_-]*) printf 'provision: invalid profile\n' >&2; exit 2 ;; esac
[ "$#" -eq 0 ] || { printf 'provision: unexpected argument\n' >&2; exit 2; }
root="$(cd "$SCRIPT_DIR/.." && pwd)"
config="$root/config/profiles/$profile/profile.conf"
hook="$root/config/profiles/$profile/provision.sh"
[ -r "$config" ] && [ -r "$hook" ] || { printf 'provision: profile hook missing\n' >&2; exit 1; }
grep -Fxq '# subyard-provision-check-v1' "$hook" \
  || { printf 'provision: profile hook does not support check protocol\n' >&2; exit 1; }
bundle="$(dirname "$hook")"
error_file="$(mktemp)"
trap 'rm -f -- "$error_file"' EXIT

env_args=(--env DEV_USER="${DEV_USER:-dev}" --env EXCLUSIVE_ENVIRONMENT_PROFILE="${EXCLUSIVE_ENVIRONMENT_PROFILE:-}" --env YARD_KIND="${YARD_KIND:-container}")
# shellcheck disable=SC1090
. "$config"
while IFS= read -r name; do
  [ -z "$name" ] || env_args+=(--env "$name=${!name-}")
done < <(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$config" | sed 's/=$//' | sort -u)
set +e
tar -C "$bundle" -cf - . | incus exec "${YARD_INSTANCE_NAME:?}" --project "${INCUS_PROJECT:?}" \
  "${env_args[@]}" -- bash -euo pipefail -c '
    owner_stat=$(<"/proc/$$/stat")
    owner_stat=${owner_stat##*) }
    read -r -a owner_fields <<< "$owner_stat"
    owner_start=${owner_fields[19]}
    profile_dir="$(mktemp -d /tmp/subyard-profile.XXXXXX)"
    trap '\''rm -rf -- "$profile_dir"'\'' EXIT
    tar -xf - -C "$profile_dir"
    subyard_supervise_profile() {
      owner_pid=$1 owner_start=$2 profile_dir=$3 mode=$4
      # Incus disconnect kills only its attached PID. Keep the hook and its
      # ordinary descendants in a separate group with an operation-local watcher.
      set +m
      group_stat=$(<"/proc/$$/stat")
      group_stat=${group_stat##*) }
      read -r -a group_fields <<< "$group_stat"
      [ "${group_fields[2]}" = "$$" ] || return 1
      mkfifo "$profile_dir/.subyard-cancel-poll"
      exec {poll_fd}<>"$profile_dir/.subyard-cancel-poll"
      rm -- "$profile_dir/.subyard-cancel-poll"
      (
        while :; do
          owner_stat=""
          if [ -r "/proc/$owner_pid/stat" ]; then
            owner_stat=$(<"/proc/$owner_pid/stat") || :
          fi
          owner_stat=${owner_stat##*) }
          read -r -a owner_fields <<< "$owner_stat"
          if [ "${owner_fields[19]-}" != "$owner_start" ] || [ "${owner_fields[0]-}" = Z ]; then
            rm -rf -- "$profile_dir"
            # This watcher remains in the group, preventing PGID reuse. In a
            # Bash subshell $$ still names the supervisor, the group leader.
            kill -KILL -- "-$$"
            exit 130
          fi
          # A builtin timed read avoids leaving a sleep process on completion.
          if read -r -t 0.1 -u "$poll_fd"; then exit 0; fi
        done
      ) &
      watcher=$!
      trap '\''status=$?; printf "stop\n" >&"$poll_fd"; wait "$watcher" || :; rm -rf -- "$profile_dir"; exit "$status"'\'' EXIT
      if [ "$mode" = check ]; then
        bash "$profile_dir/provision.sh" --check {poll_fd}>&- >/dev/null
      else
        bash "$profile_dir/provision.sh" {poll_fd}>&-
      fi
    }
    export -f subyard_supervise_profile
    setsid --wait bash -euo pipefail -c '\''subyard_supervise_profile "$@"'\'' \
      subyard "$$" "$owner_start" "$profile_dir" "$1" &
    wait "$!"
  ' subyard "$mode" 2>"$error_file"
pipeline_status=("${PIPESTATUS[@]}")
set -e
[ "${pipeline_status[0]}" -eq 0 ] || exit "${pipeline_status[0]}"
status="${pipeline_status[1]}"
if [ "$mode" = check ]; then
  case "$status" in
    0) printf 'converged\n' ;;
    10) printf 'changed\n' ;;
    *) cat "$error_file" >&2; exit "$status" ;;
  esac
else
  cat "$error_file" >&2
  exit "$status"
fi

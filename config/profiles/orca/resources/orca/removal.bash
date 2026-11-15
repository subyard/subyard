# shellcheck shell=bash
# Observe or remove the Orca wiring of a yard that does not select Orca, by the
# fixed paths this profile and its older releases installed. It runs no Orca
# helper itself; stopping the service runs the unit's own stop commands. State
# under /srv/agents/orca and the orca-ide package stay.
# Arguments: observe <guest-root> <desired> <dev-user> | apply <guest-root> <dev-user>.
# Every inspection fails closed: a failed query is never read as absence.
set -uo pipefail
mode="${1:-}"; root="${2:-}"; root="${root%/}"; uid=
main=subyard-orca.service
units=(subyard-orca-discovery.timer subyard-orca-discovery.service "$main")
files=(
  /usr/local/libexec/subyard/projects-changed.d/orca
  /usr/local/libexec/subyard/orca-registration.sha256
  /usr/local/libexec/subyard/orca-contract.sha256
  /usr/local/libexec/subyard/orca-registration.lock
  /etc/profile.d/subyard-orca-codex.sh
)
# The main unit's own files stay while it still runs: its stop needs them.
main_files=(
  /usr/local/libexec/subyard/orca-ingress
  /usr/local/libexec/subyard/orca-capture-ready
)
directories=(/usr/local/libexec/subyard/orca-registration /usr/local/libexec/subyard/orca-codex)
table=subyard_orca
marker=subyard-orca-managed

inspection_failed() { printf 'Orca removal: cannot inspect %s\n' "$1" >&2; exit 1; }

# Print "<ActiveState> <UnitFileState>" of a unit; fails when systemd cannot answer.
unit_state() {
  local active enabled
  active="$(systemctl show "$1" --property=ActiveState --value)" || return 1
  enabled="$(systemctl show "$1" --property=UnitFileState --value)" || return 1
  printf '%s %s\n' "${active:--}" "${enabled:--}"
}
unit_active() { case "$1" in active|activating|reloading|deactivating|refreshing) return 0 ;; esac; return 1; }
unit_enabled() { case "$1" in enabled|enabled-runtime|linked|linked-runtime|alias) return 0 ;; esac; return 1; }

# Print "present" or "absent" for the Orca nft table; fails when nft cannot answer.
# Without nft no such table can have been created by this profile.
table_state() {
  local tables
  command -v nft >/dev/null 2>&1 || { printf 'absent\n'; return 0; }
  tables="$(nft list tables)" || return 1
  if grep -Fxq "table inet $table" <<<"$tables"; then printf 'present\n'; else printf 'absent\n'; fi
}

# Print the Orca daemon and PTY scopes of the development user's manager.
user_systemctl() {
  runuser -u "$dev_user" -- env "XDG_RUNTIME_DIR=/run/user/$uid" \
    "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus" systemctl --user "$@"
}
daemon_scopes() {
  local state listing
  [ -n "$uid" ] || return 1
  state="$(systemctl show "user@$uid.service" --property=ActiveState --value)" || return 1
  case "$state" in
    inactive|failed) return 0 ;;
    active|activating|reloading|deactivating) ;;
    *) return 1 ;;
  esac
  listing="$(user_systemctl list-units --all --plain --no-legend --type=scope,slice \
    --state=active,activating,reloading,deactivating \
    'orca-daemon-*.scope' 'orca_pty_orca_daemon_*.scope' 'orca_ptys_orca_daemon_*.slice')" || return 1
  awk '{print $1}' <<<"$listing"
}

installed() {
  local unit path state scopes
  for unit in "${units[@]}"; do
    state="$(unit_state "$unit")" || inspection_failed "$unit"
    read -r active enabled <<<"$state"
    if unit_active "$active"; then printf 'active %s\n' "$unit"; fi
    if unit_enabled "$enabled"; then printf 'enabled %s\n' "$unit"; fi
  done
  for path in "${files[@]}" "${main_files[@]}" "/etc/systemd/system/${units[0]}" "/etc/systemd/system/${units[1]}" \
    "/etc/systemd/system/$main" "${directories[@]}"; do
    if [ -e "$root$path" ] || [ -L "$root$path" ]; then printf 'present %s\n' "$path"; fi
  done
  state="$(table_state)" || inspection_failed "the Orca nft table"
  if [ "$state" = present ]; then printf 'present nft inet %s\n' "$table"; fi
  scopes="$(daemon_scopes)" || inspection_failed "the Orca daemon scopes of $dev_user"
  if [ -n "$scopes" ]; then printf 'scope %s\n' "$scopes"; fi
}

case "$mode" in
  observe)
    desired="${3:-}"; dev_user="${4:-}"
    [[ "$desired" =~ ^[0-9a-f]{64}$ ]] || { echo 'invalid Orca removal digest' >&2; exit 2; }
    [[ "$dev_user" =~ ^[a-z_][a-z0-9_-]*$ ]] || { echo 'invalid Orca development user' >&2; exit 2; }
    uid="$(id -u "$dev_user")" || inspection_failed "the development user $dev_user"
    found="$(installed)" || exit 1
    if [ -z "$found" ]; then
      # Converged removal: current at the removal digest, proving no ownership.
      printf '{"state":"current","actual":"%s","desired":"%s","ownership_absent":true}\n' "$desired" "$desired"
    else
      printf '{"state":"stale","actual":"%s","desired":"%s"}\n' \
        "$(printf '%s\n' "$found" | sha256sum | cut -d ' ' -f 1)" "$desired"
    fi
    ;;
  apply)
    dev_user="${3:-}"
    [[ "$dev_user" =~ ^[a-z_][a-z0-9_-]*$ ]] || { echo 'invalid Orca development user' >&2; exit 2; }
    # Every independent step runs; each refusal is reported in stable order.
    # An ownership refusal is printed as a blocking line on stdout.
    refusals=()
    uid="$(id -u "$dev_user")" || { uid=; refusals+=("cannot resolve the development user $dev_user"); }
    blocking=()
    declare -A running=()
    for unit in "${units[@]}"; do
      if ! state="$(unit_state "$unit")"; then
        refusals+=("cannot inspect $unit"); running[$unit]=1; continue
      fi
      read -r active enabled <<<"$state"
      if unit_active "$active"; then
        systemctl stop "$unit" >/dev/null 2>&1 || refusals+=("could not stop $unit")
      fi
      if unit_enabled "$enabled"; then
        systemctl disable "$unit" >/dev/null 2>&1 || refusals+=("could not disable $unit")
      fi
      if ! state="$(unit_state "$unit")"; then
        refusals+=("cannot inspect $unit after its stop"); running[$unit]=1; continue
      fi
      read -r active enabled <<<"$state"
      if unit_active "$active"; then refusals+=("$unit is still running"); running[$unit]=1; fi
    done
    # The ingress guard closes the Orca port while the service runs, so it is
    # removed only after the service is stopped.
    if [ -z "${running[$main]:-}" ]; then
      if ! state="$(table_state)"; then
        refusals+=("cannot inspect the Orca nft table")
      elif [ "$state" = present ]; then
        if ! chain="$(nft list chain inet "$table" input 2>/dev/null)"; then
          refusals+=("cannot inspect the Orca nft table inet $table")
        elif grep -F "comment \"$marker\"" >/dev/null <<<"$chain"; then
          nft delete table inet "$table" || refusals+=("could not remove the Orca nft table inet $table")
        else
          blocking+=("refusing to remove the unowned nft table inet $table")
        fi
      fi
    fi
    for path in "${files[@]}"; do
      rm -f -- "$root$path" || refusals+=("could not remove $path")
    done
    for unit in "${units[@]}"; do
      [ -z "${running[$unit]:-}" ] || continue
      rm -f -- "$root/etc/systemd/system/$unit" || refusals+=("could not remove /etc/systemd/system/$unit")
    done
    if [ -z "${running[$main]:-}" ]; then
      for path in "${main_files[@]}"; do
        rm -f -- "$root$path" || refusals+=("could not remove $path")
      done
    fi
    for path in "${directories[@]}"; do
      rm -rf -- "$root$path" || refusals+=("could not remove $path")
    done
    systemctl daemon-reload || refusals+=("could not reload systemd units")
    # Orca daemon and terminal scopes of the development user.
    if ! scopes="$(daemon_scopes)"; then
      refusals+=("cannot inspect the Orca daemon scopes of $dev_user")
    else
      for _ in 1 2 3; do
        [ -n "$scopes" ] || break
        mapfile -t names <<<"$scopes"
        user_systemctl stop -- "${names[@]}" >/dev/null 2>&1 || true
        scopes="$(daemon_scopes)" || { scopes=unknown; break; }
      done
      [ -z "$scopes" ] || refusals+=("Orca daemon scopes of $dev_user are still running")
    fi
    for refusal in "${blocking[@]}" "${refusals[@]}"; do printf 'Orca removal: %s\n' "$refusal" >&2; done
    for refusal in "${blocking[@]}"; do printf 'subyard-orca-removal-blocking %s\n' "$refusal"; done
    [ "${#refusals[@]}" -eq 0 ] && [ "${#blocking[@]}" -eq 0 ]
    ;;
  *) echo 'usage: removal.bash observe <guest-root> <desired> <dev-user> | apply <guest-root> <dev-user>' >&2; exit 2 ;;
esac

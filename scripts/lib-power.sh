#!/usr/bin/env bash
# lib-power.sh — pure helpers for persisted yard power intent and host-network safety.
# Source it from host lifecycle scripts or from the installed boot reconciler. It deliberately
# does not source Subyard settings: the root boot service trusts only Incus metadata.

[ -n "${SUBYARD_LIBPOWER_SOURCED:-}" ] && return 0
SUBYARD_LIBPOWER_SOURCED=1

# shellcheck disable=SC2034 # exported to sourced callers
POWER_KEY_MANAGED=user.subyard.managed
# shellcheck disable=SC2034 # exported to sourced callers
POWER_KEY_NAME=user.subyard.name
# shellcheck disable=SC2034 # exported to sourced callers
POWER_KEY_BRIDGE=user.subyard.bridge
# shellcheck disable=SC2034 # exported to sourced callers
POWER_KEY_DESIRED=user.subyard.desired_power
# shellcheck disable=SC2034 # exported to sourced callers
POWER_KEY_INITIALIZED=user.subyard.initialized
# Written by the first finished init and never cleared; a re-init keeps it.
POWER_KEY_EVER_INITIALIZED=user.subyard.ever_initialized
POWER_ERROR=

power_fail() {
  POWER_ERROR="$*"
  return 1
}

# The CSV <columns> of exactly <instance>, without its name, or nothing when it
# is absent: `incus list NAME` also lists every instance whose name starts with NAME.
# Column 4 (IPv4) prints only the first address line; an empty value means none.
incus_instance_columns() { # <project> <instance> <columns> [timeout]
  local rows
  if [ -n "${4:-}" ]; then
    rows="$(timeout "$4" incus list "$2" --project "$1" -f csv -c "n$3" 2>/dev/null)" || return
  else
    rows="$(incus list "$2" --project "$1" -f csv -c "n$3" 2>/dev/null)" || return
  fi
  printf '%s\n' "$rows" | awk -v prefix="$2," 'index($0, prefix) == 1 { print substr($0, length(prefix) + 1); exit }'
}

power_state() { incus_instance_columns "$1" "$2" s; } # <project> <instance>

# Succeeds only for a managed instance whose init has never finished:
# initialized=false without the completion mark. A yard from a release before
# the mark has initialized=true. Any failed read is not never-initialized.
power_never_initialized() { # <project> <instance>
  local managed initialized ever
  managed="$(incus config get "$2" "$POWER_KEY_MANAGED" --project "$1" 2>/dev/null)" || return 1
  initialized="$(incus config get "$2" "$POWER_KEY_INITIALIZED" --project "$1" 2>/dev/null)" || return 1
  ever="$(incus config get "$2" "$POWER_KEY_EVER_INITIALIZED" --project "$1" 2>/dev/null)" || return 1
  [ "$managed" = true ] && [ "$initialized" = false ] && [ "$ever" != true ]
}

# Succeeds only for a running never-initialized instance whose guest lacks the
# substrate provisioning installs first (python3 and /srv/agents), so no later
# init stage has run there. It does not depend on the current developer name,
# which a rename can change. Only an explicit answer of a successful probe counts.
power_guest_never_provisioned() { # <project> <instance>
  local answer
  power_never_initialized "$1" "$2" || return 1
  [ "$(power_state "$1" "$2")" = RUNNING ] || return 1
  answer="$(incus exec "$2" --project "$1" -- sh -c \
    'if command -v python3 >/dev/null && test -d /srv/agents; then echo provisioned; else echo unprovisioned; fi' \
    2>/dev/null)" || return 1
  [ "$answer" = unprovisioned ]
}

# Validate the effective NetworkManager configuration, not merely our drop-in file: a later distro
# file can override unmanaged-devices and recreate the route-hijack incident. No active NM is safe.
power_nm_active() {
  local state
  if ! command -v systemctl >/dev/null 2>&1; then
    power_nm_binary >/dev/null 2>&1 || return 1
    power_fail "cannot inspect NetworkManager service state"
    return 2
  fi
  state="$(systemctl is-active NetworkManager 2>/dev/null)" || true
  case "$state" in
    active | activating | reloading | deactivating) return 0 ;;
    inactive | failed | unknown) return 1 ;;
    *) power_fail "cannot inspect NetworkManager service state"; return 2 ;;
  esac
}

power_nm_binary() {
  local path
  for path in /usr/sbin/NetworkManager /usr/bin/NetworkManager /sbin/NetworkManager; do
    [ -x "$path" ] && { printf '%s\n' "$path"; return 0; }
  done
  return 1
}

power_nm_prepare_reader() {
  local rc
  if power_nm_active; then :; else
    rc=$?; [ "$rc" -eq 1 ] && return 0
    return "$rc"
  fi
  [ "$(id -u)" -ne 0 ] || return 0
  command -v sudo >/dev/null 2>&1 \
    || { power_fail "sudo is required to verify NetworkManager configuration"; return 1; }
  # `sudo -v` demands a password when any matching sudoers rule does, even with NOPASSWD:ALL.
  if sudo -n true </dev/null 2>/dev/null; then
    :
  elif [ "${SUBYARD_SUDO_PREAUTHORIZED:-0}" = 1 ]; then
    power_fail "sudo authorization expired; re-run the yard command in an operator terminal"
    return 1
  elif ! power_operator_terminal; then
    power_fail "sudo authorization is required to verify NetworkManager configuration; re-run the yard command in an operator terminal"
    return 1
  else
    sudo -v \
      || { power_fail "could not authorize NetworkManager configuration check"; return 1; }
  fi
  export SUBYARD_SUDO_PREAUTHORIZED=1
}

# sudo prompts on the controlling terminal; without one, `sudo -v` can only fail.
power_operator_terminal() { { : </dev/tty; } 2>/dev/null; }

power_nm_print_config() {
  local binary
  binary="$(power_nm_binary)" || return 1
  if [ "$(id -u)" -eq 0 ] || [ "${SUBYARD_SUDO_PREAUTHORIZED:-0}" != 1 ]; then
    "$binary" --print-config
  else
    command -v sudo >/dev/null 2>&1 || return 1
    sudo -n -- "$binary" --print-config
  fi
}

# Guard rule, shared with internal/adapters/hostruntime and the vectors in
# tests/fixtures/nm-guard-vectors: NetworkManager prints [device*] sections in priority order,
# except the plain [device] section, which it always considers last. [device-subyard] must set
# managed=0 for type:veth, driver:veth and every bridge without except: entries, and no section
# printed before it may set managed=1 or stop-match for devices the guard covers. Each violation
# is printed on its own line; nothing is printed when the configuration is safe.
power_nm_config_violations() { # <bridge...>; stdin: NetworkManager --print-config
  awk -v bridge_list="$*" '
    function trim(value) { gsub(/^[[:space:]]+|[[:space:]]+$/, "", value); return value }
    function truthy(value) { value = tolower(value); return value == "1" || value == "true" || value == "yes" || value == "on" }
    function falsy(value) { value = tolower(value); return value == "0" || value == "false" || value == "no" || value == "off" }
    function related(name,    index_) {
      if (name ~ /^veth/ || name ~ /^tap........$/) return 1
      for (index_ = 1; index_ <= bridge_count; index_++) if (name == bridges[index_]) return 1
      return 0
    }
    # A section only reaches unrelated devices when every entry is an exact, unrelated name.
    function unrelated_only(list,    count, entries, index_, entry, name) {
      count = split(list, entries, /[,;]/)
      if (trim(list) == "") return 0
      for (index_ = 1; index_ <= count; index_++) {
        entry = trim(entries[index_])
        if (entry == "") continue
        if (entry ~ /^interface-name:=/) name = substr(entry, 17)
        else if (entry ~ /^interface-name:~/) name = substr(entry, 17)
        else if (entry ~ /^interface-name:/) name = substr(entry, 16)
        else if (entry !~ /:/) name = entry
        else return 0
        if (entry !~ /^interface-name:=/ && name ~ /[*?]/) return 0
        if (name == "" || related(name)) return 0
      }
      return 1
    }
    BEGIN { bridge_count = split(bridge_list, bridges, " "); current = 0 }
    /^[[:space:]]*\[/ {
      name = trim($0); gsub(/^\[|\]$/, "", name)
      sections[++section_count] = name; current = section_count
      next
    }
    current && /=/ {
      key = trim(substr($0, 1, index($0, "=") - 1)); value = trim(substr($0, index($0, "=") + 1))
      values[current, key] = value; present[current, key] = 1
    }
    END {
      for (index_ = 1; index_ <= section_count; index_++) if (sections[index_] == "device-subyard") { guard = index_; break }
      if (!guard) { print "NetworkManager has no [device-subyard] guard section"; exit }
      if (!falsy(values[guard, "managed"])) print "[device-subyard] does not set managed=0"
      list = values[guard, "match-device"]
      count = split(list, entries, /[,;]/)
      for (index_ = 1; index_ <= count; index_++) {
        entry = trim(entries[index_]); if (entry != "") listed[entry] = 1
        if (entry ~ /^except:/) print "[device-subyard] match-device has the exception " entry
      }
      required_count = split("type:veth driver:veth", required, " ")
      for (index_ = 1; index_ <= bridge_count; index_++) required[++required_count] = "interface-name:" bridges[index_]
      for (index_ = 1; index_ <= required_count; index_++)
        if (!(required[index_] in listed)) print "[device-subyard] match-device lacks " required[index_]
      for (index_ = 1; index_ < guard; index_++) {
        name = sections[index_]
        if (name !~ /^device/ || name == "device") continue
        if (!truthy(values[index_, "managed"]) && !truthy(values[index_, "stop-match"])) continue
        if (unrelated_only(values[index_, "match-device"])) continue
        print "[" name "] has priority over [device-subyard] and sets " (truthy(values[index_, "managed"]) ? "managed=1" : "stop-match") " for devices the guard covers"
      }
    }
  '
}

# Existing yard bridges that NetworkManager manages, one per line. A failed or empty nmcli
# read fails: it never stands for an empty device list.
power_nm_managed_bridges() { # <bridge...>
  local devices device state bridge
  command -v nmcli >/dev/null 2>&1 || return 1
  devices="$(nmcli -t -f DEVICE,STATE device 2>/dev/null)" || return 1
  [ -n "$devices" ] || return 1
  while IFS=: read -r device state; do
    for bridge in "$@"; do
      if [ "$device" = "$bridge" ] && [ "$state" != unmanaged ]; then printf '%s\n' "$device"; fi
    done
  done <<< "$devices"
}

POWER_NM_GUARD_COMMAND="${POWER_NM_GUARD_COMMAND:-yard init}"
# The pending NetworkManager guard record: boot ID, NetworkManager invocation and kind. `unread`:
# NetworkManager has not started since the guard was first written, so no yard may start.
# `lists`: a migrated guard waits for the host reboot. Both clear on a new boot or a new
# NetworkManager invocation, since NetworkManager reads every file when it starts.
POWER_NM_REBOOT_PENDING=/run/subyard/nm-guard-reboot-pending

power_nm_boot_id() { cat /proc/sys/kernel/random/boot_id 2>/dev/null; }

power_nm_guard_pending() { # prints the kind while the record is pending
  local boot invocation kind current
  [ -r "$POWER_NM_REBOOT_PENDING" ] || return 0
  read -r boot invocation kind < "$POWER_NM_REBOOT_PENDING" || [ -n "${boot:-}" ] || return 0
  [ -n "$boot" ] && [ "$boot" = "$(power_nm_boot_id)" ] || return 0
  if [ -n "${invocation:-}" ] && [ "$invocation" != unknown ]; then
    current="$(systemctl show -p InvocationID --value NetworkManager 2>/dev/null)" || current=''
    [ -z "$current" ] || [ "$current" = "$invocation" ] || return 0
  fi
  case "${kind:-lists}" in unread) printf 'unread\n' ;; *) printf 'lists\n' ;; esac
}

power_nm_reboot_pending() { [ "$(power_nm_guard_pending)" = lists ]; }

power_join() { # stdin lines -> one "; "-separated line
  local out='' line
  while IFS= read -r line; do [ -z "$line" ] || out+="${out:+; }$line"; done
  printf '%s' "$out"
}

power_join_comma() { # stdin lines -> one ", "-separated line
  local out='' line
  while IFS= read -r line; do [ -z "$line" ] || out+="${out:+, }$line"; done
  printf '%s' "$out"
}

power_nm_guard_effective() { # <bridge...>
  local config violations managed rc
  if power_nm_active; then :; else
    rc=$?; [ "$rc" -eq 1 ] && return 0
    return "$rc"
  fi
  config="$(power_nm_print_config 2>/dev/null)" || {
    if [ "$(id -u)" -eq 0 ]; then
      power_fail "cannot read NetworkManager's effective configuration"
    else
      power_fail "cannot read NetworkManager's effective configuration as root; check that 'sudo -n NetworkManager --print-config' works, then re-run the yard command"
    fi
    return 1
  }
  violations="$(printf '%s\n' "$config" | power_nm_config_violations "$@")"
  if [ "$(power_nm_guard_pending)" = unread ]; then
    violations+="${violations:+$'\n'}NetworkManager has not read the Subyard guard since it was written"
  fi
  if ! managed="$(power_nm_managed_bridges "$@")"; then
    violations+="${violations:+$'\n'}cannot read NetworkManager's devices with nmcli"
  elif [ -n "$managed" ]; then
    while IFS= read -r rc; do
      violations+="${violations:+$'\n'}NetworkManager manages yard bridge $rc"
    done <<< "$managed"
  fi
  [ -z "$violations" ] && return 0
  case "$(power_nm_guard_pending)" in
    unread)
      power_fail "NetworkManager guard is not effective: $(power_join <<< "$violations") — reboot the host or restart NetworkManager ('sudo systemctl restart NetworkManager'), then rerun $POWER_NM_GUARD_COMMAND"
      return 1 ;;
    lists)
      power_fail "NetworkManager guard is not effective: $(power_join <<< "$violations") — a migrated NetworkManager guard waits for a host reboot: reboot the host once, then rerun $POWER_NM_GUARD_COMMAND"
      return 1 ;;
  esac
  power_fail "NetworkManager guard is not effective: $(power_join <<< "$violations") — correct the NetworkManager configuration, then rerun $POWER_NM_GUARD_COMMAND"
}

# Subyard's own devices only: yard bridges, veths and Incus tap devices.
power_route_device_unsafe() { # <device> [bridge...]
  local dev="$1" bridge details
  shift
  for bridge in "$@"; do [ "$dev" = "$bridge" ] && return 0; done
  case "$dev" in veth*) return 0 ;; esac
  [[ "$dev" =~ ^tap.{8}$ ]] && return 0
  details="$(ip -d link show dev "$dev" 2>/dev/null)" || details=
  case " $details " in *' veth '*|*' veth@'*) return 0 ;; esac
  return 1
}

# Fail when any IPv4 default route points through an Incus/container interface. No default route is
# acceptable here: this predicate protects route ownership; it is not an Internet liveness check.
power_routes_safe() { # [bridge...]
  local routes line token expect_dev=0
  command -v ip >/dev/null 2>&1 \
    || { power_fail "ip is required for the host route guard"; return 1; }
  routes="$(ip -4 route show default 2>/dev/null)" \
    || { power_fail "cannot inspect host IPv4 default routes"; return 1; }
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    expect_dev=0
    for token in $line; do
      if [ "$expect_dev" = 1 ]; then
        if power_route_device_unsafe "$token" "$@"; then
          power_fail "unsafe host default route uses '$token': $line"
          return 1
        fi
        expect_dev=0
      elif [ "$token" = dev ]; then
        expect_dev=1
      fi
    done
  done <<<"$routes"
  return 0
}

# Both guards always run, so a refusal names every independent problem.
power_host_safe() { # <bridge...>
  local errors=()
  [ "$#" -gt 0 ] || { power_fail "power_host_safe needs at least one managed bridge"; return 1; }
  power_nm_guard_effective "$@" || errors+=("$POWER_ERROR")
  power_routes_safe "$@" || errors+=("$POWER_ERROR")
  [ "${#errors[@]}" -eq 0 ] && return 0
  power_fail "$(printf '%s\n' "${errors[@]}" | power_join)"
}

power_start_guarded() { # <project> <instance> <bridge...>
  local project="$1" instance="$2" current err
  shift 2
  power_host_safe "$@" || return 1
  if [ "${SUBYARD_ENGINE_CONTEXT:-}" = 1 ] && [ -n "${SUBYARD_DISPATCHER_PATH:-}" ] && [ -x "$SUBYARD_DISPATCHER_PATH" ]; then
    local publication_wait engine="${SUBYARD_DISPATCHER_PATH:-}"
    [ -n "$engine" ] && [ -x "$engine" ] || { power_fail 'optional publication engine is unavailable'; return 1; }
    publication_wait="$("$engine" _publications "$project" "$instance")" \
      || { power_fail 'could not reconcile optional owner publication'; return 1; }
    if [ -n "$publication_wait" ]; then
      printf 'Optional owner publication waiting for address: %s\n' "$publication_wait"
    fi
  fi
  current="$(power_state "$project" "$instance")"
  if [ "$current" != RUNNING ]; then
    # QEMU reads raw.qemu.conf only when it starts, and Incus refuses to change
    # it on a running VM: the engine writes the owner value of a stopped VM here.
    if [ "${YARD_KIND:-}" = vm ]; then
      local policy_engine="${SUBYARD_DISPATCHER_PATH:-}"
      [ -n "$policy_engine" ] && [ -x "$policy_engine" ] \
        || { power_fail 'VM start policy engine is unavailable'; return 1; }
      "$policy_engine" _vm-start-policy "$project" "$instance" "${SUBYARD_INCUS_SOCKET:-${INCUS_SOCKET:-}}" \
        || { power_fail "could not apply the VM QEMU device policy before starting $project/$instance"; return 1; }
    fi
    incus start "$instance" --project "$project" || {
      power_fail "failed to start $project/$instance"
      return 1
    }
  fi
  if ! power_host_safe "$@"; then
    err="$POWER_ERROR"
    if incus stop "$instance" --project "$project" --force >/dev/null 2>&1; then
      POWER_ERROR="$err; $project/$instance was stopped fail-closed"
    else
      POWER_ERROR="$err; FAILED to stop unsafe $project/$instance"
    fi
    return 1
  fi
  if [ -n "${VM_CPU_WEIGHT:-}" ]; then
    local engine="${SUBYARD_DISPATCHER_PATH:-}" rc=0
    err='VM CPU scheduling failed'
    if [ -z "$engine" ] || [ ! -x "$engine" ]; then
      err='VM CPU scheduling engine is unavailable'
      rc=1
    else
      "$engine" _vm-cpu apply "$project" "$instance" "${SUBYARD_INCUS_SOCKET:-${INCUS_SOCKET:-}}" || rc=$?
    fi
    if [ "$rc" -ne 0 ]; then
      incus stop "$instance" --project "$project" --force >/dev/null 2>&1 \
        || { power_fail "$err; FAILED to stop $project/$instance"; return 1; }
      power_fail "$err; $project/$instance was stopped fail-closed"
      return 1
    fi
  fi
}

power_guest_start_failed() { # <project> <instance> <error>
  local project="$1" instance="$2" err="$3" current
  if [ -n "${VM_CPU_WEIGHT:-}" ]; then
    current="$(power_state "$project" "$instance")" || current=
    if [ "$current" != STOPPED ]; then
      if incus stop "$instance" --project "$project" --force >/dev/null 2>&1; then
        err="$err; $project/$instance was stopped fail-closed"
      else
        power_fail "$err; FAILED to stop $project/$instance"
        return 1
      fi
    fi
  fi
  power_fail "$err"
}

power_stop_instance() { # <project> <instance>
  local current
  current="$(power_state "$1" "$2")"
  case "$current" in
    RUNNING) incus stop "$2" --project "$1" ;;
    STOPPED) return 0 ;;
    *) power_fail "cannot stop $1/$2 from state '${current:-unknown}'"; return 1 ;;
  esac
}

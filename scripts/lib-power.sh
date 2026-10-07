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
POWER_ERROR=

power_fail() {
  POWER_ERROR="$*"
  return 1
}

power_state() { # <project> <instance>
  incus list "$2" --project "$1" -f csv -c s 2>/dev/null
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
  if [ "${SUBYARD_SUDO_PREAUTHORIZED:-0}" = 1 ]; then
    sudo -n true </dev/null \
      || { power_fail "sudo authorization expired; re-run 'yard start' in an operator terminal"; return 1; }
  else
    sudo -v \
      || { power_fail "could not authorize NetworkManager configuration check"; return 1; }
  fi
  export SUBYARD_SUDO_PREAUTHORIZED=1
}

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

power_nm_guard_effective() { # <bridge>
  local bridge="$1" config unmanaged no_auto rc
  if power_nm_active; then :; else
    rc=$?; [ "$rc" -eq 1 ] && return 0
    return "$rc"
  fi
  config="$(power_nm_print_config 2>/dev/null)" || {
    if [ "$(id -u)" -eq 0 ]; then
      power_fail "cannot read NetworkManager's effective configuration"
    else
      power_fail "cannot read NetworkManager's effective configuration as root — run 'sudo -v'"
    fi
    return 1
  }
  unmanaged="$(printf '%s\n' "$config" | sed -n 's/^[[:space:]]*unmanaged-devices[[:space:]]*=[[:space:]]*//p' | tail -n1)"
  no_auto="$(printf '%s\n' "$config" | sed -n 's/^[[:space:]]*no-auto-default[[:space:]]*=[[:space:]]*//p' | tail -n1)"
  case ";$unmanaged;" in *';type:veth;'*) ;; *) power_fail "NM effective unmanaged-devices lacks type:veth"; return 1 ;; esac
  case ";$unmanaged;" in *';driver:veth;'*) ;; *) power_fail "NM effective unmanaged-devices lacks driver:veth"; return 1 ;; esac
  case ";$unmanaged;" in *";interface-name:$bridge;"*) ;; *) power_fail "NM effective unmanaged-devices lacks bridge $bridge"; return 1 ;; esac
  case ";$no_auto;" in *';type:veth;'*) ;; *) power_fail "NM effective no-auto-default lacks type:veth"; return 1 ;; esac
  case ";$no_auto;" in *";interface-name:$bridge;"*) ;; *) power_fail "NM effective no-auto-default lacks bridge $bridge"; return 1 ;; esac
  printf '%s\n' "$config" | grep -Eq '^[[:space:]]*managed[[:space:]]*=[[:space:]]*0([[:space:]]|$)' \
    || { power_fail "NM effective config lacks the managed=0 device guard"; return 1; }
}

power_route_device_unsafe() { # <device> [bridge...]
  local dev="$1" bridge details
  shift
  for bridge in "$@"; do [ "$dev" = "$bridge" ] && return 0; done
  case "$dev" in veth*|tap*|macvtap*|vnet*|docker*|br-*|virbr*) return 0 ;; esac
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

power_host_safe() { # <bridge...>
  local bridge
  [ "$#" -gt 0 ] || { power_fail "power_host_safe needs at least one managed bridge"; return 1; }
  for bridge in "$@"; do power_nm_guard_effective "$bridge" || return 1; done
  power_routes_safe "$@"
}

power_start_guarded() { # <project> <instance> <bridge...>
  local project="$1" instance="$2" current err
  shift 2
  power_host_safe "$@" || return 1
  current="$(power_state "$project" "$instance")"
  if [ "$current" != RUNNING ]; then
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

power_stop_instance() { # <project> <instance>
  local current
  current="$(power_state "$1" "$2")"
  case "$current" in
    RUNNING) incus stop "$2" --project "$1" ;;
    STOPPED) return 0 ;;
    *) power_fail "cannot stop $1/$2 from state '${current:-unknown}'"; return 1 ;;
  esac
}

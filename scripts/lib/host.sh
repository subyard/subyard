#!/usr/bin/env bash
# host.sh — explicit host/system adapters and fail-closed guards.

[ -n "${SUBYARD_HOST_SOURCED:-}" ] && return 0
SUBYARD_HOST_SOURCED=1

_subyard_host_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib-power.sh
. "$_subyard_host_lib_dir/../lib-power.sh"
# shellcheck source=scripts/lib/download.sh
. "$_subyard_host_lib_dir/download.sh"
unset _subyard_host_lib_dir

require_root() {
  [ "$(id -u)" -eq 0 ] && return 0
  local why="${1:-it changes the host system}" operator_user
  local -a sudo_args=()
  if command -v sudo >/dev/null 2>&1; then
    operator_user="${SUBYARD_USER:-${SUDO_USER:-$(id -un)}}"
    subyard_elevated_context
    SUBYARD_ELEVATED_ENV+=("SUBYARD_USER=$operator_user")
    warn "this needs root: $why"
    if [ "${SUBYARD_SUDO_PREAUTHORIZED:-0}" = 1 ]; then
      info "re-running under pre-authorized sudo…"
      sudo -n true </dev/null 2>/dev/null \
        || die "sudo authorization expired; re-run the parent yard command in an operator terminal"
      sudo_args=(-n)
    else
      info "re-running under sudo (you'll be asked for your password)…"
    fi
    exec sudo "${sudo_args[@]}" -- env "${SUBYARD_ELEVATED_ENV[@]}" \
      "$SUBYARD_SCRIPT_PATH" ${SUBYARD_SCRIPT_ARGV[@]+"${SUBYARD_SCRIPT_ARGV[@]}"}
  fi
  printf '\n%sNeeds root and sudo is not installed — run as root:%s\n    %s%s %s%s\n\n' \
    "$C_WARN" "$C_OFF" "$C_HEAD" "$SUBYARD_SCRIPT_PATH" "${SUBYARD_SCRIPT_ARGV[*]:-}" "$C_OFF" >&2
  exit 1
}

host_sudo() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
    return
  fi
  command -v sudo >/dev/null 2>&1 \
    || { warn "sudo is required to change the owner host"; return 1; }
  if [ "${SUBYARD_SUDO_PREAUTHORIZED:-0}" = 1 ]; then
    sudo -n true </dev/null 2>/dev/null \
      || { warn "sudo authorization expired; re-run the parent yard command in an operator terminal"; return 1; }
    sudo -n -- "$@"
  else
    sudo -- "$@"
  fi
}

subyard_home_validate_root() {
  local data_home="${1:-}" operator_home
  SUBYARD_VALIDATED_HOME=""
  case "$data_home" in /*) ;; *) return 2 ;; esac
  [ ! -L "$data_home" ] || return 2
  data_home="$(realpath -m -- "$data_home")"
  operator_home="$(realpath -m -- "${SUBYARD_OPERATOR_HOME:-/}")"
  [ "$data_home" != "$operator_home" ] && ! path_is_broad_host_root "$data_home" || return 2
  [ ! -e "$data_home" ] || [ -d "$data_home" ] || return 2
  SUBYARD_VALIDATED_HOME="$data_home"
}

subyard_home_remove_if_empty() {
  local data_home="${1:-}"
  subyard_home_validate_root "$data_home" || return
  data_home="$SUBYARD_VALIDATED_HOME"
  [ -e "$data_home" ] || { printf 'absent\n'; return 0; }
  if rmdir -- "$data_home" 2>/dev/null; then
    printf 'removed\n'
  else
    printf 'retained\n'
  fi
}

subyard_state_remove_canonical() {
  local state_dir="${1:-}" config_home="${2:-}" yard_name="${3:-}" expected
  case "$state_dir:$config_home" in /*:/*) ;; *) return 2 ;; esac
  config_home="$(realpath -m -- "$config_home")"
  ! path_is_broad_host_root "$config_home" || return 2
  expected="$config_home/projects"
  [ -z "$yard_name" ] || expected="$config_home/yards/$yard_name/projects"
  if [ -L "$state_dir" ] || [ "$(realpath -m -- "$state_dir")" != "$expected" ]; then
    printf 'preserved\n'
  elif [ ! -e "$state_dir" ]; then
    printf 'absent\n'
  elif [ ! -d "$state_dir" ]; then
    return 2
  else
    if [ -n "${SUBYARD_TEARDOWN_ARTIFACTS:-}" ]; then
      python3 "${BASH_SOURCE[0]%/*}/teardown-plan.py" remove-artifact "$state_dir" || return
    else
      rm -rf -- "$state_dir"
    fi
    printf 'removed\n'
  fi
}

incus_preflight() {
  command -v incus >/dev/null 2>&1 || die "incus not found — run 'yard init' first"
  incus info >/dev/null 2>&1 && return 0
  if id -nG "$(id -un)" 2>/dev/null | tr ' ' '\n' | grep -qx incus-admin; then
    warn "can't reach the Incus daemon: this session predates your 'incus-admin' group (the yard is fine)."
    printf "  Log out and back in once to fix it everywhere, or run %snewgrp incus-admin%s for this shell.\n" \
      "$C_HEAD" "$C_OFF" >&2
    exit 1
  fi
  die "can't reach Incus — you're not in the 'incus-admin' group, or the daemon isn't running. Run 'yard init' first."
}

# Returns 0 when the agent answers, 1 at the deadline, 2 for an invalid wait
# setting and 3 when the instance stays STOPPED, so nothing is left to wait for.
incus_wait_instance_agent() {
  local project="${1:?project required}" instance="${2:?instance required}"
  local wait_timeout=120 deadline remaining probe_timeout stopped_window stopped_since=''
  # Cloud-image seeding can reboot a VM before its agent accepts connections.
  # Allow two boot sequences while keeping one absolute deadline.
  [ "${YARD_KIND:-}" != vm ] || wait_timeout=600
  wait_timeout="${SUBYARD_INCUS_AGENT_WAIT_TIMEOUT:-$wait_timeout}"
  [[ "$wait_timeout" =~ ^[1-9][0-9]*$ ]] || return 2
  # A guest reboot that Incus serves with a new QEMU process passes through
  # STOPPED for about two seconds between the old process and the new one. Only
  # a STOPPED state held for this whole window means no boot is in progress.
  stopped_window="${SUBYARD_INCUS_STOPPED_WINDOW:-30}"
  [[ "$stopped_window" =~ ^[1-9][0-9]*$ ]] || return 2
  deadline=$((SECONDS + wait_timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    remaining=$((deadline - SECONDS))
    probe_timeout="$remaining"
    [ "$probe_timeout" -le 5 ] || probe_timeout=5
    timeout --foreground "$probe_timeout" \
      incus exec "$instance" --project "$project" -- true >/dev/null 2>&1 \
      && return 0
    # The state read has only the remaining budget, so a stalled Incus cannot extend the wait.
    remaining=$((deadline - SECONDS))
    [ "$remaining" -gt 0 ] || break
    if [ "$(incus_instance_columns "$project" "$instance" s "$remaining")" = STOPPED ]; then
      [ -n "$stopped_since" ] || stopped_since=$SECONDS
      [ $((SECONDS - stopped_since)) -lt "$stopped_window" ] || return 3
    else
      stopped_since=''
    fi
    [ "$SECONDS" -ge "$deadline" ] || sleep 1
  done
  return 1
}

# Prints the cause that an incus_wait_instance_agent status adds to a caller's
# diagnostic: nothing for a plain timeout.
incus_agent_wait_detail() { # <status> <project> <instance>
  case "$1" in
    2) printf '; the agent wait setting is invalid' ;;
    3) printf "; instance '%s' stopped and is not booting (its last boot log: incus info %s --project %s --show-log)" \
         "$3" "$3" "$2" ;;
  esac
}

# Waits for the agent of the yard instance. On failure it stores <message>, the
# cause and, for an instance that stays STOPPED, the optional <hint> in
# AGENT_WAIT_ERROR, and returns the incus_wait_instance_agent status.
incus_wait_yard_agent() { # <message> [hint]
  local rc=0
  AGENT_WAIT_ERROR=''
  incus_wait_instance_agent "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" || rc=$?
  [ "$rc" != 0 ] || return 0
  AGENT_WAIT_ERROR="$1$(incus_agent_wait_detail "$rc" "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")"
  if [ "$rc" = 3 ] && [ -n "${2:-}" ]; then AGENT_WAIT_ERROR+="; $2"; fi
  return "$rc"
}

incus_project_has_isolated_images() {
  [ "$(incus project get "$1" features.images 2>/dev/null)" != false ]
}

# --- NetworkManager guard --------------------------------------------------------------------
# A self-contained [device-subyard] section marks Subyard's own devices unmanaged: veths, Incus
# tap devices and Incus-managed bridges. NetworkManager reads the section when it starts and
# applies it to a device when the device appears. Subyard never reloads or restarts
# NetworkManager: a reload would also apply every other file on disk, including administrator
# edits nobody has applied yet. No yard starts until NetworkManager has started after the guard
# was first written; existing own bridges are released with `nmcli device set BRIDGE managed no`,
# the only state-changing NetworkManager call Subyard makes.
# Released versions instead replaced [main] no-auto-default and [keyfile] unmanaged-devices.
NM_GUARD_CONF=/etc/NetworkManager/conf.d/zz-subyard-unmanaged.conf
NM_GUARD_OWN="type:veth,driver:veth,interface-name:tap????????"
NM_DEVICES=''
# Set by nm_guard_apply: NetworkManager must start before the guard applies.
NM_GUARD_NEEDS_START=0

nm_path_exists() { [ -e "$1" ] || [ -L "$1" ]; }
nm_legacy_guard() { printf '%s/99-subyard-unmanaged.conf\n' "${1%/*}"; } # <conf>

# Incus-managed bridges only: the default-project list also shows host bridges such as an
# administrator's br0, docker0 or virbr0, which are never Subyard's to guard.
nm_incus_bridge_names() {
  local inventory
  command -v incus >/dev/null 2>&1 || return 1
  inventory="$(incus network list --project default --format json 2>/dev/null)" || return 1
  python3 -I -c '
import json, re, sys
for network in json.loads(sys.stdin.read().strip() or "[]") or []:
    name = network.get("name", "")
    if network.get("managed") is True and network.get("type") == "bridge" and re.fullmatch(r"[A-Za-z0-9_.:-]+", name):
        print(name)
' <<< "$inventory" 2>/dev/null
}

# The current format, classified exactly as hostruntime.NetworkManagerGuardCurrent does and
# checked against tests/fixtures/nm-guard-formats: comments and blank lines, one
# [device-subyard] section and nothing but its match-device and managed keys, once each.
nm_guard_format_current() { # <conf>
  [ -f "$1" ] || return 1
  awk '
    { line = $0; gsub(/^[[:space:]]+|[[:space:]]+$/, "", line) }
    line == "" || line ~ /^#/ { next }
    line == "[device-subyard]" { sections++; next }
    line ~ /^\[/ { bad = 1; next }
    {
      if (index(line, "=") == 0 || sections != 1) { bad = 1; next }
      key = substr(line, 1, index(line, "=") - 1); gsub(/[[:space:]]+$/, "", key)
      if (key != "match-device" && key != "managed") bad = 1
      keys[key]++
    }
    END { exit !(!bad && sections == 1 && keys["match-device"] == 1 && keys["managed"] == 1) }
  ' "$1"
}

# A guard written by a released version: any guard file that is not the current format, or
# the legacy 99- file (a dangling symlink included).
nm_guard_released() { # <conf>
  nm_path_exists "$(nm_legacy_guard "$1")" && return 0
  nm_path_exists "$1" || return 1
  ! nm_guard_format_current "$1"
}

nm_guard_bridges_in() { # <conf>: bridges a current-format guard lists
  [ -r "$1" ] || return 0
  awk '
    /^[[:space:]]*match-device[[:space:]]*=/ {
      value = $0; sub(/^[^=]*=/, "", value)
      count = split(value, entries, /[,;]/)
      for (index_ = 1; index_ <= count; index_++) {
        entry = entries[index_]; gsub(/^[[:space:]]+|[[:space:]]+$/, "", entry)
        if (entry ~ /^interface-name:[A-Za-z0-9_.:-]+$/) { sub(/^interface-name:/, "", entry); print entry }
      }
    }
  ' "$1" | LC_ALL=C sort -u
}

nm_guard_render() { # <newline-separated bridges>
  local bridge spec="$NM_GUARD_OWN"
  while IFS= read -r bridge; do
    [ -n "$bridge" ] || continue
    [[ "$bridge" =~ ^[A-Za-z0-9_.:-]+$ ]] || return 1
    spec="$spec,interface-name:$bridge"
  done <<< "$(printf '%s\n' "$1" | sed '/^$/d' | LC_ALL=C sort -u)"
  printf '%s\n' \
    '# Subyard: keep yard veths, Incus tap devices and Incus bridges unmanaged.' \
    '[device-subyard]' \
    "match-device=$spec" \
    'managed=0'
}

nm_device_is_veth() { ip -d -o link show dev "$1" 2>/dev/null | grep -Eq ' veth( |@|$)'; }

nm_device_is_own() { # <device> <newline-separated bridges>
  grep -Fxq -- "$1" <<< "$2" && return 0
  [[ "$1" =~ ^tap.{8}$ ]] && return 0
  nm_device_is_veth "$1"
}

nm_guard_write_file() { # <path> <contents>: atomic; the temporary name never ends in .conf
  local temporary
  temporary="${1%/*}/.${1##*/}.$$.tmp"
  printf '%s\n' "$2" > "$temporary" && chmod 0644 "$temporary" && mv -f "$temporary" "$1" \
    || { rm -f "$temporary"; return 1; }
}

# NetworkManager's devices as DEVICE:TYPE:STATE lines in NM_DEVICES. A failed or empty read is
# an error, never an empty inventory: NetworkManager always lists at least the loopback.
nm_read_devices() {
  local devices
  devices="$(nmcli -t -f DEVICE,TYPE,STATE device 2>/dev/null)" || return 1
  [ -n "$devices" ] || return 1
  NM_DEVICES="$devices"
}

# The [keyfile] unmanaged-devices value of a printed configuration.
nm_keyfile_unmanaged() {
  awk '
    /^[[:space:]]*\[/ { section = $0; gsub(/[[:space:]]/, "", section); next }
    section == "[keyfile]" && /^[[:space:]]*unmanaged-devices[[:space:]]*=/ { value = $0; sub(/^[^=]*=/, "", value) }
    END { print value }
  '
}

# Whether a NetworkManager device list selects a device: 0 yes, 2 maybe, 1 no. Matching is
# order-independent and an except: match always wins. "*", exact interface names and type:
# entries (against nmcli's TYPE) are decided exactly; globs, MACs and drivers are "maybe". A
# list of only exceptions selects every device it does not except; an empty list selects none.
nm_list_selects() { # <list> <device> [type]
  local entry name negative kind positives=0 positive=1 excepted=1
  local -a entries
  IFS=',;' read -r -a entries <<< "$1"
  for entry in "${entries[@]}"; do
    entry="${entry#"${entry%%[![:space:]]*}"}"; entry="${entry%"${entry##*[![:space:]]}"}"
    [ -n "$entry" ] || continue
    negative=0
    case "$entry" in except:*) negative=1; entry="${entry#except:}" ;; *) positives=1 ;; esac
    kind=1
    case "$entry" in
      '*') kind=0 ;;
      type:*) [ -z "${3:-}" ] && kind=2 || { [ "${entry#type:}" = "$3" ] && kind=0; } ;;
      interface-name:=*) [ "${entry#interface-name:=}" = "$2" ] && kind=0 ;;
      interface-name:*)
        name="${entry#interface-name:}"; name="${name#\~}"
        case "$name" in *[*?]*) kind=2 ;; *) [ "$name" = "$2" ] && kind=0 ;; esac ;;
      *:*) kind=2 ;;
      *) case "$entry" in *[*?]*) kind=2 ;; *) [ "$entry" = "$2" ] && kind=0 ;; esac ;;
    esac
    if [ "$negative" = 1 ]; then
      [ "$kind" = 0 ] && return 1
      [ "$kind" = 2 ] && excepted=2
    elif [ "$kind" = 0 ]; then
      positive=0
    elif [ "$kind" = 2 ] && [ "$positive" != 0 ]; then
      positive=2
    fi
  done
  if [ "$positives" = 0 ]; then
    for entry in "${entries[@]}"; do
      [ -z "${entry//[[:space:]]/}" ] || { positive=0; break; }
    done
  fi
  [ "$positive" = 1 ] && return 1
  [ "$excepted" = 2 ] && return 2
  return "$positive"
}

nm_invocation() { systemctl show -p InvocationID --value NetworkManager 2>/dev/null || true; }

# The pending record: boot ID, NetworkManager invocation and kind. `unread` means NetworkManager
# has not started since the guard was first written; `lists` means a migrated guard waits for
# the host reboot. Written before the guard file and refreshed after it, so a NetworkManager
# start between the two never clears it early.
nm_guard_record_pending() { # <kind>
  local invocation
  invocation="$(nm_invocation)"
  { install -d -m 0755 "${POWER_NM_REBOOT_PENDING%/*}" \
    && nm_guard_write_file "$POWER_NM_REBOOT_PENDING" "$(power_nm_boot_id) ${invocation:-unknown} $1"; } 2>/dev/null
}

nm_guard_refresh_pending() { # <kind>: after the rename, bind the record to the reading invocation
  local boot invocation kind
  read -r boot invocation kind < "$POWER_NM_REBOOT_PENDING" 2>/dev/null || return 0
  [ "$invocation" = "$(nm_invocation)" ] || nm_guard_record_pending "$1" || true
}

# Planning: no released guard remains, the guard lists the bridge, and NetworkManager has read it.
nm_guard_file_current() { # <conf> [bridge]
  local conf="$1" bridge="${2:-}"
  nm_guard_released "$conf" && return 1
  if ! nm_path_exists "$conf"; then
    [ -z "$bridge" ] || ! power_nm_binary >/dev/null
    return
  fi
  local listed live
  listed="$(nm_guard_bridges_in "$conf")"
  if [ -n "$bridge" ]; then
    grep -Fxq -- "$bridge" <<< "$listed" || return 1
  fi
  # Every Incus-managed bridge belongs in the guard; a new one is drift for the apply.
  if command -v incus >/dev/null 2>&1 && live="$(nm_incus_bridge_names)"; then
    while IFS= read -r bridge; do
      [ -z "$bridge" ] || grep -Fxq -- "$bridge" <<< "$listed" || return 1
    done <<< "$live"
  fi
  [ "$(power_nm_guard_pending)" != unread ]
}

# Release existing own bridges NetworkManager still manages; a bridge Incus just created is
# never touched by NetworkManager before this. Each pin is verified.
nm_guard_pin_bridges() { # <newline-separated bridges>
  local device type state
  while IFS=: read -r device type state; do
    [ -n "$device" ] && [ "$state" != unmanaged ] || continue
    grep -Fxq -- "$device" <<< "$1" || continue
    nmcli device set "$device" managed no >/dev/null 2>&1 \
      || { power_fail "could not mark the yard bridge $device unmanaged in NetworkManager"; return 1; }
    state="$(nmcli -g GENERAL.STATE device show "$device" 2>/dev/null)" \
      || { power_fail "could not verify the yard bridge $device after marking it unmanaged"; return 1; }
    case "$state" in
      *unmanaged*) ok "NetworkManager no longer manages the yard bridge $device" ;;
      *) power_fail "NetworkManager still manages the yard bridge $device ($state)"; return 1 ;;
    esac
  done <<< "$NM_DEVICES"
}

# The checks every guard write shares: NetworkManager accepts the proposed configuration, it
# satisfies the guard rule, and no own veth or tap is managed or carries the default route.
# Refusals go to NM_GUARD_REFUSALS, one per line.
NM_GUARD_REFUSALS=''
nm_guard_preflight() { # <conf> <desired or empty for removal> <bridges> <active> <binary> <proposed dir>
  local conf="$1" desired="$2" bridges="$3" active="$4" binary="$5" proposed="$6" routes device type state
  local -a bridge_args=()
  NM_GUARD_REFUSALS='' NM_GUARD_PROPOSED=''
  while IFS= read -r device; do [ -z "$device" ] || bridge_args+=("$device"); done <<< "$bridges"
  mkdir -p "$proposed"
  [ ! -d "${conf%/*}" ] || cp -a "${conf%/*}/." "$proposed/"
  rm -f "$proposed/99-subyard-unmanaged.conf" "$proposed/${conf##*/}"
  [ -z "$desired" ] || printf '%s\n' "$desired" > "$proposed/${conf##*/}"
  if [ -n "$binary" ]; then
    if ! NM_GUARD_PROPOSED="$("$binary" --config-dir "$proposed" --print-config 2>/dev/null)"; then
      NM_GUARD_REFUSALS+="NetworkManager rejects the proposed configuration"$'\n'
      NM_GUARD_PROPOSED=''
    elif [ -n "$desired" ]; then
      while IFS= read -r device; do
        [ -z "$device" ] || NM_GUARD_REFUSALS+="proposed guard: $device"$'\n'
      done < <(printf '%s\n' "$NM_GUARD_PROPOSED" | power_nm_config_violations "${bridge_args[@]}")
    fi
  fi
  [ "$active" = 1 ] || return 0
  if ! power_nm_print_config >/dev/null 2>&1; then
    NM_GUARD_REFUSALS+="cannot read NetworkManager's effective configuration"$'\n'
    return 0
  fi
  if ! command -v nmcli >/dev/null 2>&1 || ! nm_read_devices; then
    NM_GUARD_REFUSALS+="cannot read NetworkManager's devices with nmcli"$'\n'
    return 0
  fi
  [ -n "$desired" ] || return 0
  routes="$(ip -4 route show default 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "dev") print $(i + 1) }')"
  while IFS=: read -r device type state; do
    [ -n "$device" ] && [ "$type" != loopback ] || continue
    nm_device_is_own "$device" "$bridges" || continue
    if [ "$state" != unmanaged ] && ! grep -Fxq -- "$device" <<< "$bridges"; then
      NM_GUARD_REFUSALS+="NetworkManager manages $device, which the guard covers; release it first: sudo nmcli device set $device managed no"$'\n'
    fi
    if grep -Fxq -- "$device" <<< "$routes"; then
      NM_GUARD_REFUSALS+="the host default route uses $device, which the guard covers"$'\n'
    fi
  done <<< "$NM_DEVICES"
}

# Write or remove the current guard for install, init, setup and teardown without touching
# NetworkManager's configuration in memory. When NetworkManager runs and has not read a guard,
# the file is written once and NM_GUARD_NEEDS_START=1 asks for a NetworkManager start before any
# yard starts. Otherwise own bridges are pinned unmanaged. POWER_NM_GUARD_COMMAND names the
# command to rerun.
nm_guard_apply() { # <conf> <write|remove> <newline-separated bridges>
  local conf="$1" mode="$2" bridges="$3" desired='' binary active=0 rc work device unread=0 pending
  NM_GUARD_NEEDS_START=0
  while IFS= read -r device; do
    [ -z "$device" ] && continue
    [[ "$device" =~ ^[A-Za-z0-9_.:-]+$ ]] || die "unsafe Incus bridge name in NetworkManager guard: $device"
  done <<< "$bridges"
  [ "$mode" = remove ] || desired="$(nm_guard_render "$bridges")"
  if power_nm_active; then active=1; else
    rc=$?; [ "$rc" -eq 1 ] || { power_fail "$POWER_ERROR"; return 1; }
  fi
  binary="$(power_nm_binary)" || binary=''
  if [ -z "$binary" ] && ! nm_path_exists "$conf"; then
    ok "NetworkManager is not installed — no guard needed"
    return 0
  fi
  pending="$(power_nm_guard_pending)"
  # NetworkManager has read no guard: none was on disk, or the last write is still unread.
  if [ "$active" = 1 ] && { ! nm_path_exists "$conf" || [ "$pending" = unread ]; }; then unread=1; fi
  work="$(mktemp -d)" || { power_fail "cannot create a private NetworkManager work directory"; return 1; }
  chmod 0700 "$work"
  nm_guard_preflight "$conf" "$desired" "$bridges" "$active" "$binary" "$work/proposed"
  rm -rf "$work"
  if [ -n "$NM_GUARD_REFUSALS" ]; then
    power_fail "NetworkManager guard unchanged: $(power_join <<< "$NM_GUARD_REFUSALS") — then rerun $POWER_NM_GUARD_COMMAND"
    return 1
  fi
  if [ "$mode" = remove ]; then
    if nm_path_exists "$conf"; then
      rm -f "$conf" || { power_fail "could not remove $conf"; return 1; }
      ok "removed the NetworkManager guard ($conf)"
    else
      ok "NetworkManager guard already absent"
    fi
    [ "$pending" != unread ] || rm -f "$POWER_NM_REBOOT_PENDING"
    return 0
  fi
  if [ "$unread" = 1 ]; then
    nm_guard_record_pending unread \
      || { power_fail "could not record that NetworkManager has not read the guard; nothing was changed"; return 1; }
  fi
  if [ ! -f "$conf" ] || [ "$(cat "$conf")" != "$desired" ]; then
    install -d -m 0755 "${conf%/*}" || { power_fail "could not create ${conf%/*}"; return 1; }
    nm_guard_write_file "$conf" "$desired" || { power_fail "could not write $conf"; return 1; }
    ok "NetworkManager guard written for veths, Incus tap devices and Incus bridges ($conf)"
  else
    ok "NetworkManager guard already current ($conf)"
  fi
  if [ "$unread" = 1 ]; then
    nm_guard_refresh_pending unread
    NM_GUARD_NEEDS_START=1
    return 0
  fi
  [ "$active" = 1 ] || return 0
  nm_guard_pin_bridges "$bridges" || return 1
}

nm_guard_start_message() {
  printf '%s\n' "NetworkManager has not read the Subyard guard yet; it never reloads NetworkManager itself. Reboot the host or restart NetworkManager ('sudo systemctl restart NetworkManager'), then rerun $POWER_NM_GUARD_COMMAND"
}

# Install, init and setup: the yard's bridge plus every Incus-managed bridge.
nm_unmanaged_guard() { # [bridge] [conf]
  local bridge="${1:-incusbr0}" conf="${2:-$NM_GUARD_CONF}" live='' bridges
  [[ "$bridge" =~ ^[A-Za-z0-9_.:-]+$ ]] || die "unsafe Incus bridge name: $bridge"
  if command -v incus >/dev/null 2>&1 && live="$(nm_incus_bridge_names)"; then :; else
    command -v incus >/dev/null 2>&1 \
      && warn "could not list Incus-managed bridges — keeping the bridges the current guard lists"
    live="$(nm_guard_bridges_in "$conf")"
    nm_guard_format_current "$conf" || live=''
  fi
  bridges="$(printf '%s\n%s\n' "$bridge" "$live" | sed '/^$/d' | LC_ALL=C sort -u)"
  if nm_guard_released "$conf"; then
    nm_unmanaged_guard_migrate "$conf" "$bridge"
    return
  fi
  nm_guard_apply "$conf" write "$bridges" || die "$POWER_ERROR"
  [ "$NM_GUARD_NEEDS_START" = 0 ] || die "$(nm_guard_start_message)"
}

# The one migration step for a guard written by a released version (replacing [main] and
# [keyfile] lists, or the legacy 99- file). NetworkManager keeps the released section in memory,
# which already covers veths, taps and the old bridges, so nothing is reloaded: the rewrite
# records `lists` and waits for the host reboot. It refuses when NetworkManager provably manages
# a device the restored administrator lists mark unmanaged; lists it cannot decide (globs, MACs,
# drivers) migrate with a notice instead.
nm_unmanaged_guard_migrate() { # [conf] [bridge]
  local conf="${1:-$NM_GUARD_CONF}" bridges desired binary active=0 rc work list device type state
  local refusals='' undecided=''
  if ! nm_guard_released "$conf"; then
    ok "NetworkManager guard has no released form to migrate"
    return 0
  fi
  bridges="$(nm_incus_bridge_names)" \
    || die "could not list Incus-managed bridges to migrate the released NetworkManager guard; rerun $POWER_NM_GUARD_COMMAND once incus network list works"
  bridges="$(printf '%s\n%s\n' "${2:-}" "$bridges" | sed '/^$/d' | LC_ALL=C sort -u)"
  desired="$(nm_guard_render "$bridges")" || die "unsafe Incus bridge name in NetworkManager guard"
  if power_nm_active; then active=1; else
    rc=$?; [ "$rc" -eq 1 ] || die "$POWER_ERROR"
  fi
  binary="$(power_nm_binary)" || binary=''
  work="$(mktemp -d)" || die "cannot create a private NetworkManager work directory"
  chmod 0700 "$work"
  nm_guard_preflight "$conf" "$desired" "$bridges" "$active" "$binary" "$work/proposed"
  rm -rf "$work"
  refusals="$NM_GUARD_REFUSALS"
  if [ "$active" = 1 ] && [ -n "$NM_GUARD_PROPOSED" ] && [ -n "$NM_DEVICES" ]; then
    list="$(printf '%s\n' "$NM_GUARD_PROPOSED" | nm_keyfile_unmanaged)"
    while IFS=: read -r device type state; do
      [ -n "$device" ] && [ "$type" != loopback ] && [ "$state" != unmanaged ] || continue
      if nm_list_selects "$list" "$device" "$type"; then rc=0; else rc=$?; fi
      case "$rc" in
        0) refusals+="NetworkManager manages $device, which the administrator's unmanaged-devices list marks unmanaged; the released Subyard guard replaced that list, and restoring it would make NetworkManager deconfigure $device. Hand $device back first with 'sudo nmcli device set $device managed no' (or the administrator's own way)"$'\n' ;;
        2) undecided+="$device"$'\n' ;;
      esac
    done <<< "$NM_DEVICES"
  fi
  if [ -n "$refusals" ]; then
    die "NetworkManager guard unchanged: $(power_join <<< "$refusals") — then rerun $POWER_NM_GUARD_COMMAND"
  fi
  if [ "$active" = 1 ]; then
    nm_guard_record_pending lists \
      || die "could not record the pending host reboot for the NetworkManager guard; nothing was changed"
  fi
  install -d -m 0755 "${conf%/*}" || die "could not create ${conf%/*}"
  nm_guard_write_file "$conf" "$desired" || die "could not write $conf"
  rm -f "${conf%/*}/99-subyard-unmanaged.conf"
  [ "$active" = 0 ] || nm_guard_refresh_pending lists
  if [ -n "$undecided" ]; then
    warn "Subyard cannot decide whether the administrator's unmanaged-devices list ($list) marks these devices NetworkManager manages: $(power_join_comma <<< "$undecided"). Do not reload or restart NetworkManager before the reboot."
  fi
  ok "NetworkManager guard rewritten ($conf) without touching NetworkManager — reboot the host once to apply it safely"
}

# Teardown: keep the guard for surviving Incus bridges, remove it after the last one.
nm_unmanaged_guard_reconcile() { # [conf]
  local conf="${1:-$NM_GUARD_CONF}" bridges
  if ! bridges="$(nm_incus_bridge_names)"; then
    warn "could not inventory surviving Incus bridges — keeping the NetworkManager guard fail-closed"
    return 1
  fi
  if [ -n "$bridges" ]; then
    nm_guard_apply "$conf" write "$bridges" || { warn "$POWER_ERROR"; return 1; }
  else
    nm_guard_apply "$conf" remove '' || { warn "$POWER_ERROR"; return 1; }
  fi
}

ufw_yard_rules_present() {
  local bridge="${1:?ufw_yard_rules_present needs a bridge}"
  local rules="${SUBYARD_UFW_RULES_FILE:-/etc/ufw/user.rules}"
  [ -r "$rules" ] || return 1
  awk -v bridge="$bridge" '
    $1 == "###" && $2 == "tuple" && $3 == "###" {
      action = $4; dport = $6; iface = $10
      if (action == "allow" && dport == "67" && iface == "in_" bridge) dhcp = 1
      if (action == "allow" && dport == "53" && iface == "in_" bridge) dns = 1
      if (action == "route:allow" && iface == "in_" bridge) route_in = 1
      if (action == "route:allow" && iface == "out_" bridge) route_out = 1
    }
    END { exit !(dhcp && dns && route_in && route_out) }
  ' "$rules"
}

ufw_rules_set_probe_access() {
  local mode="${1:?ufw_rules_set_probe_access needs enable or disable}"
  local rules="${SUBYARD_UFW_RULES_FILE:-/etc/ufw/user.rules}" group
  [ -e "$rules" ] || return 1
  case "$mode" in
    enable) group=incus-admin; getent group "$group" >/dev/null 2>&1 || return 1 ;;
    disable) group=root ;;
    *) return 2 ;;
  esac
  chgrp "$group" "$rules" && chmod 0640 "$rules"
}

# The caller must first verify that this exact approved bridge is absent.
ufw_yard_rules_remove() {
  local bridge="${1:?bridge is required}" rules="${SUBYARD_UFW_RULES_FILE:-/etc/ufw/user.rules}" status
  command -v ufw >/dev/null 2>&1 || return 0
  [[ "$bridge" =~ ^[A-Za-z0-9_.:-]+$ ]] || return 2
  incus network list --project default -f json | jq -e --arg bridge "$bridge" \
    'all(.[]; .name != $bridge)' >/dev/null || return 1
  ufw --force delete allow in on "$bridge" to any port 67 proto udp || return
  ufw --force delete allow in on "$bridge" to any port 53 || return
  ufw --force route delete allow in on "$bridge" || return
  ufw --force route delete allow out on "$bridge" || return
  [ -e "$rules" ] || return 0
  [ -r "$rules" ] || return 1
  # Verify every removed tuple, including partially installed policies.
  awk -v bridge="$bridge" '
    $1 == "###" && $2 == "tuple" && $3 == "###" {
      if (($4 == "allow" && ($6 == "67" || $6 == "53") && $10 == "in_" bridge) ||
          ($4 == "route:allow" && ($10 == "in_" bridge || $10 == "out_" bridge))) found = 1
    }
    END { exit found }
  ' "$rules" || return 1
  # Other surviving yard policies still need the shared read-only probe grant.
  if awk '
    $1 == "###" && $2 == "tuple" && $3 == "###" &&
    (($4 == "allow" && ($6 == "67" || $6 == "53") && $10 ~ /^in_/) ||
     ($4 == "route:allow" && $10 ~ /^(in_|out_)/)) { found = 1 }
    END { exit !found }
  ' "$rules"; then
    return 0
  else
    status=$?
    [ "$status" = 1 ] || return "$status"
    ufw_rules_set_probe_access disable || return
  fi
}

# The global IPv4 address of the guest's default-route interface. A guest whose
# agent is ready may still be configuring its network after a restart, so wait
# within one bounded deadline: each probe gets at most the remaining budget, an
# answer after the deadline is not accepted, and the failure names the elapsed
# time and the last observation.
incus_instance_primary_ipv4() {
  local project="${1:?incus_instance_primary_ipv4 needs a project}"
  local instance="${2:?incus_instance_primary_ipv4 needs an instance}"
  local wait_timeout="${SUBYARD_VM_IPV4_TIMEOUT:-60}" started deadline remaining output address observed='no observation'
  [[ "$wait_timeout" =~ ^[1-9][0-9]*$ ]] || return 2
  started=$SECONDS
  deadline=$((started + wait_timeout))
  while remaining=$((deadline - SECONDS)); [ "$remaining" -gt 0 ]; do
    [ "$remaining" -le 5 ] || remaining=5
    if output="$(timeout --foreground "$remaining" incus exec "$instance" --project "$project" -- sh -eu -c '
      interface="$(ip -4 route show default | awk "NR == 1 { for (i=1; i<=NF; i++) if (\$i == \"dev\") { print \$(i+1); exit } }")"
      printf "route %s\n" "${interface:-none}"
      [ -z "$interface" ] || ip -4 -o address show dev "$interface" scope global
    ' 2>/dev/null)"; then
      address="$(printf '%s\n' "$output" | awk '$1 == "route" { next } { split($4, value, "/"); print value[1]; exit }')"
      if [ -n "$address" ] && [ "$SECONDS" -le "$deadline" ]; then
        printf '%s\n' "$address"
        return 0
      fi
      case "$(printf '%s\n' "$output" | awk '$1 == "route" { print $2; exit }')" in
        '' | none) observed='no default IPv4 route' ;;
        *) observed="default route on $(printf '%s\n' "$output" | awk '$1 == "route" { print $2; exit }') without a global IPv4 address" ;;
      esac
      [ -z "$address" ] || observed='the guest answered after the deadline'
    else
      observed='the guest network query failed'
    fi
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 1
  done
  printf 'VM %s/%s has no primary IPv4 address after %ss: %s\n' "$project" "$instance" "$((SECONDS - started))" "$observed" >&2
  return 1
}

zabbly_suite() {
  command -v apt-get >/dev/null 2>&1 || return 1
  [ -r /etc/os-release ] || return 1
  local suite
  suite="$(. /etc/os-release && printf '%s' "${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}")"
  [ -n "$suite" ] || return 1
  printf '%s\n' "$suite"
}

add_zabbly_lts_repo() {
  local key=/etc/apt/keyrings/zabbly.asc
  local source=/etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources
  local suite arch want
  suite="$(zabbly_suite)" || { warn "no apt codename in /etc/os-release — can't add the Zabbly repo"; return 1; }
  command -v curl >/dev/null 2>&1 || { warn "curl not found — can't fetch the Zabbly signing key"; return 1; }
  arch="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
  install -d -m 0755 /etc/apt/keyrings
  if [ ! -s "$key" ]; then
    subyard_download_https_atomic https://pkgs.zabbly.com/key.asc "$key" 0644 root root \
      || { warn "failed to download the Zabbly signing key"; return 1; }
    ok "installed Zabbly signing key ($key)"
  else
    ok "Zabbly signing key already present"
  fi
  want="Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/lts-6.0
Suites: $suite
Components: main
Architectures: $arch
Signed-By: $key"
  if [ ! -f "$source" ] || ! printf '%s\n' "$want" | cmp -s - "$source"; then
    printf '%s\n' "$want" > "$source"
    ok "added Zabbly LTS-6.0 apt source ($source; suite=$suite)"
  else
    ok "Zabbly LTS-6.0 apt source already present (suite=$suite)"
  fi
  local pin=/etc/apt/preferences.d/zabbly-incus.pref
  local want_pin="Package: incus incus-client
Pin: origin pkgs.zabbly.com
Pin-Priority: 700"
  if [ ! -f "$pin" ] || ! printf '%s\n' "$want_pin" | cmp -s - "$pin"; then
    printf '%s\n' "$want_pin" > "$pin"
    ok "pinned Zabbly incus packages (priority 700) ($pin)"
  fi
  info "apt-get update (Zabbly)"
  apt-get update -qq || { warn "apt-get update failed after adding the Zabbly repo"; return 1; }
}

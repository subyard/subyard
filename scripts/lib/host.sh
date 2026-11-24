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

# Native teardown guard of the running engine; the dispatcher may be pinned below /proc.
teardown_guard() { "${SUBYARD_DISPATCHER_PATH:-${BASH_SOURCE[0]%/*}/../../bin/yard}" _teardown-guard "$@"; }

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
      teardown_guard remove-artifact "$state_dir" || return
    else
      rm -rf -- "$state_dir"
    fi
    printf 'removed\n'
  fi
}

incus_preflight() {
  command -v incus >/dev/null 2>&1 || die "incus not found — run 'yard init' first"
  incus info >/dev/null 2>&1 && return 0
  if id -nG "$(id -un)" 2>/dev/null | tr ' ' '\n' | grep -x >/dev/null incus-admin; then
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
# tap devices and Incus-managed bridges. NetworkManager reads it when it starts and applies it to
# a device when the device appears. Subyard never reloads or restarts NetworkManager: a reload
# would also apply every other file on disk, including administrator edits nobody has applied.
# Whether NetworkManager read the guard is proven by order (scripts/lib-power.sh
# power_nm_guard_state), and no yard starts until it has. The only state-changing
# NetworkManager call marks an Incus-managed bridge unmanaged. Released versions instead
# replaced [main] no-auto-default and [keyfile] unmanaged-devices.
NM_GUARD_CONF="$POWER_NM_GUARD_CONF"
NM_DEVICES=''
# Set by nm_guard_inventory: known (NM_INVENTORY holds every Incus-managed bridge, possibly
# none) or unknown (incus is installed but its network list failed).
NM_INVENTORY_STATE='' NM_INVENTORY=''

nm_path_exists() { [ -e "$1" ] || [ -L "$1" ]; }
nm_legacy_guard() { printf '%s/99-subyard-unmanaged.conf\n' "${1%/*}"; } # <conf>

# Incus-managed bridges only, from this run: the default-project list also shows host bridges
# such as an administrator's br0, docker0 or virbr0, which are never Subyard's. Without incus no
# Incus bridge can exist, so the empty inventory is authoritative.
nm_guard_inventory() {
  local listing
  NM_INVENTORY='' NM_INVENTORY_STATE=known
  command -v incus >/dev/null 2>&1 || return 0
  if ! listing="$(incus network list --project default --format json 2>/dev/null)" \
    || ! NM_INVENTORY="$(python3 -I -c '
import json, re, sys
for network in json.loads(sys.stdin.read().strip() or "[]") or []:
    name = network.get("name", "")
    if network.get("managed") is True and network.get("type") == "bridge" and re.fullmatch(r"[A-Za-z0-9_.:-]+", name):
        print(name)
' <<< "$listing" 2>/dev/null)"; then
    NM_INVENTORY='' NM_INVENTORY_STATE=unknown
    return 1
  fi
  NM_INVENTORY="$(printf '%s\n' "$NM_INVENTORY" | sed '/^$/d' | LC_ALL=C sort -u)"
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
  [ -f "$1" ] || return 0
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

nm_device_is_veth() { ip -d -o link show dev "$1" 2>/dev/null | grep -E >/dev/null ' veth( |@|$)'; }

nm_device_is_own() { # <device> <newline-separated bridges>
  grep -Fxq -- "$1" <<< "$2" && return 0
  [[ "$1" =~ ^tap.{8}$ ]] && return 0
  nm_device_is_veth "$1"
}

nm_guard_write_file() { # <path> <contents>: atomic; the temporary name never ends in .conf
  local temporary
  temporary="${1%/*}/.${1##*/}.$$.tmp"
  # Durable: the bytes reach the disk before the rename, and the rename before any later step.
  # -T: the destination is a file name, never a directory a link points to.
  printf '%s\n' "$2" > "$temporary" && chmod 0644 "$temporary" && sync -- "$temporary" \
    && mv -fT -- "$temporary" "$1" && sync -- "${1%/*}" \
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

# An administrator device list from a printed configuration.
nm_config_list() { # <section> <key>
  awk -v wanted_section="[$1]" -v wanted_key="$2" '
    /^[[:space:]]*\[/ { section = $0; gsub(/[[:space:]]/, "", section); next }
    section == wanted_section && $0 ~ "^[[:space:]]*" wanted_key "[[:space:]]*=" { value = $0; sub(/^[^=]*=/, "", value) }
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
        case "$name" in '*') kind=0 ;; *[*?]*) kind=2 ;; *) [ "$name" = "$2" ] && kind=0 ;; esac ;;
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

nm_guard_render() { # <newline-separated bridges>
  local bridge entry spec=''
  for entry in $POWER_NM_GUARD_STATIC; do spec="${spec:+$spec,}$entry"; done
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

nm_guard_static_listed() { # <conf>: a current-format guard lists every static entry
  local entry entries
  entries="$(sed -n 's/^[[:space:]]*match-device[[:space:]]*=//p' "$1" 2>/dev/null | tr ',;' '\n\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
  for entry in $POWER_NM_GUARD_STATIC; do grep -Fxq -- "$entry" <<< "$entries" || return 1; done
}

# Planning: no released guard, a known inventory whose bridges the guard all lists, every static
# entry, and an active NetworkManager that has read it (a migration waiting for its reboot is no
# drift init could repair). A legacy decimal-comma record still needs canonicalization.
nm_guard_file_current() { # <conf> [bridge]
  local conf="$1" bridge="${2:-}" listed rc record='' recorded_file
  nm_guard_released "$conf" && return 1
  nm_guard_inventory || return 1
  if ! nm_path_exists "$conf"; then
    [ -z "$bridge" ] || ! power_nm_binary >/dev/null
    return
  fi
  nm_guard_static_listed "$conf" || return 1
  listed="$(nm_guard_bridges_in "$conf")"
  [ -z "$bridge" ] || grep -Fxq -- "$bridge" <<< "$NM_INVENTORY" || return 1
  while IFS= read -r bridge; do
    [ -z "$bridge" ] || grep -Fxq -- "$bridge" <<< "$listed" || return 1
  done <<< "$NM_INVENTORY"
  { IFS= read -r record < "$POWER_NM_GUARD_STATE"; } 2>/dev/null || true
  read -r _ recorded_file _ <<< "$record"
  case "$recorded_file" in *,*) return 1 ;; esac
  if power_nm_active; then :; else rc=$?; [ "$rc" -eq 1 ] && return 0; return 1; fi
  case "$(power_nm_guard_state "$conf")" in loaded|loaded-reboot|unread-migration) return 0 ;; *) return 1 ;; esac
}

# Release Incus-managed bridges NetworkManager still manages; a bridge Incus just created is never
# touched by NetworkManager before this. Each pin is verified.
nm_guard_pin_bridges() {
  local device type state
  nm_read_devices || { power_fail "cannot read NetworkManager's devices with nmcli"; return 1; }
  while IFS=: read -r device type state; do
    [ -n "$device" ] && [ "$type" = bridge ] && [ "$state" != unmanaged ] || continue
    grep -Fxq -- "$device" <<< "$NM_INVENTORY" || continue
    nmcli device set "$device" managed no >/dev/null 2>&1 \
      || { power_fail "could not mark the Incus bridge $device unmanaged in NetworkManager"; return 1; }
    state="$(nmcli -g GENERAL.STATE device show "$device" 2>/dev/null)" \
      || { power_fail "could not verify the Incus bridge $device after marking it unmanaged"; return 1; }
    case "$state" in
      *unmanaged*) ok "NetworkManager no longer manages the Incus bridge $device" ;;
      *) power_fail "NetworkManager still manages the Incus bridge $device ($state)"; return 1 ;;
    esac
  done <<< "$NM_DEVICES"
}

# Checks every guard change shares: NetworkManager accepts the proposed configuration, it
# satisfies the guard rule, and no own veth or tap is managed or carries the default route.
# Refusals go to NM_GUARD_REFUSALS, one per line; NM_GUARD_PROPOSED holds the proposal.
NM_GUARD_REFUSALS='' NM_GUARD_PROPOSED=''
nm_guard_preflight() { # <conf> <desired or empty for removal> <bridges> <active> <binary>
  local conf="$1" desired="$2" bridges="$3" active="$4" binary="$5" proposed violations routes device type state
  local -a bridge_args=()
  NM_GUARD_REFUSALS='' NM_GUARD_PROPOSED=''
  [ -n "$binary" ] || return 0
  while IFS= read -r device; do [ -z "$device" ] || bridge_args+=("$device"); done <<< "$bridges"
  proposed="$(mktemp -d)" || { NM_GUARD_REFUSALS="cannot create a private NetworkManager work directory"$'\n'; return 0; }
  chmod 0700 "$proposed"
  if [ -d "${conf%/*}" ] && ! cp -a "${conf%/*}/." "$proposed/"; then
    NM_GUARD_REFUSALS+="cannot copy NetworkManager configuration for preflight"$'\n'
    rm -rf "$proposed"
    proposed=''
  fi
  if [ -n "$proposed" ]; then
    rm -f "$proposed/99-subyard-unmanaged.conf" "$proposed/${conf##*/}"
    [ -z "$desired" ] || printf '%s\n' "$desired" > "$proposed/${conf##*/}"
    if ! NM_GUARD_PROPOSED="$("$binary" --config-dir "$proposed" --print-config 2>/dev/null)"; then
      NM_GUARD_REFUSALS+="NetworkManager rejects the proposed configuration"$'\n'
      NM_GUARD_PROPOSED=''
    elif [ -n "$desired" ]; then
      if violations="$(power_nm_config_violations "${bridge_args[@]}" <<< "$NM_GUARD_PROPOSED")"; then
        while IFS= read -r device; do
          [ -z "$device" ] || NM_GUARD_REFUSALS+="proposed guard: $device"$'\n'
        done <<< "$violations"
      else
        NM_GUARD_REFUSALS+="cannot validate the proposed NetworkManager configuration with the Subyard engine"$'\n'
      fi
    fi
    rm -rf "$proposed"
  fi
  [ "$active" = 1 ] || return 0
  # Each read is independent: one failure never hides another finding.
  power_nm_print_config >/dev/null 2>&1 \
    || NM_GUARD_REFUSALS+="cannot read NetworkManager's effective configuration"$'\n'
  NM_DEVICES=''
  if ! command -v nmcli >/dev/null 2>&1 || ! nm_read_devices; then
    NM_GUARD_REFUSALS+="cannot read NetworkManager's devices with nmcli"$'\n'
  fi
  [ -n "$desired" ] || return 0
  routes="$(ip -4 route show default 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "dev") print $(i + 1) }')"
  while IFS=: read -r device; do
    [ -n "$device" ] || continue
    nm_device_is_own "$device" "$bridges" \
      && NM_GUARD_REFUSALS+="the host default route uses $device, which the guard covers"$'\n'
  done <<< "$routes"
  while IFS=: read -r device type state; do
    [ -n "$device" ] && [ "$type" != loopback ] || continue
    nm_device_is_own "$device" "$bridges" || continue
    if [ "$state" != unmanaged ] && ! grep -Fxq -- "$device" <<< "$bridges"; then
      NM_GUARD_REFUSALS+="NetworkManager manages $device, which the guard covers; release it first: sudo nmcli device set $device managed no"$'\n'
    fi
  done <<< "$NM_DEVICES"
}

# Released guards replaced the administrator's device lists. Restoring either list can
# remove a device's connectivity at the next start. Only unmanaged-devices proves a
# takeover; no-auto-default does not affect explicit profiles, so it only gets notices.
NM_GUARD_NOTICES=''
nm_guard_hijack_check() {
  local device type state rc key section list undecided
  NM_GUARD_NOTICES=''
  [ -n "$NM_GUARD_PROPOSED" ] && [ -n "$NM_DEVICES" ] || return 0
  for key in unmanaged-devices no-auto-default; do
    section=keyfile
    [ "$key" != no-auto-default ] || section=main
    list="$(printf '%s\n' "$NM_GUARD_PROPOSED" | nm_config_list "$section" "$key")"
    undecided=''
    while IFS=: read -r device type state; do
      [ -n "$device" ] && [ "$type" != loopback ] && [ "$state" != unmanaged ] || continue
      if nm_list_selects "$list" "$device" "$type"; then rc=0; else rc=$?; fi
      if [ "$key" = no-auto-default ] && [ "$rc" = 0 ]; then
        NM_GUARD_NOTICES+="the administrator's no-auto-default list selects $device; if $device relies on an automatic connection, create an explicit profile before the reboot"$'\n'
        continue
      fi
      case "$rc" in
        0) NM_GUARD_REFUSALS+="NetworkManager manages $device, which the administrator's $key list marks unmanaged; the released Subyard guard replaced that list, and restoring it could leave $device without connectivity after reboot. Hand $device back first with 'sudo nmcli device set $device managed no' (or the administrator's own way), or change the administrator's own $key list if NetworkManager should keep the device"$'\n' ;;
        2) undecided+="$device"$'\n' ;;
      esac
    done <<< "$NM_DEVICES"
    [ -z "$undecided" ] || NM_GUARD_NOTICES+="Subyard cannot decide whether the administrator's $key list ($list) marks these devices NetworkManager manages: $(power_join_comma <<< "$undecided"). Do not reload or restart NetworkManager before the reboot."$'\n'
  done
}

nm_guard_render_notices() {
  local notice
  while IFS= read -r notice; do
    [ -z "$notice" ] || warn "$notice"
  done <<< "$NM_GUARD_NOTICES"
}

# Decision 1: a released guard counts as held when every up Incus-managed bridge it names is
# unmanaged in NetworkManager (at least one, and only from known inventories and device reads).
nm_guard_released_names() { # <conf>: exact interface names a released or legacy guard lists
  local file
  for file in "$1" "$(nm_legacy_guard "$1")"; do
    [ -f "$file" ] && sed -n 's/^[[:space:]]*\(unmanaged-devices\|match-device\)[[:space:]]*[+]\{0,1\}=//p' "$file"
  done | tr ',;' '\n\n' | sed -n 's/^[[:space:]]*interface-name:\([A-Za-z0-9_.:-]*\)[[:space:]]*$/\1/p' | LC_ALL=C sort -u
}

nm_guard_released_evidence() { # <conf>: any failed read means no evidence
  local name names link named=0
  [ "$NM_INVENTORY_STATE" = known ] && [ -n "$NM_DEVICES" ] || return 1
  names="$(nm_guard_released_names "$1")"
  while IFS= read -r name; do
    [ -n "$name" ] && grep -Fxq -- "$name" <<< "$NM_INVENTORY" || continue
    link="$(ip -o link show dev "$name" 2>/dev/null)" || return 1
    grep -q '[<,]UP[,>]' <<< "$link" || continue
    named=1
    awk -F: -v d="$name" '$1 == d && $3 == "unmanaged" { found = 1 } END { exit !found }' <<< "$NM_DEVICES" || return 1
  done <<< "$names"
  [ "$named" = 1 ]
}

# Record which guard NetworkManager holds, bound to the guard file's identity. A failed write
# removes the record (it then reads as unbound) and leaves its one next step in
# NM_GUARD_RECORD_ERROR for nm_guard_post_publish.
NM_GUARD_RECORD_ERROR=''
nm_guard_record() { # <conf> <hash> <held|unread> <init|migration> <boot>
  local file
  file="$(power_nm_file_id "$1")" || file=''
  if [ -z "$5" ]; then
    NM_GUARD_RECORD_ERROR="cannot read the boot ID (/proc/sys/kernel/random/boot_id), so Subyard cannot record which NetworkManager guard is loaded and no yard starts; fix the boot ID, then rerun $POWER_NM_GUARD_COMMAND"
  elif [ -n "$file" ] && { install -d -m 0755 "${POWER_NM_GUARD_STATE%/*}" \
    && nm_guard_write_file "$POWER_NM_GUARD_STATE" "$5 $file $2 $3 $4"; } 2>/dev/null; then
    return 0
  else
    NM_GUARD_RECORD_ERROR="could not write the NetworkManager guard record $POWER_NM_GUARD_STATE, so no yard starts; make ${POWER_NM_GUARD_STATE%/*} a writable persistent directory, then rerun $POWER_NM_GUARD_COMMAND"
  fi
  rm -f "$POWER_NM_GUARD_STATE" 2>/dev/null || true
}

# `held` when NetworkManager was explicitly inactive at the first sample (or is not installed)
# and still is now; anything else, a failed read included, is `unread`.
nm_guard_inactive_state() { # <active at the first sample> <binary>
  local rc=0
  [ "$1" = 0 ] || { printf 'unread\n'; return; }
  [ -n "$2" ] || { printf 'held\n'; return; }
  power_nm_active || rc=$?
  if [ "$rc" = 1 ]; then printf 'held\n'; else printf 'unread\n'; fi
}

# Publish one guard change; the file is rewritten only when its bytes differ.
# - Equal bytes keep a record bound to this file; without that binding what NetworkManager holds
#   is unknown, so the file is recorded `unread` for this boot.
# - A bridge-only rewrite of a bound guard keeps boot and state and refreshes the identity:
#   bridge names need no reboot, each is pinned.
# - Any other change, a repair included, first removes the record durably, then renames the guard,
#   then records `unread` for this boot. `held` needs NetworkManager explicitly inactive at both
#   samples, or a migration with evidence that NetworkManager holds the released guard.
nm_guard_publish() { # <conf> <desired, or empty to remove> <init|migration> <active> <binary> [evidence: 1|0]
  local conf="$1" desired="$2" kind="$3" active="$4" binary="$5" evidence="${6:-0}" legacy hash before record bound=0 disk guard_identity
  local old_boot old_file old_hash old_state old_kind state=unread
  NM_GUARD_RECORD_ERROR=''
  legacy="$(nm_legacy_guard "$conf")"
  if [ -z "$desired" ]; then
    rm -f "$conf" "$legacy" "$POWER_NM_GUARD_STATE" || { power_fail "could not remove $conf"; return 1; }
    ok "removed the NetworkManager guard ($conf)"
    return 0
  fi
  hash="$(power_nm_guard_text_hash "$desired")"
  before="$(power_nm_guard_state "$conf")"
  # A change while a migrated guard waits for its reboot keeps waiting for that reboot.
  [ "$before" != unread-migration ] || kind=migration
  record=''
  { IFS= read -r record < "$POWER_NM_GUARD_STATE"; } 2>/dev/null || true
  read -r old_boot old_file old_hash old_state old_kind _ <<< "$record"
  # Bound: the record names this very file and its static content (any boot).
  # Anything but a regular file in place of the guard is never bound, never read, and is replaced
  # by a regular file below.
  disk='' guard_identity=''
  if guard_identity="$(power_nm_file_id "$conf")"; then disk="$(power_nm_guard_static_hash "$conf")" || disk=''; fi
  [ "$(power_nm_guard_state_rule "$record" any "$guard_identity" "$disk")" = unbound ] \
    || nm_path_exists "$legacy" || bound=1
  if [ -n "$guard_identity" ] && ! nm_path_exists "$legacy" && [ "$(cat -- "$conf")" = "$desired" ]; then
    ok "NetworkManager guard already current ($conf)"
    if [ "$bound" = 1 ]; then
      # Keep the valid legacy record on write failure. Persist the identity already matched;
      # unlike a guard rewrite, canonicalization must never invalidate the existing binding.
      if [ "$old_file" != "$guard_identity" ]; then
        nm_guard_write_file "$POWER_NM_GUARD_STATE" "$old_boot $guard_identity $hash $old_state $old_kind" \
          || { power_fail "could not canonicalize the NetworkManager guard record $POWER_NM_GUARD_STATE; rerun $POWER_NM_GUARD_COMMAND"; return 1; }
      fi
      return 0
    fi
    nm_guard_record "$conf" "$hash" "$(nm_guard_inactive_state "$active" "$binary")" "$kind" "$(power_nm_boot_id)"
    return 0
  fi
  install -d -m 0755 "${conf%/*}" || { power_fail "could not create ${conf%/*}"; return 1; }
  if [ "$bound" = 1 ] && [ "$old_hash" = "$hash" ]; then
    nm_guard_write_file "$conf" "$desired" || { power_fail "could not write $conf"; return 1; }
    ok "NetworkManager guard written for veths, Incus tap devices and Incus bridges ($conf)"
    nm_guard_record "$conf" "$hash" "$old_state" "$old_kind" "$old_boot"
    return 0
  fi
  if [ -d "${POWER_NM_GUARD_STATE%/*}" ] \
    && ! { rm -f "$POWER_NM_GUARD_STATE" && sync -- "${POWER_NM_GUARD_STATE%/*}"; } 2>/dev/null; then
    power_fail "could not invalidate the NetworkManager guard record $POWER_NM_GUARD_STATE; nothing was changed"
    return 1
  fi
  nm_guard_write_file "$conf" "$desired" || { power_fail "could not write $conf"; return 1; }
  rm -f "$legacy"
  ok "NetworkManager guard written for veths, Incus tap devices and Incus bridges ($conf)"
  if [ "$active" = 1 ]; then
    [ "$kind" = migration ] && [ "$evidence" = 1 ] && state=held
  else
    state="$(nm_guard_inactive_state "$active" "$binary")"
  fi
  nm_guard_record "$conf" "$hash" "$state" "$kind" "$(power_nm_boot_id)"
}

nm_guard_nm_state() { # sets NM_GUARD_ACTIVE and NM_GUARD_BINARY, or fails
  local rc
  NM_GUARD_ACTIVE=0 NM_GUARD_BINARY="$(power_nm_binary)" || NM_GUARD_BINARY=''
  if power_nm_active; then NM_GUARD_ACTIVE=1; else
    rc=$?; [ "$rc" -eq 1 ] || { power_fail "$POWER_ERROR"; return 1; }
  fi
  if [ "$NM_GUARD_ACTIVE" = 1 ] && [ -z "$NM_GUARD_BINARY" ]; then
    power_fail "NetworkManager is active but its binary is unavailable at a known path; restore the binary before rerunning $POWER_NM_GUARD_COMMAND"
    return 1
  fi
}

nm_guard_unread_message() {
  printf '%s\n' "NetworkManager has not read the Subyard guard yet; Subyard never reloads or restarts NetworkManager. Reboot the host once, then rerun $POWER_NM_GUARD_COMMAND"
}

# After a publish, judged on the record rather than the first sample (NetworkManager may have
# started meanwhile). Returns 0 when NetworkManager holds the guard (a migration waiting for its
# reboot included), 1 after stopping, 2 when it does not and the caller continues (update,
# teardown): starts refuse until the one next step is done.
nm_guard_post_publish() { # <conf> <stop: 1|0>
  if [ -n "$NM_GUARD_RECORD_ERROR" ]; then
    [ "$2" = 0 ] || { power_fail "$NM_GUARD_RECORD_ERROR"; return 1; }
    warn "$NM_GUARD_RECORD_ERROR"
    return 2
  fi
  case "$(power_nm_guard_state "$1")" in
    loaded|loaded-reboot) return 0 ;;
    unbound)
      [ "$2" = 0 ] || { power_fail "the Subyard guard on disk is not bound to a record; rerun $POWER_NM_GUARD_COMMAND"; return 1; }
      return 2 ;;
    *)
      [ "$2" = 0 ] || { power_fail "$(nm_guard_unread_message)"; return 1; }
      return 2 ;;
  esac
}

# Write or remove the current guard (init, setup, teardown): preflight, publish, then stop while
# NetworkManager has not read it, or pin the Incus bridges it manages.
nm_guard_apply_current() { # <conf> <desired or empty> <bridges> <stop: 1|0> <pin: 1|0>
  local conf="$1" desired="$2" bridges="$3"
  if [ -z "$NM_GUARD_BINARY" ] && ! nm_path_exists "$conf" && ! nm_path_exists "$(nm_legacy_guard "$conf")"; then
    ok "NetworkManager is not installed — no guard needed"
    return 0
  fi
  nm_guard_preflight "$conf" "$desired" "$bridges" "$NM_GUARD_ACTIVE" "$NM_GUARD_BINARY"
  if [ -n "$NM_GUARD_REFUSALS" ]; then
    power_fail "NetworkManager guard unchanged: $(power_join <<< "$NM_GUARD_REFUSALS") — then rerun $POWER_NM_GUARD_COMMAND"
    return 1
  fi
  nm_guard_publish "$conf" "$desired" init "$NM_GUARD_ACTIVE" "$NM_GUARD_BINARY" || return 1
  [ -n "$desired" ] && [ -n "$NM_GUARD_BINARY" ] || return 0
  local rc=0
  nm_guard_post_publish "$conf" "$4" || rc=$?
  [ "$rc" != 1 ] || return 1
  [ "$rc" = 0 ] || return 0
  [ "$NM_GUARD_ACTIVE" = 1 ] && [ "$5" = 1 ] || return 0
  nm_guard_pin_bridges
}

# Migrate a guard written by a released version: refuse a provable takeover of a device the
# restored administrator lists mark unmanaged, give a notice for lists Subyard cannot decide, and
# publish without touching NetworkManager. Until NetworkManager reads it, starts wait for the
# host reboot; init (stop 1) stops there before anything creates devices.
nm_guard_migrate_with() { # <conf> <bridges> <stop: 1|0>
  local conf="$1" desired rc=0
  desired="$(nm_guard_render "$2")" || { power_fail "unsafe Incus bridge name in NetworkManager guard"; return 1; }
  nm_guard_preflight "$conf" "$desired" "$2" "$NM_GUARD_ACTIVE" "$NM_GUARD_BINARY"
  [ "$NM_GUARD_ACTIVE" = 0 ] || nm_guard_hijack_check
  if [ -n "$NM_GUARD_REFUSALS" ]; then
    power_fail "NetworkManager guard unchanged: $(power_join <<< "$NM_GUARD_REFUSALS") — then rerun $POWER_NM_GUARD_COMMAND"
    return 1
  fi
  local evidence=0
  [ "$NM_GUARD_ACTIVE" = 0 ] || ! nm_guard_released "$conf" || ! nm_guard_released_evidence "$conf" || evidence=1
  nm_guard_publish "$conf" "$desired" migration "$NM_GUARD_ACTIVE" "$NM_GUARD_BINARY" "$evidence" || return 1
  nm_guard_render_notices
  [ -n "$NM_GUARD_BINARY" ] || return 0
  nm_guard_post_publish "$conf" "$3" || rc=$?
  case "$rc" in
    0) [ "$NM_GUARD_ACTIVE" = 0 ] || ok "NetworkManager guard migrated without touching NetworkManager — reboot the host once to apply it safely" ;;
    1) return 1 ;;
    *) [ -n "$NM_GUARD_RECORD_ERROR" ] \
      || warn "NetworkManager guard migrated without touching NetworkManager; nothing shows NetworkManager holds an adequate guard, so no yard starts until the host reboots once" ;;
  esac
}

# The early install call, before Incus is installed or initialized: the static section and the
# Incus bridges already listed (never new ones), no pins.
nm_unmanaged_guard_early() { # [conf]
  local conf="${1:-$NM_GUARD_CONF}" bridges=''
  nm_guard_nm_state || die "$POWER_ERROR"
  if nm_guard_released "$conf"; then
    # Without an inventory only released names NetworkManager reports as unmanaged bridges stay
    # listed: keeping them unmanaged changes nothing NetworkManager does now. An administrator's
    # bridge NetworkManager runs (a non-Incus INCUS_BRIDGE such as br0) is dropped. When
    # NetworkManager is inactive or nmcli cannot be read, every released name stays, as it was.
    # Ownership is unknowable here; a later call with an inventory decides.
    if nm_guard_inventory; then bridges="$NM_INVENTORY"
    elif [ "$NM_GUARD_ACTIVE" = 1 ] && command -v nmcli >/dev/null 2>&1 && nm_read_devices; then
      local name
      while IFS= read -r name; do
        [ -n "$name" ] && awk -F: -v d="$name" '$1 == d && $2 == "bridge" && $3 == "unmanaged" { found = 1 } END { exit !found }' <<< "$NM_DEVICES" \
          && bridges+="$name"$'\n'
      done < <(nm_guard_released_names "$conf")
    else
      bridges="$(nm_guard_released_names "$conf")"
    fi
    nm_guard_migrate_with "$conf" "$bridges" 1 || die "$POWER_ERROR"
    return 0
  fi
  if nm_guard_inventory; then
    local listed name
    listed="$(nm_guard_bridges_in "$conf")"
    while IFS= read -r name; do
      [ -n "$name" ] && grep -Fxq -- "$name" <<< "$NM_INVENTORY" && bridges+="$name"$'\n'
    done <<< "$listed"
  else
    bridges="$(nm_guard_bridges_in "$conf")"
  fi
  nm_guard_apply_current "$conf" "$(nm_guard_render "$bridges")" "$bridges" 1 0 || die "$POWER_ERROR"
}

# Init and setup, after Incus exists: every Incus-managed bridge from this run's inventory, which
# must include the yard's bridge; pins the ones NetworkManager manages.
nm_unmanaged_guard() { # <bridge> [conf]
  local bridge="${1:?nm_unmanaged_guard needs the yard bridge}" conf="${2:-$NM_GUARD_CONF}"
  [[ "$bridge" =~ ^[A-Za-z0-9_.:-]+$ ]] || die "unsafe Incus bridge name: $bridge"
  nm_guard_nm_state || die "$POWER_ERROR"
  nm_guard_inventory || die "could not list Incus-managed bridges for the NetworkManager guard; nothing was changed — rerun $POWER_NM_GUARD_COMMAND once incus network list works"
  grep -Fxq -- "$bridge" <<< "$NM_INVENTORY" \
    || die "the yard bridge $bridge is not an Incus-managed bridge; Subyard guards and pins only Incus bridges — set INCUS_BRIDGE to an Incus bridge"
  if nm_guard_released "$conf"; then
    nm_guard_migrate_with "$conf" "$NM_INVENTORY" 1 || die "$POWER_ERROR"
    return 0
  fi
  nm_guard_apply_current "$conf" "$(nm_guard_render "$NM_INVENTORY")" "$NM_INVENTORY" 1 1 || die "$POWER_ERROR"
}

# Update: migrate a released or legacy guard. Without Incus only the static section is written.
nm_unmanaged_guard_migrate() { # [conf]
  local conf="${1:-$NM_GUARD_CONF}"
  if ! nm_guard_released "$conf"; then
    ok "NetworkManager guard has no released form to migrate"
    return 0
  fi
  nm_guard_nm_state || die "$POWER_ERROR"
  nm_guard_inventory || die "could not list Incus-managed bridges to migrate the released NetworkManager guard; nothing was changed — rerun $POWER_NM_GUARD_COMMAND once incus network list works"
  nm_guard_migrate_with "$conf" "$NM_INVENTORY" 0 || die "$POWER_ERROR"
}

# Before release recreation: bind an unrecorded guard without stopping a yard,
# pinning a device or reloading NetworkManager. The caller checks start safety.
nm_unmanaged_guard_prepare_recreation() {
  nm_unmanaged_guard_migrate "$NM_GUARD_CONF"
  nm_guard_nm_state || { power_fail "$POWER_ERROR"; return 1; }
  nm_guard_inventory || { power_fail "could not list Incus-managed bridges for recreation"; return 1; }
  nm_guard_apply_current "$NM_GUARD_CONF" "$(nm_guard_render "$NM_INVENTORY")" "$NM_INVENTORY" 0 0
}

# Teardown: keep the guard for surviving Incus bridges, remove it after the last one. A released
# guard goes through the migration's refusal; any refusal warns and keeps the guard.
nm_unmanaged_guard_reconcile() { # [conf]
  local conf="${1:-$NM_GUARD_CONF}"
  nm_guard_nm_state || { warn "$POWER_ERROR"; return 1; }
  if ! nm_guard_inventory; then
    warn "could not inventory surviving Incus bridges — keeping the NetworkManager guard fail-closed"
    return 1
  fi
  if nm_guard_released "$conf"; then
    if [ -z "$NM_INVENTORY" ]; then
      nm_guard_preflight "$conf" '' '' "$NM_GUARD_ACTIVE" "$NM_GUARD_BINARY"
      [ "$NM_GUARD_ACTIVE" = 0 ] || nm_guard_hijack_check
      if [ -n "$NM_GUARD_REFUSALS" ]; then
        warn "kept the NetworkManager guard: $(power_join <<< "$NM_GUARD_REFUSALS")"
        return 1
      fi
      nm_guard_publish "$conf" '' migration "$NM_GUARD_ACTIVE" "$NM_GUARD_BINARY" || return 1
      nm_guard_render_notices
      return
    fi
    nm_guard_migrate_with "$conf" "$NM_INVENTORY" 0 || { warn "kept the NetworkManager guard: $POWER_ERROR"; return 1; }
    return 0
  fi
  if [ -z "$NM_INVENTORY" ]; then
    nm_guard_apply_current "$conf" '' '' 0 0 || { warn "kept the NetworkManager guard: $POWER_ERROR"; return 1; }
    return 0
  fi
  nm_guard_apply_current "$conf" "$(nm_guard_render "$NM_INVENTORY")" "$NM_INVENTORY" 0 0 \
    || { warn "kept the NetworkManager guard: $POWER_ERROR"; return 1; }
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
      interface="$(ip -4 route show default | awk "NR == 1 { for (i=1; i<=NF; i++) if (\$i == \"dev\") { print \$(i+1); break } }")"
      printf "route %s\n" "${interface:-none}"
      [ -z "$interface" ] || ip -4 -o address show dev "$interface" scope global
    ' 2>/dev/null)"; then
      address="$(printf '%s\n' "$output" | awk '$1 == "route" { next } !found { split($4, value, "/"); print value[1]; found=1 }')"
      if [ -n "$address" ] && [ "$SECONDS" -le "$deadline" ]; then
        printf '%s\n' "$address"
        return 0
      fi
      case "$(printf '%s\n' "$output" | awk '!found && $1 == "route" { print $2; found=1 }')" in
        '' | none) observed='no default IPv4 route' ;;
        *) observed="default route on $(printf '%s\n' "$output" | awk '!found && $1 == "route" { print $2; found=1 }') without a global IPv4 address" ;;
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

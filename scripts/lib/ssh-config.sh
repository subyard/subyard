#!/usr/bin/env bash
# ssh-config.sh — atomic operator-owned OpenSSH config updates.

[ -n "${SUBYARD_SSH_CONFIG_SOURCED:-}" ] && return 0
SUBYARD_SSH_CONFIG_SOURCED=1

ssh_transport_identity_error() {
  printf 'unsafe Subyard SSH transport identity: %s\n' "$*" >&2
  return 1
}

ssh_config_quote() {
  local value="${1-}"
  case "$value" in *$'\n'* | *$'\r'*) return 1 ;; esac
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//\%/%%}"
  printf '"%s"' "$value"
}

ssh_yard_snippet_name() {
  local yard_name="${1-}"
  if [ -z "$yard_name" ] || [ "$yard_name" = default ]; then
    printf 'subyard.config\n'
  else
    printf 'subyard-%s.config\n' "$yard_name"
  fi
}

ssh_transport_identity_prepare() {
  local data_home="${1:?ssh_transport_identity_prepare needs the Subyard data home}"
  local keydir identity public uid private_line public_line tempdir lockfile lock_fd

  mkdir -p -- "$data_home" \
    || { ssh_transport_identity_error "cannot create data home"; return 1; }
  data_home="$(cd -P -- "$data_home" && pwd)" \
    || { ssh_transport_identity_error "cannot resolve data home"; return 1; }
  keydir="$data_home/ssh"
  identity="$keydir/id_ed25519"
  public="$identity.pub"
  uid="$(id -u)"

  if [ ! -e "$keydir" ] && [ ! -L "$keydir" ]; then
    mkdir -m 0700 -- "$keydir" \
      || { [ -d "$keydir" ] && [ ! -L "$keydir" ]; } \
      || { ssh_transport_identity_error "cannot create $keydir"; return 1; }
  fi
  [ -d "$keydir" ] && [ ! -L "$keydir" ] \
    || { ssh_transport_identity_error "$keydir must be a real directory"; return 1; }
  [ "$(stat -c %u "$keydir" 2>/dev/null)" = "$uid" ] \
    || { ssh_transport_identity_error "$keydir must be owned by the operator"; return 1; }
  [ "$(stat -c %a "$keydir" 2>/dev/null)" = 700 ] \
    || { ssh_transport_identity_error "$keydir must have mode 0700"; return 1; }

  lockfile="$keydir/.identity.lock"
  if [ ! -e "$lockfile" ] && [ ! -L "$lockfile" ]; then
    (umask 077; set -C; : > "$lockfile") 2>/dev/null || true
  fi
  [ -f "$lockfile" ] && [ ! -L "$lockfile" ] \
    || { ssh_transport_identity_error "key creation lock must be a regular non-symlink file"; return 1; }
  [ "$(stat -c %u "$lockfile" 2>/dev/null)" = "$uid" ] \
    && [ "$(stat -c %a "$lockfile" 2>/dev/null)" = 600 ] \
    || { ssh_transport_identity_error "key creation lock must be operator-owned mode 0600"; return 1; }
  exec {lock_fd}>>"$lockfile" \
    || { ssh_transport_identity_error "cannot open the key creation lock"; return 1; }
  flock "$lock_fd" \
    || { exec {lock_fd}>&-; ssh_transport_identity_error "cannot acquire the key creation lock"; return 1; }

  if { [ -e "$identity" ] || [ -L "$identity" ]; } \
    || { [ -e "$public" ] || [ -L "$public" ]; }; then
    { [ -e "$identity" ] || [ -L "$identity" ]; } \
      && { [ -e "$public" ] || [ -L "$public" ]; } \
      || {
        exec {lock_fd}>&-
        ssh_transport_identity_error "canonical key pair is incomplete"
        return 1
      }
  else
    tempdir="$(mktemp -d "$keydir/.identity.XXXXXX")" \
      || { exec {lock_fd}>&-; ssh_transport_identity_error "cannot stage a new key pair"; return 1; }
    chmod 0700 "$tempdir" \
      || {
        rmdir -- "$tempdir" 2>/dev/null || true
        exec {lock_fd}>&-
        ssh_transport_identity_error "cannot secure staged key directory"
        return 1
      }
    if ! ssh-keygen -q -t ed25519 -N '' -C 'subyard-local-transport' \
      -f "$tempdir/id_ed25519"; then
      rm -f -- "$tempdir/id_ed25519" "$tempdir/id_ed25519.pub"
      rmdir -- "$tempdir" 2>/dev/null || true
      exec {lock_fd}>&-
      ssh_transport_identity_error "could not generate an Ed25519 key pair"
      return 1
    fi
    chmod 0600 "$tempdir/id_ed25519" \
      && chmod 0644 "$tempdir/id_ed25519.pub" \
      && mv -- "$tempdir/id_ed25519.pub" "$public" \
      && mv -- "$tempdir/id_ed25519" "$identity" \
      || {
        rm -f -- "$tempdir/id_ed25519" "$tempdir/id_ed25519.pub"
        [ -e "$identity" ] || rm -f -- "$public"
        rmdir -- "$tempdir" 2>/dev/null || true
        exec {lock_fd}>&-
        ssh_transport_identity_error "could not publish the canonical key pair"
        return 1
      }
    rmdir -- "$tempdir" \
      || {
        exec {lock_fd}>&-
        ssh_transport_identity_error "could not remove the key staging directory"
        return 1
      }
  fi
  exec {lock_fd}>&-

  [ -f "$identity" ] && [ ! -L "$identity" ] \
    || { ssh_transport_identity_error "private key must be a regular non-symlink file"; return 1; }
  [ -f "$public" ] && [ ! -L "$public" ] \
    || { ssh_transport_identity_error "public key must be a regular non-symlink file"; return 1; }
  [ "$(stat -c %u "$identity" 2>/dev/null)" = "$uid" ] \
    && [ "$(stat -c %u "$public" 2>/dev/null)" = "$uid" ] \
    || { ssh_transport_identity_error "key pair must be owned by the operator"; return 1; }
  [ "$(stat -c %a "$identity" 2>/dev/null)" = 600 ] \
    || { ssh_transport_identity_error "private key must have mode 0600"; return 1; }
  [ "$(stat -c %a "$public" 2>/dev/null)" = 644 ] \
    || { ssh_transport_identity_error "public key must have mode 0644"; return 1; }

  private_line="$(ssh-keygen -y -P '' -f "$identity" 2>/dev/null)" \
    || { ssh_transport_identity_error "private key must be an unencrypted Ed25519 key"; return 1; }
  private_line="$(awk 'NF >= 2 { print $1 " " $2; exit }' <<<"$private_line")"
  public_line="$(awk 'NF >= 2 { print $1 " " $2; exit }' "$public")"
  case "$private_line" in 'ssh-ed25519 '*) ;; *)
    ssh_transport_identity_error "private key must be Ed25519"; return 1 ;;
  esac
  case "$public_line" in 'ssh-ed25519 '*) ;; *)
    ssh_transport_identity_error "public key must be Ed25519"; return 1 ;;
  esac
  [ "$private_line" = "$public_line" ] \
    || { ssh_transport_identity_error "public key does not match the private key"; return 1; }

  printf '%s\n' "$identity"
}

ssh_config_prepend_once() (
  local config="${1:?ssh_config_prepend_once needs a config path}"
  local line="${2:?ssh_config_prepend_once needs a line}"
  local directory temp

  directory="$(dirname "$config")"
  (umask 077; : >> "$directory/.subyard-config.lock") || return 1
  [ -f "$directory/.subyard-config.lock" ] \
    && [ ! -L "$directory/.subyard-config.lock" ] || return 1
  chmod 0600 "$directory/.subyard-config.lock" || return 1
  exec 9>>"$directory/.subyard-config.lock" || return 1
  flock 9 || return 1
  [ -e "$config" ] || : > "$config"
  grep -qxF "$line" "$config" 2>/dev/null && return 0

  # A fixed config.tmp may be stale or root-owned after an interrupted legacy sudo run. A fresh
  # same-directory file avoids that collision and still gives us an atomic rename.
  temp="$(mktemp "$directory/.subyard-ssh-config.XXXXXX")" || return 1
  if ! { printf '%s\n' "$line"; cat "$config"; } > "$temp"; then
    rm -f -- "$temp"
    return 1
  fi
  if ! chmod 0600 "$temp" || ! mv -f -- "$temp" "$config"; then
    rm -f -- "$temp"
    return 1
  fi
)

ssh_config_remove_exact() (
  local config="${1:?ssh_config_remove_exact needs a config path}"
  local line="${2:?ssh_config_remove_exact needs a line}"
  local directory temp

  directory="$(dirname "$config")"
  (umask 077; : >> "$directory/.subyard-config.lock") || return 1
  [ -f "$directory/.subyard-config.lock" ] \
    && [ ! -L "$directory/.subyard-config.lock" ] || return 1
  chmod 0600 "$directory/.subyard-config.lock" || return 1
  exec 9>>"$directory/.subyard-config.lock" || return 1
  flock 9 || return 1
  [ -f "$config" ] && [ ! -L "$config" ] || return 1
  grep -qxF "$line" "$config" 2>/dev/null || return 0
  temp="$(mktemp "$directory/.subyard-ssh-config.XXXXXX")" || return 1
  if ! grep -vxF "$line" "$config" > "$temp"; then
    [ ! -s "$temp" ] || { rm -f -- "$temp"; return 1; }
  fi
  chmod 0600 "$temp" && mv -f -- "$temp" "$config" \
    || { rm -f -- "$temp"; return 1; }
)

ssh_known_host_replace() (
  local known_hosts="${1:?ssh_known_host_replace needs a file}"
  local endpoint="${2:?ssh_known_host_replace needs an endpoint}"
  local public_key="${3:?ssh_known_host_replace needs a public key}"
  local directory temp type blob _rest uid

  read -r type blob _rest <<<"$public_key"
  [ "$type" = ssh-ed25519 ] && [[ "$blob" =~ ^[A-Za-z0-9+/=]+$ ]] || return 1
  directory="$(dirname "$known_hosts")"
  uid="$(id -u)"
  install -d -m 0700 "$directory" || return 1
  (umask 077; : >> "$directory/.subyard-known-hosts.lock") || return 1
  [ -f "$directory/.subyard-known-hosts.lock" ] \
    && [ ! -L "$directory/.subyard-known-hosts.lock" ] || return 1
  chmod 0600 "$directory/.subyard-known-hosts.lock" || return 1
  [ "$(stat -c %u "$directory/.subyard-known-hosts.lock" 2>/dev/null)" = "$uid" ] \
    && [ "$(stat -c %a "$directory/.subyard-known-hosts.lock" 2>/dev/null)" = 600 ] \
    || return 1
  exec 9>>"$directory/.subyard-known-hosts.lock" || return 1
  flock 9 || return 1
  temp="$(mktemp "$directory/.subyard-known-hosts.XXXXXX")" || return 1
  if [ -e "$known_hosts" ]; then
    cp -- "$known_hosts" "$temp" || { rm -f -- "$temp"; return 1; }
  fi
  ssh-keygen -R "$endpoint" -f "$temp" >/dev/null 2>&1 \
    || { rm -f -- "$temp" "$temp.old"; return 1; }
  rm -f -- "$temp.old"
  printf '%s %s %s\n' "$endpoint" "$type" "$blob" >> "$temp" \
    || { rm -f -- "$temp"; return 1; }
  chmod 0600 "$temp" && mv -f -- "$temp" "$known_hosts" \
    || { rm -f -- "$temp"; return 1; }
)

ssh_known_host_remove() (
  local known_hosts="${1:?ssh_known_host_remove needs a file}"
  local endpoint="${2:?ssh_known_host_remove needs an endpoint}"
  local owner="${3:-}" group="${4:-}" directory temp

  [ -f "$known_hosts" ] && [ ! -L "$known_hosts" ] || return 1
  directory="$(dirname "$known_hosts")"
  (umask 077; : >> "$directory/.subyard-known-hosts.lock") || return 1
  [ -f "$directory/.subyard-known-hosts.lock" ] \
    && [ ! -L "$directory/.subyard-known-hosts.lock" ] || return 1
  if [ -n "$owner" ] || [ -n "$group" ]; then
    [ -n "$owner" ] && [ -n "$group" ] || return 1
    chown "$owner:$group" "$directory/.subyard-known-hosts.lock" || return 1
  fi
  chmod 0600 "$directory/.subyard-known-hosts.lock" || return 1
  exec 9>>"$directory/.subyard-known-hosts.lock" || return 1
  flock 9 || return 1
  temp="$(mktemp "$directory/.subyard-known-hosts.XXXXXX")" || return 1
  cp -- "$known_hosts" "$temp" || { rm -f -- "$temp"; return 1; }
  ssh-keygen -R "$endpoint" -f "$temp" >/dev/null 2>&1 \
    || { rm -f -- "$temp" "$temp.old"; return 1; }
  rm -f -- "$temp.old"
  chmod 0600 "$temp" || { rm -f -- "$temp"; return 1; }
  if [ -n "$owner" ] || [ -n "$group" ]; then
    [ -n "$owner" ] && [ -n "$group" ] \
      || { rm -f -- "$temp"; return 1; }
    chown "$owner:$group" "$temp" || { rm -f -- "$temp"; return 1; }
  fi
  mv -f -- "$temp" "$known_hosts" || { rm -f -- "$temp"; return 1; }
)

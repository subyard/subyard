#!/usr/bin/env bash
# Root provisioner for the inner VM backend.
set -euo pipefail

if ! declare -F subyard_download_https_atomic >/dev/null; then
  _subyard_e2e_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)"
  # shellcheck source=scripts/lib/download.sh
  . "$_subyard_e2e_lib_dir/download.sh"
  unset _subyard_e2e_lib_dir
fi

run_with_progress() {
  local label="$1" ticker rc started=$SECONDS
  shift
  printf '  [ .. ] %s\n' "$label"
  (
    while sleep 10; do
      printf '  [ .. ] %s (still working, %ss elapsed)\n' "$label" "$((SECONDS - started))"
    done
  ) &
  ticker=$!
  if "$@"; then rc=0; else rc=$?; fi
  kill "$ticker" 2>/dev/null || true
  wait "$ticker" 2>/dev/null || true
  return "$rc"
}

inner_incus() {
  # This provisioner is executed with `bash -s`, so its stdin is the script body itself. Incus
  # create commands accept YAML on stdin; never let a child consume the remaining shell program.
  incus "$@" </dev/null
}

inner_apparmor_dropin() {
  printf '%s\n' "${SUBYARD_INNER_INCUS_APPARMOR_DROPIN:-/etc/systemd/system/incus.service.d/subyard-nested-e2e.conf}"
}

wait_inner_incus_active() {
  local _
  for _ in $(seq 1 30); do
    systemctl is-active --quiet incus.service && return 0
    sleep 1
  done
  return 1
}

reconcile_inner_incus_service() {
  local action="$1" attempt
  for attempt in 1 2 3; do
    if systemctl "$action" incus.service; then
      wait_inner_incus_active && return 0
    elif wait_inner_incus_active; then
      printf '  [warn] inner Incus recovered through systemd automatic restart\n' >&2
      return 0
    fi
    [ "$attempt" -lt 3 ] || break
    printf '  [warn] inner Incus %s failed; retrying start (%s/3)\n' \
      "$action" "$((attempt + 1))" >&2
    systemctl reset-failed incus.service >/dev/null 2>&1 || true
    action=start
  done
  systemctl --no-pager --full status incus.service >&2 || true
  journalctl -u incus.service -b --no-pager -n 120 >&2 || true
  return 1
}

reconcile_inner_apparmor_compat() {
  local dropin temp changed=0
  dropin="$(inner_apparmor_dropin)"
  install -d -m 0755 "$(dirname "$dropin")"
  temp="$(mktemp)"
  printf '%s\n' \
    '# Managed by Subyard: the outer yard profile remains the L0 security boundary.' \
    '[Service]' \
    'Environment=INCUS_SECURITY_APPARMOR=false' \
    'TimeoutStartSec=45s' > "$temp"
  if ! cmp -s "$temp" "$dropin"; then
    install -m 0644 "$temp" "$dropin"
    changed=1
  fi
  rm -f "$temp"

  systemctl daemon-reload
  systemctl enable incus.service >/dev/null
  if [ "$changed" = 1 ] && systemctl is-active --quiet incus.service; then
    # Incus reads INCUS_SECURITY_APPARMOR only at daemon startup. QEMU processes are independent
    # of the daemon, so this does not stop an already-running VM during an idempotent init rerun.
    reconcile_inner_incus_service restart
  else
    reconcile_inner_incus_service start
  fi
}

restore_inner_apparmor_default() {
  local dropin
  dropin="$(inner_apparmor_dropin)"
  [ -e "$dropin" ] || return 0
  rm -f "$dropin"
  systemctl daemon-reload
  if systemctl is-active --quiet incus.service; then
    systemctl restart incus.service
  fi
}

remove_group_member() {
  local user="$1" group="$2"
  getent group "$group" >/dev/null 2>&1 || return 0
  id -nG "$user" 2>/dev/null | tr ' ' '\n' | grep -Fxq "$group" || return 0
  gpasswd -d "$user" "$group" >/dev/null
}

disable_agent_account() {
  local user="${E2E_AGENT_USER:-subyard-e2e-agent}" home="${E2E_AGENT_HOME:-/var/lib/subyard/e2e-agent}"
  local account
  for account in "$user" $(getent passwd | cut -d: -f1 | grep '^subyard-e2e-slot-' || true); do
    id -u "$account" >/dev/null 2>&1 || continue
    if command -v pkill >/dev/null 2>&1; then
      pkill -KILL -u "$account" >/dev/null 2>&1 || true
    fi
    userdel --remove "$account" >/dev/null 2>&1 || usermod --lock "$account" >/dev/null 2>&1 || true
  done
  rm -rf "$home" /var/lib/subyard/e2e-slots
  rm -f /usr/local/libexec/subyard/test-vms-authorized-key
}

disable_agent_sshd_policy() {
  local policy=/etc/ssh/sshd_config.d/90-subyard-e2e-agent.conf
  [ -e "$policy" ] || return 0
  rm -f "$policy"
  rm -f /etc/sudoers.d/subyard-test-vms-facade
  sshd -t
  systemctl reload ssh.service
}

reconcile_agent_sshd_policy() {
  cat > /usr/local/libexec/subyard/test-vms-authorized-key <<'EOF'
#!/bin/sh
set -eu
user="${1:-}"
key_type="${2:-}"
key_blob="${3:-}"
[ "$user" = "${E2E_AGENT_USER:-subyard-e2e-agent}" ] || exit 0
[ "$key_type" = ssh-ed25519 ] || exit 0
case "$key_blob" in '' | *[!A-Za-z0-9+/=]*) exit 0 ;; esac
printf 'restrict,command="sudo -n /usr/local/libexec/subyard/test-vms-inner _test-vms-facade" %s %s\n' \
  "$key_type" "$key_blob"
EOF
  chmod 0755 /usr/local/libexec/subyard/test-vms-authorized-key
  cat > /etc/sudoers.d/subyard-test-vms-facade <<'EOF'
Defaults:subyard-e2e-agent env_keep += "SSH_ORIGINAL_COMMAND"
subyard-e2e-agent ALL=(root) NOPASSWD: /usr/local/libexec/subyard/test-vms-inner _test-vms-facade
EOF
  chmod 0440 /etc/sudoers.d/subyard-test-vms-facade
  visudo -cf /etc/sudoers.d/subyard-test-vms-facade >/dev/null
  install -d -m 0755 /etc/ssh/sshd_config.d
  cat > /etc/ssh/sshd_config.d/90-subyard-e2e-agent.conf <<'EOF'
# Managed by Subyard; key authentication only.
PasswordAuthentication no
KbdInteractiveAuthentication no
Match User subyard-e2e-agent
    AuthorizedKeysFile none
    AuthorizedKeysCommand /usr/local/libexec/subyard/test-vms-authorized-key %u %t %k
    AuthorizedKeysCommandUser nobody
EOF
  chmod 0644 /etc/ssh/sshd_config.d/90-subyard-e2e-agent.conf
  sshd -t
  systemctl reload ssh.service
}

reconcile_agent_account() {
  local user="$E2E_AGENT_USER" home="$E2E_AGENT_HOME" primary group directory keys
  if ! id -u "$user" >/dev/null 2>&1; then
    useradd --system --create-home --home-dir "$home" --shell /bin/sh "$user"
  fi
  # Unlock key login with an invalid password hash; sshd disables password login.
  usermod --home "$home" --shell /bin/sh --password x "$user"
  primary="$(id -gn "$user")"
  while IFS= read -r group; do
    [ -n "$group" ] && [ "$group" != "$primary" ] || continue
    gpasswd -d "$user" "$group" >/dev/null
  done < <(id -nG "$user" | tr ' ' '\n')
  for directory in "$home" "$home/.ssh"; do
    if [ -e "$directory" ] || [ -L "$directory" ]; then
      [ -d "$directory" ] && [ ! -L "$directory" ] \
        || { printf 'agent account directory %s is unsafe\n' "$directory" >&2; return 1; }
    fi
  done
  install -d -m 0755 -o root -g root "$home"
  install -d -m 0750 -o root -g "$primary" "$home/.ssh"
  keys="$home/.ssh/authorized_keys"
  if [ -e "$keys" ] || [ -L "$keys" ]; then
    [ -f "$keys" ] && [ ! -L "$keys" ] &&
      [ "$(stat -c '%u:%h' "$keys")" = 0:1 ] \
      || { printf 'agent authorized_keys is unsafe\n' >&2; return 1; }
    chmod 0640 "$keys"
    chown root:"$primary" "$keys"
  else
    install -m 0640 -o root -g "$primary" /dev/null "$keys"
  fi
}

retire_agent_static_keys() {
  local user="$E2E_AGENT_USER" home="$E2E_AGENT_HOME" primary keys candidate
  id -u "$user" >/dev/null 2>&1 \
    || { printf 'agent account disappeared before key retirement\n' >&2; return 1; }
  primary="$(id -gn "$user")"
  keys="$home/.ssh/authorized_keys"
  [ -d "$home" ] && [ ! -L "$home" ] &&
    [ -d "$home/.ssh" ] && [ ! -L "$home/.ssh" ] &&
    [ -f "$keys" ] && [ ! -L "$keys" ] &&
    [ "$(stat -c '%U:%G:%a:%h' "$keys")" = "root:$primary:640:1" ] \
    || { printf 'agent authorized_keys changed before retirement\n' >&2; return 1; }
  candidate="$(mktemp "$home/.ssh/.authorized_keys.XXXXXX")"
  if ! chmod 0640 "$candidate" ||
     ! chown root:"$primary" "$candidate" ||
     ! mv -fT -- "$candidate" "$keys"; then
    rm -f -- "$candidate"
    return 1
  fi
}

slot_home_has_managed_marker() {
  local home="$1" marker="$1/.subyard-managed"
  [ -f "$marker" ] \
    && [ ! -L "$marker" ] \
    && [ "$(stat -c '%u:%g:%a' "$marker")" = 0:0:644 ] \
    && [ "$(cat "$marker" 2>/dev/null || true)" = test-vms-v1 ]
}

legacy_slot_home_is_recognizable() {
  local home="$1" ssh_dir="$1/.ssh" keys="$1/.ssh/authorized_keys"
  local ssh_gid keys_gid
  [ -d "$home" ] && [ ! -L "$home" ] \
    && [ "$(stat -c '%u:%g:%a' "$home")" = 0:0:755 ] \
    && [ -d "$ssh_dir" ] && [ ! -L "$ssh_dir" ] \
    && [ "$(stat -c '%u:%a' "$ssh_dir")" = 0:750 ] \
    && [ -f "$keys" ] && [ ! -L "$keys" ] && [ ! -s "$keys" ] \
    && [ "$(stat -c '%u:%a' "$keys")" = 0:640 ] \
    || return 1
  ssh_gid="$(stat -c '%g' "$ssh_dir")"
  keys_gid="$(stat -c '%g' "$keys")"
  [ "$ssh_gid" = "$keys_gid" ] && [ "$ssh_gid" -ne 0 ]
}

reconcile_slot_accounts() {
  local slot user home primary entry actual_home actual_shell created
  for slot in $(seq 1 "$E2E_VM_SLOT_COUNT"); do
    user="subyard-e2e-slot-$slot"
    home="/var/lib/subyard/e2e-slots/$slot"
    created=0
    if id -u "$user" >/dev/null 2>&1; then
      entry="$(getent passwd "$user")" \
        || { printf 'cannot inspect existing slot account %s\n' "$user" >&2; return 1; }
      IFS=: read -r _ _ _ _ _ actual_home actual_shell <<<"$entry"
      [ "$actual_home" = "$home" ] && [ "$actual_shell" = /bin/sh ] \
        || { printf 'existing slot account %s is not Subyard-shaped\n' "$user" >&2; return 1; }
      [ -e "$home" ] \
        || { printf 'existing slot account %s has no managed home\n' "$user" >&2; return 1; }
    else
      if [ -e "$home" ]; then
        if [ -e "$home/.subyard-managed" ] || [ -L "$home/.subyard-managed" ]; then
          slot_home_has_managed_marker "$home" \
            || { printf 'existing slot home %s is not Subyard-managed\n' "$home" >&2; return 1; }
        else
          legacy_slot_home_is_recognizable "$home" \
            || { printf 'existing slot home %s is not Subyard-managed\n' "$home" >&2; return 1; }
        fi
      fi
      useradd --system --create-home --home-dir "$home" --shell /bin/sh "$user"
      created=1
    fi
    if [ -e "$home/.subyard-managed" ] || [ -L "$home/.subyard-managed" ]; then
      slot_home_has_managed_marker "$home" \
        || { printf 'slot home %s has an unexpected managed marker\n' "$home" >&2; return 1; }
    elif [ "$created" = 0 ] && [ -e "$home" ]; then
      # Upgrade only the exact pre-marker layout created by older test-vms
      # releases. A merely colliding account or directory is never adopted.
      legacy_slot_home_is_recognizable "$home" \
        || { printf 'legacy slot home %s is not safely recognizable\n' "$home" >&2; return 1; }
    fi
    usermod --home "$home" --shell /bin/sh --password x "$user"
    primary="$(id -gn "$user")"
    install -d -m 0755 -o root -g root "$home"
    install -d -m 0750 -o root -g "$primary" "$home/.ssh"
    touch "$home/.ssh/authorized_keys"
    chmod 0640 "$home/.ssh/authorized_keys"
    chown root:"$primary" "$home/.ssh/authorized_keys"
    printf 'test-vms-v1\n' > "$home/.subyard-managed"
    chmod 0644 "$home/.subyard-managed"
    chown root:root "$home/.subyard-managed"
  done
}

reconcile_test_vm_state_directory() {
  if [ "$(id -u)" = 0 ]; then
    install -d -m 0700 -o root -g root "$E2E_VM_STATE_DIR"
  else
    install -d -m 0700 "$E2E_VM_STATE_DIR"
  fi
  # Five digits clear legacy setgid on the existing 2770 directory.
  chmod 00700 "$E2E_VM_STATE_DIR"
  if [ "$(id -u)" = 0 ]; then
    find "$E2E_VM_STATE_DIR" -mindepth 1 -maxdepth 1 -type f \
      -exec chown root:root -- {} + -exec chmod 0600 -- {} +
  else
    find "$E2E_VM_STATE_DIR" -mindepth 1 -maxdepth 1 -type f -exec chmod 0600 -- {} +
  fi
}

install_inner_firewall() {
  cat > /usr/local/libexec/subyard/test-vms-firewall <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  apply)
    nft delete table inet subyard_e2e >/dev/null 2>&1 || true
    nft -f - <<'RULES'
table inet subyard_e2e {
  chain input {
    type filter hook input priority -10; policy accept;
    iifname "e2e-vm-net-*" ct direction reply ct state established,related accept
    iifname "e2e-vm-net-*" udp dport { 53, 67 } accept
    iifname "e2e-vm-net-*" tcp dport 53 accept
    iifname "e2e-vm-net-*" drop
  }
  chain forward {
    type filter hook forward priority -10; policy accept;
    iifname "e2e-vm-net-*" oifname "e2e-vm-net-*" drop
  }
}
RULES
    ;;
  remove) nft delete table inet subyard_e2e >/dev/null 2>&1 || true ;;
  *) printf 'usage: test-vms-firewall apply|remove\n' >&2; exit 2 ;;
esac
EOF
  chmod 0755 /usr/local/libexec/subyard/test-vms-firewall
  cat > /etc/systemd/system/subyard-test-vms-firewall.service <<'EOF'
[Unit]
Description=Isolate disposable nested VMs from the privileged yard
After=network-online.target incus.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/libexec/subyard/test-vms-firewall apply
ExecStop=/usr/local/libexec/subyard/test-vms-firewall remove

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now subyard-test-vms-firewall.service
  /usr/local/libexec/subyard/test-vms-firewall apply
}

disable_inner_firewall() {
  systemctl disable --now subyard-test-vms-firewall.service >/dev/null 2>&1 || true
  if command -v nft >/dev/null 2>&1; then
    nft delete table inet subyard_e2e >/dev/null 2>&1 || true
  fi
}

reconcile_inner_incus() {
  local output

  # Build the three bootstrap resources independently so a network failure never strands the
  # daemon halfway through one monolithic `incus admin init`. Nested AppArmor can deny dnsmasq's
  # syslog socket; retry that one resource with a log inside Incus' own allowed state directory.
  install -d -m 0711 /srv/incus-e2e/storage
  if ! inner_incus storage show default --project default >/dev/null 2>&1; then
    inner_incus storage create default dir source=/srv/incus-e2e/storage --project default
  fi

  if ! inner_incus network show incusbr0 --project default >/dev/null 2>&1; then
    if ! output="$(inner_incus network create incusbr0 ipv4.address=auto ipv6.address=none \
      --project default 2>&1)"; then
      case "$output" in
        *"cannot open log"*)
          inner_incus network create incusbr0 ipv4.address=auto ipv6.address=none \
            raw.dnsmasq=log-facility=/var/lib/incus/networks/incusbr0/dnsmasq.log \
            --project default ;;
        *) printf '%s\n' "$output" >&2; return 1 ;;
      esac
    fi
  fi

  inner_incus profile show default --project default >/dev/null 2>&1 \
    || inner_incus profile create default --project default
  if ! inner_incus profile device list default --project default 2>/dev/null | grep -qx root; then
    inner_incus profile device add default root disk pool=default path=/ --project default
  fi
  if ! inner_incus profile device list default --project default 2>/dev/null | grep -qx eth0; then
    inner_incus profile device add default eth0 nic network=incusbr0 --project default
  fi
}

reconcile_inner_zabbly_repo() {
  local suite arch
  suite="$(. /etc/os-release && printf '%s' "${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}")"
  [ -n "$suite" ] || { printf 'cannot resolve apt suite for the Incus LTS repository\n' >&2; return 1; }
  arch="$(dpkg --print-architecture)"
  install -d -m 0755 /etc/apt/keyrings
  if [ ! -s /etc/apt/keyrings/zabbly.asc ]; then
    subyard_download_https_atomic \
      https://pkgs.zabbly.com/key.asc /etc/apt/keyrings/zabbly.asc 0644 root root
  fi
  cat > /etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources <<EOF
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/lts-6.0
Suites: $suite
Components: main
Architectures: $arch
Signed-By: /etc/apt/keyrings/zabbly.asc
EOF
  chmod 0644 /etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources
}

# Production streams this file into L1 with `bash -s`, where BASH_SOURCE has no element 0. Treat
# that as execution; return early only when a test explicitly sources the file from another script.
[ "${BASH_SOURCE[0]:-$0}" = "$0" ] || return 0

[ "$(id -u)" = 0 ] || { printf 'test-vms provision requires root inside the yard\n' >&2; exit 1; }
: "${NESTED_E2E_VMS:=0}"
: "${DEV_USER:=dev}"
: "${E2E_VM_IMAGE:=images:debian/13/cloud}"
: "${E2E_VM_CPU:=4}"
: "${E2E_VM_MEMORY:=4GiB}"
: "${E2E_VM_DISK:=20GiB}"
: "${E2E_VM_SLOT_COUNT:=2}"
: "${E2E_VM_BOOT_TIMEOUT:=300}"
: "${E2E_VM_STATE_DIR:=/var/lib/subyard/test-vms}"
: "${E2E_BROKER_SOURCE:=test-yard}"
: "${E2E_AGENT_USER:=subyard-e2e-agent}"
: "${E2E_AGENT_HOME:=/var/lib/subyard/e2e-agent}"
: "${E2E_AGENT_PUBLIC_KEY:=}"

case "$NESTED_E2E_VMS" in 0 | 1) ;; *) printf 'invalid NESTED_E2E_VMS\n' >&2; exit 1 ;; esac
[[ "$DEV_USER" =~ ^[a-z_][a-z0-9_-]*$ ]] || { printf 'invalid DEV_USER\n' >&2; exit 1; }
[[ "$E2E_VM_CPU" =~ ^[1-9][0-9]*$ ]] || { printf 'invalid E2E_VM_CPU\n' >&2; exit 1; }
[[ "$E2E_VM_MEMORY" =~ ^[1-9][0-9]*(MiB|GiB)$ ]] || { printf 'invalid E2E_VM_MEMORY\n' >&2; exit 1; }
[[ "$E2E_VM_DISK" =~ ^[1-9][0-9]*(MiB|GiB)$ ]] || { printf 'invalid E2E_VM_DISK\n' >&2; exit 1; }
case "$E2E_VM_DISK" in
  *GiB) e2e_disk_mib=$(( ${E2E_VM_DISK%GiB} * 1024 )) ;;
  *MiB) e2e_disk_mib=${E2E_VM_DISK%MiB} ;;
esac
[ "$e2e_disk_mib" -ge 10240 ] \
  || { printf 'E2E_VM_DISK must be at least 10GiB\n' >&2; exit 1; }
case "$E2E_VM_IMAGE" in '' | -* | *[!A-Za-z0-9._:/@+-]*) printf 'invalid E2E_VM_IMAGE\n' >&2; exit 1 ;; esac
[[ "$E2E_VM_SLOT_COUNT" =~ ^[1-9][0-9]*$ ]] \
  || { printf 'invalid E2E_VM_SLOT_COUNT\n' >&2; exit 1; }
[[ "$E2E_VM_BOOT_TIMEOUT" =~ ^[0-9]+$ ]] \
  && [ "$E2E_VM_BOOT_TIMEOUT" -ge 30 ] && [ "$E2E_VM_BOOT_TIMEOUT" -le 1800 ] \
  || { printf 'invalid E2E_VM_BOOT_TIMEOUT\n' >&2; exit 1; }
case "$E2E_VM_STATE_DIR" in /var/lib/subyard/*) ;; *) printf 'unsafe E2E_VM_STATE_DIR\n' >&2; exit 1 ;; esac
case "$E2E_BROKER_SOURCE" in
  ''|*$'\n'*|*$'\r'*|*$'\t'*) printf 'unsafe E2E_BROKER_SOURCE\n' >&2; exit 1 ;;
esac
[ "${#E2E_BROKER_SOURCE}" -le 96 ] || { printf 'unsafe E2E_BROKER_SOURCE\n' >&2; exit 1; }
case "$E2E_AGENT_USER" in '' | -* | *[!a-z0-9_-]*) printf 'invalid E2E_AGENT_USER\n' >&2; exit 1 ;; esac
case "$E2E_AGENT_HOME" in /var/lib/subyard/*) ;; *) printf 'unsafe E2E_AGENT_HOME\n' >&2; exit 1 ;; esac
if [ -n "$E2E_AGENT_PUBLIC_KEY" ]; then
  [[ "$E2E_AGENT_PUBLIC_KEY" != *$'\n'* && "$E2E_AGENT_PUBLIC_KEY" != *$'\r'* ]] \
    || { printf 'E2E_AGENT_PUBLIC_KEY must be one line\n' >&2; exit 1; }
  [[ "$E2E_AGENT_PUBLIC_KEY" =~ ^ssh-ed25519[[:space:]][A-Za-z0-9+/=]+([[:space:]].*)?$ ]] \
    || { printf 'E2E_AGENT_PUBLIC_KEY must be an Ed25519 public key\n' >&2; exit 1; }
  agent_key_check="$(mktemp)"
  read -r agent_key_type agent_key_blob _agent_key_comment <<<"$E2E_AGENT_PUBLIC_KEY"
  printf '%s %s\n' "$agent_key_type" "$agent_key_blob" > "$agent_key_check"
  if ! ssh-keygen -l -f "$agent_key_check" >/dev/null 2>&1; then
    rm -f "$agent_key_check"
    printf 'E2E_AGENT_PUBLIC_KEY is not a valid Ed25519 public key\n' >&2
    exit 1
  fi
  rm -f "$agent_key_check"
fi

install -d -m 0755 /etc/subyard
config_candidate="$(mktemp /etc/subyard/.test-vms.env.XXXXXX)"
trap 'rm -f -- "$config_candidate"' EXIT
{
  printf 'NESTED_E2E_VMS=%q\n' "$NESTED_E2E_VMS"
  printf 'DEV_USER=%q\n' "$DEV_USER"
  printf 'E2E_VM_IMAGE=%q\n' "$E2E_VM_IMAGE"
  printf 'E2E_VM_CPU=%q\n' "$E2E_VM_CPU"
  printf 'E2E_VM_MEMORY=%q\n' "$E2E_VM_MEMORY"
  printf 'E2E_VM_DISK=%q\n' "$E2E_VM_DISK"
  printf 'E2E_VM_SLOT_COUNT=%q\n' "$E2E_VM_SLOT_COUNT"
  printf 'E2E_VM_BOOT_TIMEOUT=%q\n' "$E2E_VM_BOOT_TIMEOUT"
  printf 'E2E_VM_STATE_DIR=%q\n' "$E2E_VM_STATE_DIR"
  printf 'E2E_BROKER_SOURCE=%q\n' "$E2E_BROKER_SOURCE"
  printf 'E2E_AGENT_USER=%q\n' "$E2E_AGENT_USER"
  printf 'E2E_AGENT_HOME=%q\n' "$E2E_AGENT_HOME"
  printf 'E2E_AGENT_PUBLIC_KEY=%q\n' "$E2E_AGENT_PUBLIC_KEY"
} > "$config_candidate"
chmod 0644 "$config_candidate"

if [ "$NESTED_E2E_VMS" = 0 ]; then
  systemctl disable --now subyard-test-vms-gc.timer >/dev/null 2>&1 || true
  systemctl disable --now subyard-test-vms-lease-reaper.timer >/dev/null 2>&1 || true
  systemctl disable --now subyard-test-vms-broker.service >/dev/null 2>&1 || true
  disable_inner_firewall
  disable_agent_account
  disable_agent_sshd_policy
  restore_inner_apparmor_default
  mv -f -- "$config_candidate" /etc/subyard/test-vms.env
  trap - EXIT
  exit 0
fi

for node in /dev/kvm /dev/vsock /dev/vhost-vsock /dev/net/tun; do
  [ -c "$node" ] || { printf 'nested VM device missing inside yard: %s\n' "$node" >&2; exit 1; }
done

version_ok() {
  command -v incus >/dev/null 2>&1 \
    && dpkg --compare-versions "$(incus --version 2>/dev/null)" ge 6.0.6
}

if ! version_ok; then
  reconcile_inner_zabbly_repo
fi

export DEBIAN_FRONTEND=noninteractive
run_with_progress "updating inner VM backend packages" apt-get update -qq
run_with_progress "installing inner Incus and QEMU" \
  apt-get install -y -qq --no-install-recommends \
    incus qemu-system-x86 qemu-utils ovmf openssh-client nftables
version_ok || { printf 'Incus 6.0.6 or newer is required inside the yard\n' >&2; exit 1; }
# AppArmor 4.1 userspace emits AF_UNIX rules that the 6.8 kernel rejects with a type/protocol ABI
# mismatch. Disable only the inner daemon's per-instance profiles. The trusted L1 container remains
# confined by its L0 AppArmor profile and the inner daemon has no L0 socket or host paths.
reconcile_inner_apparmor_compat

getent group incus-admin >/dev/null 2>&1 || groupadd --system incus-admin
id -u "$DEV_USER" >/dev/null 2>&1 || { printf 'yard user is missing: %s\n' "$DEV_USER" >&2; exit 1; }
getent group yard >/dev/null 2>&1 || groupadd --system yard
remove_group_member "$DEV_USER" incus-admin
remove_group_member "$DEV_USER" yard
reconcile_test_vm_state_directory
reconcile_agent_account
reconcile_slot_accounts
reconcile_agent_sshd_policy
retire_agent_static_keys

reconcile_inner_incus
install_inner_firewall

systemctl disable --now subyard-test-vms-gc.timer >/dev/null 2>&1 || true
rm -f /etc/systemd/system/subyard-test-vms-gc.service \
  /etc/systemd/system/subyard-test-vms-gc.timer
cat > /etc/systemd/system/subyard-test-vms-lease-reaper.service <<'EOF'
[Unit]
Description=Fence expired Subyard test VM leases
After=incus.service
Requires=incus.service

[Service]
Type=oneshot
ExecStart=/usr/local/libexec/subyard/test-vms-inner _test-vms-worker gc
EOF
cat > /etc/systemd/system/subyard-test-vms-lease-reaper.timer <<'EOF'
[Unit]
Description=Check Subyard test VM lease heartbeats

[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
Persistent=true

[Install]
WantedBy=timers.target
EOF
cat > /etc/systemd/system/subyard-test-vms-broker.service <<'EOF'
[Unit]
Description=Subyard test VM lease broker lifecycle
After=incus.service
Requires=incus.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/libexec/subyard/test-vms-inner _test-vms-worker broker-start
ExecStart=/usr/local/libexec/subyard/test-vms-inner _test-vms-worker gc
ExecStop=/usr/local/libexec/subyard/test-vms-inner _test-vms-worker drain-all
TimeoutStopSec=10min

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload

SUBYARD_TEST_VMS_CONFIG="$config_candidate" \
  /usr/local/libexec/subyard/test-vms-inner _test-vms-worker reconcile-pool --yes
mv -f -- "$config_candidate" /etc/subyard/test-vms.env
trap - EXIT

systemctl enable --now subyard-test-vms-lease-reaper.timer
systemctl enable --now subyard-test-vms-broker.service

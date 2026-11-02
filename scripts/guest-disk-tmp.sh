#!/bin/sh
# Shared guest-only leaf of instance creation, base provisioning and readiness.
# Masking never stops a live tmpfs: existing workloads keep their files until reboot.
set -eu

policy_files='/etc/subyard-guest-tmpfiles.conf
/usr/local/lib/subyard-guest-tmp
/etc/systemd/system/subyard-tmp-shutdown.service
/etc/systemd/system/subyard-tmp-boot.service
/etc/systemd/system/subyard-tmp-clean.service
/etc/systemd/system/subyard-tmp-clean.timer'

# The host shows only these prefixed lines to the operator.
refuse() { echo "guest-disk-tmp: $*" >&2; }

safe_directory() {
  if [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c '%u:%g' "$1")" = 0:0 ] \
    && [ $((0$(stat -c '%a' "$1") & 022)) = 0 ]; then
    return 0
  fi
  refuse "unsafe guest temporary policy directory: $1 owner $(stat -c '%u:%g' "$1" 2>/dev/null || echo unknown) mode $(stat -c '%a' "$1" 2>/dev/null || echo unknown)"
  return 1
}

not_mountpoint() {
  result=0
  mountpoint -q "$1" || result=$?
  [ "$result" = 32 ]
}

tmp_mount_guard() {
  if [ -e /etc/systemd/system/tmp.mount ] || [ -L /etc/systemd/system/tmp.mount ]; then
    [ -L /etc/systemd/system/tmp.mount ] \
      && [ "$(readlink /etc/systemd/system/tmp.mount)" = /dev/null ] \
      && [ "$(stat -c '%u:%g' /etc/systemd/system/tmp.mount)" = 0:0 ] \
      || { refuse 'foreign or unsafe guest tmp.mount policy'; return 1; }
  fi
}

policy_content() {
  case "$1" in
    /etc/subyard-guest-tmpfiles.conf) cat <<'EOF'
# Subyard guest temporary storage policy v1. Invoked explicitly, never globally.
D! /tmp 1777 root root -
D /tmp/subyard-disposable 1777 root root mM:1d
EOF
      ;;
    /usr/local/lib/subyard-guest-tmp) cat <<'EOF'
#!/bin/sh
# Subyard guest temporary storage policy v1.
set -eu
fail() { echo "guest temporary cleanup refused: $*" >&2; exit 1; }
directory() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c '%u:%g' "$1")" = 0:0 ] \
    && [ $((0$(stat -c '%a' "$1") & 022)) = 0 ] || fail 'unsafe policy ancestor'
}
for parent in / /etc /usr /usr/local /usr/local/lib; do directory "$parent"; done
config=/etc/subyard-guest-tmpfiles.conf
[ -f "$config" ] && [ ! -L "$config" ] \
  && [ "$(stat -c '%a:%u:%g' "$config")" = 644:0:0 ] || fail 'unsafe policy file'
cmp -s "$config" - <<'POLICY' || fail 'foreign policy content'
# Subyard guest temporary storage policy v1. Invoked explicitly, never globally.
D! /tmp 1777 root root -
D /tmp/subyard-disposable 1777 root root mM:1d
POLICY
case "${1:-}" in
  clean|boot) ;;
  shutdown)
    state=$(systemctl is-system-running 2>/dev/null) || true
    [ "$state" = stopping ] || fail 'shutdown cleanup skipped outside native shutdown'
    ;;
  *) exit 2 ;;
esac
# A configured root is not protected by tmpfiles descendant-mount detection.
traversal_root() {
  [ -d "$1" ] && [ ! -L "$1" ] \
    && [ "$(stat -c '%a:%u:%g' "$1")" = 1777:0:0 ] || fail 'unsafe cleanup root'
  result=0
  mountpoint -q "$1" || result=$?
  [ "$result" = 32 ] || fail 'cleanup root is mounted or cannot be inspected'
}
traversal_root /tmp
if [ "$1" = clean ] || [ -e /tmp/subyard-disposable ] || [ -L /tmp/subyard-disposable ]; then
  traversal_root /tmp/subyard-disposable
fi
case "$1" in
  clean) exec systemd-tmpfiles --clean --prefix=/tmp/subyard-disposable "$config" ;;
  boot) exec systemd-tmpfiles --remove --create --boot "$config" ;;
  shutdown) exec systemd-tmpfiles --remove --boot "$config" ;;
esac
EOF
      ;;
    /etc/systemd/system/subyard-tmp-shutdown.service) cat <<'EOF'
# Subyard guest temporary storage policy v1.
[Unit]
Description=Clear yard temporary data after workloads stop
DefaultDependencies=no
After=local-fs.target
Before=sysinit.target shutdown.target umount.target
Conflicts=shutdown.target umount.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/true
ExecStop=/usr/local/lib/subyard-guest-tmp shutdown
TimeoutStopSec=30s
TimeoutStopFailureMode=kill
[Install]
WantedBy=sysinit.target
EOF
      ;;
    /etc/systemd/system/subyard-tmp-boot.service) cat <<'EOF'
# Subyard guest temporary storage policy v1.
[Unit]
Description=Recover yard temporary storage before workloads start
DefaultDependencies=no
After=local-fs.target
Before=systemd-tmpfiles-setup.service sysinit.target subyard-tmp-shutdown.service shutdown.target
Conflicts=shutdown.target
[Service]
Type=oneshot
ExecStart=/usr/local/lib/subyard-guest-tmp boot
TimeoutStartSec=30s
TimeoutStartFailureMode=kill
[Install]
WantedBy=sysinit.target
EOF
      ;;
    /etc/systemd/system/subyard-tmp-clean.service) cat <<'EOF'
# Subyard guest temporary storage policy v1.
[Unit]
Description=Age explicitly disposable yard temporary data
[Service]
Type=oneshot
ExecStart=/usr/local/lib/subyard-guest-tmp clean
TimeoutStartSec=30s
TimeoutStartFailureMode=kill
EOF
      ;;
    /etc/systemd/system/subyard-tmp-clean.timer) cat <<'EOF'
# Subyard guest temporary storage policy v1.
[Unit]
Description=Check explicitly disposable yard temporary data hourly
[Timer]
OnBootSec=15min
OnUnitActiveSec=1h
Unit=subyard-tmp-clean.service
[Install]
WantedBy=timers.target
EOF
      ;;
    *) return 2 ;;
  esac
}

# Reports every independent problem before refusing; nothing here writes.
policy_guard() {
  guard_failed=0
  for parent in / /etc /usr /usr/local /usr/local/lib /etc/systemd /etc/systemd/system; do
    safe_directory "$parent" || guard_failed=1
  done
  for file in $policy_files; do
    mode=644
    [ "$file" != /usr/local/lib/subyard-guest-tmp ] || mode=755
    if [ -e "$file" ] || [ -L "$file" ]; then
      [ -f "$file" ] && [ ! -L "$file" ] \
        && [ "$(stat -c '%a:%u:%g' "$file")" = "$mode:0:0" ] \
        && policy_content "$file" | cmp -s - "$file" \
        || { refuse "foreign or unsafe guest temporary policy file: $file"; guard_failed=1; }
    elif [ "$1" = check ]; then
      refuse "guest temporary policy file missing: $file"; guard_failed=1
    fi
  done
  for unit in subyard-tmp-shutdown.service subyard-tmp-boot.service subyard-tmp-clean.service subyard-tmp-clean.timer; do
    overrides=$(systemctl show "$unit" -p DropInPaths --value) \
      || { refuse "cannot inspect guest temporary policy unit: $unit"; guard_failed=1; continue; }
    case "$overrides" in
      */subyard-*.d/*) refuse "foreign guest temporary policy unit override: $unit"; guard_failed=1 ;;
    esac
  done
  for target in sysinit.target timers.target; do
    directory=/etc/systemd/system/$target.wants
    if [ -e "$directory" ] || [ -L "$directory" ]; then safe_directory "$directory" || guard_failed=1; fi
  done
  for unit in subyard-tmp-shutdown.service subyard-tmp-boot.service subyard-tmp-clean.timer; do
    target=sysinit.target
    [ "$unit" != subyard-tmp-clean.timer ] || target=timers.target
    link=/etc/systemd/system/$target.wants/$unit
    if [ -e "$link" ] || [ -L "$link" ]; then
      [ -L "$link" ] && [ "$(stat -c '%u:%g' "$link")" = 0:0 ] \
        && [ "$(readlink -f "$link")" = "/etc/systemd/system/$unit" ] \
        || { refuse "foreign guest temporary policy boot link: $unit"; guard_failed=1; }
    elif [ "$1" = check ]; then
      refuse "guest temporary policy boot link missing: $unit"; guard_failed=1
    fi
  done
  if [ -e /tmp/subyard-disposable ] || [ -L /tmp/subyard-disposable ]; then
    [ -d /tmp/subyard-disposable ] && [ ! -L /tmp/subyard-disposable ] \
      && [ "$(stat -c '%a:%u:%g' /tmp/subyard-disposable)" = 1777:0:0 ] \
      && not_mountpoint /tmp/subyard-disposable \
      || { refuse 'unsafe guest disposable temporary directory'; guard_failed=1; }
  fi
  return "$guard_failed"
}

unit_command() {
  value=$(systemctl show "$1" -p "$2" --value) || return
  executable=${3%% *}
  prefix="{ path=$executable ; argv[]=$3 ; ignore_errors=no ; "
  case "$value" in
    "$prefix"*) remainder=${value#"$prefix"} ;;
    *) echo 'guest temporary cleanup command is overridden' >&2; return 1 ;;
  esac
  case "$remainder" in *'argv[]='*) echo 'extra guest temporary cleanup command' >&2; return 1 ;; esac
}

check_units() {
  unit_command subyard-tmp-shutdown.service ExecStart /usr/bin/true
  unit_command subyard-tmp-shutdown.service ExecStop '/usr/local/lib/subyard-guest-tmp shutdown'
  unit_command subyard-tmp-boot.service ExecStart '/usr/local/lib/subyard-guest-tmp boot'
  unit_command subyard-tmp-clean.service ExecStart '/usr/local/lib/subyard-guest-tmp clean'
  [ "$(systemctl show subyard-tmp-shutdown.service -p TimeoutStopUSec --value)" = 30s ]
  [ "$(systemctl show subyard-tmp-shutdown.service -p TimeoutStopFailureMode --value)" = kill ]
  for unit in subyard-tmp-boot.service subyard-tmp-clean.service; do
    [ "$(systemctl show "$unit" -p TimeoutStartUSec --value)" = 30s ]
    [ "$(systemctl show "$unit" -p TimeoutStartFailureMode --value)" = kill ]
  done
}

apply_policy() {
  policy_guard apply
  for file in $policy_files; do
    [ ! -e "$file" ] || continue
    temporary=$(mktemp "${file%/*}/.subyard-tmp.XXXXXX")
    policy_content "$file" > "$temporary"
    mode=644
    [ "$file" != /usr/local/lib/subyard-guest-tmp ] || mode=755
    chmod "$mode" "$temporary"
    mv -T "$temporary" "$file"
  done
  systemctl daemon-reload
  check_units
  # No --boot/--remove: installation never clears a running guest's /tmp.
  if [ ! -e /tmp/subyard-disposable ]; then mkdir -m 1777 /tmp/subyard-disposable; fi
  [ -d /tmp/subyard-disposable ] && [ ! -L /tmp/subyard-disposable ] \
    && [ "$(stat -c '%a:%u:%g' /tmp/subyard-disposable)" = 1777:0:0 ]
  systemd-tmpfiles --create /etc/subyard-guest-tmpfiles.conf
  systemctl enable subyard-tmp-shutdown.service subyard-tmp-boot.service subyard-tmp-clean.timer
  systemctl start subyard-tmp-shutdown.service subyard-tmp-clean.timer
}

wait_boot() {
  # Incus exec works before a new container's system bus and boot mounts are
  # ready. Finish boot before masking (which reloads the manager) or deciding
  # whether the active filesystem needs a restart. Degraded is a completed boot.
  timeout --kill-after=5 120 sh -c '
    while :; do
      rc=0
      state=$(systemctl is-system-running 2>/dev/null) || rc=$?
      case "$state:$rc" in
        running:0|degraded:1) exit 0 ;;
        :1|unknown:1|offline:1|initializing:1|starting:1) ;;
        *) echo "guest boot readiness failed: state=$state exit=$rc" >&2; exit 1 ;;
      esac
      sleep 1
    done
  ' || { echo 'guest boot did not complete before temporary storage setup' >&2; exit 1; }
}

case "${1:-}" in
  wait-boot) wait_boot ;;
  assess)
    # Pre-consent observation: missing policy is eligible for installation.
    # Never mask, reload, clean or wait for a boot from this read-only leaf.
    # Exit 0 is disk-backed, 10 is memory-backed; 1 follows every reported finding.
    findings=0
    if [ -d /tmp ] && [ ! -L /tmp ]; then
      ownership=$(stat -c '%a:%u:%g' /tmp 2>/dev/null || echo unknown)
      [ "$ownership" = 1777:0:0 ] || { refuse "/tmp mode and owner are $ownership not 1777:0:0"; findings=1; }
      [ -w /tmp ] || { refuse '/tmp is not writable'; findings=1; }
    else
      refuse '/tmp is not a real directory'; findings=1
    fi
    tmp_mount_guard || findings=1
    policy_guard apply || findings=1
    memory=0
    if filesystem=$(findmnt -n -T /tmp -o FSTYPE); then
      result=0
      mountpoint -q /tmp || result=$?
      case "$filesystem" in
        tmpfs|ramfs)
          memory=1
          case "$result" in
            0|32) ;;
            *) refuse "cannot inspect the /tmp mount: mountpoint exit $result"; findings=1 ;;
          esac
          ;;
        ext2|ext3|ext4|btrfs|xfs|zfs|overlay|virtiofs|9p)
          [ "$result" = 32 ] || { refuse "disk-backed /tmp is its own mount: mountpoint exit $result"; findings=1; }
          ;;
        *) refuse "unsupported /tmp filesystem: $filesystem"; findings=1 ;;
      esac
    else
      refuse 'cannot read the /tmp filesystem'; findings=1
    fi
    [ "$findings" = 0 ] || exit 1
    [ "$memory" = 0 ] || exit 10
    ;;
  apply)
    wait_boot
    [ -d /tmp ] && [ ! -L /tmp ]
    [ "$(stat -c '%a:%u:%g' /tmp)" = 1777:0:0 ]
    tmp_mount_guard
    policy_guard apply
    chmod 1777 /tmp
    systemctl mask tmp.mount
    apply_policy
    ;;
  check)
    [ -d /tmp ] && [ ! -L /tmp ]
    [ "$(stat -c '%a:%u:%g' /tmp)" = 1777:0:0 ]
    [ "$(readlink /etc/systemd/system/tmp.mount)" = /dev/null ]
    [ "$(stat -c '%u:%g' /etc/systemd/system/tmp.mount)" = 0:0 ]
    policy_guard check
    check_units
    for unit in subyard-tmp-shutdown.service subyard-tmp-clean.timer; do
      systemctl is-active --quiet "$unit"
    done
    filesystem=$(findmnt -n -T /tmp -o FSTYPE)
    case "$filesystem" in
      tmpfs|ramfs) exit 10 ;; # Installed policy needs the next native boot.
      ext2|ext3|ext4|btrfs|xfs|zfs|overlay|virtiofs|9p) ;;
      *) exit 1 ;;
    esac
    not_mountpoint /tmp
    [ "$(systemctl show subyard-tmp-boot.service -p Result --value)" = success ]
    [ -w /tmp ]
    ;;
  *) exit 2 ;;
esac

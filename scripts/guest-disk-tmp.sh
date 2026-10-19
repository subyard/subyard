#!/bin/sh
# Shared guest-only leaf of instance creation, base provisioning and readiness.
# Masking never stops a live tmpfs: existing workloads keep their files until reboot.
set -eu

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
  apply)
    wait_boot
    [ -d /tmp ] && [ ! -L /tmp ]
    [ "$(stat -c '%u:%g' /tmp)" = 0:0 ]
    chmod 1777 /tmp
    systemctl mask tmp.mount
    ;;
  check)
    [ -d /tmp ] && [ ! -L /tmp ]
    [ "$(stat -c '%a:%u:%g' /tmp)" = 1777:0:0 ]
    [ "$(readlink /etc/systemd/system/tmp.mount)" = /dev/null ]
    [ "$(stat -c '%u:%g' /etc/systemd/system/tmp.mount)" = 0:0 ]
    filesystem=$(findmnt -n -T /tmp -o FSTYPE)
    case "$filesystem" in
      tmpfs|ramfs) exit 10 ;; # Installed policy needs the next native boot.
      ext2|ext3|ext4|btrfs|xfs|zfs|overlay|virtiofs|9p) ;;
      *) exit 1 ;;
    esac
    [ -w /tmp ]
    ;;
  *) exit 2 ;;
esac

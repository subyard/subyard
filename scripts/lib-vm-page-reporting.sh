#!/usr/bin/env bash
# Fixed owner-side QEMU override for an opted-in VM. Never accept raw
# QEMU text from yard settings or a profile.

VM_PAGE_REPORTING_CONF=$'[device "qemu_balloon"]\nfree-page-reporting = "on"'
VM_PAGE_REPORTING_TMPFILES_RULE='w /sys/module/page_reporting/parameters/page_reporting_order - - - - 1'

vm_page_reporting_required() {
  [ "${VM_FREE_PAGE_REPORTING:-0}" = 1 ]
}

vm_page_reporting_check_host() {
  local version help
  command -v qemu-system-x86_64 >/dev/null 2>&1 \
    || { printf 'VM Free Page Reporting requires qemu-system-x86_64\n' >&2; return 1; }
  version="$(qemu-system-x86_64 --version 2>/dev/null)" \
    || { printf 'cannot identify QEMU for VM Free Page Reporting\n' >&2; return 1; }
  case "$version" in
    'QEMU emulator version 8.2.'* | 'QEMU emulator version 10.0.'*) ;;
    *) printf 'unsupported QEMU for VM Free Page Reporting: expected tested 8.2.x or 10.0.x\n' >&2; return 1 ;;
  esac
  help="$(qemu-system-x86_64 -device virtio-balloon-pci,help 2>&1)" \
    || { printf 'cannot inspect QEMU virtio-balloon-pci reporting capability\n' >&2; return 1; }
  printf '%s\n' "$help" | grep -q 'free-page-reporting' \
    || { printf 'QEMU virtio-balloon-pci has no free-page-reporting property\n' >&2; return 1; }
}

vm_page_reporting_check_profile() { # <project>
  local project="$1" key value
  for key in raw.qemu raw.qemu.conf raw.qemu.qmp.early raw.qemu.qmp.pre-start \
             raw.qemu.qmp.post-start raw.qemu.scriptlet; do
    value="$(incus profile get default "$key" --project "$project")" \
      || { printf 'cannot inspect inherited %s in VM project %s\n' "$key" "$project" >&2; return 1; }
    [ -z "$value" ] \
      || { printf 'unexpected inherited %s in VM project %s\n' "$key" "$project" >&2; return 1; }
  done
}

vm_page_reporting_check_raw() { # <project> <instance> [allow-managed-local]
  local project="$1" instance="$2" managed="${3:-false}" key value local_conf
  for key in raw.qemu raw.qemu.conf raw.qemu.qmp.early raw.qemu.qmp.pre-start \
             raw.qemu.qmp.post-start raw.qemu.scriptlet; do
    value="$(incus config get "$instance" "$key" --expanded --project "$project")" \
      || { printf 'cannot inspect effective %s on %s/%s\n' "$key" "$project" "$instance" >&2; return 1; }
    [ -n "$value" ] || continue
    if [ "$key" = raw.qemu.conf ] && [ "$managed" = true ]; then
      local_conf="$(incus config get "$instance" "$key" --project "$project")" \
        || { printf 'cannot inspect local %s on %s/%s\n' "$key" "$project" "$instance" >&2; return 1; }
      [ "$local_conf" = "$VM_PAGE_REPORTING_CONF" ] && [ "$value" = "$VM_PAGE_REPORTING_CONF" ] && continue
    fi
    printf 'unexpected effective %s on VM %s/%s\n' "$key" "$project" "$instance" >&2
    return 1
  done
}

vm_page_reporting_check_guest_feature() { # <project> <instance>
  local project="$1" instance="$2" bits
  # Linux virtio.c emits negotiated feature bits from bit 0 upward. The
  # virtio-balloon REPORTING feature is bit 5 (the sixth character).
  bits="$(timeout --foreground 5 incus exec "$instance" --project "$project" -- sh -ec '
    count=0
    for file in /sys/bus/virtio/drivers/virtio_balloon/virtio*/features; do
      [ -r "$file" ] || continue
      count=$((count + 1))
      [ "$count" -eq 1 ] || { printf "multiple\n"; exit 0; }
      bits="$(cat "$file")"
    done
    [ "$count" -eq 1 ] || { printf "missing\n"; exit 0; }
    printf "%s\n" "$bits"
  ' </dev/null 2>/dev/null)" || {
    printf 'VM Free Page Reporting guest agent is unavailable or the balloon feature probe failed\n' >&2
    return 10
  }
  case "$bits" in
    missing)
      printf 'VM Free Page Reporting guest virtio_balloon device is missing\n' >&2
      return 10 ;;
    multiple)
      printf 'VM Free Page Reporting guest has multiple virtio_balloon devices\n' >&2
      return 10 ;;
    '' | *[!01]*)
      printf 'VM Free Page Reporting guest virtio_balloon feature bits are invalid\n' >&2
      return 10 ;;
  esac
  [ "${#bits}" -ge 6 ] && [ "${#bits}" -le 256 ] || {
    printf 'VM Free Page Reporting guest virtio_balloon feature bits have an unsupported length\n' >&2
    return 10
  }
  [ "${bits:5:1}" = 1 ] || {
    printf 'VM Free Page Reporting feature bit 5 was not negotiated by the guest virtio_balloon driver\n' >&2
    return 10
  }
}

vm_page_reporting_check_guest() { # <project> <instance>
  local project="$1" instance="$2" state
  vm_page_reporting_check_guest_feature "$project" "$instance" || return $?
  state="$(timeout --foreground 5 incus exec "$instance" --project "$project" -- sh -ec '
    rule=/etc/tmpfiles.d/subyard-vm-page-reporting.conf
    order=/sys/module/page_reporting/parameters/page_reporting_order
    expected=$1
    if [ -L "$rule" ] || [ ! -f "$rule" ]; then printf "rule-missing-or-unsafe\n"; exit 0; fi
    if [ "$(stat -c "%a:%u:%g" "$rule")" != 644:0:0 ]; then printf "rule-mode\n"; exit 0; fi
    if ! printf "%s\n" "$expected" | cmp -s - "$rule"; then printf "rule-content\n"; exit 0; fi
    if [ ! -r "$order" ]; then printf "order-missing\n"; exit 0; fi
    if [ "$(cat "$order")" != 1 ]; then printf "order-value\n"; exit 0; fi
    printf "ready\n"
  ' sh "$VM_PAGE_REPORTING_TMPFILES_RULE" </dev/null 2>/dev/null)" || {
    printf 'VM Free Page Reporting guest order probe failed\n' >&2
    return 10
  }
  case "$state" in
    ready) return 0 ;;
    rule-missing-or-unsafe) printf 'VM Free Page Reporting guest tmpfiles rule is missing or unsafe\n' >&2 ;;
    rule-mode) printf 'VM Free Page Reporting guest tmpfiles rule ownership or mode drifted\n' >&2 ;;
    rule-content) printf 'VM Free Page Reporting guest tmpfiles rule content drifted\n' >&2 ;;
    order-missing) printf 'VM Free Page Reporting guest page_reporting_order is unavailable\n' >&2 ;;
    order-value) printf 'VM Free Page Reporting guest page_reporting_order is not 1\n' >&2 ;;
    *) printf 'VM Free Page Reporting guest order probe returned an invalid result\n' >&2 ;;
  esac
  return 10
}

vm_page_reporting_prepare_guest() { # <project> <instance>
  local project="$1" instance="$2"
  vm_page_reporting_check_guest_feature "$project" "$instance" || return $?
  printf '%s\n' "$VM_PAGE_REPORTING_TMPFILES_RULE" | \
    timeout --foreground 30 incus exec "$instance" --project "$project" -T -- sh -ec '
      rule=/etc/tmpfiles.d/subyard-vm-page-reporting.conf
      if [ -e "$rule" ] || [ -L "$rule" ]; then
        [ -f "$rule" ] && [ ! -L "$rule" ] || exit 1
      fi
      install -o 0 -g 0 -m 0644 /dev/stdin "$rule"
      systemd-tmpfiles --create "$rule"
    ' || {
      printf 'could not install or apply VM Free Page Reporting guest tmpfiles rule\n' >&2
      return 10
    }
  vm_page_reporting_check_guest "$project" "$instance"
}

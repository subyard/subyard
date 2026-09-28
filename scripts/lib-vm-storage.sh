#!/usr/bin/env bash
# Optional bounded VM block storage. Format only a blank, newly marked volume.

vm_storage_volume() {
  incus query "/1.0/storage-pools/$SRV_POOL/volumes/custom/$SRV_VOLUME?project=$INCUS_PROJECT"
}

vm_storage_size_bytes() {
  [[ "${SRV_VOLUME_SIZE:-}" =~ ^[1-9][0-9]*(MiB|GiB)$ ]] || return 1
  numfmt --from=iec-i "${SRV_VOLUME_SIZE%B}"
}

# Inspect expanded devices because a project profile can supply a host mount
# without adding any local instance device. An exact old Subyard route mount is
# permitted only during its guarded removal before the instance starts.
vm_storage_check_device_boundary() { # <allow-old-route: true|false>
  local allow_route="${1:?allow-old-route is required}" instance profile
  [ -n "${ROOT_DISK_SIZE:-}" ] || [ "${ALLOWS_HOST_ACCESS:-true}" = false ] || return 0
  instance="$(incus query "/1.0/instances/$YARD_INSTANCE_NAME?project=$INCUS_PROJECT")" || return 1
  profile="$(incus query "/1.0/profiles/default?project=$INCUS_PROJECT")" || return 1
  printf '%s\n%s\n' "$instance" "$profile" | jq -se \
    --arg size "${ROOT_DISK_SIZE:-}" --arg no_host "${ALLOWS_HOST_ACCESS:-true}" \
    --arg pool "$SRV_POOL" --arg volume "$SRV_VOLUME" \
    --arg block "${SRV_VOLUME_TYPE:-filesystem}" --arg allow_route "$allow_route" \
    --arg route_source "$SUBYARD_HOME/e2e/routes" '
      length == 2 and
      (.[0] as $instance | .[1].devices as $profile |
       ($instance.devices // {}) as $local |
       ($instance.expanded_devices // {}) as $effective |
       ($profile.root.type == "disk" and $profile.root.path == "/" and
        ($profile.root.pool // "") != "" and ($profile.root.source // "") == "") and
       ($size == "" or $profile.root.size == $size) and
       ($local | has("root") | not) and $effective.root == $profile.root and
       (if $no_host != "false" then true else
         ($profile.eth0.type == "nic" and ($profile.eth0.network // "") != "" and
          ($effective.eth0 | del(."ipv4.address", .hwaddr)) ==
          ($profile.eth0 | del(."ipv4.address", .hwaddr))) and
         all($effective | to_entries[];
           .key as $name | .value as $device |
           if $name == "root" or $name == "eth0" then true
           elif $name == "srv" then
             $local.srv == $device and $device.type == "disk" and
             $device.pool == $pool and $device.source == $volume and
             ($device.path // "") == (if $block == "block" then "" else "/srv" end) and
             ($block != "block" or $device["io.bus"] == "virtio-scsi")
           elif $name == "subyard-e2e-routes" then
             $allow_route == "true" and $local[$name] == $device and
             $device.type == "disk" and $device.source == $route_source and
             $device.path == "/var/lib/subyard/e2e-routes" and $device.readonly == "true"
           elif $device.type == "proxy" then ($device.bind // "host") == "host"
           else false end)
       end))
    ' >/dev/null || {
      printf 'effective Incus devices violate the managed root disk or ALLOWS_HOST_ACCESS boundary\n' >&2
      return 1
    }
}

vm_storage_check() {
  local volume uuid state mounted
  [ "${SRV_VOLUME_TYPE:-filesystem}" = block ] || return 10
  [ -n "${SRV_VOLUME_SIZE:-}" ] || return 10
  volume="$(vm_storage_volume)" || return 10
  jq -e --arg size "$SRV_VOLUME_SIZE" '.content_type == "block" and
    .config.size == $size and .config["user.subyard.storage"] == "v1" and
    .config["user.subyard.format"] == "ready"' \
    >/dev/null <<<"$volume" || return 10
  uuid="$(jq -r '.config["user.subyard.uuid"] // ""' <<<"$volume")"
  [[ "$uuid" =~ ^[a-f0-9-]{36}$ ]] || return 10
  if [ -n "${ROOT_DISK_SIZE:-}" ]; then
    [ "$(incus profile device get default root size --project "$INCUS_PROJECT")" = "$ROOT_DISK_SIZE" ] || return 10
  fi
  vm_storage_check_device_boundary false || return 10
  [ "$(device_get srv type)" = disk ] && [ "$(device_get srv pool)" = "$SRV_POOL" ] \
    && [ "$(device_get srv source)" = "$SRV_VOLUME" ] && [ -z "$(device_get srv path)" ] \
    && [ "$(device_get srv io.bus)" = virtio-scsi ] || return 10
  state="$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")" || return 10
  case "$state" in
    RUNNING)
      mounted="$(incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- findmnt -n -o UUID /srv)" || return 10
      [ "$mounted" = "$uuid" ] || return 10 ;;
    STOPPED) ;;
    *) return 10 ;;
  esac
}

vm_storage_prepare() {
  [ -n "${ROOT_DISK_SIZE:-}" ] || return 0
  [ "$YARD_KIND" = vm ] || die 'ROOT_DISK_SIZE requires YARD_KIND=vm'
  local size
  size="$(incus profile device get default root size --project "$INCUS_PROJECT")"
  case "$size" in
    '')
      incus info "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" >/dev/null 2>&1 \
        && die 'refusing to set a root disk limit on an existing VM without a prior limit'
      incus profile device set default root "size=$ROOT_DISK_SIZE" --project "$INCUS_PROJECT" ;;
    "$ROOT_DISK_SIZE") ;;
    *) die 'VM root disk differs from ROOT_DISK_SIZE; refusing to resize an existing disk' ;;
  esac
}

vm_storage_attach() {
  local volume
  [ "$YARD_KIND" = vm ] && [ -n "${SRV_VOLUME_SIZE:-}" ] \
    || die 'block SRV_VOLUME_TYPE requires a VM and SRV_VOLUME_SIZE'
  if volume="$(vm_storage_volume 2>/dev/null)"; then
    jq -e --arg size "$SRV_VOLUME_SIZE" '.content_type == "block" and
      .config.size == $size and .config["user.subyard.storage"] == "v1"' \
      >/dev/null <<<"$volume" || die 'refusing an unowned or differently sized VM state volume'
  else
    incus storage volume create "$SRV_POOL" "$SRV_VOLUME" --type=block \
      "size=$SRV_VOLUME_SIZE" user.subyard.storage=v1 user.subyard.format=pending \
      --project "$INCUS_PROJECT" >/dev/null
  fi
  if device_exists srv; then
    [ "$(device_get srv type)" = disk ] && [ "$(device_get srv pool)" = "$SRV_POOL" ] \
      && [ "$(device_get srv source)" = "$SRV_VOLUME" ] && [ -z "$(device_get srv path)" ] \
      && [ "$(device_get srv io.bus)" = virtio-scsi ] \
      || die 'refusing to replace a conflicting VM srv device'
  else
    incus config device add "$YARD_INSTANCE_NAME" srv disk --project "$INCUS_PROJECT" \
      pool="$SRV_POOL" source="$SRV_VOLUME" io.bus=virtio-scsi >/dev/null
  fi
}

vm_storage_mount() {
  local pending recorded_uuid uuid bytes
  bytes="$(vm_storage_size_bytes)" || die 'invalid SRV_VOLUME_SIZE for block volume'
  pending="$(incus storage volume get "$SRV_POOL" "$SRV_VOLUME" user.subyard.format --project "$INCUS_PROJECT")"
  recorded_uuid="$(incus storage volume get "$SRV_POOL" "$SRV_VOLUME" user.subyard.uuid --project "$INCUS_PROJECT")"
  case "$pending" in
    pending) [ -z "$recorded_uuid" ] || die 'pending VM state volume has an unexpected filesystem identity' ;;
    ready) [[ "$recorded_uuid" =~ ^[a-f0-9-]{36}$ ]] || die 'ready VM state volume has no valid filesystem identity' ;;
    *) die 'VM state volume has an unknown format marker' ;;
  esac
  incus_wait_instance_agent "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" \
    || die 'VM agent did not become ready for state volume initialization'
  uuid="$(incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- bash -euo pipefail -s -- "$pending" "$bytes" "$recorded_uuid" <<'GUEST'
disk=/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_incus_srv
udevadm settle --timeout=30
[ -b "$disk" ] || { echo 'Subyard state block device is missing' >&2; exit 1; }
[ "$(blockdev --getsize64 "$disk")" = "$2" ] || exit 1
[ "$(lsblk -dn -o SERIAL "$disk" | xargs)" = incus_srv ] || exit 1
kind="$(blkid -p -s TYPE -o value "$disk" || true)"
if [ -z "$kind" ]; then
  [ "$1" = pending ] || { echo 'refusing to format existing state volume' >&2; exit 1; }
  [ -z "$(wipefs --noheadings --output TYPE "$disk")" ] || exit 1
  mkfs.ext4 -q -L subyard-srv "$disk" >&2
else
  [ "$kind" = ext4 ] && [ "$(blkid -s LABEL -o value "$disk")" = subyard-srv ] \
    || { echo 'refusing a foreign state filesystem' >&2; exit 1; }
fi
uuid="$(blkid -s UUID -o value "$disk")"
[[ "$uuid" =~ ^[a-f0-9-]{36}$ ]] || exit 1
[ "$1" != ready ] || [ "$uuid" = "$3" ] || { echo 'VM state filesystem identity changed' >&2; exit 1; }
entry="UUID=$uuid /srv ext4 defaults 0 2 # subyard-srv-v1"
if mountpoint -q /srv; then
  [ "$(findmnt -n -o UUID /srv)" = "$uuid" ] || exit 1
else
  [ ! -L /srv ] || exit 1
  [ ! -d /srv ] || [ -z "$(find /srv -mindepth 1 -maxdepth 1 -print -quit)" ] \
    || { echo 'refusing to hide existing srv data' >&2; exit 1; }
  mkdir -p /srv
fi
if ! grep -Fxq "$entry" /etc/fstab; then
  ! awk '$1 !~ /^#/ && $2 == "/srv" { found=1 } END { exit !found }' /etc/fstab || exit 1
  printf '%s\n' "$entry" >> /etc/fstab
  systemctl daemon-reload
fi
mountpoint -q /srv || mount /srv
[ "$(findmnt -n -o UUID /srv)" = "$uuid" ] || exit 1
printf '%s\n' "$uuid"
GUEST
  )" || die 'could not initialize or mount the bounded VM state volume'
  [[ "$uuid" =~ ^[a-f0-9-]{36}$ ]] || die 'VM state volume returned an invalid identity'
  [ "$pending" != ready ] || [ "$uuid" = "$recorded_uuid" ] \
    || die 'VM state filesystem identity differs from its owner marker'
  incus storage volume set "$SRV_POOL" "$SRV_VOLUME" user.subyard.format=ready \
    user.subyard.uuid="$uuid" --project "$INCUS_PROJECT" >/dev/null
}

#!/usr/bin/env bash
# Runs as root in a VM with Incus: measures a nested VM's firmware or its whole Incus boot.
# usage: l3-boot-probe.sh firmware|stack LIMIT_SECONDS
set -uo pipefail

MODE="$1"
LIMIT="$2"
QEMU=/opt/incus/bin/qemu-system-x86_64
FIRMWARE=/opt/incus/share/qemu
WORK="$(mktemp -d /tmp/l3-boot.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

# Without a boot device OVMF reaches its boot manager. It requires SMM, as Incus runs it.
firmware() {
  local start=$SECONDS pid
  cp "$FIRMWARE/OVMF_VARS.4MB.fd" "$WORK/vars"
  : > "$WORK/serial"
  LD_LIBRARY_PATH=/opt/incus/lib timeout -k 5 "$LIMIT" "$QEMU" -nodefaults -no-user-config -display none \
    -serial "file:$WORK/serial" -m 1024 -smp 1 -machine q35,smm=on -accel kvm -cpu host \
    -global driver=cfi.pflash01,property=secure,value=on \
    -drive "if=pflash,format=raw,readonly=on,file=$FIRMWARE/OVMF_CODE.4MB.fd" \
    -drive "if=pflash,format=raw,file=$WORK/vars" </dev/null >/dev/null 2>&1 &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    if grep -aqE 'BdsDxe|Shell>|UEFI Interactive Shell' "$WORK/serial"; then
      printf 'firmware: reached after %ss\n' "$((SECONDS - start))"
      pkill -P "$pid" 2>/dev/null; kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
      return 0
    fi
    sleep 1
  done
  printf 'firmware: not reached in %ss\n' "$((SECONDS - start))"
}

stack() {
  local start=$SECONDS
  incus launch images:debian/13/cloud l3-stack --vm -c limits.cpu=1 -c limits.memory=1GiB \
    -c security.secureboot=false </dev/null || { printf 'stack: launch failed\n'; return 0; }
  until incus exec l3-stack -- true </dev/null >/dev/null 2>&1; do
    if [ $((SECONDS - start)) -ge "$LIMIT" ]; then
      printf 'stack: no agent in %ss; console:\n' "$LIMIT"
      incus console l3-stack --show-log </dev/null 2>&1 | tail -n 10 | cut -c1-160
      incus delete --force l3-stack </dev/null
      return 0
    fi
    sleep 3
  done
  printf 'stack: agent after %ss\n' "$((SECONDS - start))"
  incus delete --force l3-stack </dev/null
}

case "$MODE" in
  firmware) firmware ;;
  stack) stack ;;
  *) printf 'usage: l3-boot-probe.sh firmware|stack LIMIT_SECONDS\n' >&2; exit 2 ;;
esac

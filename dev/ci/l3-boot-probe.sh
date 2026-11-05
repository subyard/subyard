#!/usr/bin/env bash
# Runs as root in a VM with Incus: measures how far a VM nested in it gets, separating the kernel,
# the firmware and KVM settings, so a slow or stuck boot can be attributed to one of them.
set -uo pipefail

LIMIT="${1:-240}"
QEMU=/opt/incus/bin/qemu-system-x86_64
FIRMWARE=/opt/incus/share/qemu
WORK="$(mktemp -d /tmp/l3-boot.XXXXXX)"
KVM_STATS=(exits io_exits mmio_exits irq_exits halt_exits pf_taken pf_fixed tlb_flush invlpg)
trap 'rm -rf "$WORK"' EXIT

facts() {
  grep -m1 'model name' /proc/cpuinfo
  grep -m1 flags /proc/cpuinfo | tr ' ' '\n' \
    | grep -xE 'svm|vmx|npt|ept|vgif|v_vmsave_vmload|avic|nrip_save|tsc_scale|hypervisor' | xargs
  grep -H . /sys/module/kvm_amd/parameters/{nested,npt,vgif,vls,avic} \
    /sys/module/kvm/parameters/tdp_mmu /sys/module/kvm_intel/parameters/ept 2>/dev/null || true
  uname -r
  ls /sys/kernel/debug/kvm 2>&1 | head -n 3
}

kvm_stats() {
  local name
  for name in "${KVM_STATS[@]}"; do
    printf '%s=%s ' "$name" "$(cat "/sys/kernel/debug/kvm/$name" 2>/dev/null || printf '?')"
  done
}

monitor() {
  python3 - "$1" "$2" <<'PY'
import socket, sys
s = socket.socket(socket.AF_UNIX)
s.settimeout(5)
s.connect(sys.argv[1])
s.sendall(sys.argv[2].encode() + b"\n")
out = b""
try:
    while out.count(b"(qemu)") < 2:
        out += s.recv(65536)
except OSError:
    pass
print(out.decode(errors="replace"))
PY
}

# Where the guest is executing: a firmware loop keeps its RIP in one narrow range.
sample_guest() {
  local socket="$1" name="$2" first second
  first="$(kvm_stats)"
  monitor "$socket" 'info registers' | grep -aoE '[ER]IP=[0-9a-f]+|CR0=[0-9a-f]+' | xargs printf '%s: %s %s\n' "$name" 2>/dev/null
  sleep 10
  second="$(kvm_stats)"
  monitor "$socket" 'info registers' | grep -aoE '[ER]IP=[0-9a-f]+|CR0=[0-9a-f]+' | xargs printf '%s: %s %s\n' "$name" 2>/dev/null
  printf '%s: 10s kvm counters\n  %s\n  %s\n' "$name" "$first" "$second"
}

# NAME MARKER QEMU_ARGS...: the seconds until MARKER reaches the serial port, or the last output.
try() {
  local name="$1" marker="$2" log="$WORK/$1.serial" socket="$WORK/$1.sock" start=$SECONDS pid sampled=0
  shift 2
  : > "$log"
  LD_LIBRARY_PATH=/opt/incus/lib timeout -k 5 "$LIMIT" "$QEMU" -nodefaults -no-user-config -display none \
    -serial "file:$log" -monitor "unix:$socket,server=on,wait=off" -m 1024 -smp 1 "$@" </dev/null \
    >"$WORK/$name.err" 2>&1 &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    if grep -aqE "$marker" "$log"; then
      printf '%s: reached after %ss\n' "$name" "$((SECONDS - start))"
      pkill -P "$pid" 2>/dev/null; kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
      return 0
    fi
    if [ "$sampled" = 0 ] && [ $((SECONDS - start)) -ge 60 ]; then
      sampled=1
      sample_guest "$socket" "$name"
    fi
    sleep 1
  done
  wait "$pid"
  printf '%s: not reached in %ss, exit %s; serial bytes %s; last output:\n' \
    "$name" "$((SECONDS - start))" "$?" "$(wc -c < "$log")"
  tail -c 300 "$log" | tr -cd '[:print:]\n'
  printf '\n'
  tail -n 5 "$WORK/$name.err"
  return 0
}

kernel_boot() {
  try "kernel-$1" 'Run /init as init process|Freeing unused kernel' -machine q35,smm=off -accel "$2" \
    -cpu "$3" -L /usr/share/qemu -bios "$FIRMWARE/seabios.bin" -kernel "/boot/vmlinuz-$(uname -r)" -initrd "/boot/initrd.img-$(uname -r)" \
    -append 'console=ttyS0 panic=-1'
}

# Without a boot device the firmware reaches its boot manager, so this covers OVMF up to the boot.
# This OVMF build requires SMM, as Incus runs it; turning SMM off crashes QEMU on any CPU.
firmware_boot() {
  local name="firmware-$1" accel="$2" cpu="$3"
  cp "$FIRMWARE/OVMF_VARS.4MB.fd" "$WORK/$name.vars"
  try "$name" 'BdsDxe|Shell>|UEFI Interactive Shell' -machine q35,smm=on -accel "$accel" -cpu "$cpu" \
    -global driver=cfi.pflash01,property=secure,value=on \
    -drive "if=pflash,format=raw,readonly=on,file=$FIRMWARE/OVMF_CODE.4MB.fd" \
    -drive "if=pflash,format=raw,file=$WORK/$name.vars"
}

reload_kvm() {
  local vendor=kvm_intel
  grep -qw svm /proc/cpuinfo && vendor=kvm_amd
  modprobe -r "$vendor" kvm && modprobe kvm "$1" && modprobe "$vendor" "$2"
}

incus_stack() {
  local start=$SECONDS
  incus profile device get default root pool </dev/null >/dev/null 2>&1 || incus admin init --auto </dev/null
  incus launch images:debian/13/cloud l3-stack --vm -c limits.cpu=1 -c limits.memory=1GiB \
    -c security.secureboot=false </dev/null || { printf 'incus-stack: launch failed\n'; return 0; }
  until incus exec l3-stack -- true </dev/null >/dev/null 2>&1; do
    if [ $((SECONDS - start)) -ge "$LIMIT" ]; then
      printf 'incus-stack: no agent in %ss; console:\n' "$LIMIT"
      incus console l3-stack --show-log </dev/null 2>&1 | tail -n 10 | cut -c1-160
      incus delete --force l3-stack </dev/null
      return 0
    fi
    sleep 3
  done
  printf 'incus-stack: agent after %ss\n' "$((SECONDS - start))"
  incus delete --force l3-stack </dev/null
}

mountpoint -q /sys/kernel/debug || mount -t debugfs debugfs /sys/kernel/debug 2>/dev/null || true
facts
if [ "${2:-}" = firmware ]; then
  firmware_boot host kvm host
  exit 0
fi
# Direct kernel boot needs QEMU's option ROMs, which the Incus packages leave out.
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends qemu-system-data </dev/null >/dev/null
incus_stack
kernel_boot host kvm host
kernel_boot tcg tcg qemu64
firmware_boot host kvm host
firmware_boot qemu64 kvm qemu64
firmware_boot tcg tcg qemu64
if grep -qw svm /proc/cpuinfo && reload_kvm tdp_mmu=Y npt=0; then
  printf 'with kvm_amd npt=0\n'
  kernel_boot npt0 kvm host
  firmware_boot npt0 kvm host
fi
if reload_kvm tdp_mmu=N ''; then
  printf 'with kvm tdp_mmu=N\n'
  kernel_boot notdpmmu kvm host
  firmware_boot notdpmmu kvm host
fi
reload_kvm tdp_mmu=Y '' || true

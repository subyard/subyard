#!/bin/bash
# Runs inside the dedicated VPN VM and shares that guest's network namespace.
set -euo pipefail
export WG_QUICK_USERSPACE_IMPLEMENTATION=amneziawg-go
export AWG_QUICK_USERSPACE_IMPLEMENTATION=amneziawg-go
cleanup() { awg-quick down /etc/amnezia/awg0.conf >/dev/null 2>&1 || true; }
trap cleanup EXIT
trap 'exit 0' TERM INT
awg-quick up /etc/amnezia/awg0.conf >/dev/null 2>&1
while :; do sleep 3600 & wait "$!" || true; done

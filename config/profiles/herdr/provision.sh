#!/usr/bin/env bash
# subyard-provision-check-v1
set -euo pipefail

linger() { [ "$(loginctl show-user "$DEV_USER" -p Linger --value)" = yes ]; }
case "$*" in
  --check) linger || exit 10 ;;
  '') linger || loginctl enable-linger "$DEV_USER" ;;
  *) exit 2 ;;
esac

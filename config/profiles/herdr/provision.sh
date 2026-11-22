#!/usr/bin/env bash
# subyard-provision-check-v1
set -euo pipefail

[ "${YARD_KIND:-container}" = container ] || exit 0
profile=$(dirname "${BASH_SOURCE[0]}")
linger() { [ "$(loginctl show-user "$DEV_USER" -p Linger --value)" = yes ]; }
case "$*" in
  --check) linger && sh "$profile/guest-ssh.sh" check || exit 10 ;;
  '')
    linger || loginctl enable-linger "$DEV_USER"
    sh "$profile/guest-ssh.sh" ensure ;;
  *) exit 2 ;;
esac

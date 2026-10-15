#!/usr/bin/env bash
# Installed-only release refresh through the existing owner/guest transport.
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
selected=0
case " $(printf '%s' "${ENVIRONMENT_PROFILES:-}" | tr '[:space:]' ' ') " in *' git-fetch '*) selected=1 ;; esac
export SUBYARD_PROFILE_SELECTED="$selected"
binding="$({
  sha256sum "$profile/"{profile.json,runtime.sh,owner.sh,guest.py,fetch.py,subyard-git-fetch.service.in,subyard-git-fetch.timer} | cut -d ' ' -f1
  printf '%s\0' "${SUBYARD_YARD:-default}" "$INCUS_PROJECT" "$YARD_INSTANCE_NAME" "$DEV_USER"
} | sha256sum | cut -d ' ' -f1)"
case "${1:-}" in
  observe) [ "$#" = 1 ]; exec "$profile/owner.sh" --observe "$binding" ;;
  apply) [ "$#" = 4 ]; exec "$profile/owner.sh" --refresh "$binding" "${@:2}" ;;
  *) exit 2 ;;
esac

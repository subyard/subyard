#!/usr/bin/env bash
set -euo pipefail
uid=$(id -u)
# Core starts its server units in the caller directory, which must be readable by this user.
cd "$HOME"
exec env -i HOME="$HOME" USER="$(id -un)" LOGNAME="$(id -un)" \
  PATH=/usr/local/bin:/usr/bin:/bin SHELL=/bin/bash \
  XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
  SUBYARD_HERDR_CLEAN_START=1 /bin/bash -lc 'exec "$HOME/.local/bin/herdr-core" "$@"' herdr "$@"

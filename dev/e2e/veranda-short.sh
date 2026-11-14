#!/usr/bin/env bash
# Veranda X11/Wayland SHORT inside one leased disposable VM, with the GUI and engine ELFs the
# candidate bundle carries as .subyard-acceptance/veranda and .subyard-acceptance/yard.
# Usage, through dev/agent-e2e.sh: veranda-short.sh [wayland-short.py options...]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [ "${SUBYARD_E2E_VM:-}" != 1 ] || [ ! -r /run/subyard-e2e-lease.json ]; then
  printf 'veranda-short: run through dev/agent-e2e.sh on an allocated VM\n' >&2
  exit 2
fi
for input in veranda yard; do
  if [ ! -f "$root/.subyard-acceptance/$input" ] || [ -L "$root/.subyard-acceptance/$input" ]; then
    printf 'veranda-short: the candidate bundle lacks .subyard-acceptance/%s\n' "$input" >&2
    exit 2
  fi
done
cd "$root"
operator="$(id -un)"
operator_home="$(getent passwd "$operator" | cut -d: -f6)"
bash dev/build-profiles.sh
. tests/helpers/test-context.sh
setup_test_context "$root/.build/incus-bootstrap"
export SUBYARD_USER="$operator" SUBYARD_OPERATOR_HOME="$operator_home"
export STORAGE_PATH="$operator_home/.cache/subyard-e2e-platform/incus/incus/storage"
SUBYARD_INSTALLATION_CONTEXT="$(test_installation_context default)"
export SUBYARD_INSTALLATION_CONTEXT
# The product Phase 1 installer prepares Incus and enrolls the operator; the helper then starts
# in a fresh session that holds the new group.
bash scripts/01-install-incus.sh --yes --zabbly
sudo -n runuser -u "$operator" -- env -i \
  HOME="$operator_home" USER="$operator" LOGNAME="$operator" PATH=/usr/local/bin:/usr/bin:/bin TMPDIR=/tmp \
  SUBYARD_E2E_VM="$SUBYARD_E2E_VM" SUBYARD_E2E_RUN_ID="${SUBYARD_E2E_RUN_ID:-}" SUBYARD_E2E_SLOT="${SUBYARD_E2E_SLOT:-}" \
  python3 "$root/veranda/desktop/wayland-short.py" \
    "$root/.subyard-acceptance/veranda" "$root/.subyard-acceptance/yard" "$@"

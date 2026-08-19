#!/usr/bin/env bash
# Real-parser contract: changing the production unit back to a oneshot forced-restart
# service must fail verification on the oldest systemd behavior that rejected it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$ROOT/config/systemd/subyard-power-reconcile.service.in"
V072_TEMPLATE="$ROOT/tests/fixtures/systemd/subyard-power-reconcile-v0.7.2.service.in"

fail() {
  printf 'power-reconciler-systemd: %s\n' "$*" >&2
  exit 1
}

command -v systemd-analyze >/dev/null 2>&1 \
  || fail 'systemd-analyze is required for the real unit parser contract'
[ -r "$TEMPLATE" ] || fail "unit template is unavailable: $TEMPLATE"
[ -r "$V072_TEMPLATE" ] || fail "v0.7.2 unit fixture is unavailable: $V072_TEMPLATE"
grep -Fxq 'Wants=network-online.target incus.service incus.socket' "$TEMPLATE" \
  || fail 'production reconciler does not start Incus before using its socket'

systemd_version="$(systemd-analyze --version | awk 'NR == 1 { print $2 }')"
[[ "$systemd_version" =~ ^[0-9]+$ ]] \
  || fail "cannot identify systemd version: $systemd_version"

temporary="$(mktemp -d)"
cleanup() {
  find "$temporary" -depth -delete
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

unit="$temporary/subyard-power-reconcile.service"
v072_unit="$temporary/subyard-power-reconcile-v0.7.2.service"
diagnostics="$temporary/systemd-analyze.stderr"
sed 's|@SUBYARD_POWER_RECONCILER@|/bin/true|g' "$TEMPLATE" > "$unit"
sed 's|@SUBYARD_POWER_RECONCILER@|/bin/true|g' "$V072_TEMPLATE" > "$v072_unit"

if [ "$systemd_version" -lt 256 ]; then
  if systemd-analyze verify "$v072_unit" 2>"$diagnostics"; then
    fail "systemd $systemd_version unexpectedly accepted the incompatible v0.7.2 unit"
  fi
  grep -Fq "RestartForceExitStatus= set, which isn't allowed for Type=oneshot services" \
    "$diagnostics" || {
      sed -n '1,160p' "$diagnostics" >&2
      fail "systemd $systemd_version rejected the v0.7.2 fixture for an unexpected reason"
    }
fi

if ! systemd-analyze verify "$unit" 2>"$diagnostics"; then
  sed -n '1,160p' "$diagnostics" >&2
  fail "systemd $systemd_version rejected the production power reconciler unit"
fi

if [ "$systemd_version" -ge 256 ]; then
  printf 'ok: systemd %s accepts the unit; CI systemd 255 owns the compatibility regression\n' \
    "$systemd_version"
else
  printf 'ok: systemd %s rejects v0.7.2 and accepts the compatible power reconciler unit\n' \
    "$systemd_version"
fi

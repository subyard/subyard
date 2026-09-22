#!/usr/bin/env bash
# Real-parser contract: changing the production unit back to a oneshot forced-restart
# service must fail verification on the oldest systemd behavior that rejected it.
# Installer contract: upgrades prepare the network lock before enabling the unit.
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

# An upgraded host may not have the network lock yet. Installing the new boot
# reconciler must make managed starts usable immediately, before the next boot.
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$temporary/install"
  fake_bin="$temporary/install/bin"
  mkdir -p "$fake_bin"
  export SUBYARD_POWER_ENGINE_SOURCE="$temporary/install/engine"
  export SUBYARD_POWER_LIBEXEC_DIR="$temporary/install/libexec"
  export SUBYARD_POWER_RECONCILER_PATH="$SUBYARD_POWER_LIBEXEC_DIR/yard-boot-reconcile"
  export SUBYARD_POWER_UNIT_PATH="$temporary/install/power.service"
  export TEST_NETWORK_LOCK="$temporary/install/network.lock"
  export TEST_SYSTEMCTL_LOG="$temporary/install/systemctl.log"
  export TEST_NETWORK_LOCK_FAIL=0

  cat > "$SUBYARD_POWER_ENGINE_SOURCE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$*" = '_network-lock ensure' ] || exit 64
[ "$TEST_NETWORK_LOCK_FAIL" = 0 ] || exit 1
: > "$TEST_NETWORK_LOCK"
EOF
  cat > "$fake_bin/id" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = -u ]; then printf '0\n'; else exec /usr/bin/id "$@"; fi
EOF
  cat > "$fake_bin/install" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|-g) shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
exec /usr/bin/install "${args[@]}"
EOF
  cat > "$fake_bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$TEST_SYSTEMCTL_LOG"
case "$1" in
  is-enabled) exit 1 ;;
  enable)
    [ -f "$TEST_NETWORK_LOCK" ] || {
      printf 'network lock unavailable before enabling the new reconciler\n' >&2
      exit 1
    }
    ;;
esac
EOF
  chmod +x "$SUBYARD_POWER_ENGINE_SOURCE" "$fake_bin"/*
  export PATH="$fake_bin:$PATH"
  if ! bash "$ROOT/scripts/install-power-reconciler.sh" --yes \
      > "$temporary/install/stdout" 2> "$temporary/install/stderr"; then
    cat "$temporary/install/stderr" >&2
    fail 'install did not prepare the network lock before enabling boot reconciliation'
  fi
  [ -f "$TEST_NETWORK_LOCK" ] || fail 'install left the upgraded host without a network lock'

  export TEST_NETWORK_LOCK_FAIL=1
  : > "$TEST_SYSTEMCTL_LOG"
  if bash "$ROOT/scripts/install-power-reconciler.sh" --yes \
      > "$temporary/install/stdout" 2> "$temporary/install/stderr"; then
    fail 'install ignored a network lock initialization failure'
  fi
  ! grep -q '^enable ' "$TEST_SYSTEMCTL_LOG" \
    || fail 'install enabled boot reconciliation after lock initialization failed'
)
printf 'ok: upgrades initialize the network lock before enabling boot reconciliation\n'

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

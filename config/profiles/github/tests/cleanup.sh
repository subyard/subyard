#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Interrupted init may leave a running instance before the developer account exists.
# Broker cleanup must not prevent teardown of that instance.
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$TMP/partial-init"
  export SUBYARD_PROFILE_SELECTED=0 SUBYARD_YARD=default
  export INCUS_LOG="$TMP/partial-init/incus.log"
  mkdir -p "$TMP/bin"
  cat > "$TMP/bin/incus" <<'INCUS'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$INCUS_LOG"
case "$*" in
  'exec yard --project subyard -- true') exit 0 ;;
  'exec yard --project subyard -- getent passwd dev') exit 2 ;;
  'exec yard --project subyard -- rm -f -- /usr/local/libexec/subyard/github-client') exit 0 ;;
  *) exit 99 ;;
esac
INCUS
  chmod +x "$TMP/bin/incus"
  export PATH="$TMP/bin:$PATH"
  bash "$ROOT/config/profiles/github/owner.sh" --check
  bash "$ROOT/config/profiles/github/owner.sh" --remove
  ! grep -Fq 'bash -euo pipefail' "$INCUS_LOG" \
    || fail 'broker cleanup invoked the profile hook without a developer account'
  grep -Fq 'rm -f -- /usr/local/libexec/subyard/github-client' "$INCUS_LOG" \
    || fail 'broker cleanup skipped the guest engine in a partial init'
)

# Ownership and inactive-state decisions stay with the profile's real hook.
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$TMP/pause"
  export SUBYARD_YARD=fixture
  export SERVICE_STATE="$TMP/pause/state" SERVICE_LOG="$TMP/pause/service.log"
  cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --user ] || exit 2
case "$2" in
  show) cat "$SERVICE_STATE" ;;
  stop|start) printf '%s\n' "$2" >> "$SERVICE_LOG" ;;
  *) exit 2 ;;
esac
SH
  chmod +x "$TMP/bin/systemctl"
  export PATH="$TMP/bin:$PATH"
  unit="$SUBYARD_OPERATOR_HOME/.config/systemd/user/subyard-github-fixture.service"
  mkdir -p "$(dirname "$unit")"
  printf 'unmanaged\n' > "$unit"
  printf 'active\n' > "$SERVICE_STATE"
  : > "$SERVICE_LOG"
  bash "$ROOT/config/profiles/github/owner.sh" --pause
  bash "$ROOT/config/profiles/github/owner.sh" --resume
  [ ! -s "$SERVICE_LOG" ] || fail 'unmanaged service was mutated'
  printf '# Managed by Subyard GitHub broker\n' > "$unit"
  printf 'inactive\n' > "$SERVICE_STATE"
  [ -z "$(bash "$ROOT/config/profiles/github/owner.sh" --pause)" ] || fail 'inactive service was paused'
  [ ! -s "$SERVICE_LOG" ] || fail 'inactive service was mutated'
  printf 'active\n' > "$SERVICE_STATE"
  [ "$(bash "$ROOT/config/profiles/github/owner.sh" --pause)" = paused ] || fail 'active service was not paused'
  bash "$ROOT/config/profiles/github/owner.sh" --resume
  [ "$(cat "$SERVICE_LOG")" = "$(printf 'stop\nstart')" ] || fail 'owned service pause/resume failed'
)

printf 'ok: GitHub partial-init cleanup\n'

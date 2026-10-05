#!/usr/bin/env bash
# Active UFW converges only when its persisted bridge rules and the NetworkManager guard match.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home"
export SUBYARD_NO_AUDIT=1
export SUBYARD_UFW_RULES_FILE="$TMP/user.rules"
mkdir -p "$HOME"
cat > "$SUBYARD_UFW_RULES_FILE" <<'RULES'
### tuple ### allow udp 67 0.0.0.0/0 any 0.0.0.0/0 in_incusbr0
### tuple ### allow any 53 0.0.0.0/0 any 0.0.0.0/0 in_incusbr0
### tuple ### route:allow any any 0.0.0.0/0 any 0.0.0.0/0 in_incusbr0
### tuple ### route:allow any any 0.0.0.0/0 any 0.0.0.0/0 out_incusbr0
RULES

# shellcheck source=scripts/lib/host.sh
. "$ROOT/scripts/lib/host.sh"

ufw_yard_rules_present incusbr0 || fail "matching active-UFW rules were not converged"
sed -i '/out_incusbr0/d' "$SUBYARD_UFW_RULES_FILE"
! ufw_yard_rules_present incusbr0 || fail "missing UFW route-out rule was accepted"
printf '%s\n' '### tuple ### route:allow any any 0.0.0.0/0 any 0.0.0.0/0 out_incusbr0' \
  >> "$SUBYARD_UFW_RULES_FILE"

access_log="$TMP/access.log"
getent() { [ "${1:-}" = group ] && [ "${2:-}" = incus-admin ]; }
chgrp() { printf 'chgrp %s %s\n' "$1" "$2" >>"$access_log"; }
chmod() { printf 'chmod %s %s\n' "$1" "$2" >>"$access_log"; }
ufw_rules_set_probe_access enable || fail "could not enable UFW probe access"
ufw_rules_set_probe_access disable || fail "could not restore root-only UFW access"
grep -Fqx "chgrp incus-admin $SUBYARD_UFW_RULES_FILE" "$access_log" \
  || fail "UFW probe access did not use incus-admin"
grep -Fqx "chgrp root $SUBYARD_UFW_RULES_FILE" "$access_log" \
  || fail "UFW teardown access did not restore root ownership"
unset -f getent chgrp chmod

mkdir -p "$TMP/bin"
cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "is-active --quiet ufw") [ "${MOCK_UFW_ACTIVE:-0}" = 1 ] ;;
  "is-active NetworkManager")
    case "${MOCK_NM_STATE:-inactive}" in
      active) printf 'active\n'; exit 0 ;;
      inactive) printf 'inactive\n'; exit 3 ;;
      *) printf '%s\n' "$MOCK_NM_STATE"; exit 3 ;;
    esac
    ;;
  *) exit 90 ;;
esac
SH
cat > "$TMP/bin/yard-engine" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "_network-lock check")
    [ "${MOCK_REQUIRE_NOAUTH:-0}" != 1 ] || [ "${SUBYARD_SUDO_PREAUTHORIZED:-0}" != 1 ] || exit 1
    [ -z "${MOCK_NETWORK_VERIFY_LOG:-}" ] || printf 'lock-check\n' >> "$MOCK_NETWORK_VERIFY_LOG"
    [ "${MOCK_NETWORK_LOCK_CHECK:-ok}" = ok ] ;;

  "_network-lock ensure") exit 0 ;;
  *) exit 90 ;;
esac
SH
cat > "$TMP/bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  info) exit 0 ;;
  "info yard --project subyard")
    [ "${MOCK_INSTANCE_EXISTS:-1}" = 1 ]
    ;;
  "list yard --project subyard -f csv -c s")
    printf '%s\n' "${MOCK_INSTANCE_STATE:-STOPPED}"
    ;;
  "list yard --project subyard -c4 -fcsv")
    if [ -n "${MOCK_INSTANCE_IP_AFTER_FILE:-}" ]; then
      attempts=0
      [ ! -e "$MOCK_INSTANCE_IP_AFTER_FILE" ] \
        || attempts="$(cat "$MOCK_INSTANCE_IP_AFTER_FILE")"
      attempts=$((attempts + 1))
      printf '%s\n' "$attempts" > "$MOCK_INSTANCE_IP_AFTER_FILE"
      [ "$attempts" -lt 2 ] || printf '%s\n' "${MOCK_INSTANCE_IP:-10.0.0.2}"
      exit 0
    fi
    printf '%s\n' "${MOCK_INSTANCE_IP:-}"
    ;;
  *) exit 90 ;;
esac
SH
cat > "$TMP/bin/ip" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "-4 route show default") printf '%s\n' "${MOCK_DEFAULT_ROUTE:-default via 192.0.2.1 dev eth0}" ;;
  *) exit 90 ;;
esac
SH
chmod +x "$TMP/bin/"*
export PATH="$TMP/bin:$PATH"
export SUBYARD_DISPATCHER_PATH="$TMP/bin/yard-engine"
export MOCK_NM_STATE=inactive MOCK_INSTANCE_EXISTS=1 MOCK_INSTANCE_STATE=STOPPED
export MOCK_INSTANCE_IP='' MOCK_DEFAULT_ROUTE='default via 192.0.2.1 dev eth0'

if MOCK_NETWORK_LOCK_CHECK=fail bash "$ROOT/scripts/06-network.sh" --verify; then
  fail "network verification ignored host-lock validation failure"
fi

SUBYARD_SUDO_PREAUTHORIZED=1 MOCK_REQUIRE_NOAUTH=1 bash "$ROOT/scripts/06-network.sh" --check \
  || fail "read-only network planning retained authorization for privileged readers"

# The network stage owns host guards, not desired-power reconciliation. A stopped instance is safe
# here even when the later init finalizer still needs to restore desired=running.
SUBYARD_POWER_DESIRED=running bash "$ROOT/scripts/06-network.sh" --verify \
  || fail "stopped desired-running instance did not converge after network apply"
SUBYARD_POWER_DESIRED=stopped bash "$ROOT/scripts/06-network.sh" --verify \
  || fail "stopped desired-stopped instance did not converge after network apply"

MOCK_INSTANCE_STATE=RUNNING MOCK_INSTANCE_IP=10.0.0.2 \
  SUBYARD_POWER_DESIRED=running bash "$ROOT/scripts/06-network.sh" --verify \
  || fail "running instance with an address did not converge"
address_attempts="$TMP/address-attempts"
MOCK_INSTANCE_STATE=RUNNING MOCK_INSTANCE_IP='' \
  MOCK_INSTANCE_IP_AFTER_FILE="$address_attempts" \
  SUBYARD_NETWORK_ADDRESS_ATTEMPTS=2 SUBYARD_POWER_DESIRED=running \
  bash "$ROOT/scripts/06-network.sh" --verify \
  || fail "post-apply verify did not wait for the running instance address"
[ "$(cat "$address_attempts")" = 2 ] \
  || fail "post-apply verify did not retry the running instance address"
if MOCK_INSTANCE_STATE=RUNNING MOCK_INSTANCE_IP='' \
  SUBYARD_NETWORK_ADDRESS_ATTEMPTS=1 SUBYARD_POWER_DESIRED=running \
  bash "$ROOT/scripts/06-network.sh" --verify; then
  fail "running instance without an address converged"
fi

if MOCK_INSTANCE_STATE=STOPPED MOCK_DEFAULT_ROUTE='default dev incusbr0' \
  SUBYARD_POWER_DESIRED=running bash "$ROOT/scripts/06-network.sh" --verify; then
  fail "stopped instance bypassed the unsafe host-route guard"
fi
if MOCK_NM_STATE=unexpected MOCK_INSTANCE_STATE=STOPPED \
  SUBYARD_POWER_DESIRED=running bash "$ROOT/scripts/06-network.sh" --verify; then
  fail "stopped instance bypassed the unknown NetworkManager-state guard"
fi

if MOCK_INSTANCE_EXISTS=0 bash "$ROOT/scripts/06-network.sh" --check; then
  fail "pre-apply network check accepted an absent instance"
fi
MOCK_INSTANCE_EXISTS=0 bash "$ROOT/scripts/06-network.sh" --verify \
  || fail "fresh-init network verify rejected an instance created by a later stage"

# Model stale group credentials at the filesystem permission port. The real
# leaf and require_root execute unchanged; the sudo port performs its exact env/argv retry.
cat > "$TMP/bin/id" <<'SH'
#!/usr/bin/env bash
if [ "$*" = -u ]; then
  [ "${SUBYARD_ELEVATED:-0}" = 1 ] && printf '0\n' || printf '1000\n'
else
  exec /usr/bin/id "$@"
fi
SH
cat > "$TMP/bin/ufw" <<'SH'
#!/usr/bin/env bash
printf 'unexpected-ufw-write\n' >> "$MOCK_NETWORK_VERIFY_LOG"
exit 90
SH
cat > "$TMP/bin/sudo" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = -n ] || exit 90
shift
if [ "$*" = true ]; then
  printf 'sudo-validation\n' >> "$MOCK_NETWORK_VERIFY_LOG"
  [ "${MOCK_SUDO_EXPIRED:-0}" != 1 ]
  exit
fi
[ "$1" = -- ] || exit 90
shift
[ "$1" = env ] || exit 90
printf 'sudo-readback\n' >> "$MOCK_NETWORK_VERIFY_LOG"
exec "$@"
SH
chmod 0700 "$TMP/bin/id" "$TMP/bin/ufw" "$TMP/bin/sudo"
function [() {
  if builtin [ "$#" = 4 ] && builtin [ "$1" = '!' ] && builtin [ "$2" = -r ] \
    && builtin [ "$3" = "$SUBYARD_UFW_RULES_FILE" ] \
    && builtin [ "${SUBYARD_ELEVATED:-0}" != 1 ]; then
    return 0
  fi
  if builtin [ "$#" = 3 ] && builtin [ "$1" = -r ] \
    && builtin [ "$2" = "$SUBYARD_UFW_RULES_FILE" ] \
    && builtin [ "${SUBYARD_ELEVATED:-0}" != 1 ]; then
    return 1
  fi
  builtin [ "$@"
}
export -f '['
export MOCK_UFW_ACTIVE=1 MOCK_NETWORK_VERIFY_LOG="$TMP/verify.log"
# The existing privilege handoff forwards only approved context; preserve the
# test-owned readonly rule path through the port, not a product env exception.
cat > "$TMP/bin/env" <<'SH'
#!/usr/bin/env bash
exec /usr/bin/env "SUBYARD_UFW_RULES_FILE=$SUBYARD_UFW_RULES_FILE" "$@"
SH
chmod 0700 "$TMP/bin/env"
before_rules="$(sha256sum "$SUBYARD_UFW_RULES_FILE")"
: > "$MOCK_NETWORK_VERIFY_LOG"
SUBYARD_SUDO_PREAUTHORIZED=1 bash "$ROOT/scripts/06-network.sh" --verify \
  || fail "authorized readonly UFW verification did not preserve the approved leaf"
[ "$(cat "$MOCK_NETWORK_VERIFY_LOG")" = $'lock-check\nsudo-validation\nsudo-readback\nlock-check' ] \
  || fail "UFW readback elevated before original lock validation or invoked a mutation"
[ "$(sha256sum "$SUBYARD_UFW_RULES_FILE")" = "$before_rules" ] \
  || fail "readonly verification changed persisted rules"
for refusal in check noauth expired lock; do
  : > "$MOCK_NETWORK_VERIFY_LOG"
  case "$refusal" in
    check) leaf_mode=--check; auth=1; expired=0; lock=ok ;;
    noauth) leaf_mode=--verify; auth=0; expired=0; lock=ok ;;
    expired) leaf_mode=--verify; auth=1; expired=1; lock=ok ;;
    lock) leaf_mode=--verify; auth=1; expired=0; lock=fail ;;
  esac
  if SUBYARD_SUDO_PREAUTHORIZED="$auth" MOCK_SUDO_EXPIRED="$expired" \
    MOCK_NETWORK_LOCK_CHECK="$lock" bash "$ROOT/scripts/06-network.sh" "$leaf_mode" >/dev/null 2>&1; then
    fail "$refusal allowed unreadable active-UFW convergence"
  fi
  ! grep -Fq sudo-readback "$MOCK_NETWORK_VERIFY_LOG" \
    || fail "$refusal escalated the leaf"
done
[ "$(sha256sum "$SUBYARD_UFW_RULES_FILE")" = "$before_rules" ] \
  || fail "refused verification changed persisted rules"
unset -f '['
printf 'ok: readonly UFW readback preserves native lock checks and authorization\n'

printf 'ok: network leaf owns guards independently from desired power\n'

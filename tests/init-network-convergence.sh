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
  "is-active --quiet ufw") exit 3 ;;
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
  "_network-lock check") [ "${MOCK_NETWORK_LOCK_CHECK:-ok}" = ok ] ;;
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

printf 'ok: network leaf owns guards independently from desired power\n'

#!/usr/bin/env bash
# Host-free contract for the Hermes owner-side browser route.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE="$ROOT/config/profiles/hermes"
DESCRIPTOR="$PROFILE/resources/dashboard.res"
HANDLER="$PROFILE/resources/dashboard/handler.sh"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -x "$HANDLER" ] || fail 'dashboard route handler is missing or not executable'
[ -r "$DESCRIPTOR" ] || fail 'dashboard route descriptor is missing'

grep -Fqx 'PROXY="hermes-dashboard HERMES_DASHBOARD_ADVERTISE_HOST HERMES_DASHBOARD_HOST_PORT tcp:127.0.0.1:9119 owner-metadata-v1 tailscale-only"' \
  "$DESCRIPTOR" || fail 'descriptor does not declare the exact owned proxy contract'
grep -Fq 'ACTION="up up security-change reversible"' "$DESCRIPTOR" \
  || fail 'route publication is not a reversible security change'
grep -Fq 'ACTION="down down security-change reversible"' "$DESCRIPTOR" \
  || fail 'route withdrawal is not a reversible security change'
if grep -Eq 'ACTION="(start|setup|logs)|service|auth|provider|telegram|whisper|cron' \
  "$DESCRIPTOR"; then
  fail 'descriptor crosses the infrastructure/application boundary'
fi

if grep -Eq '/srv/hermes|/home/dev/.hermes|\.env|config\.yaml|python|systemctl|journalctl|nft|provider|telegram|whisper|cron|HERMES_[A-Z_]*AUTH' \
  "$HANDLER"; then
  fail 'route handler knows about Hermes configuration, components or processes'
fi
grep -Fq 'tailscale ip -4' "$HANDLER" \
  || fail 'route does not constrain publication to active Tailscale IPv4'
grep -Fq 'listen=tcp:$HERMES_OWNER_IP:$HERMES_DASHBOARD_HOST_PORT' "$HANDLER" \
  || fail 'route does not publish the exact selected owner address'
grep -Fq 'connect=tcp:127.0.0.1:$HERMES_GUEST_PORT' "$HANDLER" \
  || fail 'route does not target the guest loopback endpoint'
grep -Fq 'bind=host' "$HANDLER" || fail 'route does not use host-side binding'
grep -Fq 'user.subyard.resource.hermes-dashboard' "$HANDLER" \
  || fail 'route does not persist owner-side device ownership'

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home" PATH="$TMP/bin:$PATH"
export HERMES_DASHBOARD_ADVERTISE_HOST=owner.example-tailnet.ts.net
export HERMES_DASHBOARD_HOST_PORT=19119
export HERMES_ROUTE_TEST_ROOT="$TMP"
mkdir -p "$TMP/bin"
touch "$TMP/guest-ready"

cat >"$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
root="${HERMES_ROUTE_TEST_ROOT:?}"
printf '%s\n' "$*" >>"$root/incus.log"
case "${1:-}" in
  info) exit 0 ;;
  list) printf 'RUNNING\n' ;;
  config)
    case "${2:-}" in
      device)
        case "${3:-}" in
          list) [ ! -f "$root/route" ] || cat "$root/route-name" ;;
          get)
            [ -f "$root/route" ] || exit 1
            [ "${5:-}" = "$(cat "$root/route-name")" ] || exit 1
            case "${6:-}" in
              type) sed -n '1p' "$root/route" ;;
              listen) sed -n '2p' "$root/route" ;;
              connect) sed -n '3p' "$root/route" ;;
              bind) sed -n '4p' "$root/route" ;;
              proxy_protocol) [ ! -f "$root/route-extra" ] || printf 'true\n' ;;
            esac
            ;;
          show)
            [ -f "$root/route" ] || exit 0
            printf '%s:\n' "$(cat "$root/route-name")"
            printf '  bind: %s\n' "$(sed -n '4p' "$root/route")"
            printf '  connect: %s\n' "$(sed -n '3p' "$root/route")"
            printf '  listen: %s\n' "$(sed -n '2p' "$root/route")"
            [ ! -f "$root/route-extra" ] || printf '  proxy_protocol: true\n'
            printf '  type: %s\n' "$(sed -n '1p' "$root/route")"
            ;;
          add)
            [ ! -f "$root/fail-add" ] || exit 1
            listen= connect= bind=
            for argument in "$@"; do
              case "$argument" in
                listen=*) listen="${argument#listen=}" ;;
                connect=*) connect="${argument#connect=}" ;;
                bind=*) bind="${argument#bind=}" ;;
              esac
            done
            printf '%s\n' "${5:-}" >"$root/route-name"
            printf '%s\n%s\n%s\n%s\n' "${6:-}" "$listen" "$connect" "$bind" >"$root/route"
            ;;
          remove)
            [ ! -f "$root/fail-remove" ] || exit 1
            [ "${5:-}" = "$(cat "$root/route-name")" ] || exit 1
            rm -f -- "$root/route" "$root/route-name" "$root/route-extra"
            ;;
        esac
        ;;
      get) [ ! -f "$root/ownership" ] || cat "$root/ownership" ;;
      set)
        [ ! -f "$root/fail-marker-set" ] || exit 1
        case "${5:-}" in
          v1:pending:*) ;;
          *) [ ! -f "$root/fail-active-marker" ] || exit 1 ;;
        esac
        printf '%s\n' "${5:-}" >"$root/ownership"
        ;;
      unset)
        [ ! -f "$root/fail-marker-unset" ] || exit 1
        rm -f -- "$root/ownership"
        ;;
    esac
    ;;
  exec)
    case " $* " in
      *' ss -Hltn '*)
        [ -f "$root/guest-ready" ] || exit 1
        if [ -f "$root/guest-wildcard" ]; then
          printf 'LISTEN 0 5 0.0.0.0:9119 0.0.0.0:*\n'
        else
          printf 'LISTEN 0 5 127.0.0.1:9119 0.0.0.0:*\n'
        fi
        ;;
      *) exit 1 ;;
    esac
    ;;
esac
MOCK

cat >"$TMP/bin/tailscale" <<'MOCK'
#!/usr/bin/env sh
root="${HERMES_ROUTE_TEST_ROOT:?}"
[ "${1:-} ${2:-}" = 'ip -4' ] || exit 2
[ ! -f "$root/tailscale-fail" ] || exit 1
if [ -f "$root/tailscale-ips" ]; then cat "$root/tailscale-ips"; else printf '100.64.1.20\n'; fi
MOCK
cat >"$TMP/bin/getent" <<'MOCK'
#!/usr/bin/env sh
[ "${1:-}" = ahostsv4 ] || exit 2
root="${HERMES_ROUTE_TEST_ROOT:?}"
if [ -f "$root/dns-ips" ]; then
  while IFS= read -r address; do printf '%s STREAM %s\n' "$address" "${2:-}"; done <"$root/dns-ips"
else
  printf '100.64.1.20 STREAM %s\n' "${2:-}"
fi
[ ! -f "$root/dns-extra" ] || printf '203.0.113.10 STREAM %s\n' "${2:-}"
MOCK
cat >"$TMP/bin/ip" <<'MOCK'
#!/usr/bin/env sh
root="${HERMES_ROUTE_TEST_ROOT:?}"
if [ -f "$root/active-ips" ]; then cat "$root/active-ips"; else printf 'tailscale0 UP 100.64.1.20/32\n'; fi
MOCK
cat >"$TMP/bin/ss" <<'MOCK'
#!/usr/bin/env sh
[ ! -f "${HERMES_ROUTE_TEST_ROOT:?}/port-occupied" ] \
  || printf 'LISTEN 0 5 100.64.1.20:19119 0.0.0.0:*\n'
exit 0
MOCK
cat >"$TMP/bin/timeout" <<'MOCK'
#!/usr/bin/env sh
[ ! -f "${HERMES_ROUTE_TEST_ROOT:?}/owner-not-ready-remove-route" ] \
  || { rm -f -- "$HERMES_ROUTE_TEST_ROOT/route" "$HERMES_ROUTE_TEST_ROOT/route-name"; exit 1; }
[ ! -f "${HERMES_ROUTE_TEST_ROOT:?}/owner-not-ready" ] || exit 1
[ -f "${HERMES_ROUTE_TEST_ROOT:?}/route" ]
MOCK
cat >"$TMP/bin/sleep" <<'MOCK'
#!/usr/bin/env sh
exit 0
MOCK
chmod 0755 "$TMP/bin/"*

SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/up-plan.json"
grep -Fq '"action":"up","changed":true' "$TMP/up-plan.json" \
  || fail 'up prepare did not describe the route change'
[ ! -e "$TMP/route" ] || fail 'up prepare mutated the route'

SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-up \
  "$HANDLER" up >"$TMP/up.out"
[ "$(sed -n '1p' "$TMP/route")" = proxy ] || fail 'route is not a proxy device'
grep -Fxq 'tcp:100.64.1.20:19119' "$TMP/route" \
  || fail 'route did not bind the exact Tailscale address'
grep -Fxq 'tcp:127.0.0.1:9119' "$TMP/route" \
  || fail 'route did not target guest loopback'
[ "$(sed -n '4p' "$TMP/route")" = host ] || fail 'route is not host-bound'
grep -Eq '^config device add [^ ]+ hermes-dashboard proxy .*listen=tcp:100\.64\.1\.20:19119 .*connect=tcp:127\.0\.0\.1:9119 .*bind=host' \
  "$TMP/incus.log" || fail 'route add did not use the exact declared device name and type'
[ "$(<"$TMP/ownership")" = \
  'v1:d839b683e89c7f8706a2e0131fe0c5fd421031a09781e01b0ffede6239c54f26' ] \
  || fail 'route ownership metadata does not fingerprint the exact created device'

SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=is-up SUBYARD_OPERATION_ID=op-is-up \
  "$HANDLER" is-up

# Ownership must survive mutable endpoint reconfiguration and authorize replacement of only
# the exact route previously created by this resource.
old_ownership="$(<"$TMP/ownership")"
export HERMES_DASHBOARD_HOST_PORT=19120
SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/reconfigure-plan.json"
grep -Fq '"action":"up","changed":true' "$TMP/reconfigure-plan.json" \
  || fail 'route reconfiguration was not assessed as a change'
touch "$TMP/fail-remove"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up \
  SUBYARD_OPERATION_ID=op-reconfigure-remove-failure \
  "$HANDLER" up >"$TMP/reconfigure-remove-failure.out" 2>&1; then
  fail 'route reconfiguration accepted an injected removal failure'
fi
grep -Fxq 'tcp:100.64.1.20:19119' "$TMP/route" \
  || fail 'failed replacement changed the existing route'
[ "$(<"$TMP/ownership")" = "$old_ownership" ] \
  || fail 'failed replacement cleared ownership of a still-live route'
rm -f -- "$TMP/fail-remove"
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-reconfigure \
  "$HANDLER" up >"$TMP/reconfigure.out"
grep -Fxq 'tcp:100.64.1.20:19120' "$TMP/route" \
  || fail 'owned route was not replaced after its port changed'
[ "$(<"$TMP/ownership")" != "$old_ownership" ] \
  || fail 'route ownership metadata did not track the replacement device'

touch "$TMP/route-extra"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/extra-option.out" 2>&1; then
  fail 'route with an unexpected proxy option retained trusted ownership'
fi
rm -f -- "$TMP/route-extra"

unset HERMES_DASHBOARD_ADVERTISE_HOST HERMES_DASHBOARD_HOST_PORT
SUBYARD_RESOURCE_MODE=prepare "$HANDLER" down >"$TMP/down-plan.json"
grep -Fq '"action":"down","changed":true' "$TMP/down-plan.json" \
  || fail 'route withdrawal was not available after settings were cleared'
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down SUBYARD_OPERATION_ID=op-down \
  "$HANDLER" down >"$TMP/down.out"
[ ! -e "$TMP/route" ] || fail 'down left the owned route attached'
[ ! -e "$TMP/ownership" ] || fail 'down left route ownership metadata behind'

export HERMES_DASHBOARD_ADVERTISE_HOST=owner.example-tailnet.ts.net
export HERMES_DASHBOARD_HOST_PORT=19119

export HERMES_DASHBOARD_ADVERTISE_HOST=127.0.0.1
printf '127.0.0.1\n' >"$TMP/dns-ips"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/loopback-owner.out" 2>&1; then
  fail 'up accepted localhost for a Tailscale-only owner route'
fi
rm -f -- "$TMP/dns-ips"

export HERMES_DASHBOARD_ADVERTISE_HOST=public.example.net
printf '203.0.113.10\n' >"$TMP/dns-ips"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/public-owner.out" 2>&1; then
  fail 'up accepted a public non-Tailscale owner address'
fi
rm -f -- "$TMP/dns-ips"

export HERMES_DASHBOARD_ADVERTISE_HOST=owner.example-tailnet.ts.net
printf 'eth0 UP 192.0.2.10/24\n' >"$TMP/active-ips"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/inactive-owner.out" 2>&1; then
  fail 'up accepted a Tailscale address that is not active on the owner host'
fi
rm -f -- "$TMP/active-ips"

touch "$TMP/tailscale-fail"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/missing-tailscale.out" 2>&1; then
  fail 'up accepted an unavailable Tailscale identity source'
fi
rm -f -- "$TMP/tailscale-fail"

touch "$TMP/port-occupied"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/occupied-port.out" 2>&1; then
  fail 'up accepted an occupied owner-host port'
fi
rm -f -- "$TMP/port-occupied"

# Publishing records pending ownership before adding the public device. Every partial-failure path
# must either remove the device or retain matching metadata so a later down can recover it.
touch "$TMP/fail-marker-set"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-marker-failure \
  "$HANDLER" up >"$TMP/marker-failure.out" 2>&1; then
  fail 'route publication accepted a pending-marker failure'
fi
[ ! -e "$TMP/route" ] && [ ! -e "$TMP/ownership" ] \
  || fail 'pending-marker failure published a route or retained metadata'
rm -f -- "$TMP/fail-marker-set"

touch "$TMP/fail-add"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-add-failure \
  "$HANDLER" up >"$TMP/add-failure.out" 2>&1; then
  fail 'route publication accepted a device-add failure'
fi
[ ! -e "$TMP/route" ] && [ ! -e "$TMP/ownership" ] \
  || fail 'device-add failure retained route state'
rm -f -- "$TMP/fail-add"

touch "$TMP/fail-active-marker"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-active-marker-failure \
  "$HANDLER" up >"$TMP/active-marker-failure.out" 2>&1; then
  fail 'route publication accepted an active-marker failure'
fi
[ ! -e "$TMP/route" ] && [ ! -e "$TMP/ownership" ] \
  || fail 'active-marker failure was not rolled back completely'
rm -f -- "$TMP/fail-active-marker"

touch "$TMP/owner-not-ready" "$TMP/fail-remove"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-rollback-failure \
  "$HANDLER" up >"$TMP/rollback-failure.out" 2>&1; then
  fail 'endpoint-readiness and rollback failure was accepted'
fi
[ -e "$TMP/route" ] && [ "$(<"$TMP/ownership")" = \
  'v1:d839b683e89c7f8706a2e0131fe0c5fd421031a09781e01b0ffede6239c54f26' ] \
  || fail 'incomplete rollback lost ownership of the still-live route'
rm -f -- "$TMP/owner-not-ready" "$TMP/fail-remove"
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down SUBYARD_OPERATION_ID=op-recover-down \
  "$HANDLER" down >"$TMP/recover-down.out"
[ ! -e "$TMP/route" ] && [ ! -e "$TMP/ownership" ] \
  || fail 'down did not recover an incomplete publication rollback'

# If the proxy disappears during readiness rollback, a simultaneous metadata-unset failure must
# be reported as incomplete rather than claiming that the route and its ownership were removed.
touch "$TMP/owner-not-ready-remove-route" "$TMP/fail-marker-unset"
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up \
  SUBYARD_OPERATION_ID=op-readiness-device-race \
  "$HANDLER" up >"$TMP/readiness-device-race.out" 2>&1; then
  fail 'readiness rollback accepted an externally disappeared route'
fi
grep -Fq 'rollback is incomplete' "$TMP/readiness-device-race.out" \
  || fail 'readiness rollback masked stale ownership metadata removal failure'
[ ! -e "$TMP/route" ] && [ -e "$TMP/ownership" ] \
  || fail 'readiness race did not retain only the recoverable ownership marker'
rm -f -- "$TMP/owner-not-ready-remove-route" "$TMP/fail-marker-unset"
SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down \
  SUBYARD_OPERATION_ID=op-readiness-device-race-recovery \
  "$HANDLER" down >"$TMP/readiness-device-race-recovery.out"
[ ! -e "$TMP/ownership" ] || fail 'down did not recover readiness-race metadata'

printf '%s\n' disk 'tcp:100.64.1.20:19119' 'tcp:127.0.0.1:9119' host >"$TMP/route"
printf '%s\n' hermes-dashboard >"$TMP/route-name"
foreign_before="$(sha256sum "$TMP/route")"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/foreign-prepare.out" 2>&1; then
  fail 'up prepare accepted a foreign same-name device'
fi
[ "$(sha256sum "$TMP/route")" = "$foreign_before" ] \
  || fail 'foreign-device prepare changed the device'
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=up SUBYARD_OPERATION_ID=op-foreign-up \
  "$HANDLER" up >"$TMP/foreign-up.out" 2>&1; then
  fail 'up replaced a foreign same-name device'
fi
[ "$(sha256sum "$TMP/route")" = "$foreign_before" ] \
  || fail 'failed foreign-device up changed the device'
if SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=down SUBYARD_OPERATION_ID=op-foreign-down \
  "$HANDLER" down >"$TMP/foreign-down.out" 2>&1; then
  fail 'down removed a foreign same-name device'
fi
[ "$(sha256sum "$TMP/route")" = "$foreign_before" ] \
  || fail 'failed foreign-device down changed the device'
rm -f -- "$TMP/route" "$TMP/route-name"

touch "$TMP/dns-extra"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/ambiguous.out" 2>&1; then
  fail 'up accepted a hostname with mixed Tailscale and public IPv4 answers'
fi
[ ! -e "$TMP/route" ] || fail 'ambiguous DNS refusal changed the route'
rm -f -- "$TMP/dns-extra"

rm -f -- "$TMP/guest-ready"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/not-ready.out" 2>&1; then
  fail 'up accepted an absent guest loopback endpoint'
fi
[ ! -e "$TMP/route" ] || fail 'failed readiness check changed the route'

touch "$TMP/guest-ready" "$TMP/guest-wildcard"
if SUBYARD_RESOURCE_MODE=prepare "$HANDLER" up >"$TMP/wildcard.out" 2>&1; then
  fail 'up accepted a wildcard guest listener'
fi
[ ! -e "$TMP/route" ] || fail 'wildcard refusal changed the route'

printf 'ok: Hermes route is infrastructure-only and fail-closed\n'

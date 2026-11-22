#!/usr/bin/env bash
set -euo pipefail
test_root=$(mktemp -d /tmp/subyard-herdr-route.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
die() { printf '%s\n' "$*" >&2; exit 1; }
# shellcheck source=config/profiles/herdr/resources/herdr/route.sh
. config/profiles/herdr/resources/herdr/route.sh
INCUS_PROJECT=fixture
YARD_INSTANCE_NAME=fixture
PROJ=(--project fixture)
HERDR_HOST_PORT=6768
HERDR_ADVERTISE_HOST=fixture.ts.net
tailscale() { printf '100.100.100.100\n'; }
getent() { printf '%s STREAM fixture.ts.net\n' "${resolved:-100.100.100.100}"; }
ip() { printf '12: tailscale0 inet 100.100.100.100/32 scope global\n'; }
guest_ssh() { [ "$1" = check ] && [ "${dropin:-present}" = present ]; }
listener_ready() { [ "${listener:-up}" = up ] && [ "$1:$2" = 100.100.100.100:6768 ]; }
shape=$(route_shape 100.100.100.100)
token=$(route_receipt "$shape")
pending=$(jq -cn --argjson shape "$shape" --arg token "$token" '{device:$shape,ownership:$token,version:1}')
incus() {
  if [ "$1" = query ]; then
    jq -cn --argjson shape "$shape" --arg token "$token" --arg pending "${saved:-}" --arg receipt "$receipt" --arg deferred "$deferred" \
      '{devices:{"herdr-ssh":$shape},config:{($receipt):$token,($deferred):$pending}}'
  else
    printf '%s\n' "$*" >> "$test_root/calls"
  fi
}
route_ready
if (listener=down; route_ready); then exit 1; fi
if (dropin=missing; route_ready); then exit 1; fi
if (resolved=192.0.2.1 endpoint) >/dev/null 2>&1; then exit 1; fi
saved="$pending"
remove_route "$(route_state)"
cat > "$test_root/expected" <<EOF
config unset fixture $deferred --project fixture
config device remove fixture $device --project fixture
config unset fixture $receipt --project fixture
EOF
cmp "$test_root/calls" "$test_root/expected"

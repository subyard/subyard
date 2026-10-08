#!/usr/bin/env bash
# Exercise preview publication and ownership against a stateful Incus boundary double.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
mkdir -p "$temporary/bin"
export PREVIEW_PROXY_STATE="$temporary/state.json" PREVIEW_PROXY_LOG="$temporary/log"
printf '{"config":{},"devices":{}}\n' >"$PREVIEW_PROXY_STATE"
: >"$PREVIEW_PROXY_LOG"
cat >"$temporary/bin/incus" <<'PY'
#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
path = os.environ['PREVIEW_PROXY_STATE']
with open(path) as source: state = json.load(source)
if args == ['query', '/1.0/instances/fixture?project=fixture']:
    print(json.dumps(state)); sys.exit(0)
with open(os.environ['PREVIEW_PROXY_LOG'], 'a') as log: log.write(' '.join(args) + '\n')
if args[:2] == ['config', 'set']:
    state['config'][args[3]] = args[4]
elif args[:2] == ['config', 'unset']:
    del state['config'][args[3]]
elif args[:3] == ['config', 'device', 'remove']:
    del state['devices'][args[4]]
elif args[:3] in (['config', 'device', 'set'], ['config', 'device', 'override']):
    name = args[4]
    state['devices'].setdefault(name, dict(state.get('expanded_devices', {}).get(name, {})))
    state['devices'][name].update(arg.split('=', 1) for arg in args[5:] if '=' in arg)
elif args[:3] == ['config', 'device', 'add']:
    if os.environ.get('PREVIEW_PROXY_FAIL') == '1': sys.exit(1)
    assert args[5] == 'proxy'
    state['devices'][args[4]] = {'type': 'proxy', **dict(
        arg.split('=', 1) for arg in args[6:] if '=' in arg)}
else:
    raise RuntimeError(args)
if 'expanded_devices' in state:
    for name in list(state['expanded_devices']):
        if name != 'eth0' and name not in state['devices']: del state['expanded_devices'][name]
    state['expanded_devices'].update(state['devices'])
with open(path, 'w') as target: json.dump(state, target)
# Mutation output must never corrupt the endpoint JSON returned by the library.
print('updated')
PY
cat >"$temporary/bin/ip" <<'SH'
#!/bin/sh
if [ "$*" = '-j -4 route get 1.1.1.1' ]; then
  printf '[{"prefsrc":"%s"}]\n' "${PREVIEW_SOURCE_ADDRESS:-}"
else
  printf '[{"addr_info":[{"local":"%s"},{"local":"%s"}]}]\n' "${PREVIEW_ACTIVE_ADDRESS:-}" "${PREVIEW_SOURCE_ACTIVE:-}"
fi
SH
cat >"$temporary/bin/ss" <<'SH'
#!/bin/sh
printf '%s\n' "${PREVIEW_SOCKET:-}"
SH
chmod 0755 "$temporary/bin/incus" "$temporary/bin/ip" "$temporary/bin/ss"
export PATH="$temporary/bin:$PATH"
# shellcheck source=scripts/lib/preview-proxy.sh
. "$ROOT/scripts/lib/preview-proxy.sh"

YARD_INSTANCE_NAME=fixture
INCUS_PROJECT=fixture
YARD_KIND=container
PREVIEW_HOST=127.0.0.1
WEB_PREVIEW_HOST_PORT=32222
PROJ=(--project fixture)
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
route() {
  subyard_preview_route "$PREVIEW_HOST"
}
rewrite() {
  jq "$1" "$PREVIEW_PROXY_STATE" >"$temporary/next.json"
  mv "$temporary/next.json" "$PREVIEW_PROXY_STATE"
}
unchanged_failure() {
  local before
  before="$(wc -l <"$PREVIEW_PROXY_LOG")"
  if subyard_preview_route "$PREVIEW_HOST"; then fail 'foreign or divergent route accepted'; fi
  [ "$(wc -l <"$PREVIEW_PROXY_LOG")" = "$before" ] || fail 'foreign route mutated'
}
route
[ ! -s "$PREVIEW_PROXY_LOG" ] || fail 'fresh fallback mutated state'
PREVIEW_HOST=100.101.102.103
export PREVIEW_ACTIVE_ADDRESS=100.101.102.104
unchanged_failure
export PREVIEW_ACTIVE_ADDRESS=100.101.102.103
route
jq -e '.devices["subyard-preview"] == {type:"proxy",bind:"host",listen:"tcp:100.101.102.103:32222",connect:"tcp:127.0.0.1:8765"} and
  .config["user.subyard.preview_proxy"] == "v1:100.101.102.103:32222"' "$PREVIEW_PROXY_STATE" >/dev/null
before="$(wc -l <"$PREVIEW_PROXY_LOG")"
route
[ "$(wc -l <"$PREVIEW_PROXY_LOG")" = "$before" ] || fail 'exact route was mutated'
rewrite '.devices={}'
route
jq -e '.config["user.subyard.preview_proxy"] == "v1:100.101.102.103:32222"' "$PREVIEW_PROXY_STATE" >/dev/null

# Address/port drift and interrupted publication reconcile only owned state.
PREVIEW_HOST=100.101.102.104
export PREVIEW_ACTIVE_ADDRESS=100.101.102.104
WEB_PREVIEW_HOST_PORT=32223
if PREVIEW_PROXY_FAIL=1 subyard_preview_route "$PREVIEW_HOST"; then fail 'publication failure accepted'; fi
jq -e '.devices == {} and .config["user.subyard.preview_proxy"] == "v1:pending:100.101.102.104:32223"' "$PREVIEW_PROXY_STATE" >/dev/null
route
rewrite '.config["user.subyard.preview_proxy"]="v1:pending:100.101.102.104:32223"'
route
jq -e '.config["user.subyard.preview_proxy"] == "v1:100.101.102.104:32223"' "$PREVIEW_PROXY_STATE" >/dev/null
rewrite '.devices["subyard-preview"].connect="tcp:127.0.0.1:9999"'
unchanged_failure
rewrite '.devices["subyard-preview"].connect="tcp:127.0.0.1:8765" | .expanded_devices={}'
unchanged_failure
rewrite '.expanded_devices=.devices | .devices={}'
unchanged_failure
rewrite '.devices=.expanded_devices | del(.expanded_devices) | .config={}'
unchanged_failure
rewrite '.devices={} | .config["user.subyard.preview_proxy"]="v1:100.101.102.104:032223"'
unchanged_failure

# A prepared private source must still be active; loopback removes only owned state.
rewrite '.config={} | .devices={}'
PREVIEW_HOST=192.168.1.20
export PREVIEW_SOURCE_ACTIVE=192.168.1.21
unchanged_failure
export PREVIEW_SOURCE_ACTIVE=192.168.1.20
route
PREVIEW_HOST=203.0.113.20
export PREVIEW_SOURCE_ACTIVE=203.0.113.20
unchanged_failure
PREVIEW_HOST=127.0.0.1
route
jq -e '.devices == {} and .config == {}' "$PREVIEW_PROXY_STATE" >/dev/null

# VMs consume the instance stage pin before publishing an owned NAT route.
YARD_KIND=vm
PREVIEW_HOST=192.168.1.20
export PREVIEW_SOURCE_ACTIVE=192.168.1.20
rewrite '.devices.eth0={type:"nic",network:"incusbr0"}'
unchanged_failure
rewrite '.devices.eth0["ipv4.address"]="10.80.0.10"'
route
jq -e '.devices.eth0["ipv4.address"] == "10.80.0.10" and
  .devices["subyard-preview"] == {type:"proxy",bind:"host",nat:"true",listen:"tcp:192.168.1.20:32223",connect:"tcp:10.80.0.10:8765"} and
  .config["user.subyard.preview_proxy"] == "v2:192.168.1.20:32223:10.80.0.10"' "$PREVIEW_PROXY_STATE" >/dev/null
before="$(wc -l <"$PREVIEW_PROXY_LOG")"
route
[ "$(wc -l <"$PREVIEW_PROXY_LOG")" = "$before" ] || fail 'exact VM route was mutated'
rewrite '.config["user.subyard.preview_proxy"]="v2:pending:192.168.1.20:32223:10.80.0.10"'
route
rewrite '.devices["subyard-preview"].connect="tcp:10.80.0.11:8765"'
unchanged_failure
rewrite '.devices["subyard-preview"].connect="tcp:10.80.0.10:8765"'
rewrite 'del(.devices.eth0["ipv4.address"])'
unchanged_failure
rewrite '.devices.eth0["ipv4.address"]="10.80.0.10"'

# NAT must not override an existing owner socket; profile pins stay profile-owned.
rewrite '.config={} | .devices={} | .expanded_devices={eth0:{type:"nic",network:"incusbr0","ipv4.address":"10.80.0.10"}}'
VM_PIN_IPV4=1
export PREVIEW_SOCKET='LISTEN 0 1 192.168.1.20:32223 0.0.0.0:*'
unchanged_failure
unset PREVIEW_SOCKET
route
jq -e '.devices.eth0 == null' "$PREVIEW_PROXY_STATE" >/dev/null
PREVIEW_HOST=127.0.0.1
route
jq -e '.devices == {} and .config == {}' "$PREVIEW_PROXY_STATE" >/dev/null
before="$(wc -l <"$PREVIEW_PROXY_LOG")"
route
[ "$(wc -l <"$PREVIEW_PROXY_LOG")" = "$before" ] || fail 'VM fallback was mutated'
printf 'ok: preview owner proxy lifecycle\n'

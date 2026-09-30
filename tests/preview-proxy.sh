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
elif args[:3] == ['config', 'device', 'add']:
    if os.environ.get('PREVIEW_PROXY_FAIL') == '1': sys.exit(1)
    assert args[5] == 'proxy'
    state['devices'][args[4]] = {'type': 'proxy', **dict(
        arg.split('=', 1) for arg in args[6:] if '=' in arg)}
else:
    raise RuntimeError(args)
with open(path, 'w') as target: json.dump(state, target)
# Mutation output must never corrupt the endpoint JSON returned by the library.
print('updated')
PY
cat >"$temporary/bin/tailscale" <<'SH'
#!/bin/sh
[ "$*" = 'ip -4' ] || exit 2
[ -n "${PREVIEW_TAIL_ADDRESS:-}" ] || exit 1
printf '%s\n' "$PREVIEW_TAIL_ADDRESS"
SH
cat >"$temporary/bin/ip" <<'SH'
#!/bin/sh
printf '[{"addr_info":[{"local":"%s"}]}]\n' "${PREVIEW_ACTIVE_ADDRESS:-}"
SH
chmod 0755 "$temporary/bin/incus" "$temporary/bin/tailscale" "$temporary/bin/ip"
export PATH="$temporary/bin:$PATH"
# shellcheck source=scripts/lib/preview-proxy.sh
. "$ROOT/scripts/lib/preview-proxy.sh"
YARD_INSTANCE_NAME=fixture
INCUS_PROJECT=fixture
YARD_KIND=container
WEB_PREVIEW_HOST_PORT=32222
PROJ=(--project fixture)
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
endpoint() {
  local actual
  actual="$(subyard_preview_endpoint)"
  [ "$actual" = "$1" ] || fail "unexpected endpoint: $actual"
}
rewrite() {
  jq "$1" "$PREVIEW_PROXY_STATE" >"$temporary/next.json"
  mv "$temporary/next.json" "$PREVIEW_PROXY_STATE"
}
unchanged_failure() {
  local before
  before="$(wc -l <"$PREVIEW_PROXY_LOG")"
  if subyard_preview_endpoint; then fail 'foreign or divergent route accepted'; fi
  [ "$(wc -l <"$PREVIEW_PROXY_LOG")" = "$before" ] || fail 'foreign route mutated'
}
fallback='{"version":1,"host":"127.0.0.1","port":8765}'
endpoint "$fallback"
[ ! -s "$PREVIEW_PROXY_LOG" ] || fail 'fresh fallback mutated state'
export PREVIEW_TAIL_ADDRESS=100.101.102.103 PREVIEW_ACTIVE_ADDRESS=100.101.102.104
endpoint "$fallback"
[ ! -s "$PREVIEW_PROXY_LOG" ] || fail 'inactive Tailnet address was published'
export PREVIEW_ACTIVE_ADDRESS=100.101.102.103
endpoint '{"version":1,"host":"100.101.102.103","port":32222}'
jq -e '.devices["subyard-preview"] == {type:"proxy",bind:"host",listen:"tcp:100.101.102.103:32222",connect:"tcp:127.0.0.1:8765"} and
  .config["user.subyard.preview_proxy"] == "v1:100.101.102.103:32222"' "$PREVIEW_PROXY_STATE" >/dev/null
before="$(wc -l <"$PREVIEW_PROXY_LOG")"
endpoint '{"version":1,"host":"100.101.102.103","port":32222}'
[ "$(wc -l <"$PREVIEW_PROXY_LOG")" = "$before" ] || fail 'exact route was mutated'
rewrite '.devices={}'
endpoint '{"version":1,"host":"100.101.102.103","port":32222}'
jq -e '.config["user.subyard.preview_proxy"] == "v1:100.101.102.103:32222"' "$PREVIEW_PROXY_STATE" >/dev/null

# Address/port drift and interrupted publication reconcile only owned state.
export PREVIEW_TAIL_ADDRESS=100.101.102.104 PREVIEW_ACTIVE_ADDRESS=100.101.102.104
WEB_PREVIEW_HOST_PORT=32223
if PREVIEW_PROXY_FAIL=1 subyard_preview_endpoint; then fail 'publication failure accepted'; fi
jq -e '.devices == {} and .config["user.subyard.preview_proxy"] == "v1:pending:100.101.102.104:32223"' "$PREVIEW_PROXY_STATE" >/dev/null
endpoint '{"version":1,"host":"100.101.102.104","port":32223}'
rewrite '.config["user.subyard.preview_proxy"]="v1:pending:100.101.102.104:32223"'
endpoint '{"version":1,"host":"100.101.102.104","port":32223}'
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

# No Tailnet and VM fallback remove only exactly owned container routes.
rewrite '.config={} | .devices={}'
endpoint '{"version":1,"host":"100.101.102.104","port":32223}'
unset PREVIEW_TAIL_ADDRESS PREVIEW_ACTIVE_ADDRESS
endpoint "$fallback"
jq -e '.devices == {} and .config == {}' "$PREVIEW_PROXY_STATE" >/dev/null
export PREVIEW_TAIL_ADDRESS=100.101.102.104 PREVIEW_ACTIVE_ADDRESS=100.101.102.104
endpoint '{"version":1,"host":"100.101.102.104","port":32223}'
YARD_KIND=vm
endpoint "$fallback"
jq -e '.devices == {} and .config == {}' "$PREVIEW_PROXY_STATE" >/dev/null
before="$(wc -l <"$PREVIEW_PROXY_LOG")"
endpoint "$fallback"
[ "$(wc -l <"$PREVIEW_PROXY_LOG")" = "$before" ] || fail 'VM fallback was mutated'
printf 'ok: preview owner proxy lifecycle\n'

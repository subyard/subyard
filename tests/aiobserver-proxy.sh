#!/usr/bin/env bash
# Exercise owner proxy lifecycle against a stateful Incus boundary double.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
mkdir -p "$temporary/bin"
export OBSERVER_PROXY_STATE="$temporary/state.json" OBSERVER_PROXY_LOG="$temporary/log"
printf '{"config":{},"devices":{}}\n' >"$OBSERVER_PROXY_STATE"
cat >"$temporary/bin/incus" <<'PY'
#!/usr/bin/env python3
import json, os, sys
args=sys.argv[1:]
path=os.environ['OBSERVER_PROXY_STATE']
with open(path) as f: state=json.load(f)
if args==['query','/1.0/instances/fixture?project=fixture']:
 print(json.dumps(state)); sys.exit(0)
with open(os.environ['OBSERVER_PROXY_LOG'],'a') as f: f.write(' '.join(args)+'\n')
if args[:2]==['config','get']:
 if os.environ.get('OBSERVER_PROXY_GET_FAIL')=='1': sys.exit(1)
 print(state['config'].get(args[3],'')); sys.exit(0)
elif args[:2]==['config','set']:
 state['config'][args[3]]=args[4]
elif args[:2]==['config','unset']:
 if os.environ.get('OBSERVER_PROXY_UNSET_FAIL')=='1': sys.exit(1)
 if args[3] not in state['config']: sys.exit(4)
 del state['config'][args[3]]
elif args[:3]==['config','device','remove']:
 del state['devices'][args[4]]
elif args[:3]==['config','device','add']:
 if os.environ.get('OBSERVER_PROXY_FAIL')=='1': sys.exit(1)
 assert args[5]=='proxy'
 props=dict(arg.split('=',1) for arg in args[6:] if '=' in arg)
 state['devices'][args[4]]={'type':'proxy',**props}
else: raise RuntimeError(args)
with open(path,'w') as f: json.dump(state,f)
PY
chmod +x "$temporary/bin/incus"
export PATH="$temporary/bin:$PATH"
# shellcheck source=scripts/lib/ai-observer-proxy.sh
. "$ROOT/scripts/lib/ai-observer-proxy.sh"
YARD_INSTANCE_NAME=fixture
INCUS_PROJECT=fixture
YARD_KIND=container
AI_OBSERVER_HOST_PORT=22222
PROJ=(--project fixture)
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

subyard_ai_observer_proxy 1
jq -e '.devices["ai-observer"] == {"type":"proxy","bind":"host","listen":"tcp:127.0.0.1:22222","connect":"tcp:127.0.0.1:8080"}' "$OBSERVER_PROXY_STATE" >/dev/null
before="$(wc -l <"$OBSERVER_PROXY_LOG")"
subyard_ai_observer_proxy 1
[ "$(wc -l <"$OBSERVER_PROXY_LOG")" = "$before" ] || fail 'exact route was mutated'
AI_OBSERVER_HOST_PORT=22223
subyard_ai_observer_proxy 1
jq -e '.devices["ai-observer"].listen == "tcp:127.0.0.1:22223"' "$OBSERVER_PROXY_STATE" >/dev/null
subyard_ai_observer_proxy 0
jq -e '.devices == {} and .config == {}' "$OBSERVER_PROXY_STATE" >/dev/null
YARD_KIND=vm
subyard_ai_observer_proxy 1
jq -e '.devices == {}' "$OBSERVER_PROXY_STATE" >/dev/null
YARD_KIND=container
if OBSERVER_PROXY_FAIL=1 subyard_ai_observer_proxy 1; then fail 'publication failure accepted'; fi
subyard_ai_observer_proxy 1
jq -e '.devices["ai-observer"].listen == "tcp:127.0.0.1:22223"' "$OBSERVER_PROXY_STATE" >/dev/null
printf '{"config":{},"devices":{"ai-observer":{"type":"disk","source":"/foreign","path":"/foreign"}}}\n' >"$OBSERVER_PROXY_STATE"
before="$(wc -l <"$OBSERVER_PROXY_LOG")"
if subyard_ai_observer_proxy 1; then fail 'foreign device overwritten'; fi
[ "$(wc -l <"$OBSERVER_PROXY_LOG")" = "$before" ] || fail 'foreign device mutated'
subyard_ai_observer_proxy 0
[ "$(wc -l <"$OBSERVER_PROXY_LOG")" = "$before" ] || fail 'unselected foreign device mutated'

provision_key=user.subyard.ai_observer_provision
context=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
printf '{"config":{},"devices":{}}\n' >"$OBSERVER_PROXY_STATE"
: >"$OBSERVER_PROXY_LOG"
subyard_ai_observer_provision_marker 0 ''
jq -e '.config == {}' "$OBSERVER_PROXY_STATE" >/dev/null \
  || fail 'fresh disabled convergence marker mutated state'
! grep -Fq "config unset fixture $provision_key" "$OBSERVER_PROXY_LOG" \
  || fail 'fresh disabled convergence marker tried to unset a missing key'
subyard_ai_observer_provision_marker 0 ''
! grep -Fq "config unset fixture $provision_key" "$OBSERVER_PROXY_LOG" \
  || fail 'repeated disabled convergence marker tried to unset a missing key'

jq --arg key "$provision_key" '.config[$key]="old-context"' \
  "$OBSERVER_PROXY_STATE" >"$temporary/with-marker.json"
mv "$temporary/with-marker.json" "$OBSERVER_PROXY_STATE"
subyard_ai_observer_provision_marker 0 ''
jq -e --arg key "$provision_key" '.config[$key] == null' "$OBSERVER_PROXY_STATE" >/dev/null \
  || fail 'disabled convergence marker kept an existing value'
[ "$(grep -Fc "config unset fixture $provision_key" "$OBSERVER_PROXY_LOG")" -eq 1 ] \
  || fail 'existing convergence marker was not cleared exactly once'
subyard_ai_observer_provision_marker 0 ''
[ "$(grep -Fc "config unset fixture $provision_key" "$OBSERVER_PROXY_LOG")" -eq 1 ] \
  || fail 'repeated convergence marker clear was not a no-op'

subyard_ai_observer_provision_marker 1 "$context"
jq -e --arg key "$provision_key" --arg context "$context" \
  '.config[$key] == $context' "$OBSERVER_PROXY_STATE" >/dev/null \
  || fail 'selected convergence marker was not stored'
if OBSERVER_PROXY_GET_FAIL=1 subyard_ai_observer_provision_marker 0 ''; then
  fail 'convergence marker accepted a failed Incus read'
fi
if OBSERVER_PROXY_UNSET_FAIL=1 subyard_ai_observer_provision_marker 0 ''; then
  fail 'convergence marker accepted a failed Incus unset'
fi
jq -e --arg key "$provision_key" --arg context "$context" \
  '.config[$key] == $context' "$OBSERVER_PROXY_STATE" >/dev/null \
  || fail 'failed convergence marker clear mutated state'
printf 'ok: AI Observer owner proxy lifecycle\n'

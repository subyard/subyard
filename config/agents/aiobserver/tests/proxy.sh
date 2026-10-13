#!/usr/bin/env bash
# Exercise owner proxy lifecycle against a stateful Incus boundary double.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
mkdir -p "$temporary/bin"
export OBSERVER_PROXY_STATE="$temporary/state.json" OBSERVER_PROXY_LOG="$temporary/log"
printf '{"config":{},"devices":{}}\n' >"$OBSERVER_PROXY_STATE"
cat >"$temporary/bin/incus" <<'PY'
#!/usr/bin/env python3
import copy, json, os, sys
args=sys.argv[1:]
path=os.environ['OBSERVER_PROXY_STATE']
with open(path) as f: state=json.load(f)
def restore():
 plan=state['restorePlan']; expected=plan['etag']; actual=state.get('revision',0)
 if expected!=actual:
  state['restoreResult']='etag-mismatch'
 else:
  receipt=plan['receipt']; assert receipt['ownership']==state['config']['user.subyard.ai_observer_proxy']
  state['devices']['ai-observer']=receipt['device']; state['config'].pop('user.subyard.deferred_publication.ai-observer',None)
  state['revision']=actual+1; state['restoreResult']='committed'
  if os.environ.get('OBSERVER_PROXY_RACE')=='before-query-foreign':
   state['devices']['ai-observer']['connect']='tcp:127.0.0.1:9999'; state['revision']+=1
 with open(path,'w') as f: json.dump(state,f)
if args==['query','/1.0/instances/fixture?project=fixture']:
 observed=copy.deepcopy(state)
 if os.environ.get('OBSERVER_PROXY_RACE','').startswith('before-query') and not state.get('restoreResult'): restore()
 print(json.dumps(observed)); sys.exit(0)
with open(os.environ['OBSERVER_PROXY_LOG'],'a') as f: f.write(' '.join(args)+'\n')
if args[:2]==['config','get']:
 if os.environ.get('OBSERVER_PROXY_GET_FAIL')=='1': sys.exit(1)
 print(state['config'].get(args[3],'')); sys.exit(0)
elif args[:2]==['config','set']:
 state['config'][args[3]]=args[4]; state['revision']=state.get('revision',0)+1
elif args[:2]==['config','unset']:
 if os.environ.get('OBSERVER_PROXY_UNSET_FAIL')=='1': sys.exit(1)
 if args[3] not in state['config']:
  # User metadata deletion is a full instance-config update; removing an
  # already-absent user key does not reintroduce it or resurrect the device.
  if args[3]!='user.subyard.deferred_publication.ai-observer': sys.exit(4)
 else:
  del state['config'][args[3]]; state['revision']=state.get('revision',0)+1
 if args[3]=='user.subyard.deferred_publication.ai-observer' and os.environ.get('OBSERVER_PROXY_RACE')=='after-revoke': restore()
elif args[:3]==['config','device','remove']:
 if os.environ.get('OBSERVER_PROXY_REMOVE_FAIL')=='1': sys.exit(1)
 del state['devices'][args[4]]; state['revision']=state.get('revision',0)+1
elif args[:3]==['config','device','add']:
 if os.environ.get('OBSERVER_PROXY_FAIL')=='1': sys.exit(1)
 assert args[5]=='proxy'
 props=dict(arg.split('=',1) for arg in args[6:] if '=' in arg)
 state['devices'][args[4]]={'type':'proxy',**props}; state['revision']=state.get('revision',0)+1
else: raise RuntimeError(args)
with open(path,'w') as f: json.dump(state,f)
PY
cat >"$temporary/bin/tailscale" <<'SH'
#!/bin/sh
[ "$*" = 'ip -4' ] || exit 2
[ -n "${OBSERVER_TAIL_ADDRESS:-}" ] || exit 1
printf '%s\n' "$OBSERVER_TAIL_ADDRESS"
SH
cat >"$temporary/bin/ip" <<'SH'
#!/bin/sh
printf '[{"addr_info":[{"local":"%s"}]}]\n' "${OBSERVER_ACTIVE_ADDRESS:-}"
SH
chmod +x "$temporary/bin/incus" "$temporary/bin/tailscale" "$temporary/bin/ip"
export PATH="$temporary/bin:$PATH"
export SUBYARD_REPOSITORY_ROOT="$ROOT"
# shellcheck source=config/agents/aiobserver/proxy.sh
. "$ROOT/config/agents/aiobserver/proxy.sh"
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
# Never publish a wildcard, LAN address, ambiguous output or inactive Tailscale IP.
for address in 0.0.0.0 192.168.1.1 100.063.1.2 100.064.1.2 100.128.1.2 100.100.01.2 '100.100.1.2 100.100.1.3'; do
  [ "$(OBSERVER_TAIL_ADDRESS="$address" OBSERVER_ACTIVE_ADDRESS="$address" subyard_owner_host)" = 127.0.0.1 ] \
    || fail 'unsafe owner address accepted'
done
[ "$(OBSERVER_TAIL_ADDRESS=100.100.1.2 OBSERVER_ACTIVE_ADDRESS=100.100.1.3 subyard_owner_host)" = 127.0.0.1 ] \
  || fail 'inactive Tailscale address accepted'

# Migrate the exact legacy loopback route to the active owner Tailscale address.
export OBSERVER_TAIL_ADDRESS=100.101.102.103 OBSERVER_ACTIVE_ADDRESS=100.101.102.103
subyard_ai_observer_proxy 1
jq -e '.devices["ai-observer"].listen == "tcp:100.101.102.103:22223" and
  .config["user.subyard.ai_observer_proxy"] == "v2:100.101.102.103:22223"' "$OBSERVER_PROXY_STATE" >/dev/null
before="$(wc -l <"$OBSERVER_PROXY_LOG")"
subyard_ai_observer_proxy 1
[ "$(wc -l <"$OBSERVER_PROXY_LOG")" = "$before" ] || fail 'exact Tailscale route was mutated'
export OBSERVER_TAIL_ADDRESS=100.101.102.104 OBSERVER_ACTIVE_ADDRESS=100.101.102.104
# Provisioned origins must not publish a different address after a stale observation.
export AI_OBSERVER_FRONTEND_URL=http://100.101.102.103:22223
before="$(wc -l <"$OBSERVER_PROXY_LOG")"
if subyard_ai_observer_proxy 1; then fail 'stale prepared origin accepted'; fi
[ "$(wc -l <"$OBSERVER_PROXY_LOG")" = "$before" ] || fail 'stale origin mutated the route'
export AI_OBSERVER_FRONTEND_URL=http://100.101.102.104:22223
if OBSERVER_PROXY_FAIL=1 subyard_ai_observer_proxy 1; then fail 'Tailscale publication failure accepted'; fi
subyard_ai_observer_proxy 1
jq -e '.devices["ai-observer"].listen == "tcp:100.101.102.104:22223"' "$OBSERVER_PROXY_STATE" >/dev/null
# A divergent owned device must remain untouched.
jq '.devices["ai-observer"].connect="tcp:127.0.0.1:9999"' "$OBSERVER_PROXY_STATE" >"$temporary/divergent.json"
mv "$temporary/divergent.json" "$OBSERVER_PROXY_STATE"
before="$(wc -l <"$OBSERVER_PROXY_LOG")"
if subyard_ai_observer_proxy 1; then fail 'divergent Tailscale route accepted'; fi
[ "$(wc -l <"$OBSERVER_PROXY_LOG")" = "$before" ] || fail 'divergent route mutated'
jq '.devices["ai-observer"].connect="tcp:127.0.0.1:8080"' "$OBSERVER_PROXY_STATE" >"$temporary/restored.json"
mv "$temporary/restored.json" "$OBSERVER_PROXY_STATE"
# Disable cleans up an owned Tailscale route even after its address goes away.
unset OBSERVER_TAIL_ADDRESS OBSERVER_ACTIVE_ADDRESS
unset AI_OBSERVER_FRONTEND_URL
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

# Deferred owner publication keeps the installed origin and is cancelled on retirement.
YARD_KIND=container
export OBSERVER_TAIL_ADDRESS=100.101.102.104 OBSERVER_ACTIVE_ADDRESS=100.101.102.104
AI_OBSERVER_FRONTEND_URL=''
subyard_ai_observer_proxy 1
jq '.config["user.subyard.deferred_publication.ai-observer"] = ({version:1,ownership:.config["user.subyard.ai_observer_proxy"],device:.devices["ai-observer"]}|tojson) | .devices={}' "$OBSERVER_PROXY_STATE" > "$temporary/next.json"
mv "$temporary/next.json" "$OBSERVER_PROXY_STATE"
export OBSERVER_TAIL_ADDRESS='' OBSERVER_ACTIVE_ADDRESS=''
before="$(wc -l <"$OBSERVER_PROXY_LOG")"
subyard_ai_observer_proxy 1
[ "$(wc -l <"$OBSERVER_PROXY_LOG")" = "$before" ] || fail 'deferred origin was replaced'
subyard_ai_observer_proxy 0
jq -e '.devices == {} and .config["user.subyard.ai_observer_proxy"] == null and .config["user.subyard.deferred_publication.ai-observer"] == null' "$OBSERVER_PROXY_STATE" >/dev/null

# Model the generic restorer's atomic device+journal update with an observed
# ETag, both before cancellation revokes authority and after that ETag is stale.
for race in before-query after-revoke before-query-foreign before-query-failed-remove failed-revoke contradictory; do
  python3 -B - <<'PY'
import json,os
marker='v2:100.101.102.104:22223'
device={'type':'proxy','bind':'host','listen':'tcp:100.101.102.104:22223','connect':'tcp:127.0.0.1:8080'}
receipt={'version':1,'ownership':marker,'device':device}
state={'config':{'user.subyard.ai_observer_proxy':marker,'user.subyard.deferred_publication.ai-observer':json.dumps(receipt),'user.subyard.desired_power':'running'},
       'devices':{'unrelated':{'type':'disk','path':'/unrelated'}},'revision':1,'restorePlan':{'etag':1,'receipt':receipt}}
with open(os.environ['OBSERVER_PROXY_STATE'],'w') as f: json.dump(state,f)
PY
  if [ "$race" = contradictory ]; then
    jq '.config["user.subyard.deferred_publication.ai-observer"] |= (fromjson | .ownership="foreign" | tojson)' "$OBSERVER_PROXY_STATE" >"$temporary/contradictory.json"
    mv "$temporary/contradictory.json" "$OBSERVER_PROXY_STATE"
  fi
  if [ "$race" = before-query-foreign ] || [ "$race" = before-query-failed-remove ] || [ "$race" = failed-revoke ] || [ "$race" = contradictory ]; then
    if OBSERVER_PROXY_RACE="$race" OBSERVER_PROXY_UNSET_FAIL="$([ "$race" = failed-revoke ] && printf 1 || printf 0)" \
      OBSERVER_PROXY_REMOVE_FAIL="$([ "$race" = before-query-failed-remove ] && printf 1 || printf 0)" subyard_ai_observer_proxy 0; then
      fail "cancellation accepted $race"
    fi
    if [ "$race" = before-query-foreign ]; then
      jq -e '.devices["ai-observer"].connect == "tcp:127.0.0.1:9999" and .config["user.subyard.ai_observer_proxy"] != null' "$OBSERVER_PROXY_STATE" >/dev/null \
        || fail 'cancellation removed a just-diverged device or its recovery receipt'
    elif [ "$race" = before-query-failed-remove ]; then
      jq -e '.devices["ai-observer"].connect == "tcp:127.0.0.1:8080" and .config["user.subyard.ai_observer_proxy"] != null and .config["user.subyard.deferred_publication.ai-observer"] == null' "$OBSERVER_PROXY_STATE" >/dev/null \
        || fail 'failed removal lost its live owned device or recovery receipt'
      subyard_ai_observer_proxy 0
      jq -e '.devices["ai-observer"] == null and .config["user.subyard.ai_observer_proxy"] == null' "$OBSERVER_PROXY_STATE" >/dev/null \
        || fail 'retry did not finish cancellation after failed removal'
    else
      jq -e '.devices["ai-observer"] == null and .config["user.subyard.deferred_publication.ai-observer"] != null and .config["user.subyard.ai_observer_proxy"] != null' "$OBSERVER_PROXY_STATE" >/dev/null \
        || fail 'failed revoke changed recoverable publication state'
    fi
  else
    OBSERVER_PROXY_RACE="$race" subyard_ai_observer_proxy 0
    jq -e '.devices["ai-observer"] == null and .config["user.subyard.ai_observer_proxy"] == null and .config["user.subyard.deferred_publication.ai-observer"] == null' "$OBSERVER_PROXY_STATE" >/dev/null \
      || fail "cancellation left a restorable or just-restored route: $race"
    expected=committed; [ "$race" != after-revoke ] || expected=etag-mismatch
    jq -e --arg expected "$expected" '.restoreResult == $expected' "$OBSERVER_PROXY_STATE" >/dev/null \
      || fail 'fixture did not exercise the required ETag ordering'
    subyard_ai_observer_proxy 0
  fi
  jq -e '.config["user.subyard.desired_power"] == "running" and .devices.unrelated == {type:"disk",path:"/unrelated"}' "$OBSERVER_PROXY_STATE" >/dev/null \
    || fail 'cancellation changed desired power or an unrelated device'
done
printf 'ok: AI Observer cancellation preserves restore ETag and foreign-device guards\n'

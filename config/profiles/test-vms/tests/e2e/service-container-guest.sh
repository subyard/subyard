#!/usr/bin/env bash
# Marker-owned service adoption and lifecycle inside one disposable VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
TOKEN="${1:-$(date +%s)$$}"
# Fresh installation uses the profile's creation UID; retain the same host
# namespace ownership from the fixture's first initialization onward.
export P0_E2E_DIAGNOSTIC_DEV_UID=1001
# Reuse the existing product installer, image preflight and exact owner cleanup.
# shellcheck disable=SC1090
. <(sed '/^  case "$MODE" in/,$d; /^ROOT=/d; /^owner_cleanup() {/,/^}/ s|\./bin/yard|fixture_yard|' \
  "$ROOT/dev/e2e/p0-guest.sh") service-container "$TOKEN"
PROJECT=subyard-test-yard
INSTANCE=yard-test-yard
# Consumed by the sourced holder or owner cleanup.
# shellcheck disable=SC2034
YARD=test-yard
RUNNER="$ROOT/dev/agent-e2e.sh"
HOLDER_PID=''
HOLDER_STARTED_PID=''
# Consumed by the sourced holder or owner cleanup.
# shellcheck disable=SC2034
HOLDER_STOP_GRACE_SECONDS=30
# Consumed by the sourced holder or owner cleanup.
# shellcheck disable=SC2034
HOLDER_KILL_GRACE_SECONDS=10
STATE_PARENT=''
INSTALL_ATTEMPTED=0
# shellcheck disable=SC1090
. <(sed -n '/^hold_lease() (/,/^)/p; /^start_holder_child() {/,/^}/p; /^stop_holder_child() {/,/^}/p; /^recovery_monotonic_seconds() {/,/^}/p' "$ROOT/dev/e2e/p0-broker-recovery.sh" | sed '/^  \. "$RUNNER"$/a\
  VM_COUNT_REQUESTED=1; VM_COUNT=1')

die() { printf 'service-container: %s\n' "$*" >&2; exit 2; }
fixture_yard() { "$SUBYARD_HOME/runtime/current/bin/yard" "$@"; }
outer_exec() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
reexec_with_incus_group() {
  local command
  printf -v command 'exec bash %q %q' "$ROOT/config/profiles/test-vms/tests/e2e/service-container-guest.sh" "$TOKEN"
  exec sg incus-admin -c "$command"
}
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$HOLDER_PID" ]; then
    : > "$STATE_PARENT/holder.release"
    stop_holder_child "$HOLDER_PID" || rc=3
  fi
  if [ "$INSTALL_ATTEMPTED" = 1 ]; then
    local generated="$OWNER_YARD_DIR/test-yard/config.env"
    # This exact path was absent before this fixture's supported install. Mark
    # only its private, user-owned regular result for the shared native cleanup.
    if [ -f "$generated" ] && [ ! -L "$generated" ] && [ -O "$generated" ] \
      && [ ! -L "${generated%/*}" ] && [ "$(stat -c %a "$generated")" = 600 ]; then
      grep -Fqx "# $MARKER" "$generated" || printf '# %s\n' "$MARKER" >> "$generated"
    fi
  fi
  (exit "$rc")
  owner_cleanup
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

prepare_owner_go_cache
STATE_PARENT="$OWNER_ROOT/service-container"
p0_capacity_prepare_subtree "$STATE_PARENT"
# Install the exact verified frozen artifact, without rebuilding another version.
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
candidate_engine="$(release_candidate_prepare "$ROOT")" || die 'verified frozen candidate required'
P0_OWNER_VERSION="$(jq -er .version "$ROOT/.subyard-acceptance/candidate.json")"
ensure_owner_incus
# Consumed by the sourced holder or owner cleanup.
# shellcheck disable=SC2034
OWNER_BASELINE_IMAGES="$(incus image list --project default --format csv -c f)"
# Consumed by the sourced holder or owner cleanup.
# shellcheck disable=SC2034
OWNER_BASELINE_CAPTURED=1
ensure_owner_base_image
# Consumed by the sourced holder or owner cleanup.
# shellcheck disable=SC2034
OWNER_DIAGNOSTIC_VM_MEMORY=2GiB
release_candidate_install "$ROOT" "$SUBYARD_HOME" >/dev/null
[ "$(sha256sum "$candidate_engine" | cut -d' ' -f1)" = "$(sha256sum "$SUBYARD_HOME/runtime/current/bin/yard-engine" | cut -d' ' -f1)" ] || die 'installed engine differs from frozen candidate'
printf 'service-container: frozen version=%s engine_sha256=%s\n' "$P0_OWNER_VERSION" "$(sha256sum "$candidate_engine" | cut -d' ' -f1)"
prepare_owner_image_cache_project "$PROJECT"
write_owner_registration test-yard test-vms 2224 1
registration="$OWNER_YARD_DIR/test-yard.env"
[ ! -e "$OWNER_YARD_DIR/test-yard/config.env" ] || registration="$OWNER_YARD_DIR/test-yard/config.env"
cat >> "$registration" <<'CONFIG'
E2E_MEMORY_RESERVE=128MiB
E2E_VM_OVERHEAD=512MiB
E2E_DISK_RESERVE=512MiB
E2E_CACHE_BUDGET=4GiB
CONFIG
p0_retry_init_after_plan_stale fixture_yard -Y test-yard init --yes
wait_for_outer_default_route "$INSTANCE" "$PROJECT"
p0_capacity_reclaim_default_go_build_cache
p0_capacity_reclaim_go_module_cache
p0_capacity_remove_build_cache
outer_exec apt-get clean
before_uuid="$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")"
before_pid="$(incus list "$INSTANCE" --project "$PROJECT" --format json | jq -er '.[0].state.pid')"
before_broker="$(outer_exec systemctl show subyard-test-vms-broker.service -p InvocationID --value)"
[[ "$before_broker" =~ ^[a-f0-9]{32}$ ]] || die 'broker invocation identity unavailable'

# The original holder intentionally uses the explicit pre-registration route.
route="$STATE_PARENT/route"
p0_capacity_prepare_subtree "$route"
key="$(normalized_ed25519 "$(outer_exec cat /etc/ssh/ssh_host_ed25519_key.pub)")"
printf 'service-fixture %s\n' "$key" > "$route/known_hosts"
printf 'Host yard-test-yard\n HostName 127.0.0.1\n Port 2224\n HostKeyAlias service-fixture\n UserKnownHostsFile "%s"\n StrictHostKeyChecking yes\n BatchMode yes\n' "$route/known_hosts" > "$route/ssh_config"
chmod 0600 "$route/known_hosts" "$route/ssh_config"
export SUBYARD_E2E_ROUTE_CONFIG="$route/ssh_config"
export SUBYARD_E2E_TEST_MODE=1 SUBYARD_E2E_PROJECT_LABEL=ServiceFixture SUBYARD_E2E_PROJECT_YARD=test-yard
export SUBYARD_E2E_WORKSPACES_ROOT
SUBYARD_E2E_WORKSPACES_ROOT="$(dirname "$(dirname "$ROOT")")"
start_holder_child hold_lease holder service-adoption slot-001 > "$STATE_PARENT/holder.log" 2>&1
HOLDER_PID="$HOLDER_STARTED_PID"
for ((attempt=0;attempt<1800;attempt++)); do
  [ ! -s "$STATE_PARENT/holder.ready" ] || break
  kill -0 "$HOLDER_PID" 2>/dev/null || die 'original lease holder failed'
  [ "$((attempt % 60))" -ne 0 ] || printf 'service-container: waiting for held singleton (%ss)\n' "$attempt"
  sleep 1
done
[ -s "$STATE_PARENT/holder.ready" ] || die 'original lease holder readiness timed out'
IFS=$'\t' read -r _slot held_config _project _run _purpose _generation _base < "$STATE_PARENT/holder.ready"
outer_exec /usr/local/libexec/subyard/test-vms-inner _test-vms-worker status > "$STATE_PARENT/before.json"
jq -e '.pool.slots[0].state == "held"' "$STATE_PARENT/before.json" >/dev/null
boot="$(ssh -F "$held_config" -T e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id </dev/null)"

fixture_yard service register test-vms --yard test-yard --yes
fixture_yard -Y missing-workspace test-vms status --json > "$STATE_PARENT/after.json"
python3 - "$STATE_PARENT/before.json" "$STATE_PARENT/after.json" <<'PY'
import json,sys
before,after=[json.load(open(path)) for path in sys.argv[1:]]
assert after['service']['service']=='test-vms'
for key in ('slot_id','resource_generation','lease_epoch','lease_id','state','environment'):
    assert before['pool']['slots'][0].get(key)==after['pool']['slots'][0].get(key),key
print('service-container: exact held allocation retained across registration')
PY
[ "$before_uuid" = "$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")" ] || die 'registration replaced runtime'
[ "$before_pid" = "$(incus list "$INSTANCE" --project "$PROJECT" --format json | jq -er '.[0].state.pid')" ] || die 'registration restarted container'
[ "$before_broker" = "$(outer_exec systemctl show subyard-test-vms-broker.service -p InvocationID --value)" ] || die 'registration restarted broker'
[ "$boot" = "$(ssh -F "$held_config" -T e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id </dev/null)" ] || die 'registration restarted held guest'
SUBYARD_E2E_ROUTE_REGISTRY="$SUBYARD_HOME/e2e/routes" SUBYARD_E2E_STATE_DIR="$STATE_PARENT/service-agent" \
  "$RUNNER" --status --json | jq -e '.status == "ok"' >/dev/null

# Re-register repairs a missing publication with the same registration generation.
registry="$SUBYARD_HOME/services/test-vms/default.json"
generation="$(jq -er .registration_generation "$registry")"
python3 - "$ROOT" "$SUBYARD_HOME/runtime/current/bin/yard" "$registry" <<'PY_STALE'
import importlib.util,json,pathlib,subprocess,sys,uuid
root,yard,registry=map(pathlib.Path,sys.argv[1:])
spec=importlib.util.spec_from_file_location('exact',root/'dev/e2e/exact-operation-ssh.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
original=registry.read_bytes()
try:
    with module.session([str(yard),'rpc','--stdio']) as process:
        operation='service-stale-'+uuid.uuid4().hex
        response=module.call(process,'operation.plan',operation,{'command':'test-vms','arguments':['revoke','--slot','1'],'exact':True,'stepSchema':1})
        exact=response.get('result',{})
        module.require(len(exact.get('digest',''))==64,'exact service revoke plan unavailable')
        steps=exact.get('plan',{}).get('steps',[])
        module.require(any('registration_generation=' in step.get('target','') for step in steps),'service identity missing from exact plan')
        replacement=json.loads(original);replacement['registration_generation']='b'*64
        registry.write_text(json.dumps(replacement)+'\n')
        module.error(module.call(process,'operation.execute',operation,{'confirmed':True,'digest':exact['digest']}),'plan_stale','service registration replacement')
finally:
    registry.write_bytes(original)
print('service-container: native exact plan rejects stale service registration')
PY_STALE
fixture_yard service register test-vms --yard test-yard --yes >/dev/null
[ "$generation" = "$(jq -er .registration_generation "$registry")" ] || die 'idempotent adoption changed registration generation'

: > "$STATE_PARENT/holder.release"
wait "$HOLDER_PID" || die 'original lease release failed'
HOLDER_PID=''
fixture_yard test-vms status --json | jq -e 'all(.pool.slots[]; .state == "available")' >/dev/null
# Use the published owner-service route for a fresh singleton negative test.
# The controller owns this lease's ordinary keeper, fencing and release.
[ ! -e "$STATE_PARENT/service-boundary" ] || die 'boundary controller state already exists'
env -u SUBYARD_E2E_ROLLOUT_YARD -u SUBYARD_E2E_YARD -u SUBYARD_E2E_YARD_STATE_DIR \
  -u SUBYARD_E2E_BASTION_ROUTE -u SUBYARD_E2E_BASTION_HOSTNAME -u SUBYARD_E2E_BASTION_PORT \
  -u SUBYARD_E2E_BASTION_HOST_KEY_ALIAS -u SUBYARD_E2E_BASTION_KNOWN_HOSTS \
  -u SUBYARD_E2E_BASTION_USER -u SUBYARD_E2E_ROUTE_CONFIG -u SUBYARD_E2E_IDENTITY \
  SUBYARD_E2E_ROUTE_REGISTRY="$SUBYARD_HOME/e2e/routes" SUBYARD_E2E_STATE_DIR="$STATE_PARENT/service-boundary" "$RUNNER" --pool default --slot 1 --vm-count 1 --verify-boundary
fixture_yard test-vms status --json | jq -e 'all(.pool.slots[]; .state == "available")' >/dev/null
python3 - "$SUBYARD_HOME/e2e/routes/services/test-vms/default.json" "$STATE_PARENT/service-boundary" <<'PYBOUNDARY'
import json,os,pathlib,stat,sys
publication=pathlib.Path(sys.argv[1])
record=json.loads(publication.read_text())
assert record['schema_version']==1 and record['service']=='test-vms' and record['pool']=='default'
state=pathlib.Path(sys.argv[2])/'services'/record['owner']/record['pool']/record['registration_generation']
for name in ('bastion_known_hosts','client-id'):
    path=state/name
    info=path.lstat()
    assert path.resolve()==path and stat.S_ISREG(info.st_mode) and info.st_uid==os.getuid()
    assert stat.S_IMODE(info.st_mode)==0o600 and info.st_nlink==1
assert (state/'bastion_known_hosts').read_bytes()==(publication.parents[2]/record['yard']/'current/known_hosts').read_bytes()
print('SERVICE_CONTAINER_BOUNDARY='+json.dumps(dict(default_service_route=True,owner=record['owner'],pool=record['pool'],registration_generation=record['registration_generation'],vm_count=1,negative_transport=True,release_available=True)))
PYBOUNDARY

fixture_yard service stop test-vms --yes >/dev/null
if fixture_yard test-vms status --json > "$STATE_PARENT/stopped.json" 2>/dev/null; then die 'stopped service reported available'; fi
jq -e '.code == "service_unavailable" and .candidates[0].registration_generation != ""' "$STATE_PARENT/stopped.json" >/dev/null
fixture_yard service start test-vms --yes >/dev/null
fixture_yard service update test-vms --yes >/dev/null
[ "$before_uuid" = "$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")" ] || die 'lifecycle replaced service runtime'
fixture_yard service remove test-vms --yes >/dev/null
if fixture_yard test-vms status --json > "$STATE_PARENT/absent.json" 2>/dev/null; then die 'removed service reported available'; fi
jq -e '.code == "service_absent"' "$STATE_PARENT/absent.json" >/dev/null

# Fresh supported install owns bootstrap and runtime creation under one consent.
generated="$OWNER_YARD_DIR/test-yard/config.env"
if [ -e "$generated" ] || [ -L "$generated" ]; then die 'removed local configuration remains'; fi
if fixture_yard service install test-vms </dev/null > "$STATE_PARENT/install-unconfirmed.log" 2>&1; then die 'fresh install ran without consent'; fi
if [ -e "$generated" ] || [ -L "$generated" ] || [ -e "$registry" ]; then die 'unconfirmed install wrote configuration or registration'; fi
if incus project show "$PROJECT" >/dev/null 2>&1; then die 'unconfirmed install created native namespace'; fi
INSTALL_ATTEMPTED=1
fixture_yard service install test-vms --yes > "$STATE_PARENT/install.log"
fixture_yard -Y missing-workspace test-vms status --json > "$STATE_PARENT/installed.json"
jq -e --arg before "$before_uuid" '.status == "ok" and .service.yard == "test-yard" and .service.pool == "default" and .service.runtime_uuid != $before and all(.pool.slots[]; .state == "available")' "$STATE_PARENT/installed.json" >/dev/null
grep -Fqx 'YARD_TEMPLATE=test-vms' "$generated" || die 'fresh install did not retain declared preset'
fixture_yard service remove test-vms --yes >/dev/null
printf 'service-container: PASS adoption, held lease continuity, trusted agent route, exact runtime lifecycle, removal and fresh install\n'

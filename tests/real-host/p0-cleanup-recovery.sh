#!/usr/bin/env bash
# Run interrupt and recover on successive leases of the same slot and VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
[ -n "${SUBYARD_E2E_VM:-}" ] || exit 2
mode="${1:?interrupt, recover or early-init required}"
token="${2:?numeric fixture token required}"
[[ "$token" =~ ^[0-9]+$ ]] || exit 2
project=subyard-p0-real-incus
marker=agent-e2e-p0
state="$HOME/.cache/subyard-cleanup-recovery-$token"
fail() { printf 'cleanup-recovery: %s\n' "$*" >&2; exit 1; }
observe() { timeout --foreground 30 incus "$@" </dev/null; }

# Keep the physical systemd contract beside the existing P0 cleanup fixture.
sink_lifecycle() (
  if ! id -nG | tr ' ' '\n' | grep -qx incus-admin; then
    exec sudo -n -E /usr/sbin/runuser -u "$(id -un)" -g incus-admin -- \
      env HOME="$HOME" PATH="$PATH" bash "$0" sink-lifecycle "$token"
  fi
  # Reuse the marker-owned P0 owner bootstrap and its exact artifact cleanup.
  # shellcheck disable=SC1090
  . <(sed '/^  case "$MODE" in/,$d; /^ROOT=/d' "$ROOT/dev/e2e/p0-guest.sh") sink-lifecycle "$token"
  local control="$HOME/.cache/subyard-sink-control-$token" producer=subyard-test-yard
  local common_sink=/usr/local/libexec/subyard/test-vms-host-sink
  local common_service=/etc/systemd/system/subyard-test-vms-host-sink.service
  local common_timer=/etc/systemd/system/subyard-test-vms-host-sink.timer
  local -a common_artifacts=("$common_sink" "$common_service" "$common_timer")
  local common_installed=0 path sidecar common_hash common_enabled incident_hash fixture_pid
  for path in "$common_sink" "$common_service" "$common_timer" "$control"; do
    [ ! -e "$path" ] && [ ! -L "$path" ] || fail "physical sink fixture requires an unused target: $path"
  done
  sink_cleanup() {
    local rc=$? cleanup_rc=0
    trap - EXIT
    cleanup_owner_capacity_state || cleanup_rc=1
    reclaim_owner_project_if_present "$producer" || cleanup_rc=1
    if [ "$common_installed" = 1 ]; then
      p0_capacity_cleanup_fixture_sink "$control/subyard" "$common_sink" "$common_service" "$common_timer" \
        "$OWNER_TEST_VMS_SINK_TIMER_TEMPLATE" || cleanup_rc=1
    fi
    [ ! -e "$control" ] || p0_capacity_delete_tree "$control" "$MARKER" || cleanup_rc=1
    [ "$cleanup_rc" = 0 ] || rc=3
    exit "$rc"
  }
  trap sink_cleanup EXIT
  prepare_owner_go_cache
  dev/build-engine.sh --force >/dev/null
  ensure_owner_incus
  ensure_owner_base_image
  prepare_owner_image_cache_project "$producer"
  incus profile device add default root disk pool=default path=/ --project "$producer" >/dev/null
  incus profile device add default eth0 nic network=incusbr0 --project "$producer" >/dev/null
  timeout --foreground 300 incus launch "$OWNER_BASE_IMAGE" yard-test-yard --project "$producer" \
    -c user.subyard.p0="$MARKER" -c user.subyard.test_vms_spool_schema=1 </dev/null >/dev/null
  incus exec yard-test-yard --project "$producer" -- install -d /usr/local/libexec/subyard /etc/subyard
  incus file push .build/yard yard-test-yard/usr/local/libexec/subyard/test-vms-inner --project "$producer" >/dev/null
  incus exec yard-test-yard --project "$producer" -- chmod 0755 /usr/local/libexec/subyard/test-vms-inner
  incus exec yard-test-yard --project "$producer" -- sh -c ': > /etc/subyard/test-vms.env'
  seed_sink_record() {
    incus exec yard-test-yard --project "$producer" -- python3 - "$1" "$2" <<'PY_RECORD'
import datetime,json,pathlib,sys
source,index=sys.argv[1],int(sys.argv[2]); stamp=datetime.datetime.now(datetime.timezone.utc).isoformat()
identity=f"{index:020d}-0123456789abcdef"
root=pathlib.Path('/var/lib/subyard/test-vms/spool')
for kind in ('events','incidents'): (root/kind).mkdir(parents=True,exist_ok=True)
event=dict(schema_version=1,timestamp=stamp,event_id=identity,sequence=index,component='test-vms-broker',pool='agent-e2e/test-vms',source=source,kind='fixture.event',incident_id=identity)
incident=dict(schema_version=1,incident_id=identity,created_at=stamp,source=source,slot_id='slot-001',resource_generation=1,lease_epoch=1,failure_reason='sink lifecycle fixture')
for kind,record in (('events',event),('incidents',incident)):
    path=root/kind/(identity+'.json');path.write_text(json.dumps(record)+'\n');path.chmod(0o600)
PY_RECORD
  }
  install_sink_fixture() (
    local context="$1" sink="$2" service="$3" timer="$4"
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$context"
    export SUBYARD_TEST_VMS_SINK_PATH="$sink" SUBYARD_TEST_VMS_SINK_SERVICE_PATH="$service"
    export SUBYARD_TEST_VMS_SINK_TIMER_PATH="$timer" SUBYARD_TEST_VMS_SINK_ENGINE_SOURCE="$ROOT/.build/yard"
    export SUBYARD_SUDO_PREAUTHORIZED=1
    bash "$ROOT/scripts/install-test-vms-host-sink.sh" --yes
  )
  install -d -m 0700 "$control"
  printf '%s\n' "$MARKER" > "$control/.subyard-p0-marker"
  common_installed=1
  install_sink_fixture "$control" "$common_sink" "$common_service" "$common_timer"
  # Deliberately retain disabled intent. All collections below are explicit oneshots.
  sudo -n systemctl disable --now "${common_timer##*/}"
  seed_sink_record foreign-producer 1
  sudo -n systemctl restart "${common_service##*/}"
  for path in "$common_sink" "$common_service" "$common_timer"; do
    common_artifacts+=("${path%/*}/.${path##*/}.native-install.lock" "${path%/*}/.${path##*/}.native-custody.json")
  done
  common_hash="$(sudo -n sha256sum "${common_artifacts[@]}")"
  common_enabled="$(systemctl is-enabled "${common_timer##*/}" || true)"
  incident_hash="$(sudo -n sha256sum "$control/subyard/logs/test-vms-broker-incidents/00000000000000000001-0123456789abcdef.json")"
  install_sink_fixture "$OWNER_ROOT" "$OWNER_TEST_VMS_SINK" "$OWNER_TEST_VMS_SINK_SERVICE" "$OWNER_TEST_VMS_SINK_TIMER"
  sudo -n systemctl disable --now "$OWNER_TEST_VMS_SINK_TIMER_NAME"
  [ "$(sudo -n sha256sum "${common_artifacts[@]}")" = "$common_hash" ] || fail 'fixture install changed common sink artifacts'
  [ "$(systemctl is-enabled "${common_timer##*/}" || true)" = "$common_enabled" ] || fail 'fixture install changed common timer intent'
  seed_sink_record fixture-producer 2
  sudo -n systemctl restart "$OWNER_TEST_VMS_SINK_SERVICE_NAME"
  sudo -n grep -Fq '"source":"fixture-producer"' "$OWNER_DATA_ROOT/logs/test-vms-broker.jsonl" || fail 'fixture oneshot lost its event'
  fixture_pid="$(systemctl show "$OWNER_TEST_VMS_SINK_SERVICE_NAME" -p MainPID --value)"
  cleanup_owner_capacity_state
  cleanup_owner_capacity_state
  [ "$fixture_pid" = 0 ] || fail 'fixture oneshot retained a process'
  for path in "$OWNER_ROOT" "$OWNER_TEST_VMS_SINK" "$OWNER_TEST_VMS_SINK_SERVICE" "$OWNER_TEST_VMS_SINK_TIMER"; do
    [ ! -e "$path" ] && [ ! -L "$path" ] || fail "fixture cleanup retained $path"
    for sidecar in native-install.lock native-custody.json; do
      [ ! -e "${path%/*}/.${path##*/}.$sidecar" ] || fail "fixture cleanup retained $sidecar"
    done
  done
  [ "$(systemctl show "$OWNER_TEST_VMS_SINK_SERVICE_NAME" -p LoadState --value)" = not-found ] || fail 'fixture service remains loaded'
  [ "$(systemctl show "$OWNER_TEST_VMS_SINK_TIMER_NAME" -p LoadState --value)" = not-found ] || fail 'fixture timer remains loaded'
  ! pgrep -f "^$OWNER_TEST_VMS_SINK " >/dev/null || fail 'fixture sink process remains'
  [ "$(sudo -n sha256sum "${common_artifacts[@]}")" = "$common_hash" ] || fail 'cleanup changed common sink artifacts'
  [ "$(systemctl is-enabled "${common_timer##*/}" || true)" = "$common_enabled" ] || fail 'cleanup changed common timer intent'
  [ "$(sudo -n sha256sum "$control/subyard/logs/test-vms-broker-incidents/00000000000000000001-0123456789abcdef.json")" = "$incident_hash" ] || fail 'cleanup changed foreign incident'
  seed_sink_record foreign-producer 3
  sudo -n systemctl restart "${common_service##*/}"
  sudo -n jq -s -e 'any(.[]; .source == "foreign-producer" and .sequence == 1) and any(.[]; .source == "foreign-producer" and .sequence == 3)' \
    "$control/subyard/logs/test-vms-broker.jsonl" >/dev/null || fail 'preserved common sink lost its producer'
  [ ! -e "$OWNER_ROOT" ] && [ ! -e "$P0_CAPACITY_STATE_ROOT" ] || fail 'common sink recreated deleted fixture state'
  printf 'ok: explicit systemd sink sync preserves foreign artifacts, disabled intent, events and incidents after fixture cleanup\n'
)

case "$mode" in
  sink-lifecycle) sink_lifecycle ;;
  early-init)
    scratch="$(mktemp -d /tmp/subyard-cleanup-init.XXXXXX)"
    trap 'find "$scratch" -depth -delete' EXIT
    install -d "$scratch/dev/e2e" "$scratch/.build"
    ln -s "$ROOT/scripts" "$scratch/scripts"
    cp dev/e2e/nested-teardown-data-boundary.sh "$scratch/dev/e2e/"
    cat >"$scratch/.build/yard" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = -Y ] && [ "$3" = init ] || exit 90
config="$SUBYARD_CONFIG_HOME/yards/$2/config.env"
bridge="$(sed -n 's/^INCUS_BRIDGE=//p' "$config")"
pool="$(sed -n 's/^SRV_POOL=//p' "$config")"
printf '%s\n%s\n' "$bridge" "$pool" >"$CLEANUP_INIT_RESOURCES"
[ "$(incus network get "$bridge" user.subyard.owner --project default)" = nested-teardown-e2e-v1 ] || exit 91
[ "$(incus storage get "$pool" user.subyard.owner --project default)" = nested-teardown-e2e-v1 ] || exit 92
exit 23
EOF
    chmod 0700 "$scratch/.build/yard"
    rc=0
    CLEANUP_INIT_RESOURCES="$scratch/resources" \
      bash "$scratch/dev/e2e/nested-teardown-data-boundary.sh" || rc=$?
    [ "$rc" = 23 ] || fail "expected injected init failure, got $rc"
    mapfile -t resources <"$scratch/resources"
    networks="$(observe network list --project default --format csv -c n)"
    pools="$(observe storage list --project default --format csv -c n)"
    ! grep -Fxq "${resources[0]}" <<<"$networks" || fail 'network survived early init failure'
    ! grep -Fxq "${resources[1]}" <<<"$pools" || fail 'pool survived early init failure'
    printf 'ok: early init failure removed atomically marked native network and pool\n'
    ;;
  interrupt)
    [ ! -e "$state" ] || fail 'fixture state already exists'
    bash dev/e2e/p0-guest.sh capacity-preflight "$token"
    install -d -m 0700 "$state"
    printf '%s\n' "$token" >"$state/marker"
    printf '%s\n' "$SUBYARD_E2E_GENERATION" >"$state/generation"
    child=''
    cleanup_interruption() {
      if [ -n "$child" ]; then
        kill -KILL -- "-$child" 2>/dev/null || true
        wait "$child" 2>/dev/null || true
        bash dev/e2e/p0-real-incus.sh --cleanup-only
      fi
    }
    trap cleanup_interruption EXIT
    setsid bash dev/e2e/p0-guest.sh real-incus "$token" >"$state/child.log" 2>&1 &
    child=$!
    deadline=$((SECONDS + 1800))
    while :; do
      kill -0 "$child" 2>/dev/null || { tail -n 40 "$state/child.log"; fail 'fixture exited before interruption'; }
      if command -v incus >/dev/null 2>&1 \
        && [ "$(observe list p0-vm --project "$project" -f csv -c s 2>/dev/null || true)" = RUNNING ]; then
        break
      fi
      [ "$SECONDS" -lt "$deadline" ] || fail 'fixture launch exceeded deadline'
      sleep 5
    done
    [ "$(observe project get "$project" user.subyard.p0)" = "$marker" ] || fail 'project marker missing'
    for name in p0-container p0-vm; do
      [ "$(observe config get "$name" user.subyard.p0 --project "$project")" = "$marker" ] \
        || fail 'instance marker missing'
    done
    observe image list --project default --format csv -c f | sort >"$state/images"
    kill -KILL -- "-$child"
    wait "$child" 2>/dev/null || true
    child=''
    observe list --project "$project" -f csv -c n | sort >"$state/instances"
    diff -u <(printf 'p0-container\np0-vm\n') "$state/instances"
    printf 'ok: interrupted only the owned fixture; marked resources retained for the next lease\n'
    ;;
  recover)
    [ "$(cat "$state/marker")" = "$token" ] || fail 'recovery state marker mismatch'
    [ "$(cat "$state/generation")" = "$SUBYARD_E2E_GENERATION" ] || fail 'retained VM generation changed'
    observe list --project "$project" -f csv -c n | sort | diff -u "$state/instances" -
    bash dev/e2e/p0-guest.sh capacity-preflight "$token"
    projects="$(observe project list --format csv -c n)"
    ! grep -Fxq "$project" <<<"$projects" || fail 'real-Incus project remains'
    observe image list --project default --format csv -c f | sort | diff -u "$state/images" -
    # shellcheck source=dev/e2e/lib-p0-capacity.sh
    . "$ROOT/dev/e2e/lib-p0-capacity.sh"
    p0_capacity_init "$token"
    p0_capacity_remove_build_cache
    p0_capacity_remove_root_if_empty
    bash dev/e2e/p0-guest.sh capacity-verify-cleanup "$token"
    find "$state" -depth -delete
    printf 'ok: reacquired VM recovered before capacity checks and preserved image cache\n'
    ;;
  *) fail 'expected interrupt, recover, early-init or sink-lifecycle' ;;
esac

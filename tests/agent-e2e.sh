#!/usr/bin/env bash
# Agent E2E transport copies dirty public inputs, preserves argv and owns only run directories.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
export SUBYARD_E2E_STATE_DIR="$TMP/client"

grep -Fq 'SUBYARD_E2E_ROUTE_REGISTRY:-/var/lib/subyard/e2e-routes' \
  "$ROOT/dev/agent-e2e.sh" \
  || fail 'runner does not use the boot-stable product route registry'
grep -Fq 'target=/var/lib/subyard/e2e-routes' "$ROOT/scripts/03-create-subyard.sh" \
  || fail 'yard route mount and runner registry path diverged'

# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"

[ "$E2E_YARD" = test-yard ] || fail "agent runner default yard is not test-yard"
[ "$STATE_ROOT" = "$TMP/client/yards/test-yard" ] \
  || fail "default generated client state is not yard-scoped"
[ "$IDENTITY" = "$TMP/client/id_ed25519" ] \
  || fail "controller identity is not shared outside yard-scoped state"

scope_snapshot="$(
  env -u SUBYARD_E2E_BASTION_ROUTE \
    -u SUBYARD_E2E_STATE_DIR -u SUBYARD_E2E_YARD_STATE_DIR -u SUBYARD_E2E_IDENTITY \
    -u SUBYARD_E2E_YARD SUBYARD_HOME="$TMP/default-client" \
    bash -c '
      set -euo pipefail
      . "$1/dev/agent-e2e.sh"
      printf "%s|%s|%s|%s\n" \
        "$E2E_YARD" "$BASTION_ROUTE" "$STATE_ROOT" "$IDENTITY"
      E2E_YARD=e2e-yard
      configure_yard_scope
      printf "%s|%s|%s|%s\n" \
        "$E2E_YARD" "$BASTION_ROUTE" "$STATE_ROOT" "$IDENTITY"
    ' _ "$ROOT"
)"
expected_scope_snapshot="$(printf '%s\n%s\n' \
  "test-yard|yard-test-yard|$TMP/default-client/e2e/yards/test-yard|$TMP/default-client/e2e/id_ed25519" \
  "e2e-yard|yard-e2e-yard|$TMP/default-client/e2e/yards/e2e-yard|$TMP/default-client/e2e/id_ed25519")"
[ "$scope_snapshot" = "$expected_scope_snapshot" ] \
  || fail "test-yard and explicit e2e-yard route/state scopes collide: $scope_snapshot"
if "$ROOT/dev/agent-e2e.sh" --yard '../unsafe' --prepare >/dev/null 2>&1; then
  fail "agent runner accepted an unsafe yard selector"
fi
if "$ROOT/dev/agent-e2e.sh" --slot 0 --prepare >/dev/null 2>&1; then
  fail "agent runner accepted an invalid exact slot"
fi
LEASE_REQUESTED_SLOT=''
set_requested_slot 1 SUBYARD_P0_SLOT
[ "$LEASE_REQUESTED_SLOT" = slot-001 ] \
  || fail "P0 exact-slot environment did not resolve slot-001"
if (set_requested_slot 0 SUBYARD_P0_SLOT) >/dev/null 2>&1; then
  fail "P0 exact-slot environment accepted slot zero"
fi
LEASE_REQUESTED_SLOT=''

export SUBYARD_E2E_TEST_MODE=1
export SUBYARD_E2E_WORKSPACES_ROOT="$TMP/workspaces"
fixture="$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/src"
mkdir -p "$fixture/private" "$fixture/temp"
git -C "$fixture" init -q
git -C "$fixture" remote add origin \
  'https://token:private-value@github.com/Subyard/Attribution.git?access=private-value'
printf 'private/\ntemp/\nignored.secret\n' > "$fixture/.gitignore"
printf 'tracked\n' > "$fixture/tracked.txt"
printf 'removed\n' > "$fixture/removed.txt"
printf 'dirty\n' > "$fixture/dirty.txt"
printf 'ignored\n' > "$fixture/ignored.secret"
printf 'private\n' > "$fixture/private/note.txt"
printf 'temp\n' > "$fixture/temp/cache.txt"
git -C "$fixture" add .gitignore tracked.txt removed.txt
printf 'changed\n' >> "$fixture/tracked.txt"
rm "$fixture/removed.txt"

fallback_context="$(resolve_workspace_attribution "$fixture")"
[ "$fallback_context" = $'unknown\tSubyard-2-05398f45' ] \
  || fail "safe legacy workspace fallback changed: $fallback_context"
printf '%s\n' \
  '{"schema":1,"projectId":"Subyard-2-05398f45","name":"Subyard-2","yard":"default","mode":"sync"}' \
  > "$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/.subyard-meta.json"
[ "$(resolve_workspace_attribution "$fixture")" = $'default\tSubyard-2' ] \
  || fail "runner did not use canonical workspace metadata"
cp "$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/.subyard-meta.json" "$TMP/valid-meta"
printf '%s\n' \
  '{"schema":1,"projectId":"foreign","name":"Subyard-2","yard":"default"}' \
  > "$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/.subyard-meta.json"
if (resolve_workspace_attribution "$fixture") >/dev/null 2>&1; then
  fail "runner accepted mismatched project metadata"
fi
mv "$TMP/valid-meta" "$SUBYARD_E2E_WORKSPACES_ROOT/Subyard-2-05398f45/.subyard-meta.json"
run_a="$(new_run_id)"
run_b="$(new_run_id)"
[[ "$run_a" =~ ^[0-9a-f]{8}$ && "$run_b" =~ ^[0-9a-f]{8}$ && "$run_a" != "$run_b" ]] \
  || fail "per-acquire run identities are invalid or reused"
[ "$(derive_purpose run '' bash dev/e2e/release-migration-catch-up.sh auto)" = \
    release-migration-catch-up ] \
  || fail "runner did not derive a bounded script purpose"
[ "$(derive_purpose ssh 'manual diagnostic')" = manual-diagnostic ] \
  || fail "explicit purpose was not normalized"
LEASE_YARD=default
LEASE_PROJECT=Subyard-2
LEASE_CHECKOUT=
LEASE_RUN="$run_a"
LEASE_PURPOSE=contract-tests
LEASE_GENERATION=7
BROKER_ATTRIBUTION_V2=1
lease_request="$(lease_acquire_request client SHA256:key ssh-ed25519 keyblob)"
[ "$lease_request" = \
    "acquire-v2 client SHA256:key default Subyard-2 $run_a contract-tests ssh-ed25519 keyblob" ] \
  || fail "runner did not carry canonical attribution through acquire-v2"
LEASE_REQUESTED_SLOT='slot-002'
exact_request="$(lease_acquire_request client SHA256:key ssh-ed25519 keyblob)"
[ "$exact_request" = \
  "acquire-v2 client SHA256:key default Subyard-2 $run_a contract-tests ssh-ed25519 keyblob slot-002" ] \
  || fail "runner did not retain the existing exact-slot acquire protocol"
BROKER_ATTRIBUTION_V2=0
LEASE_REQUESTED_SLOT=
legacy_request="$(lease_acquire_request client SHA256:key ssh-ed25519 keyblob)"
[ "$legacy_request" = \
  "acquire client SHA256:key Subyard-2+$run_a contract-tests ssh-ed25519 keyblob" ] \
  || fail "old-broker fallback did not use an opaque no-checkout label"
BROKER_ATTRIBUTION_V2=1
LEASE_REQUESTED_SLOT='slot-002'
LEASE_SLOT='slot-002'
lease_grant_matches_request || fail "matching exact-slot grant was rejected"
LEASE_SLOT='slot-001'
if lease_grant_matches_request; then
  fail "mismatched exact-slot grant was accepted before transport"
fi
LEASE_REQUESTED_SLOT=
LEASE_SLOT=

bundle="$TMP/worktree.tar.gz"
build_bundle "$fixture" "$bundle"
contents="$(tar -tzf "$bundle" | sort)"
printf '%s\n' "$contents" | grep -Fxq dirty.txt || fail "dirty untracked file was not copied"
printf '%s\n' "$contents" | grep -Fxq tracked.txt || fail "modified tracked file was not copied"
printf '%s\n' "$contents" | grep -Fxq .subyard-e2e-index \
  || fail "tracked-file inventory was not copied"
! printf '%s\n' "$contents" | grep -Fxq removed.txt || fail "deleted tracked file entered the bundle"
! printf '%s\n' "$contents" | grep -Eq '(^|/)(private|temp|\.git)(/|$)|ignored\.secret' \
  || fail "ignored or private data entered the worktree bundle"
inventory="$(tar -xOf "$bundle" .subyard-e2e-index | tr '\0' '\n')"
printf '%s\n' "$inventory" | grep -Fxq tracked.txt \
  || fail "tracked-file inventory omitted a tracked path"
! printf '%s\n' "$inventory" | grep -Fxq dirty.txt \
  || fail "tracked-file inventory classified an untracked path as tracked"

ln -s /etc/passwd "$fixture/escaping-link"
if (build_bundle "$fixture" "$TMP/unsafe.tar.gz") >/dev/null 2>&1; then
  fail "worktree bundling accepted a symlink outside the repository"
fi
rm "$fixture/escaping-link"

command_root="$TMP/command path"
mkdir -p "$command_root/src"
write_guest_command 2 "$command_root" sh -c \
  'test "$(id -un)" = dev && test "$SUBYARD_E2E_VM" = 2 && test "$1" = "argument with spaces"' \
  fixture 'argument with spaces' \
  > "$TMP/run.sh"
bash -n "$TMP/run.sh" || fail "guest command is not valid shell"
printf -v command_root_q '%q' "$command_root"
grep -Fxq "chown -R dev:dev $command_root_q" "$TMP/run.sh" \
  && grep -Fq 'exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev sh -c' \
    "$TMP/run.sh" \
  && grep -Fq 'fixture argument\ with\ spaces' "$TMP/run.sh" \
  || fail "guest command does not run as dev or preserve its argv"
grep -Fxq 'export SUBYARD_E2E_YARD=test-yard' "$TMP/run.sh" \
  && grep -Fxq 'export SUBYARD_E2E_PROJECT=Subyard-2' "$TMP/run.sh" \
  && grep -Fxq "export SUBYARD_E2E_RUN_ID=$run_a" "$TMP/run.sh" \
  && grep -Fxq 'export SUBYARD_E2E_PURPOSE=contract-tests' "$TMP/run.sh" \
  && grep -Fxq 'export SUBYARD_E2E_GENERATION=7' "$TMP/run.sh" \
  || fail "guest command omitted public lease context"
write_guest_command 1 "$command_root" ./bin/yard --version > "$TMP/yard-run.sh"
grep -Fxq '/usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev ./dev/build-engine.sh' \
  "$TMP/yard-run.sh" \
  || fail "direct guest yard command does not build its explicit development engine"
grep -Fxq 'exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev ./bin/yard --version' \
  "$TMP/yard-run.sh" \
  || fail "direct guest yard command changed its argv after the development build"
quoted="$(quote_ssh_command bash -c 'test "$1" = "argument with spaces"' _ 'argument with spaces')"
bash -c "$quoted" || fail "direct SSH command did not preserve its argv"

normalized_progress="$({
  printf 'download 1%%\rdownload 80%%\rdownload 100%%\n'
  printf 'plain output\n'
  printf 'apt 95%%\rapt 100%%\r\n'
  printf 'final line without newline'
} | normalize_terminal_progress)"
[ "$normalized_progress" = "$(printf '%s\n%s\n%s\n%s' \
  'download 100%' 'plain output' 'apt 100%' 'final line without newline')" ] \
  || fail "runner did not coalesce terminal progress to its final update"
grep -Fq '2>&1 | normalize_terminal_progress' "$ROOT/dev/agent-e2e.sh" \
  || fail "normal guest streams bypass terminal-progress normalization"

mkdir -p "$TMP/direct-bin"
cat > "$TMP/direct-bin/ssh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *' e2e-vm-1 '*)
    IFS= read -r forwarded
    [ "$forwarded" = explicit-stdin ]
    ;;
  *)
    if IFS= read -r leaked; then
      printf 'direct SSH leaked stdin: %s\n' "$leaked" >&2
      exit 91
    fi
    ;;
esac
printf '%s\n' "$*"
SH
chmod +x "$TMP/direct-bin/ssh"
direct_ssh="$(
  PATH="$TMP/direct-bin:$PATH" ROOT="$ROOT" bash -c '
    set -euo pipefail
    . "$ROOT/dev/agent-e2e.sh"
    CLIENT_CONFIG=/tmp/direct-ssh-config
    printf "must-not-reach-ssh\n" | run_direct_ssh 2 0 printf "%s" "argument with spaces"
  '
)"
printf '%s\n' "$direct_ssh" | grep -Fq -- '-T e2e-vm-2 --' \
  || fail "direct SSH command did not use the pinned non-TTY VM route"
direct_ssh_stdin="$(
  PATH="$TMP/direct-bin:$PATH" ROOT="$ROOT" bash -c '
    set -euo pipefail
    . "$ROOT/dev/agent-e2e.sh"
    CLIENT_CONFIG=/tmp/direct-ssh-config
    printf "explicit-stdin\n" | run_direct_ssh 1 1 sh -c "read -r value"
  '
)"
printf '%s\n' "$direct_ssh_stdin" | grep -Fq -- '-T e2e-vm-1 --' \
  || fail "explicit direct SSH stdin did not use the pinned non-TTY VM route"
grep -Fq 'guest 1 \' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'dd of="$1" status=none' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 source archive does not use the lease-local stdin transport"
grep -Fq 'set_requested_slot "$SUBYARD_P0_SLOT" SUBYARD_P0_SLOT' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 exact-slot environment does not reach the atomic lease request"
grep -Fq 'run_guest "$vm" "$P0_BUNDLE" "$P0_BUNDLE_HASH" \' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 acceptance repeats the leased runner's dev privilege transition"
grep -Fq 'run_vm "$vm" capacity-preflight' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'capacity-verify-cleanup' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'capacity_report' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 acceptance does not enforce capacity preflight, peak reporting and exact cleanup"
grep -Fq 'P0_E2E_MIN_PEAK_MEMORY_RESERVE_BYTES:-67108864' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 acceptance does not keep only the 64 MiB minimum peak memory reserve"
lane_inventory="$("$ROOT/dev/e2e/p0-acceptance.sh" --list-lanes)"
for lane in boundary nested-teardown transport dependencies real-incus profile-resource release source-upgrade \
  reboot-verify peer peer-cleanup cleanup; do
  grep -qx "$lane" <<<"$lane_inventory" || fail "P0 lane inventory omitted $lane"
done
grep -Fq $'full\tboundary nested-teardown transport release source-upgrade peer cleanup' <<<"$lane_inventory" \
  || fail 'continuous P0 gate lost a mandatory public-contract phase'
grep -Fq '.allocation == {slot: $slot, resource_generation: $generation}' \
  "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq '.bundle_hash == $bundle' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq "die 'checkpoint does not match this allocation generation and exact bundle hash'" \
    "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail 'P0 resume checkpoint does not fail closed on allocation or bundle drift'
grep -Fq 'P0_CURRENT_PHASE=final-verify' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && grep -Fq 'run_phase cleanup cleanup_lane' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && [ "$(grep -c '^    verify_boundary$' "$ROOT/dev/e2e/p0-acceptance.sh")" -eq 1 ] \
  || fail 'continuous P0 can skip final cleanup or boundary verification'
grep -Fq 'prepare_slot "$slot"' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'LEASE_PURPOSE=acceptance-prepare' \
    "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'acquire_lease' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'guest "$vm" sh -eu -c' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'fstrim -av' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'dump_broker_diagnostics' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  || fail "P1 acceptance does not prepare first-boot slots sequentially with diagnostics"
grep -Fq 'systemctl is-enabled --quiet subyard-e2e-lease-context.service' \
  "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq '/proc/sys/kernel/random/boot_id' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq '/run/subyard-e2e-lease.json' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  || fail 'lease acceptance does not verify context restoration after a holder reboot'
grep -Fq 'go build -cover' "$ROOT/dev/process-coverage.sh" \
  && grep -Fq 'go tool covdata merge' "$ROOT/dev/process-coverage.sh" \
  && grep -Fq 'SUBYARD_SHELL_COVERAGE_LOG' "$ROOT/dev/process-coverage.sh" \
  && grep -Fq 'bundle_hash' "$ROOT/dev/process-coverage.sh" \
  || fail 'process coverage does not merge an instrumented yard with Shell inventory evidence'
grep -Fq 'reclaim_owner_lease_capacity' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'owner lease fixture pool reserve' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'p0_capacity_reclaim_dependency_caches' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'OWNER_BASELINE_IMAGES' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not reclaim only test-owned migration capacity"
grep -Fq '/tmp/subyard-hermes-profile.*/storage' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq '[ "$status" = Unavailable ] && [ "$used_by" = '\''[]'\'' ]' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'P0 preflight does not narrowly recover an unused stale test pool'
grep -Fq 'cold Go dependency download heartbeat elapsed=' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'dependency download failed (attempt %s/3); retrying in %ss' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'timeout --signal=TERM --kill-after=10' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail 'dependency bootstrap lacks bounded retry and heartbeat progress'
grep -Fq 'Incus default-pool query exceeded' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'timeout --foreground "$query_timeout"' \
    "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  || fail 'P0 capacity preflight can hang on a partial Incus daemon'
grep -Fq 'p0_capacity_recover_stale_roots' "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  && grep -Fq 'stale P0 state still has an active process' \
    "$ROOT/dev/e2e/lib-p0-capacity.sh" \
  || fail 'P0 preflight cannot distinguish orphaned from active marker-owned cache state'
grep -Fq 'OWNER_DIAGNOSTIC_DEV_UID="${P0_E2E_DIAGNOSTIC_DEV_UID:-1001}"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'chown -R "$OWNER_DIAGNOSTIC_DEV_UID:$OWNER_DIAGNOSTIC_DEV_UID" "$bound"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 bind fixture is not owned by its configured diagnostic yard UID"
grep -Fq 'systemctl start subyard-test-vms-host-sink.service' \
  "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  && grep -Fq 'test-vms-broker-incidents' "$ROOT/dev/e2e/p1-lease-acceptance.sh" \
  || fail "P1 diagnostics do not flush and print the immutable broker incident"
grep -Fq 'FAULT_ROOT=/run/subyard-p0-incus-fault' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'reclaim_held_pair_capacity "$VICTIM_CONFIG" victim' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'reclaim_held_pair_capacity "$NEIGHBOR_CONFIG" neighbor' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && grep -Fq 'outer_root systemctl mask --runtime --now \' \
    "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  && awk '
    /^stop_slot_pair 2$/ { stopped = NR }
    /^rollback_candidate_update$/ { rolled_back = NR }
    /outer_root systemctl unmask --runtime/ { unmasked = NR }
    END {
      exit !(stopped && rolled_back && unmasked &&
        stopped < rolled_back && rolled_back < unmasked)
    }
  ' "$ROOT/dev/e2e/p0-broker-recovery.sh" \
  || fail "P0 broker recovery does not isolate its targeted fault before rebuilding"
if grep -Fq 'mask --runtime --now incus.service' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh"; then
  fail "P0 broker recovery fault injection drains unrelated held leases"
fi
grep -Fq '> "$PEER_ROOT/config/config.env"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'P0_PEER_YARD_TIMEOUT:-600' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer yard does not use its active config root with a bounded init"
grep -Fq '"$candidate_yard" _migrate finalize' \
  "$ROOT/scripts/migrate-source-install.sh" \
  && ! grep -Fq '_migrate-test-yard' "$ROOT/dev/bootstrap-runtime.sh" \
  || fail "source bootstrap does not use the generic ordered migration lifecycle"

ensure_identity
lease_blob="$(awk '{print $2}' "$IDENTITY.pub")"
lease_response="$(printf '{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-001","resource_generation":5,"lease_id":"aabb","capability":"ccdd","lease_epoch":3,"data_user":"subyard-e2e-slot-1","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.1.11","host_key_type":"ssh-ed25519","host_key_blob":"%s"},{"selector":2,"name":"e2e-vm-2","address":"10.42.1.12","host_key_type":"ssh-ed25519","host_key_blob":"%s"}]}}' "$lease_blob" "$lease_blob")"
parse_lease_grant "$lease_response" \
  || fail "valid lease grant was rejected"
[ "$LEASE_SLOT" = slot-001 ] && [ "$DATA_USER" = subyard-e2e-slot-1 ] \
  && [ "$LEASE_GENERATION" = 5 ] \
  && [ "${VM_IP[1]}" = 10.42.1.11 ] && [ "${VM_IP[2]}" = 10.42.1.12 ] \
  || fail "lease grant did not materialize exact fenced transport state"
if (parse_lease_grant '{"status":"ok","grant":{"capability":"secret"}}') >/dev/null 2>&1; then
  fail "incomplete lease grant was accepted"
fi
LEASE_PROJECT=Subyard/Attribution
LEASE_CHECKOUT='checkout-a'
LEASE_RUN='run-a'
LEASE_PURPOSE=contract-tests
structured_response="$(printf '{"schema_version":1,"status":"ok","grant":{"slot_id":"slot-002","lease_id":"eeff","capability":"1122","lease_epoch":4,"context":{"schema_version":1,"project":"Subyard/Attribution","checkout":"checkout-a","run":"run-a","purpose":"contract-tests"},"data_user":"subyard-e2e-slot-2","targets":[{"selector":1,"name":"e2e-vm-1","address":"10.42.2.11","host_key_type":"ssh-ed25519","host_key_blob":"%s"},{"selector":2,"name":"e2e-vm-2","address":"10.42.2.12","host_key_type":"ssh-ed25519","host_key_blob":"%s"}]}}' "$lease_blob" "$lease_blob")"
parse_lease_grant "$structured_response" \
  || fail "structured lease grant was rejected"
[ "$LEASE_SLOT" = slot-002 ] \
  || fail "structured lease context was lost"
changed_context="$(jq -c '.grant.context.project = "Foreign/Project"' <<<"$structured_response")"
if (parse_lease_grant "$changed_context") >/dev/null 2>&1; then
  fail "facade attribution mismatch was accepted"
fi
ensure_identity
BASTION_HOSTNAME=127.0.0.1
BASTION_PORT=2223
BASTION_HOST_KEY_ALIAS=''
BASTION_KNOWN_HOSTS="$TMP/bastion-known-hosts"
DATA_USER=subyard-e2e-slot-1
GUEST_IDENTITY="$TMP/lease-key"
cp "$IDENTITY" "$GUEST_IDENTITY"
printf '[127.0.0.1]:2223 %s\n' "$(normalized_public_key_file "$IDENTITY.pub")" > "$BASTION_KNOWN_HOSTS"
write_client_config
grep -Fxq '    ProxyJump subyard-e2e-data' "$CLIENT_CONFIG" \
  && grep -Fxq '    User subyard-e2e-slot-1' "$CLIENT_CONFIG" \
  || fail "VM aliases do not use the lease-scoped data account"
grep -Fxq '    ForwardAgent no' "$CLIENT_CONFIG" \
  || fail "generated SSH config permits agent forwarding"
[ "$(grep -c '^Host e2e-vm-' "$CLIENT_CONFIG")" -eq 2 ] \
  || fail "generated SSH config does not expose exactly two VM aliases"
[ "$(grep '^[[:space:]]*IdentityFile ' "$CLIENT_CONFIG" | sort -u | wc -l)" -eq 2 ] \
  || fail "controller and ephemeral guest identities were not separated"

cat > "$TMP/route-config" <<EOF
Host fixture-e2e-yard
    HostName 127.0.0.1
    Port 2223
    UserKnownHostsFile $BASTION_KNOWN_HOSTS
EOF
# shellcheck disable=SC2100 # This is an SSH host alias, not an arithmetic expression.
BASTION_ROUTE=fixture-e2e-yard
BASTION_HOSTNAME=''; BASTION_PORT=''; BASTION_HOST_KEY_ALIAS=''; BASTION_KNOWN_HOSTS=''
SUBYARD_E2E_ROUTE_CONFIG="$TMP/route-config"
SUBYARD_E2E_ROUTE_REGISTRY="$TMP/empty-route-registry"
resolve_bastion_route
[ "$BASTION_HOSTNAME:$BASTION_PORT" = 127.0.0.1:2223 ] \
  || fail "bastion route was not resolved from the isolated user SSH config"
[ "$BASTION_KNOWN_HOSTS" = "$TMP/bastion-known-hosts" ] \
  || fail "bastion route did not reuse its pre-pinned host key"

route_registry="$TMP/route-registry"
mkdir -p "$route_registry/test-yard/.route-fixture"
ln -s .route-fixture "$route_registry/test-yard/current"
cat > "$route_registry/test-yard/current/route.tsv" <<'EOF'
subyard-e2e-route-v1
hostname	10.24.0.8
port	22
host_key_alias	subyard-e2e-bastion
EOF
printf 'subyard-e2e-bastion %s\n' "$(normalized_public_key_file "$IDENTITY.pub")" \
  > "$route_registry/test-yard/current/known_hosts"
BASTION_HOSTNAME=''; BASTION_PORT=''; BASTION_HOST_KEY_ALIAS=''; BASTION_KNOWN_HOSTS=''
SUBYARD_E2E_ROUTE_REGISTRY="$route_registry"
resolve_bastion_route
[ "$BASTION_HOSTNAME:$BASTION_PORT:$BASTION_HOST_KEY_ALIAS" = \
    10.24.0.8:22:subyard-e2e-bastion ] \
  || fail "root-published shared bastion route was not selected"
[ "$BASTION_KNOWN_HOSTS" = "$route_registry/test-yard/current/known_hosts" ] \
  || fail "product-owned bastion route lost its pinned host key"

status_fixture='{"schema_version":1,"status":"ok","capabilities":["attribution-v2"],"pool":{"schema_version":1,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":1,"lease_epoch":3,"state":"held","yard":"default","project":"Subyard-2","run":"run-a","purpose":"contract-tests","acquired_at":"2026-07-26T20:00:00Z","expires_at":"2026-07-26T20:20:00Z"},{"slot_id":"slot-002","resource_generation":1,"lease_epoch":2,"state":"available"},{"slot_id":"slot-003","resource_generation":1,"lease_epoch":0,"state":"available","acquired_at":"0001-01-01T00:00:00Z","expires_at":"0001-01-01T00:00:00Z"}]}}'
rendered_status="$(render_pool_status "$status_fixture")"
printf '%s\n' "$rendered_status" | grep -Fq 'SLOT     STATE' \
  && printf '%s\n' "$rendered_status" | grep -Fq 'Subyard-2' \
  || fail "human pool status omitted active-holder attribution"
! printf '%s\n' "$rendered_status" | grep -Fq '0001-01-01' \
  || fail "human pool status exposed zero timestamps"
long_project=Project-abcdefghijklmnopqrstuvwxyz0123456789
long_status="$(jq -c --arg project "$long_project" \
  '.pool.slots[0].project = $project' <<<"$status_fixture")"
long_rendered_status="$(render_pool_status "$long_status")"
grep -Fq "$long_project" <<<"$long_rendered_status" \
  || fail "human pool status truncated an attribution identifier"

# Model direct guest SSH and cleanup locally.
guest() {
  shift
  if [ "${1:-}" = sudo ] && [ "${2:-}" = -n ]; then shift 2; fi
  case "${1:-}" in
    /tmp/subyard-worktree.*/run.sh)
      sed \
        -e '/^chown -R dev:dev /d' \
        -e 's#^exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev #exec #' \
        "$1" | bash
      return
      ;;
  esac
  "$@"
}
mock_bundle="$TMP/mock.tar.gz"
tar -C "$fixture" -czf "$mock_bundle" tracked.txt
mock_hash="$(sha256sum "$mock_bundle" | awk '{print $1}')"
run_guest 1 "$mock_bundle" "$mock_hash" test -f tracked.txt \
  || fail "mock guest command failed"
guest_directory="${GUEST_DIRS[1]:-}"
case "$guest_directory" in /tmp/subyard-worktree.*) ;; *) fail "guest run directory was not retained for cleanup" ;; esac
[ -d "$guest_directory" ] || fail "mock guest run directory is missing"
cleanup_guest 1 || fail "guest run directory cleanup failed"
[ ! -e "$guest_directory" ] || fail "guest run directory survived cleanup"

set +e
bash -c '
  set -euo pipefail
  . "$1/dev/agent-e2e.sh"
  GUEST_DIRS[1]=/tmp/subyard-worktree.fixture
  cleanup_guest() { return 1; }
  cleanup_on_exit
' _ "$ROOT" >/dev/null 2>&1
cleanup_rc=$?
set -e
[ "$cleanup_rc" = 3 ] || fail "trap cleanup failure returned $cleanup_rc instead of 3"

if sed '/^[[:space:]]*#/d' "$ROOT/dev/agent-e2e.sh" \
  | grep -Eq 'test-vms[[:space:]]+(up|down)|yard[[:space:]].*(start|stop)'; then
  fail "agent E2E transport contains an allocation lifecycle call"
fi
if sed '/^[[:space:]]*#/d' "$ROOT/dev/e2e/p0-acceptance.sh" \
  | grep -Eq 'test-vms[[:space:]]+(up|down)|yard[[:space:]].*(start|stop)'; then
  fail "P0 acceptance contains an allocation lifecycle call"
fi
grep -Fq 'trap owner_cleanup EXIT' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not clean its candidate after failure"
grep -Fq 'prepare_owner_go_cache' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'p0_capacity_reset_build_cache' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'p0_capacity_remove_build_cache' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'env -u GOCACHE go clean -cache' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane leaves candidate Go caches on the disposable VM"
grep -Fq 'dev/build-engine.sh --force' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not build an explicit source candidate"
grep -Fq 'scripts/install-runtime-release.sh' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not install an immutable candidate runtime"
grep -Fq 'RENAME_BASE_REVISION=' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not install the real pre-rename runtime"
grep -Fq "grep -Fc 'Timeout:        10 * time.Minute,'" "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq "sed -i 's/Timeout:        10 \\* time.Minute,/Timeout:        20 * time.Minute,/'" \
    "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not bound its synthetic legacy timeout override"
grep -Fq 'write_owner_registration e2e-yard e2e-vms' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not exercise the retired registration"
grep -Fq 'OWNER_DIAGNOSTIC_VM_MEMORY="${P0_E2E_DIAGNOSTIC_VM_MEMORY:-700MiB}"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'OWNER_DIAGNOSTIC_VM_MEMORY="${P0_BROKER_RECOVERY_VM_MEMORY:-700MiB}"' \
    "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'E2E_VM_MEMORY=%s' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq ': "${E2E_VM_MEMORY:=4GiB}"' "$ROOT/scripts/e2e-lab/provision.sh" \
  || fail "P0 memory limit is not diagnostic-only or changed the production default"
grep -Fq 'OWNER_DIAGNOSTIC_VM_BOOT_TIMEOUT="${P0_E2E_DIAGNOSTIC_VM_BOOT_TIMEOUT:-600}"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'E2E_VM_BOOT_TIMEOUT=%s' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq ': "${E2E_VM_BOOT_TIMEOUT:=300}"' "$ROOT/scripts/e2e-lab/provision.sh" \
  || fail "P0 boot timeout is not diagnostic-only or changed the production default"
grep -Fq 'runtime activation retained the old e2e-yard registration' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not verify automatic retirement of the old yard"
grep -Fq 'features.images=false -c user.subyard.p0-image-cache="$MARKER"' \
  "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not attach its test-owned image cache before fresh reconciliation"
owner_bootstrap_line="$(grep -n $'^\tensure_owner_incus$' "$ROOT/dev/e2e/p0-guest.sh" | head -n1 | cut -d: -f1)"
owner_incus_line="$(grep -n 'OWNER_BASELINE_IMAGES=.*incus image list' "$ROOT/dev/e2e/p0-guest.sh" | head -n1 | cut -d: -f1)"
[ -n "$owner_bootstrap_line" ] && [ -n "$owner_incus_line" ] \
  && [ "$owner_bootstrap_line" -lt "$owner_incus_line" ] \
  || fail "P0 owner lane uses Incus before its disposable-VM bootstrap"
grep -Fq './bin/yard -Y test-yard start --yes' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not make start automation explicit"
grep -Fq 'shell "$source" --yes --' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not confirm shell automation"
grep -Fq 'export "$source" --yes' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 owner lane does not confirm export automation"
grep -Fq 'exec %q/yard "$@"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq '"$release/subyard-install.sh" --yes' "$ROOT/dev/e2e/p0-guest.sh" \
  && ! grep -Fq 'YARD_ENGINE_PATH=%q' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer wrapper does not use its release-installed runtime"
grep -Fq 'PEER_YARD_ENTRY="$HOME/.local/bin/yard"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'VM1 user yard entry was not restored exactly' "$ROOT/dev/e2e/p0-acceptance.sh" \
  && ! grep -Fq '/usr/local/bin/yard' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer wrapper does not preserve the login-PATH user entrypoint"
grep -Fq 'UserKnownHostsFile="$PEER_SSH_DIR/known_hosts"' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'ConnectTimeout=8' "$ROOT/dev/e2e/p0-guest.sh" \
  && grep -Fq 'id_ed25519"' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 cross-owner SSH lacks its synthetic identity, strict pin or bounded timeout"
grep -Fq 'remove_peer_authorization' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer cleanup does not revoke its synthetic SSH authorization"
credentials_line="$(grep -n '^peer_credentials()' "$ROOT/dev/e2e/p0-guest.sh" | cut -d: -f1)"
projects_line="$(grep -n '^peer_projects()' "$ROOT/dev/e2e/p0-guest.sh" | cut -d: -f1)"
remote_remove_line="$(grep -n 'remote remove peer --yes' "$ROOT/dev/e2e/p0-guest.sh" | cut -d: -f1)"
[ -n "$credentials_line" ] && [ -n "$projects_line" ] && [ -n "$remote_remove_line" ] \
  && [ "$remote_remove_line" -gt "$credentials_line" ] \
  && [ "$remote_remove_line" -lt "$projects_line" ] \
  || fail "P0 peer alias is removed before its credentials consumer finishes"
grep -Fq 'incus "$@" </dev/null; }' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'real_incus_quiet launch "$VM_IMAGE" p0-vm' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'CONTAINER_CACHE_ALIAS="${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'VM_CACHE_ALIAS="${P0_REAL_INCUS_VM_CACHE_ALIAS:-' "$ROOT/dev/e2e/p0-real-incus.sh" \
  || fail "P0 real-Incus lane leaves YAML-reading control-plane stdin open"
grep -Fq 'wait_ready p0-container container' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'wait_ready p0-vm virtual-machine' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq -- '-c security.secureboot=false' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'P0_REAL_INCUS_RESTART_GRACE_ATTEMPTS:-30' \
    "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'stopped during first boot; replacing it once' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'relaunching real Incus VM after first-boot stop' "$ROOT/dev/e2e/p0-real-incus.sh" \
  || fail "P0 real-Incus lane does not bound first-boot VM recovery with deterministic boot policy"
grep -Fq 'cleanup delete of %s failed; retrying (%s/3)' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'refusing to delete unmarked instance' "$ROOT/dev/e2e/p0-real-incus.sh" \
  && grep -Fq 'could not delete marked instance $name after 3 attempts' "$ROOT/dev/e2e/p0-real-incus.sh" \
  || fail "P0 real-Incus cleanup retry is not bounded to marked test instances"
grep -Fq 'guest "$vm" \' "$ROOT/dev/e2e/p0-acceptance.sh" \
  || fail "P0 peer cleanup assertion bypasses direct-command argv quoting"
grep -Fq 'cleanup_peer_incus' "$ROOT/dev/e2e/p0-guest.sh" \
  || fail "P0 peer lane does not clean its Incus fixture"
grep -Fq '. "$ROOT/tests/helpers/test-context.sh"' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'run_incus_installer --yes --zabbly' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && ! grep -Fq '"$ROOT/scripts/01-install-incus.sh" --yes --zabbly' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade bootstrap bypasses the typed test engine context"
grep -Fq 'POWER_RETRY_WRAPPER=' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq '"ExecStart=$POWER_RETRY_WRAPPER"' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'exec /usr/local/libexec/subyard/yard-boot-reconcile _power-reconcile' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && ! grep -Fq 'ExecStartPre=' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade TEMPFAIL probe does not exercise the main reconciler process"
! grep -Fq '"$SOURCE_ROOT/config/qa-pool/"*' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade fixture expands operator-private paths as the outer user"
grep -Fq 'AGENTS=codex\nAGENT_codex_RULES=' "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade spends its legacy init deadline on unrelated agent downloads"
grep -Fq 's/^YARD_TEMPLATE=e2e-vms$/YARD_TEMPLATE=test-vms/' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade lane does not verify the retired template migration"
grep -Fq 'migration_transaction_directory "$VERSION_B"' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'to_release="$(jq -er ".toRelease' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && ! grep -Fq '[ "${#entries[@]}" -eq 1 ]' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  || fail "P0 source-upgrade lane does not select its journal by release identity"
grep -Fq 'OLD_VERSION=0.3.1' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'MISSED_VERSION=0.4.0' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'host_incus config device get "$CONSUMER_INSTANCE"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'running standard broker acquire from the pre-existing consumer' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'consumer restarted during route reconciliation' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up lanes do not cover both published histories and live consumer routing"
grep -Fq 'cleanup_owned_host_incus' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq '[ "$source" = "$PLATFORM_STORAGE" ]' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'host_incus storage delete default --project default' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up cleanup can leave its fixture-owned default Incus pool behind"
grep -Fq 'BROKEN_VERSION=0.4.1' "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'RELEASE_040_TARGET=releases/0.4.0-68b9925f6880' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'RELEASE_041_TARGET=releases/0.4.1-fc5b03078508' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'validate_hotfix_transaction rolling-back rolling-back' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'sync -f "$(dirname "$journal")"' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'published 0.4.0 -> broken 0.4.1 -> recovered 0.4.3 hotfix lane passed' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up hotfix lane does not cover exact published recovery and candidate retry"
grep -Fq 'clean published 0.4.0 -> 0.4.3 hotfix lane passed' \
  "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'legacy published 0.4.0 owner -> 0.4.3 hotfix lane passed' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'require_operator_password_sudo' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'operator_env sudo -k' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'spawn -noecho $env(SUBYARD_YARD_BIN) update' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'operator unexpectedly retained passwordless sudo' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && [ "$(grep -Fc '  upgrade_candidate present' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh")" -eq 4 ] \
  || fail "release hotfix lanes do not exercise ordinary updates with cold password-required sudo"
grep -Fq 'FAILED_HOTFIX_VERSION=0.4.2' \
  "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'RELEASE_042_TARGET=releases/0.4.2-17608894ab09' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'validate_failed_hotfix_transaction' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'repair_failed_hotfix_operation test-vm-broker-runtime' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  && grep -Fq 'published 0.4.0 -> broken 0.4.2 -> recovered 0.4.3 hotfix lane passed' \
    "$ROOT/dev/e2e/release-migration-catch-up.sh" \
  || fail "release catch-up does not reproduce and recover the exact broken 0.4.2 transaction"
grep -Fq 'require_operator_password_sudo' \
  "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'assert_operator_password_sudo' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'restore_operator_passwordless_sudo' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && grep -Fq 'operator unexpectedly retained passwordless sudo' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh" \
  && [ "$(grep -Fc '  require_operator_password_sudo' \
    "$ROOT/dev/e2e/p0-source-upgrade.sh")" -eq 2 ] \
  || fail "P0 source-upgrade reboots do not preserve a password-required operator boundary"
grep -Fq 'dev/agent-e2e.sh --wait 20m --vm both' \
  "$ROOT/dev/e2e/release-migration-consumer.sh" \
  && grep -Fq 'dev/agent-e2e.sh --verify-boundary' \
    "$ROOT/dev/e2e/release-migration-consumer.sh" \
  || fail "release catch-up consumer bypasses the standard broker facade"
grep -Fq 'select(.slot_id == $slot)' "$ROOT/dev/agent-e2e.sh" \
  && grep -Fq 'current lease slot is absent from pool status' "$ROOT/dev/agent-e2e.sh" \
  || fail "agent E2E boundary verification is coupled to unrelated concurrent slots"
! grep -Fq 'test-vms-inner' "$ROOT/dev/agent-e2e.sh" \
  || fail "agent E2E transport still invokes the privileged lifecycle worker"

printf 'ok: agent E2E lease transport is pinned, fenced and cleanup-owned\n'

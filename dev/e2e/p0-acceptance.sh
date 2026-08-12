#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CONFIG=''
TOKEN=''
P0_BUNDLE=''
P0_BUNDLE_HASH=''
PEERS_READY=0
PROBE_PID=''
PROBE_MARKER=''
PROBE_NAME=''
PROBE_LOG=''
VM1_YARD_ENTRY=''
VM2_YARD_ENTRY=''
VM1_SSH_STATE=''
VM2_SSH_STATE=''
SOURCE_ARCHIVE=''
SOURCE_ARCHIVE_REMOTE=''
SOURCE_HASH=''
SOURCE_COMMIT=''
SOURCE_HOST_STARTED=0
CANDIDATE_HASH=''
CAPACITY_LOG_DIR=''
PEERS_ONLY="${SUBYARD_P0_PEERS_ONLY:-0}"
BROKER_RECOVERY_ONLY="${SUBYARD_P0_BROKER_RECOVERY_ONLY:-0}"
P0_NESTED_VM="${SUBYARD_P0_NESTED_VM:-1}"
declare -A CAPACITY_PID=()
declare -A CAPACITY_FLAG=()
declare -A DEFAULT_BUILD_CACHE_BEFORE=()
declare -A MODULE_CACHE_BEFORE=()
declare -A HOME_STATE_BEFORE=()
P0_LANE=full
P0_RESUME=0
P0_CHECKPOINT=''
P0_EVIDENCE=''
P0_FAILURE_LOG=''
P0_CURRENT_PHASE='startup'
P0_PHASE_STARTED=0
P0_CHILD_PIDS=()
FULL_P0_LANES=(boundary nested-teardown transport release source-upgrade peer cleanup)

# Reuse one ordinary broker lease for the full matrix. This avoids the retired raw SSH-config
# export and ensures every direct and bundled command addresses the same retained pair.
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"

die() { printf 'p0-acceptance: %s\n' "$*" >&2; exit 2; }
usage() {
  cat <<'EOF'
Usage:
  dev/e2e/p0-acceptance.sh
  dev/e2e/p0-acceptance.sh --lane NAME [--resume]
  dev/e2e/p0-acceptance.sh --list-lanes

The no-argument form is the continuous release gate. Targeted lanes are diagnostics and do not
replace it. --resume reuses passed checkpoints only for the same slot resource generation and exact
worktree bundle hash; use SUBYARD_P0_SLOT=N to request that retained allocation again.
EOF
}
list_lanes() {
  printf '%s\n' \
    boundary nested-teardown transport dependencies real-incus profile-resource release source-upgrade \
    reboot-verify peer peer-cleanup cleanup
  printf 'full\t%s\n' "${FULL_P0_LANES[*]}"
}
parse_arguments() {
  local lane_seen=0
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --lane)
        [ "$#" -ge 2 ] || die '--lane needs a name'
        [ "$lane_seen" = 0 ] || die '--lane may be specified only once'
        P0_LANE="$2"
        lane_seen=1
        shift 2
        ;;
      --resume) P0_RESUME=1; shift ;;
      --list-lanes) list_lanes; exit 0 ;;
      -h | --help) usage; exit 0 ;;
      *) die "unknown argument '$1'" ;;
    esac
  done
  case "$P0_LANE" in
    boundary|nested-teardown|transport|dependencies|real-incus|profile-resource|release|source-upgrade|reboot-verify|peer|peer-cleanup|cleanup|full) ;;
    *) die "unknown lane '$P0_LANE'" ;;
  esac
  [ "$P0_LANE" = full ] || [ "$BROKER_RECOVERY_ONLY" = 0 ] \
    || die 'broker-recovery-only cannot be combined with --lane'
  [ "$P0_RESUME" = 0 ] || [ "$P0_LANE" != cleanup ] \
    || die '--resume is not meaningful for cleanup'
  if [ "$PEERS_ONLY" = 1 ] && [ "$lane_seen" = 0 ]; then
    P0_LANE=peer
  fi
  case "$P0_NESTED_VM" in
    1|2) ;;
    *) die 'SUBYARD_P0_NESTED_VM must be 1 or 2' ;;
  esac
}
parse_arguments "$@"
public_tree_hash() {
  local path kind mode digest
  while IFS= read -r -d '' path; do
    if [ -L "$ROOT/$path" ]; then
      kind='link'
      mode=120000
      digest="$(readlink "$ROOT/$path" | sha256sum | awk '{print $1}')"
    elif [ -f "$ROOT/$path" ]; then
      kind='file'
      mode="$(stat -c '%a' "$ROOT/$path")"
      digest="$(sha256sum "$ROOT/$path" | awk '{print $1}')"
    else
      continue
    fi
    printf '%s\0%s\0%s\0%s\0' "$path" "$kind" "$mode" "$digest"
  done < <(git -C "$ROOT" ls-files --cached --others --exclude-standard -z | sort -z)
}
prepare_run_records() {
  local checkpoint_dir evidence_dir temp
  checkpoint_dir="$STATE_ROOT/p0-checkpoints"
  evidence_dir="$STATE_ROOT/evidence"
  install -d -m 0700 "$checkpoint_dir" "$evidence_dir"
  P0_CHECKPOINT="$checkpoint_dir/$LEASE_SLOT.json"
  P0_EVIDENCE="$evidence_dir/p0-$LEASE_RUN.json"
  if [ "$P0_RESUME" = 1 ]; then
    [ -r "$P0_CHECKPOINT" ] || die "no checkpoint exists for $LEASE_SLOT"
    jq -e --arg slot "$LEASE_SLOT" --argjson generation "$LEASE_GENERATION" \
      --arg bundle "$P0_BUNDLE_HASH" '
      .schema_version == 1 and
      .allocation == {slot: $slot, resource_generation: $generation} and
      .bundle_hash == $bundle and
      (.lanes | type == "object") and
      (.resource_inventory | type == "array")
    ' "$P0_CHECKPOINT" >/dev/null \
      || die 'checkpoint does not match this allocation generation and exact bundle hash'
    return
  fi
  temp="$(mktemp "$checkpoint_dir/.checkpoint.XXXXXX")"
  jq -n --arg slot "$LEASE_SLOT" --argjson generation "$LEASE_GENERATION" \
    --arg bundle "$P0_BUNDLE_HASH" --arg marker "subyard-p0-$TOKEN" '
    {
      schema_version: 1,
      allocation: {slot: $slot, resource_generation: $generation},
      bundle_hash: $bundle,
      lanes: {},
      resource_inventory: [
        ("marker:" + $marker),
        "guest:vm1", "guest:vm2",
        "fixture:peer", "fixture:source-upgrade", "fixture:real-incus"
      ]
    }
  ' > "$temp"
  chmod 0600 "$temp"
  mv -f "$temp" "$P0_CHECKPOINT"
}
checkpoint_passed() {
  jq -e --arg lane "$1" '.lanes[$lane] == "passed"' "$P0_CHECKPOINT" >/dev/null
}
mark_checkpoint_passed() {
  local lane="$1" temp
  temp="$(mktemp "$(dirname "$P0_CHECKPOINT")/.checkpoint.XXXXXX")"
  jq --arg lane "$lane" '.lanes[$lane] = "passed"' "$P0_CHECKPOINT" > "$temp"
  chmod 0600 "$temp"
  mv -f "$temp" "$P0_CHECKPOINT"
}
write_evidence() {
  local phase="$1" status="$2" rc="$3" duration="$4" temp oldest capacity keeper
  [ -n "$P0_EVIDENCE" ] || return 0
  capacity="$(capacity_evidence_json)"
  keeper=''
  if [ -n "$LEASE_KEEPER_LOG" ] && [ -r "$LEASE_KEEPER_LOG" ]; then
    keeper="$(tail -n 1 "$LEASE_KEEPER_LOG" 2>/dev/null || true)"
  fi
  temp="$(mktemp "$(dirname "$P0_EVIDENCE")/.evidence.XXXXXX")"
  jq -n --arg phase "$phase" --arg status "$status" --argjson rc "$rc" \
    --argjson duration "$duration" --arg lane "$P0_LANE" \
    --arg run "$LEASE_RUN" --arg slot "$LEASE_SLOT" \
    --argjson generation "$LEASE_GENERATION" --arg bundle "$P0_BUNDLE_HASH" \
    --argjson capacity "$capacity" --arg keeper "$keeper" \
    --arg failure_log "$P0_FAILURE_LOG" '
    {
      schema_version: 1,
      run: $run,
      requested_lane: $lane,
      allocation: {slot: $slot, resource_generation: $generation},
      bundle_hash: $bundle,
      last_phase: $phase,
      status: $status,
      exit_status: $rc,
      duration_seconds: $duration,
      capacity: $capacity,
      lease_keeper_last: $keeper,
      failure_log: (if $failure_log == "" then null else $failure_log end),
      resource_inventory: ["guest:vm1", "guest:vm2", "marker-owned-only"]
    }
  ' > "$temp"
  chmod 0600 "$temp"
  mv -f "$temp" "$P0_EVIDENCE"
  while [ "$(find "$(dirname "$P0_EVIDENCE")" -maxdepth 1 -type f -name 'p0-*.json' | wc -l)" -gt 20 ]; do
    oldest="$(find "$(dirname "$P0_EVIDENCE")" -maxdepth 1 -type f -name 'p0-*.json' -printf '%T@\t%p\n' | sort -n | head -n1 | cut -f2-)"
    [ -n "$oldest" ] || break
    find "${oldest%.json}.failure.log" -delete 2>/dev/null || true
    find "$oldest" -delete
  done
}

capacity_evidence_json() {
  local vm log values samples first last min_root min_memory last_memory failures first_unreachable
  local summary='{}'
  [ -n "$CAPACITY_LOG_DIR" ] && [ -d "$CAPACITY_LOG_DIR" ] \
    || { printf '{}\n'; return; }
  for vm in 1 2; do
    log="$CAPACITY_LOG_DIR/vm$vm.tsv"
    [ -r "$log" ] || continue
    values="$(awk -F '\t' '
      $2 == "unreachable" {
        failures++
        if (!first_unreachable) first_unreachable=$1
      }
      NF == 7 {
        samples++
        if (samples == 1) { first=$1; min_root=$3; min_memory=$7 }
        last=$1; last_memory=$7
        if ($3 < min_root) min_root=$3
        if ($7 < min_memory) min_memory=$7
      }
      END {
        print samples+0, first+0, last+0, min_root+0, min_memory+0, last_memory+0,
          failures+0, first_unreachable+0
      }
    ' "$log")"
    read -r samples first last min_root min_memory last_memory failures first_unreachable \
      <<<"$values"
    summary="$(jq -c --arg key "vm$vm" \
      --argjson samples "$samples" --argjson first "$first" --argjson last "$last" \
      --argjson min_root "$min_root" --argjson min_memory "$min_memory" \
      --argjson last_memory "$last_memory" --argjson failures "$failures" \
      --argjson first_unreachable "$first_unreachable" '
      . + {($key): {
        samples: $samples,
        first_unix: $first,
        last_unix: $last,
        min_root_available_bytes: $min_root,
        min_memory_available_bytes: $min_memory,
        last_memory_available_bytes: $last_memory,
        unreachable_samples: $failures,
        first_unreachable_unix: $first_unreachable
      }}
    ' <<<"$summary")"
  done
  printf '%s\n' "$summary"
}

collect_failure_diagnostics() { # <stage> <truncate|append>
  local stage="${1:?failure diagnostics stage is required}"
  local write_mode="${2:?failure diagnostics write mode is required}" vm log
  [ -n "$P0_EVIDENCE" ] || return 0
  P0_FAILURE_LOG="${P0_EVIDENCE%.json}.failure.log"
  umask 077
  case "$write_mode" in
    truncate) : > "$P0_FAILURE_LOG" ;;
    append) [ -e "$P0_FAILURE_LOG" ] || : > "$P0_FAILURE_LOG" ;;
    *) return 2 ;;
  esac
  {
    printf '\n== %s snapshot ==\n' "$stage"
    printf 'timestamp_utc=%s\nphase=%s\n' "$(date -u +%FT%TZ)" "$P0_CURRENT_PHASE"
    printf '\n== lease keeper ==\n'
    [ -z "$LEASE_KEEPER_LOG" ] || tail -n 20 "$LEASE_KEEPER_LOG" 2>/dev/null || true
    printf '\n== broker status (redacted facade) ==\n'
    facade_request status 2>&1 || true
    for vm in 1 2; do
      printf '\n== VM%s capacity tail ==\n' "$vm"
      log="${CAPACITY_LOG_DIR:+$CAPACITY_LOG_DIR/vm$vm.tsv}"
      [ -z "$log" ] || tail -n 20 "$log" 2>/dev/null || true
      printf '\n== VM%s reachability and kernel memory ==\n' "$vm"
      timeout --foreground 15 ssh -F "$CONFIG" -T -o ConnectTimeout=5 \
        "e2e-vm-$vm" -- '
          systemctl is-system-running 2>/dev/null || true
          free -b 2>/dev/null || true
          sudo -n journalctl -k -b --no-pager -o short-iso 2>/dev/null \
            | grep -iE "out of memory|oom-kill|killed process" | tail -n 30 || true
        ' 2>&1 || printf 'unreachable\n'
    done
  } >> "$P0_FAILURE_LOG"
  chmod 0600 "$P0_FAILURE_LOG"
}
run_phase() {
  local phase="$1" started duration; shift
  if [ "$P0_RESUME" = 1 ] && checkpoint_passed "$phase"; then
    printf '  [ ok ] phase=%s skipped from matching checkpoint bundle=%s\n' \
      "$phase" "$P0_BUNDLE_HASH"
    return 0
  fi
  P0_CURRENT_PHASE="$phase"
  P0_PHASE_STARTED=$SECONDS
  started=$SECONDS
  printf '  [ .. ] phase=%s bundle=%s\n' "$phase" "$P0_BUNDLE_HASH"
  "$@"
  duration=$((SECONDS - started))
  mark_checkpoint_passed "$phase"
  write_evidence "$phase" passed 0 "$duration"
  printf '  [ ok ] phase=%s duration=%ss\n' "$phase" "$duration"
}
stop_runner_children() {
  local pid
  for pid in "${P0_CHILD_PIDS[@]}"; do
    kill -0 "$pid" >/dev/null 2>&1 || continue
    pkill -TERM -P "$pid" >/dev/null 2>&1 || true
    kill -TERM "$pid" >/dev/null 2>&1 || true
  done
  for pid in "${P0_CHILD_PIDS[@]}"; do
    wait "$pid" >/dev/null 2>&1 || true
  done
  P0_CHILD_PIDS=()
}
p0_guest() {
  local vm="$1"; shift
  guest "$vm" runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev \
    sh -c 'cd "$HOME"; exec "$@"' _ "$@"
}
run_vm() {
  local vm="$1" mode="$2" rc=0; shift 2
  run_guest "$vm" "$P0_BUNDLE" "$P0_BUNDLE_HASH" \
    bash dev/e2e/p0-guest.sh "$mode" "$TOKEN" "$@" || rc=$?
  cleanup_guest "$vm" || return 3
  return "$rc"
}
direct_vm() {
  local vm="$1" mode="$2"; shift 2
  p0_guest "$vm" env SUBYARD_E2E_VM="$vm" \
    bash "/tmp/subyard-p0-peer-$TOKEN/src/dev/e2e/p0-guest.sh" "$mode" "$TOKEN" "$@"
}
run_source_vm() {
  local mode="$1" rc=0; shift
  run_guest 1 "$P0_BUNDLE" "$P0_BUNDLE_HASH" \
    bash dev/e2e/p0-source-upgrade.sh "$mode" "$TOKEN" "$@" || rc=$?
  cleanup_guest 1 || return 3
  return "$rc"
}
clean_peers() {
  local rc=0
  run_vm 1 peer-clean || rc=$?
  run_vm 2 peer-clean || rc=$?
  return "$rc"
}
clean_source_host() {
  run_source_vm clean
}
stop_capacity_monitors() {
  local vm pid
  for vm in 1 2; do
    [ -z "${CAPACITY_FLAG[$vm]:-}" ] || find "${CAPACITY_FLAG[$vm]}" -delete 2>/dev/null || true
  done
  for vm in 1 2; do
    pid="${CAPACITY_PID[$vm]:-}"
    [ -z "$pid" ] || wait "$pid" >/dev/null 2>&1 || true
    CAPACITY_PID[$vm]=''
  done
}
cleanup() {
  local rc=$? cleanup_failed=0 duration
  trap - EXIT INT TERM
  set +e
  if [ "$rc" -ne 0 ] && [ -n "$P0_EVIDENCE" ]; then
    collect_failure_diagnostics failure-entry truncate
  fi
  stop_runner_children
  stop_capacity_monitors
  if [ "$rc" -ne 0 ] && [ -n "$P0_EVIDENCE" ]; then
    duration=$((SECONDS - P0_PHASE_STARTED))
    collect_failure_diagnostics post-stop append
    write_evidence "$P0_CURRENT_PHASE" failed "$rc" "$duration"
    printf '  [fail] phase=%s status=%s duration=%ss evidence=%s diagnostics=%s\n' \
      "$P0_CURRENT_PHASE" "$rc" "$duration" "$P0_EVIDENCE" "$P0_FAILURE_LOG" >&2
  fi
  if [ -n "$PROBE_PID" ]; then
    kill -TERM -- "-$PROBE_PID" >/dev/null 2>&1
    wait "$PROBE_PID" >/dev/null 2>&1
  fi
  if [ -n "$PROBE_NAME" ]; then
    p0_guest 1 pkill -f "^$PROBE_NAME 300$" >/dev/null 2>&1 || true
  fi
  if [ -n "$PROBE_MARKER" ]; then
    p0_guest 1 find "$PROBE_MARKER" -delete >/dev/null 2>&1 || cleanup_failed=1
  fi
  [ "$PEERS_READY" = 0 ] || clean_peers >/dev/null 2>&1 || cleanup_failed=1
  [ "$SOURCE_HOST_STARTED" = 0 ] || clean_source_host >/dev/null 2>&1 || cleanup_failed=1
  if [ -n "$SOURCE_ARCHIVE_REMOTE" ]; then
    p0_guest 1 \
      sh -c '[ ! -e "$1" ] || find "$1" -delete' _ "$SOURCE_ARCHIVE_REMOTE" \
      >/dev/null 2>&1 || cleanup_failed=1
  fi
  [ -z "$SOURCE_ARCHIVE" ] || [ ! -e "$SOURCE_ARCHIVE" ] \
    || find "$SOURCE_ARCHIVE" -delete >/dev/null 2>&1 \
    || cleanup_failed=1
  [ -z "$PROBE_LOG" ] || find "$PROBE_LOG" -delete >/dev/null 2>&1 || cleanup_failed=1
  [ -z "$CAPACITY_LOG_DIR" ] || find "$CAPACITY_LOG_DIR" -depth -delete \
    >/dev/null 2>&1 || cleanup_failed=1
  if [ -n "$LEASE_KEEPER_PID" ]; then
    kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
    wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
    LEASE_KEEPER_PID=''
  fi
  release_lease >/dev/null 2>&1 || cleanup_failed=1
  if [ -n "$LOCAL_TEMP" ]; then
    case "$LOCAL_TEMP" in /tmp/subyard-agent-e2e.*|"${TMPDIR:-/tmp}"/subyard-agent-e2e.*)
      find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_failed=1
      ;;
    esac
  fi
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}

capacity_cache_snapshot() {
  local vm="$1"
  p0_guest "$vm" bash -c '
    bytes() {
      if [ -e "$1" ]; then du -sx -B1 "$1" | awk "{print \$1}"; else printf "0\n"; fi
    }
    build="$(env -u GOCACHE go env GOCACHE)"
    modules="$(env -u GOMODCACHE go env GOMODCACHE)"
    printf "%s\t%s\n" "$(bytes "$build")" "$(bytes "$modules")"
  '
}

capacity_monitor() {
  local vm="$1" flag="$2" log="$3" sample capacity_sample_command
  local interval="${P0_E2E_CAPACITY_SAMPLE_SECONDS:-5}"
  [[ "$interval" =~ ^[1-9][0-9]*$ ]] || interval=5
  capacity_sample_command="$(quote_ssh_command bash -c '
    read -r root_used root_available <<EOF
$(df -B1 --output=used,avail / | awk "NR==2 {print \$1, \$2}")
EOF
    inode_used="$(df --output=iused / | awk "NR==2 {print \$1}")"
    tmp_used="$(df -B1 --output=used /tmp | awk "NR==2 {print \$1}")"
    read -r memory_used memory_available <<EOF
$(awk "
  /MemTotal:/ { total=\$2 }
  /MemAvailable:/ { available=\$2 }
  END { print (total-available)*1024, available*1024 }
" /proc/meminfo)
EOF
    printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n" \
      "$(date +%s)" "$root_used" "$root_available" "$inode_used" "$tmp_used" \
      "$memory_used" "$memory_available"
  ')"
  while [ -e "$flag" ]; do
    if sample="$(timeout --foreground 15 ssh -F "$CONFIG" -T -o ConnectTimeout=3 \
      "e2e-vm-$vm" -- "$capacity_sample_command" 2>/dev/null)"; then
      printf '%s\n' "$sample" >> "$log"
    else
      printf '%s\tunreachable\n' "$(date +%s)" >> "$log"
    fi
    sleep "$interval"
  done
}

start_capacity_monitors() {
  local vm flag log
  CAPACITY_LOG_DIR="$(mktemp -d /tmp/subyard-p0-capacity.XXXXXX)"
  for vm in 1 2; do
    flag="$CAPACITY_LOG_DIR/vm$vm.running"
    log="$CAPACITY_LOG_DIR/vm$vm.tsv"
    : > "$flag"
    : > "$log"
    CAPACITY_FLAG[$vm]="$flag"
    capacity_monitor "$vm" "$flag" "$log" &
    CAPACITY_PID[$vm]=$!
  done
}

capacity_report() {
  local vm log report root_used root_available inode_used tmp_used memory_used memory_available
  local min_root_available="${P0_E2E_MIN_PEAK_ROOT_RESERVE_BYTES:-1073741824}"
  local min_memory_available="${P0_E2E_MIN_PEAK_MEMORY_RESERVE_BYTES:-268435456}"
  stop_capacity_monitors
  for vm in 1 2; do
    log="$CAPACITY_LOG_DIR/vm$vm.tsv"
    report="$(awk -F '\t' '
      NF == 7 {
        if (!seen || $2 > root_used) root_used=$2
        if (!seen || $3 < root_available) root_available=$3
        if (!seen || $4 > inode_used) inode_used=$4
        if (!seen || $5 > tmp_used) tmp_used=$5
        if (!seen || $6 > memory_used) memory_used=$6
        if (!seen || $7 < memory_available) memory_available=$7
        seen=1
      }
      END {
        if (seen) print root_used, root_available, inode_used, tmp_used, memory_used, memory_available
      }
    ' "$log")"
    [ -n "$report" ] || die "VM$vm capacity monitor recorded no samples"
    read -r root_used root_available inode_used tmp_used memory_used memory_available <<<"$report"
    [ "$root_available" -ge "$min_root_available" ] \
      || die "VM$vm peak root reserve fell below $min_root_available bytes: $root_available"
    [ "$memory_available" -ge "$min_memory_available" ] \
      || die "VM$vm peak memory reserve fell below $min_memory_available bytes: $memory_available"
    printf '  [ ok ] VM%s measured peak root_used=%s root_reserve=%s inode_used=%s tmp_used=%s memory_used=%s memory_reserve=%s\n' \
      "$vm" "$root_used" "$root_available" "$inode_used" "$tmp_used" \
      "$memory_used" "$memory_available"
  done
}

assert_capacity_transport_stable() {
  local vm log first_unreachable
  for vm in 1 2; do
    log="$CAPACITY_LOG_DIR/vm$vm.tsv"
    [ -r "$log" ] || die "VM$vm capacity monitor log is missing"
    first_unreachable="$(awk -F '\t' '$2 == "unreachable" { print $1; exit }' "$log")"
    [ -z "$first_unreachable" ] \
      || die "VM$vm became unreachable during nested teardown at unix=$first_unreachable"
  done
}

targeted_capacity_report() {
  assert_capacity_transport_stable
  capacity_report
}

verify_cache_lifecycle() {
  local vm after default_after module_after growth max_growth=33554432
  for vm in 1 2; do
    after="$(capacity_cache_snapshot "$vm")"
    IFS=$'\t' read -r default_after module_after <<<"$after"
    growth=$((default_after - DEFAULT_BUILD_CACHE_BEFORE[$vm]))
    [ "$growth" -le "$max_growth" ] \
      || die "VM$vm shared Go build cache grew by $growth bytes; P0 must use its disposable cache"
    printf '  [ ok ] VM%s Go cache lifecycle default_build=%s->%s reusable_modules=%s->%s\n' \
      "$vm" "${DEFAULT_BUILD_CACHE_BEFORE[$vm]}" "$default_after" \
      "${MODULE_CACHE_BEFORE[$vm]}" "$module_after"
  done
}

prepare_source_archive() {
  local revision commit hash remote_hash
  revision="${SUBYARD_P0_SOURCE_REVISION:-7c67ee3}"
  commit="$(git -C "$ROOT" rev-parse --verify "$revision^{commit}")" \
    || die "source revision $revision is unavailable"
  SOURCE_ARCHIVE="$(mktemp /tmp/subyard-p0-source.XXXXXX.tar.gz)"
  git -C "$ROOT" archive --format=tar "$commit" | gzip -n > "$SOURCE_ARCHIVE"
  hash="$(sha256sum "$SOURCE_ARCHIVE" | cut -d' ' -f1)"
  SOURCE_ARCHIVE_REMOTE="/tmp/subyard-p0-source-$TOKEN.tar.gz"
  p0_guest 1 \
    sh -c 'umask 077; dd of="$1" status=none' _ "$SOURCE_ARCHIVE_REMOTE" \
    < "$SOURCE_ARCHIVE"
  remote_hash="$(ssh -F "$CONFIG" -T e2e-vm-1 -- \
    sha256sum "$SOURCE_ARCHIVE_REMOTE" | awk '{print $1}')"
  [ "$remote_hash" = "$hash" ] || die 'source archive changed in transport'
  SOURCE_HASH="$hash"
  SOURCE_COMMIT="$commit"
}

reboot_vm1() {
  local before_boot after_boot='' down=0 host_state up=0 unit_result='' route
  before_boot="$(ssh -F "$CONFIG" -T e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id)" \
    || die 'cannot read VM1 boot ID before reboot'
  set +e
  ssh -F "$CONFIG" -T e2e-vm-1 -- sudo -n systemctl reboot >/dev/null 2>&1
  set -e
  for _ in $(seq 1 60); do
    if ! ssh -F "$CONFIG" -T -o ConnectTimeout=2 e2e-vm-1 -- true \
      >/dev/null 2>&1; then
      down=1
      break
    fi
    sleep 1
  done
  [ "$down" = 1 ] || die 'VM1 did not go down for reboot'
  for _ in $(seq 1 180); do
    after_boot="$(ssh -F "$CONFIG" -T -o ConnectTimeout=3 e2e-vm-1 -- \
      cat /proc/sys/kernel/random/boot_id 2>/dev/null)" || after_boot=''
    if [ -n "$after_boot" ] && [ "$after_boot" != "$before_boot" ]; then
      up=1
      break
    fi
    sleep 1
  done
  [ "$up" = 1 ] || die 'VM1 did not return with a new boot ID'
  set +e
  host_state="$(ssh -F "$CONFIG" -T e2e-vm-1 -- \
    timeout 180 systemctl is-system-running --wait 2>/dev/null)"
  set -e
  case "$host_state" in
    running | degraded) ;;
    *) die "VM1 boot did not reach a terminal systemd state: ${host_state:-unknown}" ;;
  esac
  for _ in $(seq 1 180); do
    unit_result="$(ssh -F "$CONFIG" -T e2e-vm-1 -- \
      systemctl show subyard-power-reconcile.service --property=Result --value)"
    [ "$unit_result" != success ] || break
    sleep 1
  done
  if [ "$unit_result" != success ]; then
    ssh -F "$CONFIG" -T e2e-vm-1 -- \
      systemctl show subyard-power-reconcile.service \
        --property=LoadState --property=ActiveState --property=SubState \
        --property=Result --property=NRestarts >&2 || true
    ssh -F "$CONFIG" -T e2e-vm-1 -- \
      journalctl -b -u subyard-power-reconcile.service --no-pager -n 120 >&2 || true
    die "VM1 boot power reconciliation failed: ${unit_result:-unknown}"
  fi
  route="$(ssh -F "$CONFIG" -T e2e-vm-1 -- ip -4 route show default)"
  [ -n "$route" ] || die 'VM1 lost its default route after reboot'
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

assert_no_worktrees() {
  local vm leftover
  for vm in 1 2; do
    leftover="$(ssh -F "$CONFIG" -T "e2e-vm-$vm" -- \
      find /tmp -maxdepth 1 -type d -name 'subyard-worktree.*' -print -quit)"
    [ -z "$leftover" ] || die "VM$vm retained an agent worktree"
  done
}

yard_entry_state() {
  local vm="$1"
  p0_guest "$vm" sh -c '
    path="$HOME/.local/bin/yard"
    if [ -L "$path" ]; then
      printf "link\t%s\n" "$(readlink "$path")"
    elif [ -f "$path" ]; then
      printf "file\t%s\t%s\n" \
        "$(stat -c "%a:%u:%g" "$path")" "$(sha256sum "$path" | cut -d " " -f1)"
    elif [ -e "$path" ]; then
      printf "other\t%s\n" "$(stat -c "%f:%u:%g" "$path")"
    else
      printf "absent\n"
    fi
  '
}

ssh_state() {
  local vm="$1"
  p0_guest "$vm" sh -c '
    for path in "$HOME/.ssh/authorized_keys" "$HOME/.ssh/config"; do
      if [ -L "$path" ]; then
        printf "link\t%s\t%s\n" "$path" "$(readlink "$path")"
      elif [ -f "$path" ]; then
        printf "file\t%s\t%s\t%s\n" "$path" \
          "$(stat -c "%a:%u:%g" "$path")" "$(sha256sum "$path" | cut -d " " -f1)"
      elif [ -e "$path" ]; then
        printf "other\t%s\t%s\n" "$path" "$(stat -c "%f:%u:%g" "$path")"
      else
        printf "absent\t%s\n" "$path"
      fi
    done
  '
}

home_state() {
  local vm="$1"
  p0_guest "$vm" sh -c \
    'stat -c "%a:%u:%g" "$HOME"'
}

transport_probes() {
  local rc=0 ready=0 stopped=0 disconnect_command
  set +e
  run_guest 1 "$P0_BUNDLE" "$P0_BUNDLE_HASH" bash -c \
    'test "$1" = "argument with spaces" && test "$2" = "$SUBYARD_E2E_VM"; exit 23' \
    _ 'argument with spaces' 1
  rc=$?
  set -e
  cleanup_guest 1 || die 'failed transport probe left its worktree behind'
  [ "$rc" = 23 ] || die "failed guest command returned $rc instead of 23"
  assert_no_worktrees

  command -v setsid >/dev/null 2>&1 || die 'setsid is required'
  PROBE_MARKER="/tmp/subyard-p0-disconnect-$TOKEN.ready"
  PROBE_NAME="subyard-p0-disconnect-$TOKEN"
  PROBE_LOG="$(mktemp /tmp/subyard-p0-disconnect.XXXXXX)"
  disconnect_command="$(quote_ssh_command runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev \
    bash -c \
    'printf "ready\n" > "$1"; exec -a "$2" sleep 300' \
    _ "$PROBE_MARKER" "$PROBE_NAME")"
  setsid ssh -F "$CONFIG" -T e2e-vm-1 -- "$disconnect_command" >"$PROBE_LOG" 2>&1 &
  PROBE_PID=$!
  for _ in $(seq 1 60); do
    if ssh -F "$CONFIG" -T e2e-vm-1 -- test -f "$PROBE_MARKER"; then ready=1; break; fi
    sleep 1
  done
  [ "$ready" = 1 ] || die 'disconnect probe did not start'
  kill -TERM -- "-$PROBE_PID"
  set +e
  wait "$PROBE_PID"
  rc=$?
  set -e
  PROBE_PID=''
  [ "$rc" -ne 0 ] || die 'interrupted runner returned success'
  for _ in $(seq 1 20); do
    if ! ssh -F "$CONFIG" -T e2e-vm-1 -- pgrep -f "^$PROBE_NAME 300$" >/dev/null 2>&1; then
      stopped=1
      break
    fi
    sleep 1
  done
  [ "$stopped" = 1 ] || die 'guest process survived controller disconnect'
  ssh -F "$CONFIG" -T e2e-vm-1 -- find "$PROBE_MARKER" -delete
  PROBE_MARKER=''
  PROBE_NAME=''
  find "$PROBE_LOG" -delete
  PROBE_LOG=''
  assert_no_worktrees
}

run_lanes() {
  local owner_pid controller_pid owner_rc controller_rc
  run_vm 1 owner & owner_pid=$!
  run_vm 2 controller & controller_pid=$!
  P0_CHILD_PIDS=("$owner_pid" "$controller_pid")
  set +e
  wait "$owner_pid"; owner_rc=$?
  wait "$controller_pid"; controller_rc=$?
  set -e
  P0_CHILD_PIDS=()
  [ "$owner_rc" != 3 ] && [ "$controller_rc" != 3 ] || return 3
  [ "$owner_rc" != 2 ] && [ "$controller_rc" != 2 ] || return 2
  [ "$owner_rc" = 0 ] && [ "$controller_rc" = 0 ] || return 1
}

preflight_lane() {
  local vm
  for vm in 1 2; do
    run_vm "$vm" capacity-preflight
    run_vm "$vm" dependency-verify
  done
}

dependency_lane() {
  run_vm 1 capacity-preflight
  run_vm 1 dependency-bootstrap
  run_vm 2 dependency-verify
}

nested_teardown_lane() {
  # This is a host-boundary invariant, not a role-specific one. Duplicating a memory-intensive
  # nested QEMU fixture in one lease adds no coverage and increases cumulative resident-pressure
  # risk. Exercise one explicitly selectable VM and keep both under the capacity/transport canary.
  run_vm "$P0_NESTED_VM" nested-teardown
}

source_upgrade_lane() {
  prepare_source_archive
  SOURCE_HOST_STARTED=1
  run_source_vm prepare "$SOURCE_ARCHIVE_REMOTE" "$SOURCE_HASH" "$SOURCE_COMMIT"
  p0_guest 1 sh -c '[ ! -e "$1" ] || find "$1" -delete' _ "$SOURCE_ARCHIVE_REMOTE"
  SOURCE_ARCHIVE_REMOTE=''
  find "$SOURCE_ARCHIVE" -delete
  SOURCE_ARCHIVE=''
  reboot_vm1
  run_source_vm resume
  reboot_vm1
  run_source_vm finish
  SOURCE_HOST_STARTED=0
}

reboot_verify_lane() {
  reboot_vm1
  reboot_vm1
}

peer_lane() {
  local peer1_info peer2_info peer1_key peer2_key vm1_host_key vm2_host_key vm
  VM1_YARD_ENTRY="$(yard_entry_state 1)"
  VM2_YARD_ENTRY="$(yard_entry_state 2)"
  VM1_SSH_STATE="$(ssh_state 1)"
  VM2_SSH_STATE="$(ssh_state 2)"
  PEERS_READY=1
  run_vm 1 peer-prepare "$vm2_ip"
  run_vm 2 peer-prepare "$vm1_ip"
  peer1_info="$(direct_vm 1 peer-info)"
  peer2_info="$(direct_vm 2 peer-info)"
  peer1_key="$(awk -F '\t' '$1=="identity" {print $2; exit}' <<<"$peer1_info")"
  peer2_key="$(awk -F '\t' '$1=="identity" {print $2; exit}' <<<"$peer2_info")"
  vm1_host_key="$(awk -F '\t' '$1=="host" {print $2; exit}' <<<"$peer1_info")"
  vm2_host_key="$(awk -F '\t' '$1=="host" {print $2; exit}' <<<"$peer2_info")"
  [ -n "$peer1_key" ] && [ -n "$peer2_key" ] \
    && [ -n "$vm1_host_key" ] && [ -n "$vm2_host_key" ] \
    || die 'cross-owner synthetic SSH evidence is incomplete'
  printf '  [ .. ] installing synthetic cross-owner SSH identities and host-key pins\n'
  direct_vm 1 peer-authorize "$vm2_ip" "$peer2_key" "$vm2_host_key"
  direct_vm 2 peer-authorize "$vm1_ip" "$peer1_key" "$vm1_host_key"
  direct_vm 1 peer-probe "$vm2_ip"
  direct_vm 2 peer-probe "$vm1_ip"
  direct_vm 2 peer-yard-start
  direct_vm 1 peer-projects "$vm2_ip"
  direct_vm 2 peer-deny
  direct_vm 1 peer-projects-offline "$vm2_ip"
  direct_vm 2 peer-allow
  direct_vm 1 peer-projects-finish "$vm2_ip"
  direct_vm 1 peer-rpc "$vm2_ip"
  direct_vm 2 peer-rpc "$vm1_ip"
  direct_vm 1 peer-credentials "$vm2_ip"
  clean_peers
  PEERS_READY=0

  for vm in 1 2; do
    ssh -F "$CONFIG" -T "e2e-vm-$vm" -- test ! -e "/tmp/subyard-p0-peer-$TOKEN" \
      || die "VM$vm retained its peer fixture"
    p0_guest "$vm" \
      sh -c '! grep -Fq "$1" "$HOME/.ssh/authorized_keys" 2>/dev/null' _ "subyard-p0-$TOKEN" \
      || die "VM$vm retained a synthetic peer authorization"
  done
  [ "$(yard_entry_state 1)" = "$VM1_YARD_ENTRY" ] \
    || die 'VM1 user yard entry was not restored exactly'
  [ "$(yard_entry_state 2)" = "$VM2_YARD_ENTRY" ] \
    || die 'VM2 user yard entry was not restored exactly'
  [ "$(ssh_state 1)" = "$VM1_SSH_STATE" ] \
    || die 'VM1 SSH state was not restored exactly'
  [ "$(ssh_state 2)" = "$VM2_SSH_STATE" ] \
    || die 'VM2 SSH state was not restored exactly'
  [ "$(public_tree_hash | sha256sum | awk '{print $1}')" = "$CANDIDATE_HASH" ] \
    || die 'public candidate changed during acceptance'
  assert_no_worktrees
}

cleanup_lane() {
  local vm
  clean_peers
  clean_source_host
  for vm in 1 2; do
    run_vm "$vm" capacity-verify-cleanup
  done
  assert_no_worktrees
}

if [ -n "${SUBYARD_P0_SLOT:-}" ]; then
  set_requested_slot "$SUBYARD_P0_SLOT" SUBYARD_P0_SLOT
fi
LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
LEASE_PURPOSE="p0-$P0_LANE"
[ "$P0_LANE" != full ] || LEASE_PURPOSE=p0-acceptance
[ "$BROKER_RECOVERY_ONLY" = 0 ] || LEASE_PURPOSE=p0-broker-recovery
acquire_lease
start_lease_keeper
CONFIG="$CLIENT_CONFIG"
[ -r "$CONFIG" ] || die 'lease-local SSH config is unavailable'
P0_BUNDLE="$LOCAL_TEMP/worktree.tar.gz"
build_bundle "$ROOT" "$P0_BUNDLE"
P0_BUNDLE_HASH="$(sha256sum "$P0_BUNDLE" | awk '{print $1}')"
CANDIDATE_HASH="$(public_tree_hash | sha256sum | awk '{print $1}')"
[[ "$CANDIDATE_HASH" =~ ^[0-9a-f]{64}$ ]] || die 'candidate tree hash is invalid'
printf '  [ .. ] exact public candidate sha256=%s bundle=%s\n' \
  "$CANDIDATE_HASH" "$P0_BUNDLE_HASH"
# P0 fixtures use the token in local Unix account names and therefore retain their bounded
# numeric-token contract. Derive it from the public run identity and lease epoch, never credentials.
TOKEN="$((16#$LEASE_RUN))${LEASE_EPOCH}"
[[ "$TOKEN" =~ ^[0-9]+$ ]] || die 'lease token is invalid'
prepare_run_records

if [ "$BROKER_RECOVERY_ONLY" = 1 ]; then
  HOME_STATE_BEFORE[1]="$(home_state 1)"
  run_phase capacity-preflight run_vm 1 capacity-preflight
  run_phase broker-recovery run_vm 1 broker-recovery-owner
  run_phase cleanup run_vm 1 capacity-verify-cleanup
  [ "$(home_state 1)" = "${HOME_STATE_BEFORE[1]}" ] \
    || die 'VM1 operator home permissions or ownership changed'
  assert_no_worktrees
  printf 'ok: P0 broker logging and quarantine rebuild acceptance passed\n'
  exit 0
fi

vm1_ip="$(ssh -F "$CONFIG" -G e2e-vm-1 | awk '$1=="hostname" {print $2; exit}')"
vm2_ip="$(ssh -F "$CONFIG" -G e2e-vm-2 | awk '$1=="hostname" {print $2; exit}')"

case "$P0_LANE" in
  boundary) run_phase boundary verify_boundary ;;
  nested-teardown)
    run_phase capacity-preflight preflight_lane
    start_capacity_monitors
    run_phase nested-teardown nested_teardown_lane
    run_phase cleanup cleanup_lane
    run_phase capacity-report targeted_capacity_report
    find "$CAPACITY_LOG_DIR" -depth -delete
    CAPACITY_LOG_DIR=''
    ;;
  transport) run_phase transport transport_probes ;;
  dependencies) run_phase dependencies dependency_lane ;;
  real-incus)
    run_phase capacity-preflight run_vm 1 capacity-preflight
    run_phase real-incus run_vm 1 real-incus
    run_phase cleanup run_vm 1 capacity-verify-cleanup
    ;;
  profile-resource)
    run_phase profile-resource run_vm 1 profile-resource
    run_phase cleanup run_vm 1 capacity-verify-cleanup
    ;;
  release)
    run_phase capacity-preflight preflight_lane
    run_phase release run_lanes
    run_phase cleanup cleanup_lane
    ;;
  source-upgrade)
    run_phase capacity-preflight run_vm 1 capacity-preflight
    run_phase source-upgrade source_upgrade_lane
    run_phase cleanup cleanup_lane
    ;;
  reboot-verify) run_phase reboot-verify reboot_verify_lane ;;
  peer)
    run_phase capacity-preflight preflight_lane
    run_phase peer peer_lane
    run_phase cleanup cleanup_lane
    ;;
  peer-cleanup) run_phase peer-cleanup cleanup_lane ;;
  cleanup) run_phase cleanup cleanup_lane ;;
  full)
    for vm in 1 2; do
      snapshot="$(capacity_cache_snapshot "$vm")"
      IFS=$'\t' read -r DEFAULT_BUILD_CACHE_BEFORE[$vm] MODULE_CACHE_BEFORE[$vm] <<<"$snapshot"
      HOME_STATE_BEFORE[$vm]="$(home_state "$vm")"
    done
    run_phase capacity-preflight preflight_lane
    start_capacity_monitors
    run_phase boundary verify_boundary
    run_phase nested-teardown nested_teardown_lane
    run_phase transport transport_probes
    run_phase release run_lanes
    run_phase source-upgrade source_upgrade_lane
    run_phase peer peer_lane
    run_phase cleanup cleanup_lane
    P0_CURRENT_PHASE=final-verify
    P0_PHASE_STARTED=$SECONDS
    for vm in 1 2; do
      [ "$(home_state "$vm")" = "${HOME_STATE_BEFORE[$vm]}" ] \
        || die "VM$vm operator home permissions or ownership changed"
    done
    verify_cache_lifecycle
    capacity_report
    verify_boundary
    find "$CAPACITY_LOG_DIR" -depth -delete
    CAPACITY_LOG_DIR=''
    ;;
esac

if [ "$P0_LANE" = full ] && [ "$P0_RESUME" = 0 ]; then
  printf 'ok: continuous P0 two-VM release gate passed within one broker lease\n'
else
  printf 'ok: targeted P0 lane %s passed; continuous fresh-install P0 is still required\n' "$P0_LANE"
fi

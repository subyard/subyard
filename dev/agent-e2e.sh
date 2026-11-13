#!/usr/bin/env bash
# Developer checks on disposable typed environments via restricted L1 SSH.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"
# shellcheck source=scripts/lib/runtime.sh
. "$REPO_ROOT/scripts/lib/runtime.sh"
E2E_PROGRAM=agent-e2e
# The shared broker client owns the route, facade, lease, keeper and guest transport.
# This runner adds worktree copy, guest run directories, lanes and phase evidence.
# shellcheck source=config/profiles/test-vms-client/skills/subyard-test-vms/client.sh
. "$REPO_ROOT/config/profiles/test-vms-client/skills/subyard-test-vms/client.sh"

PREPARED_DIRECTORY=""
E2E_ACTIVE_PHASE=""
E2E_PHASE_STARTED=0
E2E_PHASE_VM=""
declare -A GUEST_DIRS=()
lease_attribution() { resolve_workspace_attribution "$REPO_ROOT"; }
# Controller-only timings contain fixed fields, never command or lease data.
phase_marker() {
  local phase="$1" state="$2" duration="$3" rc="$4" vm="${5:-}"
  case "$phase" in allocation|packing|transport|guest|guest-cleanup|cleanup/release) ;; *) return 0 ;; esac
  case "$state" in start|end) ;; *) return 0 ;; esac
  [[ "$duration" =~ ^[0-9]+$ && "$rc" =~ ^[0-9]+$ ]] || return 0
  case "$vm" in ''|1|2) ;; *) return 0 ;; esac
  { printf 'E2E_PHASE phase=%s state=%s duration_seconds=%s exit_code=%s' "$phase" "$state" "$duration" "$rc"
    [ -z "$vm" ] || printf ' vm=%s' "$vm"
    printf '\n'
  } >&2 || true
}

phase_start() {
  E2E_ACTIVE_PHASE="$1"
  E2E_PHASE_STARTED=$SECONDS
  E2E_PHASE_VM="${2:-}"
  phase_marker "$E2E_ACTIVE_PHASE" start 0 0 "$E2E_PHASE_VM"
}

phase_end() {
  local rc="$1" duration=$((SECONDS - E2E_PHASE_STARTED))
  [ "$duration" -ge 0 ] || duration=0
  [ -z "$E2E_ACTIVE_PHASE" ] || phase_marker "$E2E_ACTIVE_PHASE" end "$duration" "$rc" "$E2E_PHASE_VM"
  E2E_ACTIVE_PHASE=""
}

usage() {
  cat <<'EOF'
Usage:
  dev/agent-e2e.sh [--owner ID] [--pool ID] --prepare
  dev/agent-e2e.sh [--owner ID] [--pool ID] --status [--json]
  dev/agent-e2e.sh [--owner ID] [--pool ID] --logs [--slot N] [-n 1..200] [--json]
  dev/agent-e2e.sh [--owner ID] [--pool ID] --observe CURSOR|-
  dev/agent-e2e.sh [--owner ID] [--pool ID] --incident ID
  dev/agent-e2e.sh [--owner ID] [--pool ID] --slot N [--wait DURATION] [--purpose LABEL] [--type subyard-pair|android-test] [--vm-count 1|2] [--disk 50GiB] [--vm 1|2|both] -- COMMAND [ARG...]
  dev/agent-e2e.sh [--owner ID] [--pool ID] --slot N [--purpose LABEL] [--vm-count 1|2] [--disk 50GiB] --ssh 1|2 [-- COMMAND [ARG...]]
  dev/agent-e2e.sh [--owner ID] [--pool ID] --slot N [--purpose LABEL] [--vm-count 1|2] [--disk 50GiB] --ssh-stdin 1|2 -- COMMAND [ARG...]
  dev/agent-e2e.sh [--owner ID] [--pool ID] --slot N [--vm-count 1|2] [--disk 50GiB] --verify-boundary

The normal form copies the current tracked, dirty and non-ignored public worktree to each selected
VM, runs COMMAND as dev, streams output and removes every run directory. A direct ./bin/yard command
first builds the explicit development candidate in that run directory. --ssh opens an ordinary guest
terminal or runs a command with stdin closed. --ssh-stdin explicitly forwards stdin to a command.
The default resolves the trusted owner service publication independently of the work yard.
Use --owner ID and --pool ID for an exact service selection; ambiguity never selects the first pool.
--yard NAME explicitly selects a pre-registration route for operator-controlled rollout only.
Controller and lease state are isolated by owner, pool and registration generation.

--prepare creates or verifies the shared controller identity at ~/.subyard/e2e/id_ed25519. P1
accepts standard controller keys through the bounded forced-command facade. Every run creates a
separate ephemeral guest key.
The wrapper renews every five minutes while it and its original launching process remain alive.
Held leases expire twenty minutes after the last successful renewal or the initial held grant.

The operator owns the outer test yard. Acquire creates disposable VMs from an immutable base;
release fences access and deletes their disks. --type defaults to subyard-pair (two VMs);
--vm-count 1 requests one standard VM instead. android-test supports only one larger VM.
--disk 50GiB requests a 50 GiB disk per VM; omission uses the configured default.
--vm selects the payload destination without changing the allocation count. The default VM
selection includes every requested VM. e2e-vm-1 and e2e-vm-2 are
lease-relative selectors, not physical slot names. Every invocation acquires a new lease; use one
script or one interactive SSH session when several steps must share mutable guest state. Every
lease-taking mode requires --slot N, atomically requests that broker slot and never falls back.
--logs reads only this controller's lease events, including failed acquisition and released leases.
--incident reads the diagnostic artifact referenced by an event. Neither command acquires a lease.
The private client-id authorizes reads; keep it private. Broker diagnostics are retained for up to
30 days, subject to a 512 MiB archive cap. Older brokers need an operator runtime update first.
EOF
}

telemetry_correlation() {
  local phase="$1" helper=/usr/local/lib/subyard-telemetry/broker-correlation.py
  [ -n "${SUBYARD_AGENT_RUN_ID:-}" ] && [ -n "${SUBYARD_TELEMETRY_DATA_DIR:-}" ] || return 0
  [ -f "$helper" ] || helper="$REPO_ROOT/config/profiles/telemetry/broker-correlation.py"
  [ -f "$helper" ] || return 0
  jq -cn --arg phase "$phase" --arg owner "$E2E_YARD" --arg yard "$LEASE_YARD" \
    --arg project "$LEASE_PROJECT" --arg run "$LEASE_RUN" --arg slot "$LEASE_SLOT" \
    --arg generation "$LEASE_GENERATION" --arg epoch "$LEASE_EPOCH" \
    --arg script "$REPO_ROOT/dev/agent-e2e.sh" --arg state "$STATE_BASE" --arg yard_state "$STATE_ROOT" \
    --arg identity "$IDENTITY" --arg route "$BASTION_ROUTE" \
    --arg route_config "${SUBYARD_E2E_ROUTE_CONFIG:-$HOME/.ssh/config}" \
    '{kind:"vm",phase:$phase,owner_id:$owner,yard_id:$yard,project_id:$project,correlation_id:$run,
      slot_id:$slot,resource_generation:$generation,lease_epoch:$epoch,transport:{script:$script,yard:$owner,state_dir:$state,yard_state_dir:$yard_state,identity:$identity,route:$route,route_config:$route_config}}' \
    | timeout 1 python3 -B "$helper" >/dev/null 2>&1 || true
}

resolve_workspace_attribution() {
  local root="$1" canonical workspace_root relative project_dir metadata project yard
  if [ -n "${SUBYARD_E2E_CONTROLLER_WORKSPACE:-}" ]; then
    root="$SUBYARD_E2E_CONTROLLER_WORKSPACE"
  fi
  canonical="$(realpath -e -- "$root")" \
    || die "cannot canonicalize the E2E worktree"
  workspace_root=/srv/workspaces
  if [ "${SUBYARD_E2E_TEST_MODE:-}" = 1 ] &&
    [ -n "${SUBYARD_E2E_WORKSPACES_ROOT:-}" ]; then
    workspace_root="$(realpath -e -- "$SUBYARD_E2E_WORKSPACES_ROOT")" \
      || die "cannot canonicalize the test workspace root"
  fi
  relative="${canonical#"$workspace_root"/}"
  [ "$relative" != "$canonical" ] && [[ "$relative" != */*/* ]] &&
    [[ "$relative" == */src ]] \
    || die "E2E runner must execute from /srv/workspaces/<ProjectID>/src"
  project_dir="${relative%/src}"
  safe_project_name "$project_dir" \
    || die "managed workspace has an unsafe project ID"
  [ "$canonical" = "$workspace_root/$project_dir/src" ] \
    || die "managed workspace path does not match its canonical project ID"
  metadata="$workspace_root/$project_dir/.subyard-meta.json"
  if [ -e "$metadata" ]; then
    [ -f "$metadata" ] && [ ! -L "$metadata" ] \
      || die "project metadata must be a regular non-symlink file"
    [ "$(stat -c '%s' "$metadata")" -le 65536 ] \
      || die "project metadata exceeds the size limit"
    jq -e --arg id "$project_dir" '
      .schema == 1 and .projectId == $id and
      (.name | type == "string" and length >= 1 and length <= 50 and
        test("^[A-Za-z0-9._-]+$") and . != "." and . != ".." and
        (startswith("-") | not)) and
      ((.yard // "unknown") | type == "string")
    ' "$metadata" >/dev/null 2>&1 \
      || die "project metadata is malformed or mismatched"
    project="$(jq -r '.name' "$metadata")"
    yard="$(jq -r '.yard // "unknown"' "$metadata")"
    safe_project_name "$project" || die "project metadata has an unsafe canonical name"
    safe_attribution_yard "$yard" || die "project metadata has an unsafe yard"
  else
    project="$project_dir"
    yard=unknown
  fi
  if [ "${SUBYARD_E2E_TEST_MODE:-}" = 1 ] &&
    [ -n "${SUBYARD_E2E_PROJECT_LABEL:-}" ]; then
    project="$SUBYARD_E2E_PROJECT_LABEL"
    safe_project_name "$project" || die "test project attribution is unsafe"
  fi
  if [ "${SUBYARD_E2E_TEST_MODE:-}" = 1 ] &&
    [ -n "${SUBYARD_E2E_PROJECT_YARD:-}" ]; then
    yard="$SUBYARD_E2E_PROJECT_YARD"
    safe_attribution_yard "$yard" || die "test yard attribution is unsafe"
  fi
  printf '%s\t%s\n' "$yard" "$project"
}

derive_purpose() {
  local mode="$1" explicit="$2" argument candidate=''; shift 2
  if [ -n "$explicit" ]; then
    bounded_context_label "$explicit" 80
    return
  fi
  case "$mode" in
    verify) candidate=verify-boundary ;;
    ssh)
      if [ "$#" -eq 0 ]; then
        candidate=ssh
      fi
      ;;
  esac
  if [ -z "$candidate" ]; then
    for argument in "$@"; do
      case "$argument" in
        *.sh | */*)
          candidate="${argument##*/}"
          candidate="${candidate%.sh}"
          break
          ;;
      esac
    done
  fi
  if [ -z "$candidate" ] && [ "${1:-}" = go ] && [ "${2:-}" = test ]; then
    candidate=go-test
  fi
  [ -n "$candidate" ] || candidate="${1:-agent-e2e}"
  bounded_context_label "$candidate" 80
}

cleanup_guest() {
  local vm="$1" directory="${GUEST_DIRS[$1]:-}" rc=0
  [ -n "$directory" ] || return 0
  phase_start guest-cleanup "$vm"
  printf 'E2E_BUDGET phase=guest-cleanup budget_seconds=120 grace_seconds=5 vm=%s\n' "$vm" >&2 || true
  case "$directory" in /var/tmp/subyard-worktree.*) ;; *) phase_end 1; return 1 ;; esac
  if [ "${2:-}" = quiet ]; then
    E2E_GUEST_TIMEOUT=120 guest "$vm" sudo -n find "$directory" -depth -delete </dev/null >/dev/null 2>&1 || rc=$?
  else
    E2E_GUEST_TIMEOUT=120 guest "$vm" sudo -n find "$directory" -depth -delete </dev/null || rc=$?
  fi
  phase_end "$rc"
  [ "$rc" = 0 ] || return "$rc"
  unset 'GUEST_DIRS[$vm]'
}

cleanup_on_exit() {
  local rc=${2:-$?} vm cleanup_failed=0 started=$SECONDS
  trap - EXIT INT TERM
  set +e
  # Interrupted work stays incomplete; EXIT closes terminal failures only.
  if [ "${1:-}" != interrupted ]; then phase_end "$rc"; fi
  E2E_ACTIVE_PHASE=""
  phase_marker cleanup/release start 0 0
  if [ -n "$LEASE_KEEPER_PID" ]; then
    kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
    wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
  fi
  for vm in "${!GUEST_DIRS[@]}"; do
    cleanup_guest "$vm" quiet >/dev/null || cleanup_failed=1
  done
  if [ -n "$LEASE_ACQUIRE_PID" ] && [ -z "$LEASE_ID" ]; then
    printf 'agent-e2e: acquire cleanup remains unconfirmed: slot=%s run=%s\n' "$LEASE_REQUESTED_SLOT" "$LEASE_RUN" >&2
    cleanup_failed=1
  fi
  release_lease || cleanup_failed=1
  if [ -n "$LEASE_ACQUIRE_PID" ]; then
    kill "$LEASE_ACQUIRE_PID" >/dev/null 2>&1 || true
    wait "$LEASE_ACQUIRE_PID" >/dev/null 2>&1 || true
    LEASE_ACQUIRE_PID=''
  fi
  if [ -n "$LOCAL_TEMP" ]; then
    case "$LOCAL_TEMP" in /tmp/subyard-agent-e2e.*|"${TMPDIR:-/tmp}"/subyard-agent-e2e.*)
      find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_failed=1
      ;;
    esac
  fi
  # Keep the workload/signal outcome; cleanup has its own terminal phase.
  [ "$cleanup_failed" = 0 ] || [ "$rc" != 0 ] || rc=3
  phase_marker cleanup/release end "$((SECONDS - started))" "$((cleanup_failed * 3))"
  if [ "$rc" != 0 ] && [ -n "$LEASE_REQUESTED_SLOT" ]; then
    printf 'agent-e2e: inspect own broker diagnostics: dev/agent-e2e.sh --yard %s --logs --slot %s -n 50\n' \
      "$E2E_YARD" "$((10#${LEASE_REQUESTED_SLOT#slot-}))" >&2
  fi
  exit "$rc"
}

worktree_paths() {
  bash "$REPO_ROOT/tests/helpers/source-files.sh" "$@"
}

build_bundle() {
  local root="$1" bundle="$2" path directory resolved count=0
  shift 2
  if [ -n "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" ]; then
    [[ "${SUBYARD_E2E_CANDIDATE_SHA256:-}" =~ ^[0-9a-f]{64}$ ]] \
      || die 'SUBYARD_E2E_CANDIDATE_SHA256 must be a lowercase SHA-256 digest'
    [ -f "$SUBYARD_E2E_CANDIDATE_BUNDLE" ] && [ ! -L "$SUBYARD_E2E_CANDIDATE_BUNDLE" ] \
      || die 'SUBYARD_E2E_CANDIDATE_BUNDLE must be a regular non-symlink archive'
    [ "$(sha256sum "$SUBYARD_E2E_CANDIDATE_BUNDLE" | awk '{print $1}')" = "$SUBYARD_E2E_CANDIDATE_SHA256" ] \
      || die 'SUBYARD_E2E_CANDIDATE_BUNDLE checksum mismatch'
    cp -- "$SUBYARD_E2E_CANDIDATE_BUNDLE" "$bundle"
    return
  fi
  local -a paths=()
  while IFS= read -r -d '' path; do
    if [ ! -e "$root/$path" ] && [ ! -L "$root/$path" ]; then continue; fi
    case "$path" in
      '' | /* | ../* | */../* | .git | .git/* | private | private/* | temp | temp/*)
        die "refusing non-public worktree path '$path'"
        ;;
    esac
    directory="$root"
    [[ "$path" != */* ]] || directory="$root/${path%/*}"
    [ "$(realpath "$directory")" = "$directory" ] \
      || die "refusing symlinked worktree directory: '$path'"
    if [ -L "$root/$path" ]; then
      resolved="$(realpath "$root/$path")" || die "cannot resolve symlink '$path'"
      case "$resolved" in "$root"/*) ;; *) die "refusing symlink outside the worktree: '$path'" ;; esac
      if [ "$path" = .claude/skills ]; then
        [ "$(readlink "$root/$path")" = ../.agents/skills ] \
          && [ "$resolved" = "$root/.agents/skills" ] && [ -d "$resolved" ] \
          || die "refusing noncanonical shared skill alias: '$path'"
      fi
    fi
    paths+=("$path")
    count=$((count + 1))
  done < <(worktree_paths "$root" --include-client-aliases --follow; if [ "$#" -gt 0 ]; then printf '%s\0' "$@"; fi)
  [ "$count" -gt 0 ] || die "public worktree is empty"
  printf '%s\0' "${paths[@]}" | LC_ALL=C sort -z | tar -C "$root" --null -T - -cf - | gzip -n > "$bundle"
}

write_guest_command() {
	local vm="$1" directory="$2"; shift 2
	printf '#!/usr/bin/env bash\nset -euo pipefail\n'
	printf 'chown -R dev:dev %q\n' "$directory"
	printf 'cd %q\n' "$directory/src"
	printf 'export SUBYARD_E2E_YARD=%q\n' "$E2E_YARD"
	printf 'export SUBYARD_E2E_PROJECT=%q\n' "$LEASE_PROJECT"
	printf 'export SUBYARD_E2E_RUN_ID=%q\n' "$LEASE_RUN"
	printf 'export SUBYARD_E2E_PURPOSE=%q\n' "$LEASE_PURPOSE"
	printf 'export SUBYARD_E2E_SLOT=%q\n' "$LEASE_SLOT"
	printf 'export SUBYARD_E2E_GENERATION=%q\n' "$LEASE_GENERATION"
	printf 'export SUBYARD_E2E_TYPE=%q\n' "$ENVIRONMENT_TYPE"
	printf 'export SUBYARD_E2E_VM_COUNT=%q\n' "$VM_COUNT"
	printf 'export SUBYARD_E2E_CPU_PER_VM=%q\n' "$GRANT_CPU_PER_VM"
	printf 'export SUBYARD_E2E_MEMORY_PER_VM=%q\n' "$GRANT_MEMORY_PER_VM"
	printf 'export SUBYARD_E2E_DISK_PER_VM=%q\n' "$GRANT_DISK_PER_VM"
	printf 'export SUBYARD_E2E_BASE_FINGERPRINT=%q\n' "$BASE_FINGERPRINT"
	printf 'export SUBYARD_E2E_VM=%q\n' "$vm"
	if [ "${1:-}" = ./bin/yard ]; then
		printf '/usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev make build\n'
	fi
	printf 'exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev'
	printf ' %q' "$@"
	printf '\n'
}

prepare_guest() {
  local vm="$1" bundle="$2" expected_hash="$3" directory actual_hash fstype
  # Release assets and native build outputs must not consume the guest's RAM reserve.
  fstype="$(guest "$vm" findmnt -n -o FSTYPE -T /var/tmp </dev/null)" \
    || { printf 'agent-e2e: VM%s could not inspect guest staging storage\n' "$vm" >&2; return 2; }
  case "$fstype" in
    ''|tmpfs|ramfs)
      printf 'agent-e2e: VM%s guest staging requires persistent storage\n' "$vm" >&2
      return 2
      ;;
  esac
  directory="$(guest "$vm" mktemp -d /var/tmp/subyard-worktree.XXXXXX </dev/null)" \
    || { printf 'agent-e2e: VM%s did not create a run directory\n' "$vm" >&2; return 2; }
  case "$directory" in
    /var/tmp/subyard-worktree.*) ;;
    *) printf 'agent-e2e: VM%s returned an unsafe run directory\n' "$vm" >&2; return 2 ;;
  esac
  GUEST_DIRS[$vm]="$directory"

  info "VM$vm: streaming current worktree" >&2
  guest "$vm" dd "of=$directory/worktree.tar.gz" status=none < "$bundle" \
    || { printf 'agent-e2e: VM%s worktree transfer failed\n' "$vm" >&2; return 2; }
  actual_hash="$(guest "$vm" sha256sum "$directory/worktree.tar.gz" </dev/null | awk '{print $1}')" \
    || { printf 'agent-e2e: VM%s checksum query failed\n' "$vm" >&2; return 2; }
  [ "$actual_hash" = "$expected_hash" ] \
    || { printf 'agent-e2e: VM%s worktree checksum mismatch\n' "$vm" >&2; return 2; }
  guest "$vm" mkdir "$directory/src" </dev/null \
    || { printf 'agent-e2e: VM%s source directory creation failed\n' "$vm" >&2; return 2; }
  guest "$vm" tar -xzf "$directory/worktree.tar.gz" -C "$directory/src" </dev/null \
    || { printf 'agent-e2e: VM%s worktree extraction failed\n' "$vm" >&2; return 2; }
  PREPARED_DIRECTORY="$directory"
}

run_guest() {
  local vm="$1" bundle="$2" expected_hash="$3" directory rc=0; shift 3
  phase_start transport "$vm"
  prepare_guest "$vm" "$bundle" "$expected_hash" || { rc=$?; phase_end "$rc"; return "$rc"; }
  directory="$PREPARED_DIRECTORY"
  write_guest_command "$vm" "$directory" "$@" \
    | guest "$vm" dd "of=$directory/run.sh" status=none \
    || { printf 'agent-e2e: VM%s command transfer failed\n' "$vm" >&2; phase_end 2; return 2; }
  guest "$vm" chmod 0700 "$directory/run.sh" </dev/null \
    || { printf 'agent-e2e: VM%s command preparation failed\n' "$vm" >&2; phase_end 2; return 2; }
  phase_end 0
  printf '\n== e2e-vm-%s ==\n' "$vm"
  phase_start guest "$vm"
  guest "$vm" "$directory/run.sh" </dev/null 2>&1 | normalize_terminal_progress
  rc=$?
  phase_end "$rc"
  return "$rc"
}

normalize_terminal_progress() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ "$line" == *$'\r'* ]]; then
      line="${line%$'\r'}"
      line="${line##*$'\r'}"
    fi
    printf '%s\n' "$line"
  done
}

run_direct_ssh() {
  local vm="$1" forward_stdin="$2" command rc; shift 2
  phase_start guest "$vm"
  if [ "$#" -gt 0 ]; then
    command="$(quote_ssh_command "$@")"
    if [ "$forward_stdin" = 1 ]; then
      ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$command"
      rc=$?; phase_end "$rc"; return "$rc"
    fi
    ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$command" </dev/null
    rc=$?; phase_end "$rc"; return "$rc"
  fi
  [ "$forward_stdin" = 0 ] || die "--ssh-stdin requires a command"
  ssh -F "$CLIENT_CONFIG" -tt "e2e-vm-$vm"
  rc=$?; phase_end "$rc"; return "$rc"
}

main() {
  local selector=all root bundle bundle_hash vm guest_rc run_failed=0 cleanup_failed=0
  local mode=run ssh_vm='' ssh_stdin=0 wait_value purpose_override='' status_json=0 slot_value=''
  local log_count=50 log_count_set=0 incident_id='' diagnostic_mode=''
  local status_response observer_cursor=''
  local -a selected=() command=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --yard) [ "$#" -ge 2 ] || die "--yard needs a yard name"; E2E_YARD="$2"; E2E_LEGACY_ROUTE=1; shift 2 ;;
      --owner) [ "$#" -ge 2 ] && [ -z "$E2E_OWNER" ] || die '--owner requires one owner ID'; E2E_OWNER="$2"; shift 2 ;;
      --pool) [ "$#" -ge 2 ] && [ -z "$E2E_POOL" ] || die '--pool requires one pool ID'; E2E_POOL="$2"; shift 2 ;;
	    --slot)
	      [ "$#" -ge 2 ] || die "--slot needs a number from 1 to 999"
	      [ -z "$slot_value" ] || die "--slot may be specified only once"
	      slot_value="$2"
	      set_requested_slot "$slot_value" --slot
	      shift 2
        ;;
      --type)
        [ "$#" -ge 2 ] || die "--type needs subyard-pair or android-test"
        ENVIRONMENT_TYPE="$2"; shift 2 ;;
      --disk)
        [ "$#" -ge 2 ] || die '--disk needs 50GiB'
        [ -z "$DISK_REQUESTED" ] || die '--disk may be specified only once'
        [ "$2" = 50GiB ] || die '--disk must be 50GiB'
        DISK_REQUESTED="$2"; shift 2 ;;
      --vm-count)
        [ "$#" -ge 2 ] || die '--vm-count needs 1 or 2'
        [ -z "$VM_COUNT_REQUESTED" ] || die '--vm-count may be specified only once'
        case "$2" in 1|2) VM_COUNT_REQUESTED="$2" ;; *) die '--vm-count must be 1 or 2' ;; esac
        shift 2 ;;
      --vm) [ "$#" -ge 2 ] || die "--vm needs 1, 2 or both"; selector="$2"; shift 2 ;;
      --ssh) [ "$#" -ge 2 ] || die "--ssh needs 1 or 2"; mode=ssh; ssh_vm="$2"; shift 2 ;;
      --ssh-stdin)
        [ "$#" -ge 2 ] || die "--ssh-stdin needs 1 or 2"
        mode=ssh
        ssh_vm="$2"
        ssh_stdin=1
        shift 2
        ;;
      --observe)
        [ "$#" -ge 2 ] && [ -z "$diagnostic_mode" ] || die '--observe needs one cursor'
        mode=observe; diagnostic_mode=observe; observer_cursor="$2"; shift 2 ;;
      --status) mode=status; shift ;;
      --logs)
        [ -z "$diagnostic_mode" ] || die 'choose one diagnostic mode'
        mode=logs; diagnostic_mode=logs; shift ;;
      --incident)
        [ -z "$diagnostic_mode" ] || die 'choose one diagnostic mode'
        [ "$#" -ge 2 ] || die '--incident needs an incident ID'
        incident_id="$2"
        [[ "$incident_id" =~ ^[0-9]{20}-[0-9a-f]{16}$ ]] || die 'invalid incident ID'
        mode=incident; diagnostic_mode=incident; shift 2 ;;
      -n)
        [ "$#" -ge 2 ] || die '-n needs a number from 1 to 200'
        [[ "$2" =~ ^[0-9]{1,3}$ ]] && [ "$((10#$2))" -ge 1 ] && [ "$((10#$2))" -le 200 ] \
          || die '-n needs a number from 1 to 200'
        log_count="$((10#$2))"; log_count_set=1; shift 2 ;;
      --json) status_json=1; shift ;;
      --purpose)
        [ "$#" -ge 2 ] || die "--purpose needs a safe label"
        purpose_override="$2"
        shift 2
        ;;
      --wait)
        [ "$#" -ge 2 ] || die "--wait needs a duration"
        wait_value="$2"
        case "$wait_value" in
          *m) WAIT_SECONDS=$((${wait_value%m} * 60)) ;;
          *s) WAIT_SECONDS=${wait_value%s} ;;
          *) die "--wait duration must use s or m" ;;
        esac
        [[ "$WAIT_SECONDS" =~ ^[0-9]+$ ]] || die "invalid --wait duration"
        shift 2
        ;;
      --prepare) mode=prepare; shift ;;
      --verify-boundary) mode=verify; shift ;;
      --) shift; command=("$@"); break ;;
      -h | --help) usage; return 0 ;;
      *) die "unknown argument '$1' (put the guest command after --)" ;;
    esac
  done
  resolve_vm_count
  case "$ENVIRONMENT_TYPE" in
    subyard-pair) ;;
    android-test)
      [ "$selector" != 2 ] && [ "$selector" != both ] && [ "$ssh_vm" != 2 ] \
        || die "android-test has only VM 1; selectors 2 and both are invalid"
      [ "$mode" != verify ] || die "--verify-boundary requires subyard-pair"
      ;;
    *) die "--type must be subyard-pair or android-test" ;;
  esac
  if [ "$VM_COUNT" = 1 ]; then
    [ "$selector" != 2 ] && [ "$selector" != both ] && [ "$ssh_vm" != 2 ] \
      || die 'one-VM allocation has only VM 1; selectors 2 and both are invalid'
  fi
  case "$mode" in
    run|ssh|verify) [ -n "$LEASE_REQUESTED_SLOT" ] || die "an exact --slot is required before acquiring an E2E lease" ;;
  esac
  resolve_service_selection "$mode"
  configure_yard_scope
  [ -z "$diagnostic_mode" ] || [ "$diagnostic_mode" = "$mode" ] || die 'diagnostic reads cannot be combined with another mode'
  [ "$status_json" = 0 ] || [ "$mode" = status ] || [ "$mode" = logs ] || die "--json is valid only with --status or --logs"
  [ "$log_count_set" = 0 ] || [ "$mode" = logs ] || die '-n is valid only with --logs'
  [ -z "$incident_id" ] || [ "$mode" = incident ] || die '--incident cannot be combined with another mode'
  if [ "$mode" = observe ]; then
    [ "${#command[@]}" -eq 0 ] && [ -z "$purpose_override" ] && [ -z "$VM_COUNT_REQUESTED" ] && [ -z "$DISK_REQUESTED" ] && [ "$WAIT_SECONDS" = 0 ] && [ -z "$LEASE_REQUESTED_SLOT" ] || die 'observer reads accept no allocation options'
    read_agent_observations "$observer_cursor"
    return
  fi
  if [ "$mode" = logs ] || [ "$mode" = incident ]; then
    [ -z "$purpose_override" ] && [ -z "$VM_COUNT_REQUESTED" ] && [ -z "$DISK_REQUESTED" ] && [ "$WAIT_SECONDS" = 0 ] \
      || die 'diagnostic reads do not accept allocation options'
    [ "$mode" != incident ] || [ -z "$LEASE_REQUESTED_SLOT" ] || die '--slot is not valid with --incident'
    [ "${#command[@]}" -eq 0 ] || die 'diagnostic reads take no guest command'
    read_agent_logs "$mode" "$log_count" "$status_json" "$incident_id"
    return
  fi
  [ -z "$purpose_override" ] || [ "$mode" != status ] || die "--purpose is not valid with --status"
  [ -z "$LEASE_REQUESTED_SLOT" ] || {
    [ "$mode" != status ] && [ "$mode" != prepare ] \
      || die "--slot is valid only for commands that acquire a lease"
  }
  case "$mode" in
    run|ssh|verify)
      [ -n "$LEASE_REQUESTED_SLOT" ] \
        || die "an exact --slot is required before acquiring an E2E lease"
      ;;
  esac
  LEASE_PURPOSE="$(derive_purpose "$mode" "$purpose_override" "${command[@]}")"
  case "$mode" in
    prepare)
      [ "${#command[@]}" -eq 0 ] || die "--prepare takes no command"
      prepare_identity
      return
      ;;
    status)
      [ "${#command[@]}" -eq 0 ] || die "--status takes no command"
      status_response="$(facade_request status)" || die "test environment unavailable"
      if [ "$status_json" = 1 ]; then
        printf '%s\n' "$status_response"
      else
        render_pool_status "$status_response"
      fi
      return
      ;;
    verify)
      [ "${#command[@]}" -eq 0 ] || die "--verify-boundary takes no command"
      trap cleanup_on_exit EXIT
      trap 'cleanup_on_exit interrupted 130' INT
      trap 'cleanup_on_exit interrupted 143' TERM
      LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
      phase_start allocation
      acquire_lease
      phase_end "$?"
      start_lease_keeper
      verify_boundary
      return
      ;;
    ssh)
      case "$ssh_vm" in 1 | 2) ;; *) die "--ssh needs VM selector 1 or 2" ;; esac
      trap cleanup_on_exit EXIT
      trap 'cleanup_on_exit interrupted 130' INT
      trap 'cleanup_on_exit interrupted 143' TERM
      LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
      phase_start allocation
      acquire_lease
      phase_end "$?"
      start_lease_keeper
      run_direct_ssh "$ssh_vm" "$ssh_stdin" "${command[@]}"
      return
      ;;
  esac

  [ "${#command[@]}" -gt 0 ] || die "a guest command is required after --"
  case "$selector" in all) for ((vm=1; vm<=VM_COUNT; vm++)); do selected+=("$vm"); done ;; 1) selected=(1) ;; 2) selected=(2) ;; both) selected=(1 2) ;; *) die "--vm must be 1, 2 or both" ;; esac
  root="$REPO_ROOT"
  command -v rg >/dev/null 2>&1 || die "ripgrep is required"
  command -v tar >/dev/null 2>&1 || die "tar is required"
  command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"

  trap cleanup_on_exit EXIT
  trap 'cleanup_on_exit interrupted 130' INT
  trap 'cleanup_on_exit interrupted 143' TERM
  LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
  phase_start allocation
  acquire_lease
  phase_end "$?"
  start_lease_keeper
  bundle="$LOCAL_TEMP/worktree.tar.gz"
  info "packing current public worktree"
  phase_start packing
  build_bundle "$root" "$bundle"
  bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
  phase_end "$?"
  ok "worktree bundle ready (sha256=$bundle_hash)"

  for vm in "${selected[@]}"; do
    if run_guest "$vm" "$bundle" "$bundle_hash" "${command[@]}"; then :; else
      guest_rc=$?
      [ "$run_failed" != 0 ] || run_failed="$guest_rc"
    fi
    if ! cleanup_guest "$vm"; then
      printf 'agent-e2e: VM%s run directory cleanup failed\n' "$vm" >&2
      cleanup_failed=1
    fi
  done
  [ "$run_failed" = 0 ] || return "$run_failed"
  [ "$cleanup_failed" = 0 ] || return 3
  ok "selected VM checks passed; run-specific worktrees removed"
}

[ "${BASH_SOURCE[0]}" != "$0" ] || main "$@"

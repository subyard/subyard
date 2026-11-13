#!/usr/bin/env bash
# Shared test-VM broker client: trusted route, forced facade, leases and guest transport.
# dev/agent-e2e.sh sources it; agents run the installed copy for session lease handles.

CLIENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
CLIENT_SELF="$CLIENT_DIR/$(basename "${BASH_SOURCE[0]}")"
E2E_PROGRAM="${E2E_PROGRAM:-subyard-test-vms}"

client_operator_home() {
  if declare -F subyard_operator_home >/dev/null; then
    subyard_operator_home
  else
    printf '%s\n' "${HOME:?}"
  fi
}

# Callers may replace these hooks after sourcing.
telemetry_correlation() { :; }
lease_attribution() { client_attribution; }

E2E_ROUTE_CAPTURED=0
# process: keeper bound to the calling process; handle: detached keeper bound to a handle.
LEASE_KEEPER_MODE=process
CLIENT_PROJECT=""
HANDLE_IDLE_SECONDS=1200

E2E_YARD="${SUBYARD_E2E_YARD:-test-yard}"
E2E_LEGACY_ROUTE=0
E2E_OWNER=""
E2E_POOL=""
E2E_SERVICE_GENERATION=""
E2E_CAPTURED_ROUTE_DIRECTORY=""
BASTION_USER="${SUBYARD_E2E_BASTION_USER:-subyard-e2e-agent}"
STATE_BASE="${SUBYARD_E2E_STATE_DIR:-${SUBYARD_HOME:-$(client_operator_home)/.subyard}/e2e}"
BASTION_ROUTE=""
STATE_ROOT=""
IDENTITY=""
CLIENT_CONFIG=""
GUEST_KNOWN_HOSTS=""
BASTION_HOSTNAME=""
BASTION_PORT=""
BASTION_HOST_KEY_ALIAS=""
BASTION_KNOWN_HOSTS=""
LOCAL_TEMP=""
GUEST_IDENTITY=""
GUEST_USER=root
DATA_USER=""
LEASE_SLOT=""
LEASE_GENERATION=""
LEASE_ID=""
LEASE_EPOCH=""
LEASE_CAPABILITY=""
LEASE_KEEPER_PID=""
LEASE_ACQUIRE_PID=""
LEASE_ACQUIRE_ACK=""
LEASE_ACQUIRE_REPLY=""
LEASE_ACQUIRE_FAILURE=""
LEASE_ACQUIRE_GUARD_RC=""
LEASE_ACQUIRE_PROTOCOL=acquire-v3
LEASE_KEEPER_LOG=""
LEASE_YARD=""
LEASE_PROJECT=""
LEASE_RUN=""
LEASE_PURPOSE=""
LEASE_REQUESTED_SLOT=""
ENVIRONMENT_TYPE=subyard-pair
VM_COUNT=2
VM_COUNT_REQUESTED=''
DISK_REQUESTED=''
BASE_FINGERPRINT=""
GRANT_CPU_PER_VM=""
GRANT_MEMORY_PER_VM=""
GRANT_DISK_PER_VM=""
WAIT_SECONDS=0
declare -A VM_IP=()
declare -A VM_HOST_KEY=()

die() { printf '%s: %s\n' "$E2E_PROGRAM" "$*" >&2; exit 2; }
# Log parsers match the fixed refusal line; the preceding cause line names the die point.
die_acquire_unknown() {
  printf '%s: lease acquire unknown-outcome cause=%s\n' "$E2E_PROGRAM" "$1" >&2
  die "lease acquire outcome is unknown; refusing a second allocation"
}
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

configure_yard_scope() {
  case "$E2E_YARD" in
    '' | *[!a-z0-9_-]* | _* | -*) die "unsafe yard selector '$E2E_YARD'" ;;
  esac
  BASTION_ROUTE="${SUBYARD_E2E_BASTION_ROUTE:-yard-$E2E_YARD}"
  if [ -n "$E2E_SERVICE_GENERATION" ]; then
    STATE_ROOT="$STATE_BASE/services/$E2E_OWNER/$E2E_POOL/$E2E_SERVICE_GENERATION"
  else
    STATE_ROOT="${SUBYARD_E2E_YARD_STATE_DIR:-$STATE_BASE/yards/$E2E_YARD}"
  fi
  IDENTITY="${SUBYARD_E2E_IDENTITY:-$STATE_BASE/id_ed25519}"
  CLIENT_CONFIG="$STATE_ROOT/ssh_config"
  GUEST_KNOWN_HOSTS="$STATE_ROOT/guest_known_hosts"
}

configure_yard_scope

ensure_state_root() {
  umask 077
  install -d -m 0700 "$STATE_ROOT"
}

normalized_public_key_file() {
  local file="$1" type blob _rest
  [ -r "$file" ] || return 1
  read -r type blob _rest < "$file"
  [ "$type" = ssh-ed25519 ] && [[ "$blob" =~ ^[A-Za-z0-9+/=]+$ ]] || return 1
  printf '%s %s\n' "$type" "$blob"
}

ensure_identity() {
  local derived recorded type blob _rest
  ensure_state_root
  if [ ! -e "$IDENTITY" ] && [ ! -e "$IDENTITY.pub" ]; then
    ssh-keygen -q -t ed25519 -N '' -C subyard-e2e-agent -f "$IDENTITY"
  fi
  [ -s "$IDENTITY" ] && [ -s "$IDENTITY.pub" ] \
    || die "incomplete agent identity at $IDENTITY (both private and .pub files are required)"
  [ "$(stat -c '%a' "$IDENTITY")" = 600 ] || chmod 0600 "$IDENTITY"
  [ "$(stat -c '%a' "$IDENTITY.pub")" = 644 ] || chmod 0644 "$IDENTITY.pub"
  recorded="$(normalized_public_key_file "$IDENTITY.pub")" \
    || die "agent identity must use Ed25519"
  derived="$(ssh-keygen -y -f "$IDENTITY" 2>/dev/null)" \
    || die "cannot read the agent private identity"
  read -r type blob _rest <<<"$derived"
  derived="$type $blob"
  [ "$derived" = "$recorded" ] || die "agent identity public and private halves do not match"
}

prepare_identity() {
  ensure_identity
  ok "private controller identity ready at $IDENTITY"
  info "the operator-owned outer test yard and its yard-to-yard route must already be available"
}

valid_route_word() { [[ "$1" =~ ^[A-Za-z0-9._:%-]+$ ]]; }

known_host_lookup_name() {
  if [ -n "$BASTION_HOST_KEY_ALIAS" ] && [ "$BASTION_HOST_KEY_ALIAS" != none ]; then
    printf '%s\n' "$BASTION_HOST_KEY_ALIAS"
  elif [ "$BASTION_PORT" = 22 ]; then
    printf '%s\n' "$BASTION_HOSTNAME"
  else
    printf '[%s]:%s\n' "$BASTION_HOSTNAME" "$BASTION_PORT"
  fi
}

resolve_bastion_route() {
  # A held lease and its keeper retain the exact pinned endpoint captured before acquire.
  [ -z "$E2E_SERVICE_GENERATION" ] || [ -z "$BASTION_KNOWN_HOSTS" ] || return 0
  # A session handle carries its own captured route and never re-resolves it.
  [ "$E2E_ROUTE_CAPTURED" = 0 ] || return 0
  if [ -n "$E2E_SERVICE_GENERATION" ] && [ -n "${SUBYARD_E2E_BASTION_HOSTNAME:-}" ]; then
    die 'service routes require the trusted publication; explicit transport overrides require --yard'
  fi
  local rendered key value rest lookup candidate route_config="${SUBYARD_E2E_ROUTE_CONFIG:-${HOME:?}/.ssh/config}"
  local explicit_known="${SUBYARD_E2E_BASTION_KNOWN_HOSTS:-}"
  local registry="${SUBYARD_E2E_ROUTE_REGISTRY:-/var/lib/subyard/e2e-routes}"
  local route_file="$registry/$E2E_YARD/current/route.tsv"
  local known_file="$registry/$E2E_YARD/current/known_hosts"
  if [ -n "$E2E_CAPTURED_ROUTE_DIRECTORY" ]; then
    route_file="$E2E_CAPTURED_ROUTE_DIRECTORY/route.tsv"
    known_file="$E2E_CAPTURED_ROUTE_DIRECTORY/known_hosts"
  fi
  local header route_key route_value route_extra hostname_seen=0 port_seen=0 alias_seen=0
  local -a known_candidates=()
  local -a extra_known_candidates=()
  case "$BASTION_USER" in '' | -* | *[!a-z0-9_-]*) die "unsafe bastion user '$BASTION_USER'" ;; esac
  if [ -n "${SUBYARD_E2E_BASTION_HOSTNAME:-}" ]; then
    BASTION_HOSTNAME="$SUBYARD_E2E_BASTION_HOSTNAME"
    BASTION_PORT="${SUBYARD_E2E_BASTION_PORT:-22}"
    BASTION_HOST_KEY_ALIAS="${SUBYARD_E2E_BASTION_HOST_KEY_ALIAS:-}"
  elif [ -r "$route_file" ] && [ -r "$known_file" ]; then
    IFS= read -r header < "$route_file"
    [ "$header" = subyard-e2e-route-v1 ] || die "test environment route has an unknown format"
    while IFS=$'\t' read -r route_key route_value route_extra; do
      [ -z "$route_extra" ] || die "test environment route contains an invalid record"
      case "$route_key" in
        subyard-e2e-route-v1 | '') ;;
        hostname)
          [ "$hostname_seen" = 0 ] || die "duplicate test environment hostname"
          BASTION_HOSTNAME="$route_value"; hostname_seen=1 ;;
        port)
          [ "$port_seen" = 0 ] || die "duplicate test environment port"
          BASTION_PORT="$route_value"; port_seen=1 ;;
        host_key_alias)
          [ "$alias_seen" = 0 ] || die "duplicate test environment host-key alias"
          BASTION_HOST_KEY_ALIAS="$route_value"; alias_seen=1 ;;
        *) die "test environment route contains an unexpected record '$route_key'" ;;
      esac
    done < "$route_file"
    [ "$hostname_seen$port_seen$alias_seen" = 111 ] || die "test environment route is incomplete"
    known_candidates=("$known_file")
  else
    [ -z "$E2E_SERVICE_GENERATION" ] || die 'test environment unavailable: service_unavailable (published route absent)'
    [ -r "$route_config" ] || route_config=/dev/null
    rendered="$(ssh -G -F "$route_config" "$BASTION_ROUTE" 2>/dev/null)" \
      || die "cannot resolve SSH route '$BASTION_ROUTE'"
    while read -r key value rest; do
      case "$key" in
        hostname) [ -n "$BASTION_HOSTNAME" ] || BASTION_HOSTNAME="$value" ;;
        port) [ -n "$BASTION_PORT" ] || BASTION_PORT="$value" ;;
        hostkeyalias) [ -n "$BASTION_HOST_KEY_ALIAS" ] || BASTION_HOST_KEY_ALIAS="$value" ;;
        userknownhostsfile)
          known_candidates+=("$value")
          read -r -a extra_known_candidates <<<"$rest"
          known_candidates+=("${extra_known_candidates[@]}")
          ;;
      esac
    done <<<"$rendered"
  fi
  [ -n "$BASTION_HOSTNAME" ] && valid_route_word "$BASTION_HOSTNAME" \
    || die "resolved bastion hostname is missing or unsafe"
  [[ "$BASTION_PORT" =~ ^[0-9]+$ ]] && [ "$BASTION_PORT" -ge 1 ] && [ "$BASTION_PORT" -le 65535 ] \
    || die "resolved bastion port is invalid"
  [ -z "$BASTION_HOST_KEY_ALIAS" ] || valid_route_word "$BASTION_HOST_KEY_ALIAS" \
    || die "resolved bastion host-key alias is unsafe"
  lookup="$(known_host_lookup_name)"
  if [ -n "$explicit_known" ]; then known_candidates=("$explicit_known"); fi
  for candidate in "${known_candidates[@]}"; do
    case "$candidate" in \~/*) candidate="${HOME:?}/${candidate#\~/}" ;; esac
    [ -r "$candidate" ] || continue
    if ssh-keygen -F "$lookup" -f "$candidate" >/dev/null 2>&1; then
      BASTION_KNOWN_HOSTS="$candidate"
      break
    fi
  done
  [ -n "$BASTION_KNOWN_HOSTS" ] || die "test environment unavailable: no pinned host key for $lookup"
  if [ -n "$E2E_SERVICE_GENERATION" ]; then
    local captured_known
    captured_known="$(mktemp "$STATE_ROOT/.bastion-known.XXXXXX")"
    cat -- "$BASTION_KNOWN_HOSTS" > "$captured_known"
    chmod 0600 "$captured_known"
    mv -f -- "$captured_known" "$STATE_ROOT/bastion_known_hosts"
    BASTION_KNOWN_HOSTS="$STATE_ROOT/bastion_known_hosts"
  fi
}

ssh_config_value() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "$value"
}

render_client_config() {
  local selector alias
  {
    printf 'Host subyard-e2e-bastion\n'
    printf '    HostName %s\n' "$BASTION_HOSTNAME"
    printf '    Port %s\n' "$BASTION_PORT"
    printf '    User %s\n' "$BASTION_USER"
    printf '    IdentityFile '; ssh_config_value "$IDENTITY"; printf '\n'
    printf '    IdentitiesOnly yes\n'
    printf '    BatchMode yes\n'
    printf '    ForwardAgent no\n'
    printf '    RequestTTY no\n'
    printf '    StrictHostKeyChecking yes\n'
    printf '    UserKnownHostsFile '; ssh_config_value "$BASTION_KNOWN_HOSTS"; printf '\n'
    [ -z "$BASTION_HOST_KEY_ALIAS" ] || [ "$BASTION_HOST_KEY_ALIAS" = none ] \
      || printf '    HostKeyAlias %s\n' "$BASTION_HOST_KEY_ALIAS"
    if [ -n "$DATA_USER" ]; then
      printf '\nHost subyard-e2e-data\n'
      printf '    HostName %s\n' "$BASTION_HOSTNAME"
      printf '    Port %s\n' "$BASTION_PORT"
      printf '    User %s\n' "$DATA_USER"
      printf '    IdentityFile '; ssh_config_value "$GUEST_IDENTITY"; printf '\n'
      printf '    IdentitiesOnly yes\n'
      printf '    BatchMode yes\n'
      printf '    ForwardAgent no\n'
      printf '    RequestTTY no\n'
      printf '    StrictHostKeyChecking yes\n'
      printf '    UserKnownHostsFile '; ssh_config_value "$BASTION_KNOWN_HOSTS"; printf '\n'
      [ -z "$BASTION_HOST_KEY_ALIAS" ] || [ "$BASTION_HOST_KEY_ALIAS" = none ] \
        || printf '    HostKeyAlias %s\n' "$BASTION_HOST_KEY_ALIAS"
    fi
    if [ "${#VM_IP[@]}" -gt 0 ]; then
      for ((selector=1; selector<=${#VM_IP[@]}; selector++)); do
        alias="e2e-vm-$selector"
        printf '\nHost %s\n' "$alias"
        printf '    HostName %s\n' "${VM_IP[$selector]}"
        printf '    Port 22\n'
        printf '    User %s\n' "$GUEST_USER"
        printf '    IdentityFile '; ssh_config_value "${GUEST_IDENTITY:-$IDENTITY}"; printf '\n'
        printf '    IdentitiesOnly yes\n'
        printf '    BatchMode yes\n'
        printf '    ForwardAgent no\n'
        printf '    ProxyJump subyard-e2e-data\n'
        printf '    ConnectTimeout 10\n'
        printf '    ConnectionAttempts 1\n'
        printf '    ServerAliveInterval 15\n'
        printf '    ServerAliveCountMax 3\n'
        printf '    HostKeyAlias %s\n' "$alias"
        printf '    StrictHostKeyChecking yes\n'
        printf '    UserKnownHostsFile '; ssh_config_value "$GUEST_KNOWN_HOSTS"; printf '\n'
      done
    fi
  }
}

ensure_client_id() {
  local path="$STATE_ROOT/client-id" temp value
  ensure_state_root
  # Parallel controllers must retain the same private diagnostic read capability.
  (
  flock -x 9
  if [ -r "$path" ]; then
    read -r value < "$path"
  else
    value="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
    temp="$(mktemp "$STATE_ROOT/.client-id.XXXXXX")"
    printf '%s\n' "$value" > "$temp"
    chmod 0600 "$temp"
    mv -f "$temp" "$path"
  fi
  [[ "$value" =~ ^[0-9a-f]{32}$ ]] || die "invalid persistent E2E client id"
  printf '%s\n' "$value"
  ) 9>"$STATE_ROOT/.client-id.lock"
}

read_agent_logs() {
  local mode="$1" count="$2" json_output="$3" incident_id="$4" client response request status
  command -v jq >/dev/null 2>&1 || die 'jq is required'
  # A read must not manufacture a new identity with no ownership history.
  [ -f "$STATE_ROOT/client-id" ] && [ ! -L "$STATE_ROOT/client-id" ] \
    || die 'no controller lease history; acquire a lease with this controller first'
  read -r client < "$STATE_ROOT/client-id"
  [[ "$client" =~ ^[0-9a-f]{32}$ ]] || die 'invalid persistent E2E client id'
  status="$(facade_request status)" || die 'test environment unavailable'
  jq -e '.schema_version == 1 and .status == "ok" and
    ((.capabilities // []) | index("lease-logs-v1") != null)' <<<"$status" >/dev/null 2>&1 \
    || die 'broker does not support lease logs (lease-logs-v1); operator runtime update required'
  if [ "$mode" = incident ]; then
    request="incident-v1 $client $incident_id"
  else
    request="logs-v1 $client ${LEASE_REQUESTED_SLOT:--} $count"
  fi
  response="$(facade_request "$request")" || die 'test environment unavailable'
  jq -e '.schema_version == 1 and .status == "ok"' <<<"$response" >/dev/null 2>&1 \
    || die 'diagnostics are unavailable for this controller'
  if [ "$mode" = incident ]; then
    jq -e '.incident | type == "object"' <<<"$response" >/dev/null \
      || die 'invalid incident response'
    jq '.incident' <<<"$response"
  elif [ "$json_output" = 1 ]; then
    jq -ce '.events | select(type == "array")' <<<"$response" \
      || die 'invalid lease log response'
  else
    jq -ce '.events | select(type == "array") | .[]' <<<"$response" \
      || { [ "$(jq -c '.events' <<<"$response")" = '[]' ] || die 'invalid lease log response'; }
  fi
}

read_agent_observations() {
  local cursor="$1" client status
  [[ "$cursor" = - || "$cursor" =~ ^[0-9]{20}-[0-9a-f]{16}$ ]] || die 'invalid observer cursor'
  [ -f "$STATE_ROOT/client-id" ] && [ ! -L "$STATE_ROOT/client-id" ] || die 'no controller lease history'
  [ "$(stat -c '%a' "$STATE_ROOT/client-id")" = 600 ] || die 'unsafe controller identity mode'
  read -r client < "$STATE_ROOT/client-id"
  [[ "$client" =~ ^[0-9a-f]{32}$ ]] || die 'invalid controller identity'
  status="$(facade_request status)" || die 'test environment unavailable'
  jq -e '.schema_version == 1 and .status == "ok" and ((.capabilities // []) | index("resource-observe-v1") != null)' <<<"$status" >/dev/null || die 'resource observations unsupported'
  facade_request "observe-v1 $client $cursor 100"
}

bounded_context_label() {
  local raw="$1" maximum="$2" value digest keep
  value="$(printf '%s' "$raw" | LC_ALL=C tr -cs 'A-Za-z0-9._/+-:' '-')"
  value="${value#-}"
  value="${value%-}"
  [ -n "$value" ] || value=unknown
  if [ "${#value}" -gt "$maximum" ]; then
    digest="$(printf '%s' "$value" | sha256sum | awk '{print substr($1, 1, 8)}')"
    keep=$((maximum - 9))
    value="${value:0:keep}-$digest"
  fi
  printf '%s\n' "$value"
}

safe_project_name() {
  local value="$1"
  [ -n "$value" ] && [ "${#value}" -le 50 ] &&
    [[ "$value" =~ ^[A-Za-z0-9._-]+$ ]] &&
    [ "$value" != . ] && [ "$value" != .. ] && [[ "$value" != -* ]]
}

safe_attribution_yard() {
  local value="$1"
  [ -n "$value" ] && [ "${#value}" -le 32 ] &&
    [[ "$value" =~ ^[a-z0-9][a-z0-9_-]*$ ]]
}

new_run_id() {
  od -An -N4 -tx1 /dev/urandom | tr -d ' \n'
}

lease_acquire_request() {
  local client="$1" fingerprint="$2" key_type="$3" key_blob="$4"
  [ -n "$LEASE_REQUESTED_SLOT" ] || die "an exact --slot is required before acquiring an E2E lease"
  printf '%s %s %s %s %s %s %s %s %s %s %s' \
    "$LEASE_ACQUIRE_PROTOCOL" \
    "$ENVIRONMENT_TYPE" "$client" "$fingerprint" "$LEASE_YARD" "$LEASE_PROJECT" "$LEASE_RUN" \
    "$LEASE_PURPOSE" "$key_type" "$key_blob" "$LEASE_REQUESTED_SLOT"
  if [ -n "$DISK_REQUESTED" ]; then
    printf ' %s %s' "$VM_COUNT" "$DISK_REQUESTED"
  elif [ -n "$VM_COUNT_REQUESTED" ]; then
    printf ' %s' "$VM_COUNT_REQUESTED"
  fi
  printf '\n'
}

resolve_vm_count() {
  case "$ENVIRONMENT_TYPE" in
    subyard-pair) VM_COUNT=2 ;;
    android-test) VM_COUNT=1 ;;
    *) die "--type must be subyard-pair or android-test" ;;
  esac
  if [ -n "$VM_COUNT_REQUESTED" ]; then
    case "$VM_COUNT_REQUESTED" in 1|2) ;; *) die '--vm-count must be 1 or 2' ;; esac
    [ "$ENVIRONMENT_TYPE" != android-test ] || [ "$VM_COUNT_REQUESTED" = 1 ] \
      || die 'android-test supports only --vm-count 1'
    VM_COUNT="$VM_COUNT_REQUESTED"
  fi
}

facade_request() {
  local command="$1" bootstrap rc=0
  ensure_identity
  resolve_bastion_route
  bootstrap="$(mktemp "$STATE_ROOT/.facade-config.XXXXXX")"
  VM_IP=(); VM_HOST_KEY=()
  render_client_config > "$bootstrap"
  chmod 0600 "$bootstrap"
  if [ "${3:-}" = stream ]; then
    ssh -F "$bootstrap" -T subyard-e2e-bastion -- "$command" || rc=$?
  elif [ -n "${2:-}" ]; then
    timeout --signal=TERM --kill-after=5 "$2" ssh -F "$bootstrap" -T subyard-e2e-bastion -- "$command" </dev/null || rc=$?
  else
    ssh -F "$bootstrap" -T subyard-e2e-bastion -- "$command" </dev/null || rc=$?
  fi
  rm -f "$bootstrap"
  return "$rc"
}


# One owned acquire session publishes credentials before its long provisioning work.
# The existing PID/start watcher also closes this session when the wrapper is killed.
lease_acquire_input() {
  while lease_keeper_owner_alive; do
    if [ -e "$LEASE_ACQUIRE_ACK" ]; then printf 'ack\n'; return; fi
    printf 'keepalive\n' || return
    sleep 1
  done
}


lease_keeper_stop_jobs() {
  local pid
  for pid in "$keeper_timer_pid" "$keeper_job_pid"; do
    [ -z "$pid" ] || { kill -KILL -- "-$pid" >/dev/null 2>&1 || true; wait "$pid" >/dev/null 2>&1 || true; }
  done
}

lease_acquire_guard() {
  local owner_pid="$1" owner_identity="$2" requester_pid="$3" requester_identity="$4" request="$5"
  local keeper_timer_pid='' keeper_job_pid='' rc=0 reservation
  set -m
  trap lease_keeper_stop_jobs EXIT
  trap 'exit 0' INT TERM
  # A refusal closes stdin before the next heartbeat; keep the SSH exit status.
  (set +m; set +o pipefail; lease_acquire_input | facade_request "$request" '' stream) > "$LEASE_ACQUIRE_REPLY" &
  keeper_job_pid=$!
  if lease_keeper_wait; then
    wait "$keeper_job_pid" || rc=$?
    keeper_job_pid=''
    return "$rc"
  fi
  : > "$LEASE_ACQUIRE_REPLY.owner-lost" 2>/dev/null || true
  kill -KILL -- "-$keeper_job_pid" >/dev/null 2>&1 || true
  wait "$keeper_job_pid" >/dev/null 2>&1 || true
  keeper_job_pid=''
  # EOF already asks the server to drain. Confirm cleanup when early credentials arrived.
  if IFS= read -r reservation < "$LEASE_ACQUIRE_REPLY" && parse_lease_reservation "$reservation"; then
    release_lease || printf '%s: interrupted acquire cleanup remains unconfirmed: slot=%s run=%s\n' "$E2E_PROGRAM" "$LEASE_SLOT" "$LEASE_RUN" >&2
  fi
  return 1
}

parse_lease_reservation() {
  local response="$1"
  jq -e '.schema_version == 1 and .status == "reserved" and
    (.grant.slot_id | type == "string" and test("^slot-[0-9]{3}$")) and
    (.grant.lease_id | type == "string" and test("^[0-9a-f]+$")) and
    (.grant.lease_epoch | type == "number" and . >= 1 and . == floor) and
    (.grant.capability | type == "string" and test("^[0-9a-f]+$"))' <<<"$response" >/dev/null 2>&1 || return 1
  LEASE_SLOT="$(jq -r '.grant.slot_id' <<<"$response")"
  LEASE_ID="$(jq -r '.grant.lease_id' <<<"$response")"
  LEASE_EPOCH="$(jq -r '.grant.lease_epoch' <<<"$response")"
  LEASE_CAPABILITY="$(jq -r '.grant.capability' <<<"$response")"
}

lease_acquire_stream() {
  local request="$1" owner_pid="$BASHPID" owner_identity requester_pid requester_identity response fd partial=''
  local guard_rc='' frame_rc
  LEASE_ACQUIRE_FAILURE='identity-unreadable'
  LEASE_ACQUIRE_GUARD_RC=''
  owner_identity="$(lease_process_identity "$owner_pid")" || return 1
  requester_pid="${owner_identity#*:}"
  [ "$requester_pid" -gt 1 ] && requester_identity="$(lease_process_identity "$requester_pid")" || return 1
  LEASE_ACQUIRE_FAILURE=''
  LEASE_ACQUIRE_REPLY="$LOCAL_TEMP/acquire-replies"
  LEASE_ACQUIRE_ACK="$LOCAL_TEMP/acquire-ack"
  : > "$LEASE_ACQUIRE_REPLY"
  rm -f "$LEASE_ACQUIRE_REPLY.owner-lost"
  chmod 0600 "$LEASE_ACQUIRE_REPLY"
  lease_acquire_guard "$owner_pid" "$owner_identity" "$requester_pid" "$requester_identity" "$request" &
  LEASE_ACQUIRE_PID=$!
  exec {fd}< "$LEASE_ACQUIRE_REPLY"
  while true; do
    if IFS= read -r response <&"$fd"; then
      response="$partial$response"
      partial=''
    else
      partial+="$response"
      [ "${#partial}" -le 65536 ] || { LEASE_ACQUIRE_FAILURE='oversized-frame'; exec {fd}<&-; return 1; }
      if [ -n "$guard_rc" ]; then
        # The guard exited before the final read, so this EOF is final; an unterminated
        # last frame is still complete. Only a stream without a terminal frame is unknown.
        if [ -z "$partial" ]; then
          LEASE_ACQUIRE_FAILURE="stream-ended-no-frame guard-rc=$guard_rc"
          [ ! -e "$LEASE_ACQUIRE_REPLY.owner-lost" ] || LEASE_ACQUIRE_FAILURE+=' owner-or-requester-lost'
          exec {fd}<&-
          return 1
        fi
        response="$partial"
        partial=''
      elif ! kill -0 "$LEASE_ACQUIRE_PID" >/dev/null 2>&1; then
        # Frames may land between the EOF read and this check; drain once more.
        guard_rc=0
        wait "$LEASE_ACQUIRE_PID" 2>/dev/null || guard_rc=$?
        LEASE_ACQUIRE_PID=''
        LEASE_ACQUIRE_GUARD_RC="$guard_rc"
        continue
      else
        sleep 0.1
        continue
      fi
    fi
    frame_rc=0
    lease_acquire_frame "$response" || frame_rc=$?
    [ "$frame_rc" = 2 ] && continue
    exec {fd}<&-
    return "$frame_rc"
  done
}

# Returns 0 for a terminal frame, 1 for an invalid reservation and 2 to keep reading.
lease_acquire_frame() {
  local response="$1"
  if [ "$(jq -r '.status // empty' <<<"$response")" = reserved ]; then
    parse_lease_reservation "$response" || { LEASE_ACQUIRE_FAILURE='invalid-reservation'; return 1; }
    [ "$LEASE_SLOT" = "$LEASE_REQUESTED_SLOT" ] || { LEASE_ACQUIRE_FAILURE='reservation-slot-mismatch'; return 1; }
    return 2
  fi
  ACQUIRE_RESPONSE="$response"
}

finish_acquire_session() {
  # Handoff needs the ack; a guard reaped while draining the grant could not send it.
  [ -n "$LEASE_ACQUIRE_PID" ] || return 1
  : > "$LEASE_ACQUIRE_ACK"
  local rc=0
  wait "$LEASE_ACQUIRE_PID" || rc=$?
  LEASE_ACQUIRE_PID=''
  return "$rc"
}

format_duration() {
  local seconds="$1"
  [ "$seconds" -ge 0 ] 2>/dev/null || seconds=0
  if [ "$seconds" -ge 3600 ]; then
    printf '%dh%02dm' "$((seconds / 3600))" "$(((seconds % 3600) / 60))"
  elif [ "$seconds" -ge 60 ]; then
    printf '%dm%02ds' "$((seconds / 60))" "$((seconds % 60))"
  else
    printf '%ds' "$seconds"
  fi
}

render_pool_status() {
  local response="$1" now slot state yard project run purpose acquired expires reference
  local reference_epoch age detail environment resources
  now="$(date -u +%s)"
  printf '%-8s %-12s %-16s %-32s %-10s %-24s %-8s %-14s %-14s %s\n' \
    SLOT STATE YARD PROJECT RUN PURPOSE AGE EXPIRES TYPE RESOURCES
  while IFS=$'\t' read -r slot state yard project run purpose acquired expires environment resources; do
    age=-
    detail=-
    if [ "$state" != available ]; then
      reference="$acquired"
      if [ "$reference" != "-" ]; then
        reference_epoch="$(date -u -d "$reference" +%s 2>/dev/null || true)"
        if [[ "$reference_epoch" =~ ^[0-9]+$ ]]; then
          age="$(format_duration "$((now - reference_epoch))")"
        fi
      fi
      if [ "$expires" != "-" ]; then
        reference_epoch="$(date -u -d "$expires" +%s 2>/dev/null || true)"
        if [[ "$reference_epoch" =~ ^[0-9]+$ ]]; then
          detail="in $(format_duration "$((reference_epoch - now))")"
        fi
      fi
    fi
    printf '%-8s %-12s %-16s %-32s %-10s %-24s %-8s %-14s %-14s %s\n' \
      "$slot" "$state" "$yard" "$project" "$run" "$purpose" "$age" "$detail" "$environment" "$resources"
  done < <(jq -r '
    .pool.slots[] |
    [
      .slot_id, .state,
      (if .state == "available" then "-" else (.yard // "unknown") end),
      (if .state == "available" then "-" else (.project // .display_label // "-") end),
      (if .state == "available" then "-" else (.run // "-") end),
      (if .state == "available" then "-" else (.purpose // "-") end),
      (if (.acquired_at // "") | startswith("0001-") then "-"
       else (.acquired_at // "-") end),
      (if (.expires_at // "") | startswith("0001-") then "-"
       else (.expires_at // "-") end),
      (.environment.type // "-"),
      (if .environment then
         "\(.environment.vm_count)x cpu=\(.environment.cpu_per_vm) ram=\(.environment.memory_per_vm) disk=\(.environment.disk_per_vm)"
       else "-" end)
    ] | @tsv
  ' <<<"$response")
  jq -r '
    select(.resources != null) | .resources |
    "Memory: available=\(.memory.available_bytes // "unknown") reserved=\(.reserved_vm_memory_bytes) bytes; L0=\(.outer_host_evidence)",
    "Storage: driver=\(.storage.driver // "unknown") physical_used=\(.storage.physical_used_bytes // "unknown") physical_free=\(.storage.physical_free_bytes // "unknown") virtual_reserved=\(.reserved_vm_virtual_disk_bytes) bytes",
    "Budgets: disk=\(if .budgets.disk_bytes == 0 then "unlimited" else (.budgets.disk_bytes | tostring) end) cache=\(.budgets.cache_bytes) bytes",
    (.bases[] | "Base: type=\(.type) fingerprint=\(.fingerprint) age=\(.age_seconds)s current=\(.current) expired=\(.expired)"),
    (if .builder then "Builder: type=\(.builder.type) reserved_memory=\(.builder.reserved_memory_bytes) reserved_disk_peak=\(.builder.reserved_disk_peak_bytes) bytes; observed_peak=\(.builder.observed_peak)" else empty end),
    (if .last_build_error then "Base failure: \(.last_build_error)" else empty end),
    (.errors[]? | "Telemetry gap: " + .)
  ' <<<"$response"

}

validate_exact_busy_response() {
  local response="$1"
  jq -e '
    def safe_label($maximum):
      type == "string" and length >= 1 and length <= $maximum and
      (startswith("/") | not) and (endswith("/") | not) and
      (contains("//") | not) and
      (split("/") | all(. != "." and . != "..")) and
      test("^[A-Za-z0-9._/+:-]+$");
    def safe_project:
      type == "string" and length >= 1 and length <= 50 and
      test("^[A-Za-z0-9._-]+$") and . != "." and . != ".." and
      (startswith("-") | not);
    def safe_time:
      type == "string" and
      test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$");
    .schema_version == 1 and .status == "error" and .code == "busy" and
    .message == "requested slot is unavailable" and
    if .state == "held" and .reason == "busy" then
      ((keys | sort) ==
        ["code", "message", "owner", "reason", "schema_version", "state", "status"]) and
      (.owner | type == "object") and
      ((.owner | keys | sort) ==
        ["acquired_at", "display_label", "expires_at", "project", "purpose", "run", "yard"]) and
      (.owner.display_label | type == "string" and length >= 1 and length <= 80) and
      (.owner.yard | type == "string" and length >= 1 and length <= 32 and
        test("^[a-z0-9][a-z0-9_-]*$")) and
      (.owner.project | safe_project) and
      (.owner.run | safe_label(24)) and
      (.owner.purpose | safe_label(80)) and
      (.owner.display_label == (.owner.project + "#" + .owner.run)) and
      (.owner.acquired_at | safe_time) and (.owner.expires_at | safe_time)
    else
      (((keys | sort) ==
         ["code", "message", "reason", "schema_version", "state", "status"]) and
       ((.state == "provisioning" and .reason == "provisioning") or
        (.state == "draining" and .reason == "draining") or
        (.state == "quarantined" and .reason == "quarantined") or
        (.state == "recovering" and .reason == "recovering") or
        (.state == "unavailable" and .reason == "unavailable")) and
       (has("owner") | not))
    end
  ' <<<"$response" >/dev/null 2>&1
}

report_exact_slot_wait() {
  local started="$1" state="$2" reason="$3" owner_summary="$4" elapsed remaining
  elapsed=$((SECONDS - started))
  remaining=$((WAIT_SECONDS - elapsed))
  [ "$remaining" -ge 0 ] || remaining=0
  printf '%s: requested slot %s unavailable: state=%s reason=%s; waited %s, deadline in %s\n' "$E2E_PROGRAM" \
    "$LEASE_REQUESTED_SLOT" "$state" "$reason" \
    "$(format_duration "$elapsed")" "$(format_duration "$remaining")" >&2
  [ -z "$owner_summary" ] || printf '%s: %s\n' "$E2E_PROGRAM" "$owner_summary" >&2
}

parse_lease_grant() {
  local response="$1" count selector name address key_type key_blob
  local response_yard response_project response_run response_purpose
  [ "$(jq -r '.status // empty' <<<"$response")" = ok ] || return 1
  LEASE_SLOT="$(jq -r '.grant.slot_id // empty' <<<"$response")"
  LEASE_GENERATION="$(jq -r '.grant.resource_generation // empty' <<<"$response")"
  LEASE_ID="$(jq -r '.grant.lease_id // empty' <<<"$response")"
  LEASE_EPOCH="$(jq -r '.grant.lease_epoch // empty' <<<"$response")"
  LEASE_CAPABILITY="$(jq -r '.grant.capability // empty' <<<"$response")"
  DATA_USER="$(jq -r '.grant.data_user // empty' <<<"$response")"
  case "$DATA_USER" in subyard-e2e-slot-[1-9]* ) ;; *) die "facade returned an invalid data user" ;; esac
  [[ "$LEASE_SLOT" =~ ^slot-[0-9]{3}$ && "$LEASE_ID" =~ ^[0-9a-f]+$ \
    && "$LEASE_EPOCH" =~ ^[1-9][0-9]*$ && "$LEASE_CAPABILITY" =~ ^[0-9a-f]+$ ]] \
    || die "facade returned invalid lease credentials"
  jq -e '
    (.grant.context | type == "object") and
    ((.grant.context | keys | sort) ==
      ["project", "purpose", "run", "schema_version", "yard"]) and
    .grant.context.schema_version == 2
  ' <<<"$response" >/dev/null 2>&1 \
    || die "facade returned invalid or missing v2 lease attribution"
  response_yard="$(jq -r '.grant.context.yard // empty' <<<"$response")"
  response_project="$(jq -r '.grant.context.project // empty' <<<"$response")"
  response_run="$(jq -r '.grant.context.run // empty' <<<"$response")"
  response_purpose="$(jq -r '.grant.context.purpose // empty' <<<"$response")"
  [ "$response_yard" = "$LEASE_YARD" ] \
    && [ "$response_project" = "$LEASE_PROJECT" ] \
    && [ "$response_run" = "$LEASE_RUN" ] \
    && [ "$response_purpose" = "$LEASE_PURPOSE" ] \
    || die "facade changed the requested lease attribution"
  jq -e --arg environment "$ENVIRONMENT_TYPE" --argjson count "$VM_COUNT" '
    (.grant.environment | keys | sort) ==
      ["cpu_per_vm", "disk_per_vm", "lifecycle", "memory_per_vm", "type", "vm_count"] and
    .grant.environment.type == $environment and .grant.environment.vm_count == $count and
    .grant.environment.lifecycle == "disposable-v1" and
    (.grant.environment.cpu_per_vm | type == "number" and . >= 1 and . == floor) and
    (.grant.environment.memory_per_vm | type == "string" and test("^[1-9][0-9]*(MiB|GiB|MB|GB)$")) and
    (.grant.environment.disk_per_vm | type == "string" and test("^[1-9][0-9]*(MiB|GiB|MB|GB)$")) and
    (.grant.base_fingerprint | type == "string" and test("^[a-f0-9]{64}$")) and
    (.grant.targets | type == "array" and length == $count)
  ' <<<"$response" >/dev/null 2>&1 \
    || die "facade returned an invalid disposable environment grant"
  GRANT_CPU_PER_VM="$(jq -r '.grant.environment.cpu_per_vm' <<<"$response")"
  GRANT_MEMORY_PER_VM="$(jq -r '.grant.environment.memory_per_vm' <<<"$response")"
  GRANT_DISK_PER_VM="$(jq -r '.grant.environment.disk_per_vm' <<<"$response")"
  [ -z "$DISK_REQUESTED" ] || [ "$GRANT_DISK_PER_VM" = "$DISK_REQUESTED" ] \
    || die "facade changed the requested disk size"
  BASE_FINGERPRINT="$(jq -r '.grant.base_fingerprint' <<<"$response")"
  count="$VM_COUNT"
  VM_IP=(); VM_HOST_KEY=()
  for ((selector=1; selector<=count; selector++)); do
    name="$(jq -r --argjson selector "$selector" \
      '.grant.targets[] | select(.selector == $selector) | .name' <<<"$response")"
    address="$(jq -r --argjson selector "$selector" \
      '.grant.targets[] | select(.selector == $selector) | .address' <<<"$response")"
    key_type="$(jq -r --argjson selector "$selector" \
      '.grant.targets[] | select(.selector == $selector) | .host_key_type' <<<"$response")"
    key_blob="$(jq -r --argjson selector "$selector" \
      '.grant.targets[] | select(.selector == $selector) | .host_key_blob' <<<"$response")"
    [[ "$name" =~ ^e2e-vm-[a-z0-9-]+$ ]] || die "facade returned an unsafe VM name"
    valid_ipv4 "$address" || die "facade returned an invalid VM address"
    [ "$key_type" = ssh-ed25519 ] && [[ "$key_blob" =~ ^[A-Za-z0-9+/=]+$ ]] \
      || die "facade returned an invalid VM host key"
    VM_IP[$selector]="$address"
    VM_HOST_KEY[$selector]="$key_type $key_blob"
  done
  for ((selector=1; selector<=count; selector++)); do
    printf 'e2e-vm-%s %s\n' "$selector" "${VM_HOST_KEY[$selector]}"
  done > "$GUEST_KNOWN_HOSTS"
  chmod 0600 "$GUEST_KNOWN_HOSTS"
}

resolve_lease_generation() {
  local response
  if [[ "$LEASE_GENERATION" =~ ^[1-9][0-9]*$ ]]; then
    return 0
  fi
  response="$(facade_request status)" || die "lease generation query failed"
  LEASE_GENERATION="$(jq -r --arg slot "$LEASE_SLOT" --argjson epoch "$LEASE_EPOCH" '
    .pool.slots[] |
    select(.slot_id == $slot and .lease_epoch == $epoch and .state == "held") |
    .resource_generation
  ' <<<"$response")"
  [[ "$LEASE_GENERATION" =~ ^[1-9][0-9]*$ ]] \
    || die "facade returned no resource generation for the held lease"
}

lease_grant_matches_request() {
  [ -z "$LEASE_REQUESTED_SLOT" ] || [ "$LEASE_SLOT" = "$LEASE_REQUESTED_SLOT" ]
}

capacity_refusal_snapshot() {
  # Render only the bounded numeric contract; arbitrary backend error text stays private.
  jq -ce '
    (.message | split("; admission=")) as $parts | select($parts | length == 2) |
    ($parts[1] | fromjson) as $snapshot |
    ["effective_available_bytes", "reserve_bytes", "pending_vm_bytes", "pending_builder_bytes",
     "headroom_bytes", "required_bytes", "visible_available_bytes", "physical_available_bytes",
     "cgroup_headroom_bytes"] as $numbers |
    ($numbers + ["physical_available_known", "cgroup_limit_available", "limiting_source",
                 "type", "vm_count", "slot_state"]) as $allowed |
    select($snapshot | type == "object") |
    select(($snapshot | keys) - $allowed | length == 0) |
    select(all($numbers[]; . as $key | $snapshot[$key] |
      type == "number" and . >= 0 and . <= 18446744073709551615 and floor == .)) |
    select($snapshot.physical_available_known | type == "boolean") |
    select($snapshot.cgroup_limit_available | type == "boolean") |
    select($snapshot.limiting_source == "visible_meminfo" or
           $snapshot.limiting_source == "visible_cgroup" or
           $snapshot.limiting_source == "verified_physical_meminfo") |
    select(($snapshot | has("type") | not) or
           $snapshot.type == "subyard-pair" or $snapshot.type == "android-test") |
    select(($snapshot | has("vm_count") | not) or
           $snapshot.vm_count == 1 or $snapshot.vm_count == 2) |
    select(($snapshot | has("slot_state") | not) or $snapshot.slot_state == "provisioning") |
    $snapshot
  ' <<<"$1" 2>/dev/null
}

acquire_lease() {
  resolve_vm_count
  local client fingerprint type blob response code reason state started last_report request
  local status_response workspace_context elapsed sleep_seconds capacity_wait attempt=0 last_state='' last_reason=''
  local owner_display='' owner_yard='' owner_project='' owner_run='' owner_purpose=''
  local owner_acquired='' owner_expires='' owner_acquired_epoch='' owner_expires_epoch=''
  local owner_summary='' last_owner_summary='' transport_rc guard_rc
  [ -n "$LEASE_REQUESTED_SLOT" ] \
    || die "an exact --slot is required before acquiring an E2E lease"
  command -v jq >/dev/null 2>&1 || die "jq is required"
  [ -n "$LOCAL_TEMP" ] || die "lease temporary directory is not initialized"
  ensure_identity
  CLIENT_CONFIG="$LOCAL_TEMP/ssh_config"
  GUEST_KNOWN_HOSTS="$LOCAL_TEMP/guest_known_hosts"
  resolve_bastion_route
  GUEST_IDENTITY="$LOCAL_TEMP/lease_id_ed25519"
  ssh-keygen -q -t ed25519 -N '' -C subyard-e2e-lease -f "$GUEST_IDENTITY"
  read -r type blob _ < "$GUEST_IDENTITY.pub"
  client="$(ensure_client_id)"
  fingerprint="$(ssh-keygen -lf "$IDENTITY.pub" -E sha256 | awk '{print $2}')"
  workspace_context="$(lease_attribution)"
  IFS=$'\t' read -r LEASE_YARD LEASE_PROJECT <<<"$workspace_context"
  [ -n "$LEASE_RUN" ] || LEASE_RUN="$(new_run_id)"
  [[ "$LEASE_RUN" =~ ^[0-9a-f]{8}$ ]] || die "invalid E2E run identity"
  [ -n "$LEASE_PURPOSE" ] || LEASE_PURPOSE=agent-e2e
  telemetry_correlation request
  if ! status_response="$(facade_request status)"; then
    die "broker capability probe failed"
  fi
  jq -e 'type == "object" and .schema_version == 1 and .status == "ok" and
    (.capabilities | type == "array" and index("attribution-v2") != null and
      index("environment-acquire-v3") != null and index("disposable-v1") != null)' \
    <<<"$status_response" >/dev/null \
    || die "broker does not support required attribution-v2/environment-acquire-v3/disposable-v1 acquire"
  if [ -n "$VM_COUNT_REQUESTED" ]; then
    jq -e '(.capabilities // []) | index("vm-count-v1") != null' <<<"$status_response" >/dev/null \
      || die 'broker does not support explicit VM counts (vm-count-v1)'
  fi
  if [ -n "$DISK_REQUESTED" ]; then
    jq -e '(.capabilities // []) | index("disk-size-v1") != null' <<<"$status_response" >/dev/null \
      || die 'broker does not support explicit disk size (disk-size-v1)'
  fi
  LEASE_ACQUIRE_PROTOCOL=acquire-v3
  if jq -e '(.capabilities // []) | index("process-acquire-v4") != null' <<<"$status_response" >/dev/null; then
    LEASE_ACQUIRE_PROTOCOL=acquire-v4
  fi
  # --wait bounds both busy and capacity retries. Without it, a collector may bound
  # capacity retries alone; a busy slot still fails at once.
  capacity_wait=$WAIT_SECONDS
  if [ "$capacity_wait" -eq 0 ] && [ -n "${SUBYARD_E2E_CAPACITY_WAIT:-}" ]; then
    [[ "$SUBYARD_E2E_CAPACITY_WAIT" =~ ^([0-9]{1,6})([sm])$ ]] || die 'SUBYARD_E2E_CAPACITY_WAIT must be a duration such as 30m or 90s'
    capacity_wait=$((10#${BASH_REMATCH[1]}))
    [ "${BASH_REMATCH[2]}" = s ] || capacity_wait=$((capacity_wait * 60))
  fi
  request="$(lease_acquire_request "$client" "$fingerprint" "$type" "$blob")"
  started=$SECONDS
  last_report=-30
  while true; do
    elapsed=$((SECONDS - started))
    if [ "$attempt" -gt 0 ] && [ "$last_state" != capacity ] && [ "$elapsed" -ge "$WAIT_SECONDS" ]; then
      printf '%s: timed out waiting for %s: state=%s reason=%s\n' "$E2E_PROGRAM" \
        "$LEASE_REQUESTED_SLOT" "$last_state" "$last_reason" >&2
      [ -z "$last_owner_summary" ] || printf '%s: %s\n' "$E2E_PROGRAM" "$last_owner_summary" >&2
      return 4
    fi
    attempt=$((attempt + 1))
    if [ "$LEASE_ACQUIRE_PROTOCOL" = acquire-v4 ]; then
      lease_acquire_stream "$request" || die_acquire_unknown "${LEASE_ACQUIRE_FAILURE:-stream-failed}"
      response="$ACQUIRE_RESPONSE"
    else
      transport_rc=0
      response="$(facade_request "$request")" || transport_rc=$?
      [ "$transport_rc" = 0 ] || die_acquire_unknown "transport-rc=$transport_rc"
    fi
    jq -e 'type == "object" and .schema_version == 1 and
      (.status == "ok" or .status == "error")' \
      <<<"$response" >/dev/null 2>&1 \
      || die_acquire_unknown invalid-envelope
    code="$(jq -r '.code // empty' <<<"$response")"
    if parse_lease_grant "$response"; then
      elapsed=$((SECONDS - started))
      if [ "$WAIT_SECONDS" -gt 0 ] && [ "$elapsed" -ge "$WAIT_SECONDS" ]; then
        printf '%s: deadline elapsed before the lease grant arrived; releasing %s without guest access\n' "$E2E_PROGRAM" \
          "$LEASE_REQUESTED_SLOT" >&2
        release_lease \
          || die "late lease grant could not be released safely"
        return 4
      fi
      if ! lease_grant_matches_request; then
        printf '%s: broker granted %s instead of requested %s; releasing without guest access\n' "$E2E_PROGRAM" \
          "$LEASE_SLOT" "$LEASE_REQUESTED_SLOT" >&2
        release_lease \
          || die "broker returned the wrong slot and its lease could not be released"
        die "broker returned a slot other than the exact requested slot"
      fi
      resolve_lease_generation
      telemetry_correlation held
      write_client_config
      if [ "$LEASE_ACQUIRE_PROTOCOL" = acquire-v4 ]; then
        start_lease_keeper
        finish_acquire_session || die "lease acquire handoff is unknown; cleanup required"
      fi
      printf 'E2E lease: yard=%s project=%s run=%s purpose=%s slot=%s' \
        "$LEASE_YARD" "$LEASE_PROJECT" "$LEASE_RUN" "$LEASE_PURPOSE" "$LEASE_SLOT" >&2
      printf ' type=%s vms=%s base=%s\n' "$ENVIRONMENT_TYPE" "$VM_COUNT" "$BASE_FINGERPRINT" >&2
      return 0
    fi
    if [ -n "$LEASE_ACQUIRE_PID" ]; then
      guard_rc=0
      wait "$LEASE_ACQUIRE_PID" || guard_rc=$?
      [ "$guard_rc" = 0 ] || die_acquire_unknown "refusal-guard-rc=$guard_rc"
      LEASE_ACQUIRE_PID=''
    elif [ "${LEASE_ACQUIRE_GUARD_RC:-0}" != 0 ]; then
      die_acquire_unknown "refusal-guard-rc=$LEASE_ACQUIRE_GUARD_RC"
    fi
    reason="$(jq -r '.reason // empty' <<<"$response")"
    state="$(jq -r '.state // empty' <<<"$response")"
    if [ "$code" = capacity ]; then
      jq -e '
        (keys | sort) == ["code", "message", "reason", "schema_version", "status"] and
        .schema_version == 1 and .status == "error" and .code == "capacity" and
        (.reason == "memory" or .reason == "disk") and
        (.message | type == "string" and length > 0 and length <= 1024 and test("^[^\\x00-\\x1f\\x7f]+$"))
      ' <<<"$response" >/dev/null 2>&1 \
        || die_acquire_unknown invalid-capacity-refusal
      # A validated capacity refusal confirms the broker aborted before creating guests.
      [ "$LEASE_ACQUIRE_PROTOCOL" != acquire-v4 ] || LEASE_ID=''
      elapsed=$((SECONDS - started))
      # Retry only while another request can still start before the deadline, so a
      # grant never arrives after it and the final refusal stays typed.
      if [ $((capacity_wait - elapsed)) -gt 1 ]; then
        last_state=capacity
        last_reason="$reason"
        last_owner_summary=''
        if [ "$((SECONDS - last_report))" -ge 30 ]; then
          printf '%s: waiting for %s capacity for %s; waited %s, deadline in %s\n' "$E2E_PROGRAM" \
            "$reason" "$LEASE_REQUESTED_SLOT" "$(format_duration "$elapsed")" \
            "$(format_duration $((capacity_wait - elapsed)))" >&2
          last_report=$SECONDS
        fi
        sleep_seconds=$((capacity_wait - elapsed - 1))
        [ "$sleep_seconds" -le 5 ] || sleep_seconds=5
        sleep "$sleep_seconds"
        continue
      fi
      printf '%s: retryable capacity refusal for %s: resource=%s; retry after resources become available\n' "$E2E_PROGRAM" \
        "$LEASE_REQUESTED_SLOT" "$reason" >&2
      local admission_snapshot
      if [ "$reason" = memory ] && admission_snapshot="$(capacity_refusal_snapshot "$response")"; then
        printf '%s: memory admission=%s\n' "$E2E_PROGRAM" "$admission_snapshot" >&2
      fi
      return 4
    fi
    if [ "$code" != busy ]; then
      reason="$(jq -r '.reason // .message // empty' <<<"$response")"
      die "lease acquire failed (${code:-invalid_response}: ${reason:-unspecified})"
    fi
    validate_exact_busy_response "$response" \
      || die_acquire_unknown invalid-busy-refusal
    owner_summary=''
    if [ "$state" = held ]; then
      owner_display="$(jq -r '.owner.display_label' <<<"$response")"
      owner_yard="$(jq -r '.owner.yard' <<<"$response")"
      owner_project="$(jq -r '.owner.project' <<<"$response")"
      owner_run="$(jq -r '.owner.run' <<<"$response")"
      owner_purpose="$(jq -r '.owner.purpose' <<<"$response")"
      owner_acquired="$(jq -r '.owner.acquired_at' <<<"$response")"
      owner_expires="$(jq -r '.owner.expires_at' <<<"$response")"
      owner_acquired_epoch="$(date -u -d "$owner_acquired" +%s 2>/dev/null)" \
        || die_acquire_unknown invalid-busy-owner-acquired
      owner_expires_epoch="$(date -u -d "$owner_expires" +%s 2>/dev/null)" \
        || die_acquire_unknown invalid-busy-owner-expires
      [ "$owner_acquired_epoch" -le "$owner_expires_epoch" ] \
        || die_acquire_unknown invalid-busy-owner-window
      owner_summary="owner=$owner_yard/$owner_project run=$owner_run purpose=$owner_purpose acquired=$owner_acquired expires=$owner_expires label=$owner_display"
    fi
    last_state="$state"
    last_reason="$reason"
    last_owner_summary="$owner_summary"
    if [ "$WAIT_SECONDS" -eq 0 ]; then
      die "requested E2E slot $LEASE_REQUESTED_SLOT is unavailable: state=$state reason=$reason${owner_summary:+; $owner_summary}"
    fi
    elapsed=$((SECONDS - started))
    if [ "$elapsed" -ge "$WAIT_SECONDS" ]; then
      printf '%s: timed out waiting for %s: state=%s reason=%s\n' "$E2E_PROGRAM" \
        "$LEASE_REQUESTED_SLOT" "$state" "$reason" >&2
      [ -z "$owner_summary" ] || printf '%s: %s\n' "$E2E_PROGRAM" "$owner_summary" >&2
      return 4
    fi
    if [ "$((SECONDS - last_report))" -ge 30 ]; then
      report_exact_slot_wait "$started" "$state" "$reason" "$owner_summary"
      last_report=$SECONDS
    fi
    sleep_seconds=$((WAIT_SECONDS - elapsed))
    [ "$sleep_seconds" -le 5 ] || sleep_seconds=5
    sleep "$sleep_seconds"
  done
}

lease_command() {
  printf '%s %s %s %s %s\n' "$1" "$LEASE_SLOT" "$LEASE_ID" "$LEASE_EPOCH" "$LEASE_CAPABILITY"
}

release_lease() {
  local released_slot="$LEASE_SLOT" released_run="$LEASE_RUN" response code started=$SECONDS rc=0
  [ -n "$LEASE_ID" ] || return 0
  printf 'E2E_RELEASE state=start duration_seconds=0 budget_seconds=180 grace_seconds=5 exit_code=0\n' >&2 || true
  if response="$(facade_request "$(lease_command release)" 180)"; then :; else
    rc=$?
    printf 'E2E_RELEASE state=end duration_seconds=%s budget_seconds=180 grace_seconds=5 exit_code=%s\n' "$((SECONDS - started))" "$rc" >&2 || true
    printf '%s: release failed for slot=%s run=%s\n' "$E2E_PROGRAM" \
      "$released_slot" "$released_run" >&2
    return 1
  fi
  if [ "$(jq -r '.status // empty' <<<"$response")" != ok ]; then
    printf 'E2E_RELEASE state=end duration_seconds=%s budget_seconds=180 grace_seconds=5 exit_code=1\n' "$((SECONDS - started))" >&2 || true
    code="$(jq -r '.code // "invalid_response"' <<<"$response")"
    printf '%s: release rejected for slot=%s run=%s (%s)\n' "$E2E_PROGRAM" \
      "$released_slot" "$released_run" "$code" >&2
    return 1
  fi
  LEASE_ID=""
  printf 'E2E_RELEASE state=end duration_seconds=%s budget_seconds=180 grace_seconds=5 exit_code=0\n' "$((SECONDS - started))" >&2 || true
  printf 'E2E lease released: slot=%s run=%s\n' "$released_slot" "$released_run" >&2
}

lease_process_identity() {
  local stat
  local -a fields=()
  [[ "$1" =~ ^[1-9][0-9]*$ ]] || return 1
  IFS= read -r stat 2>/dev/null < "/proc/$1/stat" || return 1
  # The command name may contain spaces and ')'; fields after its last ')' are fixed.
  read -r -a fields <<<"${stat##*) }"
  [ "${fields[0]:-Z}" != Z ] && [ "${fields[0]:-X}" != X ] \
    && [[ "${fields[19]:-}" =~ ^[0-9]+$ && "${fields[1]:-}" =~ ^[0-9]+$ ]] || return 1
  printf '%s:%s\n' "${fields[19]}" "${fields[1]}"
}

lease_keeper_owner_alive() {
  [ "$(lease_process_identity "$owner_pid")" = "$owner_identity" ] \
    && [ "$(lease_process_identity "$requester_pid")" = "$requester_identity" ]
}

lease_keeper_wait() {
  while kill -0 "$keeper_job_pid" >/dev/null 2>&1; do
    lease_keeper_owner_alive || return 1
    sleep 1 &
    keeper_timer_pid=$!
    wait "$keeper_timer_pid" 2>/dev/null || return 1
    keeper_timer_pid=''
  done
  lease_keeper_owner_alive
}

lease_keeper() {
  local owner_pid="$1" owner_identity="$2" requester_pid="$3" requester_identity="$4"
  local response response_file keeper_timer_pid='' keeper_job_pid='' renew_rc=0
  # Separate background process groups let cleanup close every transport descendant's streams.
  set -m
  response_file="$(mktemp "${LOCAL_TEMP:-/tmp}/.subyard-lease-renew.XXXXXX")"
  trap 'lease_keeper_stop_jobs; rm -f "$response_file"' EXIT
  trap 'exit 0' INT TERM
  # Exit the background child directly, keeping local PIDs in scope for its EXIT trap.
  while true; do
    lease_keeper_owner_alive || exit 0
    sleep 300 &
    keeper_job_pid=$!
    lease_keeper_wait || exit 0
    wait "$keeper_job_pid" 2>/dev/null || exit 0
    keeper_job_pid=''
    lease_keeper_owner_alive || exit 0
    # SSH toggles inherited stdio flags; renewal must not make payload output nonblocking.
    (set +m; facade_request "$(lease_command renew)" || exit "$?") > "$response_file" 2>/dev/null &
    keeper_job_pid=$!
    lease_keeper_wait || exit 0
    renew_rc=0
    wait "$keeper_job_pid" 2>/dev/null || renew_rc=$?
    keeper_job_pid=''
    response="$(cat "$response_file")"
    if [ "$renew_rc" != 0 ] ||
      [ "$(jq -r '.status // empty' <<<"$response")" != ok ]; then
      [ -z "$LEASE_KEEPER_LOG" ] \
        || printf '%s\tfailure\n' "$(date +%s)" >> "$LEASE_KEEPER_LOG"
      printf '%s: lease lost; stopping payload transport\n' "$E2E_PROGRAM" >&2
      lease_keeper_owner_alive && kill -TERM "$owner_pid" >/dev/null 2>&1 || true
      exit 1
    fi
    [ -z "$LEASE_KEEPER_LOG" ] \
      || printf '%s\tok\n' "$(date +%s)" >> "$LEASE_KEEPER_LOG"
  done
}

start_lease_keeper() {
  [ -z "$LEASE_KEEPER_PID" ] || return 0
  if [ "$LEASE_KEEPER_MODE" = handle ]; then
    start_handle_keeper
    return
  fi
  local owner_pid="$BASHPID" owner_identity requester_pid requester_identity
  owner_identity="$(lease_process_identity "$owner_pid")" \
    || die 'cannot identify lease-owning wrapper'
  requester_pid="${owner_identity#*:}"
  [ "$requester_pid" -gt 1 ] \
    && requester_identity="$(lease_process_identity "$requester_pid")" \
    || die 'cannot identify lease requester'
  if [ -n "$LOCAL_TEMP" ]; then
    LEASE_KEEPER_LOG="$LOCAL_TEMP/lease-keeper.tsv"
    : > "$LEASE_KEEPER_LOG"
    chmod 0600 "$LEASE_KEEPER_LOG"
    printf '%s\tstarted\n' "$(date +%s)" >> "$LEASE_KEEPER_LOG"
  fi
  lease_keeper "$owner_pid" "$owner_identity" "$requester_pid" "$requester_identity" &
  LEASE_KEEPER_PID=$!
}

write_client_config() {
  local temp
  temp="$(mktemp "$STATE_ROOT/.ssh-config.XXXXXX")"
  render_client_config > "$temp"
  chmod 0600 "$temp"
  mv -f "$temp" "$CLIENT_CONFIG"
}

valid_ipv4() {
  local address="$1" octet
  local -a octets=()
  IFS=. read -r -a octets <<<"$address"
  [ "${#octets[@]}" -eq 4 ] || return 1
  for octet in "${octets[@]}"; do
    [[ "$octet" =~ ^[0-9]+$ ]] && [ "$octet" -le 255 ] || return 1
  done
}

verify_boundary() {
  [ "$ENVIRONMENT_TYPE" = subyard-pair ] || die "--verify-boundary requires subyard-pair"
  local before after output vm pty_log transfer_log probe expected_hash actual_hash rc
  local -a requested=()
  before="$(facade_request status | jq -c --arg slot "$LEASE_SLOT" '
    [.pool.slots[] | select(.slot_id == $slot) |
      {slot_id, resource_generation, lease_epoch, state, display_label, purpose}]
  ')" || die "cannot read lease pool status"
  [ "$(jq 'length' <<<"$before")" = 1 ] \
    || die "current lease slot is absent from pool status"

  for probe in id 'sudo -n id' 'incus list' \
    'cat /var/lib/subyard/test-vms/worker-key' \
    '/usr/local/libexec/subyard/test-vms-worker arbitrary-command'; do
    read -r -a requested <<<"$probe"
    output="$(ssh -F "$CLIENT_CONFIG" -T subyard-e2e-bastion -- "${requested[@]}" </dev/null)" \
      || die "forced facade probe failed"
    [ "$(jq -r '.code // empty' <<<"$output")" = invalid_request ] \
      || { printf '%s: bastion did not reject a requested L1 command\n' "$E2E_PROGRAM" >&2; return 1; }
  done

  pty_log="$(mktemp "$STATE_ROOT/.pty-probe.XXXXXX")"
  LC_ALL=C ssh -vv -F "$CLIENT_CONFIG" -tt subyard-e2e-bastion -- id </dev/null \
    > /dev/null 2> "$pty_log" || true
  grep -Fq 'PTY allocation request failed' "$pty_log" \
    || { rm -f "$pty_log"; printf '%s: bastion did not reject a PTY request\n' "$E2E_PROGRAM" >&2; return 1; }
  rm -f "$pty_log"

  for probe in sftp scp; do command -v "$probe" >/dev/null 2>&1 || die "$probe is required"; done
  transfer_log="$(mktemp "$STATE_ROOT/.transfer-probe.XXXXXX")"
  if sftp -F "$CLIENT_CONFIG" -b /dev/null subyard-e2e-bastion \
    </dev/null > /dev/null 2> "$transfer_log"; then
    rm -f "$transfer_log"
    printf '%s: bastion allowed SFTP\n' "$E2E_PROGRAM" >&2
    return 1
  fi
  probe="$(mktemp "$STATE_ROOT/.scp-probe.XXXXXX")"
  printf 'synthetic transfer probe\n' > "$probe"
  if scp -F "$CLIENT_CONFIG" -q "$probe" subyard-e2e-bastion:/tmp/subyard-e2e-agent-forbidden \
    </dev/null > /dev/null 2> "$transfer_log"; then
    rm -f "$probe" "$transfer_log"
    printf '%s: bastion allowed SCP\n' "$E2E_PROGRAM" >&2
    return 1
  fi
  rm -f "$probe" "$transfer_log"

  if ssh -F "$CLIENT_CONFIG" -T -o User=dev subyard-e2e-bastion -- true \
    </dev/null >/dev/null 2>&1; then
    printf '%s: agent identity unexpectedly logged in as L1 dev\n' "$E2E_PROGRAM" >&2
    return 1
  fi
  if ssh -F "$CLIENT_CONFIG" -T -W 127.0.0.1:22 subyard-e2e-bastion \
    </dev/null >/dev/null 2>&1; then
    printf '%s: bastion allowed an unlisted forwarding target\n' "$E2E_PROGRAM" >&2
    return 1
  fi

  for ((vm=1; vm<=VM_COUNT; vm++)); do
    [ "$(guest "$vm" sudo -n id -u </dev/null)" = 0 ] \
      || { printf '%s: VM%s direct SSH/passwordless sudo failed\n' "$E2E_PROGRAM" "$vm" >&2; return 1; }
    expected_hash="$(printf '\0subyard-binary-stdin\377' | sha256sum | awk '{print $1}')"
    actual_hash="$(printf '\0subyard-binary-stdin\377' | guest "$vm" sha256sum | awk '{print $1}')"
    [ "$actual_hash" = "$expected_hash" ] \
      || { printf '%s: VM%s binary stdin changed in transport\n' "$E2E_PROGRAM" "$vm" >&2; return 1; }
    set +e
    printf 'exit 23\n' | guest "$vm" /bin/sh -s >/dev/null 2>&1
    rc=$?
    set -e
    [ "$rc" = 23 ] \
      || { printf '%s: VM%s SSH exit status changed (%s)\n' "$E2E_PROGRAM" "$vm" "$rc" >&2; return 1; }
    if printf '%s\n' \
      'set -eu' \
      'if timeout 3 bash -c "</dev/tcp/$1/22" 2>/dev/null; then exit 42; fi' \
      | ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- sudo -n bash -s -- "$BASTION_HOSTNAME"; then
      :
    else
      case "$?" in
        42) printf '%s: VM%s root can reach L1 SSH management\n' "$E2E_PROGRAM" "$vm" >&2; return 1 ;;
        *) printf '%s: VM%s L1-isolation probe failed unexpectedly\n' "$E2E_PROGRAM" "$vm" >&2; return 1 ;;
      esac
    fi
  done

  after="$(facade_request status | jq -c --arg slot "$LEASE_SLOT" '
    [.pool.slots[] | select(.slot_id == $slot) |
      {slot_id, resource_generation, lease_epoch, state, display_label, purpose}]
  ')" || die "post-probe lease pool status failed"
  [ "$after" = "$before" ] \
    || { printf '%s: boundary probes changed allocation state\n' "$E2E_PROGRAM" >&2; return 1; }
  ok "VM SSH works; L1 commands, PTY, transfers, dev login, forwarding and guest return paths are blocked"
}

guest() {
  local vm="$1" command; shift
  command="$(quote_ssh_command "$@")"
  if [ -n "${E2E_GUEST_TIMEOUT:-}" ]; then
    timeout --signal=TERM --kill-after=5 "$E2E_GUEST_TIMEOUT" ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$command"
  else
    ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$command"
  fi
}

quote_ssh_command() {
  local argument quoted command=''
  [ "$#" -gt 0 ] || return 1
  for argument in "$@"; do
    printf -v quoted '%q' "$argument"
    command+="${command:+ }$quoted"
  done
  printf '%s\n' "$command"
}

set_requested_slot() {
  local value="$1" source="${2:---slot}"
  [[ "$value" =~ ^[1-9][0-9]{0,2}$ ]] \
    || die "$source needs a number from 1 to 999"
  printf -v LEASE_REQUESTED_SLOT 'slot-%03d' "$((10#$value))"
}

# Resolve the trusted owner publication unless an explicit rollout route was selected.
resolve_service_selection() {
  local mode="$1" service_route _runtime_uuid route_generation
  # A host whose installed release predates service registration has no owner route yet.
  if [ "$E2E_LEGACY_ROUTE" = 0 ] && [ -n "${SUBYARD_E2E_ROLLOUT_YARD:-}" ] \
    && [ -z "$E2E_OWNER" ] && [ -z "$E2E_POOL" ]; then
    E2E_YARD="$SUBYARD_E2E_ROLLOUT_YARD"
    E2E_LEGACY_ROUTE=1
  fi
  if [ "$E2E_LEGACY_ROUTE" = 0 ] && [ "$mode" != prepare ]; then
    service_route="$(python3 "$CLIENT_DIR/service-route.py" \
      --registry "${SUBYARD_E2E_ROUTE_REGISTRY:-/var/lib/subyard/e2e-routes}" \
      --owner "$E2E_OWNER" --pool "$E2E_POOL")" || die 'test environment unavailable: owner service resolution failed'
    IFS=$'\t' read -r E2E_OWNER E2E_POOL E2E_YARD E2E_SERVICE_GENERATION _runtime_uuid route_generation <<< "$service_route"
    [[ "$route_generation" =~ ^\.route-[A-Za-z0-9_-]+$ ]] || die 'test environment unavailable: unsafe service route generation'
    E2E_CAPTURED_ROUTE_DIRECTORY="${SUBYARD_E2E_ROUTE_REGISTRY:-/var/lib/subyard/e2e-routes}/$E2E_YARD/$route_generation"
  elif [ "$E2E_LEGACY_ROUTE" = 1 ] && { [ -n "$E2E_OWNER" ] || [ -n "$E2E_POOL" ]; }; then
    die '--yard cannot be combined with --owner or --pool'
  fi
}

# Attribution without a Subyard checkout: the yard name from the installed guest facts
# and an explicit or yard-named project label. These are display fields only.
client_attribution() {
  local facts=/etc/subyard/guest-runtime.json yard=unknown name project="$CLIENT_PROJECT"
  if [ "${SUBYARD_E2E_TEST_MODE:-}" = 1 ] && [ -n "${SUBYARD_E2E_GUEST_FACTS:-}" ]; then
    facts="$SUBYARD_E2E_GUEST_FACTS"
  fi
  if [ -f "$facts" ] && [ -r "$facts" ] \
    && name="$(jq -r '.target.yard.name // empty' "$facts" 2>/dev/null)" \
    && safe_attribution_yard "$name"; then
    yard="$name"
  fi
  [ -n "$project" ] || project="$yard"
  safe_project_name "$project" || die "--project must be a safe project label"
  printf '%s\t%s\n' "$yard" "$project"
}

parse_client_duration() {
  local value="$1" option="$2" number
  [[ "$value" =~ ^([0-9]{1,6})([smh])$ ]] || die "$option needs a duration such as 90s, 20m or 2h"
  number=$((10#${BASH_REMATCH[1]}))
  case "${BASH_REMATCH[2]}" in m) number=$((number * 60)) ;; h) number=$((number * 3600)) ;; esac
  printf '%s\n' "$number"
}

# --- Session lease handles ------------------------------------------------------
# A handle keeps one lease across separate client invocations. Its private directory
# holds the captured route, lease credentials and ephemeral guest key. A detached
# keeper renews only while that exact handle exists and was used recently.

HANDLE_DIRECTORY=""

handle_base() { printf '%s\n' "$STATE_BASE/handles"; }

handle_directory() {
  local handle="$1"
  [[ "$handle" =~ ^[0-9a-f]{8}$ ]] || die "invalid lease handle '$handle'"
  printf '%s/%s\n' "$(handle_base)" "$handle"
}

# Keeper timing: renewal cadence, transport retry pause, and how long transport
# failures may be retried after the last successful renewal (below the broker's
# 20-minute expiry). Tests shorten them only in test mode.
handle_timing() {
  local name="$1" value="$2" override
  case "$name" in
    renew) override="${SUBYARD_E2E_TEST_RENEW_SECONDS:-}" ;;
    retry) override="${SUBYARD_E2E_TEST_RENEW_RETRY_SECONDS:-}" ;;
    stale) override="${SUBYARD_E2E_TEST_RENEW_STALE_SECONDS:-}" ;;
  esac
  if [ "${SUBYARD_E2E_TEST_MODE:-}" = 1 ] && [[ "$override" =~ ^[1-9][0-9]{0,4}$ ]]; then
    value="$override"
  fi
  printf '%s\n' "$value"
}

# A failed idle release is retried this many times before the keeper gives up the
# lease to broker expiry.
HANDLE_IDLE_RELEASE_ATTEMPTS=3

handle_mark_lost() {
  printf '%s\t%s\n' "$(date +%s)" "$2" >> "$1/keeper.log"
  date -u +%Y-%m-%dT%H:%M:%SZ > "$1/lost"
}

handle_lease_identity() { stat -c '%d:%i' "$1/lease.json" 2>/dev/null; }

write_handle_state() {
  local dir="$HANDLE_DIRECTORY" temp
  # The handle keeps its own pin; a later publication cannot change this lease's route.
  cat -- "$BASTION_KNOWN_HOSTS" > "$dir/bastion_known_hosts"
  chmod 0600 "$dir/bastion_known_hosts"
  BASTION_KNOWN_HOSTS="$dir/bastion_known_hosts"
  write_client_config
  : > "$dir/activity"
  : > "$dir/active.lock"
  : > "$dir/release.lock"
  : > "$dir/keeper.lock"
  temp="$(mktemp "$dir/.lease.XXXXXX")"
  jq -n \
    --arg handle "$LEASE_RUN" --arg slot "$LEASE_SLOT" --arg generation "$LEASE_GENERATION" \
    --arg lease_id "$LEASE_ID" --arg epoch "$LEASE_EPOCH" --arg capability "$LEASE_CAPABILITY" \
    --arg yard "$LEASE_YARD" --arg project "$LEASE_PROJECT" --arg purpose "$LEASE_PURPOSE" \
    --arg type "$ENVIRONMENT_TYPE" --argjson vm_count "$VM_COUNT" \
    --arg cpu "$GRANT_CPU_PER_VM" --arg memory "$GRANT_MEMORY_PER_VM" --arg disk "$GRANT_DISK_PER_VM" \
    --arg base "$BASE_FINGERPRINT" --argjson idle "$HANDLE_IDLE_SECONDS" \
    --arg acquired "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg owner "$E2E_OWNER" --arg pool "$E2E_POOL" --arg route_yard "$E2E_YARD" \
    --arg service_generation "$E2E_SERVICE_GENERATION" --arg state_root "$STATE_ROOT" \
    --arg identity "$IDENTITY" --arg hostname "$BASTION_HOSTNAME" --arg port "$BASTION_PORT" \
    --arg alias "$BASTION_HOST_KEY_ALIAS" --arg user "$BASTION_USER" --arg data_user "$DATA_USER" \
    '{schema_version:1, handle:$handle, slot:$slot, resource_generation:$generation,
      lease_id:$lease_id, lease_epoch:$epoch, capability:$capability, yard:$yard, project:$project,
      purpose:$purpose, type:$type, vm_count:$vm_count, cpu_per_vm:$cpu, memory_per_vm:$memory,
      disk_per_vm:$disk, base_fingerprint:$base, idle_seconds:$idle, acquired_at:$acquired,
      route:{owner:$owner, pool:$pool, yard:$route_yard, service_generation:$service_generation,
        state_root:$state_root, identity:$identity, hostname:$hostname, port:$port,
        host_key_alias:$alias, user:$user, data_user:$data_user}}' > "$temp"
  chmod 0600 "$temp"
  mv -f -- "$temp" "$dir/lease.json"
}

load_handle() {
  local dir="$1" fields
  [ -d "$dir" ] && [ ! -L "$dir" ] && [ -f "$dir/lease.json" ] && [ ! -L "$dir/lease.json" ] || return 1
  # The unit separator keeps empty fields, such as an unpublished owner, in place.
  fields="$(jq -r 'select(.schema_version == 1 and (.vm_count == 1 or .vm_count == 2) and
      (.idle_seconds | type == "number" and . >= 1)) |
    [.handle, .slot, .resource_generation, .lease_id, .lease_epoch, .capability, .yard,
     .project, .purpose, .type, (.vm_count | tostring), (.idle_seconds | tostring),
     .route.owner, .route.pool, .route.yard, .route.service_generation, .route.state_root,
     .route.identity, .route.hostname, .route.port, .route.host_key_alias, .route.user,
     .route.data_user, "end"] | map(tostring) | join("\u001f")' "$dir/lease.json" 2>/dev/null)" || return 1
  local end=''
  IFS=$'\x1f' read -r LEASE_RUN LEASE_SLOT LEASE_GENERATION LEASE_ID LEASE_EPOCH LEASE_CAPABILITY \
    LEASE_YARD LEASE_PROJECT LEASE_PURPOSE ENVIRONMENT_TYPE VM_COUNT HANDLE_IDLE_SECONDS \
    E2E_OWNER E2E_POOL E2E_YARD E2E_SERVICE_GENERATION STATE_ROOT IDENTITY BASTION_HOSTNAME \
    BASTION_PORT BASTION_HOST_KEY_ALIAS BASTION_USER DATA_USER end <<<"$fields"
  [ "$end" = end ] || return 1
  [[ "$LEASE_SLOT" =~ ^slot-[0-9]{3}$ && "$LEASE_ID" =~ ^[0-9a-f]+$ \
    && "$LEASE_EPOCH" =~ ^[1-9][0-9]*$ && "$LEASE_CAPABILITY" =~ ^[0-9a-f]+$ \
    && "$LEASE_RUN" = "${dir##*/}" ]] || return 1
  HANDLE_DIRECTORY="$dir"
  LOCAL_TEMP="$dir"
  E2E_ROUTE_CAPTURED=1
  BASTION_KNOWN_HOSTS="$dir/bastion_known_hosts"
  GUEST_IDENTITY="$dir/lease_id_ed25519"
  GUEST_KNOWN_HOSTS="$dir/guest_known_hosts"
  CLIENT_CONFIG="$dir/ssh_config"
}

open_handle() {
  local handle="$1" dir
  dir="$(handle_directory "$handle")"
  load_handle "$dir" || die "no active lease handle '$handle'; list handles with: leases"
}

# Exit 0 while the recorded keeper process, identified by PID and start time, lives.
handle_keeper_running() {
  local pid start identity
  read -r pid start < "$1/keeper.ready" 2>/dev/null || return 1
  identity="$(lease_process_identity "$pid")" || return 1
  [ "${identity%%:*}" = "$start" ]
}

remove_handle_directory() {
  local dir="$1"
  case "$dir" in "$(handle_base)"/[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;; *) return 1 ;; esac
  [ ! -L "$dir" ] || return 1
  # Retire the credentials first: a keeper treats a missing lease file as the end of its lease.
  rm -f -- "$dir/lease.json"
  find "$dir" -depth -delete 2>/dev/null
}

# Release the loaded handle's lease once. Returns 0 when the broker released it or it
# had already ended; the handle is then removed.
release_handle() {
  local dir="$HANDLE_DIRECTORY" reason="$1" fd response code started=$SECONDS release_rc=0
  exec {fd}>>"$dir/release.lock" || return 1
  flock -x "$fd"
  if [ ! -f "$dir/lease.json" ]; then
    exec {fd}>&-
    return 0
  fi
  # The same bounded release evidence as release_lease; only a confirmed release reports 0.
  printf 'E2E_RELEASE state=start duration_seconds=0 budget_seconds=180 grace_seconds=5 exit_code=0\n' >&2 || true
  if response="$(facade_request "$(lease_command release)" 180)"; then
    code="$(jq -r 'if .status == "ok" then "ok" else (.code // "invalid_response") end' <<<"$response" 2>/dev/null || printf invalid_response)"
    [ "$code" = ok ] || release_rc=1
  else
    release_rc=$?
    code=''
  fi
  printf 'E2E_RELEASE state=end duration_seconds=%s budget_seconds=180 grace_seconds=5 exit_code=%s\n' \
    "$((SECONDS - started))" "$release_rc" >&2 || true
  if [ -z "$code" ]; then
    exec {fd}>&-
    printf '%s: release failed for handle=%s slot=%s; the handle is kept, retry release\n' \
      "$E2E_PROGRAM" "$LEASE_RUN" "$LEASE_SLOT" >&2
    return 1
  fi
  case "$code" in
    ok) printf 'E2E lease released: handle=%s slot=%s reason=%s\n' "$LEASE_RUN" "$LEASE_SLOT" "$reason" >&2 ;;
    lease_lost) printf 'E2E lease had already ended: handle=%s slot=%s\n' "$LEASE_RUN" "$LEASE_SLOT" >&2 ;;
    *)
      exec {fd}>&-
      printf '%s: release rejected for handle=%s slot=%s (%s)\n' "$E2E_PROGRAM" "$LEASE_RUN" "$LEASE_SLOT" "$code" >&2
      return 1
      ;;
  esac
  remove_handle_directory "$dir" || true
  exec {fd}>&-
}

# Ownership of one renewal: this exact handle and its live keeper.
handle_renew_owned() {
  local dir="$1" identity="$2" keeper="$3" start="$4" current
  [ "$(handle_lease_identity "$dir")" = "$identity" ] || return 1
  current="$(lease_process_identity "$keeper")" || return 1
  [ "${current%%:*}" = "$start" ]
}

# One renewal transport, run as its own process group. It checks ownership right before
# sending and, while in flight, kills its whole group when the handle or the keeper is
# gone or its bound expires, so an orphaned transport cannot complete a renewal.
handle_renew_transport() {
  local dir="$1" identity="$2" keeper="$3" start="$4" transport waited=0
  handle_renew_owned "$dir" "$identity" "$keeper" "$start" || exit 75
  facade_request "$(lease_command renew)" &
  transport=$!
  while kill -0 "$transport" 2>/dev/null; do
    if ! handle_renew_owned "$dir" "$identity" "$keeper" "$start" || [ "$waited" -ge 300 ]; then
      kill -KILL 0
    fi
    sleep 0.2
    waited=$((waited + 1))
  done
  wait "$transport"
}

# The detached keeper: renews while this exact handle exists, stops when it is
# released or replaced, and releases the lease after the idle limit.
handle_keeper_main() {
  local dir="$1" self identity identity_start renew_seconds retry_seconds stale_seconds next now last afd
  local response rc status last_ok idle_attempt=0 idle_failures=0
  HANDLE_RENEW_JOB=''
  [ "$#" -eq 1 ] || exit 2
  set +e
  # Renewal transports get their own process groups so ownership loss can end them whole.
  set -m
  trap '[ -z "$HANDLE_RENEW_JOB" ] || kill -KILL -- "-$HANDLE_RENEW_JOB" >/dev/null 2>&1' EXIT
  trap 'exit 0' INT TERM
  exec 9>>"$dir/keeper.lock" || exit 1
  flock -n 9 || exit 0
  load_handle "$dir" || exit 0
  identity="$(handle_lease_identity "$dir")" || exit 0
  renew_seconds="$(handle_timing renew 300)"
  retry_seconds="$(handle_timing retry 60)"
  stale_seconds="$(handle_timing stale 900)"
  next=$((SECONDS + renew_seconds))
  last_ok=$SECONDS
  self="$BASHPID"
  identity_start="$(lease_process_identity "$self")" || exit 1
  printf '%s %s\n' "$self" "${identity_start%%:*}" > "$dir/keeper.ready"
  while true; do
    # An orphaned keeper never renews: its handle was released or replaced.
    [ "$(handle_lease_identity "$dir")" = "$identity" ] || exit 0
    [ ! -e "$dir/lost" ] || exit 1
    exec {afd}>>"$dir/active.lock" || exit 1
    # A running exec holds a shared lock and counts as activity.
    if [ "$SECONDS" -ge "$idle_attempt" ] && flock -x -n "$afd"; then
      now="$(date +%s)"
      last="$(stat -c %Y "$dir/activity" 2>/dev/null || printf 0)"
      if [ $((now - last)) -ge "$HANDLE_IDLE_SECONDS" ]; then
        printf '%s\tidle-release\n' "$now" >> "$dir/keeper.log"
        release_handle idle && exit 0
        idle_failures=$((idle_failures + 1))
        if [ "$idle_failures" -ge "$HANDLE_IDLE_RELEASE_ATTEMPTS" ]; then
          # Stop renewing; the broker's expiry then reclaims the slot.
          handle_mark_lost "$dir" idle-release-abandoned
          exit 1
        fi
        # Renewal continues; retry a failed idle release after a pause.
        idle_attempt=$((SECONDS + retry_seconds))
      fi
      flock -u "$afd"
    fi
    exec {afd}>&-
    if [ "$SECONDS" -ge "$next" ]; then
      rc=0
      (set +m; handle_renew_transport "$dir" "$identity" "$self" "${identity_start%%:*}") \
        > "$dir/.renew-response" 2>/dev/null &
      HANDLE_RENEW_JOB=$!
      while kill -0 "$HANDLE_RENEW_JOB" 2>/dev/null; do
        # A released or replaced handle ends the in-flight transport; the EXIT trap kills it.
        [ "$(handle_lease_identity "$dir")" = "$identity" ] || exit 0
        sleep 0.2
      done
      wait "$HANDLE_RENEW_JOB" 2>/dev/null || rc=$?
      HANDLE_RENEW_JOB=''
      [ "$(handle_lease_identity "$dir")" = "$identity" ] || exit 0
      response="$(cat "$dir/.renew-response" 2>/dev/null)"
      status="$(jq -r 'if type == "object" then (.status // "") else "" end' <<<"$response" 2>/dev/null)"
      if [ "$rc" = 0 ] && [ "$status" = ok ]; then
        printf '%s\tok\n' "$(date +%s)" >> "$dir/keeper.log"
        last_ok=$SECONDS
        next=$((SECONDS + renew_seconds))
      elif [ "$status" = error ]; then
        # An explicit broker rejection: the lease is no longer current.
        handle_mark_lost "$dir" renew-rejected
        exit 1
      elif [ $((SECONDS - last_ok + retry_seconds)) -lt "$stale_seconds" ]; then
        # A transport failure or timeout: retry while the lease is safely unexpired.
        printf '%s\trenew-retry\n' "$(date +%s)" >> "$dir/keeper.log"
        next=$((SECONDS + retry_seconds))
      else
        handle_mark_lost "$dir" renew-unconfirmed
        exit 1
      fi
    fi
    sleep 1
  done
}

start_handle_keeper() {
  local dir="$HANDLE_DIRECTORY" waited=0
  [ -n "$dir" ] || die 'lease handle directory is not initialized'
  write_handle_state
  setsid -f bash "$CLIENT_SELF" _keeper "$dir" </dev/null >>"$dir/keeper.log" 2>&1 \
    || die 'cannot start the lease keeper'
  while [ ! -s "$dir/keeper.ready" ]; do
    [ "$waited" -lt 100 ] || die 'lease keeper did not start'
    sleep 0.1
    waited=$((waited + 1))
  done
  LEASE_KEEPER_PID=detached
}

handle_acquire_cleanup() {
  local rc="${1:-$?}" failed=0
  trap - EXIT INT TERM
  set +e
  if [ "${HANDLE_READY:-0}" != 1 ]; then
    # Retire the handle first so a started keeper cannot renew while release runs.
    [ -z "$HANDLE_DIRECTORY" ] || rm -f -- "$HANDLE_DIRECTORY/lease.json"
    if [ -n "$LEASE_ACQUIRE_PID" ] && [ -z "$LEASE_ID" ]; then
      printf '%s: acquire cleanup remains unconfirmed: slot=%s run=%s\n' "$E2E_PROGRAM" "$LEASE_REQUESTED_SLOT" "$LEASE_RUN" >&2
      failed=1
    fi
    release_lease || failed=1
    if [ -n "$LEASE_ACQUIRE_PID" ]; then
      kill "$LEASE_ACQUIRE_PID" >/dev/null 2>&1 || true
      wait "$LEASE_ACQUIRE_PID" >/dev/null 2>&1 || true
    fi
    [ -z "$HANDLE_DIRECTORY" ] || remove_handle_directory "$HANDLE_DIRECTORY" || failed=1
    [ "$failed" = 0 ] || [ "$rc" != 0 ] || rc=3
  fi
  exit "$rc"
}

client_acquire() {
  local slot='' purpose='' tool dir
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --owner) [ "$#" -ge 2 ] && [ -z "$E2E_OWNER" ] || die '--owner requires one owner ID'; E2E_OWNER="$2"; shift 2 ;;
      --pool) [ "$#" -ge 2 ] && [ -z "$E2E_POOL" ] || die '--pool requires one pool ID'; E2E_POOL="$2"; shift 2 ;;
      --yard) [ "$#" -ge 2 ] || die '--yard needs a yard name'; E2E_YARD="$2"; E2E_LEGACY_ROUTE=1; shift 2 ;;
      --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || die '--slot needs one number'; slot="$2"; set_requested_slot "$slot" --slot; shift 2 ;;
      --wait) [ "$#" -ge 2 ] || die '--wait needs a duration'; WAIT_SECONDS="$(parse_client_duration "$2" --wait)"; shift 2 ;;
      --purpose) [ "$#" -ge 2 ] || die '--purpose needs a label'; purpose="$2"; shift 2 ;;
      --project) [ "$#" -ge 2 ] || die '--project needs a label'; CLIENT_PROJECT="$2"; shift 2 ;;
      --type) [ "$#" -ge 2 ] || die '--type needs subyard-pair or android-test'; ENVIRONMENT_TYPE="$2"; shift 2 ;;
      --vm-count)
        [ "$#" -ge 2 ] && [ -z "$VM_COUNT_REQUESTED" ] || die '--vm-count needs 1 or 2'
        case "$2" in 1|2) VM_COUNT_REQUESTED="$2" ;; *) die '--vm-count must be 1 or 2' ;; esac
        shift 2 ;;
      --disk) [ "$#" -ge 2 ] && [ "$2" = 50GiB ] && [ -z "$DISK_REQUESTED" ] || die '--disk must be 50GiB'; DISK_REQUESTED="$2"; shift 2 ;;
      --idle)
        [ "$#" -ge 2 ] || die '--idle needs a duration'
        HANDLE_IDLE_SECONDS="$(parse_client_duration "$2" --idle)"
        if [ "${SUBYARD_E2E_TEST_MODE:-}" != 1 ]; then
          [ "$HANDLE_IDLE_SECONDS" -ge 60 ] && [ "$HANDLE_IDLE_SECONDS" -le 7200 ] || die '--idle must be between 1m and 2h'
        fi
        [ "$HANDLE_IDLE_SECONDS" -ge 1 ] || die '--idle must be positive'
        shift 2 ;;
      *) die "unknown acquire option '$1'" ;;
    esac
  done
  [ -n "$LEASE_REQUESTED_SLOT" ] || die 'acquire requires --slot N; choose an available slot from status'
  # A session lease is one standard VM unless a pair is requested explicitly.
  [ -n "$VM_COUNT_REQUESTED" ] || [ "$ENVIRONMENT_TYPE" = android-test ] || VM_COUNT_REQUESTED=1
  resolve_vm_count
  for tool in setsid flock jq ssh ssh-keygen; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
  done
  resolve_service_selection acquire
  configure_yard_scope
  LEASE_PURPOSE="$(bounded_context_label "${purpose:-agent}" 80)"
  LEASE_RUN="$(new_run_id)"
  LEASE_KEEPER_MODE=handle
  ensure_state_root
  install -d -m 0700 "$(handle_base)"
  dir="$(handle_directory "$LEASE_RUN")"
  mkdir -m 0700 -- "$dir" || die 'cannot create the lease handle'
  HANDLE_DIRECTORY="$dir"
  LOCAL_TEMP="$dir"
  HANDLE_READY=0
  trap handle_acquire_cleanup EXIT
  trap 'handle_acquire_cleanup 130' INT
  trap 'handle_acquire_cleanup 143' TERM
  acquire_lease || exit "$?"
  start_lease_keeper
  HANDLE_READY=1
  trap - EXIT INT TERM
  printf 'E2E handle: %s slot=%s vms=%s idle_release=%ss\n' \
    "$LEASE_RUN" "$LEASE_SLOT" "$VM_COUNT" "$HANDLE_IDLE_SECONDS" >&2
  printf '%s\n' "$LEASE_RUN"
}

# Refuse use of a handle whose lease cannot be current.
require_usable_handle() {
  local dir="$HANDLE_DIRECTORY"
  [ ! -e "$dir/lost" ] \
    || die "lease $LEASE_RUN was lost (expired or fenced); run release $LEASE_RUN, then acquire again"
  handle_keeper_running "$dir" \
    || die "lease keeper for $LEASE_RUN is not running; the broker expires the lease within 20 minutes of its last renewal; run release $LEASE_RUN"
}

client_exec() {
  local handle="${1:-}" vm=1 stdin=0 fd rc=0 interactive="$2"
  shift 2 || die 'exec needs a handle'
  [ -n "$handle" ] || die 'a lease handle is required'
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --vm) [ "$#" -ge 2 ] || die '--vm needs 1 or 2'; vm="$2"; shift 2 ;;
      --stdin) [ "$interactive" = 0 ] || die '--stdin is valid only with exec'; stdin=1; shift ;;
      --) shift; break ;;
      *) die "unknown option '$1' (put the guest command after --)" ;;
    esac
  done
  open_handle "$handle"
  case "$vm" in 1|2) [ "$vm" -le "$VM_COUNT" ] || die "this lease has only $VM_COUNT VM" ;; *) die '--vm must be 1 or 2' ;; esac
  if [ "$interactive" = 0 ]; then
    [ "$#" -gt 0 ] || die 'exec needs a guest command after --'
  else
    [ "$#" -eq 0 ] || die 'ssh opens an interactive root session and takes no command; use exec'
  fi
  exec {fd}>>"$HANDLE_DIRECTORY/active.lock" || die 'cannot open the lease handle'
  flock -s -w 30 "$fd" || die "lease $handle is being released"
  [ -f "$HANDLE_DIRECTORY/lease.json" ] || die "lease $handle was released"
  require_usable_handle
  touch -- "$HANDLE_DIRECTORY/activity"
  if [ "$interactive" = 1 ]; then
    ssh -F "$CLIENT_CONFIG" -tt "e2e-vm-$vm" || rc=$?
  elif [ "$stdin" = 1 ]; then
    ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$(quote_ssh_command "$@")" || rc=$?
  else
    ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$(quote_ssh_command "$@")" </dev/null || rc=$?
  fi
  touch -- "$HANDLE_DIRECTORY/activity" 2>/dev/null || true
  exec {fd}>&-
  return "$rc"
}

client_release() {
  [ "$#" -eq 1 ] || die 'release needs exactly one handle'
  local dir
  dir="$(handle_directory "$1")"
  if ! load_handle "$dir"; then
    printf 'E2E lease handle %s is not active; nothing to release\n' "$1" >&2
    return 0
  fi
  release_handle explicit || exit 1
}

# Exit 0 while an exec holds the handle's shared activity lock.
handle_in_use() {
  local fd rc=1
  exec {fd}>>"$1/active.lock" || return 1
  flock -x -n "$fd" || rc=0
  exec {fd}>&-
  return "$rc"
}

client_leases() {
  local dir now state idle last left keeper countdown
  [ "$#" -eq 0 ] || die 'leases takes no arguments'
  printf '%-10s %-9s %-13s %-4s %-24s %-9s %s\n' HANDLE SLOT TYPE VMS PURPOSE STATE IDLE_RELEASE
  [ -d "$(handle_base)" ] || return 0
  now="$(date +%s)"
  for dir in "$(handle_base)"/*; do
    [ -d "$dir" ] || continue
    load_handle "$dir" || continue
    state=active
    keeper=1
    handle_keeper_running "$dir" || keeper=0
    if [ -e "$dir/lost" ]; then state=lost; elif [ "$keeper" = 0 ]; then state=no-keeper; fi
    if handle_in_use "$dir"; then
      countdown=in-use
    else
      last="$(stat -c %Y "$dir/activity" 2>/dev/null || printf 0)"
      idle=$((now - last))
      left=$((HANDLE_IDLE_SECONDS - idle))
      [ "$left" -ge 0 ] || left=0
      countdown="in $(format_duration "$left")"
    fi
    printf '%-10s %-9s %-13s %-4s %-24s %-9s %s\n' "$LEASE_RUN" "$LEASE_SLOT" "$ENVIRONMENT_TYPE" \
      "$VM_COUNT" "$LEASE_PURPOSE" "$state" "$countdown"
  done
}

client_usage() {
  cat <<'EOF'
Usage: subyard-test-vms COMMAND [OPTIONS]

  status [--json]                       show slots, their state and holders
  acquire --slot N [--wait 20m] [--purpose LABEL] [--project NAME] [--idle 20m]
          [--vm-count 1|2] [--type subyard-pair|android-test] [--disk 50GiB]
                                        take a lease; prints its HANDLE on stdout
  exec HANDLE [--vm 1|2] [--stdin] -- COMMAND [ARG...]
                                        run a command as root in the leased VM
  ssh HANDLE [--vm 1|2]                 interactive root shell (terminals only)
  release HANDLE                        release the lease; the VM disks are deleted
  leases                                list this user's lease handles
  logs [--slot N] [-n 1..200] [--json]  this controller's broker events
  incident ID                           one referenced diagnostic artifact
  prepare                               create the controller identity

status, acquire, logs and incident accept --owner ID --pool ID for an exact service.
A handle keeps one lease across separate commands. A detached keeper renews it every
five minutes while the handle exists and releases it after --idle (default 20m, at
most 2h) without exec use; `exec HANDLE -- true` resets that timer. If the keeper
stops, the broker expires the lease twenty minutes after its last renewal. Nothing in
the VM survives release or expiry.
EOF
}

client_main() {
  local command="${1:-}" json=0 count=50 incident=''
  [ "$#" -gt 0 ] || { client_usage >&2; exit 2; }
  shift
  case "$command" in
    -h|--help|help) client_usage; return 0 ;;
    _keeper) handle_keeper_main "$@"; return ;;
    acquire) client_acquire "$@"; return ;;
    exec) client_exec "${1:-}" 0 "${@:2}"; return ;;
    ssh) client_exec "${1:-}" 1 "${@:2}"; return ;;
    release) client_release "$@"; return ;;
    leases) client_leases "$@"; return ;;
    status|logs|incident|prepare) ;;
    *) die "unknown command '$command'; see --help" ;;
  esac
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --owner) [ "$#" -ge 2 ] && [ -z "$E2E_OWNER" ] || die '--owner requires one owner ID'; E2E_OWNER="$2"; shift 2 ;;
      --pool) [ "$#" -ge 2 ] && [ -z "$E2E_POOL" ] || die '--pool requires one pool ID'; E2E_POOL="$2"; shift 2 ;;
      --yard) [ "$#" -ge 2 ] || die '--yard needs a yard name'; E2E_YARD="$2"; E2E_LEGACY_ROUTE=1; shift 2 ;;
      --json) [ "$command" = status ] || [ "$command" = logs ] || die '--json is valid only with status or logs'; json=1; shift ;;
      --slot) [ "$command" = logs ] && [ "$#" -ge 2 ] || die '--slot is valid only with logs'; set_requested_slot "$2" --slot; shift 2 ;;
      -n)
        [ "$command" = logs ] && [ "$#" -ge 2 ] && [[ "$2" =~ ^[0-9]{1,3}$ ]] \
          && [ "$((10#$2))" -ge 1 ] && [ "$((10#$2))" -le 200 ] || die '-n needs a number from 1 to 200 with logs'
        count="$((10#$2))"; shift 2 ;;
      *)
        [ "$command" = incident ] && [ -z "$incident" ] || die "unknown option '$1'"
        [[ "$1" =~ ^[0-9]{20}-[0-9a-f]{16}$ ]] || die 'invalid incident ID'
        incident="$1"; shift ;;
    esac
  done
  [ "$command" != incident ] || [ -n "$incident" ] || die 'incident needs an incident ID'
  resolve_service_selection "$command"
  configure_yard_scope
  case "$command" in
    prepare) prepare_identity ;;
    status)
      local response
      response="$(facade_request status)" || die 'test environment unavailable'
      if [ "$json" = 1 ]; then printf '%s\n' "$response"; else render_pool_status "$response"; fi
      ;;
    logs) read_agent_logs logs "$count" "$json" '' ;;
    incident) read_agent_logs incident "$count" 0 "$incident" ;;
  esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  client_main "$@"
fi

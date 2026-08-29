#!/usr/bin/env bash
# Developer checks on an allocated two-VM lab via restricted L1 SSH.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"
# shellcheck source=scripts/lib/runtime.sh
. "$REPO_ROOT/scripts/lib/runtime.sh"

E2E_YARD="${SUBYARD_E2E_YARD:-test-yard}"
BASTION_USER="${SUBYARD_E2E_BASTION_USER:-subyard-e2e-agent}"
STATE_BASE="${SUBYARD_E2E_STATE_DIR:-${SUBYARD_HOME:-$(subyard_operator_home)/.subyard}/e2e}"
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
PREPARED_DIRECTORY=""
GUEST_IDENTITY=""
GUEST_USER=root
DATA_USER=""
LEASE_SLOT=""
LEASE_GENERATION=""
LEASE_ID=""
LEASE_EPOCH=""
LEASE_CAPABILITY=""
LEASE_KEEPER_PID=""
LEASE_KEEPER_LOG=""
LEASE_YARD=""
LEASE_PROJECT=""
LEASE_RUN=""
LEASE_PURPOSE=""
LEASE_REQUESTED_SLOT=""
WAIT_SECONDS=0
declare -A GUEST_DIRS=()
declare -A VM_IP=()
declare -A VM_HOST_KEY=()

die() { printf 'agent-e2e: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

configure_yard_scope() {
  case "$E2E_YARD" in
    '' | *[!a-z0-9_-]* | _* | -*) die "unsafe yard selector '$E2E_YARD'" ;;
  esac
  BASTION_ROUTE="${SUBYARD_E2E_BASTION_ROUTE:-yard-$E2E_YARD}"
  STATE_ROOT="${SUBYARD_E2E_YARD_STATE_DIR:-$STATE_BASE/yards/$E2E_YARD}"
  IDENTITY="${SUBYARD_E2E_IDENTITY:-$STATE_BASE/id_ed25519}"
  CLIENT_CONFIG="$STATE_ROOT/ssh_config"
  GUEST_KNOWN_HOSTS="$STATE_ROOT/guest_known_hosts"
}

configure_yard_scope

usage() {
  cat <<'EOF'
Usage:
  dev/agent-e2e.sh [--yard NAME] --prepare
  dev/agent-e2e.sh [--yard NAME] --status [--json]
  dev/agent-e2e.sh [--yard NAME] --slot N [--wait DURATION] [--purpose LABEL] [--vm 1|2|both] -- COMMAND [ARG...]
  dev/agent-e2e.sh [--yard NAME] --slot N [--purpose LABEL] --ssh 1|2 [-- COMMAND [ARG...]]
  dev/agent-e2e.sh [--yard NAME] --slot N [--purpose LABEL] --ssh-stdin 1|2 -- COMMAND [ARG...]
  dev/agent-e2e.sh [--yard NAME] --slot N --verify-boundary

The normal form copies the current tracked, dirty and non-ignored public worktree to each selected
VM, runs COMMAND as dev, streams output and removes every run directory. A direct ./bin/yard command
first builds the explicit development candidate in that run directory. --ssh opens an ordinary guest
terminal or runs a command with stdin closed. --ssh-stdin explicitly forwards stdin to a command.
NAME defaults to test-yard. During a temporary migration, select the old yard explicitly with
`--yard e2e-yard`; route and generated client state remain isolated per yard.

--prepare creates or verifies the shared controller identity at ~/.subyard/e2e/id_ed25519. P1
accepts standard controller keys through the bounded forced-command facade. Every run creates a
separate ephemeral guest key.

The operator owns the outer test yard. Acquire creates or starts only the selected inner slot pair;
release fences access and stops that pair without deleting its disks. e2e-vm-1 and e2e-vm-2 are
lease-relative selectors, not physical slot names. Every invocation acquires a new lease; use one
script or one interactive SSH session when several steps must share mutable guest state. Every
lease-taking mode requires --slot N, atomically requests that broker slot and never falls back.
EOF
}

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
  local rendered key value rest lookup candidate route_config="${SUBYARD_E2E_ROUTE_CONFIG:-${HOME:?}/.ssh/config}"
  local explicit_known="${SUBYARD_E2E_BASTION_KNOWN_HOSTS:-}"
  local registry="${SUBYARD_E2E_ROUTE_REGISTRY:-/var/lib/subyard/e2e-routes}"
  local route_file="$registry/$E2E_YARD/current/route.tsv"
  local known_file="$registry/$E2E_YARD/current/known_hosts"
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
    if [ "${#VM_IP[@]}" -eq 2 ]; then
      for selector in 1 2; do
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

resolve_workspace_attribution() {
  local root="$1" canonical workspace_root relative project_dir metadata project yard
  canonical="$(git -C "$root" rev-parse --show-toplevel 2>/dev/null)" \
    || die "cannot identify the E2E worktree"
  canonical="$(realpath -e -- "$canonical")" \
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

new_run_id() {
  od -An -N4 -tx1 /dev/urandom | tr -d ' \n'
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

lease_acquire_request() {
  local client="$1" fingerprint="$2" key_type="$3" key_blob="$4"
  [ -n "$LEASE_REQUESTED_SLOT" ] || die "an exact --slot is required before acquiring an E2E lease"
  printf 'acquire-v2 %s %s %s %s %s %s %s %s %s\n' \
    "$client" "$fingerprint" "$LEASE_YARD" "$LEASE_PROJECT" "$LEASE_RUN" \
    "$LEASE_PURPOSE" "$key_type" "$key_blob" "$LEASE_REQUESTED_SLOT"
}

facade_request() {
  local command="$1" bootstrap
  ensure_identity
  resolve_bastion_route
  bootstrap="$(mktemp "$STATE_ROOT/.facade-config.XXXXXX")"
  VM_IP=(); VM_HOST_KEY=()
  render_client_config > "$bootstrap"
  chmod 0600 "$bootstrap"
  ssh -F "$bootstrap" -T subyard-e2e-bastion -- "$command" </dev/null
  local rc=$?
  rm -f "$bootstrap"
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
  local reference_epoch age detail
  now="$(date -u +%s)"
  printf '%-8s %-12s %-16s %-32s %-10s %-24s %-8s %s\n' \
    SLOT STATE YARD PROJECT RUN PURPOSE AGE EXPIRES
  while IFS=$'\t' read -r slot state yard project run purpose acquired expires; do
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
    printf '%-8s %-12s %-16s %-32s %-10s %-24s %-8s %s\n' \
      "$slot" "$state" "$yard" "$project" "$run" "$purpose" "$age" "$detail"
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
       else (.expires_at // "-") end)
    ] | @tsv
  ' <<<"$response")
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
  printf 'agent-e2e: requested slot %s unavailable: state=%s reason=%s; waited %s, deadline in %s\n' \
    "$LEASE_REQUESTED_SLOT" "$state" "$reason" \
    "$(format_duration "$elapsed")" "$(format_duration "$remaining")" >&2
  [ -z "$owner_summary" ] || printf 'agent-e2e: %s\n' "$owner_summary" >&2
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
  count="$(jq '.grant.targets | length' <<<"$response")"
  [ "$count" = 2 ] || die "facade returned an incomplete VM pair"
  VM_IP=(); VM_HOST_KEY=()
  for selector in 1 2; do
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
  printf 'e2e-vm-1 %s\ne2e-vm-2 %s\n' "${VM_HOST_KEY[1]}" "${VM_HOST_KEY[2]}" > "$GUEST_KNOWN_HOSTS"
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

acquire_lease() {
  local client fingerprint type blob response code reason state started last_report request
  local status_response workspace_context elapsed sleep_seconds attempt=0 last_state='' last_reason=''
  local owner_display='' owner_yard='' owner_project='' owner_run='' owner_purpose=''
  local owner_acquired='' owner_expires='' owner_acquired_epoch='' owner_expires_epoch=''
  local owner_summary='' last_owner_summary=''
  [ -n "$LEASE_REQUESTED_SLOT" ] \
    || die "an exact --slot is required before acquiring an E2E lease"
  command -v jq >/dev/null 2>&1 || die "jq is required"
  [ -n "$LOCAL_TEMP" ] || die "lease temporary directory is not initialized"
  CLIENT_CONFIG="$LOCAL_TEMP/ssh_config"
  GUEST_KNOWN_HOSTS="$LOCAL_TEMP/guest_known_hosts"
  resolve_bastion_route
  GUEST_IDENTITY="$LOCAL_TEMP/lease_id_ed25519"
  ssh-keygen -q -t ed25519 -N '' -C subyard-e2e-lease -f "$GUEST_IDENTITY"
  read -r type blob _ < "$GUEST_IDENTITY.pub"
  client="$(ensure_client_id)"
  fingerprint="$(ssh-keygen -lf "$IDENTITY.pub" -E sha256 | awk '{print $2}')"
  workspace_context="$(resolve_workspace_attribution "$REPO_ROOT")"
  IFS=$'\t' read -r LEASE_YARD LEASE_PROJECT <<<"$workspace_context"
  [ -n "$LEASE_RUN" ] || LEASE_RUN="$(new_run_id)"
  [[ "$LEASE_RUN" =~ ^[0-9a-f]{8}$ ]] || die "invalid E2E run identity"
  [ -n "$LEASE_PURPOSE" ] || LEASE_PURPOSE=agent-e2e
  if ! status_response="$(facade_request status)"; then
    die "broker capability probe failed"
  fi
  jq -e 'type == "object" and .schema_version == 1 and .status == "ok" and
    (.capabilities | type == "array" and index("attribution-v2") != null)' \
    <<<"$status_response" >/dev/null \
    || die "broker does not support required attribution-v2 acquire"
  request="$(lease_acquire_request "$client" "$fingerprint" "$type" "$blob")"
  started=$SECONDS
  last_report=-30
  while true; do
    elapsed=$((SECONDS - started))
    if [ "$attempt" -gt 0 ] && [ "$elapsed" -ge "$WAIT_SECONDS" ]; then
      printf 'agent-e2e: timed out waiting for %s: state=%s reason=%s\n' \
        "$LEASE_REQUESTED_SLOT" "$last_state" "$last_reason" >&2
      [ -z "$last_owner_summary" ] || printf 'agent-e2e: %s\n' "$last_owner_summary" >&2
      return 4
    fi
    attempt=$((attempt + 1))
    if ! response="$(facade_request "$request")"; then
      die "lease acquire outcome is unknown; refusing a second allocation"
    fi
    jq -e 'type == "object" and .schema_version == 1 and
      (.status == "ok" or .status == "error")' \
      <<<"$response" >/dev/null 2>&1 \
      || die "lease acquire outcome is unknown; refusing a second allocation"
    code="$(jq -r '.code // empty' <<<"$response")"
    if parse_lease_grant "$response"; then
      elapsed=$((SECONDS - started))
      if [ "$WAIT_SECONDS" -gt 0 ] && [ "$elapsed" -ge "$WAIT_SECONDS" ]; then
        printf 'agent-e2e: deadline elapsed before the lease grant arrived; releasing %s without guest access\n' \
          "$LEASE_REQUESTED_SLOT" >&2
        release_lease \
          || die "late lease grant could not be released safely"
        return 4
      fi
      if ! lease_grant_matches_request; then
        printf 'agent-e2e: broker granted %s instead of requested %s; releasing without guest access\n' \
          "$LEASE_SLOT" "$LEASE_REQUESTED_SLOT" >&2
        release_lease \
          || die "broker returned the wrong slot and its lease could not be released"
        die "broker returned a slot other than the exact requested slot"
      fi
      resolve_lease_generation
      write_client_config
      printf 'E2E lease: yard=%s project=%s run=%s purpose=%s slot=%s' \
        "$LEASE_YARD" "$LEASE_PROJECT" "$LEASE_RUN" "$LEASE_PURPOSE" "$LEASE_SLOT" >&2
      printf '\n' >&2
      return 0
    fi
    reason="$(jq -r '.reason // empty' <<<"$response")"
    state="$(jq -r '.state // empty' <<<"$response")"
    if [ "$code" != busy ]; then
      die "lease acquire failed (${code:-invalid_response}: ${reason:-unspecified})"
    fi
    validate_exact_busy_response "$response" \
      || die "lease acquire outcome is unknown; refusing a second allocation"
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
        || die "lease acquire outcome is unknown; refusing a second allocation"
      owner_expires_epoch="$(date -u -d "$owner_expires" +%s 2>/dev/null)" \
        || die "lease acquire outcome is unknown; refusing a second allocation"
      [ "$owner_acquired_epoch" -le "$owner_expires_epoch" ] \
        || die "lease acquire outcome is unknown; refusing a second allocation"
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
      printf 'agent-e2e: timed out waiting for %s: state=%s reason=%s\n' \
        "$LEASE_REQUESTED_SLOT" "$state" "$reason" >&2
      [ -z "$owner_summary" ] || printf 'agent-e2e: %s\n' "$owner_summary" >&2
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
  local released_slot="$LEASE_SLOT" released_run="$LEASE_RUN" response code
  [ -n "$LEASE_ID" ] || return 0
  if ! response="$(facade_request "$(lease_command release)")"; then
    printf 'agent-e2e: release failed for slot=%s run=%s\n' \
      "$released_slot" "$released_run" >&2
    return 1
  fi
  if [ "$(jq -r '.status // empty' <<<"$response")" != ok ]; then
    code="$(jq -r '.code // "invalid_response"' <<<"$response")"
    printf 'agent-e2e: release rejected for slot=%s run=%s (%s)\n' \
      "$released_slot" "$released_run" "$code" >&2
    return 1
  fi
  LEASE_ID=""
  printf 'E2E lease released: slot=%s run=%s\n' "$released_slot" "$released_run" >&2
}

lease_keeper() {
  local owner_pid="$1" response
  while sleep 60; do
    if ! response="$(facade_request "$(lease_command renew)")" ||
      [ "$(jq -r '.status // empty' <<<"$response")" != ok ]; then
      [ -z "$LEASE_KEEPER_LOG" ] \
        || printf '%s\tfailure\n' "$(date +%s)" >> "$LEASE_KEEPER_LOG"
      printf 'agent-e2e: lease lost; stopping payload transport\n' >&2
      kill -TERM "$owner_pid" >/dev/null 2>&1 || true
      return 1
    fi
    [ -z "$LEASE_KEEPER_LOG" ] \
      || printf '%s\tok\n' "$(date +%s)" >> "$LEASE_KEEPER_LOG"
  done
}

start_lease_keeper() {
  if [ -n "$LOCAL_TEMP" ]; then
    LEASE_KEEPER_LOG="$LOCAL_TEMP/lease-keeper.tsv"
    : > "$LEASE_KEEPER_LOG"
    chmod 0600 "$LEASE_KEEPER_LOG"
    printf '%s\tstarted\n' "$(date +%s)" >> "$LEASE_KEEPER_LOG"
  fi
  lease_keeper "$$" &
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
      || { printf 'agent-e2e: bastion did not reject a requested L1 command\n' >&2; return 1; }
  done

  pty_log="$(mktemp "$STATE_ROOT/.pty-probe.XXXXXX")"
  LC_ALL=C ssh -vv -F "$CLIENT_CONFIG" -tt subyard-e2e-bastion -- id </dev/null \
    > /dev/null 2> "$pty_log" || true
  grep -Fq 'PTY allocation request failed' "$pty_log" \
    || { rm -f "$pty_log"; printf 'agent-e2e: bastion did not reject a PTY request\n' >&2; return 1; }
  rm -f "$pty_log"

  for probe in sftp scp; do command -v "$probe" >/dev/null 2>&1 || die "$probe is required"; done
  transfer_log="$(mktemp "$STATE_ROOT/.transfer-probe.XXXXXX")"
  if sftp -F "$CLIENT_CONFIG" -b /dev/null subyard-e2e-bastion \
    </dev/null > /dev/null 2> "$transfer_log"; then
    rm -f "$transfer_log"
    printf 'agent-e2e: bastion allowed SFTP\n' >&2
    return 1
  fi
  probe="$(mktemp "$STATE_ROOT/.scp-probe.XXXXXX")"
  printf 'synthetic transfer probe\n' > "$probe"
  if scp -F "$CLIENT_CONFIG" -q "$probe" subyard-e2e-bastion:/tmp/subyard-e2e-agent-forbidden \
    </dev/null > /dev/null 2> "$transfer_log"; then
    rm -f "$probe" "$transfer_log"
    printf 'agent-e2e: bastion allowed SCP\n' >&2
    return 1
  fi
  rm -f "$probe" "$transfer_log"

  if ssh -F "$CLIENT_CONFIG" -T -o User=dev subyard-e2e-bastion -- true \
    </dev/null >/dev/null 2>&1; then
    printf 'agent-e2e: agent identity unexpectedly logged in as L1 dev\n' >&2
    return 1
  fi
  if ssh -F "$CLIENT_CONFIG" -T -W 127.0.0.1:22 subyard-e2e-bastion \
    </dev/null >/dev/null 2>&1; then
    printf 'agent-e2e: bastion allowed an unlisted forwarding target\n' >&2
    return 1
  fi

  for vm in 1 2; do
    [ "$(guest "$vm" sudo -n id -u </dev/null)" = 0 ] \
      || { printf 'agent-e2e: VM%s direct SSH/passwordless sudo failed\n' "$vm" >&2; return 1; }
    expected_hash="$(printf '\0subyard-binary-stdin\377' | sha256sum | awk '{print $1}')"
    actual_hash="$(printf '\0subyard-binary-stdin\377' | guest "$vm" sha256sum | awk '{print $1}')"
    [ "$actual_hash" = "$expected_hash" ] \
      || { printf 'agent-e2e: VM%s binary stdin changed in transport\n' "$vm" >&2; return 1; }
    set +e
    printf 'exit 23\n' | guest "$vm" /bin/sh -s >/dev/null 2>&1
    rc=$?
    set -e
    [ "$rc" = 23 ] \
      || { printf 'agent-e2e: VM%s SSH exit status changed (%s)\n' "$vm" "$rc" >&2; return 1; }
    if printf '%s\n' \
      'set -eu' \
      'if timeout 3 bash -c "</dev/tcp/$1/22" 2>/dev/null; then exit 42; fi' \
      | ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- sudo -n bash -s -- "$BASTION_HOSTNAME"; then
      :
    else
      case "$?" in
        42) printf 'agent-e2e: VM%s root can reach L1 SSH management\n' "$vm" >&2; return 1 ;;
        *) printf 'agent-e2e: VM%s L1-isolation probe failed unexpectedly\n' "$vm" >&2; return 1 ;;
      esac
    fi
  done

  after="$(facade_request status | jq -c --arg slot "$LEASE_SLOT" '
    [.pool.slots[] | select(.slot_id == $slot) |
      {slot_id, resource_generation, lease_epoch, state, display_label, purpose}]
  ')" || die "post-probe lease pool status failed"
  [ "$after" = "$before" ] \
    || { printf 'agent-e2e: boundary probes changed allocation state\n' >&2; return 1; }
  ok "VM SSH works; L1 commands, PTY, transfers, dev login, forwarding and guest return paths are blocked"
}

guest() {
  local vm="$1" command; shift
  command="$(quote_ssh_command "$@")"
  ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$command"
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

cleanup_guest() {
  local vm="$1" directory="${GUEST_DIRS[$1]:-}"
  [ -n "$directory" ] || return 0
  case "$directory" in /tmp/subyard-worktree.*) ;; *) return 1 ;; esac
  guest "$vm" sudo -n find "$directory" -depth -delete </dev/null
  unset 'GUEST_DIRS[$vm]'
}

cleanup_on_exit() {
  local rc=$? vm cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  if [ -n "$LEASE_KEEPER_PID" ]; then
    kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
    wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
  fi
  for vm in "${!GUEST_DIRS[@]}"; do
    cleanup_guest "$vm" >/dev/null 2>&1 || cleanup_failed=1
  done
  if [ -n "$LOCAL_TEMP" ]; then
    case "$LOCAL_TEMP" in /tmp/subyard-agent-e2e.*|"${TMPDIR:-/tmp}"/subyard-agent-e2e.*)
      find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_failed=1
      ;;
    esac
  fi
  release_lease || cleanup_failed=1
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}

worktree_paths() {
  git ls-files --cached --others --exclude-standard -z
}

build_bundle() {
  local root="$1" bundle="$2" path resolved count=0 archive inventory inventory_dir
  local -a paths=()
  while IFS= read -r -d '' path; do
    if [ ! -e "$root/$path" ] && [ ! -L "$root/$path" ]; then continue; fi
    case "$path" in
      '' | /* | ../* | */../* | .git | .git/* | private | private/* | temp | temp/*)
        die "refusing non-public worktree path '$path'"
        ;;
    esac
    if [ -L "$root/$path" ]; then
      resolved="$(realpath "$root/$path")" || die "cannot resolve symlink '$path'"
      case "$resolved" in "$root"/*) ;; *) die "refusing symlink outside the worktree: '$path'" ;; esac
    fi
    paths+=("$path")
    count=$((count + 1))
  done < <(cd "$root" && worktree_paths)
  [ "$count" -gt 0 ] || die "public worktree is empty"
  inventory_dir="$(mktemp -d "$(dirname "$bundle")/.bundle-index.XXXXXX")"
  inventory="$inventory_dir/.subyard-e2e-index"
  while IFS= read -r -d '' path; do
    if [ -e "$root/$path" ] || [ -L "$root/$path" ]; then
      printf '%s\0' "$path"
    fi
  done < <(git -C "$root" ls-files --cached -z) > "$inventory"
  chmod 0600 "$inventory"
  touch -d @0 "$inventory"
  archive="$bundle.tar"
  printf '%s\0' "${paths[@]}" | tar -C "$root" --null -T - -cf "$archive"
  tar -C "$inventory_dir" -rf "$archive" .subyard-e2e-index
  gzip -n < "$archive" > "$bundle"
  rm -f -- "$archive"
  find "$inventory_dir" -depth -delete
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
	printf 'export SUBYARD_E2E_VM=%q\n' "$vm"
	if [ "${1:-}" = ./bin/yard ]; then
		printf '/usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev ./dev/build-engine.sh\n'
	fi
	printf 'exec /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev'
	printf ' %q' "$@"
	printf '\n'
}

prepare_guest() {
  local vm="$1" bundle="$2" expected_hash="$3" directory actual_hash
  directory="$(guest "$vm" mktemp -d /tmp/subyard-worktree.XXXXXX </dev/null)" \
    || die "VM$vm did not create a run directory"
  case "$directory" in /tmp/subyard-worktree.*) ;; *) die "VM$vm returned an unsafe run directory" ;; esac
  GUEST_DIRS[$vm]="$directory"

  info "VM$vm: streaming current worktree" >&2
  guest "$vm" dd "of=$directory/worktree.tar.gz" status=none < "$bundle" \
    || die "VM$vm worktree transfer failed"
  actual_hash="$(guest "$vm" sha256sum "$directory/worktree.tar.gz" </dev/null | awk '{print $1}')" \
    || die "VM$vm checksum query failed"
  [ "$actual_hash" = "$expected_hash" ] || die "VM$vm worktree checksum mismatch"
  guest "$vm" mkdir "$directory/src" </dev/null || die "VM$vm source directory creation failed"
  guest "$vm" tar -xzf "$directory/worktree.tar.gz" -C "$directory/src" </dev/null \
    || die "VM$vm worktree extraction failed"
  if guest "$vm" test -f "$directory/src/.subyard-e2e-index" </dev/null; then
    guest "$vm" bash -c '
      root="$1"
      inventory="$root/.subyard-e2e-index"
      git -C "$root" init -q
      xargs -0 -r git -C "$root" --literal-pathspecs add -f -- < "$inventory"
      rm -f -- "$inventory"
    ' _ "$directory/src" </dev/null || die "VM$vm Git index reconstruction failed"
  fi
  PREPARED_DIRECTORY="$directory"
}

run_guest() {
  local vm="$1" bundle="$2" expected_hash="$3" directory; shift 3
  prepare_guest "$vm" "$bundle" "$expected_hash" || return
  directory="$PREPARED_DIRECTORY"
  write_guest_command "$vm" "$directory" "$@" \
    | guest "$vm" dd "of=$directory/run.sh" status=none \
    || die "VM$vm command transfer failed"
  guest "$vm" chmod 0700 "$directory/run.sh" </dev/null \
    || die "VM$vm command preparation failed"
  printf '\n== e2e-vm-%s ==\n' "$vm"
  guest "$vm" "$directory/run.sh" </dev/null 2>&1 | normalize_terminal_progress
}

normalize_terminal_progress() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    printf '%s\n' "${line##*$'\r'}"
  done
}

run_direct_ssh() {
  local vm="$1" forward_stdin="$2" command; shift 2
  if [ "$#" -gt 0 ]; then
    command="$(quote_ssh_command "$@")"
    if [ "$forward_stdin" = 1 ]; then
      ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$command"
      return
    fi
    ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$command" </dev/null
    return
  fi
  [ "$forward_stdin" = 0 ] || die "--ssh-stdin requires a command"
  ssh -F "$CLIENT_CONFIG" -tt "e2e-vm-$vm"
}

set_requested_slot() {
  local value="$1" source="${2:---slot}"
  [[ "$value" =~ ^[1-9][0-9]{0,2}$ ]] \
    || die "$source needs a number from 1 to 999"
  printf -v LEASE_REQUESTED_SLOT 'slot-%03d' "$((10#$value))"
}

main() {
  local selector=both root bundle bundle_hash vm run_failed=0 cleanup_failed=0
  local mode=run ssh_vm='' ssh_stdin=0 wait_value purpose_override='' status_json=0 slot_value=''
  local status_response
  local -a selected=() command=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --yard) [ "$#" -ge 2 ] || die "--yard needs a yard name"; E2E_YARD="$2"; shift 2 ;;
	    --slot)
	      [ "$#" -ge 2 ] || die "--slot needs a number from 1 to 999"
	      [ -z "$slot_value" ] || die "--slot may be specified only once"
	      slot_value="$2"
	      set_requested_slot "$slot_value" --slot
	      shift 2
        ;;
      --vm) [ "$#" -ge 2 ] || die "--vm needs 1, 2 or both"; selector="$2"; shift 2 ;;
      --ssh) [ "$#" -ge 2 ] || die "--ssh needs 1 or 2"; mode=ssh; ssh_vm="$2"; shift 2 ;;
      --ssh-stdin)
        [ "$#" -ge 2 ] || die "--ssh-stdin needs 1 or 2"
        mode=ssh
        ssh_vm="$2"
        ssh_stdin=1
        shift 2
        ;;
      --status) mode=status; shift ;;
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
  configure_yard_scope
  [ "$status_json" = 0 ] || [ "$mode" = status ] || die "--json is valid only with --status"
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
      trap cleanup_on_exit EXIT INT TERM
      LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
      acquire_lease
      start_lease_keeper
      verify_boundary
      return
      ;;
    ssh)
      case "$ssh_vm" in 1 | 2) ;; *) die "--ssh needs VM selector 1 or 2" ;; esac
      trap cleanup_on_exit EXIT INT TERM
      LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
      acquire_lease
      start_lease_keeper
      run_direct_ssh "$ssh_vm" "$ssh_stdin" "${command[@]}"
      return
      ;;
  esac

  [ "${#command[@]}" -gt 0 ] || die "a guest command is required after --"
  case "$selector" in 1) selected=(1) ;; 2) selected=(2) ;; both) selected=(1 2) ;; *) die "--vm must be 1, 2 or both" ;; esac
  root="$REPO_ROOT"
  git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1 \
    || die "agent E2E must run from a Git worktree"
  command -v tar >/dev/null 2>&1 || die "tar is required"
  command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"

  trap cleanup_on_exit EXIT INT TERM
  LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
  acquire_lease
  start_lease_keeper
  bundle="$LOCAL_TEMP/worktree.tar.gz"
  info "packing current public worktree"
  build_bundle "$root" "$bundle"
  bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
  ok "worktree bundle ready (sha256=$bundle_hash)"

  for vm in "${selected[@]}"; do
    if ! run_guest "$vm" "$bundle" "$bundle_hash" "${command[@]}"; then run_failed=1; fi
    if ! cleanup_guest "$vm"; then
      printf 'agent-e2e: VM%s run directory cleanup failed\n' "$vm" >&2
      cleanup_failed=1
    fi
  done
  [ "$cleanup_failed" = 0 ] || return 3
  [ "$run_failed" = 0 ] || return 1
  ok "selected VM checks passed; run-specific worktrees removed"
}

[ "${BASH_SOURCE[0]}" != "$0" ] || main "$@"

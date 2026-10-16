#!/usr/bin/env bash
# Contracts for the real-VM Hermes substrate lane.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../" && pwd)"
PROFILE="$ROOT/config/profiles/hermes"
E2E="$ROOT/config/profiles/hermes/tests/e2e/hermes-profile.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

if grep -Eq 'hermes (doctor|setup|model|gateway|cron|update)|hermes-agent\.git|scripts/install\.sh|release_tag|faster[_-]whisper|telegram|config\.yaml|/srv/hermes|HERMES_DASHBOARD_BASIC|HERMES_PROVIDER' \
  "$E2E"; then
  fail 'real lane validates or configures Hermes application behavior'
fi
if grep -Eq 'write_yard_config|cat >.*yards/.*/config\.env' "$E2E"; then
  fail 'Hermes E2E writes a named-yard definition directly'
fi
if grep -Eq '^export (ENVIRONMENT_PROFILES|CODING_TOOL_INTEGRATIONS|DEV_SUDO|FORWARD_SSH_AGENT|NESTED_E2E_VMS)=' \
  "$E2E"; then
  fail 'Hermes E2E masks the shipped preset with command environment values'
fi

for forbidden in \
  backup-to-restic.sh hermes-backup-create hermes-backup-finalize hermes-provider-ready \
  hermes-release-update hermes-restore hermes-runtime-check hermes-serve.service \
  hermes-dashboard.service hermes-dashboard-ingress; do
  [ ! -e "$PROFILE/$forbidden" ] || fail "application-aware profile helper remains: $forbidden"
done

# Execute the owning setup with native effects replaced at their boundary.
fixture="$(mktemp -d /tmp/subyard-hermes-bootstrap.XXXXXX)"
trap 'find "$fixture" -depth -delete' EXIT
bootstrap="$(sed -n '/^# Keep the synthetic owner endpoint stable/,/^printf .*provisioning only/p' "$E2E")"
fault="$(sed -n '/^cat >.*faultbin\/timeout/,/^rm -f -- .*faultbin\/timeout/p' "$E2E")"
[ -n "$bootstrap" ] && [ -n "$fault" ] || fail 'missing fixture setup or fault operation'
for scenario in free busy occupied-address; do
  (
    STATE="$fixture/$scenario"
    install -d -m 0700 "$STATE/config"
    SUBYARD_CONFIG_HOME="$STATE/config"
    YARD=fixture
    # Consumed by the actual fixture sections evaluated below.
    # shellcheck disable=SC2034
    instance=fixture project=fixture ssh_port=32123
    TAILSCALE_IP=''
    native_timeout="$(command -v timeout)"
    native_sleep="$(command -v sleep)"
    die() { fail "$@"; }
    ip() {
      if [ "$1" = -4 ] && [ "$scenario" = occupied-address ]; then
        printf 'lo 100.127.254.%s/32\n' "$((20 + ($$ % 200)))"
      elif [ "$1" = address ]; then
        [ "$2" = add ] || fail 'unexpected address operation'
        : >"$STATE/address-added"
      fi
    }
    sudo() { [ "$1" = -n ]; shift; "$@"; }
    ss() { if [ "$scenario" = busy ]; then printf 'LISTEN\n'; fi; }
    incus() { :; }
    yard() {
      shift
      [ -f "$STATE/address-added" ] || fail 'init ran before the owner address existed'
      [ "$(tailscale ip -4)" = "$TAILSCALE_IP" ] || fail 'owner address changed'
      case "$1" in
        config)
          [ ! -e "$STATE/initialized" ] || fail 'SSH port changed after initialization'
          [ "$2 $3 $4" = 'set SSH_PORT 32123' ] || fail 'wrong collision fallback'
          : >"$STATE/port-selected" ;;
        init)
          if [ "$2" = --profile ]; then
            install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD"
            install -m 0600 "$PROFILE/yard.env" "$SUBYARD_CONFIG_HOME/yards/$YARD/config.env"
          fi
          : >"$STATE/initialized" ;;
        start) [ -f "$STATE/initialized" ] ;;
        dashboard) timeout 1 true ;;
        *) fail 'unexpected fixture operation' ;;
      esac
    }
    if [ "$scenario" = occupied-address ]; then
      if (trap '[ -z "$TAILSCALE_IP" ] || touch "$STATE/foreign-address-owned"' EXIT
          eval "$bootstrap") >"$STATE/collision.log" 2>&1; then
        fail 'an existing address was claimed'
      fi
      [ ! -e "$STATE/address-added" ] && [ ! -e "$STATE/foreign-address-owned" ]
    else
      eval "$bootstrap" >/dev/null
      [ "$scenario" != busy ] || [ -f "$STATE/port-selected" ]
      eval "$fault"
      [ "$(command -v timeout)" = "$native_timeout" ] && [ "$(command -v sleep)" = "$native_sleep" ] \
        || fail 'fault tools leaked into capture or cleanup'
      [ "$(tailscale ip -4)" = "$TAILSCALE_IP" ] || fail 'fault changed the owner address'
    fi
  )
done

# Execute the disabled-resource setup with stale preview admission at the CLI boundary.
# Model currentReport (not internal Inspection): the public JSON has no plan token.
cat > "$fixture/disabled-resource-contract.sh" <<'CONTRACT'
set -euo pipefail
STATE_ROOT="$1"
scenario="$2"
PROFILE="$3"
TOKEN=123
OPERATOR_HOME="$STATE_ROOT/operator"
install -d -m 0700 "$OPERATOR_HOME/.local/bin"
printf baseline > "$STATE_ROOT/owner-host"
printf baseline > "$STATE_ROOT/preview-host"
die() { printf '%s\n' "$*" >&2; exit 1; }
ip() {
  case "$1 $2" in
    '-4 address') return 0 ;;
    'address add') printf synthetic > "$STATE_ROOT/owner-host" ;;
    'address del') printf baseline > "$STATE_ROOT/owner-host" ;;
    *) die 'unexpected native address operation' ;;
  esac
}
sudo() { [ "$1" = -n ]; shift; "$@"; }
operator_env() { "$@"; }
guest_yard() { :; }
curl() { test -f "$STATE_ROOT/dashboard-up"; }
incus() { printf '%s\n' '{"status":"Running","devices":{},"expanded_devices":{},"config":{},"expanded_config":{}}'; }
operator_yard() {
  local resource=activation.core-guest-runtime scope=activation blocker='[]'
  if [ "$1" = migrate ]; then
    if [ "$2" = --check ]; then
      if cmp -s "$STATE_ROOT/owner-host" "$STATE_ROOT/preview-host"; then
        jq -n '{schemaVersion:1,current:"candidate",owner:"candidate",
          outcome:{status:"ready",code:"ready",reachedGoal:true,active:"candidate",target:"candidate"},
          domains:[],journal:{transaction:"completed",checkpoint:"complete",from:"candidate",target:"candidate",steps:[]}}'
      else
        printf 'inspect\n' >> "$STATE_ROOT/events"
        [ "$scenario" != unexpected-drift ] || resource=activation.materialized-config
        [ "$scenario" != wrong-scope ] || scope=settings
        [ "$scenario" != blocker ] || blocker='[{"code":"precondition-blocked"}]'
        jq -n --arg resource "$resource" --arg scope "$scope" --argjson blockers "$blocker" \
          '{schemaVersion:1,current:"candidate",owner:"candidate",
            outcome:{status:"migration-required",code:"transition-required",reachedGoal:false,active:"candidate",target:"candidate"},
            domains:[],journal:{transaction:"completed",checkpoint:"complete",from:"candidate",target:"candidate",steps:[]},
            next:"run yard migrate",consequences:["reconcile the installed runtime"],
            blockers:$blockers,decisions:[{resource:"source-metadata",decision:"preserve"},
              {resource:$resource,scope:$scope,decision:"canonicalize",result:"converged"}]}'
      fi
    elif [ "$2" = --yes ]; then
      printf 'repair\n' >> "$STATE_ROOT/events"
      [ "$scenario" != refused-repair ] || return 74
      if [ "$scenario" = second-transition-fails ] && [ -f "$STATE_ROOT/first-setter" ]; then return 75; fi
      [ "$scenario" = not-converged ] || cp "$STATE_ROOT/owner-host" "$STATE_ROOT/preview-host"
    else
      die 'migration did not request fresh explicit authorization'
    fi
    return
  fi
  [ "$1 $2" = '-Y hermes' ] || die 'wrong resource target'
  shift 2
  cmp -s "$STATE_ROOT/owner-host" "$STATE_ROOT/preview-host" || die 'mutation refused: preview owner host is stale'
  case "$1 $2" in
    'config set') : > "$STATE_ROOT/first-setter" ;;
    'dashboard up') : > "$STATE_ROOT/dashboard-up" ;;
    'dashboard down') printf '  [skip] native.route: incus:subyard-hermes/yard-hermes/hermes-dashboard\n' ;;
    *) die 'unexpected resource mutation' ;;
  esac
}
# shellcheck source=config/profiles/hermes/tests/e2e/disabled-release-resource.sh
. "$PROFILE/tests/e2e/disabled-release-resource.sh"
hermes_disabled_resource_prepare
cmp "$STATE_ROOT/owner-host" "$STATE_ROOT/preview-host"
[ ! -e "$OPERATOR_HOME/.local/bin/tailscale" ] && [ ! -e "$STATE_ROOT/dashboard-address" ]
[ "$(grep -c '^repair$' "$STATE_ROOT/events")" = 2 ]
: > "$STATE_ROOT/continued"
CONTRACT
chmod 0600 "$fixture/disabled-resource-contract.sh"
for scenario in success unexpected-drift wrong-scope blocker refused-repair not-converged second-transition-fails; do
  state="$fixture/disabled-$scenario"
  install -d -m 0700 "$state"
  if bash "$fixture/disabled-resource-contract.sh" "$state" "$scenario" "$PROFILE" > "$state/output" 2>&1; then
    [ "$scenario" = success ] || fail "disabled-resource setup accepted $scenario"
    [ -f "$state/continued" ] || fail 'disabled-resource setup did not continue after both repairs'
  else
    if [ "$scenario" = success ]; then
      cat "$state/output" >&2
      fail 'disabled-resource setup did not repair both preview host transitions'
    fi
    [ ! -e "$state/continued" ] || fail 'disabled-resource setup continued after a refused repair'
    if [ "$scenario" != second-transition-fails ]; then
      [ ! -e "$state/first-setter" ] || fail 'disabled-resource setup mutated before scoped readiness'
    else
      [ -f "$state/first-setter" ] || fail 'second transition was not exercised'
    fi
    case "$scenario" in
      unexpected-drift|wrong-scope|blocker)
        ! grep -q '^repair$' "$state/events" || fail 'unexpected activation plan was authorized' ;;
    esac
  fi
done

printf 'ok: Hermes VM lane stays inside the substrate boundary\n'

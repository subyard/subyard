#!/usr/bin/env bash
# Sourced owning Hermes assertions; lease/operator/yard lifecycle belongs to the caller.

hermes_disabled_resource_assert() {
  local snapshot boundary="${1:-repeat}"
  case "$boundary" in after-down|preboot|postboot|repeat) ;; *) die 'unknown Hermes proof boundary' ;; esac
  snapshot="$(incus query '/1.0/instances/yard-hermes?project=subyard-hermes')" \
    || die 'cannot read native Hermes dashboard state'
  jq -e 'type == "object" and .status == "Running" and
    (.devices | type == "object") and (.expanded_devices | type == "object") and
    (.config | type == "object") and (.expanded_config | type == "object") and
    (.devices | has("hermes-dashboard") | not) and
    (.expanded_devices | has("hermes-dashboard") | not) and
    (.config["user.subyard.resource.hermes-dashboard"] // "") == "" and
    (.config["user.subyard.deferred_publication.hermes-dashboard"] // "") == "" and
    (.expanded_config["user.subyard.deferred_publication.hermes-dashboard"] // "") == "" and
    (.expanded_config["user.subyard.resource.hermes-dashboard"] // "") == ""' \
    <<< "$snapshot" >/dev/null || die 'native Hermes dashboard state is unavailable or retains its route/ownership'
  # A repeated public shutdown proves a native no-op without a Tailnet address or guest endpoint.
  # Check absence first so a reintroduced route cannot be removed and mistaken for retained state.
  install -m 0600 /dev/null "$STATE_ROOT/dashboard-disabled.txt"
  operator_yard -Y hermes dashboard down --yes > "$STATE_ROOT/dashboard-disabled.txt" 2>&1 \
    || die 'disabled Hermes dashboard repeat shutdown failed'
  grep -Fxq '  [skip] native.route: incus:subyard-hermes/yard-hermes/hermes-dashboard' "$STATE_ROOT/dashboard-disabled.txt" \
    || die 'disabled Hermes dashboard repeat shutdown did not report the absent native target'
  [ -f "$STATE_ROOT/hermes-dashboard-native.jsonl" ] || install -m 0600 /dev/null "$STATE_ROOT/hermes-dashboard-native.jsonl"
  jq -c --arg boundary "$boundary" '
    {boundary:$boundary,instance:"yard-hermes",project:"subyard-hermes",status,
     route_present:(.devices | has("hermes-dashboard")),expanded_route_present:(.expanded_devices | has("hermes-dashboard")),
     ownership_empty:((.config["user.subyard.resource.hermes-dashboard"] // "") == ""),
     deferred_publication_empty:((.config["user.subyard.deferred_publication.hermes-dashboard"] // "") == ""),
     expanded_deferred_publication_empty:((.expanded_config["user.subyard.deferred_publication.hermes-dashboard"] // "") == ""),
     expanded_ownership_empty:((.expanded_config["user.subyard.resource.hermes-dashboard"] // "") == ""),
     native_query_exit_code:0,dashboard_down_repeat_exit_code:0}
  ' <<< "$snapshot" >> "$STATE_ROOT/hermes-dashboard-native.jsonl"
}

# Changing the fixture Tailnet address also changes the attached core preview host.
# Inspect that one expected owner before authorizing a fresh, bound runtime repair.
hermes_disabled_resource_reconcile_owner_host() {
  local boundary="$1" plan="$STATE_ROOT/dashboard-owner-$1-plan.json"
  local ready="$STATE_ROOT/dashboard-owner-$1-ready.json" log="$STATE_ROOT/dashboard-owner-$1-repair.log"
  install -m 0600 /dev/null "$plan"
  operator_yard migrate --check --json > "$plan" || die "cannot inspect Hermes owner address $boundary"
  jq -e '.schemaVersion == 1 and
    .outcome.status == "migration-required" and .outcome.code == "transition-required" and
    .outcome.reachedGoal == false and ((.blockers // []) | length) == 0 and
    ([.decisions[]? | select(.decision != "preserve")] |
      length == 1 and .[0].resource == "activation.core-guest-runtime" and
      .[0].scope == "activation" and .[0].decision == "canonicalize" and .[0].result == "converged")' \
    "$plan" >/dev/null || die "unexpected activation drift after Hermes owner address $boundary"
  install -m 0600 /dev/null "$log"
  operator_yard migrate --yes > "$log" 2>&1 || die "Hermes owner address repair failed after $boundary"
  install -m 0600 /dev/null "$ready"
  operator_yard migrate --check --json > "$ready" || die "cannot verify Hermes owner address $boundary"
  jq -e '.schemaVersion == 1 and .outcome.status == "ready" and .outcome.code == "ready" and
    .outcome.reachedGoal == true and .outcome.active == .outcome.target and
    ((.blockers // []) | length) == 0 and
    ([.decisions[]? | select(.decision != "preserve")] | length) == 0' \
    "$ready" >/dev/null || die "Hermes owner address repair did not converge after $boundary"
}

hermes_disabled_resource_prepare() {
  local address port
  address="100.127.253.$((20 + TOKEN % 200))"
  port=$((42000 + TOKEN % 1000))
  ! ip -4 address show | grep -Fq "$address/" || die 'synthetic dashboard address already exists'
  sudo -n ip address add "$address/32" dev lo
  printf '%s\n' "$address" > "$STATE_ROOT/dashboard-address"
  chmod 0600 "$STATE_ROOT/dashboard-address"
  # A synthetic Tailnet address exercises the shipped route handler without an account.
  operator_env bash -c '
    set -eu
    test ! -e "$1"
    printf "#!/bin/sh\n# subyard-disabled-dashboard-v1\n[ \"\$1\" = ip ] && [ \"\$2\" = -4 ] || exit 2\nprintf \047%%s\\n\047 \047%s\047\n" "$2" > "$1"
    chmod 0700 "$1"
  ' _ "$OPERATOR_HOME/.local/bin/tailscale" "$address"
  hermes_disabled_resource_reconcile_owner_host added
  guest_yard hermes sh -euc '
    test ! -e /var/tmp/subyard-disabled-dashboard.pid
    install -m 0600 /dev/null /var/tmp/subyard-disabled-dashboard.log
    nohup python3 -m http.server 9119 --bind 127.0.0.1 >/var/tmp/subyard-disabled-dashboard.log 2>&1 &
    printf "%s\n" "$!" > /var/tmp/subyard-disabled-dashboard.pid
    chmod 0600 /var/tmp/subyard-disabled-dashboard.pid /var/tmp/subyard-disabled-dashboard.log
  '
  for _ in $(seq 1 30); do
    guest_yard hermes bash -c 'exec 3<>/dev/tcp/127.0.0.1/9119' >/dev/null 2>&1 && break
    sleep 1
  done
  guest_yard hermes bash -c 'exec 3<>/dev/tcp/127.0.0.1/9119' >/dev/null 2>&1 \
    || die 'controlled Hermes loopback endpoint did not start'
  operator_yard -Y hermes config set HERMES_DASHBOARD_ADVERTISE_HOST "$address" --scope yard --local --yes
  operator_yard -Y hermes config set HERMES_DASHBOARD_HOST_PORT "$port" --scope yard --local --yes
  operator_yard -Y hermes dashboard up --yes
  curl --noproxy '*' -fsS --max-time 5 "http://$address:$port/" >/dev/null \
    || die 'enabled Hermes resource did not publish its endpoint'
  operator_yard -Y hermes dashboard down --yes
  hermes_disabled_resource_assert after-down
  operator_env bash -c '
    set -eu
    test "$(head -n 2 "$1" | tail -n 1)" = "# subyard-disabled-dashboard-v1"
    rm "$1"
  ' _ "$OPERATOR_HOME/.local/bin/tailscale"
  sudo -n ip address del "$address/32" dev lo
  find "$STATE_ROOT/dashboard-address" -delete
  hermes_disabled_resource_reconcile_owner_host removed
  printf 'ok: explicitly disabled shipped Hermes resource retains absent route/ownership\n'
}

hermes_disabled_resource_cleanup() {
  [ -f "$STATE_ROOT/dashboard-address" ] || return 0
  local address
  address="$(cat "$STATE_ROOT/dashboard-address")"
  [[ "$address" =~ ^100\.127\.253\.[0-9]+$ ]] || return 1
  sudo -n ip address del "$address/32" dev lo || return 1
  find "$STATE_ROOT/dashboard-address" -delete
}

#!/usr/bin/env bash
# Private, bounded diagnostics; never send their contents to the terminal.
hermes_wait_github_runtime_or_report() {
  local deadline=$((SECONDS + ${1:-30})) rc=1 remaining
  local -a codes
  while [ "$SECONDS" -lt "$deadline" ]; do
    remaining=$((deadline - SECONDS))
    [ "$remaining" -gt 0 ] || break
    [ "$remaining" -le 5 ] || remaining=5
    if timeout --signal=TERM --kill-after=2 "$remaining" "$YARD_BIN" -Y "$YARD" profile status github --json 2>/dev/null \
      | jq -e '.runtime == "ready"' >/dev/null 2>&1; then
      return 0
    else
      codes=("${PIPESTATUS[@]}")
    fi
    rc="${codes[0]}"
    [ "$rc" != 0 ] || rc="${codes[1]}"
    [ "$SECONDS" -ge "$deadline" ] || sleep 1
  done
  hermes_capture_release_readiness "$rc" || true
  return "$rc"
}

hermes_configure_dashboard_route() {
  local rc=0
  yard "$YARD" config set HERMES_DASHBOARD_ADVERTISE_HOST "$TAILSCALE_IP" --scope yard --yes || rc=$?
  if [ "$rc" = 0 ]; then
    # shellcheck disable=SC2154
    yard "$YARD" config set HERMES_DASHBOARD_HOST_PORT "$dashboard_port" --scope yard --yes || rc=$?
  fi
  if [ "$rc" != 0 ]; then hermes_capture_release_readiness "$rc" || true; fi
  return "$rc"
}

# Project only fixed enums from one read-only inspection; keep its native exit
# separate from parsing and from the fixture failure that triggered it.
hermes_capture_release_readiness() {
  local original="$1"
  local -a codes
  timeout --signal=TERM --kill-after=2 15 "$YARD_BIN" -Y "$YARD" migrate --check --json 2>/dev/null | python3 -c '
from collections import Counter
import json, sys
statuses = {"ready", "migration-required", "recovering", "operator-action-required"}
codes = {"ready", "transition-required", "recovery-pending", "registry-invalid",
    "unsupported-epoch", "unsupported-kind", "confirmation-required", "operation-declined",
    "plan-stale", "migration-stale", "resource-conflict", "precondition-blocked",
    "dependency-unavailable", "verification-failed", "activation-ambiguous",
    "recovery-ambiguous", "journal-invalid", "rollback-incompatible", "rollback-expired"}
owners = {"activation."+name for name in ("core-guest-runtime", "key-tools-runtime",
    "materialized-config", "github-runtime", "git-fetch-runtime", "openlit-runtime", "orca-runtime")}
try:
    payload = sys.stdin.buffer.read(65537)
    if len(payload) > 65536:
        raise ValueError()
    value = json.loads(payload)
    outcome, decisions, blockers = value["outcome"], value.get("decisions", []), value.get("blockers", [])
    if (type(value.get("schemaVersion")) is not int or value["schemaVersion"] != 1
            or outcome["status"] not in statuses or outcome["code"] not in codes
            or type(outcome["reachedGoal"]) is not bool
            or not isinstance(decisions, list) or len(decisions) > 64
            or not isinstance(blockers, list) or len(blockers) > 32
            or any(not isinstance(row, dict) for row in decisions+blockers)):
        raise ValueError()
    changed = [row for row in decisions if row.get("decision") != "preserve"]
    known = [row["resource"] for row in changed
             if row.get("scope") == "activation" and row.get("resource") in owners]
    projection = {"status": outcome["status"], "code": outcome["code"],
        "reachedGoal": outcome["reachedGoal"], "decision_count": len(decisions),
        "changed_activation_owners": sorted(set(known)), "other_changed_decisions": len(changed)-len(known),
        "blocker_codes": dict(Counter(row["code"] for row in blockers if row.get("code") in codes)),
        "other_blocker_codes": sum(row.get("code") not in codes for row in blockers)}
    print("HERMES_RELEASE_READINESS="+json.dumps(projection, sort_keys=True))
except (ValueError, KeyError, TypeError, AttributeError, RecursionError):
    print("HERMES_RELEASE_READINESS_CAPTURE category=invalid-projection")
    sys.exit(2)
'
  codes=("${PIPESTATUS[@]}")
  printf 'HERMES_RELEASE_DIAGNOSTIC original_exit_code=%s native_exit_code=%s capture_exit_code=%s\n' \
    "$original" "${codes[0]}" "${codes[1]}"
  [ "${codes[0]}" = 0 ] && [ "${codes[1]}" = 0 ]
}

hermes_capture_failure() {
  local directory="$1" original="$2" unit="subyard-github-$YARD.service" uid capture_rc=0
  [ ! -e "$directory" ] && [ ! -L "$directory" ] || return 2
  install -d -m 0700 "$directory" || return 2
  local name
  for name in unit journal client probes result; do
    install -m 0600 /dev/null "$directory/$name" || return 2
  done
  uid="$(id -u)"
  hermes_capture_probe "$directory" unit env XDG_RUNTIME_DIR="/run/user/$uid" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" systemctl --user show "$unit" \
    -p LoadState -p ActiveState -p SubState -p Result -p ExecMainStatus -p NRestarts \
    || capture_rc=1
  hermes_capture_probe "$directory" journal journalctl --user -u "$unit" \
    --no-pager -n 80 -o short-precise || capture_rc=1
  # Resolve these before teardown, including failures before normal fixture discovery.
  local instance project
  instance="$(timeout --signal=TERM --kill-after=2 10 "$YARD_BIN" -Y "$YARD" \
    config show YARD_INSTANCE_NAME 2>/dev/null | sed -n 's/^effective: //p')" || capture_rc=1
  project="$(timeout --signal=TERM --kill-after=2 10 "$YARD_BIN" -Y "$YARD" \
    config show INCUS_PROJECT 2>/dev/null | sed -n 's/^effective: //p')" || capture_rc=1
  hermes_capture_probe "$directory" client incus exec "$instance" --project "$project" -- \
    runuser -u dev -- /usr/local/bin/subyard-github status --runtime || capture_rc=1
  printf 'original_exit_code=%s\ncapture_exit_code=%s\n' "$original" "$capture_rc" >"$directory/result"
  return "$capture_rc"
}

hermes_capture_probe() {
  local directory="$1" name="$2"
  shift 2
  local size
  local -a codes
  timeout --signal=TERM --kill-after=2 10 "$@" 2>&1 | head -c 65536 >"$directory/$name"
  codes=("${PIPESTATUS[@]}")
  size="$(stat -c %s "$directory/$name")"
  printf '%s\t%s\t%s\t%s\n' "$name" "${codes[0]}" "${codes[1]}" "$size" >>"$directory/probes"
  [ "${codes[0]}" = 0 ] && [ "${codes[1]}" = 0 ] && [ "$size" -lt 65536 ]
}

hermes_retain_failure() {
  local source="$1" output="$2" rc=0 command
  local -a codes
  install -m 0600 /dev/null "$output/failure.tar" || return 2
  install -m 0600 /dev/null "$output/capture-transport.log" || return 2
  # The fixed allowlist excludes config, environment, credentials and arbitrary paths.
  command="$(quote_ssh_command timeout --signal=TERM --kill-after=2 15 bash -euc '
    test -d "$1" && test ! -L "$1"
    test "$(stat -c %a "$1")" = 700
    for name in unit journal client probes result; do
      path="$1/$name"
      test -f "$path" && test ! -L "$path"
      test "$(stat -c %a "$path")" = 600
      test "$(stat -c %s "$path")" -le 65536
    done
    exec tar -C "$1" -cf - unit journal client probes result
  ' _ "$source")" || return 2
  # Bound the native local transport too: remote timeout cannot stop stalled SSH.
  timeout --signal=TERM --kill-after=2 20 ssh -F "$CLIENT_CONFIG" -T e2e-vm-1 -- "$command" \
    2>"$output/capture-transport.log" | head -c 220000 >"$output/failure.tar"
  codes=("${PIPESTATUS[@]}")
  [ "${codes[0]}" = 0 ] && [ "${codes[1]}" = 0 ] || rc=1
  # Validate the archive without extracting or printing private original bytes.
  python3 - "$output/failure.tar" <<'PY' || rc=1
import sys
import tarfile
try:
    with tarfile.open(sys.argv[1]) as archive:
        members = archive.getmembers()
        assert sorted(item.name for item in members) == ['client', 'journal', 'probes', 'result', 'unit']
        assert all(item.isfile() and item.mode == 0o600 and item.size <= 65536 for item in members)
        result = archive.extractfile('result').read().decode('ascii')
        assert 'capture_exit_code=0\n' in result
except (OSError, tarfile.TarError, AssertionError, UnicodeError):
    sys.exit(1)
PY
  return "$rc"
}

hermes_repeat_init() {
  if yard "$YARD" init --yes; then
    :
  else
    repeat_init_rc=$?
    hermes_capture_release_readiness "$repeat_init_rc" || true
    exit "$repeat_init_rc"
  fi
}

hermes_cleanup() {
  local original=$? capture_rc=0 cleanup_rc=0
  trap - EXIT INT TERM
  set +e
  if [ "$original" -ne 0 ] && [ -n "$YARD" ]; then
    hermes_capture_failure "$ROOT/.build/hermes-failure" "$original" || capture_rc=$?
  fi
  if [ -n "$YARD" ] && [ -f "$SUBYARD_CONFIG_HOME/yards/$YARD/config.env" ]; then
    yard "$YARD" teardown --yes >/dev/null 2>&1 || cleanup_rc=3
  fi
  if [ -n "$TAILSCALE_IP" ]; then
    sudo -n ip address del "$TAILSCALE_IP/32" dev lo >/dev/null 2>&1 || true
  fi
  cleanup_fixture_pool || cleanup_rc=3
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-hermes-profile.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-hermes-profile-e2e-v3 ]; then
    sudo -n find "$STATE" -depth -delete || cleanup_rc=3
  fi
  printf 'HERMES_GUEST_RESULT original_exit_code=%s capture_exit_code=%s cleanup_exit_code=%s\n' \
    "$original" "$capture_rc" "$cleanup_rc"
  [ "$original" -eq 0 ] || exit "$original"
  exit "$cleanup_rc"
}

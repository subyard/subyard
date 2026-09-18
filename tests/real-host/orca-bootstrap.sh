#!/usr/bin/env bash
# Fresh `yard orca up` bootstrap against real Incus and stock Orca. Disposable E2E VM only.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=config/profiles/orca/release.env
. "$ROOT/config/profiles/orca/release.env"

STATE=''
PROJECT=''
INSTANCE=''
COLLISION_PID=''
KEEP_FAILED_UPGRADE=0
ORIGINAL_HOME="$HOME"
YARD_BIN="$ROOT/.build/yard"
EXISTING_YARD="${SUBYARD_E2E_ORCA_EXISTING_YARD:-0}"
CODEX_CONFIG="${SUBYARD_E2E_ORCA_CODEX_CONFIG:-0}"
CODEX_PERMISSIONS="${SUBYARD_E2E_ORCA_CODEX_PERMISSIONS:-0}"
SSH_AGENT="${SUBYARD_E2E_ORCA_SSH_AGENT:-0}"
KEEP_FAILED="${SUBYARD_E2E_ORCA_KEEP_FAILED:-0}"
RESUME_STATE="${SUBYARD_E2E_ORCA_RESUME:-}"
FIXTURE_READY=0
INSTALLED_RELEASE=''
UPGRADE_FROM="${SUBYARD_E2E_ORCA_UPGRADE_FROM:-}"
UPGRADE_INSTALLER_SHA256="${SUBYARD_E2E_ORCA_UPGRADE_INSTALLER_SHA256:-}"

stage() { printf 'orca-bootstrap-e2e: %s\n' "$*" >&2; }
die() { printf 'orca-bootstrap-e2e: %s\n' "$*" >&2; exit 1; }

[ "${SUBYARD_E2E_ORCA_BOOTSTRAP:-}" = 1 ] \
  || die 'set SUBYARD_E2E_ORCA_BOOTSTRAP=1 inside a disposable test host'
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
case "$EXISTING_YARD" in
  0|1) ;;
  *) die 'SUBYARD_E2E_ORCA_EXISTING_YARD must be 0 or 1' ;;
esac
case "$CODEX_CONFIG" in
  0) ;;
  1) EXISTING_YARD=1 ;;
  *) die 'SUBYARD_E2E_ORCA_CODEX_CONFIG must be 0 or 1' ;;
esac
case "$CODEX_PERMISSIONS" in
  0) ;;
  1)
    [ "$EXISTING_YARD" = 0 ] && [ "$CODEX_CONFIG" = 0 ] && [ -z "$UPGRADE_FROM" ] \
      || die 'Codex permissions acceptance is separate from config and upgrade modes'
    ;;
  *) die 'SUBYARD_E2E_ORCA_CODEX_PERMISSIONS must be 0 or 1' ;;
esac
case "$SSH_AGENT" in
  0) ;;
  1)
    [ "$EXISTING_YARD" = 0 ] && [ "$CODEX_PERMISSIONS" = 0 ] && [ -z "$UPGRADE_FROM" ] \
      || die 'SSH agent acceptance is separate from config and upgrade modes'
    ;;
  *) die 'SUBYARD_E2E_ORCA_SSH_AGENT must be 0 or 1' ;;
esac
case "$KEEP_FAILED" in 0|1) ;; *) die 'SUBYARD_E2E_ORCA_KEEP_FAILED must be 0 or 1' ;; esac
if [ "$KEEP_FAILED" = 1 ] || [ -n "$RESUME_STATE" ]; then
  [ "$SSH_AGENT" = 1 ] || die 'retaining or resuming a fixture is supported only in SSH agent mode'
fi
if [ -n "$UPGRADE_FROM" ]; then
  [ "$CODEX_CONFIG" = 0 ] || die 'Codex config and predecessor upgrade modes are separate checks'
  [[ "$UPGRADE_FROM" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die 'upgrade source must be an exact published version'
  [[ "$UPGRADE_INSTALLER_SHA256" =~ ^[0-9a-f]{64}$ ]] || die 'the published installer SHA-256 is required'
  EXISTING_YARD=1
fi
for command in git go incus jq python3 rg script sudo timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
platform_marker="$ORIGINAL_HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker"

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() { timeout --signal=TERM --kill-after=5s 900 "$YARD_BIN" "$@"; }
guest_root() { incus --project "$PROJECT" exec "$INSTANCE" -- "$@"; }

cleanup() {
  local rc=$? cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  [ "$FIXTURE_READY" = 1 ] || exit "$rc"
  if [ "$SSH_AGENT" = 1 ]; then
    if declare -F ssh_agent_cleanup >/dev/null; then
      ssh_agent_cleanup || { rc=3; cleanup_failed=1; }
    else
      yard ssh-agent lock >/dev/null 2>&1 || { rc=3; cleanup_failed=1; }
      if [ -f "$SUBYARD_CONFIG_HOME/yards/secondary/config.env" ]; then
        yard -Y secondary ssh-agent lock >/dev/null 2>&1 || { rc=3; cleanup_failed=1; }
      fi
    fi
  fi
  if [ -n "$COLLISION_PID" ] && kill -0 "$COLLISION_PID" 2>/dev/null; then
    kill -TERM "$COLLISION_PID" 2>/dev/null || true
    wait "$COLLISION_PID" 2>/dev/null || true
  fi
  if [ "$KEEP_FAILED_UPGRADE" = 1 ]; then
    stage "retained marked failed upgrade fixture: $STATE"
    exit "$rc"
  fi
  if [ "$rc" -ne 0 ] && [ "$KEEP_FAILED" = 1 ] && [ "$cleanup_failed" = 0 ]; then
    stage "retained marked SSH fixture after revoking grants: $STATE"
    stage 'resume with SUBYARD_E2E_ORCA_RESUME set to that exact path; successful runs clean up normally'
    exit "$rc"
  fi
  if [ -f "${SUBYARD_CONFIG_HOME:-}/yards/secondary/config.env" ]; then
    yard -Y secondary teardown --yes >/dev/null 2>&1 || { rc=3; cleanup_failed=1; }
  fi
  if [ -f "${SUBYARD_CONFIG_HOME:-}/yards/default/config.env" ]; then
    yard teardown --yes >/dev/null 2>&1 || { rc=3; cleanup_failed=1; }
  fi
  if [ "$cleanup_failed" = 0 ] && [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-orca-bootstrap.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

setting_value() {
  yard config show "$1" | awk -F': ' '$1 == "effective" {print $2}'
}

release_ready() {
  local phase="$1" output error
  output="$STATE/release-$phase.json"
  error="$STATE/release-$phase.err"
  yard update --check --offline --version "$release_version" >"$output" 2>"$error" || return 1
  jq -s -e --arg target "$INSTALLED_RELEASE" '
    map(select(type == "object") | (.inspection // .)) |
    map(select(has("outcome") and has("assessment"))) as $reports |
    ($reports | length) == 1 and
    $reports[0].outcome.status == "ready" and
    $reports[0].outcome.reachedGoal == true and
    $reports[0].outcome.active == $target and
    $reports[0].outcome.target == $target and
    $reports[0].assessment.changed == false and
    (($reports[0].blockers // []) | length) == 0
  ' "$output" >/dev/null
}

print_release_state() {
  local phase="$1" output
  output="$STATE/release-$phase.json"
  if [ -f "$output" ]; then
    jq -s -r '
      map(select(type == "object") | (.inspection // .)) |
      map(select(has("outcome") and has("assessment"))) as $reports |
      if ($reports | length) == 1 then
        $reports[0] |
        "release-readiness: status=\(.outcome.status // "unavailable") " +
        "code=\(.outcome.code // "unavailable") " +
        "active=\(.outcome.active // "unavailable") " +
        "target=\(.outcome.target // "unavailable") " +
        "changed=\(if (.assessment | has("changed")) then .assessment.changed else "unavailable" end) " +
        "blockers=\((.blockers // []) | length)"
      else
        "release-readiness: reports=\($reports | length)"
      end
    ' "$output" >&2 || true
  fi
}

compare_json_managed_projection() {
  local source="$1" destination="$2" label="$3" diagnostics="${4:-0}"
  guest_root python3 -c '
import json
import re
import sys

safe = re.compile(r"^[A-Za-z0-9_.-]{1,64}$")
reported = 0
mismatch = False

def name(path):
    return ".".join(part if safe.fullmatch(part) else "<field>" for part in path) or "<root>"

def report(kind, path):
    global reported
    if reported < 64:
        print(f"materialized-json-{kind}:{name(path)}")
    reported += 1

def compare(expected, actual, path=()):
    global mismatch
    if isinstance(expected, dict):
        if not isinstance(actual, dict):
            report("managed-mismatch", path)
            mismatch = True
            return
        for key, value in expected.items():
            if key not in actual:
                report("managed-missing", path + (key,))
                mismatch = True
            else:
                compare(value, actual[key], path + (key,))
        if sys.argv[2] == "1":
            for key in actual.keys() - expected.keys():
                report("runtime-field", path + (key,))
    elif expected != actual:
        report("managed-mismatch", path)
        mismatch = True

try:
    expected = json.load(sys.stdin)
    with open(sys.argv[1], encoding="utf-8") as handle:
        actual = json.load(handle)
    compare(expected, actual)
except (OSError, UnicodeError, json.JSONDecodeError):
    print("materialized-json-state:unavailable-or-invalid")
    mismatch = True
sys.exit(1 if mismatch else 0)
' "$destination" "$diagnostics" <"$source" | sed "s|^|$label:|" >&2
}

assert_materialized_json() {
  local claude_source="${1:-$ROOT/config/agents/claude/settings.json}"
  local pi_source="${2:-$ROOT/config/agents/pi/settings.json}"
  local asset source destination
  for asset in claude/settings.json pi/settings.json; do
    case "$asset" in
      claude/*) source="$claude_source"; destination=/home/dev/.claude/settings.json ;;
      pi/*) source="$pi_source"; destination=/home/dev/.pi/agent/settings.json ;;
    esac
    compare_json_managed_projection "$source" "$destination" "$asset" \
      || return 1
  done
}

assert_orca_readiness() {
  local scope="${1:-}"
  [ "$(guest_root stat -c %a /srv/agents/orca/ready.json)" = 600 ] \
    || die 'Orca readiness file is not mode 0600'
  guest_root jq -e --arg endpoint "ws://127.0.0.1:$ORCA_PORT" --arg scope "$scope" '
    .type == "orca_server_ready" and
    .schemaVersion == 1 and
    .advertisedEndpoint == $endpoint and
    .pairing.available == true and
    ($scope == "" or .pairing.scope == $scope) and
    (.pairing.url | type == "string" and startswith("orca://pair?"))
  ' /srv/agents/orca/ready.json >/dev/null \
    || die 'Orca readiness contract was unavailable'
}

assert_pairing_file() {
  local path="$1" scope="$2"
  [ "$(stat -c %a "$path")" = 600 ] || die 'pairing output file is not mode 0600'
  python3 - "$path" "$scope" "ws://127.0.0.1:$ORCA_PORT" <<'PY' \
    || die 'pairing link did not contain the expected private contract'
import base64
import json
import sys
from pathlib import Path

try:
    raw = Path(sys.argv[1]).read_text(encoding="utf-8").splitlines()
    if not raw or not raw[-1].startswith("orca://pair?code="):
        raise ValueError
    code = raw[-1].removeprefix("orca://pair?code=")
    if not code or len(code) % 4 == 1:
        raise ValueError
    payload = json.loads(base64.urlsafe_b64decode(code + "=" * (-len(code) % 4)))
    valid = (
        payload.get("v") == 2
        and payload.get("endpoint") == sys.argv[3]
        and isinstance(payload.get("deviceToken"), str) and payload["deviceToken"]
        and isinstance(payload.get("publicKeyB64"), str) and payload["publicKeyB64"]
        and payload.get("scope") == sys.argv[2]
    )
except Exception:
    valid = False
raise SystemExit(0 if valid else 1)
PY
}

assert_fresh_pairing_files() {
  python3 - "$1" "$2" <<'PY' || die 'mobile pairing reused the old offer'
import sys
from pathlib import Path

try:
    first = Path(sys.argv[1]).read_text(encoding="utf-8").splitlines()[-1]
    second = Path(sys.argv[2]).read_text(encoding="utf-8").splitlines()[-1]
except (OSError, UnicodeError, IndexError):
    raise SystemExit(1)
raise SystemExit(0 if first != second else 1)
PY
}

assert_no_pairing_capability_logs() {
  local status="$STATE/orca-status.out" journal="$STATE/orca-journal.out"
  yard orca status >"$status" 2>"$STATE/orca-status.err"
  if rg -q 'orca://|deviceToken|publicKeyB64' "$status" "$STATE/orca-status.err"; then
    die 'Orca status leaked a pairing capability'
  fi
  install -m 0600 /dev/null "$journal"
  guest_root journalctl -u subyard-orca.service --no-pager >"$journal"
  if rg -q 'orca://|deviceToken|publicKeyB64' "$journal"; then
    die 'Orca service journal leaked a pairing capability'
  fi
}

guest_json_projection_hash() {
  local filter="$1" path="$2"
  guest_root sh -c '
    set -eu
    canonical="$(jq -cS "$1" "$2" 2>/dev/null)"
    printf "%s" "$canonical" | sha256sum | cut -d " " -f 1
  ' sh "$filter" "$path"
}

capture_orca_runtime_json() {
  guest_root jq -e '
    (.hooks | type == "object" and length > 0) and
    (.statusLine != null)
  ' /home/dev/.claude/settings.json >/dev/null \
    || die 'Orca did not add Claude hooks and status line'
  CLAUDE_HOOKS_COUNT="$(guest_root jq -er '.hooks | length | select(. > 0)' \
    /home/dev/.claude/settings.json 2>/dev/null)" \
    || die 'Orca did not add Claude hooks'
  CLAUDE_HOOKS_HASH="$(guest_json_projection_hash '.hooks' \
    /home/dev/.claude/settings.json)" \
    || die 'Claude hooks could not be fingerprinted'
  CLAUDE_STATUS_HASH="$(guest_json_projection_hash '.statusLine | select(. != null)' \
    /home/dev/.claude/settings.json)" \
    || die 'Orca did not add a Claude status line'

  guest_root runuser -u dev -- python3 -c '
import json
import os
import tempfile

updates = {
    "/home/dev/.claude/settings.json": lambda value: value.setdefault("env", {}).update(
        {"SUBYARD_E2E_RUNTIME_ADDITION": "claude-runtime"}
    ),
    "/home/dev/.pi/agent/settings.json": lambda value: value.setdefault(
        "runtimeAdditions", {}
    ).update({"subyardE2E": {"enabled": True, "generation": 1}}),
}
for path, update in updates.items():
    with open(path, encoding="utf-8") as source:
        value = json.load(source)
    update(value)
    descriptor, temporary = tempfile.mkstemp(prefix=".subyard-e2e.", dir=os.path.dirname(path))
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(value, output, sort_keys=True, separators=(",", ":"))
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.chmod(temporary, 0o644)
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            os.unlink(temporary)
' >/dev/null || die 'synthetic runtime JSON additions could not be prepared'

  CLAUDE_RUNTIME_HASH="$(guest_json_projection_hash \
    '{env: {SUBYARD_E2E_RUNTIME_ADDITION: .env.SUBYARD_E2E_RUNTIME_ADDITION}}' \
    /home/dev/.claude/settings.json)" \
    || die 'Claude runtime addition could not be fingerprinted'
  PI_RUNTIME_HASH="$(guest_json_projection_hash '.runtimeAdditions.subyardE2E' \
    /home/dev/.pi/agent/settings.json)" \
    || die 'Pi runtime addition could not be fingerprinted'
}

assert_orca_runtime_json_preserved() {
  guest_root jq -e '
    .env.SUBYARD_E2E_RUNTIME_ADDITION == "claude-runtime" and
    (.hooks | length) > 0 and .statusLine != null
  ' /home/dev/.claude/settings.json >/dev/null \
    || die 'Claude runtime additions did not survive configuration materialization'
  guest_root jq -e '
    .runtimeAdditions.subyardE2E == {"enabled": true, "generation": 1}
  ' /home/dev/.pi/agent/settings.json >/dev/null \
    || die 'Pi runtime additions did not survive configuration materialization'
  [ "$(guest_root jq -er '.hooks | length' /home/dev/.claude/settings.json)" = \
    "$CLAUDE_HOOKS_COUNT" ] || die 'Claude hook count changed during configuration materialization'
  [ "$(guest_json_projection_hash '.hooks' /home/dev/.claude/settings.json)" = \
    "$CLAUDE_HOOKS_HASH" ] || die 'Claude hooks changed during configuration materialization'
  [ "$(guest_json_projection_hash '.statusLine' /home/dev/.claude/settings.json)" = \
    "$CLAUDE_STATUS_HASH" ] || die 'Claude status line changed during configuration materialization'
  [ "$(guest_json_projection_hash \
    '{env: {SUBYARD_E2E_RUNTIME_ADDITION: .env.SUBYARD_E2E_RUNTIME_ADDITION}}' \
    /home/dev/.claude/settings.json)" = "$CLAUDE_RUNTIME_HASH" ] \
    || die 'Claude synthetic runtime field changed during configuration materialization'
  [ "$(guest_json_projection_hash '.runtimeAdditions.subyardE2E' \
    /home/dev/.pi/agent/settings.json)" = "$PI_RUNTIME_HASH" ] \
    || die 'Pi synthetic runtime field changed during configuration materialization'
}

report_config_failure() {
  python3 - "$1" <<'PY_REPORT'
import json
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text(errors="replace")[:65536]
for line in text.splitlines():
    try:
        value = json.loads(line)
    except ValueError:
        continue
    if isinstance(value, dict) and "status" in value and "code" in value:
        safe = lambda field: field if isinstance(field, str) and re.fullmatch(r"[a-z-]{1,64}", field) else "unavailable"
        print("config-release-gate: status=" + safe(value["status"]) + " code=" + safe(value["code"]), file=sys.stderr)
        break
else:
    categories = {
        "invalid desired JSON": "invalid-desired-json",
        "invalid JSON materialization observation": "invalid-json-observation",
        "JSON configuration observation failed": "json-observation-failed",
        "JSON configuration apply failed": "json-apply-failed",
        "yard scope requires selecting a non-default yard": "unsupported-default-file-scope",
        "stale": "stale-plan",
    }
    category = next((category for message, category in categories.items() if message in text), "unclassified")
    print("config-error: " + category, file=sys.stderr)
PY_REPORT
}

assert_config_drift() {
  local phase="$1"
  local output="$STATE/config-status-$phase.out" error="$STATE/config-status-$phase.err"
  if yard config status >"$output" 2>"$error"; then
    die "config status did not detect $phase drift"
  fi
  if ! grep -Fq 'agent config drift' "$error"; then
    report_config_failure "$error"
    die "config status did not classify $phase drift"
  fi
}

apply_imported_claude_template() {
  local phase="$1" source="$2"
  yard config import AGENT_claude_CONFIG "$source" --scope host --yes \
    >"$STATE/config-import-$phase.out" 2>"$STATE/config-import-$phase.err" \
    || { report_config_failure "$STATE/config-import-$phase.err"; die "public config import failed for $phase"; }
  assert_config_drift "$phase"
  yard config apply --yes >"$STATE/config-apply-$phase.out" 2>"$STATE/config-apply-$phase.err" \
    || { report_config_failure "$STATE/config-apply-$phase.err"; die "public config apply failed for $phase"; }
  yard config status >"$STATE/config-converged-$phase.out" \
    2>"$STATE/config-converged-$phase.err" \
    || die "config status did not converge after $phase apply"
}

print_materialized_json_diagnostics() {
  local asset destination
  for asset in claude/settings.json pi/settings.json; do
    case "$asset" in
      claude/*) destination=/home/dev/.claude/settings.json ;;
      pi/*) destination=/home/dev/.pi/agent/settings.json ;;
    esac
    compare_json_managed_projection "$ROOT/config/agents/$asset" "$destination" "$asset" 1 \
      || true
  done
}

client_status() {
	local pairing="$1" profile="$2" output
	output="$STATE/$profile-status.json"
  install -d -m 0700 \
    "$STATE/$profile/home" "$STATE/$profile/config" "$STATE/$profile/data" "$STATE/$profile/state"
  HOME="$STATE/$profile/home" \
    XDG_CONFIG_HOME="$STATE/$profile/config" \
    XDG_DATA_HOME="$STATE/$profile/data" \
    XDG_STATE_HOME="$STATE/$profile/state" \
    LIBGL_ALWAYS_SOFTWARE=1 \
    timeout 30 /usr/bin/orca-ide --pairing-code "$pairing" status --json \
    >"$output" 2>"$STATE/$profile-status.err" \
    || die 'stock Orca client could not use the private pairing link'
  jq -e '.ok == true and .result.runtime.reachable == true' "$output" >/dev/null \
    || die 'stock Orca client did not reach the bootstrapped runtime'
}

install_stock_orca_client() {
  local url digest artifact cache
  if [ -x /usr/bin/orca-ide ] && [ "$(dpkg-query -W -f='${Version}' orca-ide 2>/dev/null)" = "$ORCA_VERSION" ]; then
    return
  fi
  stage 'installing a stock Orca client without exposing its pairing capability'
  case "$(dpkg --print-architecture)" in
    amd64) url="$ORCA_DEB_AMD64_URL"; digest="$ORCA_DEB_AMD64_SHA256" ;;
    arm64) url="$ORCA_DEB_ARM64_URL"; digest="$ORCA_DEB_ARM64_SHA256" ;;
    *) die 'unsupported architecture' ;;
  esac
  artifact="$STATE/orca.deb"
  cache="/var/tmp/subyard-orca-$ORCA_VERSION-$digest.deb"
  if printf '%s  %s\n' "$digest" "$cache" | sha256sum -c --status 2>/dev/null; then
    cp "$cache" "$artifact"
  else
    curl --proto '=https' --tlsv1.2 -fsSL \
      --retry 3 --retry-all-errors --connect-timeout 20 --max-time 1200 \
      "$url" -o "$artifact"
    printf '%s  %s\n' "$digest" "$artifact" | sha256sum -c - >/dev/null
    cp "$artifact" "$cache"
  fi
  sudo -n apt-get update -qq
  sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    "$artifact" file jq nftables zlib1g-dev \
    libasound2t64 libgbm1 libgtk-3-0t64 libnss3 >/dev/null
  test -x /usr/bin/orca-ide || die 'stock Orca client is unavailable'
}

prepare_cached_orca_guest() {
  # Focused config/SSH acceptance exercises the real installed runtime and public
  # service reconciliation. Reusing a verified package is not fresh-download evidence.
  local digest cache guest_artifact
  [ "$SSH_AGENT" = 1 ] || [ "$CODEX_CONFIG" = 1 ] || [ "$CODEX_PERMISSIONS" = 1 ] || return 0
  install_stock_orca_client
  case "$(guest_root dpkg --print-architecture)" in
    amd64) digest="$ORCA_DEB_AMD64_SHA256" ;;
    arm64) digest="$ORCA_DEB_ARM64_SHA256" ;;
    *) die 'unsupported guest architecture' ;;
  esac
  cache="/var/tmp/subyard-orca-$ORCA_VERSION-$digest.deb"
  if ! printf '%s  %s\n' "$digest" "$cache" | sha256sum -c --status 2>/dev/null; then
    stage 'no verified Orca package cache; public Orca up will download its pinned release'
    return 0
  fi
  stage 'reusing the checksum-verified stock Orca package for focused runtime acceptance'
  [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ] \
    && [[ "$INSTANCE" = yard-orca-bootstrap-* ]] || die 'guest package target is not fixture-owned'
  guest_artifact="/tmp/subyard-orca-bootstrap-$token.deb"
  incus --project "$PROJECT" file push "$cache" "$INSTANCE$guest_artifact" --mode 0644
  printf '%s  %s\n' "$digest" "$guest_artifact" | guest_root sha256sum -c --status \
    || die 'copied Orca package failed guest checksum verification'
  guest_root timeout 900 env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    "$guest_artifact" >/dev/null || die 'cached Orca package could not be installed in the fixture'
  guest_root rm -f -- "$guest_artifact"
  [ "$(guest_root dpkg-query -W -f='${Version}' orca-ide)" = "$ORCA_VERSION" ] \
    || die 'cached Orca package installed an unexpected version'
}

if [ -n "$RESUME_STATE" ]; then
  # Validate before claiming cleanup ownership: a rejected path must never become
  # a teardown target. Do not source configuration from the retained directory.
  [[ "$RESUME_STATE" =~ ^/var/tmp/subyard-orca-bootstrap\.[A-Za-z0-9]{6,}$ ]] \
    || die 'resume path is not a canonical fixture directory'
  [ "$(readlink -f -- "$RESUME_STATE")" = "$RESUME_STATE" ] \
    || die 'resume path contains a symlink'
  for relative in '' /home /config /config/yards /config/yards/default /data /host; do
    directory="$RESUME_STATE$relative"
    [ -d "$directory" ] && [ ! -L "$directory" ] \
      && [ "$(stat -c %u "$directory")" = "$EUID" ] \
      && [ "$((0$(stat -c %a "$directory") & 022))" -eq 0 ] \
      || die 'resume directory ownership or permissions are unsafe'
  done
  for relative in .marker config/config.env config/yards/default/config.env; do
    file="$RESUME_STATE/$relative"
    [ -f "$file" ] && [ ! -L "$file" ] \
      && [ "$(stat -c %u "$file")" = "$EUID" ] \
      && [ "$((0$(stat -c %a "$file") & 022))" -eq 0 ] \
      || die 'resume marker or configuration is unsafe'
  done
  [ "$(<"$RESUME_STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ] \
    || die 'resume marker does not match this fixture'
  if [ -e "$RESUME_STATE/config/yards/secondary" ] || [ -L "$RESUME_STATE/config/yards/secondary" ]; then
    directory="$RESUME_STATE/config/yards/secondary"
    file="$directory/config.env"
    [ -d "$directory" ] && [ ! -L "$directory" ] && [ -f "$file" ] && [ ! -L "$file" ] \
      && [ "$(stat -c %u "$directory")" = "$EUID" ] && [ "$(stat -c %u "$file")" = "$EUID" ] \
      && [ "$((0$(stat -c %a "$directory") & 022))" -eq 0 ] \
      && [ "$((0$(stat -c %a "$file") & 022))" -eq 0 ] \
      || die 'resume secondary-yard configuration is unsafe'
  fi
  STATE="$RESUME_STATE"
else
  STATE="$(mktemp -d /var/tmp/subyard-orca-bootstrap.XXXXXX)"
  printf '%s\n' subyard-orca-bootstrap-e2e-v1 > "$STATE/.marker"
  chmod 0600 "$STATE/.marker"
fi
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
PROJECT="subyard-orca-bootstrap-$token"
INSTANCE="yard-orca-bootstrap-$token"

bash "$ROOT/dev/build-engine.sh" >/dev/null
# Keep the disposable VM's prepared toolchain caches when isolating the operator home.
GOMODCACHE="$(go env GOMODCACHE)"
GOCACHE="$(go env GOCACHE)"
export GOMODCACHE GOCACHE
if [ -z "$RESUME_STATE" ]; then
  install -d -m 0700 "$STATE/home" "$STATE/config/yards/default" "$STATE/data" "$STATE/host"
fi
export HOME="$STATE/home"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$ORIGINAL_HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
coding_integrations=''
yard_profiles=subyard-dev
[ "$SSH_AGENT" = 0 ] || yard_profiles=orca
if [ "$CODEX_PERMISSIONS" = 1 ]; then
  coding_integrations=codex
  yard_profiles=orca
fi
if [ "$EXISTING_YARD" = 1 ]; then
  coding_integrations='claude pi'
  if [ "$CODEX_CONFIG" = 1 ]; then coding_integrations+=' codex'; fi
  yard_profiles='subyard-dev orca'
fi
if [ -z "$RESUME_STATE" ]; then
  SSH_PORT="$(free_port)"
  cat > "$SUBYARD_CONFIG_HOME/config.env" <<EOF
SSH_PORT=$SSH_PORT
INCUS_PROJECT=$PROJECT
YARD_INSTANCE_NAME=$INSTANCE
CODING_TOOL_INTEGRATIONS="$coding_integrations"
ORCA_ADVERTISE_HOST=127.0.0.1
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
  printf 'ENVIRONMENT_PROFILES=%q\n' "$yard_profiles" \
    > "$SUBYARD_CONFIG_HOME/yards/default/config.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/config.env" \
    "$SUBYARD_CONFIG_HOME/yards/default/config.env"
else
  [ "$(setting_value INCUS_PROJECT)" = "$PROJECT" ] \
    && [ "$(setting_value YARD_INSTANCE_NAME)" = "$INSTANCE" ] \
    && [ "$(setting_value HOST_BASE)" = "$STATE/host" ] \
    && [ "$(setting_value CODING_TOOL_INTEGRATIONS)" = '<unset>' ] \
    && [ "$(setting_value ENVIRONMENT_PROFILES)" = orca ] \
    || die 'resume configuration does not describe this SSH fixture'
  SSH_PORT="$(setting_value SSH_PORT)"
  [[ "$SSH_PORT" =~ ^[0-9]+$ ]] || die 'resume SSH port is invalid'
  if [ -f "$SUBYARD_CONFIG_HOME/yards/secondary/config.env" ]; then
    [ "$(yard -Y secondary config show INCUS_PROJECT | awk -F': ' '$1 == "effective" {print $2}')" = "$PROJECT-secondary" ] \
      && [ "$(yard -Y secondary config show YARD_INSTANCE_NAME | awk -F': ' '$1 == "effective" {print $2}')" = "$INSTANCE-secondary" ] \
      || die 'resume secondary configuration targets another fixture'
  fi
  stage "resuming marked SSH fixture: $STATE"
fi
FIXTURE_READY=1

if [ "$CODEX_CONFIG" = 1 ]; then
  # This acceptance owns CONFIG/RULES semantics, not Codex installation or execution.
  # Use an ordinary fixture hook so a CLI download cannot hide that regression.
  printf '#!/bin/sh\nexit 0\n' > "$STATE/codex-config-provision.sh"
  chmod 0600 "$STATE/codex-config-provision.sh"
  {
    printf 'AGENT_codex_PROVISION=%q\n' "$STATE/codex-config-provision.sh"
    printf 'AGENT_codex_COMMAND=true\nAGENT_codex_CHECK=true\n'
  } >> "$SUBYARD_CONFIG_HOME/config.env"
fi

if [ "$SSH_AGENT" = 1 ]; then
  install_stock_orca_client
  stage 'initializing the candidate and stock Orca for focused SSH agent acceptance'
  yard init --yes
  prepare_cached_orca_guest
  yard orca up --yes
  ORCA_PORT="$(setting_value ORCA_HOST_PORT)"
  assert_orca_readiness
  install_stock_orca_client
  pairing="$(yard orca pair --yes | tail -n1)"
  case "$pairing" in orca://pair\?code=*) ;; *) die 'Orca did not return a private pairing link' ;; esac
  client_status "$pairing" ssh-agent-client
  # shellcheck source=tests/real-host/ssh-agent.sh
  . "$ROOT/tests/real-host/ssh-agent.sh"
  exit 0
fi

if [ "$CODEX_PERMISSIONS" = 1 ]; then
  install_stock_orca_client
  stage 'initializing the candidate and stock Orca for focused Codex permissions acceptance'
  yard init --yes
  prepare_cached_orca_guest
  yard orca up --yes
  ORCA_PORT="$(setting_value ORCA_HOST_PORT)"
  assert_orca_readiness
  pairing="$(yard orca pair --yes | tail -n1)"
  case "$pairing" in orca://pair\?code=*) ;; *) die 'Orca did not return a private pairing link' ;; esac
  client_status "$pairing" codex-permissions-client
  # shellcheck source=tests/real-host/codex-yard.sh
  . "$ROOT/tests/real-host/codex-yard.sh"
  exit 0
fi

stage 'installing a packaged candidate through the public release installer'
# The VM receives public source without Git metadata. Give packaging its normal
# tracked-file allowlist in a disposable copy, without altering the checkout.
bash "$ROOT/tests/helpers/source-files.sh" >"$STATE/source-files"
mkdir "$STATE/source"
tar -C "$ROOT" --null -T "$STATE/source-files" -cf - | tar -C "$STATE/source" -xf -
git -C "$STATE/source" init --quiet
git -C "$STATE/source" add --all
if [ -n "$UPGRADE_FROM" ]; then
  # The predecessor must be converged: its released updater cannot change
  # targets while an existing activation needs repair. Only the candidate
  # introduces this new desired value, in both local yards.
  python3 - "$STATE/source/config/agents/claude/settings.json" <<'PY_UPGRADE_DEFAULT'
import json
import pathlib
import sys
path = pathlib.Path(sys.argv[1])
value = json.loads(path.read_text())
value["autoMemoryDirectory"] = "~/.claude-memory-upgrade-fixture"
path.write_text(json.dumps(value) + "\n")
PY_UPGRADE_DEFAULT
fi
release_version=0.13.3-orca-bootstrap-e2e
bash "$STATE/source/dev/package-engine.sh" --version "$release_version" \
  --output-dir "$STATE/release" >/dev/null
if [ -n "$UPGRADE_FROM" ]; then
  stage "installing published Subyard $UPGRADE_FROM and starting Orca before the upgrade"
  published_installer="$STATE/published-install.sh"
  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 --connect-timeout 20 --max-time 120 \
    "https://github.com/subyard/subyard/releases/download/v$UPGRADE_FROM/subyard-install.sh" \
    -o "$published_installer"
  printf '%s  %s\n' "$UPGRADE_INSTALLER_SHA256" "$published_installer" | sha256sum -c - >/dev/null
  YARD_RELEASE_VERSION="$UPGRADE_FROM" YARD_BIN_DIR="$HOME/.local/bin" SHELL=/bin/bash \
    bash "$published_installer" --version "$UPGRADE_FROM" --yes >"$STATE/published-install.out" 2>&1 \
    || die 'published release installation failed'
  YARD_BIN="$HOME/.local/bin/yard"
  [ "$(yard --version)" = "yard $UPGRADE_FROM" ] || die 'published release is not active'
  published_target="$(readlink "$SUBYARD_HOME/runtime/current")"
  yard init --yes >"$STATE/published-init.out" 2>"$STATE/published-init.err" \
    || die 'published yard initialization failed'
  yard update --offline --version "$UPGRADE_FROM" --yes \
    >"$STATE/published-transition.out" 2>"$STATE/published-transition.err" \
    || die 'published release transition did not complete before the upgrade fixture'
  yard orca up --yes >"$STATE/published-orca-up.out" 2>"$STATE/published-orca-up.err" \
    || die 'published Orca startup failed'
  ORCA_PORT="$(setting_value ORCA_HOST_PORT)"
  assert_orca_readiness
  capture_orca_runtime_json
  pairing="$(yard orca pair --yes | tail -n1)"
  case "$pairing" in orca://pair\?code=*) ;; *) die 'published Orca did not return a pairing link' ;; esac
  install_stock_orca_client
  client_status "$pairing" upgrade-client

  stage 'preparing another local yard for changed candidate defaults'
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/secondary"
  cat > "$SUBYARD_CONFIG_HOME/yards/secondary/config.env" <<EOF
INCUS_PROJECT=$PROJECT-secondary
YARD_INSTANCE_NAME=$INSTANCE-secondary
SSH_PORT=$(free_port)
ENVIRONMENT_PROFILES=""
CODING_TOOL_INTEGRATIONS=claude
EOF
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/secondary/config.env"
  yard -Y secondary init --yes >"$STATE/secondary-init.out" 2>"$STATE/secondary-init.err" \
    || die 'secondary yard initialization failed'
  yard -Y secondary start --yes >"$STATE/secondary-start.out" 2>"$STATE/secondary-start.err" \
    || die 'secondary yard did not start before the upgrade'
  yard config status --all-local >"$STATE/before-upgrade-config.out" 2>"$STATE/before-upgrade-config.err" \
    || die 'published release configs are not converged before the upgrade'
  rg -Fxq 'yard secondary materialized-config: converged' "$STATE/before-upgrade-config.out" \
    || die 'secondary yard must be running and converged before the upgrade'

  stage "updating published Subyard $UPGRADE_FROM through the public update command"
  YARD_RELEASE_BASE_URL="file://$STATE/release" yard update --version "$release_version" --yes \
    >"$STATE/upgrade.out" 2>"$STATE/upgrade.err" \
    || {
      KEEP_FAILED_UPGRADE=1
      report_config_failure "$STATE/upgrade.out"
      report_config_failure "$STATE/upgrade.err"
      python3 - "$STATE/upgrade.err" <<'PY_UPGRADE_ERROR'
import pathlib
import re
import sys

# This fixture uses public desired configs and synthetic identities. Bound the
# diagnostic and remove capabilities before the marked temporary state is cleaned.
text = pathlib.Path(sys.argv[1]).read_text(errors="replace")[-8192:]
text = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", text)
text = re.sub(r"orca://\S+", "<redacted>", text)
text = re.sub(r"(?im)^.*(?:authorization|bearer|pairing|token|secret|password).*$", "<redacted>", text)
text = re.sub(r"[A-Za-z0-9_+/=-]{64,}", "<redacted>", text)
for line in text.splitlines()[-20:]:
    print("upgrade-error: " + line, file=sys.stderr)
PY_UPGRADE_ERROR
      die 'published release upgrade failed'
    }
  [ "$(yard --version)" = "yard $release_version" ] || die 'upgrade did not activate the candidate'
  installed_target="$(readlink "$SUBYARD_HOME/runtime/current")"
  [ "$installed_target" != "$published_target" ] || die 'upgrade retained the predecessor'
  INSTALLED_RELEASE="${installed_target#releases/}"
  [ "$(readlink "$SUBYARD_HOME/runtime/previous")" = "$published_target" ] \
    || die 'upgrade did not retain the published predecessor'
  release_ready upgraded || { print_release_state upgraded; die 'upgraded release is not ready'; }
  yard config status --all-local >"$STATE/upgrade-config.out" 2>"$STATE/upgrade-config.err" \
    || die 'upgrade left materialized config drift in a local yard'
  rg -Fxq 'yard secondary materialized-config: converged' "$STATE/upgrade-config.out" \
    || die 'secondary yard must remain running and converged after the upgrade'
  yard orca restart --yes >"$STATE/upgrade-restart.out" 2>"$STATE/upgrade-restart.err" \
    || die 'Orca restart was blocked after the upgrade'
  yard orca status >"$STATE/upgrade-status.out" 2>"$STATE/upgrade-status.err" \
    || die 'upgraded Orca status failed'
  [ "$(setting_value ORCA_HOST_PORT)" = "$ORCA_PORT" ] || die 'upgrade changed the Orca endpoint'
  assert_materialized_json "$STATE/source/config/agents/claude/settings.json" \
    "$STATE/source/config/agents/pi/settings.json" || die 'upgrade did not converge managed JSON fields'
  assert_orca_runtime_json_preserved
  assert_orca_readiness
  client_status "$pairing" upgrade-client
  printf 'ok: published %s upgraded to the candidate; Orca remained ready with its saved grant and runtime JSON\n' "$UPGRADE_FROM"
  exit 0
fi

YARD_RELEASE_BASE_URL="file://$STATE/release" YARD_RELEASE_VERSION="$release_version" \
  YARD_BIN_DIR="$HOME/.local/bin" SHELL=/bin/bash \
  "$STATE/release/subyard-install.sh" --yes >/dev/null
YARD_BIN="$HOME/.local/bin/yard"
[ "$(yard --version)" = "yard $release_version" ] || die 'packaged candidate is not active'
installed_target="$(readlink "$SUBYARD_HOME/runtime/current")"
case "$installed_target" in
  releases/*) ;;
  *) die 'packaged candidate current link is not a canonical release target' ;;
esac
INSTALLED_RELEASE="${installed_target#releases/}"
[ -n "$INSTALLED_RELEASE" ] && [ -d "$SUBYARD_HOME/runtime/releases/$INSTALLED_RELEASE" ] \
  || die 'packaged candidate release directory is unavailable'
stage 'checking the installed release before Orca bootstrap'
yard update --check --offline --version "$release_version"

if [ "$EXISTING_YARD" = 1 ]; then
  stage 'initializing an existing yard with Orca and materialized agent configs selected'
  yard init --yes
  stage 'completing and verifying the public release transition before Orca startup'
  yard update --offline --version "$release_version" --yes \
    >"$STATE/release-before-up.out" 2>"$STATE/release-before-up.err" \
    || die 'public release update did not complete before Orca startup'
  if ! release_ready before-up; then
    print_release_state before-up
    die 'release was not ready before Orca startup'
  fi
  assert_materialized_json \
    || die 'existing yard agent configuration was not materialized before Orca startup'
fi

stage 'holding the preferred owner port to exercise automatic collision selection'
python3 -m http.server 6768 --bind 127.0.0.1 \
  >"$STATE/collision.out" 2>"$STATE/collision.err" &
COLLISION_PID=$!
for _ in $(seq 1 30); do
  kill -0 "$COLLISION_PID" 2>/dev/null || {
    sed -n '1,40p' "$STATE/collision.err" >&2
    die 'preferred-port collision listener exited'
  }
  python3 -c 'import socket; s=socket.create_connection(("127.0.0.1", 6768), 0.2); s.close()' \
    >/dev/null 2>&1 && break
  sleep 0.1
done
python3 -c 'import socket; s=socket.create_connection(("127.0.0.1", 6768), 0.2); s.close()' \
  >/dev/null 2>&1 || die 'preferred-port collision listener did not become ready'

if [ "$EXISTING_YARD" = 1 ]; then
  stage 'starting Orca interactively in an initialized release-ready yard'
else
  stage 'bootstrapping an uninitialized yard interactively with one confirmation'
fi
[ "$CODEX_CONFIG" = 0 ] || prepare_cached_orca_guest
export SUBYARD_TTY_TEST_ENGINE="$YARD_BIN"
# A real controlling terminal catches background-handler stops that --yes and
# redirected stdin hide. Enter accepts the normal top-level default-yes prompt.
env -u ASSUME_YES timeout --signal=TERM --kill-after=5s 1800 \
  script --quiet --return --command 'exec "$SUBYARD_TTY_TEST_ENGINE" orca up' /dev/null \
  <<< '' >"$STATE/bootstrap-terminal.out" 2>&1 \
  || { tail -n 60 "$STATE/bootstrap-terminal.out" >&2; die 'interactive Orca bootstrap failed'; }
[ "$(grep -Fc 'Proceed? [Y/n]' "$STATE/bootstrap-terminal.out")" = 1 ] \
  || die 'interactive Orca bootstrap did not use exactly one default-yes confirmation'
status_reached=0
timeout --signal=TERM --kill-after=5s 60 \
  script --quiet --return --command 'exec "$SUBYARD_TTY_TEST_ENGINE" orca status' /dev/null \
  >"$STATE/status-terminal.out" 2>&1 \
  && grep -Fq 'Orca profile selected for yard init' "$STATE/status-terminal.out" \
  && status_reached=1
stage 'checking release convergence after Orca bootstrap'
if [ "$EXISTING_YARD" = 1 ]; then
  if ! release_ready after-up; then
    print_release_state after-up
    print_materialized_json_diagnostics
    if [ "$status_reached" != 1 ]; then
      sed -n '1,40p' "$STATE/status-terminal.out" >&2
    fi
    die 'release was not ready after Orca startup'
  fi
  assert_materialized_json || die 'Orca startup changed a Subyard-managed JSON field'
else
  yard update --check --offline --version "$release_version"
fi
[ "$status_reached" = 1 ] \
  || { sed -n '1,40p' "$STATE/status-terminal.out" >&2; die 'interactive status did not reach the Orca resource'; }
profiles="$(setting_value ENVIRONMENT_PROFILES)"
[ "$profiles" = 'subyard-dev orca' ] \
  || die "Orca bootstrap did not preserve the selected profiles: $profiles"
[ "$(setting_value ORCA_ADVERTISE_HOST)" = 127.0.0.1 ] \
  || die 'fresh Orca bootstrap did not retain the explicit loopback endpoint'
ORCA_PORT="$(setting_value ORCA_HOST_PORT)"
case "$ORCA_PORT" in
  ''|*[!0-9]*) die 'fresh Orca bootstrap did not select a numeric endpoint port' ;;
esac
[ "$ORCA_PORT" != 6768 ] || die 'automatic endpoint selection reused the occupied preferred port'
guest_root systemctl is-active --quiet subyard-orca.service \
  || die 'Orca did not start through the production bootstrap path'
guest_root test -x /usr/local/libexec/subyard/projects-changed \
  || die 'Orca bootstrap did not run the existing init reconciler'
[ "$(incus --project "$PROJECT" config device get "$INSTANCE" orca-server listen)" = \
  "tcp:127.0.0.1:$ORCA_PORT" ] || die 'Orca bootstrap published the wrong owner endpoint'
assert_orca_readiness
if [ "$CODEX_CONFIG" = 1 ]; then
  stage 'checking Codex TOML runtime additions through public apply and Orca restart'
  install_stock_orca_client
  pairing="$(yard orca pair --yes | tail -n1)"
  case "$pairing" in orca://pair\?code=*) ;; *) die 'Orca pair returned no private link' ;; esac
  client_status "$pairing" paired-client
  # Seed representative runtime additions explicitly: plain serve readiness does
  # not exercise every desktop integration that can add hooks/projects/tui.
  guest_root python3 -c '
import pathlib, sys, tomllib
writer = {}
exec(sys.stdin.read(), writer)
path = pathlib.Path("/home/dev/.codex/config.toml")
value = tomllib.loads(path.read_text())
for table in ("hooks", "projects", "tui"):
    value.setdefault(table, {})["subyard_e2e"] = {"preserved": True}
path.write_text(writer["dumps"](value))
' < "$ROOT/internal/adapters/configmaterial/tomli_w.py"
  yard config status >/dev/null || die 'runtime-only Codex fields caused config drift'
  release_ready codex-runtime-additions || die 'runtime-only Codex fields blocked release readiness'
  guest_root python3 -c '
import pathlib, sys, tomllib
writer = {}
exec(sys.stdin.read(), writer)
path = pathlib.Path("/home/dev/.codex/config.toml")
value = tomllib.loads(path.read_text())
value["approval_policy"] = "never"
path.write_text(writer["dumps"](value))
' < "$ROOT/internal/adapters/configmaterial/tomli_w.py"
  assert_config_drift codex-managed-policy
  yard config apply --yes >"$STATE/codex-apply.out" 2>"$STATE/codex-apply.err" \
    || die 'public config apply could not repair managed Codex drift'
  yard orca restart --yes >/dev/null || die 'Codex config blocked Orca restart'
  assert_orca_readiness
  client_status "$pairing" paired-client
  yard migrate --check --json >"$STATE/codex-migrate.json"
  jq -e '.outcome.status == "ready" and .outcome.reachedGoal == true' \
    "$STATE/codex-migrate.json" >/dev/null || die 'release drift returned after restart/reconnect'
  guest_root python3 -c '
import pathlib, tomllib
value = tomllib.loads(pathlib.Path("/home/dev/.codex/config.toml").read_text())
assert value["approval_policy"] == "on-request"
assert all(value[table]["subyard_e2e"] == {"preserved": True} for table in ("hooks", "projects", "tui"))
' || die 'Codex runtime fields or managed policy were not preserved'
  printf 'ok: Codex runtime fields preserved; managed drift repaired; Orca restart and saved-client readiness passed\n'
  exit 0
fi
if [ "$EXISTING_YARD" = 1 ]; then
  stage 'preserving runtime JSON additions through public config import and apply'
  capture_orca_runtime_json
  desired_one="$STATE/claude-desired-one"
  jq '
    .autoMemoryDirectory = "~/.claude-memory-e2e" |
    .permissions.defaultMode = "acceptEdits" |
    .env = {"SUBYARD_E2E_MANAGED_SETTING": "first"}
  ' "$ROOT/config/agents/claude/settings.json" >"$desired_one"
  chmod 0600 "$desired_one"
  apply_imported_claude_template first "$desired_one"
  assert_materialized_json "$desired_one" "$ROOT/config/agents/pi/settings.json" \
    || die 'first imported desired JSON fields were not materialized'
  guest_root jq -e '
    .autoMemoryDirectory == "~/.claude-memory-e2e" and
    .permissions.defaultMode == "acceptEdits" and
    .env.SUBYARD_E2E_MANAGED_SETTING == "first"
  ' /home/dev/.claude/settings.json >/dev/null \
    || die 'first imported managed settings were not honored'
  assert_orca_runtime_json_preserved
  assert_orca_readiness

  stage 'retiring one managed JSON field while retaining its runtime sibling'
  desired_two="$STATE/claude-desired-two"
  jq 'del(.env.SUBYARD_E2E_MANAGED_SETTING)' "$desired_one" >"$desired_two"
  chmod 0600 "$desired_two"
  apply_imported_claude_template retired "$desired_two"
  assert_materialized_json "$desired_two" "$ROOT/config/agents/pi/settings.json" \
    || die 'second imported desired JSON fields were not materialized'
  guest_root jq -e '
    (.env | type == "object") and
    (.env | has("SUBYARD_E2E_MANAGED_SETTING") | not) and
    .env.SUBYARD_E2E_RUNTIME_ADDITION == "claude-runtime"
  ' /home/dev/.claude/settings.json >/dev/null \
    || die 'retired managed field or its nested runtime sibling was handled incorrectly'
  assert_orca_runtime_json_preserved
  assert_orca_readiness
fi
if [ ! -f "$platform_marker" ]; then
  incus info >/dev/null
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
  install -d -m 0700 "${platform_marker%/*}"
  printf '%s\n' subyard-e2e-platform-v1 > "$platform_marker"
  chmod 0600 "$platform_marker"
fi
[ "$(<"$platform_marker")" = subyard-e2e-platform-v1 ] \
  || die 'unexpected shared E2E platform marker'

kill -TERM "$COLLISION_PID"
wait "$COLLISION_PID" 2>/dev/null || true
COLLISION_PID=''

install_stock_orca_client

pairing="$(yard orca pair --yes | tail -n1)"
case "$pairing" in
  orca://pair\?code=*) ;;
  *) die 'Orca pair did not return a private stock pairing link' ;;
esac
client_status "$pairing" paired-client

stage 'connecting mobile-scoped clients through stock CLI and restoring Desktop pairing'
mobile_pair_one="$STATE/mobile-pair-one.out"
mobile_pair_two="$STATE/mobile-pair-two.out"
desktop_pair_after_mobile="$STATE/desktop-pair-after-mobile.out"
for pairing_output in "$mobile_pair_one" "$mobile_pair_two" "$desktop_pair_after_mobile" \
  "$STATE/mobile-pair-one.err" "$STATE/mobile-pair-two.err" "$STATE/desktop-pair-after-mobile.err"; do
  install -m 0600 /dev/null "$pairing_output"
done
yard orca pair --mobile --yes >"$mobile_pair_one" 2>"$STATE/mobile-pair-one.err"
assert_orca_readiness mobile
assert_pairing_file "$mobile_pair_one" mobile
guest_root test ! -e /srv/agents/orca/ready.json.mobile-request \
  || die 'Orca consumed mobile pairing request was not cleared'
client_status "$pairing" paired-client
# Authenticate the first mobile grant: stock Orca reuses an unconsumed offer.
client_status "$(tail -n1 "$mobile_pair_one")" mobile-client-one
yard orca pair --mobile --yes >"$mobile_pair_two" 2>"$STATE/mobile-pair-two.err"
assert_orca_readiness mobile
assert_pairing_file "$mobile_pair_two" mobile
assert_fresh_pairing_files "$mobile_pair_one" "$mobile_pair_two"
client_status "$(tail -n1 "$mobile_pair_two")" mobile-client-two
client_status "$(tail -n1 "$mobile_pair_one")" mobile-client-one
yard orca restart --yes >/dev/null
assert_orca_readiness runtime
guest_root test ! -e /srv/agents/orca/ready.json.mobile-request \
  || die 'ordinary restart retained a mobile pairing request'
client_status "$pairing" paired-client
yard orca pair --yes >"$desktop_pair_after_mobile" 2>"$STATE/desktop-pair-after-mobile.err"
assert_orca_readiness runtime
assert_pairing_file "$desktop_pair_after_mobile" runtime
desktop_pair_after_mobile="$(tail -n1 "$desktop_pair_after_mobile")"
client_status "$desktop_pair_after_mobile" desktop-after-mobile-client
client_status "$(tail -n1 "$mobile_pair_one")" mobile-client-one
assert_no_pairing_capability_logs
if [ "$EXISTING_YARD" = 1 ]; then
  stage 'syncing and applying a tracked desired config while preserving runtime state and the grant'
  sync_host_id="$(<"$SUBYARD_CONFIG_HOME/host-id")"
  case "$sync_host_id" in
    ''|*[!A-Za-z0-9._-]*) die 'config sync host identity is unavailable' ;;
  esac
  sync_source="$STATE/config-sync-source"
  sync_claude="$sync_source/hosts/$sync_host_id/overrides/agents/claude/settings.json"
  install -d -m 0700 "${sync_claude%/*}"
  desired_sync="$STATE/claude-desired-sync"
  jq '.autoMemoryDirectory = "~/.claude-memory-e2e-sync"' "$desired_two" >"$desired_sync"
  chmod 0600 "$desired_sync"
  cp "$desired_sync" "$sync_claude"
  chmod 0600 "$sync_claude"
  printf '{"schemaVersion":1}\n' >"$sync_source/subyard-config.json"
  chmod 0600 "$sync_source/subyard-config.json"
  git -C "$sync_source" init --quiet
  git -C "$sync_source" add --all
  git -C "$sync_source" -c user.name='Subyard Test' -c user.email=test@invalid \
    commit --quiet -m 'Update managed agent configuration'
  yard config sync "$sync_source" --adopt --apply --yes \
    >"$STATE/config-sync-apply.out" 2>"$STATE/config-sync-apply.err" \
    || { report_config_failure "$STATE/config-sync-apply.err"; die 'public config sync --apply did not complete'; }
  yard config sync "$sync_source" --check \
    >"$STATE/config-sync-check.out" 2>"$STATE/config-sync-check.err" \
    || { report_config_failure "$STATE/config-sync-check.err"; die 'public config sync did not converge'; }
  yard config status >"$STATE/config-sync-status.out" 2>"$STATE/config-sync-status.err" \
    || die 'config sync --apply did not converge materialized settings'
  assert_materialized_json "$desired_sync" "$ROOT/config/agents/pi/settings.json" \
    || die 'config sync --apply did not materialize its managed JSON fields'
  guest_root jq -e '
    .autoMemoryDirectory == "~/.claude-memory-e2e-sync" and
    (.env | has("SUBYARD_E2E_MANAGED_SETTING") | not)
  ' /home/dev/.claude/settings.json >/dev/null \
    || die 'config sync --apply did not honor the tracked managed settings'
  assert_orca_runtime_json_preserved
  assert_orca_readiness
  client_status "$pairing" paired-client

  stage 'updating the installed candidate while preserving JSON additions and the saved grant'
  yard update --offline --version "$release_version" --yes \
    >"$STATE/release-after-json.out" 2>"$STATE/release-after-json.err" \
    || die 'public release update failed after runtime JSON additions'
  if ! release_ready after-json; then
    print_release_state after-json
    die 'release was not ready after the runtime JSON preservation update'
  fi
  assert_materialized_json "$desired_sync" "$ROOT/config/agents/pi/settings.json" \
    || die 'release update changed an imported managed JSON field'
  assert_orca_runtime_json_preserved
  assert_orca_readiness
  client_status "$pairing" paired-client
fi

stage 'preserving the selected endpoint and grant across restart and down/up'
yard orca restart --yes >/dev/null
assert_orca_readiness
if [ "$EXISTING_YARD" = 1 ] && ! release_ready after-restart; then
  print_release_state after-restart
  die 'release was not ready after Orca restart'
fi
client_status "$pairing" paired-client
yard orca down --yes >/dev/null
yard orca up --yes >/dev/null
[ "$(setting_value ORCA_HOST_PORT)" = "$ORCA_PORT" ] \
  || die 'down/up changed the saved endpoint port'
[ "$(setting_value ORCA_ADVERTISE_HOST)" = 127.0.0.1 ] \
  || die 'down/up changed the explicit loopback endpoint'
[ "$(setting_value ENVIRONMENT_PROFILES)" = 'subyard-dev orca' ] \
  || die 'down/up lost the selected Orca profile'
[ "$(incus --project "$PROJECT" config device get "$INSTANCE" orca-server listen)" = \
  "tcp:127.0.0.1:$ORCA_PORT" ] || die 'down/up changed the published owner endpoint'
assert_orca_readiness
if [ "$EXISTING_YARD" = 1 ]; then
  assert_materialized_json "$desired_sync" "$ROOT/config/agents/pi/settings.json" \
    || die 'restart/down-up changed an imported managed JSON field'
  assert_orca_runtime_json_preserved
  if ! release_ready after-down-up; then
    print_release_state after-down-up
    die 'release was not ready after Orca down/up'
  fi
fi
client_status "$pairing" paired-client

printf 'ok: fresh Orca bootstrap reconciled the yard and preserved its endpoint and grant\n'

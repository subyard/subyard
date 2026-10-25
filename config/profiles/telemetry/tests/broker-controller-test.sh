#!/usr/bin/env bash
# Optional correlation and observer calls preserve native controller boundaries.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d /tmp/subyard-telemetry-controller.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
STATE_ROOT="$tmp/controller"
install -d -m 0700 "$STATE_ROOT"
printf '%032d\n' 1 > "$STATE_ROOT/client-id"
chmod 0600 "$STATE_ROOT/client-id"
facade_request() {
  case "$1" in
    status) printf '%s\n' '{"schema_version":1,"status":"ok","capabilities":["resource-observe-v1"]}' ;;
    'observe-v1 00000000000000000000000000000001 - 100')
      printf '%s\n' '{"schema_version":1,"status":"ok","observation":{"events":[],"observations":[],"next_cursor":"-","gap":false,"more":false}}' ;;
    *) return 91 ;;
  esac
}
configure_yard_scope() { :; }
main --observe - | jq -e '.status == "ok" and .observation.events == []' >/dev/null
if (main --observe invalid) >"$tmp/refusal" 2>&1; then exit 1; fi
export SUBYARD_AGENT_RUN_ID=fixture-agent-run SUBYARD_TELEMETRY_DATA_DIR="$tmp/data"
# Consumed by the native correlation function sourced above.
# shellcheck disable=SC2034
LEASE_YARD=yard LEASE_PROJECT=project LEASE_RUN=12345678 LEASE_SLOT=slot-001 LEASE_GENERATION=2 LEASE_EPOCH=3
telemetry_correlation held
python3 -B - "$tmp/data" "$ROOT/config/profiles/telemetry" <<'PY'
import json
from pathlib import Path
import sys
root = Path(sys.argv[1])
sys.path.insert(0, sys.argv[2])
import sources
sources.merge_intents(root, root/'sources')
config = json.loads(next((root / 'sources').glob('*.json')).read_text())
binding = config['associations']['12345678']
assert binding['agent_run_id'] == 'fixture-agent-run'
assert binding['resource_generation'] == 2 and binding['lease_epoch'] == 3
assert next((root / 'sources').glob('*.json')).stat().st_mode & 0o777 == 0o600
PY
# An unavailable observer cannot replace native controller success.
SUBYARD_TELEMETRY_DATA_DIR="$tmp/unavailable/file"
printf unavailable > "$tmp/unavailable"
telemetry_correlation held
printf 'ok: broker observer reads and optional explicit correlation\n'

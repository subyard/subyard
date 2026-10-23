#!/usr/bin/env bash
# Native Claude root-managed policy. Run only through dev/agent-e2e.sh.
set -euo pipefail
[ "${SUBYARD_E2E_VM:-}" = 1 ] || exit 2
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
fixture="$(mktemp -d /tmp/subyard-claude-native.XXXXXX)"
trap 'rm -rf -- "$fixture"' EXIT
if [ "${1:-}" != --installed-policy ]; then
  sudo -n bash "$ROOT/config/agents/claude/provision.sh"
fi
if ! python3 -m venv "$fixture/venv"; then
  sudo -n apt-get update -qq
  sudo -n apt-get install -y python3-venv
  python3 -m venv "$fixture/venv"
fi
"$fixture/venv/bin/pip" install --disable-pip-version-check -q claude-agent-sdk
result=0
"$fixture/venv/bin/python" -B "$ROOT/config/agents/claude/tests/e2e/permission-policy.py" \
  "$ROOT/config/agents/claude/settings.json" "$fixture/evidence" || result=$?
if [ "$result" = 0 ]; then
  PARITY_HOST_PROBE=1 \
    "$fixture/venv/bin/python" -B "$ROOT/config/agents/claude/tests/e2e/permission-policy.py" \
    "$ROOT/config/agents/claude/settings.json" "$fixture/evidence" || result=$?
fi
# These logs contain only fixture commands and the synthetic loopback model exchange.
for log in "$fixture"/evidence/*.jsonl; do
  [ ! -f "$log" ] || cat "$log"
done
exit "$result"

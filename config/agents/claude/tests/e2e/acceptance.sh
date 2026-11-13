#!/usr/bin/env bash
# Native Claude CLI and root-managed policy. Run only through dev/agent-e2e.sh.
set -euo pipefail
[ "${SUBYARD_E2E_VM:-}" = 1 ] || exit 2
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
fixture="$(mktemp -d /tmp/subyard-claude-native.XXXXXX)"
trap 'rm -rf -- "$fixture"' EXIT
if [ "${1:-}" != --installed-policy ]; then
  sudo -n bash "$ROOT/config/agents/claude/provision.sh"
fi
if [ "${1:-}" = --install-only ]; then
  version="$(claude --version)"
  claude --help >/dev/null
  before="$(sha256sum /opt/subyard/claude/claude)"
  sudo -n bash "$ROOT/config/agents/claude/provision.sh"
  [ "$before" = "$(sha256sum /opt/subyard/claude/claude)" ]
  claude-policy-check
  sudo -n chmod 0777 /opt/subyard/claude
  if claude-policy-check >/dev/null 2>&1; then
    printf 'FAIL: writable runtime directory passed readiness\n' >&2
    exit 1
  fi
  sudo -n chmod 0755 /opt/subyard/claude
  claude-policy-check
  printf 'ok: native Claude CLI installation, repeat provision and directory drift: %s\n' "$version"
  exit 0
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
if [ "$result" = 0 ]; then
  # The SDK bundles its own CLI; yard sessions run the managed one.
  PARITY_CLAUDE_CLI=/usr/local/bin/claude \
    "$fixture/venv/bin/python" -B "$ROOT/config/agents/claude/tests/e2e/permission-policy.py" \
    "$ROOT/config/agents/claude/settings.json" "$fixture/evidence-managed" || result=$?
fi
if [ "$result" = 0 ] && [ "${1:-}" != --installed-policy ]; then
  # Prompts off removes only the ask rules; restore the default mode afterwards.
  sudo -n env CODING_TOOL_GIT_APPROVAL=off bash "$ROOT/config/agents/claude/provision.sh"
  claude-policy-check
  PARITY_GIT_APPROVAL=off PARITY_CLAUDE_CLI=/usr/local/bin/claude \
    "$fixture/venv/bin/python" -B "$ROOT/config/agents/claude/tests/e2e/permission-policy.py" \
    "$ROOT/config/agents/claude/settings.json" "$fixture/evidence-off" || result=$?
  sudo -n bash "$ROOT/config/agents/claude/provision.sh"
  claude-policy-check
fi
# These logs contain only fixture commands and the synthetic loopback model exchange.
for log in "$fixture"/evidence/*.jsonl "$fixture"/evidence-managed/*.jsonl "$fixture"/evidence-off/*.jsonl; do
  [ ! -f "$log" ] || cat "$log"
done
exit "$result"

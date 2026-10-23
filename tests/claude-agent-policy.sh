#!/usr/bin/env bash
# Installation checks only; native approval behavior has a separate disposable-VM fixture.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d /tmp/subyard-claude-policy.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
export CLAUDE_POLICY_DIR="$tmp/etc/claude-code/managed-settings.d"
export CLAUDE_POLICY_CHECK_PATH="$tmp/bin/claude-policy-check"
export CLAUDE_POLICY_TEST_ALLOW_NON_ROOT=1
hook="$ROOT/config/agents/claude/provision.sh"
mkdir -p "$CLAUDE_POLICY_DIR"
printf '{"permissions":{"deny":["Bash(secret *)"]}}\n' > "$CLAUDE_POLICY_DIR/10-foreign.json"
before="$(sha256sum "$CLAUDE_POLICY_DIR/10-foreign.json")"
bash "$hook" >/dev/null
bash "$hook" >/dev/null
[ "$before" = "$(sha256sum "$CLAUDE_POLICY_DIR/10-foreign.json")" ]
"$CLAUDE_POLICY_CHECK_PATH" >/dev/null
policy="$CLAUDE_POLICY_DIR/50-subyard-git-approval.json"
jq '. + {allowManagedPermissionRulesOnly:true}' "$policy" > "$tmp/drift.json"
cp "$tmp/drift.json" "$policy"
if "$CLAUDE_POLICY_CHECK_PATH" >/dev/null 2>&1; then exit 1; fi
bash "$hook" >/dev/null
chmod 0666 "$policy"
if "$CLAUDE_POLICY_CHECK_PATH" >/dev/null 2>&1; then exit 1; fi
bash "$hook" >/dev/null
printf '{}\n' > "$policy"
if bash "$hook" >/dev/null 2>&1; then exit 1; fi
rm "$policy"
ln -s "$CLAUDE_POLICY_DIR/10-foreign.json" "$policy"
if bash "$hook" >/dev/null 2>&1; then exit 1; fi
printf 'ok: Claude managed policy idempotence, drift, foreign ownership and symlink refusal\n'

#!/usr/bin/env bash
# OpenClaw consumer and exclusive handoff checks on shared credential fixtures.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
# shellcheck source=tests/helpers/credential-context.sh
. "$ROOT/tests/helpers/credential-context.sh"
setup_credential_context "$TMP"
for context in one two one_alt; do bootstrap_keys "$context" >/dev/null; done
yard_one keys trust @two --yes >/dev/null
yard_two keys trust @one --yes >/dev/null
actor_one="$(jq -r '.actorId' "$SUBYARD_KEYS_ROOT/one/identity.json")"
# shellcheck source=config/profiles/openclaw/tests/credential-scenario.sh
. "$ROOT/config/profiles/openclaw/tests/credential-scenario.sh"
printf 'ok: OpenClaw credential consumers and exclusive handoff\n'

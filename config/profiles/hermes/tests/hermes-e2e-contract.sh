#!/usr/bin/env bash
# Static contract for the real-VM Hermes substrate lane.
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

printf 'ok: Hermes VM lane stays inside the substrate boundary\n'

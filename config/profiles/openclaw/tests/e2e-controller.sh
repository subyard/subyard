#!/usr/bin/env bash
# Check OpenClaw acceptance dispatch without acquiring a VM lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
manifest="$ROOT/config/profiles/openclaw/tests/e2e/external-obligations.json"
jq -e '.schema_version == 1 and (.obligations | length >= 1) and all(.obligations[]; (.id | type == "string" and length > 0) and (.description | type == "string" and length > 0) and (.required_inputs | type == "array" and length > 0))' \
  "$manifest" >/dev/null
controller=config/profiles/openclaw/tests/e2e/acceptance.sh
mkdir -p "$tmp/$(dirname "$controller")" "$tmp/dev"
cp "$ROOT/$controller" "$tmp/$controller"
cat > "$tmp/dev/agent-e2e.sh" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*"
exit "${PROFILE_CONTROLLER_EXIT:-0}"
SH
if bash "$tmp/$controller" > "$tmp/missing" 2>&1; then
  printf 'FAIL: OpenClaw controller accepted a missing slot\n' >&2; exit 1
fi
! grep -q -- '--purpose' "$tmp/missing"
if bash "$tmp/$controller" --slot 0 > "$tmp/zero" 2>&1; then
  printf 'FAIL: OpenClaw controller accepted slot zero\n' >&2; exit 1
fi
bash "$tmp/$controller" --slot 7 > "$tmp/calls"
grep -Fxq -- '--slot 7 --vm-count 1 --purpose openclaw-provision --vm 1 -- bash config/profiles/openclaw/tests/e2e/owner.sh' "$tmp/calls"
rc=0
PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" --slot 7 > "$tmp/failure" 2>&1 || rc=$?
[ "$rc" -eq 23 ]
[ "$(wc -l < "$tmp/failure")" -eq 1 ]
printf 'ok: OpenClaw acceptance dispatch, slot requirement and failure propagation\n'

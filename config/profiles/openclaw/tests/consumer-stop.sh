#!/usr/bin/env bash
# Verify the profile's stop/observe protocol without a gateway or real credentials.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cat > "$TMP/yard" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CALLS"
case "$*" in
  '-Y fixture staging status demo')
    if [ -e "$STOPPED" ]; then
      printf '%s\n' "${AFTER:-gateway: down}"
    else
      printf '%s\n' "${BEFORE:-gateway: running (pid 1)}"
    fi ;;
  '-Y fixture staging stop demo --yes') [ "${FAIL_STOP:-0}" != 1 ]; touch "$STOPPED" ;;
  *) exit 2 ;;
esac
SH
chmod 700 "$TMP/yard"
export CALLS="$TMP/calls" STOPPED="$TMP/stopped"
hook="$ROOT/config/profiles/openclaw/consumer-stop.sh"
"$hook" "$TMP/yard" fixture demo
[ "$(wc -l < "$CALLS")" -eq 3 ] || exit 1
rm -f "$STOPPED"
if AFTER='gateway: running (pid 1)' "$hook" "$TMP/yard" fixture demo; then
  printf 'FAIL: running consumer passed final observation\n' >&2; exit 1
fi
rm -f "$STOPPED"
if BEFORE='unrecognized output' "$hook" "$TMP/yard" fixture demo; then
  printf 'FAIL: unknown observation permitted handoff\n' >&2; exit 1
fi
if FAIL_STOP=1 "$hook" "$TMP/yard" fixture demo; then
  printf 'FAIL: failed stop permitted handoff\n' >&2; exit 1
fi
BEFORE="zone 'demo': (no runner)" "$hook" "$TMP/yard" fixture demo
printf 'ok: OpenClaw credential handoff verifies the old consumer stopped\n'

#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
version=1.1.4
tools="$ROOT/.build/tools"
mkdir -p "$tools"
report="$(mktemp "$tools/govulncheck-report.XXXXXX")"
findings="$(mktemp "$tools/govulncheck-findings.XXXXXX")"
download=
trap 'rm -f -- "$report" "$findings"; if [ -n "$download" ]; then rm -rf -- "$download"; fi' EXIT

audit() {
  local compiler tool status=0
  compiler="$(go env GOVERSION)" || return "$?"
  tool="$tools/govulncheck-$version-$compiler"
  if [ ! -x "$tool" ]; then
    download="$(mktemp -d "$tools/govulncheck.XXXXXX")" || return "$?"
    # Build with the repository's compiler; versioned go install ignores go.mod.
    GOTOOLCHAIN="$compiler" GOBIN="$download" \
      go install "golang.org/x/vuln/cmd/govulncheck@v$version" || return "$?"
    install -m 0755 "$download/govulncheck" "$download/tool" || return "$?"
    mv "$download/tool" "$tool" || return "$?"
  fi
  # JSON mode exits zero for findings; only the reviewed policy decides admission.
  "$tool" -format json ./... > "$findings" || return "$?"
  # Render the same human-readable report without running another scan. Conversion's
  # exit 3 means findings were rendered, not that the allowlist policy passed.
  "$tool" -mode convert < "$findings" || status=$?
  if [ "$status" -ne 0 ] && [ "$status" -ne 3 ]; then return "$status"; fi
  python3 -I "$ROOT/dev/check-govulncheck.py" "$findings" "$ROOT/dev/govulncheck-allowlist.json"
}

status=0
audit > "$report" 2>&1 || status=$?
cat "$report"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  python3 -I - "$report" <<'PY' >> "$GITHUB_STEP_SUMMARY"
from pathlib import Path
import sys

limit = 900000  # Leave room below GitHub's 1 MiB step-summary limit.
header = b"### Go dependency audit\n\n```text\n"
footer = b"\n```\n"
with Path(sys.argv[1]).open("rb") as stream:
    content = stream.read(limit + 1)
if len(header) + len(content) + len(footer) > limit:
    footer = b"\n```\n\nReport truncated; full report in job log.\n"
    content = content[:limit - len(header) - len(footer)].decode("utf-8", "ignore").encode("utf-8")
sys.stdout.buffer.write(header + content + footer)
PY
fi
exit "$status"

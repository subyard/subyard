#!/usr/bin/env bash
# Host-free check of the E2E lanes' failure-output redaction at truncation boundaries.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/orca-lane-output.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
# shellcheck source=config/profiles/orca/tests/e2e/orca-lane-output.bash
. "$ROOT/config/profiles/orca/tests/e2e/orca-lane-output.bash"

python3 - "$tmp/out" <<'PY'
import sys
with open(sys.argv[1], "w") as out:
    # The marker lies before the 1 MiB read window and the short value inside it: the partial
    # leading line is dropped.
    out.write("authToken: " + " " * 1048576 + "WINDOWVALUE7\n")
    # A tail cut 50 characters before this value, long after its marker, must not print it:
    # the whole line is redacted before any truncation.
    tail = ["x" * 600 + " token=LONGLINEVALUE7\n", "pair with orca://pair?code=PAIRCODEVALUE7 now\n",
            "plain diagnostic line\n"]
    spaced = "SPACEDVALUE7\n"
    filler = 16384 - 50 - len(spaced) - sum(map(len, tail)) - 1
    out.write("authToken: " + " " * 1000 + spaced)
    out.write(("y " * filler)[:filler] + "\n")
    out.writelines(tail)
PY
show_output "$tmp/out" 2>"$tmp/shown"
grep -Fq 'out: plain diagnostic line' "$tmp/shown" || fail 'ordinary output was not shown'
for secret in WINDOWVALUE7 SPACEDVALUE7 LONGLINEVALUE7 PAIRCODEVALUE7; do
  ! grep -Fq "$secret" "$tmp/shown" || fail "a credential value was printed: $secret"
done
[ "$(grep -c . "$tmp/shown")" -le 40 ] || fail 'more than 40 lines were shown'
show_output "$tmp/missing" 2>"$tmp/missing-shown"
[ ! -s "$tmp/missing-shown" ] || fail 'a missing file produced output'
printf 'ok: lane failure output redacts before truncating\n'

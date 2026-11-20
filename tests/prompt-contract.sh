#!/usr/bin/env bash
# Mutating UX is fail-closed by default and --yes only bypasses the explicit confirmation port.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=tests/helpers/test-lib.sh
. "$ROOT/tests/helpers/test-lib.sh"
test_tmp

# shellcheck source=scripts/lib/runtime.sh
. "$ROOT/scripts/lib/runtime.sh"
# shellcheck source=scripts/lib/ui.sh
. "$ROOT/scripts/lib/ui.sh"

ASSUME_YES=0
if confirm 'Proceed?' n </dev/null; then fail 'non-interactive default-N prompt was accepted'; fi
if (proceed_or_die) >"$TMP/out" 2>"$TMP/err"; then fail 'refused confirmation did not abort'; fi
grep -Fq 'aborted by user' "$TMP/err" || fail 'abort diagnostic drifted'

ASSUME_YES=1
confirm 'Proceed?' n </dev/null || fail 'automation confirmation bypass failed'
proceed_or_die

YARD_NAME=fixture
announce 'Mutation' 'Change one synthetic fixture.' > "$TMP/announce"
grep -Fq '[yard:fixture] Mutation' "$TMP/announce" || fail 'named-yard prompt lost context'
grep -Fq 'This will:' "$TMP/announce" || fail 'mutation announcement lost its impact heading'

printf 'ok: confirmation remains fail-closed with an explicit automation bypass\n'

#!/usr/bin/env bash
# Exercise immutable candidate selection without installing a runtime or using a VM.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
# shellcheck source=config/profiles/orca/tests/e2e/orca-release-prepare.sh
. "$REPO/config/profiles/orca/tests/e2e/orca-release-prepare.sh"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
ROOT="$tmp/source"
mkdir -p "$ROOT/.subyard-acceptance/release" "$ROOT/config/agents/claude" "$ROOT/dev" "$ROOT/tests/helpers"
printf '{"version":"0.16.10-immutable-test"}\n' > "$ROOT/.subyard-acceptance/candidate.json"
printf 'original release asset\n' > "$ROOT/.subyard-acceptance/release/runtime.tar.gz"
printf '{"autoMemoryDirectory":"original"}\n' > "$ROOT/config/agents/claude/settings.json"
export ORCA_PREPARATION_CALLS="$tmp/package-calls"
cat > "$ROOT/dev/package-engine.sh" <<'PACKAGE'
#!/usr/bin/env bash
printf 'unexpected packaging\n' >> "$ORCA_PREPARATION_CALLS"
exit 91
PACKAGE
cat > "$ROOT/tests/helpers/source-files.sh" <<'FILES'
#!/usr/bin/env bash
printf 'config/agents/claude/settings.json\0dev/package-engine.sh\0'
FILES
snapshot() {
  (cd "$ROOT"; find . -type f -print0 | sort -z | xargs -0 sha256sum)
}
snapshot > "$tmp/before"
die() { printf '%s\n' "$*" >&2; exit 3; }
UPGRADE_FROM=0.13.13
for HANDLER_ACCEPTANCE in direct inherited; do
  STATE="$tmp/$HANDLER_ACCEPTANCE"
  mkdir "$STATE"
  orca_prepare_bootstrap_release
  [ "$release_version" = 0.16.10-immutable-test ]
  cmp "$ROOT/.subyard-acceptance/release/runtime.tar.gz" "$STATE/release/runtime.tar.gz"
  [ ! -e "$STATE/source" ] && [ ! -e "$ORCA_PREPARATION_CALLS" ]
  snapshot > "$tmp/after"
  cmp "$tmp/before" "$tmp/after"
done
HANDLER_ACCEPTANCE=''
STATE="$tmp/mutated"
mkdir "$STATE"
code=0
(orca_prepare_bootstrap_release) > "$tmp/refused" 2>&1 || code=$?
[ "$code" -eq 3 ]
rg -Fq 'the mutated upgrade fixture requires a separate candidate' "$tmp/refused"
[ ! -e "$STATE/source" ] && [ ! -e "$STATE/release" ] && [ ! -e "$ORCA_PREPARATION_CALLS" ]
snapshot > "$tmp/after"
cmp "$tmp/before" "$tmp/after"
printf 'ok: bound direct/inherited handler releases stay immutable; mutated upgrade refuses before preparation\n'

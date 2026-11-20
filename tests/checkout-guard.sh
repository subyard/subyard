#!/usr/bin/env bash
# The checkout-unchanged guard of tests/run.sh; fixtures live under /tmp.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/tests/helpers/test-lib.sh"
TEST_LIB_PREFIX='checkout-guard'
GUARD="$ROOT/tests/helpers/checkout-guard.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

commit_all() {
  git -C "$1" add -A
  git -C "$1" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm fixture
}

checkout="$TMP/checkout"
mkdir -p "$checkout/internal" "$checkout/private/yards" "$checkout/private/tasks"
git -C "$checkout" init -q
printf '/private/\n/.build/\n' > "$checkout/.gitignore"
printf 'package internal\n' > "$checkout/internal/tracked.go"
commit_all "$checkout"
git -C "$checkout/private" init -q
printf 'rules\n' > "$checkout/private/AGENTS.md"
printf 'SSH_PORT=2222\n' > "$checkout/private/yards/e2e-yard.env"
commit_all "$checkout/private"
printf 'draft\n' > "$checkout/internal/draft.go"
printf 'stale\n' > "$checkout/private/yards/neighbor.env"

# expect STATUS [CHANGE...]: compare against the snapshot and match the reported changes.
expect() {
  local want="$1" status=0 line
  shift
  bash "$GUARD" compare "$checkout" "$TMP/snapshot" 2> "$TMP/stderr" || status=$?
  [ "$status" = "$want" ] || fail "exit $status, want $want: $(cat "$TMP/stderr")"
  for line in "$@"; do
    grep -Fxq -- "$line" "$TMP/stderr" || fail "missing '$line' in: $(cat "$TMP/stderr")"
  done
  [ "$(grep -c '^[-+~] ' "$TMP/stderr" || true)" = "$#" ] || fail "unexpected changes: $(cat "$TMP/stderr")"
}

bash "$GUARD" snapshot "$checkout" > "$TMP/snapshot"
# Concurrent edits of existing paths, owner documents and .build/ pass.
printf 'package internal // edited\n' > "$checkout/internal/tracked.go"
printf 'draft edited\n' > "$checkout/internal/draft.go"
printf 'edited\n' > "$checkout/private/AGENTS.md"
printf 'SSH_PORT=2223\n' > "$checkout/private/yards/e2e-yard.env"
printf 'plan\n' > "$checkout/private/tasks/p1-plan.md"
mkdir -p "$checkout/.build" && printf 'log\n' > "$checkout/.build/run.log"
expect 0

# Added, removed and rewritten untracked private/ paths fail together.
bash "$GUARD" snapshot "$checkout" > "$TMP/snapshot"
printf 'leak\n' > "$checkout/internal/leak.txt"
rm "$checkout/internal/draft.go"
mkdir -p "$checkout/private/yards/nested"
printf 'SSH_PORT=2399\n' > "$checkout/private/yards/nested/config.env"
printf 'stale\n' > "$checkout/private/yards/neighbor.env.tmp"
mv "$checkout/private/yards/neighbor.env.tmp" "$checkout/private/yards/neighbor.env"
expect 1 '+ internal/leak.txt' '- internal/draft.go' '+ private/yards/nested/config.env' '~ private/yards/neighbor.env'

# An in-place rewrite with the same content is still a write.
bash "$GUARD" snapshot "$checkout" > "$TMP/snapshot"
touch -d '+1 second' "$checkout/private/yards/neighbor.env"
expect 1 '~ private/yards/neighbor.env'

# Outside a private/ clone only the checkout paths are compared.
rm -rf "$checkout/private/.git"
bash "$GUARD" snapshot "$checkout" > "$TMP/snapshot"
printf 'other\n' > "$checkout/private/yards/neighbor.env"
expect 0

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
tab_name=$'yards/tab\tname.env'
newline_name=$'yards/new\nline.env'
mkdir -p "$checkout/internal" "$checkout/private/yards" "$checkout/private/tasks" \
  "$checkout/private/inputs" "$checkout/private/agents/codex"
git -C "$checkout" init -q
printf '/private/\n/.build/\n' > "$checkout/.gitignore"
printf 'package internal\n' > "$checkout/internal/tracked.go"
commit_all "$checkout"
git -C "$checkout/private" init -q
printf 'rules\n' > "$checkout/private/AGENTS.md"
printf 'SSH_PORT=2222\n' > "$checkout/private/yards/e2e-yard.env"
commit_all "$checkout/private"
printf 'draft\n' > "$checkout/internal/draft.go"
for name in yards/neighbor.env agents/codex/repo.rules config.env "$tab_name" "$newline_name" \
  inputs/note.txt plan.md; do
  printf 'stale\n' > "$checkout/private/$name"
done

# Each case starts and checks the guard exactly as tests/run.sh does, in a fresh run directory.
run_dir=''
begin() {
  run_dir="$(mktemp -d "$TMP/run.XXXXXX")"
  # File times have a coarse clock: existing files predate the start, and the run's
  # writes follow it.
  find "$checkout/private" -path "$checkout/private/.git" -prune -o -type f -exec touch -d '-2 seconds' {} +
  bash "$GUARD" start "$checkout" "$run_dir"
  touch -d '-1 second' "$run_dir/checkout-before"
}

# expect STATUS [CHANGE...]: match the exit status and the complete list of reported changes.
expect() {
  local want="$1" status=0 line
  shift
  bash "$GUARD" check "$checkout" "$run_dir" 2> "$TMP/stderr" || status=$?
  [ "$status" = "$want" ] || fail "exit $status, want $want: $(cat "$TMP/stderr")"
  for line in "$@"; do
    grep -Fxq -- "$line" "$TMP/stderr" || fail "missing '$line' in: $(cat "$TMP/stderr")"
  done
  [ "$(grep -c '^[-+~] ' "$TMP/stderr" || true)" = "$#" ] || fail "unexpected changes: $(cat "$TMP/stderr")"
}

begin
# Concurrent edits of existing paths, private/ files outside the config overlay and
# .build/ pass.
printf 'package internal // edited\n' > "$checkout/internal/tracked.go"
printf 'draft edited\n' > "$checkout/internal/draft.go"
printf 'edited\n' > "$checkout/private/AGENTS.md"
printf 'edited\n' > "$checkout/private/plan.md"
printf 'edited\n' > "$checkout/private/inputs/note.txt"
printf 'SSH_PORT=2223\n' > "$checkout/private/yards/e2e-yard.env"
printf 'plan\n' > "$checkout/private/tasks/p1-plan.md"
mkdir -p "$checkout/.build" && printf 'log\n' > "$checkout/.build/run.log"
expect 0

# Added, removed and replaced paths fail together; an added file is reported once.
begin
printf 'leak\n' > "$checkout/internal/leak.txt"
rm "$checkout/internal/draft.go"
mkdir -p "$checkout/private/yards/nested"
printf 'SSH_PORT=2399\n' > "$checkout/private/yards/nested/config.env"
printf 'stale\n' > "$checkout/private/yards/neighbor.env.tmp"
mv "$checkout/private/yards/neighbor.env.tmp" "$checkout/private/yards/neighbor.env"
expect 1 '+ internal/leak.txt' '- internal/draft.go' '+ private/yards/nested/config.env' \
  '~ private/yards/neighbor.env'

# In-place rewrites with the same content fail, including names with a tab or newline.
begin
for name in agents/codex/repo.rules config.env "$tab_name" "$newline_name"; do
  printf 'stale\n' > "$checkout/private/$name"
done
expect 1 '~ private/agents/codex/repo.rules' '~ private/config.env' "~ private/$tab_name" "~ private/${newline_name%%$'\n'*}"
grep -Fxq 'line.env' "$TMP/stderr" || fail "newline name was cut: $(cat "$TMP/stderr")"

# Outside a private/ clone only the checkout paths are compared.
rm -rf "$checkout/private/.git"
begin
printf 'other\n' > "$checkout/private/yards/neighbor.env"
expect 0

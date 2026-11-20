#!/usr/bin/env bash
# Checkout state for tests/run.sh: tests keep disposable state outside the checkout.
#   snapshot ROOT          print the state of ROOT and its optional private/ clone
#   compare ROOT SNAPSHOT  fail when ROOT differs from SNAPSHOT
# No path outside .build/ may appear or disappear. Content and Git status changes of
# existing paths are concurrent edits, except for untracked private/ files outside the
# coordinator- and operator-owned documents: they have no concurrent writer, so a rewrite
# is a test leak even when an earlier run left the file behind.
set -euo pipefail

PRIVATE_DOCUMENTS='^(tasks|questions|reference|spec)/|^[^/]*\.md$'

checkout_paths() {
  { git -C "$1" ls-files --cached --others --exclude-standard
    git -C "$1" ls-files --others --ignored --exclude-standard --directory
  } | LC_ALL=C sort -u | LC_ALL=C comm -23 - <(git -C "$1" ls-files --deleted | LC_ALL=C sort -u)
}

snapshot() {
  local root="$1"
  checkout_paths "$root" | { grep -vx '\.build/' || true; } | sed 's|^|path\t|'
  [ -e "$root/private/.git" ] || return 0
  checkout_paths "$root/private" | { grep -vE "$PRIVATE_DOCUMENTS" || true; } | sed 's|^|path\tprivate/|'
  # Inode, size and modification time; a file removed meanwhile is reported as a path.
  git -C "$root/private" ls-files -z --others | { grep -zvE "$PRIVATE_DOCUMENTS" || true; } \
    | (cd "$root/private" || exit 0; xargs -0r stat --printf 'file\tprivate/%n\t%i %s %.9Y\n' -- 2>/dev/null || true)
}

compare() {
  local root="$1" before="$2" after changes
  after="$(snapshot "$root" | LC_ALL=C sort)"
  changes="$(
    LC_ALL=C comm -3 <(sed -n 's/^path\t//p' "$before" | LC_ALL=C sort) <(printf '%s\n' "$after" | sed -n 's/^path\t//p') \
      | sed -e 's/^\t/+ /' -e '/^+ /!s/^/- /'
    LC_ALL=C join -t $'\t' <(sed -n 's/^file\t//p' "$before" | LC_ALL=C sort) <(printf '%s\n' "$after" | sed -n 's/^file\t//p') \
      | awk -F '\t' '$2 != $3 { print "~ " $1 }'
  )"
  [ -z "$changes" ] || {
    printf 'FAIL: tests added (+), removed (-) or rewrote (~) checkout paths outside .build/:\n%s\n' "$changes" >&2
    exit 1
  }
}

case "$#:${1:-}" in
  2:snapshot) snapshot "$2" ;;
  3:compare) compare "$2" "$3" ;;
  *) printf 'Usage: %s snapshot ROOT | compare ROOT SNAPSHOT\n' "$0" >&2; exit 2 ;;
esac

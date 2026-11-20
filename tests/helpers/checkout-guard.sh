#!/usr/bin/env bash
# Checkout guard for tests/run.sh: tests keep disposable state outside the checkout.
#   start ROOT RUN_DIR  record the state of ROOT and its optional private/ clone
#   check ROOT RUN_DIR  fail when the run changed that state
# No path outside .build/ may appear or disappear. Content and Git status changes of
# existing paths are concurrent edits. Untracked files of the retired private/ config
# overlay, which tests seed as checkout fixtures, must also not be rewritten: a leak
# left by an earlier run is otherwise part of the starting state.
set -euo pipefail

PRIVATE_DOCUMENTS='^(tasks|questions|reference|spec)/|^[^/]*\.md$'
PRIVATE_CONFIG=(yards agents config.env)

checkout_paths() {
  { git -C "$1" ls-files --cached --others --exclude-standard
    git -C "$1" ls-files --others --ignored --exclude-standard --directory
  } | LC_ALL=C sort -u | LC_ALL=C comm -23 - <(git -C "$1" ls-files --deleted | LC_ALL=C sort -u)
}

checkout_state() {
  checkout_paths "$1" | { grep -vx '\.build/' || true; }
  [ -e "$1/private/.git" ] || return 0
  checkout_paths "$1/private" | { grep -vE "$PRIVATE_DOCUMENTS" || true; } | sed 's|^|private/|'
}

private_config_files() {
  [ ! -e "$1/private/.git" ] || git -C "$1/private" ls-files -z --others -- "${PRIVATE_CONFIG[@]}"
}

# The snapshot's modification time marks the start of the run.
start() {
  private_config_files "$1" > "$2/checkout-private-config"
  checkout_state "$1" | LC_ALL=C sort > "$2/checkout-before"
}

check() {
  local root="$1" before="$2/checkout-before" changes file
  changes="$(LC_ALL=C comm -3 "$before" <(checkout_state "$root" | LC_ALL=C sort) \
    | sed -e 's/^\t/+ /' -e '/^+ /!s/^/- /')"
  local -A existing=()
  while IFS= read -r -d '' file; do existing["$file"]=1; done < "$2/checkout-private-config"
  while IFS= read -r -d '' file; do
    if [ -n "${existing["$file"]:-}" ] && [ "$root/private/$file" -nt "$before" ]; then
      changes+="${changes:+$'\n'}~ private/$file"
    fi
  done < <(private_config_files "$root")
  [ -z "$changes" ] || {
    printf 'FAIL: tests added (+), removed (-) or rewrote (~) checkout paths outside .build/:\n%s\n' "$changes" >&2
    exit 1
  }
}

case "$#:${1:-}" in
  3:start) start "$2" "$3" ;;
  3:check) check "$2" "$3" ;;
  *) printf 'Usage: %s start|check ROOT RUN_DIR\n' "$0" >&2; exit 2 ;;
esac

#!/usr/bin/env bash
# Host-free top-level CLI/help/completion contracts.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
export SUBYARD_NO_AUDIT=1
CLI_TMP="$(mktemp -d)"
trap 'rm -rf "$CLI_TMP"' EXIT

commands="$($ROOT/bin/yard --list)"
grep -qx security <<<"$commands" || fail "security command missing"
"$ROOT/bin/yard" -y --list >/dev/null || fail "leading global --yes is not accepted"
"$ROOT/bin/yard" --help >/dev/null
"$ROOT/bin/yard" --resources >/dev/null
"$ROOT/bin/yard" --version >/dev/null
if "$ROOT/bin/yard" rpc >/dev/null 2>&1; then fail "rpc accepted a non-stdio invocation"; fi

set +e
"$ROOT/bin/yard" definitely-not-a-command >"$CLI_TMP/unknown" 2>&1
unknown_rc=$?
set -e
[ "$unknown_rc" -eq 2 ] || fail "unknown command returned $unknown_rc instead of 2"

for cmd in $commands; do "$ROOT/bin/yard" "$cmd" --help >/dev/null; done

profiles="$(for f in "$ROOT"/config/profiles/*/profile.conf; do basename "$(dirname "$f")"; done | sort)"
bash_profiles="$(TEST_ROOT="$ROOT" bash -c '. "$TEST_ROOT/completions/yard.bash"; _yard_repo(){ printf "%s\\n" "$TEST_ROOT"; }; _yard_profiles yard' | sort)"
[ "$bash_profiles" = "$profiles" ] || fail "bash completion profiles drifted"

# Bash consumes option/verb tokens from the command manifest, including options that previously
# drifted from Zsh (`init --reset`, profile values, and the global resource listing).
completion_words="$({
  # shellcheck source=completions/yard.bash
  . "$ROOT/completions/yard.bash"
  COMP_WORDS=("$ROOT/bin/yard" init --r); COMP_CWORD=2; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" provision ope); COMP_CWORD=2; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" --res); COMP_CWORD=1; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" config sy); COMP_CWORD=2; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" config sync pu); COMP_CWORD=3; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" config sync push --a); COMP_CWORD=4; _yard; printf '%s\n' "${COMPREPLY[@]}"
} | sort -u)"
grep -qx -- '--reset' <<<"$completion_words" || fail 'Bash completion omitted manifest init options'
grep -qx -- 'openclaw' <<<"$completion_words" || fail 'Bash completion omitted profile values'
grep -qx -- '--resources' <<<"$completion_words" || fail 'Bash completion omitted global resources option'
grep -qx -- 'sync' <<<"$completion_words" || fail 'Bash completion omitted config sync'
grep -qx -- 'pull' <<<"$completion_words" || fail 'Bash completion omitted config sync pull'
grep -qx -- '--apply' <<<"$completion_words" || fail 'Bash completion omitted config sync push --apply'

# Ambiguous project names complete to canonical project-first selectors. Host-first selectors do
# not match an already typed project-name prefix and made `yard code Subyard<Tab>` return nothing.
project_selectors="$({
  yard() {
    case "$1" in
      --command-completion) printf '%s\n' project ;;
      --command-options) ;;
      list)
        [ "${2:-}" = --complete-projects ] &&
          printf '%s\n' 'Subyard/owner-a' 'Subyard/owner-b'
        ;;
    esac
  }
  # shellcheck source=completions/yard.bash
  . "$ROOT/completions/yard.bash"
  COMP_WORDS=(yard code Subyard); COMP_CWORD=2; _yard
  printf '%s\n' "${COMPREPLY[@]}"
} | sort)"
[ "$project_selectors" = $'Subyard/owner-a\nSubyard/owner-b' ] ||
  fail 'Bash completion lost ambiguous project selectors after a typed name prefix'

# Native records are authoritative; stale state must never become completion inventory.
# The one local fixture catches accidental fallback without reproducing registry discovery rules.
mkdir -p "$CLI_TMP/config/projects" "$CLI_TMP/config/yards"
printf '%s\n' '{"name":"Stale Project"}' >"$CLI_TMP/config/projects/stale.json"
printf '%s\n' fixture >"$CLI_TMP/config/yards/stale.env"
command -v zsh >/dev/null 2>&1 || fail 'zsh is required for CLI completion contracts'
for shell in bash zsh; do
  shell_args=(-c)
  [ "$shell" != zsh ] || shell_args=(-fc)
  mkdir -p "$CLI_TMP/noninteractive-$shell"
  source_output="$(TEST_COMPLETION_FILE="$ROOT/completions/yard.$shell" \
    HOME="$CLI_TMP/noninteractive-$shell" ZDOTDIR="$CLI_TMP/noninteractive-$shell" "$shell" "${shell_args[@]}" '
    source "$TEST_COMPLETION_FILE"
    source "$TEST_COMPLETION_FILE"
  ' 2>&1)" || fail "$shell noninteractive source failed"
  [ -z "$source_output" ] || fail "$shell noninteractive source emitted diagnostics: $source_output"
  [ -z "$(find "$CLI_TMP/noninteractive-$shell" -mindepth 1 -print -quit)" ] ||
    fail "$shell noninteractive source wrote startup state"
  for provider in yards projects; do
    for mode in success empty failure failure-output; do
      actual="$(TEST_COMPLETION_FILE="$ROOT/completions/yard.$shell" TEST_PROVIDER="$provider" TEST_MODE="$mode" \
        SUBYARD_CONFIG_HOME="$CLI_TMP/config" "$shell" "${shell_args[@]}" '
          yard() {
            [ "$#" -eq 2 ] && [ "$1" = list ] && [ "$2" = "--complete-$TEST_PROVIDER" ] || return 99
            case "$TEST_MODE" in
              success) printf "%s\n" "Native Project/Owner" "owner/dev" "literal \\ value" ;;
              empty) return 0 ;;
              failure) return 2 ;;
              failure-output) printf "%s\n" "untrusted partial output"; return 2 ;;
            esac
          }
          source "$TEST_COMPLETION_FILE"
          source "$TEST_COMPLETION_FILE"
          "_yard_$TEST_PROVIDER" yard
        ')" || fail "$shell $provider $mode did not fail soft"
      if [ "$mode" = success ]; then
        expected=$'Native Project/Owner\nowner/dev\nliteral \\ value'
      elif [ "$provider" = yards ]; then
        expected=default
      else
        expected=''
      fi
      [ "$actual" = "$expected" ] || fail "$shell $provider $mode: expected [$expected], got [$actual]"
    done
  done
  zsh -f "$ROOT/tests/helpers/shell-completion-interaction.zsh" "$shell" \
    "$ROOT/completions/yard.$shell" "$ROOT" || fail "$shell default completion interaction failed"
done

# Caller-installed completion directories must not prompt inside the PTY fixture.
mkdir -p "$CLI_TMP/insecure-completions"
printf '#compdef fixture\n' > "$CLI_TMP/insecure-completions/_fixture"
chmod 0777 "$CLI_TMP/insecure-completions"
FPATH="$CLI_TMP/insecure-completions:$(zsh -fc 'print -r -- "$FPATH"')" \
  zsh -f "$ROOT/tests/helpers/shell-completion-interaction.zsh" zsh \
    "$ROOT/completions/yard.zsh" "$ROOT" \
  || fail 'Zsh completion interaction inherited unsafe caller FPATH'

zsh -f "$ROOT/tests/helpers/zsh-completion-buffers.zsh" "$ROOT/completions/yard.zsh" "$ROOT" \
  || fail 'Zsh native multi-record completion corrupted the command buffer'
zsh_profiles="$(TEST_ROOT="$ROOT" zsh -fc '
  source "$TEST_ROOT/completions/yard.zsh"
  _yard_repo() { print -r -- "$TEST_ROOT" }
  _yard_profiles
' | sort)"
[ "$zsh_profiles" = "$profiles" ] || fail "Zsh completion joined profile records: $zsh_profiles"

printf 'ok: CLI help, globals and profile completion contract\n'

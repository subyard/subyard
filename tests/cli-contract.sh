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

profiles="$("$ROOT/bin/yard" list --complete-profiles | sort)"
bash_profiles="$(TEST_ROOT="$ROOT" bash -c '. "$TEST_ROOT/completions/yard.bash"; _yard_profiles "$TEST_ROOT/bin/yard"' | sort)"
[ "$bash_profiles" = "$profiles" ] || fail "bash completion profiles drifted"

# Bash consumes option/verb tokens from the command manifest, including options that previously
# drifted from Zsh (`init --reset`, profile values, and the global resource listing).
completion_words="$({
  # shellcheck source=completions/yard.bash
  . "$ROOT/completions/yard.bash"
  COMP_WORDS=("$ROOT/bin/yard" init --r); COMP_CWORD=2; _yard; printf '%s\n' "${COMPREPLY[@]}"
  fixture_engine() {
    if [ "${1:-}" = list ] && [ "${2:-}" = --complete-profiles ]; then printf '%s\n' synthetic
    else "$ROOT/bin/yard" "$@"; fi
  }
  COMP_WORDS=(fixture_engine provision syn); COMP_CWORD=2; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" --res); COMP_CWORD=1; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" integration en); COMP_CWORD=2; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" -Y demo integration dis); COMP_CWORD=4; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" config sy); COMP_CWORD=2; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" config sync pu); COMP_CWORD=3; _yard; printf '%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=("$ROOT/bin/yard" config sync push --a); COMP_CWORD=4; _yard; printf '%s\n' "${COMPREPLY[@]}"
} | sort -u)"
grep -qx -- '--reset' <<<"$completion_words" || fail 'Bash completion omitted manifest init options'
grep -qx -- 'synthetic' <<<"$completion_words" || fail 'Bash completion omitted profile values'
grep -qx -- '--resources' <<<"$completion_words" || fail 'Bash completion omitted global resources option'
grep -qx -- 'enable' <<<"$completion_words" || fail 'Bash completion omitted integration enable'
grep -qx -- 'disable' <<<"$completion_words" || fail 'Bash completion omitted selected-yard integration disable'
grep -qx -- 'sync' <<<"$completion_words" || fail 'Bash completion omitted config sync'
grep -qx -- 'pull' <<<"$completion_words" || fail 'Bash completion omitted config sync pull'
grep -qx -- '--apply' <<<"$completion_words" || fail 'Bash completion omitted config sync push --apply'

# The ssh-agent provider completes its verbs and the unlock key path without
# depending on a built engine that already contains the new registry row.
ssh_agent_completion="$({
  printf 'fixture\n' >"$CLI_TMP/id_fixture"
  yard() {
    case "$1" in
      --command-completion) printf '%s\n' ssh-agent ;;
      --command-options) printf '%s\n' '--key --ttl --json --yes --help' ;;
      --command-verbs) printf '%s\n' 'unlock status lock' ;;
    esac
  }
  # shellcheck source=completions/yard.bash
  . "$ROOT/completions/yard.bash"
  COMP_WORDS=(yard ssh-agent unl); COMP_CWORD=2; _yard
  printf 'verb:%s\n' "${COMPREPLY[@]}"
  COMP_WORDS=(yard ssh-agent unlock --key "$CLI_TMP/"); COMP_CWORD=4; _yard
  printf 'key:%s\n' "${COMPREPLY[@]}"
} | sort)"
grep -qx 'verb:unlock' <<<"$ssh_agent_completion" || fail 'Bash completion omitted ssh-agent unlock'
grep -Fqx "key:$CLI_TMP/id_fixture" <<<"$ssh_agent_completion" || fail 'Bash completion did not complete ssh-agent --key as a path'

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
  for provider in yards projects profiles; do
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

# Model a runner with an insecure directory in Zsh's default search path.
mkdir -p "$CLI_TMP/zsh-bin"
cat > "$CLI_TMP/zsh-bin/zsh" <<'ZSH'
#!/usr/bin/env bash
set -euo pipefail
if [ -z "${FPATH+x}" ]; then
  FPATH="$TEST_DEFAULT_COMPLETIONS:$("$TEST_REAL_ZSH" -fc 'print -r -- "$FPATH"')"
  export FPATH
fi
exec "$TEST_REAL_ZSH" "$@"
ZSH
chmod +x "$CLI_TMP/zsh-bin/zsh"
TEST_REAL_ZSH="$(command -v zsh)" TEST_DEFAULT_COMPLETIONS="$CLI_TMP/insecure-completions" \
  PATH="$CLI_TMP/zsh-bin:$PATH" zsh -f "$ROOT/tests/helpers/shell-completion-interaction.zsh" zsh \
    "$ROOT/completions/yard.zsh" "$ROOT" \
  || fail 'Zsh completion interaction used unsafe default FPATH'

zsh -f "$ROOT/tests/helpers/zsh-completion-buffers.zsh" "$ROOT/completions/yard.zsh" "$ROOT" \
  || fail 'Zsh native multi-record completion corrupted the command buffer'
zsh_profiles="$(TEST_ROOT="$ROOT" zsh -fc '
  source "$TEST_ROOT/completions/yard.zsh"
  _yard_profiles "$TEST_ROOT/bin/yard"
' | sort)"
[ "$zsh_profiles" = "$profiles" ] || fail "Zsh completion joined profile records: $zsh_profiles"

printf 'ok: CLI help, globals and profile completion contract\n'

# Shared-only onboarding uses the real CLI and a terminal, without a VM/SSH server.
(
  set -euo pipefail
  shared_root="$(mktemp -d /tmp/subyard-shared-source.XXXXXX)"
  trap 'rm -rf -- "$shared_root"' EXIT
  source="$shared_root/source" operator="$shared_root/operator" asset_root="$shared_root/tooling"
  config_home="$operator/.config/subyard" data_home="$operator/.subyard"
  checkout="$operator/.local/share/subyard-config"
  install -d -m 0700 "$source/shared" "$config_home" "$data_home" "$asset_root/bin"
  # Use current assets and launcher; exclude any checkout-private operator config.
  cp -a "$ROOT/config" "$asset_root/config"
  install -m 0755 "$ROOT/bin/yard" "$asset_root/bin/yard"
  install -m 0600 /dev/null "$config_home/config.env"
  printf 'local sentinel\n' > "$config_home/local-only.keep"
  chmod 0600 "$config_home/local-only.keep"
  printf '{"schemaVersion":1}\n' > "$source/subyard-config.json"
  printf 'YARD_IMAGE=images:debian/13\n' > "$source/shared/config.env"
  git -C "$source" init -q -b main
  git -C "$source" add subyard-config.json shared/config.env
  git -C "$source" -c user.name='CLI fixture' -c user.email='fixture@invalid' \
    -c commit.gpgsign=false commit -qm 'Create shared source'
  original_head="$(git -C "$source" rev-parse HEAD)"
  for path in source operator; do cp -a "$shared_root/$path" "$shared_root/$path.before"; done
  config_environment=(env -i "PATH=$PATH" "HOME=$operator" "SUBYARD_OPERATOR_HOME=$operator"
    "SUBYARD_CONFIG_HOME=$config_home" "SUBYARD_HOME=$data_home" "SUBYARD_NO_AUDIT=1"
    "YARD_ENGINE_PATH=${YARD_ENGINE_PATH:-$ROOT/.build/yard}" "GIT_TERMINAL_PROMPT=0")
  shared_yard() { "${config_environment[@]}" "$asset_root/bin/yard" "$@"; }
  shared_terminal() {
    local command_line
    printf -v command_line '%q ' "${config_environment[@]}" "$asset_root/bin/yard" "$@"
    (cd "$shared_root"; SHELL=/bin/bash script --echo never -qefc "$command_line" /dev/null)
  }
  if printf 'n\n' | shared_terminal config sync connect "$source" --host-id shared-fixture \
    > "$shared_root/decline.log" 2>&1; then fail 'declined shared source connected'; fi
  grep -Fq 'operation declined' "$shared_root/decline.log" || { cat "$shared_root/decline.log"; fail 'wrong decline refusal'; }
  [ "$(grep -Fc 'Proceed? [Y/n]' "$shared_root/decline.log")" = 1 ] || fail 'decline prompt count'
  for path in source operator; do diff -r "$shared_root/$path.before" "$shared_root/$path"; done
  [ ! -e "$checkout" ] && [ ! -e "$config_home/host-id" ] \
    && [ ! -e "$config_home/.sync/source.json" ] || fail 'decline published source state'
  if shared_yard config sync path >/dev/null 2>&1; then fail 'decline registered a source'; fi
  printf 'y\n' | shared_terminal config sync connect "$source" --host-id shared-fixture \
    > "$shared_root/accept.log" 2>&1 || { cat "$shared_root/accept.log"; fail 'shared source accept failed'; }
  [ "$(grep -Fc 'Proceed? [Y/n]' "$shared_root/accept.log")" = 1 ] || fail 'accept prompt count'
  grep -Fq '.sync/settings/overrides/shared/config.env' "$shared_root/accept.log" \
    && grep -Fq 'config sync: connected' "$shared_root/accept.log" || fail 'shared source preview/result missing'
  ! grep -Fq 'images:debian/13' "$shared_root/accept.log" || fail 'source preview disclosed a setting'
  [ "$(cat "$config_home/host-id")" = shared-fixture ] || fail 'local HostID missing'
  cmp "$source/shared/config.env" "$config_home/.sync/settings/overrides/shared/config.env"
  cmp "$operator.before/.config/subyard/config.env" "$config_home/config.env"
  cmp "$operator.before/.config/subyard/local-only.keep" "$config_home/local-only.keep"
  [ ! -e "$checkout/hosts" ] && [ -z "$(git -C "$checkout" status --porcelain=v1 --untracked-files=all)" ] \
    && [ "$(git -C "$source" rev-parse HEAD)" = "$original_head" ] || fail 'shared onboarding changed Git inputs'
  [ "$(shared_yard config sync path)" = "$checkout" ] || fail 'wrong authoritative checkout'
  shared_yard config sync --check >/dev/null
)

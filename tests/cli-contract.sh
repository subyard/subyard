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

# A source/runtime version skew can make the native completion provider unavailable. The local
# fallback must still emit safe, working project-first selectors instead of greedy JSON tails.
mkdir -p "$CLI_TMP/config/projects" "$CLI_TMP/config/yards/dev/projects"
printf '%s\n' 'fixture' >"$CLI_TMP/config/yards/dev.env"
mkdir -p "$CLI_TMP/config/yards/nested" "$CLI_TMP/config/yards/directory.env"
printf '%s\n' 'fixture' >"$CLI_TMP/config/yards/nested/config.env"
printf '%s\n' 'fixture' >"$CLI_TMP/config/yards/default.env"
printf '%s\n' 'fixture' >"$CLI_TMP/config/yards/config.env"
printf '%s\n' 'fixture' >"$CLI_TMP/config/yards/bad name.env"
ln -s "$CLI_TMP/config/yards/dev.env" "$CLI_TMP/config/yards/linked.env"
printf '%s\n' 'CarbonX1' >"$CLI_TMP/config/host-id"
printf '%s\n' \
  '{"schema":1,"projectId":"subyard-12345678","name":"Subyard","hostPath":"/host/Subyard","target":"yard"}' \
  >"$CLI_TMP/config/projects/subyard-12345678.json"
printf '%s\n' \
  '{"schema":1,"projectId":"subyard-dev-12345678","name":"Subyard","hostPath":"/host/Subyard","target":"yard"}' \
  >"$CLI_TMP/config/yards/dev/projects/subyard-dev-12345678.json"
fallback_selectors="$({
  yard() {
    case "$1" in
      --command-completion) printf '%s\n' project ;;
      --command-options) ;;
      list) return 2 ;;
    esac
  }
  export SUBYARD_CONFIG_HOME="$CLI_TMP/config"
  # shellcheck source=completions/yard.bash
  . "$ROOT/completions/yard.bash"
  _yard_projects yard
} | sort)"
[ "$fallback_selectors" = $'Subyard/CarbonX1\nSubyard/dev/CarbonX1' ] ||
  fail "Bash fallback emitted broken project selectors: $fallback_selectors"

fallback_completion="$({
  yard() {
    case "$1" in
      --command-completion) printf '%s\n' project ;;
      --command-options) ;;
      list) return 2 ;;
    esac
  }
  export SUBYARD_CONFIG_HOME="$CLI_TMP/config"
  # shellcheck source=completions/yard.bash
  . "$ROOT/completions/yard.bash"
  COMP_WORDS=(yard code Subyard/Carb); COMP_CWORD=2; _yard
  printf '%s\n' "${COMPREPLY[@]}"
})"
[ "$fallback_completion" = 'Subyard/CarbonX1' ] ||
  fail "Bash fallback did not complete a qualified local selector: $fallback_completion"

rm "$CLI_TMP/config/host-id"
bash_no_host_id="$({
  yard() { return 2; }
  export SUBYARD_CONFIG_HOME="$CLI_TMP/config"
  # shellcheck source=completions/yard.bash
  . "$ROOT/completions/yard.bash"
  _yard_projects yard
} | sort)"
[ "$bash_no_host_id" = 'Subyard' ] ||
  fail "Bash fallback emitted duplicate bare project selectors: $bash_no_host_id"

printf '%s\n' 'CarbonX1' >"$CLI_TMP/config/host-id"
command -v zsh >/dev/null 2>&1 || fail 'zsh is required for CLI completion contracts'
zsh -f "$ROOT/tests/helpers/zsh-completion-buffers.zsh" "$ROOT/completions/yard.zsh" "$ROOT" \
  || fail 'Zsh native multi-record completion corrupted the command buffer'

zsh_profiles="$(TEST_ROOT="$ROOT" zsh -fc '
  source "$TEST_ROOT/completions/yard.zsh"
  _yard_repo() { print -r -- "$TEST_ROOT" }
  _yard_profiles
' | sort)"
[ "$zsh_profiles" = "$profiles" ] || fail "Zsh completion joined profile records: $zsh_profiles"

for shell in bash zsh; do
  native_projects="$({
    if [ "$shell" = bash ]; then
      TEST_ROOT="$ROOT" SUBYARD_CONFIG_HOME="$CLI_TMP/config" bash -c '
        yard() {
          [ "$1" = list ] && [ "$2" = --complete-projects ] &&
            printf "%s\\n" "Native Project/Owner" "Subyard/owner-a" skills
        }
        source "$TEST_ROOT/completions/yard.bash"; _yard_projects yard
      '
    else
      TEST_ROOT="$ROOT" SUBYARD_CONFIG_HOME="$CLI_TMP/config" zsh -fc '
        yard() {
          if [[ $1 == list && $2 == --complete-projects ]]; then
            print -r -- "Native Project/Owner"
            print -r -- Subyard/owner-a
            print -r -- skills
          fi
        }
        source "$TEST_ROOT/completions/yard.zsh"; _yard_projects
      '
    fi
  })"
  [ "$native_projects" = $'Native Project/Owner\nSubyard/owner-a\nskills' ] ||
    fail "$shell native project records were not preserved: $native_projects"

  empty_native_projects="$({
    if [ "$shell" = bash ]; then
      TEST_ROOT="$ROOT" SUBYARD_CONFIG_HOME="$CLI_TMP/config" bash -c '
        yard() { [ "$1" = list ] && [ "$2" = --complete-projects ] && return 0; }
        source "$TEST_ROOT/completions/yard.bash"; _yard_projects yard
      '
    else
      TEST_ROOT="$ROOT" SUBYARD_CONFIG_HOME="$CLI_TMP/config" zsh -fc '
        yard() { [[ $1 == list && $2 == --complete-projects ]] && return 0 }
        source "$TEST_ROOT/completions/yard.zsh"; _yard_projects
      '
    fi
  } | sort)"
  [ "$empty_native_projects" = $'Subyard/CarbonX1\nSubyard/dev/CarbonX1' ] ||
    fail "$shell empty native project provider did not fall back: $empty_native_projects"
done

zsh_native_yards="$(TEST_ROOT="$ROOT" zsh -fc '
  yard() {
    if [[ $1 == list && $2 == --complete-yards ]]; then
      print -r -- default
      print -r -- owner/dev
    fi
  }
  source "$TEST_ROOT/completions/yard.zsh"
  _yard_yards
')"
[ "$zsh_native_yards" = $'default\nowner/dev' ] ||
  fail "Zsh native yard records were not preserved: $zsh_native_yards"

bash_fallback_yards="$(TEST_ROOT="$ROOT" TEST_CONFIG_HOME="$CLI_TMP/config" bash -c '
  yard() { return 2; }
  source "$TEST_ROOT/completions/yard.bash"
  _yard_repo() { return 1; }
  _yard_config_home() { printf "%s\\n" "$TEST_CONFIG_HOME"; }
  _yard_yards yard
' | sort)"
[ "$bash_fallback_yards" = $'config\ndefault\ndev\nnested' ] ||
  fail "Bash fallback yard records were not filtered: $bash_fallback_yards"

zsh_fallback_yards="$(TEST_ROOT="$ROOT" TEST_CONFIG_HOME="$CLI_TMP/config" zsh -fc '
  yard() { return 2 }
  source "$TEST_ROOT/completions/yard.zsh"
  _yard_repo() { return 1 }
  _yard_config_home() { print -r -- "$TEST_CONFIG_HOME" }
  _yard_yards
' | sort)"
[ "$zsh_fallback_yards" = $'config\ndefault\ndev\nnested' ] ||
  fail "Zsh fallback yard records were not preserved: $zsh_fallback_yards"

# The fallback derives private registrations from the effective config directory, just like the
# native registry. It must not silently switch back to the executable's repository root.
mkdir -p "$CLI_TMP/alternate/private/yards" "$CLI_TMP/config-target/config" \
  "$CLI_TMP/config-target/private/yards" "$CLI_TMP/unrelated/private/yards" \
  "$CLI_TMP/empty-home"
ln -s "$CLI_TMP/config-target/config" "$CLI_TMP/alternate/config"
printf '%s\n' fixture >"$CLI_TMP/alternate/private/yards/wanted.env"
printf '%s\n' fixture >"$CLI_TMP/config-target/private/yards/symlink-target.env"
printf '%s\n' fixture >"$CLI_TMP/unrelated/private/yards/unrelated.env"
for shell in bash zsh; do
  alternate_yards="$(
    if [ "$shell" = bash ]; then
      TEST_ROOT="$ROOT" TEST_REPO="$CLI_TMP/unrelated" \
        SUBYARD_CONFIG_DIR="$CLI_TMP/alternate/config" \
        SUBYARD_CONFIG_HOME="$CLI_TMP/empty-home" bash -c '
          yard() { return 2; }
          source "$TEST_ROOT/completions/yard.bash"
          _yard_repo() { printf "%s\\n" "$TEST_REPO"; }
          _yard_yards yard
        '
    else
      TEST_ROOT="$ROOT" TEST_REPO="$CLI_TMP/unrelated" \
        SUBYARD_CONFIG_DIR="$CLI_TMP/alternate/config" \
        SUBYARD_CONFIG_HOME="$CLI_TMP/empty-home" zsh -fc '
          yard() { return 2 }
          source "$TEST_ROOT/completions/yard.zsh"
          _yard_repo() { print -r -- "$TEST_REPO" }
          _yard_yards
        '
    fi
  )"
  [ "$alternate_yards" = $'default\nwanted' ] ||
    fail "$shell fallback ignored SUBYARD_CONFIG_DIR: $alternate_yards"
done

# With no explicit Subyard state root, fallback completion follows the loader's XDG placement.
for shell in bash zsh; do
  if [ "$shell" = bash ]; then
    xdg_config_home="$(
      env -u SUBYARD_CONFIG_HOME -u SUBYARD_OPERATOR_HOME \
        TEST_ROOT="$ROOT" HOME="$CLI_TMP/home" XDG_CONFIG_HOME="$CLI_TMP/xdg" bash -c '
          source "$TEST_ROOT/completions/yard.bash"
          _yard_repo() { printf "%s\\n" "$TEST_ROOT"; }
          _yard_config_home yard
        '
    )"
  else
    xdg_config_home="$(
      env -u SUBYARD_CONFIG_HOME -u SUBYARD_OPERATOR_HOME \
        TEST_ROOT="$ROOT" HOME="$CLI_TMP/home" XDG_CONFIG_HOME="$CLI_TMP/xdg" zsh -fc '
          source "$TEST_ROOT/completions/yard.zsh"
          _yard_repo() { print -r -- "$TEST_ROOT" }
          _yard_config_home
        '
    )"
  fi
  [ "$xdg_config_home" = "$CLI_TMP/xdg/subyard" ] ||
    fail "$shell fallback ignored XDG_CONFIG_HOME: $xdg_config_home"

  if [ "$shell" = bash ]; then
    explicit_config_home="$(
      SUBYARD_CONFIG_HOME="$CLI_TMP/explicit" XDG_CONFIG_HOME="$CLI_TMP/xdg" \
        TEST_ROOT="$ROOT" bash -c '
          source "$TEST_ROOT/completions/yard.bash"
          _yard_config_home yard
        '
    )"
  else
    explicit_config_home="$(
      SUBYARD_CONFIG_HOME="$CLI_TMP/explicit" XDG_CONFIG_HOME="$CLI_TMP/xdg" \
        TEST_ROOT="$ROOT" zsh -fc '
          source "$TEST_ROOT/completions/yard.zsh"
          _yard_config_home
        '
    )"
  fi
  [ "$explicit_config_home" = "$CLI_TMP/explicit" ] ||
    fail "$shell fallback did not prefer SUBYARD_CONFIG_HOME: $explicit_config_home"
done

zsh_fallback="$(TEST_ROOT="$ROOT" SUBYARD_CONFIG_HOME="$CLI_TMP/config" zsh -fc '
    yard() { return 2 }
    source "$TEST_ROOT/completions/yard.zsh"
    _yard_projects
' | sort)"
[ "$zsh_fallback" = $'Subyard/CarbonX1\nSubyard/dev/CarbonX1' ] ||
  fail "Zsh fallback emitted broken project selectors: $zsh_fallback"

zsh_prefix="$(TEST_ROOT="$ROOT" SUBYARD_CONFIG_HOME="$CLI_TMP/config" zsh -f <<'ZSH'
zmodload zsh/zpty
zpty -b completion zsh -fi
zpty -w completion $'autoload -Uz compinit; compinit -D -i\n'
zpty -w completion $'yard() { case "$1" in --list) print -r -- code ;; --command-completion) print -r -- project ;; --command-options|--command-verbs) ;; list) return 2 ;; esac }\n'
zpty -w completion "source ${(q)TEST_ROOT}/completions/yard.zsh"$'\n'
zpty -w completion $'compdef _yard yard\nbindkey -e\nbindkey "^Xc" complete-word\nreport_buffer() { print -r -- "RESULT:$BUFFER"; }\nzle -N report_buffer\nbindkey "^Xr" report_buffer\nprint -r -- READY\n'
transcript=''
for _ in {1..50}; do
  chunk=''
  if zpty -r -tm completion chunk '*READY*'; then
    transcript+=$chunk; ready=$chunk; break
  fi
  transcript+=$chunk
  sleep 0.1
done
[[ $ready == *READY* ]] || { print -u2 -r -- "Zsh completion setup timed out: ${(qqq)transcript}"; exit 1; }
zpty -w -n completion $'yard code Subyard/Carb'
zpty -w -n completion $'\C-xc\C-xr'
for _ in {1..50}; do
  chunk=''
  if zpty -r -tm completion chunk $'*RESULT:*\r\n'; then
    transcript+=$chunk; result=$chunk; break
  fi
  transcript+=$chunk
  sleep 0.1
done
[[ $result == *RESULT:* ]] || { print -u2 -r -- "Zsh completion result timed out: ${(qqq)transcript}"; exit 1; }
result=${result#*RESULT:}
result=${result%%$'\r'*}
result=${result%%$'\n'*}
print -r -- "$result"
zpty -w completion $'exit\n'
zpty -d completion
ZSH
)"
[ "$zsh_prefix" = 'yard code Subyard/CarbonX1 ' ] ||
  fail "Zsh fallback did not complete a qualified local selector: $zsh_prefix"

rm "$CLI_TMP/config/host-id"
zsh_no_host_id="$(TEST_ROOT="$ROOT" SUBYARD_CONFIG_HOME="$CLI_TMP/config" zsh -fc '
    yard() { return 2 }
    source "$TEST_ROOT/completions/yard.zsh"
    _yard_projects
' | sort)"
[ "$zsh_no_host_id" = 'Subyard' ] ||
  fail "Zsh fallback emitted duplicate bare project selectors: $zsh_no_host_id"
grep -Fq -- '--command-options' "$ROOT/completions/yard.zsh" \
  && grep -Fq -- '--command-verbs' "$ROOT/completions/yard.zsh" \
  || fail 'Zsh completion does not consume manifest options and verbs'

printf 'ok: CLI help, globals and profile completion contract\n'

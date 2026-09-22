#!/usr/bin/env zsh
# Actual Tab/Shift-Tab/Enter, without replacing the product's completion widgets.
# An optional installed HOME tests normal startup without manually sourcing completion.
emulate -L zsh
setopt ERR_EXIT NO_UNSET PIPE_FAIL
if (( $# < 3 || $# > 4 )) || [[ $1 != (bash|zsh) ]]; then
  print -u2 -- 'usage: shell-completion-interaction.zsh <bash|zsh> <completion-file> <runtime-root> [installed-home]'
  exit 2
fi
shell_name=$1
export TEST_COMPLETION_FILE=${2:A}
runtime_root=${3:A}
installed_home=${4:-}
[[ -r $TEST_COMPLETION_FILE && -d $runtime_root ]] || {
  print -u2 -- 'Completion fixture paths are unavailable'
  exit 2
}
fixture=$(mktemp -d)
cleanup() { zpty -d completion 2>/dev/null || true; rm -rf -- "$fixture"; }
zmodload zsh/zpty
trap cleanup EXIT
mkdir -p "$fixture/home" "$fixture/work"
# A command can share its spelling with a directory in the current repository.
mkdir -p "$fixture/work/config" "$fixture/work/Folder Space"
: > "$fixture/work/Folder Space/file"
export TEST_COMPLETION_RESULT="$fixture/result"
export TEST_COMPLETION_STUB="$fixture/provider"
export INPUTRC="$fixture/inputrc"
export HISTFILE="$fixture/history"
export TERM=xterm-256color
print -r -- '' > "$INPUTRC"
cat > "$TEST_COMPLETION_STUB" <<'STUB'
# Strict machine calls expose wrong argument wiring instead of returning permissive data.
yard() {
  case "$1" in
    --list)
      [ "$#" -eq 1 ] || return 99
      printf '%s\n' code config shell status
      ;;
    --command-completion|--command-options|--command-verbs)
      [ "$#" -eq 2 ] || return 99
      case "$1:$2" in
        --command-completion:code|--command-completion:shell) printf '%s\n' project ;;
        --command-completion:config) printf '%s\n' config ;;
        --command-completion:status) printf '%s\n' status ;;
        --command-options:*|--command-verbs:*) ;;
        *) return 99 ;;
      esac
      ;;
    list)
      [ "$#" -eq 2 ] || return 99
      case "$2" in --complete-yards|--complete-projects) ;; *) return 99 ;; esac
      case "$TEST_PROVIDER_MODE" in
        empty) return 0 ;;
        failure) printf '%s\n' 'Rejected/partial'; return 2 ;;
      esac
      case "$2" in
        --complete-yards) printf '%s\n' default owner/dev owner/tools ;;
        --complete-projects) printf '%s\n' 'Native Project/owner-a' 'Native Project/owner-b' ;;
      esac
      ;;
    *) printf '<%s>' "$TEST_COMPLETION_COMMAND" "$@" > "$TEST_COMPLETION_RESULT" ;;
  esac
}
sy() { yard "$@"; }
TEST_PROVIDER_MODE=success
STUB

wait_for_marker() {
  local marker=$1 chunk='' transcript=''
  integer attempt
  for (( attempt = 0; attempt < 100; ++attempt )); do
    chunk=''
    if zpty -r -tm completion chunk "*${marker}*"; then
      completion_transcript=$transcript$chunk
      return 0
    fi
    transcript+=$chunk
    sleep 0.05
  done
  print -u2 -r -- "$shell_name/$editing_mode timed out waiting for $marker: ${(qqq)transcript}"
  return 1
}

run_line() {
  local label=$1 input=$2 expected=$3 actual=''
  : > "$TEST_COMPLETION_RESULT"
  zpty -w -n completion "$input"$'\r'
  # An execution marker split by shell quotes cannot match terminal input echo.
  zpty -w completion 'printf "\nEXEC%s\n" UTED'
  wait_for_marker EXECUTED
  actual=$(<"$TEST_COMPLETION_RESULT")
  [[ $actual == "$expected" ]] || {
    print -u2 -r -- "$shell_name/$editing_mode $label: expected [$expected], got [$actual]"
    print -u2 -r -- "Shell startup: ${(qqq)startup_transcript}"
    print -u2 -r -- "Completion interaction: ${(qqq)completion_transcript}"
    return 1
  }
  if [[ -n ${4:-} && $completion_transcript != *$4* ]]; then
    print -u2 -r -- "$shell_name/$editing_mode $label did not display candidate $4: ${(qqq)completion_transcript}"
    return 1
  fi
}

if [[ $shell_name == zsh ]]; then
  # Isolate caller paths and unsafe system defaults before interactive startup.
  # Otherwise compinit's security prompt consumes the fixture's queued commands.
  safe_zsh_fpath=$(env -u FPATH zsh -fc '
    autoload -Uz compaudit
    safe_fpath=()
    for directory in "$fpath[@]"; do
      if compaudit "$directory" >/dev/null 2>&1; then
        safe_fpath+=("$directory")
      fi
    done
    (( $#safe_fpath )) || { print -u2 -- "No safe Zsh completion directories"; exit 1; }
    print -r -- ${(j.:.)safe_fpath}
  ')
fi

for editing_mode in emacs vi; do
  if [[ -n $installed_home ]]; then
    shell_home=${installed_home:A}
  else
    shell_home="$fixture/home"
    # Select the user's mode before loading completion; completion must preserve it.
    print -rl -- 'source "$TEST_COMPLETION_FILE"' 'source "$TEST_COMPLETION_FILE"' > "$shell_home/.bashrc"
    cp "$shell_home/.bashrc" "$shell_home/.zshrc"
  fi
  cd "$fixture/work"
  if [[ $shell_name == bash ]]; then
    zpty -b completion exec env HOME="$shell_home" INPUTRC="$INPUTRC" bash --noprofile -o "$editing_mode" -i
  else
    zpty -b completion exec env FPATH="$safe_zsh_fpath" HOME="$shell_home" ZDOTDIR="$shell_home" EDITOR="$editing_mode" VISUAL="$editing_mode" zsh -i
  fi
  zpty -w completion 'source "$TEST_COMPLETION_STUB"; PS1="test> "; printf "\nBOOT%s\n" READY'
  wait_for_marker BOOTREADY
  startup_transcript=$completion_transcript
  # Assert that startup has not silently changed the user's chosen editing mode.
  if [[ $shell_name == bash ]]; then
    zpty -w completion "[[ -o $editing_mode ]] && printf '\\nMODE%s\\n' OK"
  elif [[ $editing_mode == vi ]]; then
    zpty -w completion '[[ $(bindkey -lL main) == *viins* ]] && printf "\nMODE%s\n" OK'
  else
    zpty -w completion '[[ $(bindkey -lL main) == *emacs* ]] && printf "\nMODE%s\n" OK'
  fi
  wait_for_marker MODEOK
  for command_name in yard sy; do
    zpty -w completion "TEST_COMPLETION_COMMAND=$command_name; printf '\\nCASE%s\\n' READY"
    wait_for_marker CASEREADY
    run_line command-first "$command_name c"$'\t' "<$command_name><code>" config
    run_line command-next "$command_name c"$'\t\t' "<$command_name><config>"
    run_line command-back "$command_name c"$'\t\t\e[Z' "<$command_name><code>"
    run_line project-first "$command_name code Nat"$'\t' "<$command_name><code><Native Project/owner-a>"
    run_line project-next "$command_name shell Nat"$'\t\t' "<$command_name><shell><Native Project/owner-b>"
    run_line project-back "$command_name code Nat"$'\t\t\e[Z' "<$command_name><code><Native Project/owner-a>"
    run_line directory-space "$command_name code Fold"$'\t'file "<$command_name><code><Folder Space/file>"
    run_line context-short-command "$command_name -Y owner/dev c"$'\t\t' "<$command_name><-Y><owner/dev><config>"
    run_line context-at-command "$command_name @owner/dev c"$'\t\t' "<$command_name><@owner/dev><config>"
    run_line context-equals-command "$command_name --yard=owner/dev c"$'\t\t' "<$command_name><--yard=owner/dev><config>"
    run_line yard-first "$command_name -Y owner/"$'\t'" status" "<$command_name><-Y><owner/dev><status>"
    run_line yard-next "$command_name --yard owner/"$'\t\t'" status" "<$command_name><--yard><owner/tools><status>"
    run_line yard-equals "$command_name --yard=owner/"$'\t\t\e[Z'" status" "<$command_name><--yard=owner/dev><status>"
    run_line yard-back "$command_name @owner/"$'\t\t\e[Z'" status" "<$command_name><@owner/dev><status>"
    for provider_mode in empty failure; do
      zpty -w completion "TEST_PROVIDER_MODE=$provider_mode; printf '\\nCASE%s\\n' READY"
      wait_for_marker CASEREADY
      run_line "$provider_mode-yard" "$command_name -Y de"$'\t'" status" "<$command_name><-Y><default><status>"
      run_line "$provider_mode-project" "$command_name code Nat"$'\t' "<$command_name><code><Nat>"
    done
    zpty -w completion 'TEST_PROVIDER_MODE=success; printf "\nCASE%s\n" READY'
    wait_for_marker CASEREADY
  done
  zpty -d completion
done
print -r -- "ok: $shell_name default completion interaction"

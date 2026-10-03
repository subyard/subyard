#!/usr/bin/env zsh
# Exercise the real ZLE buffer against a selected yard.zsh completion artifact.
emulate -L zsh
setopt ERR_EXIT NO_UNSET PIPE_FAIL

if (( $# != 2 )); then
  print -u2 -r -- 'usage: zsh-completion-buffers.zsh <completion-file> <runtime-root>'
  exit 2
fi
export TEST_ZSH_COMPLETION_FILE=${1:A}
export TEST_ZSH_RUNTIME_ROOT=${2:A}
[[ -r $TEST_ZSH_COMPLETION_FILE && -d $TEST_ZSH_RUNTIME_ROOT ]] || {
  print -u2 -r -- 'Zsh completion fixture paths are unavailable'
  exit 2
}
cd "$TEST_ZSH_RUNTIME_ROOT"

zmodload zsh/zpty
zpty -b completion zsh -fi
cleanup_completion() { zpty -d completion 2>/dev/null || true }
trap cleanup_completion EXIT

wait_for_marker() {
  local marker=$1 phase=$2
  integer attempts=$3
  local chunk='' transcript=''
  for (( attempt = 1; attempt <= attempts; ++attempt )); do
    chunk=''
    if zpty -r -tm completion chunk "*${marker}*"; then
      return 0
    fi
    transcript+=$chunk
    sleep 0.1
  done
  print -u2 -r -- "Zsh completion $phase timed out: ${(qqq)transcript}"
  return 1
}

# Do not put a readiness marker literally in the command sent to the PTY: the terminal echoes
# input before Zsh has executed it. Empty quoted strings keep the echoed command distinct from
# the marker that the command eventually prints.
zpty -w completion $'print -r -- BOOT""_READY\n'
wait_for_marker BOOT_READY startup 50
zpty -w completion $'autoload -Uz compinit; compinit -D -i; print -r -- COMPI""NIT_READY\n'
wait_for_marker COMPINIT_READY initialization 300
zpty -w completion $'yard() { case "$1" in --list) print -r -l -- code shell provision remote keys ;; --command-completion) case "$2" in code) print -r -- project ;; shell) print -r -- project-shell ;; provision) print -r -- profiles ;; remote|keys) print -r -- "$2" ;; esac ;; --command-options) ;; --command-verbs) case "$2" in remote) print -r -- "add repair-key remove list" ;; keys) print -r -- "trust untrust sync move" ;; esac ;; list) case "$2" in --complete-projects) print -r -- Alpha; print -r -- completions/owner; print -r -- "Native Project/Owner"; print -r -- skills; print -r -- Subyard/alpha; print -r -- Subyard/beta ;; --complete-yards) print -r -- default; print -r -- owner/dev; print -r -- tools ;; esac ;; esac }\n'
zpty -w completion $'source "$TEST_ZSH_COMPLETION_FILE"\n'
zpty -w completion $'_yard_repo() { print -r -- "$TEST_ZSH_RUNTIME_ROOT" }\n_yard_profiles() { print -r -l -- synthetic another }\ncompdef _yard yard\nbindkey -e\nbindkey "^Xc" complete-word\nreport_buffer() { print -r -- "RESULT:$BUFFER"; }\nzle -N report_buffer\nbindkey "^Xr" report_buffer\nprint -r -- SET""UP_READY\n'
wait_for_marker SETUP_READY setup 100

complete_buffer() {
  local label=$1 input=$2 chunk='' result=''
  zpty -w -n completion $'\C-u'
  zpty -w -n completion "$input"
  zpty -w -n completion $'\C-xc\C-xr'
  for _ in {1..50}; do
    chunk=''
    if zpty -r -tm completion chunk $'*RESULT:*\r\n'; then
      result=$chunk
      break
    fi
    sleep 0.1
  done
  [[ $result == *RESULT:* ]] || {
    print -u2 -r -- "Zsh completion result timed out for $label: ${(qqq)result}"
    exit 1
  }
  result=${result#*RESULT:}
  result=${result%%$'\r'*}
  result=${result%%$'\n'*}
  print -r -- "$label:$result"
}

buffers="$({
  complete_buffer empty 'yard code '
  complete_buffer unique 'yard code skil'
  complete_buffer ambiguous 'yard code Subyard'
  complete_buffer sibling 'yard shell skil'
  complete_buffer at-yard 'yard @to'
  complete_buffer yard-option 'yard -Y to'
  complete_buffer remote 'yard remote remove to'
  complete_buffer keys 'yard keys trust @to'
  complete_buffer profile 'yard provision syn'
  complete_buffer directory 'yard code compl'
})"
expected=$'empty:yard code \nunique:yard code skills \nambiguous:yard code Subyard/\nsibling:yard shell skills \nat-yard:yard @tools \nyard-option:yard -Y tools \nremote:yard remote remove tools \nkeys:yard keys trust @tools \nprofile:yard provision synthetic \ndirectory:yard code completions'
[[ $buffers == $expected ]] || {
  print -u2 -r -- "Zsh native multi-record completion did not preserve separate candidates: $buffers"
  exit 1
}

zpty -w completion $'exit\n'
zpty -d completion
trap - EXIT

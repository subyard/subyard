# shellcheck shell=bash
# shellcheck disable=SC2034,SC2207 # completion API requires dynamic word splitting/global names
# yard.bash — bash completion for the `yard` (and `sy`) CLI.
# Self-contained: no bash-completion package required. Top-level commands come
# from `yard --list` so they stay in sync with bin/yard; profile names are read
# from the repo's config/profiles/ (resolved via the yard symlink on PATH).

_yard_repo() {
  local bin
  bin="$(command -v "${1:-yard}" 2>/dev/null)" || return 1
  bin="$(readlink -f "$bin" 2>/dev/null)" || return 1
  ( cd "$(dirname "$bin")/.." && pwd )
}

_yard_profiles() {
  local repo; repo="$(_yard_repo "$1")" || return 0
  local d="$repo/config/profiles"
  [ -d "$d" ] || return 0
  local f
  for f in "$d"/*/profile.conf; do [ -r "$f" ] && basename "$(dirname "$f")"; done
}

# The native inventory owns yard/project discovery. A missing or incompatible engine
# fails soft; never rediscover registrations or project state in the shell.
# $2 is an optional literal prefix (e.g. '@' for the first-token selector).
_yard_yards() {
  local inventory n pfx="${2:-}"
  if inventory="$("${1:-yard}" list --complete-yards 2>/dev/null)" && [ -n "$inventory" ]; then
    while IFS= read -r n; do printf '%s%s\n' "$pfx" "$n"; done <<<"$inventory"
  else
    printf '%s%s\n' "$pfx" default
  fi
}

_yard_projects() {
  local inventory
  if inventory="$("${1:-yard}" list --complete-projects 2>/dev/null)" && [ -n "$inventory" ]; then
    printf '%s\n' "$inventory"
  fi
}

_yard() {
  local cur prev words cword cmd
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  cword=$COMP_CWORD

  local globals='-Y --yard -h --help -l --list --resources -V --version -y --yes'

  # A named-yard context may precede the command: -Y <name> / --yard <name> / --yard=<name>
  # / @<name>. Complete its VALUE with registry yard names, and skip it when locating the
  # command slot below.
  if [ "$prev" = "-Y" ] || [ "$prev" = "--yard" ]; then
    local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}")" -- "$cur") ); return 0
  fi
  # Readline splits @ in COMP_WORDS but includes it in the replacement text.
  if [ "$prev" = @ ]; then
    local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}" @)" -- "@$cur") ); return 0
  fi
  if [ "$prev" = = ] && [ "${COMP_WORDS[COMP_CWORD-2]:-}" = --yard ]; then
    local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}")" -- "$cur") ); return 0
  fi
  case "$cur" in
    @*)       local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}" @)" -- "$cur") ); return 0 ;;
    --yard=*) local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}" '--yard=')" -- "$cur") ); return 0 ;;
  esac

  # Command slot: 1, or shifted past a leading context selector.
  local cmdidx=1
  case "${COMP_WORDS[1]:-}" in
    -Y | @)         cmdidx=3 ;;
    --yard)
      if [ "${COMP_WORDS[2]:-}" = = ]; then cmdidx=4; else cmdidx=3; fi
      ;;
    --yard=* | @?*)  cmdidx=2 ;;
  esac

  # The command position: a global option or a command name.
  if [ "$cword" -eq "$cmdidx" ]; then
    local cmds
    cmds="$("${COMP_WORDS[0]}" --list 2>/dev/null)"
    case "$cur" in
      -*) COMPREPLY=( $(compgen -W "$globals" -- "$cur") ) ;;
      *)  COMPREPLY=( $(compgen -W "$cmds" -- "$cur") ) ;;
    esac
    return 0
  fi

  cmd="${COMP_WORDS[cmdidx]}"
  local provider command_options command_verbs filenames=false
  provider="$("${COMP_WORDS[0]}" --command-completion "$cmd" 2>/dev/null || true)"
  command_options="$("${COMP_WORDS[0]}" --command-options "$cmd" 2>/dev/null || true)"
  command_verbs="$("${COMP_WORDS[0]}" --command-verbs "$cmd" 2>/dev/null || true)"

  case "$provider" in
    project-env-up|project-env|remove|project|project-shell)
      # Project selectors may contain spaces; path candidates also need slash handling.
      if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else
        filenames=true
        local IFS=$'\n'
        COMPREPLY=( $(compgen -W "$(_yard_projects "${COMP_WORDS[0]}")" -- "$cur") )
        COMPREPLY+=( $(compgen -d -- "$cur") )
      fi
      ;;
    project-target)
      if [ "$prev" = "--target" ]; then COMPREPLY=( $(compgen -W "yard $(_yard_profiles "${COMP_WORDS[0]}")" -- "$cur") ); return 0; fi
      if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else filenames=true; local IFS=$'\n'; COMPREPLY=( $(compgen -d -- "$cur") ); fi
      ;;
    path)
      if [[ "$cur" == -* ]]; then COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else filenames=true; local IFS=$'\n'; COMPREPLY=( $(compgen -d -- "$cur") ); fi
      ;;
    profiles)
      if [[ "$cur" == -* ]]; then COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else COMPREPLY=( $(compgen -W "$(_yard_profiles "${COMP_WORDS[0]}")" -- "$cur") ); fi
      ;;
    teardown|stop|simple|status) COMPREPLY=( $(compgen -W "$command_options" -- "$cur") ) ;;
    remote)
      # remote <add|repair-key|remove|list>; repair/remove take a registered yard name.
      if [ "$cword" -eq "$((cmdidx + 1))" ]; then COMPREPLY=( $(compgen -W "$command_verbs" -- "$cur") )
      elif [ "${COMP_WORDS[cmdidx+1]}" = remove ] || [ "${COMP_WORDS[cmdidx+1]}" = repair-key ]; then local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}")" -- "$cur") )
      elif [ "${COMP_WORDS[cmdidx+1]}" = add ]; then [[ "$cur" == -* ]] && COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else COMPREPLY=( $(compgen -W "$command_options" -- "$cur") ); fi
      ;;
    keys)
      if [ "$cword" -eq "$((cmdidx + 1))" ]; then
        COMPREPLY=( $(compgen -W "$command_verbs" -- "$cur") )
      elif [ "${COMP_WORDS[cmdidx+1]}" = trust ] || [ "${COMP_WORDS[cmdidx+1]}" = untrust ] \
        || [ "${COMP_WORDS[cmdidx+1]}" = sync ] || [ "${COMP_WORDS[cmdidx+1]}" = move ]; then
        local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}" @)" -- "$cur") )
      elif [ "${COMP_WORDS[cmdidx+1]}" = import ] || [ "$prev" = --file ]; then
        filenames=true; local IFS=$'\n'
        COMPREPLY=( $(compgen -f -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      fi
      ;;
    config)
      local config_action="${COMP_WORDS[cmdidx+1]:-}"
      if [ "$cword" -eq "$((cmdidx + 1))" ]; then
        COMPREPLY=( $(compgen -W "$command_verbs" -- "$cur") )
      elif [ "$config_action" = sync ]; then
        local sync_action="${COMP_WORDS[cmdidx+2]:-}"
        if [ "$cword" -eq "$((cmdidx + 2))" ]; then
          COMPREPLY=( $(compgen -W "connect path status pull push help --check --adopt --apply --yes --help" -- "$cur") )
        elif [ "$prev" = --checkout ]; then
          filenames=true; local IFS=$'\n'
          COMPREPLY=( $(compgen -d -- "$cur") )
        else
          case "$sync_action" in
            connect) COMPREPLY=( $(compgen -W "--host-id --checkout --init --apply --yes --help" -- "$cur") ) ;;
            status) COMPREPLY=( $(compgen -W "--offline --help" -- "$cur") ) ;;
            pull) COMPREPLY=( $(compgen -W "--apply --yes --help" -- "$cur") ) ;;
            push) COMPREPLY=( $(compgen -W "-m --message --apply --yes --help" -- "$cur") ) ;;
            path|help) COMPREPLY=() ;;
            *) COMPREPLY=( $(compgen -W "--check --adopt --apply --yes --help" -- "$cur") ) ;;
          esac
        fi
      elif [ "$prev" = --scope ]; then
        COMPREPLY=( $(compgen -W "shared host yard" -- "$cur") )
      elif [ "$config_action" = import ] && [[ "$cur" != -* ]]; then
        filenames=true; local IFS=$'\n'
        COMPREPLY=( $(compgen -f -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "--scope --all-local --yes --help" -- "$cur") )
      fi
      ;;
    clone)
      if [ "$prev" = "--target" ]; then COMPREPLY=( $(compgen -W "yard $(_yard_profiles "${COMP_WORDS[0]}")" -- "$cur") ); return 0; fi
      [[ "$cur" == -* ]] && COMPREPLY=( $(compgen -W "$command_options" -- "$cur") ) ;;
    none) ;;
    *)
      # Profile-resource command (emu handled above for its bridge flags)? complete its verbs from
      # the registry (`yard --resources` => "<command>\t<verbs>"), so new resources need no edit here.
      local _rc _rv _verbs=''
      while IFS=$'\t' read -r _rc _rv; do [ "$_rc" = "$cmd" ] && { _verbs="$_rv"; break; }; done \
        < <("${COMP_WORDS[0]}" --resources 2>/dev/null)
      if [ -n "$_verbs" ]; then
        if [ "$cword" -eq 2 ]; then COMPREPLY=( $(compgen -W "$_verbs" -- "$cur") )
        else COMPREPLY=( $(compgen -W '--yes --help' -- "$cur") ); fi
      fi
      ;;  # otherwise: leave to default
  esac
  # Do not mark commands, options, profiles or yard names as filenames: a matching
  # directory in the current checkout must not append '/' to those candidates.
  if $filenames; then compopt -o filenames 2>/dev/null || true; fi
  return 0
}

complete -F _yard yard sy

# These Readline bindings apply shell-wide. Configure both insert keymaps without
# switching the user's editing mode; Enter keeps its normal accept-line behavior.
if [[ $- == *i* ]]; then
  bind 'set show-all-if-ambiguous on'
  bind -m emacs-standard '"\C-i": menu-complete'
  bind -m emacs-standard '"\e[Z": menu-complete-backward'
  bind -m vi-insert '"\C-i": menu-complete'
  bind -m vi-insert '"\e[Z": menu-complete-backward'
fi

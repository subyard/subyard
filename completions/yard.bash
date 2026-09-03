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

# Registry yard names come from the CLI. The compatibility fallback mirrors its supported
# private-flat and installed nested/flat layouts without following symlinks. $2, if set, is a
# prefix emitted before each name (e.g. '@' for the first-token sugar).
_yard_yards() {
  local config_dir private_dir pfx="${2:-}" entry kind f n home inventory parent emitted=$'\ndefault\n'
  local -a files=()
  if inventory="$("${1:-yard}" list --complete-yards 2>/dev/null)" && [ -n "$inventory" ]; then
    while IFS= read -r n; do printf '%s%s\n' "$pfx" "$n"; done <<<"$inventory"
    return 0
  fi
  printf '%s%s\n' "$pfx" default
  config_dir="$(_yard_config_dir "$1")" || config_dir=""
  if [ -n "$config_dir" ]; then
    private_dir="$(dirname "$config_dir")/private/yards"
    for f in "$private_dir"/*.env; do files+=( "flat:$f" ); done
  fi
  home="$(_yard_config_home "$1")" || home=""
  if [ -n "$home" ]; then
    for f in "$home/yards"/*.env; do files+=( "flat:$f" ); done
    for f in "$home/yards"/*/config.env; do files+=( "nested:$f" ); done
  fi
  for entry in "${files[@]}"; do
    kind="${entry%%:*}"
    f="${entry#*:}"
    if [ ! -f "$f" ] || [ -L "$f" ]; then continue; fi
    if [ "$kind" = nested ]; then
      parent="$(dirname "$f")"
      [ ! -L "$parent" ] || continue
      n="$(basename "$parent")"
    else
      n="$(basename "$f" .env)"
    fi
    case "$n" in ""|*[!a-z0-9_-]*|[!a-z0-9]*) continue ;; esac
    case "$emitted" in *$'\n'"$n"$'\n'*) continue ;; esac
    emitted+="$n"$'\n'
    printf '%s%s\n' "$pfx" "$n"
  done
}

# Shipped config root: honor the same environment override as the native loader.
_yard_config_dir() {
  if [ -n "${SUBYARD_CONFIG_DIR:-}" ]; then printf '%s\n' "$SUBYARD_CONFIG_DIR"; return 0; fi
  local repo; repo="$(_yard_repo "$1")" || return 1
  printf '%s\n' "$repo/config"
}

# Host-side state home: honor an explicit override, else derive the same default as
# config/host.env (so completion and the CLI can never disagree on where state lives).
_yard_config_home() {
  if [ -n "${SUBYARD_CONFIG_HOME:-}" ]; then printf '%s\n' "$SUBYARD_CONFIG_HOME"; return 0; fi
  if [ -n "${XDG_CONFIG_HOME:-}" ]; then printf '%s/subyard\n' "$XDG_CONFIG_HOME"; return 0; fi
  local repo; repo="$(_yard_repo "$1")" || return 1
  [ -r "$repo/config/host.env" ] || return 1
  ( . "$repo/config/host.env" >/dev/null 2>&1; printf '%s\n' "${SUBYARD_CONFIG_HOME:-}" )
}

# Project selectors from the native bounded inventory. If an older/missing engine cannot provide
# it, fall back to local state and qualify every project with the local HostID so duplicate names
# still resolve. Project names are SafeProjectName values, so they never contain JSON escapes.
_yard_projects() {
  local home inventory
  if inventory="$("${1:-yard}" list --complete-projects 2>/dev/null)" && [ -n "$inventory" ]; then
    printf '%s\n' "$inventory"
    return 0
  fi
  home="$(_yard_config_home "$1")" || return 0
  [ -n "$home" ] || return 0
  local d f name host_id="" yard emitted=""
  [ ! -r "$home/host-id" ] || IFS= read -r host_id <"$home/host-id"
  d="$home/projects"
  for f in "$d"/*.json; do
    [ -e "$f" ] || continue
    name="$(sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"\\]*\)".*/\1/p' "$f" | head -n1)"
    [ -n "$name" ] || continue
    if [ -n "$host_id" ]; then
      printf '%s/%s\n' "$name" "$host_id"
    elif case $'\n'"$emitted" in *$'\n'"$name"$'\n'*) true;; *) false;; esac; then
      :
    else
      emitted="${emitted}${name}"$'\n'; printf '%s\n' "$name"
    fi
  done
  for d in "$home"/yards/*/projects; do
    [ -d "$d" ] || continue
    yard="$(basename "$(dirname "$d")")"
    for f in "$d"/*.json; do
      [ -e "$f" ] || continue
      name="$(sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"\\]*\)".*/\1/p' "$f" | head -n1)"
      [ -n "$name" ] || continue
      if [ -n "$host_id" ]; then
        printf '%s/%s/%s\n' "$name" "$yard" "$host_id"
      elif case $'\n'"$emitted" in *$'\n'"$name"$'\n'*) true;; *) false;; esac; then
        :
      else
        emitted="${emitted}${name}"$'\n'; printf '%s\n' "$name"
      fi
    done
  done
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
  case "$cur" in
    @*)       local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}" @)" -- "$cur") ); return 0 ;;
    --yard=*) local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_yards "${COMP_WORDS[0]}" '--yard=')" -- "$cur") ); return 0 ;;
  esac

  # Command slot: 1, or shifted past a leading context selector.
  local cmdidx=1
  case "${COMP_WORDS[1]:-}" in
    -Y | --yard)     cmdidx=3 ;;
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
  local provider command_options command_verbs
  provider="$("${COMP_WORDS[0]}" --command-completion "$cmd" 2>/dev/null || true)"
  command_options="$("${COMP_WORDS[0]}" --command-options "$cmd" 2>/dev/null || true)"
  command_verbs="$("${COMP_WORDS[0]}" --command-verbs "$cmd" 2>/dev/null || true)"

  case "$provider" in
    project-env-up)
      # up [path|name] [--rebuild]
      [[ "$cur" == -* ]] && COMPREPLY=( $(compgen -W "$command_options" -- "$cur") ) \
        || { local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_projects "${COMP_WORDS[0]}")" -- "$cur") ); COMPREPLY+=( $(compgen -d -- "$cur") ); }
      ;;
    project-env)
      [[ "$cur" == -* ]] && COMPREPLY=( $(compgen -W "$command_options" -- "$cur") ) \
        || { local IFS=$'\n'; COMPREPLY=( $(compgen -W "$(_yard_projects "${COMP_WORDS[0]}")" -- "$cur") ); COMPREPLY+=( $(compgen -d -- "$cur") ); }
      ;;
    remove)
      if [[ "$cur" == -* ]]; then COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else
        local IFS=$'\n'
        COMPREPLY=( $(compgen -W "$(_yard_projects "${COMP_WORDS[0]}")" -- "$cur") )
        COMPREPLY+=( $(compgen -d -- "$cur") )
      fi
      ;;
    project-target)
      if [ "$prev" = "--target" ]; then COMPREPLY=( $(compgen -W "yard $(_yard_profiles "${COMP_WORDS[0]}")" -- "$cur") ); return 0; fi
      if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else COMPREPLY=( $(compgen -d -- "$cur") ); fi
      ;;
    path) [[ "$cur" == -* ]] && COMPREPLY=( $(compgen -W "$command_options" -- "$cur") ) || COMPREPLY=( $(compgen -d -- "$cur") ) ;;
    profiles)
      if [[ "$cur" == -* ]]; then COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else COMPREPLY=( $(compgen -W "$(_yard_profiles "${COMP_WORDS[0]}")" -- "$cur") ); fi
      ;;
    project|project-shell)
      # `yard code` and `yard shell` take a project NAME (from `yard list`) or a directory path.
      if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$command_options" -- "$cur") )
      else
        local IFS=$'\n'  # keep project names with spaces intact
        COMPREPLY=( $(compgen -W "$(_yard_projects "${COMP_WORDS[0]}")" -- "$cur") )
        COMPREPLY+=( $(compgen -d -- "$cur") )
      fi
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
  return 0
}

complete -F _yard yard sy

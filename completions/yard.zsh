#compdef yard sy
# yard.zsh — zsh completion for the `yard` (and `sy`) CLI.
# Place on $fpath (e.g. ~/.zsh/completions) as `_yard`, or source it directly.
# Top-level commands come from `yard --list`; profiles from config/profiles/.

_yard_repo() {
  local bin
  bin="$(command -v yard 2>/dev/null)" || return 1
  bin="${bin:A}"               # resolve symlink
  print -r -- "${bin:h:h}"     # dirname twice → repo root
}

_yard_profiles() {
  local repo d
  repo="$(_yard_repo)" || return 0
  d="$repo/config/profiles"
  [[ -d $d ]] || return 0
  local -a profiles
  profiles=( $d/*/profile.conf(N:h:t) )
  print -r -l -- $profiles
}

# Registry yard names come from the CLI. The compatibility fallback mirrors its supported
# private-flat and installed nested/flat layouts without following symlinks.
_yard_yards() {
  local config_dir private_dir home entry kind f n parent inventory
  local -a names files
  local -A seen
  inventory="$(yard list --complete-yards 2>/dev/null)"
  if [[ -n $inventory ]]; then
    print -r -- "$inventory"
    return 0
  fi
  names=( default )
  seen[default]=1
  config_dir="$(_yard_config_dir)" || config_dir=""
  if [[ -n $config_dir ]]; then
    private_dir="${config_dir:h}/private/yards"
    for f in $private_dir/*.env(N); do files+=( "flat:$f" ); done
  fi
  home="$(_yard_config_home)" || home=""
  if [[ -n $home ]]; then
    for f in $home/yards/*.env(N); do files+=( "flat:$f" ); done
    for f in $home/yards/*/config.env(N); do files+=( "nested:$f" ); done
  fi
  for entry in $files; do
    kind=${entry%%:*}
    f=${entry#*:}
    [[ -f $f && ! -L $f ]] || continue
    if [[ $kind == nested ]]; then
      parent=${f:h}
      [[ ! -L $parent ]] || continue
      n=${parent:t}
    else
      n=${f:t:r}
    fi
    case "$n" in ""|*[!a-z0-9_-]*|[!a-z0-9]*) continue ;; esac
    [[ -z ${seen[$n]:-} ]] || continue
    seen[$n]=1
    names+=( $n )
  done
  print -r -l -- $names
}

# Shipped config root: honor the same environment override as the native loader.
_yard_config_dir() {
  if [[ -n ${SUBYARD_CONFIG_DIR:-} ]]; then print -r -- "$SUBYARD_CONFIG_DIR"; return 0; fi
  local repo; repo="$(_yard_repo)" || return 1
  print -r -- "$repo/config"
}

# _arguments action: complete a yard name for -Y/--yard.
_yard_yard_names() {
  local -a n; n=( ${(f)"$(_yard_yards)"} )
  compadd -a n
}

# Host-side state home: honor an explicit override, else derive the same default as
# config/host.env (so completion and the CLI agree on where state lives).
_yard_config_home() {
  if [[ -n ${SUBYARD_CONFIG_HOME:-} ]]; then print -r -- "$SUBYARD_CONFIG_HOME"; return 0; fi
  if [[ -n ${XDG_CONFIG_HOME:-} ]]; then print -r -- "$XDG_CONFIG_HOME/subyard"; return 0; fi
  local repo; repo="$(_yard_repo)" || return 1
  [[ -r $repo/config/host.env ]] || return 1
  ( source "$repo/config/host.env" >/dev/null 2>&1; print -r -- "${SUBYARD_CONFIG_HOME:-}" )
}

# Project selectors from the native bounded inventory. If an older/missing engine cannot provide
# it, fall back to local state and qualify every project with the local HostID so duplicate names
# still resolve. Project names are SafeProjectName values, so they never contain JSON escapes.
_yard_projects() {
  local home d f name inventory host_id='' yard
  local -A emitted
  if inventory="$(yard list --complete-projects 2>/dev/null)" && [[ -n $inventory ]]; then
    print -r -- "$inventory"
    return 0
  fi
  home="$(_yard_config_home)" || return 0
  [[ -n $home ]] || return 0
  [[ ! -r $home/host-id ]] || IFS= read -r host_id < "$home/host-id"
  d="$home/projects"
  for f in $d/*.json(N); do
    name="$(sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"\\]*\)".*/\1/p' "$f" | head -n1)"
    [[ -n $name ]] || continue
    if [[ -n $host_id ]]; then
      print -r -- "$name/$host_id"
    elif [[ -z ${emitted[$name]:-} ]]; then
      emitted[$name]=1; print -r -- "$name"
    fi
  done
  for d in "$home"/yards/*/projects(N/); do
    yard="$(basename "$(dirname "$d")")"
    for f in $d/*.json(N); do
      name="$(sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"\\]*\)".*/\1/p' "$f" | head -n1)"
      [[ -n $name ]] || continue
      if [[ -n $host_id ]]; then
        print -r -- "$name/$yard/$host_id"
      elif [[ -z ${emitted[$name]:-} ]]; then
        emitted[$name]=1; print -r -- "$name"
      fi
    done
  done
}

# `yard code` target: a known project name or a directory path.
_yard_code_target() {
  local -a projs; projs=( ${(f)"$(_yard_projects)"} )
  _alternative \
    'projects:project:compadd -a projs' \
    'directories:directory:_files -/'
}

_yard() {
  local -a cmds
  cmds=( ${(f)"$(yard --list 2>/dev/null)"} )

  local curcontext="$curcontext" state line
  typeset -A opt_args

  _arguments -C \
    '(-Y --yard)'{-Y,--yard}'[run the command against a named yard]:yard:_yard_yard_names' \
    '(-h --help)'{-h,--help}'[show help]' \
    '(-l --list)'{-l,--list}'[list command names]' \
    '--resources[list profile resource commands and verbs]' \
    '(-V --version)'{-V,--version}'[show version]' \
    '(-y --yes)'{-y,--yes}'[skip confirmation prompt]' \
    '1: :->cmd' \
    '*:: :->args' \
    && return 0

  case $state in
    cmd)
      _describe -t commands 'yard command' cmds
      # First-token sugar: @<name> selects a yard context (== -Y <name>).
      local -a atnames; atnames=( ${${(f)"$(_yard_yards)"}/#/@} )
      _describe -t yards 'yard context (@name)' atnames
      ;;
    args)
      local provider="$(yard --command-completion "${words[1]}" 2>/dev/null)"
      local command_options="$(yard --command-options "${words[1]}" 2>/dev/null)"
      local command_verbs="$(yard --command-verbs "${words[1]}" 2>/dev/null)"
      local -a registry_options; registry_options=( ${(z)command_options} )
      case $provider in
        project-env-up|project-env) _arguments ${registry_options[@]} '*:project:_yard_code_target' ;;
        remove) _arguments ${registry_options[@]} '*:project:_yard_code_target' ;;
        project-target)
          if [[ ${words[CURRENT-1]} == --target ]]; then
            local -a tg; tg=( yard ${(f)"$(_yard_profiles)"} )
            _describe -t targets 'target' tg
          else
            registry_options=( ${registry_options:#--target} )
            _arguments '--target[where it runs: yard or a profile]:target:->tgt' ${registry_options[@]} '*:project:_files -/'
          fi
          ;;
        path) _arguments ${registry_options[@]} '*:project:_files -/' ;;
        profiles)
          local -a profiles; profiles=( ${(f)"$(_yard_profiles)"} )
          _arguments ${registry_options[@]} '*:profile:compadd -a profiles'
          ;;
        project) _arguments ${registry_options[@]} '*:project:_yard_code_target' ;;
        project-shell) _arguments ${registry_options[@]} '1:project:_yard_code_target' '*::command: _normal' ;;
        status)
          _arguments '--all[summarize all yards even when a selector is present]' \
            '--yes[accept the compatible global option]' '--help[show help]'
          ;;
        stop|simple|teardown) _arguments ${registry_options[@]} ;;
        remote)
          if (( CURRENT == 2 )); then
            local -a sub; sub=( ${=command_verbs} )
            _describe -t subcommands 'remote subcommand' sub
          elif [[ ${words[2]} == remove || ${words[2]} == repair-key ]]; then
            local -a n; n=( ${(f)"$(_yard_yards)"} ); _describe -t yards 'remote yard' n
          elif [[ ${words[2]} == add ]]; then
            registry_options=( ${registry_options:#--yard} )
            _arguments '--yard[target a named yard on the remote host]:remote yard:' ${registry_options[@]}
          else
            _arguments ${registry_options[@]}
          fi
          ;;
        keys)
          if (( CURRENT == 2 )); then
            local -a sub; sub=( ${=command_verbs} )
            _describe -t subcommands 'keys subcommand' sub
          elif [[ ${words[2]} == trust || ${words[2]} == untrust || ${words[2]} == sync || ${words[2]} == move ]]; then
            local -a kn; kn=( ${${(f)"$(_yard_yards)"}/#/@} ); _describe -t yards 'key peer' kn
          elif [[ ${words[2]} == import || ${words[CURRENT-1]} == --file ]]; then
            _files
          else
            _arguments ${registry_options[@]}
          fi
          ;;
        config)
          if (( CURRENT == 2 )); then
            local -a sub; sub=( ${=command_verbs} )
            _describe -t subcommands 'config subcommand' sub
          elif [[ ${words[2]} == sync ]]; then
            if (( CURRENT == 3 )); then
              local -a syncsub
              syncsub=( connect path status pull push help --check --adopt --apply --yes --help )
              _describe -t subcommands 'config sync action' syncsub
            else
              case ${words[3]} in
                connect)
                  _arguments '--host-id[owner host ID]:host ID:' \
                    '--checkout[private checkout path]:directory:_directories' \
                    '--init[initialize an empty remote]' \
                    '--apply[refresh affected running yards]' \
                    '--yes[skip confirmation]' '--help[show help]'
                  ;;
                status) _arguments '--offline[use cached refs only]' '--help[show help]' ;;
                pull) _arguments '--apply[refresh affected running yards]' '--yes[skip confirmation]' '--help[show help]' ;;
                push)
                  _arguments '(-m --message)'{-m,--message}'[commit message]:message:' \
                    '--apply[refresh affected running yards]' \
                    '--yes[skip confirmation]' '--help[show help]'
                  ;;
                path|help) ;;
                *) _arguments '--check[read-only convergence check]' '--adopt[adopt unmanaged live files]' \
                    '--apply[refresh affected running yards]' '--yes[skip confirmation]' '--help[show help]' '*:checkout:_directories' ;;
              esac
            fi
          elif [[ ${words[CURRENT-1]} == --scope ]]; then
            local -a scopes; scopes=( shared host yard )
            _describe -t scopes 'persistent scope' scopes
          elif [[ ${words[2]} == import ]]; then
            _arguments '--scope[persistent scope]:scope:(shared host yard)' \
              '--yes[skip confirmation]' '1:setting:' '2:file:_files'
          elif [[ ${words[2]} == set ]]; then
            _arguments '--scope[persistent scope]:scope:(shared host yard)' \
              '--yes[skip confirmation]' '1:setting:' '2:value:'
          elif [[ ${words[2]} == unset || ${words[2]} == edit ]]; then
            _arguments '--scope[persistent scope]:scope:(shared host yard)' \
              '--yes[skip confirmation]' '1:setting:'
          else
            _arguments ${registry_options[@]}
          fi
          ;;
        clone)
          if [[ ${words[CURRENT-1]} == --target ]]; then
            local -a tg; tg=( yard ${(f)"$(_yard_profiles)"} )
            _describe -t targets 'target' tg
          else
            registry_options=( ${registry_options:#--target} )
            _arguments '--target[where it runs: yard or a profile]:target:->tgt' ${registry_options[@]} '*: :_message "repository URL"'
          fi
          ;;
        none) ;;
        *)
          # Profile-resource command (emu handled above)? complete its verbs from the registry
          # (`yard --resources` => "<command>\t<verbs>"), so new resources need no edit here.
          local rline rc rv
          for rline in "${(@f)$(yard --resources 2>/dev/null)}"; do
            rc=${rline%%$'\t'*}; rv=${rline#*$'\t'}
            if [[ $rc == ${words[1]} ]]; then
              if (( CURRENT == 2 )); then
                local -a vv; vv=( ${=rv} ); _describe -t verbs "${words[1]} verb" vv
              else
                _arguments '--yes[skip prompt]'
              fi
              return 0
            fi
          done
          _arguments '--yes[skip prompt]' '--help[show help]'
          ;;
      esac
      ;;
  esac
}

# Register. Works whether this file is autoloaded on $fpath or sourced from
# .zshrc. When sourced before compinit ran, bootstrap it so compdef exists.
if (( ! $+functions[compdef] )); then
  autoload -Uz compinit && compinit
fi
compdef _yard yard sy 2>/dev/null

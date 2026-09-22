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

# The native inventory owns yard/project discovery. A missing or incompatible engine
# fails soft; never rediscover registrations or project state in the shell.
_yard_yards() {
  local inventory
  if inventory="$(yard list --complete-yards 2>/dev/null)" && [[ -n $inventory ]]; then
    print -r -- "$inventory"
  else
    print -r -- default
  fi
}

# _arguments action: complete a yard name for -Y/--yard.
_yard_yard_names() {
  local -a n; n=( ${(f)"$(_yard_yards)"} )
  compadd -a n
}

_yard_projects() {
  local inventory
  if inventory="$(yard list --complete-projects 2>/dev/null)" && [[ -n $inventory ]]; then
    print -r -- "$inventory"
  fi
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

  # Parse a completed @ selector as the equivalent option without changing the buffer.
  local -a words=( "${words[@]}" )
  if (( CURRENT > 2 )) && [[ ${words[2]:-} == @?* ]]; then
    words[2]="--yard=${words[2]#@}"
  fi

  local curcontext="$curcontext" state line
  typeset -A opt_args

  _arguments -C \
    '(-Y --yard)'{-Y,--yard=}'[run the command against a named yard]:yard:_yard_yard_names' \
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
        integration)
          if (( CURRENT == 2 )); then
            local -a sub; sub=( ${=command_verbs} )
            _describe -t subcommands "integration subcommand" sub
          else
            _arguments ${registry_options[@]}
          fi
          ;;
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
        ssh-agent)
          if (( CURRENT == 2 )); then
            local -a sub; sub=( ${=command_verbs} )
            _describe -t subcommands 'ssh-agent subcommand' sub
          elif [[ ${words[2]} == unlock ]]; then
            _arguments '--key[private key path]:private key:_files' \
              '--ttl[key lifetime (for example 2h)]:duration:' \
              '--yes[skip confirmation]' '--help[show help]'
          elif [[ ${words[2]} == status ]]; then
            _arguments '--json[print machine-readable status]' '--help[show help]'
          elif [[ ${words[2]} == lock ]]; then
            _arguments '--help[show help]'
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
if [[ -o interactive ]] && (( ! $+functions[compdef] )); then
  autoload -Uz compinit && compinit
fi
if (( $+functions[compdef] )); then
  compdef _yard yard sy 2>/dev/null
fi

# These ZLE bindings apply shell-wide. Configure both insert keymaps without
# switching the user's editing mode; Enter keeps its normal accept-line behavior.
if [[ -o interactive ]]; then
  bindkey -M emacs '^I' menu-complete
  bindkey -M emacs '^[[Z' reverse-menu-complete
  bindkey -M viins '^I' menu-complete
  bindkey -M viins '^[[Z' reverse-menu-complete
fi

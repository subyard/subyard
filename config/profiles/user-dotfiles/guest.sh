#!/usr/bin/env bash
# As the yard developer: clone the dotfiles source and install it once; uninstall on disable.
# Rules: docs/user-dotfiles.md.
# Usage: guest.sh check|verify|apply [URL [INSTALL_COMMAND]]; an empty URL disables.
# Check and verify print "work" for drift.
set -euo pipefail
mode="$1"
url="${2:-}"
command="${3:-}"
cd "$HOME"
store="$HOME/.local/share/subyard-user-dotfiles"
state="$store/.installed" # OWNER/REPO of the installed clone; names never start with a dot
scripts=(install.sh install bootstrap.sh bootstrap script/bootstrap setup.sh setup script/setup)
segment='[A-Za-z0-9][A-Za-z0-9._-]*'
work=0
skipped() {
  case "$mode" in check) work=1 ;; apply) printf 'user-dotfiles: warning: %s\n' "$1" >&2 ;; esac
}
run_script() { # <clone> <relative script>: run it in the clone; on failure warn with its output
  local log rc=0
  log="$(mktemp)"
  if [ -x "$1/$2" ]; then
    (cd "$1" && timeout -k 10 300 "./$2") </dev/null >"$log" 2>&1 || rc=$?
  else
    (cd "$1" && timeout -k 10 300 bash "./$2") </dev/null >"$log" 2>&1 || rc=$?
  fi
  if [ "$rc" != 0 ]; then
    printf 'user-dotfiles: warning: %s/%s exited with status %s; once fixed, run it again in %s. Its output:\n' \
      "${1#"$store/"}" "$2" "$rc" "$1" >&2
    tail -n 50 -- "$log" | sed 's/^/  /' >&2
  fi
  rm -f -- "$log"
}
install_clone() { # <clone>: the configured command, else the first script, else link top-level dot entries
  local script entry
  if [ -n "$command" ]; then
    if [[ "$command" =~ ^$segment(/$segment)*$ ]] && [ -f "$1/$command" ]; then
      run_script "$1" "$command"
    else
      printf 'user-dotfiles: warning: USER_DOTFILES_INSTALL_COMMAND names no file in %s; nothing was installed\n' "${1#"$store/"}" >&2
    fi
    return
  fi
  for script in "${scripts[@]}"; do
    if [ -f "$1/$script" ]; then run_script "$1" "$script"; return; fi
  done
  for entry in "$1"/.[!.]* "$1"/..?*; do
    [ -e "$entry" ] || [ -L "$entry" ] || continue
    [ "${entry##*/}" != .git ] || continue
    if [ -e "$HOME/${entry##*/}" ] || [ -L "$HOME/${entry##*/}" ]; then
      printf 'user-dotfiles: warning: left ~/%s in place; move it away and link it to %s yourself\n' "${entry##*/}" "$entry" >&2
    else
      ln -sT -- "$entry" "$HOME/${entry##*/}"
    fi
  done
}
uninstall_clone() { # <OWNER/REPO>: run uninstall.sh when the clone has one, then forget the install
  local clone="$store/$1"
  if ! [[ "$1" =~ ^$segment/$segment$ ]]; then
    printf 'user-dotfiles: warning: forgot an invalid install record\n' >&2
  elif [ -f "$clone/uninstall.sh" ]; then
    run_script "$clone" uninstall.sh
  else
    printf 'user-dotfiles: warning: %s has no uninstall.sh; left the files it installed in place\n' "$1" >&2
  fi
  rm -f -- "$state"
}
installed=''
if [ -f "$state" ]; then installed="$(<"$state")"; fi
name=''
if [ -n "$url" ]; then
  # Query and fragment go first, then the authority (with userinfo) or the SCP host.
  path="${url%%[?#]*}"
  case "$path" in
    *://*) path="${path#*://}"; case "$path" in */*) path="${path#*/}" ;; *) path="" ;; esac ;;
    \[*\]:*|*@\[*\]:*) path="${path#*\]:}" ;; # bracketed SCP host, such as git@[::1]:owner/repo
    *) path="${path#*:}" ;;
  esac
  path="${path%/}"
  path="${path%.git}"
  path="${path%/}"
  path="${path#/}"
  name="${path#"${path%/*/*}/"}"
  if ! [[ "$name" =~ ^$segment/$segment$ ]]; then
    name=''
    skipped 'skipped the source: its path has no valid owner/repo; fix USER_DOTFILES_SOURCE'
  fi
fi
# Disabled, or the source moved to another repository: uninstall what was installed.
if [ -n "$installed" ] && [ "$installed" != "$name" ] && { [ -n "$name" ] || [ -z "$url" ]; }; then
  if [ "$mode" = apply ]; then uninstall_clone "$installed"; installed=''; else work=1; fi
fi
# Not yet installed, or its clone is gone: clone when needed, then install once.
if [ -n "$name" ] && { [ "$installed" != "$name" ] || [ -z "$(ls -A "$store/$name" 2>/dev/null)" ]; }; then
  clone="$store/$name"
  case "$mode" in
    check) work=1 ;;
    verify) ;; # Apply warned about a source it could not clone; check reports it for retry.
    apply)
      # An existing clone is reused as is: the yard never pulls or replaces it.
      if [ -z "$(ls -A "$clone" 2>/dev/null)" ]; then
        mkdir -p "${clone%/*}"
        temporary="$(mktemp -d "${clone%/*}/.tmp-${clone##*/}.XXXXXX")"
        if GIT_TERMINAL_PROMPT=0 GIT_ALLOW_PROTOCOL=https:ssh:file:subyard-github timeout -k 10 60 \
          git clone -q -- "$url" "$temporary" </dev/null >/dev/null 2>&1; then
          mv -T "$temporary" "$clone" # replaces only an empty directory
        else
          rmdir "$temporary" 2>/dev/null || :
          skipped "could not clone $name; fix Git access or USER_DOTFILES_SOURCE, then retry yard profile enable user-dotfiles or yard init"
        fi
      fi
      if [ -n "$(ls -A "$clone" 2>/dev/null)" ]; then
        # Recorded before the install runs: a failed install is not retried.
        printf '%s\n' "$name" >"$state.tmp"
        mv -fT "$state.tmp" "$state"
        install_clone "$clone"
      fi
      ;;
  esac
fi
if [ "$work" = 1 ]; then printf 'work\n'; fi

#!/usr/bin/env bash
# As the yard developer: clone the dotfiles source once and link agent instructions.
# Rules: docs/user-dotfiles.md.
# Usage: guest.sh check|verify|apply [URL [SUBDIR]]; check and verify print "work" for drift.
set -euo pipefail
mode="$1"
url="${2:-}"
subdir="${3:-}"
cd "$HOME"
store="$HOME/.local/share/subyard-user-dotfiles"
links=(.claude/CLAUDE.md .codex/AGENTS.md .config/opencode/AGENTS.md)
segment='[A-Za-z0-9][A-Za-z0-9._-]*'
work=0
target='' # the configured instruction file, empty when none is linked
link_text() { # <link>: the exact text; command substitution would strip trailing newlines
  local text
  text="$(readlink -- "$1"; printf x)"
  text="${text%x}"
  printf '%s' "${text%$'\n'}"
}
skipped() {
  case "$mode" in check) work=1 ;; apply) printf 'user-dotfiles: warning: %s\n' "$1" >&2 ;; esac
}
# Pass 1: parse and clone the source, giving the link target.
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
  subdir="${subdir#/}"
  subdir="${subdir%/}"
  if ! [[ "$name" =~ ^$segment/$segment$ ]]; then
    skipped 'skipped the source: its path has no valid owner/repo; fix USER_DOTFILES_SOURCE'
  elif [ -n "$subdir" ] && ! [[ "$subdir" =~ ^$segment(/$segment)*$ ]]; then
    skipped 'skipped the source: USER_DOTFILES_SUBDIR is not a relative path of plain names; fix it'
  else
    clone="$store/$name"
    if [ -z "$(ls -A "$clone" 2>/dev/null)" ]; then
      # Verify accepts a source that apply could not clone; check reports it for retry.
      case "$mode" in
        check) work=1 ;;
        verify) ;;
        apply)
          mkdir -p "${clone%/*}"
          temporary="$(mktemp -d "${clone%/*}/.tmp-${clone##*/}.XXXXXX")"
          if GIT_TERMINAL_PROMPT=0 GIT_ALLOW_PROTOCOL=https:ssh:file:subyard-github timeout -k 10 60 \
            git clone -q -- "$url" "$temporary" </dev/null >/dev/null 2>&1; then
            mv -T "$temporary" "$clone" # replaces only an empty directory
          else
            rmdir "$temporary" 2>/dev/null || :
            skipped "could not clone $name; fix Git access or USER_DOTFILES_SOURCE, then retry yard profile enable user-dotfiles or yard init"
          fi
          ;;
      esac
    fi
    file="$clone${subdir:+/$subdir}/agents/AGENTS.md"
    if [ -f "$file" ]; then
      target="$file"
    elif [ -n "$(ls -A "$clone" 2>/dev/null)" ]; then
      skipped "$name${subdir:+/$subdir} has no agents/AGENTS.md; add it or fix USER_DOTFILES_SUBDIR"
    fi
  fi
fi
# Pass 2: ours is a link whose text is exactly <store>/OWNER/REPO[/SUBDIR]/agents/AGENTS.md.
# Keep it only when it is the configured target.
for relative in "${links[@]}"; do
  link="$HOME/$relative"
  [ -L "$link" ] || continue
  text="$(link_text "$link"; printf x)"
  text="${text%x}"
  [[ "$text" =~ ^"$store"/$segment(/$segment)+/agents/AGENTS\.md$ ]] || continue
  [ -z "$target" ] || [ "$text" != "$target" ] || continue
  if [ "$mode" = apply ]; then unlink -- "$link"; else work=1; fi
done
# Pass 3: link each agent instruction path that is free.
if [ -n "$target" ]; then
  for relative in "${links[@]}"; do
    link="$HOME/$relative"
    if [ -L "$link" ]; then
      text="$(link_text "$link"; printf x)"
      [ "${text%x}" != "$target" ] || continue
    fi
    if [ -e "$link" ] || [ -L "$link" ]; then
      skipped "left ~/$relative in place: it is not a link to $name${subdir:+/$subdir}/agents/AGENTS.md; move it away to link it"
      continue
    fi
    if [ "$mode" = apply ]; then
      mkdir -p "${link%/*}"
      ln -sT -- "$target" "$link"
    else
      work=1
    fi
  done
fi
if [ "$work" = 1 ]; then printf 'work\n'; fi

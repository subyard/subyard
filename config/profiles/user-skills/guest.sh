#!/usr/bin/env bash
# As the yard developer: clone each missing source once, then link its skills.
# Usage: guest.sh check|verify|apply URL...; check and verify print "work" for drift.
set -euo pipefail
mode="$1"
shift
cd "$HOME"
store="$HOME/.local/share/subyard-user-skills"
work=0
for url in "$@"; do
  # Label from the repository path only: never userinfo, host, query or fragment.
  case "$url" in
    *://*) name="${url#*://}"; name="${name#*/}" ;;
    *) name="${url#*:}" ;;
  esac
  name="${name%%[?#]*}"
  name="${name%/}"
  name="${name%.git}"
  name="${name%/}"
  name="${name##*/}"
  case "$name" in ''|.|..|*[!A-Za-z0-9._-]*) name=source ;; esac
  clone="$store/$name"
  if [ -z "$(ls -A "$clone" 2>/dev/null)" ]; then
    # Verify accepts a source that apply could not clone; check reports it for retry.
    case "$mode" in check) work=1; continue ;; verify) continue ;; esac
    mkdir -p "$store"
    temporary="$(mktemp -d "$store/.tmp-$name.XXXXXX")"
    if GIT_TERMINAL_PROMPT=0 timeout -k 10 60 git clone -q -- "$url" "$temporary" </dev/null >/dev/null 2>&1 \
      || { command -v subyard-github >/dev/null && GIT_TERMINAL_PROMPT=0 timeout -k 10 60 \
        subyard-github run -- git clone -q -- "$url" "$temporary" </dev/null >/dev/null 2>&1; }; then
      mv -T "$temporary" "$clone" # replaces only an empty directory
    else
      rmdir "$temporary" 2>/dev/null || :
      printf 'user-skills: warning: could not clone %s; fix Git access or USER_SKILL_SOURCES, then run yard init\n' "$name" >&2
      continue
    fi
  fi
  for manifest in "$clone"/skills/*/SKILL.md; do
    [ -f "$manifest" ] || continue
    skill="${manifest%/SKILL.md}"
    case "${skill##*/}" in subyard-*) continue ;; esac # shipped skill names
    for harness in .agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills; do
      link="$HOME/$harness/${skill##*/}"
      if [ -e "$link" ] || [ -L "$link" ]; then continue; fi # never replace an existing entry
      if [ "$mode" = apply ]; then
        mkdir -p "${link%/*}"
        ln -s "$skill" "$link"
      else
        work=1
      fi
    done
  done
done
if [ "$work" = 1 ]; then printf 'work\n'; fi

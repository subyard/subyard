#!/usr/bin/env bash
# As the yard developer: clone each missing source once, then link its skills.
# Usage: guest.sh check|verify|apply|remove|check-remove URL...; checks print "work" for drift.
# remove unlinks every link into the clone store and keeps the clones.
set -euo pipefail
mode="$1"
shift
cd "$HOME"
store="$HOME/.local/share/subyard-user-skills"
harnesses=(.agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills)
work=0
# Only our links: absolute targets without . or .. segments whose canonical form lies in the
# physical store; harness directories reached through a symlink are skipped. Apply unlinks
# dangling ones.
home="$(realpath -e -- "$HOME")"
physical="$(realpath -e -- "$store" 2>/dev/null)" || physical=""
for harness in "${harnesses[@]}"; do
  [ -n "$physical" ] && [ "$(realpath -m -- "$HOME/$harness")" = "$home/$harness" ] || continue
  for link in "$HOME/$harness"/*; do
    [ -L "$link" ] || continue
    target="$(readlink -- "$link")"
    case "$target/" in /*) ;; *) continue ;; esac
    case "$target/" in */./*|*/../*) continue ;; esac
    case "$(realpath -m -- "$target")" in "$physical"/*) ;; *) continue ;; esac
    case "$mode" in remove|check-remove) ;; *) [ ! -e "$link" ] || continue ;; esac
    case "$mode" in apply|remove) unlink "$link" ;; *) work=1 ;; esac
  done
done
case "$mode" in remove|check-remove) set -- ;; esac
clones=()
for url in "$@"; do
  # Directory from the repository path only: never userinfo, host, query or fragment.
  # Query and fragment go first, then the authority (with userinfo) or the SCP host.
  path="${url%%[?#]*}"
  case "$path" in
    *://*) path="${path#*://}"; case "$path" in */*) path="${path#*/}" ;; *) path="" ;; esac ;;
    *) path="${path#*:}" ;;
  esac
  path="${path%/}"
  path="${path%.git}"
  path="${path%/}"
  path="${path#/}"
  name=""
  IFS=/ read -r -a segments <<<"$path" || :
  [ "${#segments[@]}" -gt 0 ] || segments=("")
  for segment in "${segments[@]}"; do
    case "$segment" in ''|.|..|*[!A-Za-z0-9._-]*) segment=source ;; esac
    name="${name:+$name/}$segment"
  done
  clone="$store/$name"
  conflict=0
  for other in "${clones[@]}"; do
    case "$clone/" in "$other/"*) conflict=1 ;; esac
    case "$other/" in "$clone/"*) conflict=1 ;; esac
  done
  if [ "$conflict" = 1 ]; then
    # Status stays not ready until the operator resolves the overlap.
    case "$mode" in check) work=1 ;; apply) printf 'user-skills: warning: skipped %s: another source uses the same clone directory; fix USER_SKILL_SOURCES\n' "$name" >&2 ;; esac
    continue
  fi
  clones+=("$clone")
  if [ -z "$(ls -A "$clone" 2>/dev/null)" ]; then
    # Verify accepts a source that apply could not clone; check reports it for retry.
    case "$mode" in check) work=1; continue ;; verify) continue ;; esac
    mkdir -p "${clone%/*}"
    temporary="$(mktemp -d "${clone%/*}/.tmp-${clone##*/}.XXXXXX")"
    if GIT_TERMINAL_PROMPT=0 timeout -k 10 60 git clone -q -- "$url" "$temporary" </dev/null >/dev/null 2>&1 \
      || { command -v subyard-github >/dev/null && GIT_TERMINAL_PROMPT=0 timeout -k 10 60 \
        subyard-github run -- git clone -q -- "$url" "$temporary" </dev/null >/dev/null 2>&1; }; then
      mv -T "$temporary" "$clone" # replaces only an empty directory
    else
      rmdir "$temporary" 2>/dev/null || :
      printf 'user-skills: warning: could not clone %s; fix Git access or USER_SKILL_SOURCES, then retry yard profile enable user-skills or yard init\n' "$name" >&2
      continue
    fi
  fi
  for manifest in "$clone"/skills/*/SKILL.md; do
    [ -f "$manifest" ] || continue
    skill="${manifest%/SKILL.md}"
    case "${skill##*/}" in subyard-*) continue ;; esac # shipped skill names
    for harness in "${harnesses[@]}"; do
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

#!/usr/bin/env bash
# As the yard developer: clone, link and unlink user skills. Rules: docs/user-skills.md.
# Usage: guest.sh check|verify|apply [URL...]; check and verify print "work" for drift.
set -euo pipefail
mode="$1"
shift
cd "$HOME"
store="$HOME/.local/share/subyard-user-skills"
harnesses=(.agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills)
segment='[A-Za-z0-9][A-Za-z0-9._-]*'
work=0
seen=()
names=() # configured, not skipped
skipped() {
  case "$mode" in check) work=1 ;; apply) printf 'user-skills: warning: %s\n' "$1" >&2 ;; esac
}
# Pass 1: parse and clone every source, giving the configured, non-skipped names.
position=0
for url in "$@"; do
  position=$((position + 1))
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
    skipped "skipped source $position: its path has no valid owner/repo; fix USER_SKILL_SOURCES"
    continue
  fi
  duplicate=0
  for other in "${seen[@]}"; do [ "$other" != "$name" ] || duplicate=1; done
  if [ "$duplicate" = 1 ]; then
    skipped "skipped source $position: $name duplicates an earlier source; fix USER_SKILL_SOURCES"
    continue
  fi
  seen+=("$name")
  clone="$store/$name"
  if [ -z "$(ls -A "$clone" 2>/dev/null)" ]; then
    # Verify accepts a source that apply could not clone; check reports it for retry.
    case "$mode" in check) work=1; continue ;; verify) continue ;; esac
    mkdir -p "${clone%/*}"
    temporary="$(mktemp -d "${clone%/*}/.tmp-${clone##*/}.XXXXXX")"
    if GIT_TERMINAL_PROMPT=0 GIT_ALLOW_PROTOCOL=https:ssh:file timeout -k 10 60 \
      git clone -q -- "$url" "$temporary" </dev/null >/dev/null 2>&1; then
      mv -T "$temporary" "$clone" # replaces only an empty directory
    else
      rmdir "$temporary" 2>/dev/null || :
      skipped "could not clone $name; fix Git access or USER_SKILL_SOURCES, then retry yard profile enable user-skills or yard init"
      continue
    fi
  fi
  names+=("$name")
done
# Pass 2, before linking so that a freed path can be relinked in the same run.
# Ours: the link text is exactly $store/OWNER/REPO/skills/<link name>. Keep it only while its
# source is configured, not skipped, and the skill folder still has SKILL.md.
for harness in "${harnesses[@]}"; do
  for link in "$HOME/$harness"/*; do
    [ -L "$link" ] || continue
    # Keep the exact link text: command substitution would strip trailing newlines.
    target="$(readlink -- "$link"; printf x)"
    target="${target%x}"
    target="${target%$'\n'}"
    name="${target#"$store/"}"
    name="${name%"/skills/${link##*/}"}"
    [ "$target" = "$store/$name/skills/${link##*/}" ] && [[ "$name" =~ ^$segment/$segment$ ]] || continue
    keep=0
    for other in "${names[@]}"; do [ "$other" != "$name" ] || keep=1; done
    if [ "$keep" = 1 ] && [ -e "$target/SKILL.md" ]; then continue; fi
    if [ "$mode" = apply ]; then unlink -- "$link"; else work=1; fi
  done
done
# Pass 3: link each configured skill where the path is free.
for name in "${names[@]}"; do
  for manifest in "$store/$name"/skills/*/SKILL.md; do
    [ -f "$manifest" ] || continue
    skill="${manifest%/SKILL.md}"
    case "${skill##*/}" in subyard-*) continue ;; esac # shipped skill names
    for harness in "${harnesses[@]}"; do
      link="$HOME/$harness/${skill##*/}"
      if [ -e "$link" ] || [ -L "$link" ]; then continue; fi # never replace an existing entry
      if [ "$mode" = apply ]; then
        mkdir -p "$HOME/$harness"
        ln -sT -- "$skill" "$link"
      else
        work=1
      fi
    done
  done
done
if [ "$work" = 1 ]; then printf 'work\n'; fi

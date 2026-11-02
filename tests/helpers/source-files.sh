#!/usr/bin/env bash
# Current public source paths, independent of a checkout's Git metadata or history.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${1:-$ROOT}"
[ "$#" = 0 ] || shift
# Shared client skills are one public directory alias, not a second source tree.
# Worktree archives/fingerprints include the link; regular build inventories do not.
alias_exclusion=()
if [ "${1:-}" = --include-client-aliases ]; then
  shift
  if [ -L .claude/skills ]; then
    printf '.claude/skills\0'
    alias_exclusion=(-g '!.claude/skills')
  fi
fi
exec rg --no-config --files --hidden --null --no-require-git \
  --no-ignore-dot --no-ignore-global --no-ignore-parent --no-ignore-exclude -g '!.git' "${alias_exclusion[@]}" "$@"

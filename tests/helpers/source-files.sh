#!/usr/bin/env bash
# Current public source paths, independent of a checkout's Git metadata or history.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
exec rg --no-config --files --hidden --null --no-require-git \
  --no-ignore-dot --no-ignore-global --no-ignore-parent --no-ignore-exclude -g '!.git'

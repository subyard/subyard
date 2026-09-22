#!/usr/bin/env bash
# Fetch and rebase the public and private repositories onto their branch upstreams.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Check both checkouts before updating either; do not fall back to the parent repo.
for repo in "$ROOT" "$ROOT/private"; do
  if [ ! -e "$repo/.git" ]; then
    printf 'Missing Git checkout: %s\n' "$repo" >&2
    exit 1
  fi
  git -C "$repo" rev-parse --verify '@{upstream}' >/dev/null
done

for repo in "$ROOT" "$ROOT/private"; do
  printf 'Updating %s\n' "$repo"
  git -C "$repo" fetch
  git -C "$repo" rebase --autostash '@{upstream}'
done

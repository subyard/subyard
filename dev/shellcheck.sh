#!/usr/bin/env bash
set -euo pipefail

logs="$(mktemp -d)"
trap 'rm -rf -- "$logs"' EXIT

find scripts dev config/profiles config/agents tests -type f -name '*.sh' -print0 \
  | sort -z > "$logs/files"
mapfile -d '' -t shell_files < "$logs/files"
files=(bin/yard completions/yard.bash "${shell_files[@]}")

# Buffer each worker's diagnostics so they print in file order.
workers="$(nproc)"
status=0
# shellcheck disable=SC2016 # The child shell expands its own arguments.
for i in "${!files[@]}"; do printf '%s\0%s\0' "$i" "${files[$i]}"; done \
  | xargs -0 -n 2 -P "$workers" bash -c \
    'shellcheck -x -S warning "$3" > "$1/$2.log" 2>&1' shellcheck "$logs" \
  || status=$?
for i in "${!files[@]}"; do cat "$logs/$i.log"; done
exit "$status"

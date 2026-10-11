#!/usr/bin/env bash
# Build package-owned native executables, never linked into the engine.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="$ROOT/.build/profiles"
agent_output="$ROOT/.build/agents"
if [ "${1:-}" = --output-dir ] && [ "$#" -eq 2 ]; then
  output="$2"
elif [ "$#" -ne 0 ]; then
  printf 'usage: build-profiles.sh [--output-dir PATH]\n' >&2
  exit 2
fi
cd "$ROOT"
for manifest in config/profiles/*/profile.json config/agents/*/integration.json; do
  [ -f "$manifest" ] || continue
  declarations="$(jq -er '
    def safe_path: type == "string" and
      test("^[A-Za-z0-9_-]+(/[A-Za-z0-9_.-]+)*$") and
      (split("/") | all(. != "." and . != ".."));
    if .schema_version != 1 or
       ((.native // []) | type != "array" or
         any(.[]; (.package | safe_path | not) or (.path | safe_path | not)))
    then error("invalid native profile declaration")
    else (.native // [] | map([.package, .path] | @tsv) | join("\n")) end
  ' "$manifest")"
  profile="${manifest%/profile.json}"
  profile="${profile%/integration.json}"
  name="${profile##*/}"
  native_output="$output"
  if [[ "$manifest" = config/agents/* ]]; then
    native_output="$agent_output"
    if [ "${1:-}" = --output-dir ]; then native_output="$(dirname "$output")/agents"; fi
  fi
  while IFS=$'\t' read -r package path; do
    [ -n "$package" ] || continue
    destination="$native_output/$name/$path"
    mkdir -p "$(dirname "$destination")"
    tmp="$(mktemp "$(dirname "$destination")/.native.XXXXXX")"
    trap 'rm -f -- "$tmp"' EXIT
    CGO_ENABLED=0 go build -buildvcs=false -mod=readonly -trimpath -ldflags '-s -w' \
      -o "$tmp" "./$profile/$package"
    chmod 0755 "$tmp"
    mv -f -- "$tmp" "$destination"
    trap - EXIT
  done <<< "$declarations"
done

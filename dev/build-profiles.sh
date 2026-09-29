#!/usr/bin/env bash
# Build native executables declared by shipped profiles, never linked into the engine.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="$ROOT/.build/profiles"
if [ "${1:-}" = --output-dir ] && [ "$#" -eq 2 ]; then
  output="$2"
elif [ "$#" -ne 0 ]; then
  printf 'usage: build-profiles.sh [--output-dir PATH]\n' >&2
  exit 2
fi
cd "$ROOT"
for manifest in config/profiles/*/profile.json; do
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
  name="${profile##*/}"
  while IFS=$'\t' read -r package path; do
    [ -n "$package" ] || continue
    destination="$output/$name/$path"
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

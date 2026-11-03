#!/usr/bin/env bash
# Print the exact files attached to one stable GitHub Release.
set -euo pipefail

RELEASE_DIR=.build/release
VERSION=''

while [ $# -gt 0 ]; do
  case "$1" in
    --release-dir)
      [ $# -ge 2 ] || { printf 'release-assets: --release-dir needs a path\n' >&2; exit 2; }
      RELEASE_DIR="$2"
      shift 2
      ;;
    --version)
      [ $# -ge 2 ] || { printf 'release-assets: --version needs a value\n' >&2; exit 2; }
      VERSION="$2"
      shift 2
      ;;
    *) printf 'release-assets: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

case "$VERSION" in
  ''|*[!A-Za-z0-9._+-]*) printf 'release-assets: safe --version is required\n' >&2; exit 2 ;;
esac

assets=(
  subyard-install.sh
  subyard-install-runtime-release.sh
  subyard-install-runtime-release.sh.sha256
)
for arch in amd64 arm64; do
  for prefix in "yard-$VERSION-linux-$arch" "subyard-$VERSION-linux-$arch.tar.gz"; do
    assets+=("$prefix" "$prefix.sha256" "$prefix.manifest.json" "$prefix.provenance.json")
  done
done

for asset in "${assets[@]}"; do
  path="$RELEASE_DIR/$asset"
  [ -f "$path" ] && [ ! -L "$path" ] \
    || { printf 'release-assets: missing regular asset: %s\n' "$path" >&2; exit 1; }
  printf '%s\n' "$path"
done

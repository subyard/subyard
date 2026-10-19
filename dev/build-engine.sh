#!/usr/bin/env bash
# Build the native Linux control-plane engine atomically.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
OUTPUT="$REPO/.build/yard"
VERSION="${YARD_BUILD_VERSION:-0.1.0-dev}"
FORCE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --output) [ $# -ge 2 ] || { printf 'build-engine: --output needs a path\n' >&2; exit 2; }; OUTPUT="$2"; shift 2 ;;
    --force) FORCE=1; shift ;;
    *) printf 'build-engine: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

engine_stale() {
  [ "$FORCE" = 1 ] && return 0
  [ -x "$OUTPUT" ] || return 0
  [ "$("$OUTPUT" --version 2>/dev/null)" = "$(basename "$OUTPUT") $VERSION" ] || return 0
  [ "$REPO/go.mod" -nt "$OUTPUT" ] || [ "$REPO/go.sum" -nt "$OUTPUT" ] || \
    find "$REPO/cmd" "$REPO/internal" -type f -name '*.go' ! -name '*_test.go' \
      -newer "$OUTPUT" -print -quit | grep -q .
}

engine_stale || exit 0
command -v go >/dev/null 2>&1 || {
  printf 'build-engine: Go is required (go.mod selects the supported toolchain)\n' >&2
  exit 2
}

mkdir -p "$(dirname "$OUTPUT")"
lock="$(dirname "$OUTPUT")/.build.lock"
(
  flock 9
  engine_stale || exit 0
  tmp="$(mktemp "$(dirname "$OUTPUT")/.yard.tmp.XXXXXX")"
  trap 'rm -f "$tmp"' EXIT
  # Build metadata must not depend on the caller's HOME-level git safe.directory list. Tests and
  # packaged builds deliberately use isolated homes, and the explicit VERSION is authoritative.
  cd "$REPO"
  CGO_ENABLED=0 go build -buildvcs=false -mod=readonly -trimpath \
    -ldflags "-s -w -X github.com/Subyard/Subyard/internal/cli.Version=$VERSION" \
    -o "$tmp" ./cmd/yard
  chmod 0755 "$tmp"
  mv -f "$tmp" "$OUTPUT"
  trap - EXIT
) 9>"$lock"

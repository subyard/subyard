#!/usr/bin/env bash
# Use the official binary distribution; keep tool dependencies out of go.mod.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version=2.15.0
case "$(uname -sm)" in
  'Linux x86_64')
    arch=amd64
    checksum=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea
    ;;
  'Linux aarch64')
    arch=arm64
    checksum=b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998
    ;;
  *) printf 'unsupported golangci-lint platform\n' >&2; exit 1 ;;
esac
tools="$ROOT/.build/tools"
tool="$tools/golangci-lint-$version-$arch"
if [ ! -x "$tool" ]; then
  mkdir -p "$tools"
  download="$(mktemp -d "$tools/golangci-lint.XXXXXX")"
  trap 'rm -rf -- "$download"' EXIT
  archive="golangci-lint-$version-linux-$arch"
  # Retry transient GitHub 5xx outages; the archive is still checksum-verified below.
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    --retry 6 --retry-all-errors --retry-max-time 600 --connect-timeout 20 --max-time 300 \
    "https://github.com/golangci/golangci-lint/releases/download/v$version/$archive.tar.gz" \
    --output "$download/archive.tar.gz"
  printf '%s  %s\n' "$checksum" "$download/archive.tar.gz" | sha256sum --check --status
  tar -xzf "$download/archive.tar.gz" -C "$download" "$archive/golangci-lint"
  install -m 0755 "$download/$archive/golangci-lint" "$download/tool"
  mv "$download/tool" "$tool"
fi
"$tool" version --short | grep -Fx "$version" >/dev/null
cd "$ROOT"
"$tool" run --config "$ROOT/.golangci.yml" \
  ./internal/releasetransition/... ./internal/cli/... \
  ./internal/adapters/releaseruntime/... ./internal/adapters/reconcileruntime/... \
  ./internal/migration/... ./internal/testyardmigration/... ./internal/ownerpublication/...

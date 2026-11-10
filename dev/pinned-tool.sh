#!/usr/bin/env bash
# Official release binaries only; keep tool dependencies out of go.mod.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
name="${1:?usage: pinned-tool.sh actionlint|zizmor|ruff|osv-scanner}"
case "$(uname -sm)" in
  'Linux x86_64') arch=amd64; triple=x86_64-unknown-linux-gnu ;;
  'Linux aarch64') arch=arm64; triple=aarch64-unknown-linux-gnu ;;
  *) printf 'unsupported lint tool platform\n' >&2; exit 1 ;;
esac
case "$name" in
  actionlint)
    version=1.7.12
    # https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_checksums.txt
    case "$arch" in
      amd64) checksum=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8 ;;
      arm64) checksum=325e971b6ba9bfa504672e29be93c24981eeb1c07576d730e9f7c8805afff0c6 ;;
    esac
    url="https://github.com/rhysd/actionlint/releases/download/v$version/actionlint_${version}_linux_$arch.tar.gz"
    member=actionlint
    ;;
  zizmor)
    version=1.31.1
    # Official release API assets[].digest SHA-256 values (no checksum manifest):
    # https://api.github.com/repos/zizmorcore/zizmor/releases/tags/v1.31.1
    case "$arch" in
      amd64) checksum=d185f726bdaf92df78273217dfdd24ebcd6b0ef48cf3ce776ff5762b6e138c99 ;;
      arm64) checksum=37eebbd5762a44f262d94875431b14a6d2402991e90a685113ea97b8b41be833 ;;
    esac
    url="https://github.com/zizmorcore/zizmor/releases/download/v$version/zizmor-$triple.tar.gz"
    member=zizmor
    ;;
  ruff)
    version=0.17.2
    # https://github.com/astral-sh/ruff/releases/download/0.17.2/sha256.sum
    case "$arch" in
      amd64) checksum=7b2e6a6e132ecf7b5fe726163b186d5d209befe93bb25f83767b2d97fbdf14e5 ;;
      arm64) checksum=468f29d64949574f0d28442c02f0b4fb680cff7ba8fc26b6287a73b945308e1c ;;
    esac
    url="https://github.com/astral-sh/ruff/releases/download/$version/ruff-$triple.tar.gz"
    member="ruff-$triple/ruff"
    ;;
  osv-scanner)
    version=2.7.0
    # https://github.com/google/osv-scanner/releases/download/v2.7.0/osv-scanner_SHA256SUMS
    case "$arch" in
      amd64) checksum=160c6253e2922e9e0a6a9771c5c4c40a4b3c6090b34a3cdd5e80c96cc2e4218b ;;
      arm64) checksum=4061ef4c021ed3c47f203b818f337e351336cc5cd67c0274fc36bd1dfc728dd7 ;;
    esac
    url="https://github.com/google/osv-scanner/releases/download/v$version/osv-scanner_linux_$arch"
    member=archive
    ;;
  *) printf 'unknown pinned tool: %s\n' "$name" >&2; exit 1 ;;
esac
tools="$ROOT/.build/tools"
tool="$tools/$name-$version-$arch"
if [ ! -x "$tool" ]; then
  mkdir -p "$tools"
  download="$(mktemp -d "$tools/$name.XXXXXX")"
  trap 'rm -rf -- "$download"' EXIT
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    --retry 6 --retry-all-errors --retry-max-time 600 --connect-timeout 20 --max-time 300 \
    "$url" --output "$download/archive"
  printf '%s  %s\n' "$checksum" "$download/archive" | sha256sum --check --status
  if [ "$member" != archive ]; then
    tar -xzf "$download/archive" -C "$download" "$member"
  fi
  install -m 0755 "$download/$member" "$download/tool"
  mv "$download/tool" "$tool"
fi
case "$name" in
  actionlint) actual="$("$tool" -version)"; expected="$version" ;;
  zizmor) actual="$("$tool" --version)"; expected="zizmor $version" ;;
  ruff) actual="$("$tool" --version)"; expected="ruff $version" ;;
  osv-scanner) actual="$("$tool" --version)"; expected="osv-scanner version: $version" ;;
esac
[ "${actual%%$'\n'*}" = "$expected" ] || { printf 'unexpected %s version\n' "$name" >&2; exit 1; }
printf '%s\n' "$tool"

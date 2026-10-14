#!/usr/bin/env bash
# Exercise the owner transport from the actual immutable installed distribution.
set -euo pipefail
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
[ "$#" -le 1 ] || { printf 'usage: package-transport.sh [runtime-bundle]\n' >&2; exit 2; }
state="$(mktemp -d /tmp/subyard-openlit-package.XXXXXX)"
chmod 0700 "$state"
trap 'rm -rf -- "$state"' EXIT
if [ "$#" = 1 ]; then
  bundle="$(realpath "$1")"
else
  bash "$root/dev/package-engine.sh" --output-dir "$state/release" \
    --version 0.11.1-openlit.package-fixture >/dev/null
  case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) exit 2 ;; esac
  bundle="$state/release/subyard-0.11.1-openlit.package-fixture-linux-$arch.tar.gz"
fi
release_args=(--runtime-root "$state/runtime" --bundle "$bundle"
  --checksum "$bundle.sha256" --manifest "$bundle.manifest.json"
  --provenance "$bundle.provenance.json")
target="$(bash "$root/scripts/install-runtime-release.sh" "${release_args[@]}" --publish-only)"
installed="$state/runtime/$target"
bash "$root/config/profiles/openlit/tests/owner-lifecycle.sh" "$installed"
printf 'ok: installed OpenLIT lifecycle transport\n'

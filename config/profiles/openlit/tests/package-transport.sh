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
bash "$root/scripts/install-runtime-release.sh" "${release_args[@]}" --check >/dev/null
target="$(bash "$root/scripts/install-runtime-release.sh" "${release_args[@]}" --publish-only)"
repeat="$(bash "$root/scripts/install-runtime-release.sh" "${release_args[@]}" --publish-only)"
[ "$target" = "$repeat" ]
[ ! -e "$state/runtime/current" ] && [ ! -e "$state/runtime/previous" ]
installed="$state/runtime/$target"
python3 - "$bundle" <<'PY'
import sys, tarfile
with tarfile.open(sys.argv[1]) as archive:
    for asset in ("tomli_w.py", "TOMLI_W_LICENSE"):
        assert archive.getmember("./internal/adapters/configmaterial/" + asset).mode == 0o644
PY
for asset in tomli_w.py TOMLI_W_LICENSE; do
  relative="internal/adapters/configmaterial/$asset"
  cmp "$root/$relative" "$installed/$relative"
done
bash "$root/config/profiles/openlit/tests/owner-lifecycle.sh" "$installed"
# An immutable repeat must validate the shared dependency too.
writer="$installed/internal/adapters/configmaterial/tomli_w.py"
printf '\n# fixture corruption\n' >> "$writer"
if bash "$root/scripts/install-runtime-release.sh" "${release_args[@]}" --publish-only >/dev/null 2>&1; then
  printf 'FAIL: installed dependency corruption accepted\n' >&2; exit 1
fi
printf 'ok: installed OpenLIT transport, shared assets and immutable repeat validation\n'

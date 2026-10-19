#!/usr/bin/env bash
# The controller owns the disposable VM lease and source bundle.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [ "${1:-}" = --preflight ]; then
  python3 -m py_compile "$ROOT/scripts/lib/producer-run.py" "$ROOT/tests/producer-run.py" "$ROOT/dev/e2e/producer-lifecycle.py"
  shellcheck -x -S warning "$ROOT/scripts/provision-profile.sh" "$ROOT/dev/build-paseo-headless.sh" "$0"
  exit
fi
[ "${SUBYARD_E2E_VM:-}" = 1 ] || exit 2
python3 "$ROOT/tests/producer-run.py"
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
sudo -n env SUBYARD_E2E_VM=1 python3 "$ROOT/dev/e2e/producer-lifecycle.py"
artifact="$("$ROOT/dev/build-paseo-headless.sh" --arch amd64 --output-dir "$ROOT/.build/native output % $ :")"
sha256sum "$artifact"
"$ROOT/tests/native/paseo-headless.sh" --artifact "$artifact"
printf 'ok: native amd64 Paseo deploy closure\n'

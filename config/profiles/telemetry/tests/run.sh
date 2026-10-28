#!/usr/bin/env bash
set -euo pipefail
umask 0022
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE="$(cd "$ROOT/../../.." && pwd)"
cd "$SOURCE"
case "$#:${1:-}" in
  0:) bash "$ROOT/tests/permissions.sh" ;;
  1:--after-permissions) ;;
  *) printf 'Usage: tests/run.sh [--after-permissions]\n' >&2; exit 2 ;;
esac
for check in assets process native native-signals launcher lifecycle forward owner sqlite-fixture; do
  python3 -B "$ROOT/tests/$check-test.py"
done
bash "$ROOT/tests/broker-controller-test.sh"
python3 -B - "$ROOT" <<'PY'
import ast, pathlib, sys
for path in pathlib.Path(sys.argv[1]).rglob('*.py'):
    ast.parse(path.read_text(), filename=str(path))
PY
bash -n "$ROOT/profile.sh" "$ROOT/runtime.sh"
go vet ./config/profiles/telemetry/collector
go test -race -count=1 ./config/profiles/telemetry/collector ./dev/telemetry-compare/minimal

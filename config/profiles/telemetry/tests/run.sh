#!/usr/bin/env bash
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE="$(cd "$ROOT/../../.." && pwd)"
cd "$SOURCE"
for check in assets records process native native-signals launcher lifecycle forward owner sqlite-fixture resources; do
  python3 -B "$ROOT/tests/$check-test.py"
done
bash "$ROOT/tests/broker-controller-test.sh"
python3 -B - "$ROOT" <<'PY'
import ast, pathlib, sys
for path in pathlib.Path(sys.argv[1]).rglob('*.py'):
    ast.parse(path.read_text(), filename=str(path))
PY
bash -n "$ROOT/profile.sh" "$ROOT/runtime.sh"
umask 0022
go vet ./config/profiles/telemetry/collector
go test -race -count=1 ./config/profiles/telemetry/collector ./dev/telemetry-compare/minimal

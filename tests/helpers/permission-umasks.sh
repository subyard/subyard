#!/usr/bin/env bash
# Repeat short owning Shell/Python contracts in independent processes.
set -euo pipefail
export TMPDIR=/tmp
[ "$#" -gt 0 ] || { printf 'FAIL: empty Shell/Python permission selection\n' >&2; exit 1; }
for check in "$@"; do
  [ -f "$check" ] || { printf 'FAIL: selected permission check missing: %s\n' "$check" >&2; exit 1; }
  case "$check" in
    *.sh | *.py) ;;
    *) printf 'FAIL: unsupported permission check: %s\n' "$check" >&2; exit 1 ;;
  esac
done
for mask in 0022 0002 0077; do
  (
    umask "$mask"
    for check in "$@"; do
      printf 'Permission check: umask=%s check=%s\n' "$(umask)" "$check"
      case "$check" in
        *.sh) bash "$check" ;;
        *.py)
          python3 -B - "$check" <<'PYTEST'
import importlib.util
from pathlib import Path
import sys
import unittest

path = Path(sys.argv[1])
sys.path.insert(0, str(path.parent))
spec = importlib.util.spec_from_file_location('permission_check', path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
suite = unittest.defaultTestLoader.loadTestsFromModule(module)
count = suite.countTestCases()
if not count:
    raise SystemExit(f'FAIL: empty permission test module: {path}')
print(f'Permission selection: cases={count} check={path}', flush=True)
result = unittest.TextTestRunner().run(suite)
if result.skipped:
    print(f'FAIL: skipped required permission cases: {len(result.skipped)}', file=sys.stderr)
raise SystemExit(0 if result.wasSuccessful() and not result.skipped else 1)
PYTEST
          ;;
      esac
    done
  )
done

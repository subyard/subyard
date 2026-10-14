#!/usr/bin/env bash
# Focused host-free checks; physical ingestion belongs to this profile's VM lane.
set -euo pipefail
export TMPDIR=/tmp
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
bash "$root/tests/helpers/profile-go.sh" openlit
bash "$root/config/profiles/openlit/tests/e2e-controller.sh"
bash "$root/config/profiles/openlit/tests/candidate-test.sh"
bash "$root/config/profiles/openlit/tests/network-lifecycle.sh"
bash "$root/config/profiles/openlit/tests/owner-lifecycle.sh"
python3 -B "$root/config/profiles/openlit/tests/ui-origin-test.py"
python3 -B "$root/config/profiles/openlit/tests/ui-browser-test.py"
python3 -B "$root/config/profiles/openlit/tests/dashboard-test.py"
python3 -B "$root/config/profiles/openlit/tests/failure-facts-test.py"
python3 -B "$root/config/profiles/openlit/tests/pull-diagnostic-test.py"
python3 -B "$root/config/profiles/openlit/tests/network-facts-test.py"
python3 -B "$root/config/profiles/openlit/tests/profile-readiness-test.py"
bash "$root/config/profiles/openlit/tests/package-transport.sh"
python3 -B "$root/config/profiles/openlit/tests/native-test.py"
python3 -B "$root/config/profiles/openlit/tests/trace-cutover-test.py"
python3 -B "$root/config/profiles/openlit/tests/migration-process-test.py"
python3 -B "$root/config/profiles/openlit/tests/guest-test.py"
python3 -B "$root/config/profiles/openlit/tests/account-test.py"
python3 -B "$root/config/profiles/openlit/tests/runtime-test.py"
bash -n "$root/config/profiles/openlit/owner.sh" "$root/config/profiles/openlit/guest.sh" "$root/config/profiles/openlit/runtime.sh" "$root/config/profiles/openlit/tests/e2e/owner.sh"
shellcheck -S warning "$root/config/profiles/openlit/owner.sh" "$root/config/profiles/openlit/guest.sh" "$root/config/profiles/openlit/runtime.sh" "$root/config/profiles/openlit/tests/"*.sh "$root/config/profiles/openlit/tests/e2e/"*.sh
python3 - "$root/config/profiles/openlit" <<'PY'
import ast, pathlib, sys
for path in pathlib.Path(sys.argv[1]).rglob('*.py'):
    ast.parse(path.read_text(), filename=str(path))
PY
OPENLIT_DB_PASSWORD=synthetic docker compose -f "$root/config/profiles/openlit/compose.yaml" config --quiet
systemd-analyze verify "$root/config/profiles/openlit/subyard-openlit.service" "$root/config/profiles/openlit/subyard-openlit-trace-cutover.service" "$root/config/profiles/openlit/subyard-openlit-trace-cutover.timer"
bash "$root/config/profiles/openlit/tests/e2e/acceptance.sh" --help >/dev/null
if [ "$#" -gt 0 ]; then
  python3 -B "$root/config/profiles/openlit/tests/packaged-runtime.py" "$@"
fi

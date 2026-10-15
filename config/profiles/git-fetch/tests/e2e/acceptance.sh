#!/usr/bin/env bash
# Direct service and ordinary installed-release activation in one disposable VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
slot=''
preflight=0
usage() { printf 'Usage: config/profiles/git-fetch/tests/e2e/acceptance.sh --slot N | --preflight\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || { usage >&2; exit 2; }; slot="$2"; shift 2 ;;
    --preflight) preflight=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
if [ "$preflight" = 1 ]; then
  bash -n "$ROOT/config/profiles/git-fetch/tests/e2e/"*.sh
  shellcheck -S warning "$ROOT/config/profiles/git-fetch/tests/e2e/"*.sh
  bash "$ROOT/config/profiles/git-fetch/tests/e2e/installed-activation.sh" --preflight
  python3 - "$ROOT/config/profiles/git-fetch/tests/e2e/guest.py" <<'PY'
import ast,pathlib,sys
ast.parse(pathlib.Path(sys.argv[1]).read_text())
PY
  printf 'ok: git-fetch fixture syntax, ShellCheck and Python AST; native image/Incus prerequisites are checked on the allocated VM\n'
  exit 0
fi
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { usage >&2; exit 2; }
[ -n "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" ] && [[ "${SUBYARD_E2E_CANDIDATE_SHA256:-}" =~ ^[a-f0-9]{64}$ ]] \
  || { printf 'git-fetch acceptance requires a frozen candidate bundle and SHA256\n' >&2; exit 2; }
bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --vm-count 1 --purpose git-fetch-profile --vm 1 -- \
  timeout --signal=TERM --kill-after=120s 20m bash -euo pipefail -c '
    timeout --signal=TERM --kill-after=10s 3m sudo -n python3 -B config/profiles/git-fetch/tests/e2e/guest.py
    bash config/profiles/git-fetch/tests/e2e/installed-activation.sh
  '

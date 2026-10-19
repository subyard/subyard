#!/usr/bin/env bash
# Direct service and ordinary installed-release activation in one disposable VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
slot=''
preflight=0
native_loss=''
usage() { printf 'Usage: config/profiles/git-fetch/tests/e2e/acceptance.sh --slot N [--native-loss owner|helper|group] | --preflight\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || { usage >&2; exit 2; }; slot="$2"; shift 2 ;;
    --preflight) preflight=1; shift ;;
    --native-loss) [ "$#" -ge 2 ] && [ -z "$native_loss" ] || exit 2; native_loss="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
case "$native_loss" in ''|owner|helper|group) ;; *) usage >&2; exit 2 ;; esac
if [ "$preflight" = 1 ]; then
  bash -n "$ROOT/config/profiles/git-fetch/tests/e2e/"*.sh
  shellcheck -S warning "$ROOT/config/profiles/git-fetch/tests/e2e/"*.sh
  bash "$ROOT/config/profiles/git-fetch/tests/e2e/installed-activation.sh" --preflight
  python3 - "$ROOT/config/profiles/git-fetch/tests/e2e/guest.py" <<'PY'
import ast,pathlib,sys
ast.parse(pathlib.Path(sys.argv[1]).read_text())
PY
  printf 'ok: git-fetch fixture syntax, ShellCheck and Python AST; native image/Incus prerequisites are checked on the allocated VM\n'
  if [ -n "$native_loss" ]; then
    python3 -B "$ROOT/tests/helpers/profile-native-loss.py" --preflight \
      --bundle "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" --bundle-sha256 "${SUBYARD_E2E_CANDIDATE_SHA256:-}"
  fi
  exit 0
fi
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { usage >&2; exit 2; }
[ -n "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" ] && [[ "${SUBYARD_E2E_CANDIDATE_SHA256:-}" =~ ^[a-f0-9]{64}$ ]] \
  || { printf 'git-fetch acceptance requires a frozen candidate bundle and SHA256\n' >&2; exit 2; }
loss_arguments=()
work_timeout=20m
[ -z "$native_loss" ] || loss_arguments=(--native-loss "$native_loss")
if [ -n "$native_loss" ]; then
  work_timeout=25m
  python3 -B "$ROOT/tests/helpers/profile-native-loss.py" --preflight \
    --bundle "$SUBYARD_E2E_CANDIDATE_BUNDLE" --bundle-sha256 "$SUBYARD_E2E_CANDIDATE_SHA256"
fi
bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --vm-count 1 --purpose git-fetch-profile --vm 1 -- \
  timeout --signal=TERM --kill-after=120s "$work_timeout" bash -euo pipefail -c '
    timeout --signal=TERM --kill-after=10s 3m sudo -n python3 -B config/profiles/git-fetch/tests/e2e/guest.py
    bash config/profiles/git-fetch/tests/e2e/installed-activation.sh "$@"
  ' git-fetch-fixture "${loss_arguments[@]}"

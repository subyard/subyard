#!/usr/bin/env bash
# Owning broker upgrade/cold acquire and retained full-uninstall gates.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
slot='' lane=all preflight=0
usage() { printf 'usage: acceptance.sh --slot N [--lane broker-cold-upgrade|full-uninstall] [--preflight]\n' >&2; exit 2; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || usage; slot="$2"; shift 2 ;;
    --lane) [ "$#" -ge 2 ] && [ "$lane" = all ] || usage; lane="$2"; shift 2 ;;
    --preflight) preflight=1; shift ;;
    *) usage ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || usage
case "$lane" in all|broker-cold-upgrade|full-uninstall) ;; *) usage ;; esac
printf 'test-vms physical selection: recipe/archive producer, installer/libraries, broker activation or release migration require supported 0.19 upgrade and cold acquire; full-uninstall remains required\n'
bash "$ROOT/config/profiles/test-vms/tests/run.sh" --preflight
[ "$preflight" = 0 ] || exit 0
if [ "$lane" = all ] || [ "$lane" = broker-cold-upgrade ]; then
  [[ "${SUBYARD_E2E_CANDIDATE_SHA256:-}" =~ ^[a-f0-9]{64}$ ]] \
    && [ -f "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" ] && [ ! -L "$SUBYARD_E2E_CANDIDATE_BUNDLE" ] \
    || { printf 'test-vms cold upgrade requires frozen candidate bundle and SHA256\n' >&2; exit 2; }
  [ "$(sha256sum "$SUBYARD_E2E_CANDIDATE_BUNDLE" | cut -d' ' -f1)" = "$SUBYARD_E2E_CANDIDATE_SHA256" ] \
    || { printf 'test-vms frozen candidate checksum mismatch\n' >&2; exit 2; }
  bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --type android-test --purpose broker-cold-upgrade --vm 1 -- \
    timeout --signal=TERM --kill-after=120 7200 bash dev/e2e/staging-reliability.sh --supported-upgrade
fi
if [ "$lane" = all ] || [ "$lane" = full-uninstall ]; then
  exec bash "$ROOT/dev/e2e/p0-acceptance.sh" --slot "$slot" --lane full-uninstall
fi

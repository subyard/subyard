#!/usr/bin/env bash
# Test VM recipe, publication and cleanup contracts.
set -euo pipefail
umask 0022
PROFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$PROFILE/../../.." && pwd)"
export PYTHONDONTWRITEBYTECODE=1 TMPDIR=/tmp
preflight=0
case "$#:${1:-}" in 0:) ;; 1:--preflight) preflight=1 ;; *) printf 'Usage: tests/run.sh [--preflight]\n' >&2; exit 2 ;; esac
if [ "$preflight" = 1 ] && [ -n "${SUBYARD_E2E_LOCAL_EVIDENCE:-}" ]; then
  python3 - "$ROOT" "$SUBYARD_E2E_LOCAL_EVIDENCE" <<'PYREUSE'
import importlib.util,pathlib,sys
root=pathlib.Path(sys.argv[1])
spec=importlib.util.spec_from_file_location('acceptance',root/'dev/release-acceptance.py')
module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
source,receipt=module.load_bound(pathlib.Path(sys.argv[2]))
check=receipt['checks']['verify']
if (source.resolve()!=root.resolve() or check['status']!='passed'
    or check.get('command')!=module.command_identity(['make','verify'],source)
    or check.get('execution_environment')!=module.execution_environment()):
    raise SystemExit('FAIL: test-vms prerequisite evidence is not applicable')
print('test-vms preflight: reuse validated make verify PASS; installer then bundled recipe prerequisites already passed')
PYREUSE
  exit 0
fi
log="$(mktemp /tmp/subyard-test-vms-checks.XXXXXX)"
trap 'rm -f -- "$log"' EXIT
printf 'test-vms prerequisite: installer package-only identity regression before bundled base recipe\n'
if bash "$ROOT/tests/install-incus-data-home.sh" >"$log" 2>&1; then
  tail -n 1 "$log"
else
  rc=$?; cat "$log"; exit "$rc"
fi
selection=()
[ "$preflight" = 0 ] || selection=(-run '^(TestBaseRecipeInstallerContract|TestServicePoolFenceIsAtomicWithAdmissionAndDoesNotRevokeBeforeStop|TestServiceProfileAndNativePoolBindingAgree|TestServiceContextRereadsShippedPresetAndPreservesCustomPort|TestServiceRemovedRuntimeIsAbsentForReleaseAdmission)$')
if go -C "$ROOT" test -json -count=1 ./config/profiles/test-vms "${selection[@]}" >"$log"; then :; else
  rc=$?; cat "$log"; exit "$rc"
fi
python3 - "$log" <<'PYCOUNT'
import json,sys
required={'TestBaseRecipeInstallerContract','TestBaseRecipeInstallerContract/subyard-pair','TestBaseRecipeInstallerContract/android-test','TestServicePoolFenceIsAtomicWithAdmissionAndDoesNotRevokeBeforeStop','TestServiceProfileAndNativePoolBindingAgree','TestServiceContextRereadsShippedPresetAndPreservesCustomPort','TestServiceRemovedRuntimeIsAbsentForReleaseAdmission'}
passed=set(); skipped=[]
for line in open(sys.argv[1]):
    event=json.loads(line)
    if event.get('Test') and event.get('Action')=='pass': passed.add(event['Test'])
    if event.get('Test') and event.get('Action')=='skip': skipped.append(event['Test'])
if not passed or required-passed or skipped:
    raise SystemExit('FAIL: missing or skipped test-vms recipe cases: '+','.join(sorted(required-passed)+skipped))
print(f'test-vms Go tests: passed={len(passed)} required_recipe_cases={len(required)} skips=0')
PYCOUNT
python3 -B "$PROFILE/tests/test_service_route.py"
python3 -B "$PROFILE/tests/test_full_cleanup.py"
python3 -B "$ROOT/dev/e2e/staging-reliability-test.py"
python3 -B "$ROOT/dev/e2e/pregrant-creation-observer-test.py"

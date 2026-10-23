#!/usr/bin/env bash
# Broker and integration checks owned by the GitHub profile.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
preflight=0
case "$#:${1:-}" in 0:) ;; 1:--preflight) preflight=1 ;; *) printf 'Usage: tests/run.sh [--preflight]\n' >&2; exit 2 ;; esac
if [ "$preflight" = 1 ] && [ -n "${SUBYARD_E2E_LOCAL_EVIDENCE:-}" ]; then
  # Reuse only the existing collector's fully validated, command-bound local gate.
  python3 - "$ROOT" "$SUBYARD_E2E_LOCAL_EVIDENCE" <<'PY'
import importlib.util,pathlib,sys
root=pathlib.Path(sys.argv[1])
spec=importlib.util.spec_from_file_location('acceptance',root/'dev/release-acceptance.py')
module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
source,receipt=module.load_bound(pathlib.Path(sys.argv[2]))
check=receipt['checks']['verify']
if (source.resolve()!=root.resolve() or check['status']!='passed'
    or check.get('command')!=module.command_identity(['make','verify'],source)
    or check.get('execution_environment')!=module.execution_environment()):
    raise SystemExit('FAIL: local prerequisite evidence is not applicable')
print('GitHub local preflight: reuse validated make verify PASS; protocol, ownership and fixture checks already executed')
PY
  exit 0
fi
log="$(mktemp /tmp/subyard-github-checks.XXXXXX)"
trap 'rm -f -- "$log"' EXIT
required='TestConnectionFixtureIssuerLocal|TestClientHandlerAndFakeGitHubContractOverUnixSocket|TestRuntimeStatusReportsLoadedExecutableWithoutCredentials|TestClientRuntimeStatusRejectsPredecessorAndIncompatibleIdentity'
if [ "$preflight" = 1 ]; then
  selection=(-run "^($required)$" ./config/profiles/github/broker)
else
  selection=(./config/profiles/github/...)
fi
printf 'GitHub profile race tests: required protocol/identity cases; umask=0022 preflight=%s\n' "$preflight"
if (umask 0022; go -C "$ROOT" test -json -race -count=1 "${selection[@]}") >"$log"; then :; else
  rc=$?; cat "$log"; exit "$rc"
fi
python3 - "$log" "$required" <<'PY'
import json,sys
required=set(sys.argv[2].split('|'))
passed=set()
count=0
for line in open(sys.argv[1]):
    event=json.loads(line)
    name=event.get('Test')
    if event.get('Action')=='pass' and name:
        count+=1
        passed.add(name)
    if name in required and event.get('Action') in ('skip','fail'):
        raise SystemExit('FAIL: required GitHub protocol case '+name+' '+event['Action'])
if required-passed or not count:
    raise SystemExit('FAIL: required GitHub protocol cases absent: '+','.join(sorted(required-passed)))
print(f'GitHub Go tests: passed={count} required_protocol_cases={len(required)} skips=0(required)')
PY
python3 -B "$ROOT/config/profiles/github/tests/skills-test.py"
python3 "$ROOT/config/profiles/github/tests/release-activation-test.py"
python3 -B "$ROOT/config/profiles/github/tests/e2e/runtime-hook-trace-test.py"
go -C "$ROOT" test ./config/profiles/github/tests/e2e -run '^TestRuntimeHookTraceNativeLauncher$' -count=1
if [ "$preflight" = 1 ]; then
  bash "$ROOT/tests/installed-runtime-refresh.sh"
  if bash "$ROOT/tests/install-incus-data-home.sh" >"$log" 2>&1; then
    tail -n 1 "$log"
  else
    rc=$?; cat "$log"; exit "$rc"
  fi
  exit 0
fi
mapfile -t unformatted < <(gofmt -l "$ROOT/config/profiles/github")
[ "${#unformatted[@]}" -eq 0 ] \
  || { printf 'FAIL: gofmt required: %s\n' "${unformatted[*]}" >&2; exit 1; }
go -C "$ROOT" vet ./config/profiles/github/...
bash "$ROOT/tests/helpers/go-permission-umasks.sh" "$ROOT" \
  './config/profiles/github/broker:TestLoadConfigAndKeyRejectUnsafeFiles|TestProtectedFilesRejectSymlinksAndOversize'
bash "$ROOT/config/profiles/github/tests/cleanup.sh"
bash "$ROOT/config/profiles/github/tests/teardown-identity.sh"
bash "$ROOT/config/profiles/github/tests/composition.sh"
python3 -B "$ROOT/config/profiles/github/tests/git-wiring-test.py"
python3 -B "$ROOT/config/profiles/github/tests/git-remote-test.py"
python3 "$ROOT/config/profiles/github/tests/runtime-test.py"
python3 -B "$ROOT/config/profiles/github/tests/release-observation-test.py"
python3 -B "$ROOT/config/profiles/github/tests/native-loss-test.py"
bash "$ROOT/config/profiles/github/tests/runner-permissions.sh"

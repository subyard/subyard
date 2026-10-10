#!/usr/bin/env bash
# Exercise the real runner against a tiny checkout, without recursively running this suite.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fixture="$tmp/source with spaces"
mkdir -p "$fixture"/{bin,cmd,internal,scripts,dev,config/profiles,config/agents,tests/suites} "$tmp/tools"
cp "$ROOT/tests/run.sh" "$fixture/tests/run.sh"
printf '#!/usr/bin/env bash\n' > "$fixture/bin/yard"
printf '#!/usr/bin/env bash\nexit 0\n' > "$tmp/tools/go"
cp "$tmp/tools/go" "$tmp/tools/gofmt"
cat > "$tmp/tools/go" <<'SH'
#!/usr/bin/env bash
set -eu
[ "${TMPDIR:-}" = /tmp ] || exit 25
case " $* " in
  *' test -race '*)
    printf '%s %s\n' "$(umask)" "$*" >> "$RUNNER_GO_LOG"
    if [ "$(umask)" = "${RUNNER_FAIL_UMASK:-}" ]; then
      printf '%s\n' '--- FAIL: TestEarlyFailure' '    fixture_test.go:1: expected failure detail'
      for ((i=0; i<50; i++)); do printf 'ok trailing/package/%s\n' "$i"; done
      exit 24
    fi
    if [ "$(umask)" = "${RUNNER_BLOCK_UMASK:-}" ] && [ -n "${RUNNER_SIGNAL_MARKER:-}" ]; then
      : > "$RUNNER_SIGNAL_MARKER"
      while :; do sleep 1; done
    fi
    ;;
esac
SH
chmod +x "$tmp/tools/"*
cat > "$fixture/dev/build-engine.sh" <<'SH'
#!/usr/bin/env bash
set -eu
root="$(cd "$(dirname "$0")/.." && pwd)"
printf '#!/usr/bin/env bash\n' > "$root/.build/yard"
chmod +x "$root/.build/yard"
SH
chmod +x "$fixture/dev/build-engine.sh"
for suite in unit contract integration; do
  printf '%s.sh\n' "$suite" > "$fixture/tests/suites/$suite.list"
  printf '#!/usr/bin/env bash\nprintf "hidden successful output\\n"\n' > "$fixture/tests/$suite.sh"
done
# A child that reads stdin must not consume the remaining manifest entries.
printf 'contract.sh\nafter-input.sh\n' > "$fixture/tests/suites/contract.list"
printf '#!/usr/bin/env bash\ncat >/dev/null\nprintf "hidden successful output\\n"\n' \
  > "$fixture/tests/contract.sh"
printf '#!/usr/bin/env bash\nprintf "following test ran\\n"\n' > "$fixture/tests/after-input.sh"

run_fixture() {
  env PATH="$tmp/tools:$PATH" RUNNER_GO_LOG="$tmp/$1.go" TMPDIR="$fixture/.build" \
    bash "$fixture/tests/run.sh" > "$tmp/$1.out" 2>&1
}
summary_path() { sed -n 's/^RESULTS //p' "$tmp/$1.out"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

run_fixture success
awk '$1 != "0002" && $1 != "0022" && $1 != "0077" { exit 1 }
  !/-count=1/ { exit 1 }
  { masks[$1]++; count++ }
  END { if (count != 3 || masks["0002"] != 1 || masks["0022"] != 1 || masks["0077"] != 1) exit 1 }
' "$tmp/success.go" || fail 'Go tests did not run uncached under each umask'
summary="$(summary_path success)"
awk -F '\t' '
  NR == 1 { if ($0 != "kind\tsuite\tcheck\tstatus\texit_code\tduration_seconds\tlog") exit 1; next }
  $4 != "passed" || $5 != 0 || $6 !~ /^[0-9]+$/ { exit 1 }
  $1 == "check" && $3 ~ /^tests\// { suites[$2]++ }
  $1 == "run" { completed++ }
  END { if (suites["unit"] != 1 || suites["contract"] != 2 || suites["integration"] != 1 || completed != 1) exit 1 }
' "$summary" || fail 'successful result is incomplete or malformed'
! grep -q 'hidden successful output' "$tmp/success.out" || fail 'successful output leaked to console'
while IFS=$'\t' read -r kind _ _ _ _ _ log; do
  [ "$kind" != check ] || [ -f "$(dirname "$summary")/$log" ] || fail 'missing check log'
done < "$summary"
unit_log="$(awk -F '\t' '$3 == "tests/unit.sh" { print $7 }' "$summary")"
grep -q 'hidden successful output' "$(dirname "$summary")/$unit_log" || fail 'successful log was lost'
run_fixture second
[ "$(summary_path second)" != "$summary" ] && [ -f "$summary" ] || fail 'second run overwrote first run'

# Child commands must not consume the manifest supplying the runner's loop.
printf '#!/usr/bin/env bash\ncat >/dev/null\n' > "$fixture/tests/unit.sh"
printf 'unit.sh\nafter-stdin.sh\n' > "$fixture/tests/suites/unit.list"
printf '#!/usr/bin/env bash\nexit 0\n' > "$fixture/tests/after-stdin.sh"
run_fixture stdin-consuming
summary="$(summary_path stdin-consuming)"
awk -F '\t' '$1 == "check" && $3 == "tests/after-stdin.sh" && $4 == "passed" { found=1 }
  END { exit !found }' "$summary" || fail 'child stdin consumption skipped the next declared check'
printf 'unit.sh\n' > "$fixture/tests/suites/unit.list"
rm "$fixture/tests/after-stdin.sh"

rc=0
RUNNER_FAIL_UMASK=0022 run_fixture umask-failure || rc=$?
[ "$rc" -eq 24 ] || fail 'umask matrix hid a failed Go run'
grep -Fq -- '--- FAIL: TestEarlyFailure' "$tmp/umask-failure.out" \
  && grep -Fq 'fixture_test.go:1: expected failure detail' "$tmp/umask-failure.out" \
  || fail 'early Go failure was hidden by trailing package output'
! grep -q '^0077 ' "$tmp/umask-failure.go" || fail 'umask matrix continued after failure'
! grep -q 'RUN build' "$tmp/umask-failure.out" || fail 'runner built after a failed Go run'

# Preserve the child's exact failure and stop before the next test/suite.
printf '#!/usr/bin/env bash\nprintf "expected failure detail\\n" >&2\nexit 23\n' > "$fixture/tests/unit.sh"
rc=0
run_fixture failure || rc=$?
[ "$rc" -eq 23 ] || fail "expected exit 23, got $rc"
summary="$(summary_path failure)"
awk -F '\t' '$1 == "check" && $3 == "tests/unit.sh" && $4 == "failed" && $5 == 23 { found=1 }
  END { exit !found }' "$summary" || fail 'failed test missing from summary'
tail -n 1 "$summary" | grep -q $'^run\tall\tall\tfailed\t23\t' || fail 'failed run reported incorrectly'
grep -q 'expected failure detail' "$tmp/failure.out" || fail 'failure detail was suppressed'
! grep -q 'RUN tests/contract.sh' "$tmp/failure.out" || fail 'runner continued after failure'

# Signals during a foreground check must finalize the active check and run as failed.
python3 - "$fixture/tests/run.sh" "$tmp" <<'PY'
import csv
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

runner = Path(sys.argv[1])
root = Path(sys.argv[2])
def interrupted_case(name, sig, expected_status):
    case = root / name
    case.mkdir()
    marker = case / 'blocked'
    output_path = case / 'runner.out'
    env = os.environ.copy()
    env.update({
        'PATH': str(root / 'tools') + os.pathsep + env.get('PATH', ''),
        'RUNNER_GO_LOG': str(case / 'go.log'),
        'RUNNER_BLOCK_UMASK': '0077',
        'RUNNER_SIGNAL_MARKER': str(marker),
        'TMPDIR': str(root / 'source with spaces' / '.build'),
    })
    with output_path.open('w') as output:
        process = subprocess.Popen(
            ['bash', str(runner)], stdout=output, stderr=subprocess.STDOUT,
            env=env, start_new_session=True,
        )
        try:
            deadline = time.monotonic() + 10
            while not marker.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.05)
            if not marker.exists():
                raise AssertionError(f'{name}: runner did not reach blocked umask 0077 check')
            os.killpg(process.pid, sig)
            status = process.wait(timeout=10)
        except BaseException:
            if process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=5)
            raise

    output = output_path.read_text()
    summaries = [line.removeprefix('RESULTS ') for line in output.splitlines()
                 if line.startswith('RESULTS ')]
    if len(summaries) != 1:
        raise AssertionError(f'{name}: missing unique summary path')
    with Path(summaries[0]).open(newline='') as summary_file:
        rows = list(csv.DictReader(summary_file, delimiter='\t'))
    checks = [row for row in rows if row['kind'] == 'check']
    runs = [row for row in rows if row['kind'] == 'run']
    if status != expected_status:
        raise AssertionError(f'{name}: native status {status}, expected {expected_status}')
    summary_code = str(expected_status)
    if not checks or (checks[-1]['suite'], checks[-1]['check'], checks[-1]['status'], checks[-1]['exit_code']) != (
            'go', 'go-race', 'failed', summary_code):
        raise AssertionError(f'{name}: interrupted check summary is wrong: {checks!r}')
    if len(runs) != 1 or (runs[0]['status'], runs[0]['exit_code']) != ('failed', summary_code):
        raise AssertionError(f'{name}: run summary is wrong: {runs!r}')
    if any(line.startswith(('RUN go-fuzz', 'RUN build', 'RUN tests/')) for line in output.splitlines()):
        raise AssertionError(f'{name}: runner continued after the interrupted check')


for name, sig, expected in (('signal-int', signal.SIGINT, 130), ('signal-term', signal.SIGTERM, 143)):
    interrupted_case(name, sig, expected)
print('ok: SIGINT/SIGTERM fail the active check and stop the runner')
PY

# A failure inside a composite function must still stop it, even if its last command would pass.
printf 'if\n' > "$fixture/scripts/00-broken.sh"
rc=0
run_fixture syntax || rc=$?
[ "$rc" -ne 0 ] || fail 'syntax failure was ignored'
summary="$(summary_path syntax)"
awk -F '\t' '$1 == "check" && $3 == "syntax" && $4 == "failed" { found=1 }
  END { exit !found }' "$summary" || fail 'syntax failure missing from summary'
! grep -q 'RUN go-toolchain' "$tmp/syntax.out" || fail 'runner continued after syntax failure'

# Profiles own their test entrypoints; discovery does not require core registration.
cp "$ROOT/dev/test-profiles.sh" "$fixture/dev/test-profiles.sh"
mkdir -p "$fixture/config/profiles/example/tests"
printf '#!/usr/bin/env bash\n[ "$TMPDIR" = /tmp ] || exit 25\nprintf "profile check ran\\n"\n' \
  > "$fixture/config/profiles/example/tests/run.sh"
TMPDIR="$fixture/.build" bash "$fixture/dev/test-profiles.sh" > "$tmp/profiles.out" 2>&1
grep -q 'profile check ran' "$tmp/profiles.out" || fail 'profile tests were not discovered'
cp "$ROOT/Makefile" "$fixture/Makefile"
TMPDIR="$fixture/.build" PATH="$tmp/tools:$PATH" \
  make -s -C "$fixture" test > "$tmp/make-test.out" 2>&1
grep -q 'profile check ran' "$tmp/make-test.out" || fail 'make test skipped profile tests'

# The Go helper is also a supported direct entrypoint, without the profile wrapper.
mkdir -p "$fixture/tests/helpers"
cp "$ROOT/tests/helpers/profile-go.sh" "$fixture/tests/helpers/profile-go.sh"
TMPDIR="$fixture/.build" PATH="$tmp/tools:$PATH" RUNNER_GO_LOG="$tmp/profile-go.log" \
  bash "$fixture/tests/helpers/profile-go.sh" example > "$tmp/profile-go.out" 2>&1
[ "$(wc -l < "$tmp/profile-go.log")" -eq 3 ] \
  || fail 'standalone profile Go checks did not complete with isolated fixtures'

printf '#!/usr/bin/env bash\nexit 29\n' > "$fixture/config/profiles/example/tests/run.sh"
rc=0
bash "$fixture/dev/test-profiles.sh" > "$tmp/profiles-failure.out" 2>&1 || rc=$?
[ "$rc" -eq 29 ] || fail 'profile runner hid the original failure'
mkdir -p "$fixture/config/profiles/example/tests/e2e"
cat > "$fixture/config/profiles/example/tests/e2e/acceptance.sh" <<'SH'
#!/usr/bin/env bash
[ -z "${EXPECTED_TMPDIR:-}" ] || [ "$TMPDIR" = "$EXPECTED_TMPDIR" ] || exit 26
printf 'profile e2e %s\n' "$*"
SH
TMPDIR="$fixture/.build" EXPECTED_TMPDIR="$fixture/.build" \
  bash "$fixture/dev/test-profiles.sh" --e2e --slot 7 > "$tmp/profiles-e2e.out" 2>&1
grep -Fxq 'profile e2e --slot 7' "$tmp/profiles-e2e.out" \
  || fail 'profile E2E selection or arguments were lost'
bash "$fixture/dev/test-profiles.sh" --e2e --list > "$tmp/profiles-list.out"
grep -Fxq $'runner\texample\tconfig/profiles/example/tests/e2e/acceptance.sh' \
  "$tmp/profiles-list.out" || fail 'profile runner listing is incomplete'
! grep -q 'profile e2e' "$tmp/profiles-list.out" \
  || fail 'profile listing executed an acceptance runner'

# Discovery must never turn an omitted profile obligation into a passing gate.
mkdir -p "$fixture/config/profiles/undeclared/tests/e2e"
rc=0
bash "$fixture/dev/test-profiles.sh" --e2e > "$tmp/profiles-missing.out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || fail 'missing profile acceptance runner was silently skipped'
grep -q 'undeclared incomplete' "$tmp/profiles-missing.out" || fail 'missing profile was not identified'
rc=0
bash "$fixture/dev/test-profiles.sh" --e2e --list > "$tmp/profiles-list-missing.out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || fail 'profile listing accepted a missing required runner'
grep -q 'undeclared incomplete' "$tmp/profiles-list-missing.out" \
  || fail 'profile listing did not identify the missing runner'
! grep -q '^profile e2e' "$tmp/profiles-missing.out" \
  || fail 'a profile ran before the complete acceptance inventory was validated'
printf ' \n\t\n' > "$fixture/config/profiles/undeclared/tests/e2e/acceptance.not-applicable"
rc=0
bash "$fixture/dev/test-profiles.sh" --e2e > "$tmp/profiles-empty-reason.out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || fail 'not-applicable without a reason was accepted'
printf 'No runtime assets; reserved fixture directory.\n' \
  > "$fixture/config/profiles/undeclared/tests/e2e/acceptance.not-applicable"
bash "$fixture/dev/test-profiles.sh" --e2e > "$tmp/profiles-exempt.out" 2>&1
grep -q 'undeclared not-applicable: No runtime assets' "$tmp/profiles-exempt.out" \
  || fail 'explicit not-applicable reason was lost'
printf '#!/usr/bin/env bash\nexit 0\n' > "$fixture/config/profiles/undeclared/tests/e2e/acceptance.sh"
rc=0
bash "$fixture/dev/test-profiles.sh" --e2e > "$tmp/profiles-conflict.out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || fail 'conflicting runner and exemption were accepted'

empty="$tmp/empty"
mkdir -p "$empty/dev" "$empty/config/profiles"
cp "$ROOT/dev/test-profiles.sh" "$empty/dev/test-profiles.sh"
rc=0
bash "$empty/dev/test-profiles.sh" > "$tmp/profiles-empty.out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || fail 'empty profile discovery passed'
printf 'ok: runners preserve results, logs, fail-fast and failure exit codes\n'

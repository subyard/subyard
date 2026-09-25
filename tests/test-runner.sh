#!/usr/bin/env bash
# Exercise the real runner against a tiny checkout, without recursively running this suite.
set -euo pipefail
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
case " $* " in
  *' test -race '*)
    printf '%s %s\n' "$(umask)" "$*" >> "$RUNNER_GO_LOG"
    [ "$(umask)" != "${RUNNER_FAIL_UMASK:-}" ] || exit 24
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

run_fixture() {
  env PATH="$tmp/tools:$PATH" RUNNER_GO_LOG="$tmp/$1.go" \
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
  END { if (suites["unit"] != 1 || suites["contract"] != 1 || suites["integration"] != 1 || completed != 1) exit 1 }
' "$summary" || fail 'successful result is incomplete or malformed'
! grep -q 'hidden successful output' "$tmp/success.out" || fail 'successful output leaked to console'
while IFS=$'\t' read -r kind _ _ _ _ _ log; do
  [ "$kind" != check ] || [ -f "$(dirname "$summary")/$log" ] || fail 'missing check log'
done < "$summary"
unit_log="$(awk -F '\t' '$3 == "tests/unit.sh" { print $7 }' "$summary")"
grep -q 'hidden successful output' "$(dirname "$summary")/$unit_log" || fail 'successful log was lost'
run_fixture second
[ "$(summary_path second)" != "$summary" ] && [ -f "$summary" ] || fail 'second run overwrote first run'

rc=0
RUNNER_FAIL_UMASK=0022 run_fixture umask-failure || rc=$?
[ "$rc" -eq 24 ] || fail 'umask matrix hid a failed Go run'
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

# A failure inside a composite function must still stop it, even if its last command would pass.
printf 'if\n' > "$fixture/scripts/00-broken.sh"
rc=0
run_fixture syntax || rc=$?
[ "$rc" -ne 0 ] || fail 'syntax failure was ignored'
summary="$(summary_path syntax)"
awk -F '\t' '$1 == "check" && $3 == "syntax" && $4 == "failed" { found=1 }
  END { exit !found }' "$summary" || fail 'syntax failure missing from summary'
! grep -q 'RUN go-toolchain' "$tmp/syntax.out" || fail 'runner continued after syntax failure'
printf 'ok: runner preserves results, logs, fail-fast and failure exit codes\n'

#!/usr/bin/env bash
# Keep the real runner's full/focused permission selection under its profile owner.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
tmp="$(mktemp -d /tmp/subyard-github-runner.XXXXXX)"
cleanup() {
  local rc=$?
  trap - EXIT
  if [ "$rc" -eq 0 ]; then
    rm -rf -- "$tmp"
  else
    printf 'github-runner: failed fixture retained at %s\n' "$tmp" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
fixture="$tmp/source with spaces"
mkdir -p "$fixture/config/profiles/github/tests/e2e" "$fixture/tests/helpers" "$tmp/tools"
cp "$ROOT/config/profiles/github/tests/run.sh" "$fixture/config/profiles/github/tests/run.sh"
cp "$ROOT/tests/helpers/go-permission-umasks.sh" "$fixture/tests/helpers/go-permission-umasks.sh"
printf '# Isolated permission fixture.\n' > "$fixture/tests/profile-native-loss.sh"
# Only the permission runner is under test here; its other owning checks run for real
# through the outer entrypoint. Include every shell/Python child, including this one.
for check in "$ROOT/config/profiles/github/tests/"*.sh "$ROOT/config/profiles/github/tests/"*.py; do
  [ "${check##*/}" = run.sh ] || printf '# Isolated permission fixture.\n' > "$fixture/config/profiles/github/tests/${check##*/}"
done
printf '# Isolated permission fixture.\n' > "$fixture/config/profiles/github/tests/e2e/runtime-hook-trace-test.py"
cat > "$tmp/tools/go" <<'SH'
#!/usr/bin/env bash
set -eu
[ "${TMPDIR:-}" = /tmp ] || exit 25
case " $* " in
  *' test -list '*)
    printf '%s\n' TestLoadConfigAndKeyRejectUnsafeFiles TestProtectedFilesRejectSymlinksAndOversize ;;
  *' test -race '*|*' test -json -race '*)
    printf '%s %s\n' "$(umask)" "$*" >> "$RUNNER_GO_LOG"
    if [[ " $* " = *' -json '* ]]; then
      for name in TestConnectionFixtureIssuerLocal TestClientHandlerAndFakeGitHubContractOverUnixSocket \
        TestRuntimeStatusReportsLoadedExecutableWithoutCredentials TestClientRuntimeStatusRejectsPredecessorAndIncompatibleIdentity; do
        printf '{"Action":"pass","Test":"%s"}\n' "$name"
      done
    fi ;;
esac
SH
printf '#!/usr/bin/env bash\nexit 0\n' > "$tmp/tools/gofmt"
chmod 0755 "$tmp/tools/go" "$tmp/tools/gofmt"
(umask 0077; TMPDIR="$fixture/.build" PATH="$tmp/tools:$PATH" RUNNER_GO_LOG="$tmp/github-go.log" \
  bash "$fixture/config/profiles/github/tests/run.sh") > "$tmp/github-go.out" 2>&1
# GitHub repeats protected broker inputs, not its client/issuer/SSH suites.
awk '!/-count=1/ { exit 1 }
  $1 == "0022" { if (/-run / || !/\.\/config\/profiles\/github\/\.\.\./) exit 1; full++ }
  $1 == "0002" || $1 == "0077" {
    if (!/-run / || /\.\.\./ || !/TestLoadConfigAndKeyRejectUnsafeFiles/ || !/TestProtectedFilesRejectSymlinksAndOversize/) exit 1
    focused[$1]++
  }
  END { if (NR != 3 || full != 1 || focused["0002"] != 1 || focused["0077"] != 1) exit 1 }
' "$tmp/github-go.log" || fail 'GitHub full/focused permission coverage drifted'
printf 'ok: GitHub runner preserves full/focused uncached permission coverage\n'

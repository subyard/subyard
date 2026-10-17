#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Interrupted init may leave a running instance before the developer account exists.
# Broker cleanup must not prevent teardown of that instance.
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$TMP/partial-init"
  export SUBYARD_PROFILE_SELECTED=0 SUBYARD_YARD=default
  export INCUS_LOG="$TMP/partial-init/incus.log"
  SUBYARD_TEARDOWN_ARTIFACTS="$(python3 - "$SUBYARD_HOME" "$SUBYARD_OPERATOR_HOME" <<'PY'
import json, sys
data, operator = sys.argv[1:]
print(json.dumps([{'Path': path, 'Binding': ''} for path in (
    data + '/github-broker/default-engine', data + '/github-broker/default.json',
    operator + '/.config/systemd/user/subyard-github-default.service')]))
PY
)"
  export SUBYARD_TEARDOWN_ARTIFACTS
  mkdir -p "$TMP/bin"
  cat > "$TMP/bin/incus" <<'INCUS'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$INCUS_LOG"
case "$*" in
  'exec yard --project subyard -- true') exit 0 ;;
  'exec yard --project subyard -- getent passwd dev') exit 2 ;;
  'exec yard --project subyard -- rm -f -- /usr/local/libexec/subyard/github-client') exit 0 ;;
  *) exit 99 ;;
esac
INCUS
  chmod +x "$TMP/bin/incus"
  export PATH="$TMP/bin:$PATH"
  bash "$ROOT/config/profiles/github/owner.sh" --check
  bash "$ROOT/config/profiles/github/owner.sh" --remove
  ! grep -Fq 'bash -euo pipefail' "$INCUS_LOG" \
    || fail 'broker cleanup invoked the profile hook without a developer account'
  grep -Fq 'rm -f -- /usr/local/libexec/subyard/github-client' "$INCUS_LOG" \
    || fail 'broker cleanup skipped the guest engine in a partial init'
)

# Ownership and inactive-state decisions stay with the profile's real hook.
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$TMP/pause"
  export SUBYARD_YARD=fixture
  export SERVICE_STATE="$TMP/pause/state" SERVICE_LOG="$TMP/pause/service.log"
  cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --user ] || exit 2
case "$2" in
  show) cat "$SERVICE_STATE" ;;
  stop|start) printf '%s\n' "$2" >> "$SERVICE_LOG" ;;
  *) exit 2 ;;
esac
SH
  chmod +x "$TMP/bin/systemctl"
  export PATH="$TMP/bin:$PATH"
  unit="$SUBYARD_OPERATOR_HOME/.config/systemd/user/subyard-github-fixture.service"
  mkdir -p "$(dirname "$unit")"
  printf 'unmanaged\n' > "$unit"
  printf 'active\n' > "$SERVICE_STATE"
  : > "$SERVICE_LOG"
  bash "$ROOT/config/profiles/github/owner.sh" --pause
  bash "$ROOT/config/profiles/github/owner.sh" --resume
  [ ! -s "$SERVICE_LOG" ] || fail 'unmanaged service was mutated'
  printf '# Managed by Subyard GitHub broker\n' > "$unit"
  printf 'inactive\n' > "$SERVICE_STATE"
  [ -z "$(bash "$ROOT/config/profiles/github/owner.sh" --pause)" ] || fail 'inactive service was paused'
  [ ! -s "$SERVICE_LOG" ] || fail 'inactive service was mutated'
  printf 'active\n' > "$SERVICE_STATE"
  [ "$(bash "$ROOT/config/profiles/github/owner.sh" --pause)" = paused ] || fail 'active service was not paused'
  bash "$ROOT/config/profiles/github/owner.sh" --resume
  [ "$(cat "$SERVICE_LOG")" = "$(printf 'stop\nstart')" ] || fail 'owned service pause/resume failed'
)

# Exact release observations include the real owner's read-only removed check.
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$TMP/release-observation"
  export SUBYARD_PROFILE_SELECTED=0 SUBYARD_YARD=default
  export GITHUB_RELEASE_FIXTURE="$ROOT/config/profiles/github/tests/release-observation.py"
  export GITHUB_RELEASE_FIXTURE_LOG="$TMP/release-observation/commands.jsonl"
  cat > "$TMP/bin/incus" <<'PY'
#!/usr/bin/env python3
import json,os,pathlib,runpy,sys
fixture=pathlib.Path(os.environ['GITHUB_RELEASE_FIXTURE'])
commands=runpy.run_path(str(fixture))['read_only_commands'](fixture.parent.parent)
responses={tuple(entry[0]):(entry[1],entry[2] if len(entry)==3 else 0) for entry in commands}
arguments=tuple(sys.argv[1:])
if arguments not in responses: sys.exit(99)
sys.stdin.buffer.read()
with open(os.environ['GITHUB_RELEASE_FIXTURE_LOG'],'a') as output: output.write(json.dumps(arguments)+'\n')
output,code=responses[arguments]
sys.stdout.write(output)
sys.exit(code)
PY
  chmod 0755 "$TMP/bin/incus"
  export PATH="$TMP/bin:$PATH"
  bash "$ROOT/config/profiles/github/owner.sh" --check
  python3 - "$GITHUB_RELEASE_FIXTURE_LOG" <<'PY'
import json,pathlib,sys
calls=[json.loads(line) for line in pathlib.Path(sys.argv[1]).read_text().splitlines()]
assert len(calls)==2 and calls[0][-3:]==['getent','passwd','dev'] and calls[1][-1]=='--check-removed'
PY
)

printf 'ok: GitHub partial-init cleanup and exact release absence observations\n'

#!/usr/bin/env bash
# Host-file identity must survive the service stop between approval and unlink.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP/identity"
export SUBYARD_PROFILE_SELECTED=0 SUBYARD_YARD=fixture
SUBYARD_USER="$(id -un)"
export SUBYARD_USER
export SERVICE_LOG="$TMP/service.log"
export UNIT_FILE="$SUBYARD_OPERATOR_HOME/.config/systemd/user/subyard-github-fixture.service"
install -d -m 0700 "$TMP/bin" "$(dirname "$UNIT_FILE")" "$SUBYARD_HOME/github-broker"
cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --user ] || exit 2
printf '%s\n' "$2" >> "$SERVICE_LOG"
case "$2" in
  disable)
    if [ "$REPLACE_ON_DISABLE" = 1 ]; then
      python3 - "$UNIT_FILE" <<'PY'
import os, stat, sys
path = sys.argv[1]
before = os.stat(path)
with open(path, 'rb') as source:
    body = source.read()
replacement = path + '.replacement'
with open(replacement, 'wb') as target:
    target.write(body)
os.chmod(replacement, stat.S_IMODE(before.st_mode))
os.utime(replacement, ns=(before.st_atime_ns, before.st_mtime_ns))
os.replace(replacement, path)
PY
    fi ;;
  daemon-reload) ;;
  *) exit 2 ;;
esac
SH
cat > "$TMP/bin/incus" <<'SH'
#!/usr/bin/env bash
exit 1
SH
chmod 0755 "$TMP/bin/systemctl" "$TMP/bin/incus"
export PATH="$TMP/bin:$PATH"

capture() {
  python3 -B - "$ROOT/scripts/lib/teardown-plan.py" "$UNIT_FILE" \
    "$SUBYARD_HOME/github-broker/fixture.json" "$SUBYARD_HOME/github-broker/fixture-engine" <<'PY'
import importlib.util, json, os, sys
spec = importlib.util.spec_from_file_location('teardown_plan', sys.argv[1])
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)
artifacts = []
for path in sys.argv[2:]:
    parent = os.open(os.path.dirname(path), os.O_RDONLY | os.O_DIRECTORY)
    try:
        entries = helper.artifact_entries(parent, os.path.basename(path))
        artifacts.append({'Path': path, 'Binding': helper.artifact_digest(entries) if entries else ''})
    finally:
        os.close(parent)
print(json.dumps(artifacts))
PY
}

for phase in before-stop during-stop; do
  printf '# Managed by Subyard GitHub broker\n' > "$UNIT_FILE"
  chmod 0600 "$UNIT_FILE"
  SUBYARD_TEARDOWN_ARTIFACTS="$(capture)"
  export SUBYARD_TEARDOWN_ARTIFACTS
  : > "$SERVICE_LOG"
  export REPLACE_ON_DISABLE=0
  if [ "$phase" = before-stop ]; then
    # Copying bytes and timestamps hides replacement from the old digest.
    REPLACE_ON_DISABLE=1 "$TMP/bin/systemctl" --user disable
    : > "$SERVICE_LOG"
  else
    export REPLACE_ON_DISABLE=1
  fi
  if bash "$ROOT/config/profiles/github/owner.sh" --remove > "$TMP/output" 2>&1; then
    fail "$phase replacement was accepted"
  fi
  grep -q 'plan_stale' "$TMP/output" || fail "$phase did not report stale approval"
  [ -f "$UNIT_FILE" ] || fail "$phase replacement was deleted"
  if [ "$phase" = before-stop ]; then
    [ ! -s "$SERVICE_LOG" ] || fail 'stale unit triggered a service operation'
  else
    [ "$(cat "$SERVICE_LOG")" = disable ] || fail 'replacement was not checked immediately after disabling'
  fi
done
printf 'ok: GitHub teardown preserves replacement unit identities\n'

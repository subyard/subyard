#!/usr/bin/env bash
# Host-free full-backup, archive-verification and service-state checks.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE_DIR="$ROOT/config/profiles/hermes"
CREATE="$PROFILE_DIR/hermes-backup-create"
FINALIZE="$PROFILE_DIR/hermes-backup-finalize"
VERIFY="$PROFILE_DIR/verify-backup.py"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/runtime/source/node_modules/.bin" \
  "$tmp/runtime/venv/bin" "$tmp/runtime/node/bin" \
  "$tmp/runtime/playwright/chromium-fixture" "$tmp/home/workspace" "$tmp/stages"
printf 'lock\n' > "$tmp/runtime/source/uv.lock"
printf '%s\n' \
  '{"name":"fixture","lockfileVersion":3,"packages":{"node_modules/agent-browser":{"version":"0.26.0"}}}' \
  > "$tmp/runtime/source/package-lock.json"
printf '%s\n' 3ef6bbd201263d354fd83ec55b3c306ded2eb72a \
  > "$tmp/runtime/.subyard-commit"
browser_contract='node=22.20.0 npm=10.9.3 agent-browser=0.26.0 playwright=1.62.1'
printf -v browser_contract_q '%q' "$browser_contract"
printf '%s\n' "$browser_contract" > "$tmp/runtime/.subyard-browser-runtime"
printf 'model:\n  provider: fixture\n' > "$tmp/home/config.yaml"
printf 'HERMES_DASHBOARD_SESSION_TOKEN=%064d\n' 0 > "$tmp/home/.serve.env"

cat > "$tmp/runtime/venv/bin/hermes" <<'HERMES'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = --version ]; then
  printf 'hermes 0.19.0\n'
  exit
fi
if [ "${1:-}" = backup ] && [ "${2:-}" = --output ]; then
  archive="$3"
  python3 - "$archive" "$HERMES_HOME" <<'PY'
import sys
import zipfile
from pathlib import Path

archive = Path(sys.argv[1])
home = Path(sys.argv[2])
with zipfile.ZipFile(archive, "w") as output:
    output.write(home / "config.yaml", "config.yaml")
    output.write(home / ".serve.env", ".serve.env")
PY
  if [ "${HERMES_TEST_BACKUP_INCOMPLETE:-0}" = 1 ]; then
    printf 'Backup incomplete: %s\n' "$archive"
  else
    printf 'Backup complete: %s\n' "$archive"
  fi
  exit
fi
exit 90
HERMES
chmod +x "$tmp/runtime/venv/bin/hermes"
cp "$tmp/runtime/venv/bin/hermes" "$tmp/hermes"
chmod 0755 "$tmp/hermes"
cat > "$tmp/runtime/venv/bin/python" <<'PYTHON'
#!/usr/bin/env bash
if [ "${1:-}" = -c ]; then
  printf '0.19.0\n'
  exit
fi
exec python3 "$@"
PYTHON
chmod +x "$tmp/runtime/venv/bin/python"

cat > "$tmp/runtime/node/bin/node" <<'NODE'
#!/usr/bin/env bash
[ "${1:-}" = --version ] && { printf 'v22.20.0\n'; exit; }
exit 89
NODE
cat > "$tmp/runtime/node/bin/npm" <<'NPM'
#!/usr/bin/env bash
[ "${1:-}" = --version ] && { printf '10.9.3\n'; exit; }
exit 88
NPM
cat > "$tmp/runtime/source/node_modules/.bin/agent-browser" <<'AGENT_BROWSER'
#!/usr/bin/env bash
[ "${1:-}" = --version ] && { printf 'agent-browser 0.26.0\n'; exit; }
exit 87
AGENT_BROWSER
printf '#!/usr/bin/env bash\nexit 0\n' > "$tmp/runtime/node/bin/npx"
printf '#!/usr/bin/env bash\nexit 0\n' \
  > "$tmp/runtime/playwright/chromium-fixture/chrome"
chmod +x "$tmp/runtime/node/bin/node" "$tmp/runtime/node/bin/npm" \
  "$tmp/runtime/node/bin/npx" "$tmp/runtime/source/node_modules/.bin/agent-browser" \
  "$tmp/runtime/playwright/chromium-fixture/chrome"
ln -s "$tmp/runtime/node/bin/node" "$tmp/node-command"
ln -s "$tmp/runtime/node/bin/npm" "$tmp/npm-command"
ln -s "$tmp/runtime/node/bin/npx" "$tmp/npx-command"
cp "$tmp/runtime/source/node_modules/.bin/agent-browser" "$tmp/agent-browser-command"
chmod 0755 "$tmp/agent-browser-command"

cat > "$tmp/bin/systemctl" <<'SYSTEMCTL'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  is-active)
    [ "$(<"$HERMES_TEST_SERVICE_STATE")" = active ]
    ;;
  stop)
    printf 'inactive\n' > "$HERMES_TEST_SERVICE_STATE"
    ;;
  start)
    printf 'active\n' > "$HERMES_TEST_SERVICE_STATE"
    ;;
  *) ;;
esac
SYSTEMCTL
cat > "$tmp/bin/pgrep" <<'PGREP'
#!/usr/bin/env bash
exit 1
PGREP
cat > "$tmp/bin/runuser" <<'RUNUSER'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = -u ]
shift 2
[ "${1:-}" = -- ]
shift
exec "$@"
RUNUSER
chmod +x "$tmp/bin/systemctl" "$tmp/bin/pgrep" "$tmp/bin/runuser"
printf 'active\n' > "$tmp/service.state"

runtime_env="$tmp/runtime.env"
cat > "$runtime_env" <<EOF
HERMES_VERSION=0.19.0
HERMES_TAG=v2026.7.20
HERMES_COMMIT=3ef6bbd201263d354fd83ec55b3c306ded2eb72a
HERMES_PORT=9119
HERMES_HOME=$tmp/home
HERMES_INSTALL_ROOT=$tmp/runtime
HERMES_SOURCE=$tmp/runtime/source
HERMES_VENV=$tmp/runtime/venv
HERMES_NODE_VERSION=22.20.0
HERMES_NPM_VERSION=10.9.3
HERMES_AGENT_BROWSER_VERSION=0.26.0
HERMES_PLAYWRIGHT_VERSION=1.62.1
HERMES_BROWSER_RUNTIME_MARKER=$tmp/runtime/.subyard-browser-runtime
HERMES_BROWSER_RUNTIME_CONTRACT=$browser_contract_q
HERMES_NODE=$tmp/runtime/node/bin/node
HERMES_NPM=$tmp/runtime/node/bin/npm
HERMES_NPX=$tmp/runtime/node/bin/npx
HERMES_AGENT_BROWSER=$tmp/runtime/source/node_modules/.bin/agent-browser
HERMES_PLAYWRIGHT_BROWSERS_PATH=$tmp/runtime/playwright
HERMES_BROWSER_EXECUTABLE=$tmp/runtime/playwright/chromium-fixture/chrome
HERMES_NODE_COMMAND=$tmp/node-command
HERMES_NPM_COMMAND=$tmp/npm-command
HERMES_NPX_COMMAND=$tmp/npx-command
HERMES_AGENT_BROWSER_COMMAND=$tmp/agent-browser-command
HERMES_DEV_USER=$(id -un)
HERMES_DEV_GROUP=$(id -gn)
HERMES_DEV_HOME=$tmp
HERMES_LAUNCHER=$tmp/hermes
HERMES_RUNTIME_OWNER=$(id -u):$(id -g)
HERMES_PIN_CHECK=$PROFILE_DIR/hermes-pin-check
HERMES_VERIFY_BACKUP=$VERIFY
EOF

test_env=(
  PATH="$tmp/bin:$PATH"
  HERMES_RUNTIME_ENV="$runtime_env"
  HERMES_BACKUP_TEST_ALLOW_NON_ROOT=1
  HERMES_BACKUP_STAGE_ROOT="$tmp/stages"
  HERMES_BACKUP_LOCK_PATH="$tmp/backup.lock"
  HERMES_TEST_SERVICE_STATE="$tmp/service.state"
)

output="$(env "${test_env[@]}" "$CREATE" scheduled)"
stage="$(printf '%s\n' "$output" | sed -n 's/^BACKUP_DIR=//p')"
archive="$(printf '%s\n' "$output" | sed -n 's/^BACKUP_ZIP=//p')"
sha="$(printf '%s\n' "$output" | sed -n 's/^BACKUP_SHA256=//p')"
[ -d "$stage" ] && [ -f "$archive" ] || fail "backup staging was not retained"
[ "$(<"$tmp/service.state")" = inactive ] \
  || fail "service restarted before durable snapshot confirmation"
[ "$(stat -c %a "$stage")" = 700 ] || fail "backup staging mode is not 0700"
[ "$(stat -c %a "$archive")" = 600 ] || fail "backup ZIP mode is not 0600"
"$VERIFY" "$archive" "$sha" >/dev/null
grep -Fxq "sha256=$sha" "$stage/metadata.env" \
  || fail "metadata does not bind the archive hash"
grep -Fxq 'backup_type=scheduled' "$stage/metadata.env" \
  || fail "metadata omitted the backup type"

env "${test_env[@]}" "$FINALIZE" "$stage" success
[ ! -e "$stage" ] || fail "confirmed staging was not removed"
[ "$(<"$tmp/service.state")" = active ] \
  || fail "service state was not restored after confirmation"

unsafe="$tmp/unsafe.zip"
python3 - "$unsafe" <<'PY'
import sys
import zipfile

with zipfile.ZipFile(sys.argv[1], "w") as output:
    output.writestr("../config.yaml", "x")
    output.writestr(".serve.env", "x")
PY
if "$VERIFY" "$unsafe" >/dev/null 2>&1; then
  fail "unsafe archive path was accepted"
fi

printf 'active\n' > "$tmp/service.state"
if env "${test_env[@]}" HERMES_TEST_BACKUP_INCOMPLETE=1 \
  "$CREATE" scheduled >"$tmp/incomplete.out" 2>&1; then
  fail "upstream incomplete marker was accepted"
fi
[ "$(<"$tmp/service.state")" = active ] \
  || fail "service state was not restored after backup failure"
grep -Fq 'upstream reported an incomplete backup' "$tmp/incomplete.out" \
  || fail "incomplete-backup error is unclear"

printf 'ok: Hermes full backup verification and service-state recovery\n'

#!/usr/bin/env bash
# Hermes profile checks with fake downloads, uv and systemd.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROFILE_DIR="$ROOT/config/profiles/hermes"
PROFILE="$PROFILE_DIR/profile.conf"
HOOK="$PROFILE_DIR/provision.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -x "$HOOK" ] || fail "Hermes provision hook is not executable"
# shellcheck source=config/profiles/hermes/profile.conf
. "$PROFILE"
[ "$PROFILE_NAME" = hermes ] || fail "profile name drifted"
[ "$HERMES_VERSION" = 0.19.0 ] || fail "Hermes version is not pinned"
[[ "$HERMES_COMMIT" =~ ^[0-9a-f]{40}$ ]] || fail "Hermes commit is not a full SHA"
[[ "$HERMES_SOURCE_SHA256" =~ ^[0-9a-f]{64}$ ]] || fail "source hash is invalid"
[ "$HERMES_NODE_VERSION" = 22.20.0 ] || fail "Node.js version is not pinned"
[ "$HERMES_NPM_VERSION" = 10.9.3 ] || fail "npm version is not pinned"
[ "$HERMES_AGENT_BROWSER_VERSION" = 0.26.0 ] \
  || fail "agent-browser version is not pinned"
[[ "$HERMES_AGENT_BROWSER_SHA256" =~ ^[0-9a-f]{64}$ ]] \
  || fail "agent-browser package hash is invalid"
[ "$HERMES_PLAYWRIGHT_VERSION" = 1.62.1 ] \
  || fail "Playwright version is not pinned"
[[ "$HERMES_NODE_AMD64_SHA256" =~ ^[0-9a-f]{64}$ ]] \
  || fail "Node.js amd64 hash is invalid"
[[ "$HERMES_NODE_ARM64_SHA256" =~ ^[0-9a-f]{64}$ ]] \
  || fail "Node.js arm64 hash is invalid"
[ "$HERMES_HOME" = /srv/hermes ] || fail "persistent home drifted"
[ "$HERMES_PORT" = 9119 ] || fail "loopback port drifted"

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/source/hermes-fixture" \
  "$tmp/uv/uv-x86_64-unknown-linux-gnu" \
  "$tmp/uv/uv-aarch64-unknown-linux-gnu" \
  "$tmp/node/node-v22.20.0-linux-x64/bin" \
  "$tmp/agent-browser/package/bin" "$tmp/root"
printf 'fixture lock\n' > "$tmp/source/hermes-fixture/uv.lock"
printf '[project]\nname="fixture"\n' > "$tmp/source/hermes-fixture/pyproject.toml"
printf '{"name":"hermes-fixture","version":"1.0.0"}\n' \
  > "$tmp/source/hermes-fixture/package.json"
printf '%s\n' \
  '{"name":"hermes-fixture","lockfileVersion":3,"packages":{"node_modules/agent-browser":{"version":"0.26.0"}}}' \
  > "$tmp/source/hermes-fixture/package-lock.json"
tar -czf "$tmp/source.tar.gz" -C "$tmp/source" hermes-fixture

cat > "$tmp/agent-browser/package/bin/agent-browser.js" <<'AGENT_BROWSER'
#!/usr/bin/env bash
if [ -n "${HERMES_TEST_EXPECTED_NODE_BIN:-}" ]; then
  [ "${PATH%%:*}" = "$HERMES_TEST_EXPECTED_NODE_BIN" ] || exit 83
fi
if [ -n "${HERMES_TEST_AGENT_BROWSER_ENV_LOG:-}" ]; then
  printf 'browsers=%s\nexecutable=%s\n' \
    "${PLAYWRIGHT_BROWSERS_PATH:-}" "${AGENT_BROWSER_EXECUTABLE_PATH:-}" \
    > "$HERMES_TEST_AGENT_BROWSER_ENV_LOG"
fi
if [ "${1:-}" = --version ]; then printf 'agent-browser 0.26.0\n'; exit; fi
exit 88
AGENT_BROWSER
printf '%s\n' '{"name":"agent-browser","version":"0.26.0"}' \
  > "$tmp/agent-browser/package/package.json"
chmod +x "$tmp/agent-browser/package/bin/agent-browser.js"
tar -czf "$tmp/agent-browser.tar.gz" -C "$tmp/agent-browser" package

cat > "$tmp/node/node-v22.20.0-linux-x64/bin/node" <<'NODE'
#!/usr/bin/env bash
if [ "${1:-}" = --version ]; then printf 'v22.20.0\n'; exit; fi
exit 90
NODE
cat > "$tmp/node/node-v22.20.0-linux-x64/bin/npm" <<'NPM'
#!/usr/bin/env bash
set -euo pipefail
if [ -n "${HERMES_TEST_EXPECTED_NODE_BIN:-}" ]; then
  [ "${PATH%%:*}" = "$HERMES_TEST_EXPECTED_NODE_BIN" ] || exit 84
fi
if [ "${1:-}" = --version ]; then printf '10.9.3\n'; exit; fi
printf '%s\n' "$*" >> "$HERMES_TEST_NPM_LOG"
exit 89
NPM
cat > "$tmp/node/node-v22.20.0-linux-x64/bin/npx" <<'NPX'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$HERMES_TEST_NPX_LOG"
[ "$*" = '--yes playwright@1.62.1 install --with-deps chromium' ] || exit 87
[ -n "${PLAYWRIGHT_BROWSERS_PATH:-}" ] || exit 86
mkdir -p "$PLAYWRIGHT_BROWSERS_PATH/chromium-fixture"
printf '#!/usr/bin/env bash\nexit 0\n' > "$PLAYWRIGHT_BROWSERS_PATH/chromium-fixture/chrome"
chmod +x "$PLAYWRIGHT_BROWSERS_PATH/chromium-fixture/chrome"
NPX
chmod +x "$tmp/node/node-v22.20.0-linux-x64/bin/node" \
  "$tmp/node/node-v22.20.0-linux-x64/bin/npm" \
  "$tmp/node/node-v22.20.0-linux-x64/bin/npx"
tar -cJf "$tmp/node.tar.xz" -C "$tmp/node" node-v22.20.0-linux-x64
: > "$tmp/npm.log"

cat > "$tmp/uv/uv-x86_64-unknown-linux-gnu/uv" <<'UV'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$HERMES_TEST_UV_LOG"
if [ "${1:-}" = --version ]; then
  printf 'uv %s (x86_64-unknown-linux-gnu)\n' "$HERMES_UV_VERSION"
  exit
fi
if [ "${1:-}" = python ] && [ "${2:-}" = install ]; then
  exit
fi
if [ "${1:-}" = sync ]; then
  mkdir -p "$UV_PROJECT_ENVIRONMENT/bin"
  cat > "$UV_PROJECT_ENVIRONMENT/bin/hermes" <<'HERMES'
#!/usr/bin/env bash
if [ "${1:-}" = --version ]; then
  printf 'hermes 0.19.0\n'
  exit
fi
printf 'home=%s\nlazy=%s\nnode_path=%s\nbrowsers=%s\nexecutable=%s\nargs=%s\n' \
  "${HERMES_HOME:-}" "${HERMES_DISABLE_LAZY_INSTALLS:-}" \
  "${PATH%%:*}" "${PLAYWRIGHT_BROWSERS_PATH:-}" \
  "${AGENT_BROWSER_EXECUTABLE_PATH:-}" "$*" \
  > "$HERMES_TEST_LAUNCH_LOG"
HERMES
  cat > "$UV_PROJECT_ENVIRONMENT/bin/python" <<'PYTHON'
#!/usr/bin/env bash
if [ "${1:-}" = -c ]; then
  printf '0.19.0\n'
  exit
fi
exec python3 "$@"
PYTHON
  chmod +x "$UV_PROJECT_ENVIRONMENT/bin/hermes" "$UV_PROJECT_ENVIRONMENT/bin/python"
  exit
fi
exit 91
UV
cp "$tmp/uv/uv-x86_64-unknown-linux-gnu/uv" \
  "$tmp/uv/uv-aarch64-unknown-linux-gnu/uv"
chmod +x "$tmp/uv/uv-x86_64-unknown-linux-gnu/uv" \
  "$tmp/uv/uv-aarch64-unknown-linux-gnu/uv"
tar -czf "$tmp/uv.tar.gz" -C "$tmp/uv" \
  uv-x86_64-unknown-linux-gnu uv-aarch64-unknown-linux-gnu
source_sha="$(sha256sum "$tmp/source.tar.gz" | awk '{print $1}')"
uv_sha="$(sha256sum "$tmp/uv.tar.gz" | awk '{print $1}')"

cat > "$tmp/bin/apt-get" <<'APT'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$HERMES_TEST_APT_LOG"
APT
cat > "$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
url=""
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    http*) url="$1"; shift ;;
    -o) output="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$url" ] && [ -n "$output" ]
case "$url" in
  *codeload.github.com*) cp "$HERMES_TEST_SOURCE_ARCHIVE" "$output" ;;
  *astral-sh/uv*) cp "$HERMES_TEST_UV_ARCHIVE" "$output" ;;
  *nodejs.org*) cp "$HERMES_TEST_NODE_ARCHIVE" "$output" ;;
  *registry.npmjs.org/agent-browser*) cp "$HERMES_TEST_AGENT_BROWSER_ARCHIVE" "$output" ;;
  *) exit 92 ;;
esac
printf '%s\n' "$url" >> "$HERMES_TEST_CURL_LOG"
CURL
cat > "$tmp/bin/systemctl" <<'SYSTEMCTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$HERMES_TEST_SYSTEMCTL_LOG"
active="$HERMES_TEST_SYSTEMCTL_STATE.active"
enabled="$HERMES_TEST_SYSTEMCTL_STATE.enabled"
[ -e "$active" ] || printf '%s\n' "${HERMES_TEST_SERVICE_ACTIVE:-0}" > "$active"
[ -e "$enabled" ] || printf '%s\n' "${HERMES_TEST_SERVICE_ENABLED:-0}" > "$enabled"
case "${1:-}" in
  is-active) [ "$(<"$active")" = 1 ] ;;
  is-enabled) [ "$(<"$enabled")" = 1 ] ;;
  start|restart) printf '1\n' > "$active" ;;
  stop) printf '0\n' > "$active" ;;
  try-restart) : ;;
  enable)
    printf '1\n' > "$enabled"
    for argument in "$@"; do
      [ "$argument" != --now ] || printf '1\n' > "$active"
    done
    ;;
  disable)
    printf '0\n' > "$enabled"
    for argument in "$@"; do
      [ "$argument" != --now ] || printf '0\n' > "$active"
    done
    ;;
esac
SYSTEMCTL
chmod +x "$tmp/bin/apt-get" "$tmp/bin/curl" "$tmp/bin/systemctl"

common_env=(
  PATH="$tmp/bin:$PATH"
  HERMES_TEST_ALLOW_NON_ROOT=1
  HERMES_TEST_ROOT="$tmp/root"
  HERMES_TEST_SOURCE_ARCHIVE="$tmp/source.tar.gz"
  HERMES_TEST_UV_ARCHIVE="$tmp/uv.tar.gz"
  HERMES_TEST_NODE_ARCHIVE="$tmp/node.tar.xz"
  HERMES_TEST_AGENT_BROWSER_ARCHIVE="$tmp/agent-browser.tar.gz"
  HERMES_TEST_APT_LOG="$tmp/apt.log"
  HERMES_TEST_CURL_LOG="$tmp/curl.log"
  HERMES_TEST_SYSTEMCTL_LOG="$tmp/systemctl.log"
  HERMES_TEST_SYSTEMCTL_STATE="$tmp/systemctl-state"
  HERMES_TEST_UV_LOG="$tmp/uv.log"
  HERMES_TEST_NPM_LOG="$tmp/npm.log"
  HERMES_TEST_NPX_LOG="$tmp/npx.log"
  HERMES_TEST_AGENT_BROWSER_ENV_LOG="$tmp/agent-browser-env.log"
  HERMES_TEST_LAUNCH_LOG="$tmp/launch.log"
  HERMES_TEST_EXPECTED_NODE_BIN="$tmp/root/opt/hermes-agent/node/bin"
  DEV_USER="$(id -un)"
  DEV_GROUP="$(id -gn)"
  HERMES_DEV_HOME="$tmp/home"
  HERMES_VERSION="$HERMES_VERSION"
  HERMES_TAG="$HERMES_TAG"
  HERMES_COMMIT="$HERMES_COMMIT"
  HERMES_SOURCE_SHA256="$source_sha"
  HERMES_PYTHON_VERSION="$HERMES_PYTHON_VERSION"
  HERMES_UV_VERSION="$HERMES_UV_VERSION"
  HERMES_UV_AMD64_SHA256="$uv_sha"
  HERMES_UV_ARM64_SHA256="$uv_sha"
  HERMES_NODE_VERSION="$HERMES_NODE_VERSION"
  HERMES_NPM_VERSION="$HERMES_NPM_VERSION"
  HERMES_NODE_AMD64_SHA256="$(sha256sum "$tmp/node.tar.xz" | awk '{print $1}')"
  HERMES_NODE_ARM64_SHA256="$(sha256sum "$tmp/node.tar.xz" | awk '{print $1}')"
  HERMES_AGENT_BROWSER_VERSION="$HERMES_AGENT_BROWSER_VERSION"
  HERMES_AGENT_BROWSER_SHA256="$(sha256sum "$tmp/agent-browser.tar.gz" | awk '{print $1}')"
  HERMES_PLAYWRIGHT_VERSION="$HERMES_PLAYWRIGHT_VERSION"
  HERMES_HOME="$HERMES_HOME"
  HERMES_PORT="$HERMES_PORT"
)

env "${common_env[@]}" bash "$HOOK" >/dev/null
install_root="$tmp/root/opt/hermes-agent"
state_root="$tmp/root/srv/hermes"
[ "$(<"$install_root/.subyard-commit")" = "$HERMES_COMMIT" ] \
  || fail "runtime commit marker drifted"
[ -x "$install_root/venv/bin/hermes" ] || fail "runtime entrypoint was not installed"
[ "$($install_root/node/bin/node --version)" = "v$HERMES_NODE_VERSION" ] \
  || fail "pinned Node.js runtime was not installed"
[ "$($install_root/node/bin/npm --version)" = "$HERMES_NPM_VERSION" ] \
  || fail "pinned npm runtime was not installed"
agent_browser="$install_root/agent-browser/bin/agent-browser.js"
[ "$($agent_browser --version)" = "agent-browser $HERMES_AGENT_BROWSER_VERSION" ] \
  || fail "locked agent-browser runtime was not installed"
[ -x "$install_root/playwright/chromium-fixture/chrome" ] \
  || fail "pinned Playwright Chromium was not installed"
[ ! -s "$tmp/npm.log" ] \
  || fail "provision installed unrelated Hermes JavaScript dependencies"
grep -Fq "agent-browser/-/agent-browser-$HERMES_AGENT_BROWSER_VERSION.tgz" \
  "$tmp/curl.log" || fail "exact agent-browser package was not downloaded"
grep -Fxq -- "--yes playwright@$HERMES_PLAYWRIGHT_VERSION install --with-deps chromium" \
  "$tmp/npx.log" || fail "Playwright Chromium install was not exact or lacked OS dependencies"
grep -Fq "node-v$HERMES_NODE_VERSION-linux-x64.tar.xz" "$tmp/curl.log" \
  || fail "pinned Node.js archive was not downloaded"
for command in node npm npx; do
  command_path="$tmp/root/usr/local/bin/$command"
  [ -L "$command_path" ] || fail "$command canonical path is not a managed symlink"
done
[ "$(readlink "$tmp/root/usr/local/bin/node")" = "$install_root/node/bin/node" ] \
  || fail "canonical Node.js target drifted"
agent_browser_command="$tmp/root/usr/local/bin/agent-browser"
[ -f "$agent_browser_command" ] && [ ! -L "$agent_browser_command" ] \
  && [ -x "$agent_browser_command" ] \
  || fail "canonical agent-browser wrapper was not installed"
[ "$(HERMES_TEST_AGENT_BROWSER_ENV_LOG="$tmp/agent-browser-env.log" \
  "$agent_browser_command" --version)" = \
  "agent-browser $HERMES_AGENT_BROWSER_VERSION" ] \
  || fail "canonical agent-browser wrapper failed"
browser_env="$(<"$tmp/agent-browser-env.log")"
[ "$browser_env" = "browsers=$install_root/playwright
executable=$install_root/playwright/chromium-fixture/chrome" ] \
  || fail "canonical agent-browser browser env drifted: $browser_env"
launcher="$tmp/root/usr/local/bin/hermes"
[ -x "$launcher" ] || fail "yard-wide Hermes launcher was not installed"
[ "$(stat -c %a "$launcher")" = 755 ] || fail "Hermes launcher mode is not 0755"
expected_owner="$(id -u):$(id -g)"
[ "$(stat -c %u:%g "$launcher")" = "$expected_owner" ] \
  || fail "Hermes launcher owner drifted"
env HERMES_HOME=/tmp/wrong HERMES_DISABLE_LAZY_INSTALLS=0 \
  HERMES_TEST_LAUNCH_LOG="$tmp/launch.log" "$launcher" chat --fixture
grep -Fxq "home=$state_root" "$tmp/launch.log" \
  || fail "launcher did not force the persistent Hermes home"
grep -Fxq 'lazy=1' "$tmp/launch.log" \
  || fail "launcher did not disable lazy installs"
grep -Fxq "node_path=$install_root/node/bin" "$tmp/launch.log" \
  || fail "launcher did not prefer the pinned Node.js runtime"
grep -Fxq "browsers=$install_root/playwright" "$tmp/launch.log" \
  || fail "launcher did not select the pinned browser runtime"
grep -Fxq "executable=$install_root/playwright/chromium-fixture/chrome" \
  "$tmp/launch.log" \
  || fail "launcher did not select the pinned Chromium executable"
grep -Fxq 'args=chat --fixture' "$tmp/launch.log" \
  || fail "launcher did not preserve arguments"
[ -x "$tmp/root/usr/local/sbin/hermes-provider-ready" ] \
  || fail "provider-ready helper was not installed"
[ -x "$tmp/root/usr/local/sbin/hermes-backup-create" ] \
  || fail "backup helper was not installed"
[ -x "$tmp/root/usr/local/sbin/hermes-restore" ] \
  || fail "restore helper was not installed"
[ -f "$tmp/root/etc/systemd/system/hermes-serve.service" ] \
  || fail "systemd unit was not installed"
grep -Fq 'HERMES_DISABLE_LAZY_INSTALLS=1' \
  "$tmp/root/etc/systemd/system/hermes-serve.service" \
  || fail "service does not seal runtime dependencies"
grep -Fq 'Environment=PLAYWRIGHT_BROWSERS_PATH=/opt/hermes-agent/playwright' \
  "$tmp/root/etc/systemd/system/hermes-serve.service" \
  || fail "service does not select the pinned browser runtime"
grep -Fq "Environment=AGENT_BROWSER_EXECUTABLE_PATH=$install_root/playwright/chromium-fixture/chrome" \
  "$tmp/root/etc/systemd/system/hermes-serve.service" \
  || fail "service does not select the pinned Chromium executable"
grep -Fq 'Environment=PATH=/opt/hermes-agent/node/bin:/opt/hermes-agent/agent-browser/bin:' \
  "$tmp/root/etc/systemd/system/hermes-serve.service" \
  || fail "service does not prefer the pinned Node.js/browser commands"
grep -Fq 'ExecStart=/opt/hermes-agent/venv/bin/hermes serve --host 127.0.0.1 --port 9119 --skip-build' \
  "$tmp/root/etc/systemd/system/hermes-serve.service" \
  || fail "service bind or command drifted"
[ "$(stat -c %a "$state_root")" = 700 ] || fail "Hermes home is not private"
[ "$(stat -c %a "$state_root/.serve.env")" = 600 ] \
  || fail "session token file mode is not 0600"
token_hash="$(sha256sum "$state_root/.serve.env")"
grep -Eq '^HERMES_DASHBOARD_SESSION_TOKEN=[0-9a-f]{64}$' "$state_root/.serve.env" \
  || fail "session token file is malformed"
grep -Fq 'sync --locked --no-dev --python' "$tmp/uv.log" \
  || fail "uv sync is not locked or includes development dependencies"

pin_check="$tmp/root/usr/local/libexec/subyard-hermes-pin-check"
runtime_env="$tmp/root/etc/subyard/hermes-runtime.env"
curl_lines="$(wc -l < "$tmp/curl.log")"
systemctl_lines="$(wc -l < "$tmp/systemctl.log")"
env "${common_env[@]}" bash "$HOOK" --check >/dev/null \
  || fail "converged Hermes provision check failed"
[ "$(wc -l < "$tmp/curl.log")" -eq "$curl_lines" ] \
  || fail "Hermes provision check downloaded content"
[ "$(wc -l < "$tmp/systemctl.log")" -eq "$systemctl_lines" ] \
  || fail "Hermes provision check changed service state"
printf '%s\n' 0000000000000000000000000000000000000000 > "$install_root/.subyard-commit"
set +e
env "${common_env[@]}" bash "$HOOK" --check >/dev/null 2>&1
check_status=$?
set -e
[ "$check_status" -eq 10 ] || fail "drifted Hermes provision check returned $check_status, want 10"
printf '%s\n' "$HERMES_COMMIT" > "$install_root/.subyard-commit"
grep -Fxq "HERMES_BROWSER_EXECUTABLE=$install_root/playwright/chromium-fixture/chrome" \
  "$runtime_env" || fail "runtime contract omitted the Chromium executable"
grep -Fq 'PLAYWRIGHT_BROWSERS_PATH="$HERMES_PLAYWRIGHT_BROWSERS_PATH"' \
  "$tmp/root/usr/local/sbin/hermes-provider-ready" \
  || fail "provider approval does not expose the Playwright browser root to Hermes doctor"
grep -Fq 'AGENT_BROWSER_EXECUTABLE_PATH="$HERMES_BROWSER_EXECUTABLE"' \
  "$tmp/root/usr/local/sbin/hermes-provider-ready" \
  || fail "provider approval does not expose Chromium to Hermes doctor"
grep -Fq 'PATH="$(dirname "$HERMES_NODE"):$HERMES_VENV/bin:' \
  "$tmp/root/usr/local/sbin/hermes-provider-ready" \
  || fail "provider approval does not expose pinned Node.js to Hermes doctor"
HERMES_RUNTIME_ENV="$runtime_env" "$pin_check" --runtime-only \
  || fail "installed runtime-only pin check failed"
browser_marker="$install_root/.subyard-browser-runtime"
browser_contract="$(<"$browser_marker")"
printf '%s\n' 'node=0.0.0 npm=0.0.0 agent-browser=0.0.0 playwright=0.0.0' \
  > "$browser_marker"
if HERMES_RUNTIME_ENV="$runtime_env" "$pin_check" --runtime-only >/dev/null 2>&1; then
  fail "pin check accepted a browser runtime contract mismatch"
fi
printf '%s\n' "$browser_contract" > "$browser_marker"
mv "$launcher" "$launcher.missing"
if HERMES_RUNTIME_ENV="$runtime_env" "$pin_check" --runtime-only >/dev/null 2>&1; then
  fail "pin check accepted a missing yard-wide launcher"
fi
mv "$launcher.missing" "$launcher"
chmod 0777 "$launcher"
if HERMES_RUNTIME_ENV="$runtime_env" "$pin_check" --runtime-only >/dev/null 2>&1; then
  fail "pin check accepted unsafe launcher permissions"
fi
chmod 0755 "$launcher"
if HERMES_RUNTIME_ENV="$runtime_env" "$pin_check" >/dev/null 2>&1; then
  fail "pin check accepted a missing provider-ready marker"
fi
printf '%s\n' "$HERMES_COMMIT" > "$state_root/.provider-ready"
HERMES_RUNTIME_ENV="$runtime_env" "$pin_check" \
  || fail "commit-bound provider-ready marker was rejected"
printf '%s\n' 0000000000000000000000000000000000000000 \
  > "$install_root/.subyard-commit"
if HERMES_RUNTIME_ENV="$runtime_env" "$pin_check" --runtime-only >/dev/null 2>&1; then
  fail "pin check accepted a runtime commit mismatch"
fi
printf '%s\n' "$HERMES_COMMIT" > "$install_root/.subyard-commit"
rm -f "$state_root/.provider-ready"

curl_count="$(wc -l < "$tmp/curl.log")"
npm_count="$(wc -l < "$tmp/npm.log")"
npx_count="$(wc -l < "$tmp/npx.log")"
env "${common_env[@]}" bash "$HOOK" >/dev/null
[ "$(sha256sum "$state_root/.serve.env")" = "$token_hash" ] \
  || fail "re-provision rotated the session token"
[ "$(wc -l < "$tmp/curl.log")" -eq "$curl_count" ] \
  || fail "re-provision downloaded an already pinned runtime"
[ "$(wc -l < "$tmp/npm.log")" -eq "$npm_count" ] \
  || fail "re-provision reinstalled an already locked browser runtime"
[ "$(wc -l < "$tmp/npx.log")" -eq "$npx_count" ] \
  || fail "re-provision downloaded an already pinned Chromium runtime"

cat > "$install_root/node/bin/node" <<'DRIFTED_NODE'
#!/usr/bin/env bash
if [ "${1:-}" = --version ]; then printf 'v0.0.0\n'; exit; fi
exit 85
DRIFTED_NODE
chmod +x "$install_root/node/bin/node"
printf '%s\n' "$HERMES_COMMIT" > "$state_root/.provider-ready"
printf '1\n' > "$tmp/systemctl-state.active"
printf '1\n' > "$tmp/systemctl-state.enabled"
env "${common_env[@]}" bash "$HOOK" >/dev/null
[ "$($install_root/node/bin/node --version)" = "v$HERMES_NODE_VERSION" ] \
  || fail "same-pin provision did not repair Node.js version drift"
[ "$(<"$tmp/systemctl-state.active")" = 1 ] \
  || fail "same-pin repair did not restart the previously active service"
[ "$(<"$tmp/systemctl-state.enabled")" = 1 ] \
  || fail "same-pin repair changed the enabled service state"
[ "$(wc -l < "$tmp/curl.log")" -gt "$curl_count" ] \
  || fail "same-pin runtime drift did not trigger a verified rebuild"

cat > "$install_root/node/bin/node" <<'DRIFTED_NODE'
#!/usr/bin/env bash
if [ "${1:-}" = --version ]; then printf 'v0.0.0\n'; exit; fi
exit 85
DRIFTED_NODE
chmod +x "$install_root/node/bin/node"
printf '%s\n' "$HERMES_COMMIT" > "$state_root/.provider-ready"
printf '1\n' > "$tmp/systemctl-state.active"
printf '1\n' > "$tmp/systemctl-state.enabled"
if env "${common_env[@]}" HERMES_TEST_ABORT_AFTER_SERVICE_STOP=1 \
  bash "$HOOK" >"$tmp/aborted-after-stop.out" 2>&1; then
  fail "pre-move interruption unexpectedly succeeded"
fi
[ "$(<"$tmp/systemctl-state.active")" = 0 ] \
  || fail "pre-move interruption did not occur after service stop"
[ -d "$install_root/.subyard-rollback-state" ] \
  && [ -f "$install_root.transaction" ] \
  || fail "pre-move interruption did not retain recoverable transaction state"
env "${common_env[@]}" bash "$HOOK" >/dev/null
[ "$(<"$tmp/systemctl-state.active")" = 1 ] \
  || fail "pre-move interruption recovery did not restore the active service"
[ "$(<"$tmp/systemctl-state.enabled")" = 1 ] \
  || fail "pre-move interruption recovery did not restore the enabled service"
[ "$($install_root/node/bin/node --version)" = "v$HERMES_NODE_VERSION" ] \
  || fail "pre-move interruption recovery did not converge the runtime"
[ ! -e "$install_root.rollback" ] && [ ! -e "$install_root.transaction" ] \
  || fail "pre-move interruption recovery retained transaction state"

next_commit=1111111111111111111111111111111111111111
printf 'commit=%s\nsnapshot=fixture-transaction\n' "$HERMES_COMMIT" \
  > "$state_root/.last-verified-backup"
printf '%s\n' "$HERMES_COMMIT" > "$state_root/.provider-ready"
runtime_env_hash="$(sha256sum "$runtime_env" | awk '{print $1}')"
launcher_hash="$(sha256sum "$launcher" | awk '{print $1}')"
unit_hash="$(sha256sum "$tmp/root/etc/systemd/system/hermes-serve.service" | awk '{print $1}')"

if env "${common_env[@]}" HERMES_COMMIT="$next_commit" \
  HERMES_TEST_FAIL_AFTER_PUBLICATION=1 HERMES_TEST_ABORT_AFTER_RUNTIME_RESTORE=1 \
  bash "$HOOK" >"$tmp/aborted-runtime-restore.out" 2>&1; then
  fail "rollback-restore interruption unexpectedly succeeded"
fi
[ -d "$install_root/.subyard-rollback-state" ] \
  && [ -f "$install_root.transaction" ] \
  || fail "rollback-restore interruption did not retain recoverable state"
env "${common_env[@]}" bash "$HOOK" >/dev/null
[ "$(<"$install_root/.subyard-commit")" = "$HERMES_COMMIT" ] \
  || fail "rollback-restore interruption did not recover the previous runtime"
[ "$(sha256sum "$runtime_env" | awk '{print $1}')" = "$runtime_env_hash" ] \
  || fail "rollback-restore interruption did not recover managed files"
[ "$(<"$tmp/systemctl-state.active")" = 1 ] \
  && [ "$(<"$tmp/systemctl-state.enabled")" = 1 ] \
  || fail "rollback-restore interruption did not recover service state"
[ ! -e "$install_root.rollback" ] && [ ! -e "$install_root.transaction" ] \
  || fail "rollback-restore interruption recovery retained transaction state"

: > "$tmp/systemctl.log"
if env "${common_env[@]}" HERMES_COMMIT="$next_commit" \
  HERMES_TEST_SERVICE_ACTIVE=1 HERMES_TEST_FAIL_AFTER_PUBLICATION=1 \
  bash "$HOOK" >"$tmp/failed-publication.out" 2>&1; then
  fail "post-publication failure did not abort provisioning"
fi
if [ "$(<"$install_root/.subyard-commit")" != "$HERMES_COMMIT" ]; then
  sed -n '1,160p' "$tmp/failed-publication.out" >&2
  fail "post-publication failure did not restore the previous runtime"
fi
[ "$(sha256sum "$runtime_env" | awk '{print $1}')" = "$runtime_env_hash" ] \
  || fail "post-publication failure did not restore the runtime environment"
[ "$(sha256sum "$launcher" | awk '{print $1}')" = "$launcher_hash" ] \
  || fail "post-publication failure did not restore the Hermes launcher"
[ "$(sha256sum "$tmp/root/etc/systemd/system/hermes-serve.service" | awk '{print $1}')" = \
  "$unit_hash" ] || fail "post-publication failure did not restore the systemd unit"
[ "$(<"$state_root/.provider-ready")" = "$HERMES_COMMIT" ] \
  || fail "post-publication failure did not restore provider approval"
grep -Fxq 'stop hermes-serve.service' "$tmp/systemctl.log" \
  || fail "runtime replacement did not stop the active service"
grep -Fxq 'start hermes-serve.service' "$tmp/systemctl.log" \
  || fail "failed runtime replacement did not restart the previously active service"
[ "$(<"$tmp/systemctl-state.active")" = 1 ] \
  || fail "failed runtime replacement did not restore active service state"
[ "$(<"$tmp/systemctl-state.enabled")" = 1 ] \
  || fail "failed runtime replacement did not restore enabled service state"
[ ! -e "$install_root.rollback" ] && [ ! -e "$install_root.transaction" ] \
  || fail "failed runtime replacement left transaction state behind"

for service_case in enabled-only active-only; do
  case "$service_case" in
    enabled-only) expected_active=0; expected_enabled=1 ;;
    active-only) expected_active=1; expected_enabled=0 ;;
  esac
  printf '%s\n' "$expected_active" > "$tmp/systemctl-state.active"
  printf '%s\n' "$expected_enabled" > "$tmp/systemctl-state.enabled"
  printf '%s\n' "$HERMES_COMMIT" > "$state_root/.provider-ready"
  if env "${common_env[@]}" HERMES_COMMIT="$next_commit" \
    HERMES_TEST_FAIL_AFTER_PUBLICATION=1 \
    bash "$HOOK" >"$tmp/failed-$service_case.out" 2>&1; then
    fail "$service_case rollback fixture did not abort provisioning"
  fi
  [ "$(<"$tmp/systemctl-state.active")" = "$expected_active" ] \
    || fail "$service_case rollback changed active service state"
  [ "$(<"$tmp/systemctl-state.enabled")" = "$expected_enabled" ] \
    || fail "$service_case rollback changed enabled service state"
  [ "$(<"$install_root/.subyard-commit")" = "$HERMES_COMMIT" ] \
    || fail "$service_case rollback did not restore the previous runtime"
  [ ! -e "$install_root.rollback" ] && [ ! -e "$install_root.transaction" ] \
    || fail "$service_case rollback retained transaction state"
done

cat > "$install_root/node/bin/node" <<'DRIFTED_NODE'
#!/usr/bin/env bash
if [ "${1:-}" = --version ]; then printf 'v0.0.0\n'; exit; fi
exit 85
DRIFTED_NODE
chmod +x "$install_root/node/bin/node"
if (env "${common_env[@]}" HERMES_TEST_ABORT_DURING_RUNTIME=1 \
  bash "$HOOK") >"$tmp/aborted-runtime.out" 2>&1; then
  fail "interrupted runtime replacement unexpectedly succeeded"
fi
env "${common_env[@]}" bash "$HOOK" >/dev/null
[ "$(<"$install_root/.subyard-commit")" = "$HERMES_COMMIT" ] \
  || fail "provision did not recover an interrupted runtime replacement"
[ "$($install_root/node/bin/node --version)" = "v$HERMES_NODE_VERSION" ] \
  || fail "interrupted runtime recovery did not rebuild the pinned Node.js runtime"
[ ! -e "$install_root.rollback" ] && [ ! -e "$install_root.transaction" ] \
  || fail "successful recovery retained transaction state"

mkdir "$install_root.rollback"
printf 'partial committed cleanup\n' > "$install_root.rollback/partial"
env "${common_env[@]}" bash "$HOOK" >/dev/null
[ ! -e "$install_root.rollback" ] \
  || fail "provision did not resume interrupted post-commit rollback cleanup"

mkdir "$install_root/.subyard-rollback-state"
: > "$install_root/.subyard-rollback-state/item-0.present"
env "${common_env[@]}" bash "$HOOK" >/dev/null
[ ! -e "$install_root/.subyard-rollback-state" ] \
  || fail "provision did not resume interrupted restored-state cleanup"
[ -L "$tmp/root/usr/local/bin/node" ] \
  || fail "partial restored-state cleanup was incorrectly replayed"

rm -f "$state_root/.last-verified-backup"
if env "${common_env[@]}" HERMES_COMMIT="$next_commit" bash "$HOOK" \
  >"$tmp/unverified-update.out" 2>&1; then
  fail "pin update proceeded without a verified backup"
fi
grep -Fq "pin update requires a verified backup of commit $HERMES_COMMIT" \
  "$tmp/unverified-update.out" || fail "unverified update error is unclear"
printf 'commit=%s\nsnapshot=fixture-old\n' "$HERMES_COMMIT" \
  > "$state_root/.last-verified-backup"
printf '%s\n' "$HERMES_COMMIT" > "$state_root/.provider-ready"
env "${common_env[@]}" HERMES_COMMIT="$next_commit" bash "$HOOK" >/dev/null
[ "$(<"$install_root/.subyard-commit")" = "$next_commit" ] \
  || fail "verified pin update did not install the reviewed commit"
[ ! -e "$state_root/.provider-ready" ] \
  || fail "pin update retained stale provider approval"

printf 'commit=%s\nsnapshot=fixture-new\n' "$next_commit" \
  > "$state_root/.last-verified-backup"
env "${common_env[@]}" bash "$HOOK" >/dev/null
[ "$(<"$install_root/.subyard-commit")" = "$HERMES_COMMIT" ] \
  || fail "verified rollback did not restore the exact old commit"

cat > "$tmp/bin/incus" <<'INCUS'
#!/usr/bin/env bash
printf '%s\n' "$*" > "$HERMES_TEST_INCUS_ARGS"
tar -tf - > "$HERMES_TEST_BUNDLE_LOG"
INCUS
chmod +x "$tmp/bin/incus"
engine_env=(
  PATH="$tmp/bin:$PATH"
  HERMES_TEST_INCUS_ARGS="$tmp/incus.args"
  HERMES_TEST_BUNDLE_LOG="$tmp/bundle.log"
  SUBYARD_ENGINE_CONTEXT=1
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1
  SUBYARD_OPERATOR_HOME="$tmp/home"
  SUBYARD_CONFIG_DIR="$tmp/config"
  SUBYARD_CONFIG_HOME="$tmp/config-home"
  SUBYARD_HOME="$ROOT"
  STORAGE_PATH="$tmp/storage"
  HOST_BASE="$tmp/host"
  RESTRICTED_DISK_PATHS=""
  ACCESS_KIND=local
  YARD_KIND=container
  YARD_INSTANCE_NAME=yard-hermes
  INCUS_PROJECT=subyard-hermes
  INCUS_BRIDGE=incusbr0
  SSH_HOST=yard-hermes
  DEV_USER=dev
  DEV_UID=1000
  DEV_SUDO=1
  FORWARD_SSH_AGENT=0
  NESTED_E2E_VMS=0
)
env "${engine_env[@]}" bash "$ROOT/scripts/provision-profile.sh" hermes
grep -Fxq './provision.sh' "$tmp/bundle.log" \
  || fail "profile bundle omitted provision.sh"
grep -Fxq './hermes' "$tmp/bundle.log" \
  || fail "profile bundle omitted the canonical launcher"
grep -Fxq './hermes-serve.service' "$tmp/bundle.log" \
  || fail "profile bundle omitted the systemd unit"
grep -Fxq './hermes-backup-create' "$tmp/bundle.log" \
  || fail "profile bundle omitted a runtime helper"

printf 'ok: Hermes pinned provision and profile bundle\n'

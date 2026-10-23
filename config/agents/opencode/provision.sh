#!/usr/bin/env bash
# Install a missing OpenCode CLI as DEV_USER and expose it on the yard PATH.
set -euo pipefail

DEV_USER="${DEV_USER:-dev}"
INSTALL_URL="${OPENCODE_INSTALL_URL:-https://opencode.ai/install}"
DEV_HOME="${OPENCODE_INSTALL_HOME:-$(getent passwd "$DEV_USER" | cut -d: -f6)}"
: "${DEV_HOME:=/home/$DEV_USER}"
INSTALL_BIN="${OPENCODE_INSTALL_BIN:-$DEV_HOME/.opencode/bin/opencode}"
BIN_LINK="${OPENCODE_BIN_LINK:-/usr/local/bin/opencode}"
SYSTEM_PATH="${OPENCODE_SYSTEM_PATH:-/usr/local/bin:/usr/bin:/bin}"
PERMISSION_CHECK="${OPENCODE_PERMISSION_CHECK_PATH:-$(dirname "$BIN_LINK")/opencode-permission-check}"

die() { printf 'OpenCode provision: %s\n' "$*" >&2; exit 1; }

# This diagnostic never grants approval and is deliberately not an installation gate.
# Native V1 clients can save `always`, regardless of the configured ask patterns.
case "$PERMISSION_CHECK" in /*) ;; *) die 'permission check path must be absolute' ;; esac
if [ -L "$PERMISSION_CHECK" ] || { [ -e "$PERMISSION_CHECK" ] && [ ! -f "$PERMISSION_CHECK" ]; }; then
  die 'permission check is not a regular file'
fi
if [ -f "$PERMISSION_CHECK" ] && ! grep -Fxq '# Subyard OpenCode permission diagnostic' "$PERMISSION_CHECK"; then
  die 'permission check belongs to another install'
fi
mkdir -p "$(dirname "$PERMISSION_CHECK")"
check_stage="$(mktemp "$(dirname "$PERMISSION_CHECK")/.opencode-permission-check.XXXXXX")"
trap 'rm -f -- "$check_stage"' EXIT
cat > "$check_stage" <<'CHECK'
#!/usr/bin/env python3
# Subyard OpenCode permission diagnostic
"""Inspect native effective V1 config without printing configuration or credentials."""
import fnmatch
import json
import os
import selectors
import signal
import subprocess
import sys
import time

ACTIONS = ('allow', 'ask', 'deny')

def native_config():
    process = subprocess.Popen(['opencode', 'debug', 'config', '--pure'],
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                               start_new_session=True)
    deadline = time.monotonic() + 10
    output = bytearray()
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not selector.select(remaining):
                    raise TimeoutError('native config timeout')
                chunk = os.read(process.stdout.fileno(), 65536)
                if not chunk:
                    break
                output.extend(chunk)
                if len(output) > 1024 * 1024:
                    raise ValueError('native config oversized')
        if process.wait(timeout=max(0.01, deadline - time.monotonic())) != 0:
            raise ValueError('native config unavailable')
        return json.loads(output)
    finally:
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
        process.stdout.close()

def decision(rules, command):
    if isinstance(rules, str):
        return rules if rules in ACTIONS else 'unknown'
    if not isinstance(rules, dict):
        return 'unknown'
    action = rules.get('*', 'ask')
    bash = rules.get('bash', action)
    if isinstance(bash, str):
        return bash if bash in ACTIONS else 'unknown'
    if not isinstance(bash, dict):
        return 'unknown'
    for pattern, value in bash.items():
        if fnmatch.fnmatchcase(command, pattern):
            action = value
    return action if action in ACTIONS else 'unknown'

try:
    config = native_config()
    if not isinstance(config, dict) or not isinstance(config.get('permission'), dict):
        raise ValueError('unknown native permission schema')
    rules = config['permission']
    protected = ['git commit', 'git commit -m fixture', 'git commit --amend',
                 'git push', 'git push origin main']
    actions = [decision(rules, command) for command in protected]
    agents = config.get('agent', {})
    overrides = sum(1 for value in agents.values() if isinstance(value, dict) and 'permission' in value)
    print(json.dumps({'state': 'unsupported-single-use', 'configured_protected': actions,
                      'configured_ordinary': decision(rules, 'sh dev-check.sh'),
                      'agent_permission_overrides': overrides,
                      'reason': 'native always approval persists; project, agent and UI permissions can override defaults'}))
    sys.exit(3)
except (OSError, ValueError, TypeError, AttributeError, TimeoutError, subprocess.TimeoutExpired):
    print('{"state":"unverified","reason":"native V1 effective config unavailable"}')
    sys.exit(2)
CHECK
chmod 0755 "$check_stage"
mv -fT -- "$check_stage" "$PERMISSION_CHECK"

run_as_dev() {
  if [ "$(id -un)" = "$DEV_USER" ]; then
    env HOME="$DEV_HOME" SHELL=/bin/bash \
      PATH="$DEV_HOME/.opencode/bin:/usr/local/bin:/usr/bin:/bin" "$@"
  else
    command -v runuser >/dev/null 2>&1 || die "runuser is required to install as $DEV_USER"
    runuser -u "$DEV_USER" -- env HOME="$DEV_HOME" SHELL=/bin/bash \
      PATH="$DEV_HOME/.opencode/bin:/usr/local/bin:/usr/bin:/bin" "$@"
  fi
}

# Keep existing installs.
if [ -x "$BIN_LINK" ]; then
  printf 'OpenCode already available at %s — keeping it\n' "$BIN_LINK"
  exit 0
fi
existing="$(PATH="$SYSTEM_PATH" command -v opencode 2>/dev/null || true)"
if [ -n "$existing" ] && [ -x "$existing" ]; then
  printf 'OpenCode already available at %s — keeping it\n' "$existing"
  exit 0
fi

if [ ! -x "$INSTALL_BIN" ]; then
  printf 'Installing OpenCode for %s with the official installer\n' "$DEV_USER"
  curl -fsSL "$INSTALL_URL" | run_as_dev bash -s -- --no-modify-path
fi
[ -x "$INSTALL_BIN" ] || die "installer did not create $INSTALL_BIN"

# The stable link works for non-login launches without editing shell rc files.
if [ -L "$BIN_LINK" ]; then
  [ "$(readlink "$BIN_LINK")" = "$INSTALL_BIN" ] \
    || die "$BIN_LINK is a symlink managed by another install — leaving it unchanged"
elif [ -e "$BIN_LINK" ]; then
  die "$BIN_LINK already exists and is not executable — leaving it unchanged"
else
  mkdir -p "$(dirname "$BIN_LINK")"
  ln -s "$INSTALL_BIN" "$BIN_LINK"
fi

run_as_dev "$BIN_LINK" --version >/dev/null
printf 'OpenCode is ready at %s\n' "$BIN_LINK"

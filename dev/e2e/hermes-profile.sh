#!/usr/bin/env bash
# Real Hermes profile, tunnel, backup and restore acceptance on one leased VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
AUTH_STAGE="${HERMES_E2E_CODEX_AUTH:-/home/dev/.cache/subyard-hermes-e2e-codex-auth.json}"
REQUIRE_CODEX="${HERMES_E2E_REQUIRE_CODEX:-0}"
STATE=""
SOURCE_YARD=""
RESTORE_YARD=""
TUNNEL_PID=""

die() { printf 'hermes-profile-e2e: %s\n' "$*" >&2; exit 2; }

[ -n "${SUBYARD_E2E_VM:-}" ] || die "run through dev/agent-e2e.sh"
for command in curl incus openssl python3 ssh sudo timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done

if [ ! -x "$ROOT/.build/yard" ]; then
  command -v go >/dev/null 2>&1 || die "Go is required in the leased VM"
  "$ROOT/dev/build-engine.sh"
fi

stop_tunnel() {
  if [ -n "$TUNNEL_PID" ]; then
    kill "$TUNNEL_PID" >/dev/null 2>&1 || true
    wait "$TUNNEL_PID" 2>/dev/null || true
    TUNNEL_PID=""
  fi
}

recover_stale_fixture_pool() {
  local state source status used_by fingerprint
  state="$(timeout --foreground 30 incus storage show default --project default 2>/dev/null)" \
    || return 0
  source="$(printf '%s\n' "$state" | sed -n 's/^  source: //p')"
  case "$source" in /tmp/subyard-hermes-profile.*/storage|/var/tmp/subyard-hermes-profile.*/storage) ;; *) return 0 ;; esac
  status="$(printf '%s\n' "$state" | sed -n 's/^status: //p')"
  [ "$status" = Unavailable ] || return 0
  used_by="$(printf '%s\n' "$state" | sed -n '/^used_by:/,/^status:/p' | sed '$d')"
  printf '  [ .. ] inspecting stale Hermes test pool at %s\n' "$source"
  case "$(printf '%s' "$used_by" | tr -d '[:space:]')" in
    'used_by:[]') ;;
    'used_by:-/1.0/profiles/default')
      incus profile device remove default root --project default >/dev/null
      ;;
    *) die "refusing to recover stale Hermes test pool with consumers: $used_by" ;;
  esac
  while IFS= read -r fingerprint; do
    [ -n "$fingerprint" ] || continue
    incus image delete "$fingerprint" --project default >/dev/null
  done < <(incus image list --project default --format csv -c f)
  printf '  [ .. ] removing unused stale Hermes test pool at %s\n' "$source"
  timeout --foreground 120 incus storage delete default --project default >/dev/null
}

cleanup_fixture_pool() {
  local state source fingerprint
  [ -n "$STATE" ] || return 0
  [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-hermes-profile-e2e-v1 ] || return 1
  state="$(incus storage show default --project default 2>/dev/null)" || return 0
  source="$(printf '%s\n' "$state" | sed -n 's/^  source: //p')"
  [ "$source" = "$STORAGE_PATH" ] || return 0
  case "$source" in /tmp/subyard-hermes-profile.*/storage|/var/tmp/subyard-hermes-profile.*/storage) ;; *) return 1 ;; esac
  [ -z "$(incus list --all-projects --format csv -c n)" ] || return 1
  if incus profile device list default --project default 2>/dev/null | grep -qx root; then
    incus profile device remove default root --project default >/dev/null || return 1
  fi
  while IFS= read -r fingerprint; do
    [ -n "$fingerprint" ] || continue
    incus image delete "$fingerprint" --project default >/dev/null || return 1
  done < <(incus image list --project default --format csv -c f)
  incus storage delete default --project default >/dev/null
}

yard() {
  local name="$1"
  shift
  "$ROOT/.build/yard" -Y "$name" "$@"
}

set_yard_port() {
  local name="$1" port="$2"
  yard "$name" config set SSH_PORT "$port" --scope yard --yes
  yard "$name" start --yes
  yard "$name" init --yes
}

setting() {
  local name="$1" key="$2" output value
  output="$(yard "$name" config show "$key")"
  printf '%s\n' "$output" | grep -q '^effective: ' \
    || die "could not resolve $key for $name"
  value="$(printf '%s\n' "$output" | sed -n 's/^effective: //p')"
  printf '%s\n' "$value"
}

assert_setting() {
  local name="$1" key="$2" want="$3" got
  got="$(setting "$name" "$key")"
  [ -z "$want" ] && [ "$got" = "<unset>" ] && return 0
  [ "$got" = "$want" ] || die "$name $key is '$got', expected '$want'"
}

cleanup() {
  local rc=$? name project
  trap - EXIT INT TERM
  set +e
  stop_tunnel
  for name in "$RESTORE_YARD" "$SOURCE_YARD"; do
    [ -n "$name" ] || continue
    project="subyard-$name"
    if [ -f "$SUBYARD_CONFIG_HOME/yards/$name/config.env" ] \
      && incus project show "$project" >/dev/null 2>&1; then
      yard "$name" teardown --yes >/dev/null 2>&1 || rc=3
    fi
  done
  cleanup_fixture_pool || rc=3
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-hermes-profile.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-hermes-profile-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

recover_stale_fixture_pool

STATE="$(mktemp -d /var/tmp/subyard-hermes-profile.XXXXXX)"
printf '%s\n' subyard-hermes-profile-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
SOURCE_YARD="hermes-e2e-$token"
RESTORE_YARD="hermes-restore-$token"

export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$STATE/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME"
{
  printf '%s\n' \
    'ENVIRONMENT_PROFILES=openclaw' \
    'CODING_TOOL_INTEGRATIONS=claude' \
    'YARD_CAPABILITIES=android' \
    'YARD_CAPS=fuse' \
    'YARD_DEVICES=gpu' \
    'FORWARD_SSH_AGENT=1' \
    'DEV_SUDO=1' \
    'NESTED_E2E_VMS=1'
  printf 'HOST_MOUNTS=host-fixture:/mnt/host-fixture:ro:0755\n'
  printf 'HOST_LINKS=.claude/sessions:/mnt/host/agent-sessions/claude/sessions\n'
  printf 'HOST_CLAUDE_MD=%s/CLAUDE.md\n' "$STATE"
  printf 'HOST_CODEX_AGENTS_MD=%s/CODEX-AGENTS.md\n' "$STATE"
  printf 'HOST_OPENCODE_AGENTS_MD=%s/OPENCODE-AGENTS.md\n' "$STATE"
  printf 'YARD_MOUNTS=cache:/srv/cache:rw:0755\n'
} > "$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"

next_port() {
  local candidate="$1"
  while ss -H -ltn "sport = :$candidate" 2>/dev/null | grep -q .; do
    candidate=$((candidate + 1))
  done
  printf '%s\n' "$candidate"
}

source_ssh_port="$(next_port "$((32000 + ($$ % 10000)))")"
restore_ssh_port="$(next_port "$((source_ssh_port + 1))")"
[ "$restore_ssh_port" != "$source_ssh_port" ] || die "SSH port allocation collided"

assert_clean_yard_boundary() { # <instance> <project>
  local instance="$1" project="$2" device
  while IFS= read -r device; do
    case "$device" in host-*|yx-*) die "unexpected host/extras device $device" ;; esac
  done < <(incus config device list "$instance" --project "$project")
  if incus config device show "$instance" --project "$project" | grep -Fq 9119; then
    die "Hermes port is published as an Incus device"
  fi
  incus exec "$instance" --project "$project" \
    --user 1000 --group 1000 --env HOME=/home/dev -- sh -euc '
test -d /home/dev/.codex
test ! -L /home/dev/.codex
test -f /home/dev/.codex/config.toml
test -f /home/dev/.codex/rules/repo.rules
test ! -e /home/dev/.codex/auth.json
test ! -L /home/dev/.codex/sessions
for path in \
  /home/dev/.claude \
  /home/dev/.config/opencode \
  /home/dev/.pi \
  /opt/paseo-agent \
  /usr/local/bin/paseo; do
  test ! -e "$path"
done
for command in claude opencode pi paseo; do
  ! command -v "$command" >/dev/null 2>&1
done
! command -v tailscale >/dev/null 2>&1
[ "$(codex --version)" = "codex-cli 0.147.0" ]
codex app-server --help >/dev/null
[ "$(node --version)" = "v22.20.0" ]
[ "$(npm --version)" = "10.9.3" ]
[ "$(agent-browser --version)" = "agent-browser 0.26.0" ]
[ "$(readlink /usr/local/bin/node)" = "/opt/hermes-agent/node/bin/node" ]
[ "$(readlink /usr/local/bin/npm)" = "/opt/hermes-agent/node/bin/npm" ]
[ "$(readlink /usr/local/bin/npx)" = "/opt/hermes-agent/node/bin/npx" ]
test -x /usr/local/bin/agent-browser
test -n "$(find /opt/hermes-agent/playwright -type f -name chrome -perm /111 -print -quit)"
agent-browser doctor --offline --quick >/dev/null
timeout 60 agent-browser --session subyard-e2e open about:blank >/dev/null
agent-browser --session subyard-e2e close >/dev/null
doctor_output="$(hermes doctor)"
printf "%s\n" "$doctor_output" \
  | grep -F "Playwright Chromium" | grep -F "(browser engine)" >/dev/null
! printf "%s\n" "$doctor_output" | grep -Fq "Playwright Chromium not installed"
hermes --version >/dev/null
'
  incus exec "$instance" --project "$project" -- codex-check >/dev/null
}

assert_hermes_browser_dispatch() { # <instance> <project> <task-id>
  local instance="$1" project="$2" task_id="$3"
  incus exec "$instance" --project "$project" \
    --user 1000 --group 1000 --cwd /srv/hermes/workspace \
    --env HOME=/home/dev --env HERMES_HOME=/srv/hermes -- \
    sh -euc '
. /etc/subyard/hermes-runtime.env
export PATH="$(dirname "$HERMES_NODE"):$HERMES_INSTALL_ROOT/agent-browser/bin:$PATH"
export HERMES_DISABLE_LAZY_INSTALLS=1
export PLAYWRIGHT_BROWSERS_PATH="$HERMES_PLAYWRIGHT_BROWSERS_PATH"
export AGENT_BROWSER_EXECUTABLE_PATH="$HERMES_BROWSER_EXECUTABLE"
exec "$HERMES_VENV/bin/python" - "$1" <<"PY"
import json
import sys

from tools import browser_tool
from tools.registry import registry

task_id = sys.argv[1]
definitions = registry.get_definitions({"browser_navigate"}, quiet=True)
assert [item["function"]["name"] for item in definitions] == ["browser_navigate"]
try:
    result = registry.dispatch(
        "browser_navigate", {"url": "about:blank"}, task_id=task_id
    )
    payload = json.loads(result)
    assert payload.get("success") is True, result
finally:
    browser_tool.cleanup_browser(task_id)
PY
' sh "$task_id"
}

install_codex_auth() { # <yard> <instance> <project>
  local name="$1" instance="$2" project="$3"
  [ -f "$AUTH_STAGE" ] && [ ! -L "$AUTH_STAGE" ] \
    || die "staged Codex auth is required"
  [ "$(stat -c %u "$AUTH_STAGE")" -eq "$(id -u dev)" ] \
    || die "staged Codex auth has the wrong owner"
  (( (8#$(stat -c %a "$AUTH_STAGE") & 077) == 0 )) \
    || die "staged Codex auth is not private"
  incus exec "$instance" --project "$project" -- \
    install -d -m 0700 -o 1000 -g 1000 /home/dev/.codex
  incus file push "$AUTH_STAGE" "$instance/home/dev/.codex/auth.json" \
    --project "$project" --quiet
  incus exec "$instance" --project "$project" -- \
    chown 1000:1000 /home/dev/.codex/auth.json
  incus exec "$instance" --project "$project" -- \
    chmod 0600 /home/dev/.codex/auth.json
  yard "$name" shell -- codex login status >/dev/null \
    || die "yard-local codex login status failed"
}

import_codex_auth() { # <instance> <project>
  local instance="$1" project="$2"
  incus exec "$instance" --project "$project" \
    --user 1000 --group 1000 \
    --env HOME=/home/dev --env HERMES_HOME=/srv/hermes \
    --env CODEX_HOME=/home/dev/.codex -- \
    /opt/hermes-agent/venv/bin/python -c '
from hermes_cli.auth import (
    DEFAULT_CODEX_BASE_URL,
    _import_codex_cli_tokens,
    _save_codex_tokens,
    _update_config_for_provider,
)
tokens = _import_codex_cli_tokens()
if not tokens:
    raise SystemExit("Codex CLI auth could not be imported")
_save_codex_tokens(tokens)
_update_config_for_provider(
    "openai-codex", DEFAULT_CODEX_BASE_URL, "gpt-5.5"
)
'
}

printf '  [ .. ] creating and provisioning source named yard\n'
yard "$SOURCE_YARD" init --profile hermes --yes
source_definition="$SUBYARD_CONFIG_HOME/yards/$SOURCE_YARD/config.env"
cmp "$ROOT/config/profiles/hermes/yard.env" "$source_definition" \
  || die "profile bootstrap did not persist the exact shipped preset"
[ "$(stat -c %a "$source_definition")" = 600 ] \
  || die "profile bootstrap did not create a mode-0600 yard definition"
for pair in \
  'ENVIRONMENT_PROFILES=hermes' 'CODING_TOOL_INTEGRATIONS=codex' \
  'HOST_CLAUDE_MD=' 'HOST_CODEX_AGENTS_MD=' 'HOST_OPENCODE_AGENTS_MD=' \
  'HOST_MOUNTS=' 'HOST_LINKS=' \
  'YARD_CAPABILITIES=' 'YARD_CAPS=' 'YARD_DEVICES=' 'YARD_MOUNTS=' \
  'FORWARD_SSH_AGENT=0' 'DEV_SUDO=0' 'NESTED_E2E_VMS=0'; do
  assert_setting "$SOURCE_YARD" "${pair%%=*}" "${pair#*=}"
done
set_yard_port "$SOURCE_YARD" "$source_ssh_port"
yard "$SOURCE_YARD" provision --yes
yard "$SOURCE_YARD" start --yes
source_instance="$(setting "$SOURCE_YARD" YARD_INSTANCE_NAME)"
source_project="$(setting "$SOURCE_YARD" INCUS_PROJECT)"
source_alias="$(setting "$SOURCE_YARD" SSH_HOST)"

[ "$(incus config get "$source_instance" boot.autostart \
  --project "$source_project")" = false ] || die "yard boot policy drifted"
[ "$(incus config get "$source_instance" user.subyard.name \
  --project "$source_project")" = "$SOURCE_YARD" ] || die "yard identity marker drifted"
assert_clean_yard_boundary "$source_instance" "$source_project"
assert_hermes_browser_dispatch "$source_instance" "$source_project" source-browser

source_token="$STATE/source-token.env"
incus file pull "$source_instance/srv/hermes/.serve.env" "$source_token" \
  --project "$source_project" --quiet
chmod 0600 "$source_token"
token_hash="$(sha256sum "$source_token" | awk '{print $1}')"

yard "$SOURCE_YARD" provision --yes
yard "$SOURCE_YARD" start --yes
incus file pull "$source_instance/srv/hermes/.serve.env" "$STATE/reprovision-token.env" \
  --project "$source_project" --quiet
[ "$(sha256sum "$STATE/reprovision-token.env" | awk '{print $1}')" = "$token_hash" ] \
  || die "re-provision rotated the dashboard token"
[ "$(incus exec "$source_instance" --project "$source_project" -- \
  systemctl is-enabled hermes-serve.service 2>/dev/null || true)" = disabled ] \
  || die "service was enabled before provider approval"

if [ "$REQUIRE_CODEX" = 1 ]; then
  install_codex_auth "$SOURCE_YARD" "$source_instance" "$source_project"
  import_codex_auth "$source_instance" "$source_project"

  printf '  [ .. ] running real Codex inference and terminal-tool proof in L1\n'
  incus exec "$source_instance" --project "$source_project" \
    --user 1000 --group 1000 --cwd /srv/hermes/workspace \
    --env HOME=/home/dev --env HERMES_HOME=/srv/hermes \
    --env HERMES_DISABLE_LAZY_INSTALLS=1 -- \
    /usr/local/bin/hermes -t terminal -z \
    'Use the terminal tool to create /srv/hermes/workspace/e2e-tool-marker with exact contents HERMES_E2E_TOOL_MARKER.'
  [ "$(incus exec "$source_instance" --project "$source_project" -- \
    cat /srv/hermes/workspace/e2e-tool-marker)" = HERMES_E2E_TOOL_MARKER ] \
    || die "terminal tool did not create the L1 marker"
else
  incus exec "$source_instance" --project "$source_project" \
    --user 1000 --group 1000 -- sh -euc \
    'printf "model:\n  provider: fixture\n  default: fixture\n" > /srv/hermes/config.yaml'
fi

yard "$SOURCE_YARD" shell --root -- hermes-provider-ready --inference-ok

api_ws_check() {
  local instance="$1" project="$2"
  incus exec "$instance" --project "$project" \
    --user 1000 --group 1000 --env HOME=/home/dev --env HERMES_HOME=/srv/hermes -- \
    /opt/hermes-agent/venv/bin/python -c '
import asyncio
import json
from pathlib import Path

import httpx
import websockets
from websockets.exceptions import InvalidStatus

token = Path("/srv/hermes/.serve.env").read_text().strip().split("=", 1)[1]
base = "http://127.0.0.1:9119"
status = httpx.get(base + "/api/status", timeout=5)
assert status.status_code == 200 and status.json()["version"] == "0.19.0"
assert httpx.get(base + "/api/config", timeout=5).status_code == 401
authorized = httpx.get(
    base + "/api/config",
    headers={"X-Hermes-Session-Token": token},
    timeout=5,
)
assert authorized.status_code == 200

async def check():
    try:
        async with websockets.connect("ws://127.0.0.1:9119/api/ws") as ws:
            await ws.recv()
    except InvalidStatus as exc:
        assert exc.response.status_code == 403
    else:
        raise AssertionError("unauthorized WebSocket was accepted")
    async with websockets.connect(
        "ws://127.0.0.1:9119/api/ws?token=" + token
    ) as ws:
        event = json.loads(await asyncio.wait_for(ws.recv(), 10))
        assert event["params"]["type"] == "gateway.ready"

asyncio.run(check())
'
}
api_ws_check "$source_instance" "$source_project"

guest_listener="$(incus exec "$source_instance" --project "$source_project" -- \
  ss -H -ltnp 'sport = :9119')"
printf '%s\n' "$guest_listener" | grep -Fq '127.0.0.1:9119' \
  || die "Hermes is not listening on guest loopback"
printf '%s\n' "$guest_listener" | grep -Eq '0\.0\.0\.0:9119|\[::\]:9119' \
  && die "Hermes exposed a non-loopback listener"
ss -H -ltn 'sport = :9119' | grep -q . \
  && die "owner side unexpectedly exposes port 9119"
guest_ip="$(incus list "$source_instance" --project "$source_project" -f csv -c 4 \
  | tr ',' '\n' | awk '/^[0-9]+\./ {print; exit}')"
[ -n "$guest_ip" ] || die "source yard has no IPv4 address"
if curl --noproxy '*' -fsS --connect-timeout 2 \
  "http://$guest_ip:9119/api/status" >/dev/null 2>&1; then
  die "guest port 9119 is reachable off-loopback"
fi

sudo -n apt-get update -qq
sudo -n apt-get install -y -qq restic python3-websockets >/dev/null

tunnel_port="$(next_port "$((42000 + ($$ % 10000)))")"
tunnel_check() {
  local alias="$1" token_file="$2"
  stop_tunnel
  ssh -NT \
    -o BatchMode=yes \
    -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=10 \
    -o ServerAliveCountMax=3 \
    -L "127.0.0.1:$tunnel_port:127.0.0.1:9119" \
    "$alias" &
  TUNNEL_PID=$!
  for _ in $(seq 1 30); do
    curl -fsS --max-time 2 "http://127.0.0.1:$tunnel_port/api/status" \
      >/dev/null 2>&1 && break
    sleep 1
  done
  curl -fsS --max-time 2 "http://127.0.0.1:$tunnel_port/api/status" \
    | python3 -c 'import json,sys; assert json.load(sys.stdin)["version"] == "0.19.0"'
  python3 - "$tunnel_port" "$token_file" <<'PY'
import asyncio
import json
from pathlib import Path
import sys

import websockets

port = int(sys.argv[1])
token = Path(sys.argv[2]).read_text().strip().split("=", 1)[1]

async def check():
    async with websockets.connect(
        f"ws://127.0.0.1:{port}/api/ws?token={token}"
    ) as ws:
        event = json.loads(await asyncio.wait_for(ws.recv(), 10))
        assert event["params"]["type"] == "gateway.ready"

asyncio.run(check())
PY
}

tunnel_check "$source_alias" "$source_token"
stop_tunnel
for _ in $(seq 1 20); do
  ! ss -H -ltn "sport = :$tunnel_port" | grep -q . && break
  sleep 0.2
done
ss -H -ltn "sport = :$tunnel_port" | grep -q . \
  && die "closing the SSH tunnel did not close local access"
tunnel_check "$source_alias" "$source_token"
stop_tunnel

incus exec "$source_instance" --project "$source_project" \
  --user 1000 --group 1000 -- sh -euc '
install -d -m 0700 \
  /srv/hermes/sessions \
  /srv/hermes/memory \
  /srv/hermes/skills/e2e \
  /srv/hermes/cron \
  /srv/hermes/kanban
printf "session-e2e\n" > /srv/hermes/sessions/e2e-session.txt
printf "memory-e2e\n" > /srv/hermes/memory/e2e-memory.md
printf "# E2E skill\n" > /srv/hermes/skills/e2e/SKILL.md
printf "{\"jobs\":[]}\n" > /srv/hermes/cron/jobs.json
printf "kanban-e2e\n" > /srv/hermes/kanban/e2e-state.txt
'

restic_env="$STATE/restic.env"
restic_repo="$STATE/restic-repository"
restic_password="$STATE/restic-password"
sudo -n install -d -m 0700 "$restic_repo"
openssl rand -hex 32 > "$STATE/password.tmp"
sudo -n install -m 0600 -o root -g root "$STATE/password.tmp" "$restic_password"
rm -f "$STATE/password.tmp"
{
  printf 'RESTIC_REPOSITORY=%q\n' "$restic_repo"
  printf 'RESTIC_PASSWORD_FILE=%q\n' "$restic_password"
} > "$STATE/restic.tmp"
sudo -n install -m 0600 -o root -g root "$STATE/restic.tmp" "$restic_env"
rm -f "$STATE/restic.tmp"
sudo -n bash -euc 'set -a; . "$1"; set +a; restic init >/dev/null' bash "$restic_env"

printf '  [ .. ] creating real stopped-service Hermes backup in encrypted restic\n'
backup_output="$(
  sudo -n --preserve-env=SUBYARD_CONFIG_HOME,SUBYARD_HOME,STORAGE_PATH \
    "$ROOT/config/profiles/hermes/backup-to-restic.sh" \
    --yard "$SOURCE_YARD" --restic-env "$restic_env" --type scheduled
)"
snapshot="$(printf '%s\n' "$backup_output" | sed -n 's/.*snapshot=\([^ ]*\).*/\1/p')"
backup_sha="$(printf '%s\n' "$backup_output" | sed -n 's/.*sha256=\([0-9a-f]*\).*/\1/p')"
[[ "$snapshot" =~ ^[0-9a-f]+$ ]] || die "backup omitted a restic snapshot ID"
[[ "$backup_sha" =~ ^[0-9a-f]{64}$ ]] || die "backup omitted SHA-256"
restic_listing="$STATE/restic-listing"
sudo -n bash -euc \
  'set -a; . "$1"; set +a; umask 077; restic check >/dev/null; restic ls "$2" > "$3"; grep -Fxq /hermes-backup.zip "$3"' \
  bash "$restic_env" "$snapshot" "$restic_listing"

restore_extract="$STATE/restic-restore"
sudo -n install -d -m 0700 "$restore_extract"
sudo -n bash -euc \
  'set -a; . "$1"; set +a; restic restore "$2" --target "$3" >/dev/null' \
  bash "$restic_env" "$snapshot" "$restore_extract"
restored_zip="$(sudo -n find "$restore_extract" -type f -name hermes-backup.zip -print -quit)"
[ -n "$restored_zip" ] || die "restic restore omitted the Hermes archive"
restored_stage="$STATE/restored-hermes-backup.zip"
sudo -n install -m 0600 -o "$(id -u)" -g "$(id -g)" \
  "$restored_zip" "$restored_stage"

printf '  [ .. ] provisioning clean same-commit restore yard\n'
yard "$RESTORE_YARD" init --profile hermes --yes
set_yard_port "$RESTORE_YARD" "$restore_ssh_port"
yard "$RESTORE_YARD" provision --yes
yard "$RESTORE_YARD" start --yes
restore_instance="$(setting "$RESTORE_YARD" YARD_INSTANCE_NAME)"
restore_project="$(setting "$RESTORE_YARD" INCUS_PROJECT)"
restore_alias="$(setting "$RESTORE_YARD" SSH_HOST)"
assert_clean_yard_boundary "$restore_instance" "$restore_project"
incus file push "$restored_stage" "$restore_instance/var/tmp/hermes-backup.zip" \
  --project "$restore_project" --quiet
incus exec "$restore_instance" --project "$restore_project" -- \
  chmod 0600 /var/tmp/hermes-backup.zip
incus exec "$restore_instance" --project "$restore_project" -- \
  hermes-restore /var/tmp/hermes-backup.zip "$backup_sha"

[ "$(incus exec "$restore_instance" --project "$restore_project" -- \
  systemctl is-enabled hermes-serve.service 2>/dev/null || true)" = disabled ] \
  || die "restore enabled the service before provider re-validation"
if incus exec "$restore_instance" --project "$restore_project" -- \
  test -e /srv/hermes/.provider-ready; then
  die "restore retained provider approval"
fi
incus file pull "$restore_instance/srv/hermes/.serve.env" "$STATE/restored-token.env" \
  --project "$restore_project" --quiet
[ "$(sha256sum "$STATE/restored-token.env" | awk '{print $1}')" = "$token_hash" ] \
  || die "restore changed the stable dashboard token"

for path in \
  sessions/e2e-session.txt \
  memory/e2e-memory.md \
  skills/e2e/SKILL.md \
  cron/jobs.json \
  kanban/e2e-state.txt; do
  incus exec "$restore_instance" --project "$restore_project" -- \
    test -s "/srv/hermes/$path" || die "restore omitted $path"
done
assert_hermes_browser_dispatch "$restore_instance" "$restore_project" restored-browser

if [ "$REQUIRE_CODEX" = 1 ]; then
  printf '  [ .. ] re-authorizing Codex after restore and proving real inference\n'
  install_codex_auth "$RESTORE_YARD" "$restore_instance" "$restore_project"
  import_codex_auth "$restore_instance" "$restore_project"
  incus exec "$restore_instance" --project "$restore_project" \
    --user 1000 --group 1000 --cwd /srv/hermes/workspace \
    --env HOME=/home/dev --env HERMES_HOME=/srv/hermes \
    --env HERMES_DISABLE_LAZY_INSTALLS=1 -- \
    /usr/local/bin/hermes -t terminal -z \
    'Use the terminal tool to create /srv/hermes/workspace/e2e-restored-tool-marker with exact contents HERMES_RESTORE_TOOL_MARKER.'
  [ "$(incus exec "$restore_instance" --project "$restore_project" -- \
    cat /srv/hermes/workspace/e2e-restored-tool-marker)" = HERMES_RESTORE_TOOL_MARKER ] \
    || die "restored Hermes terminal tool did not create the L1 marker"
fi

yard "$RESTORE_YARD" shell --root -- hermes-provider-ready --inference-ok
api_ws_check "$restore_instance" "$restore_project"
tunnel_check "$restore_alias" "$STATE/restored-token.env"
stop_tunnel

if [ "$REQUIRE_CODEX" = 1 ]; then
  provider_proof="real Codex inference"
else
  provider_proof="fixture provider"
fi
printf '  [ ok ] named-yard isolation, %s, backend auth, SSH tunnel, restic backup and clean restore verified\n' \
  "$provider_proof"

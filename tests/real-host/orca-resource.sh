#!/usr/bin/env bash
# Real-host acceptance for the minimal Orca resource. Disposable E2E VM only.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=config/profiles/orca/release.env
. "$ROOT/config/profiles/orca/release.env"
# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"

[ "${SUBYARD_E2E_ORCA_RESOURCE:-}" = 1 ] || {
  printf 'orca-resource: set SUBYARD_E2E_ORCA_RESOURCE=1 inside a disposable test host\n' >&2
  exit 2
}

project=subyard-orca-resource
instance=orca-resource-l1
network=orca-net
storage=orca-pool
host_port=17679
incus=(sudo incus --project "$project")
work="$(mktemp -d)"
fakebin="$work/bin"
artifact="$work/orca.deb"
mkdir -p "$fakebin"

stage() { printf 'orca-resource: %s\n' "$1" >&2; }
die() { printf 'orca-resource: %s\n' "$1" >&2; exit 1; }

cleanup() {
  "${incus[@]}" delete -f "$instance" >/dev/null 2>&1 || true
  "${incus[@]}" network delete "$network" >/dev/null 2>&1 || true
  sudo incus project delete "$project" >/dev/null 2>&1 || true
  sudo incus storage delete "$storage" >/dev/null 2>&1 || true
  rm -rf -- "$work"
}
trap cleanup EXIT

sudo incus project show "$project" >/dev/null 2>&1 && die "refusing to reuse project $project"

case "$(dpkg --print-architecture)" in
  amd64) url="$ORCA_DEB_AMD64_URL"; digest="$ORCA_DEB_AMD64_SHA256" ;;
  arm64) url="$ORCA_DEB_ARM64_URL"; digest="$ORCA_DEB_ARM64_SHA256" ;;
  *) die 'unsupported architecture' ;;
esac
cache="/var/tmp/subyard-orca-$ORCA_VERSION-$digest.deb"
if printf '%s  %s\n' "$digest" "$cache" | sha256sum -c --status 2>/dev/null; then
  cp "$cache" "$artifact"
else
  stage 'downloading the pinned deb'
  curl --proto '=https' --tlsv1.2 -fsSL \
    --retry 3 --retry-all-errors --connect-timeout 20 --max-time 1200 \
    "$url" -o "$artifact"
  printf '%s  %s\n' "$digest" "$artifact" | sha256sum -c -
  cp "$artifact" "$cache"
fi
stage 'preparing an independent stock Orca CLI client'
sudo apt-get update -qq
sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  "$artifact" file jq nftables python3-websocket xauth zlib1g-dev \
  libasound2t64 libgbm1 libgtk-3-0t64 libnss3 >/dev/null
client=/usr/bin/orca-ide
test -x "$client"

stage 'launching the nested yard'
sudo incus storage show "$storage" >/dev/null 2>&1 &&
  die "refusing to reuse storage pool $storage"
sudo incus storage create "$storage" dir >/dev/null
sudo incus project create "$project" \
  -c features.images=false -c features.profiles=false >/dev/null
"${incus[@]}" network create "$network" \
  ipv4.address=auto ipv4.nat=true ipv6.address=none >/dev/null
"${incus[@]}" launch images:debian/13/cloud "$instance" \
  --network "$network" --storage "$storage" >/dev/null
for _ in $(seq 1 90); do
  "${incus[@]}" exec "$instance" -- true >/dev/null 2>&1 && break
  sleep 1
done
"${incus[@]}" exec "$instance" -- true >/dev/null 2>&1 || die 'nested yard did not start'
"${incus[@]}" exec "$instance" -- bash -se <<'YARD'
set -euo pipefail
id dev >/dev/null 2>&1 || useradd --create-home --shell /bin/bash dev
command -v git >/dev/null 2>&1 || {
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git >/dev/null
}
for id in alpha-12345678 beta-12345678; do
  root="/srv/workspaces/$id"
  install -d -o dev -g dev "$root/src"
  printf '{"schema":1,"projectId":"%s","name":"%s","mode":"sync"}\n' \
    "$id" "$id" >"$root/.subyard-meta.json"
  chown dev:dev "$root/.subyard-meta.json"
  runuser -u dev -- git -C "$root/src" init -q
done
install -d -m 0755 /etc/subyard /usr/local/libexec/subyard/projects-changed.d
: > /etc/subyard/agent-project-hooks
YARD
"${incus[@]}" file push "$ROOT/config/projects-changed.sh" \
  "$instance/usr/local/libexec/subyard/projects-changed" --mode 0755
if "${incus[@]}" exec "$instance" -- command -v tailscale >/dev/null 2>&1; then
  die 'Tailscale unexpectedly exists inside the yard'
fi

owner_ip="$(ip -4 -brief address show scope global |
  awk 'NR==1{sub(/\/.*/, "", $3); print $3}')"
[ -n "$owner_ip" ] || die 'owner has no global IPv4 address'
advertise="$owner_ip"

cat >"$fakebin/incus" <<'MOCK'
#!/usr/bin/env bash
exec sudo /usr/bin/incus "$@"
MOCK
cat >"$fakebin/tailscale" <<'MOCK'
#!/usr/bin/env bash
state_root="$(cd "$(dirname "$0")/.." && pwd)"
[ "${1:-} ${2:-}" = 'ip -4' ] && cat "$state_root/owner-ip"
MOCK
cat >"$fakebin/getent" <<'MOCK'
#!/usr/bin/env bash
state_root="$(cd "$(dirname "$0")/.." && pwd)"
owner_ip="$(cat "$state_root/owner-ip")"
advertise="$(cat "$state_root/advertise")"
if [ "${1:-} ${2:-}" = "ahostsv4 $advertise" ]; then
  printf '%s STREAM %s\n' "$owner_ip" "$advertise"
else
  exec /usr/bin/getent "$@"
fi
MOCK
cat >"$fakebin/curl" <<'MOCK'
#!/usr/bin/env bash
state_root="$(cd "$(dirname "$0")/.." && pwd)"
destination=
release=0
arguments=("$@")
for ((index=0; index < ${#arguments[@]}; index++)); do
  case "${arguments[$index]}" in
    -o|--output)
      index=$((index + 1))
      destination="${arguments[$index]}"
      ;;
    https://github.com/stablyai/orca/releases/*) release=1 ;;
  esac
done
if [ "$release" = 1 ]; then
  [ -n "$destination" ] || exit 2
  cp "$state_root/orca.deb" "$destination"
  exit 0
fi
exec /usr/bin/curl "$@"
MOCK
chmod 0755 "$fakebin/"*
printf '%s\n' "$owner_ip" >"$work/owner-ip"
printf '%s\n' "$advertise" >"$work/advertise"

setup_test_context "$work/context" "$project" "$instance"
export PATH="$fakebin:$PATH"
export ORCA_ADVERTISE_HOST="$advertise" ORCA_HOST_PORT="$host_port" ASSUME_YES=1
export YARD_ENGINE_PATH="$ROOT/.build/yard"
bash "$ROOT/dev/build-engine.sh" >/dev/null

run_orca() {
  # This fixture builds its own minimal Incus instance. Exercise the physical
  # resource adapter; orca-bootstrap/orca-projects cover the complete native CLI.
  local assessment action
  assessment="$(SUBYARD_RESOURCE_MODE=prepare \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" "$@")" || return
  action="$(jq -er '.action' <<<"$assessment")" || return
  SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION="$action" \
    SUBYARD_OPERATION_ID=orca-resource-acceptance \
    "$ROOT/config/profiles/orca/resources/orca/handler.sh" "$@"
}

server_cli() {
  "${incus[@]}" exec "$instance" -- runuser -u dev -- env \
    HOME=/home/dev \
    XDG_CONFIG_HOME=/srv/agents/orca/config \
    XDG_DATA_HOME=/srv/agents/orca/data \
    XDG_STATE_HOME=/srv/agents/orca/state \
    /usr/bin/orca-ide "$@"
}

client_status() {
  local pairing="$1" profile="$2"
  local output="$work/$profile-status.json"
  local error="$work/$profile-status.err"
  if ! client_cli "$pairing" "$profile" status --json >"$output" 2>"$error"; then
    sed -n '1,80p' "$error" >&2
    die "$profile stock client command failed"
  fi
  if ! jq -e '.ok == true and .result.runtime.reachable == true' "$output" >/dev/null; then
    jq -c '{ok, result: {app: .result.app, runtime: .result.runtime, graph: .result.graph}}' \
      "$output" >&2 || true
    die "$profile did not reach the paired runtime"
  fi
}

client_cli() {
  local pairing="$1" profile="$2"
  shift 2
  mkdir -p "$work/$profile/home" "$work/$profile/config" "$work/$profile/data" "$work/$profile/state"
  HOME="$work/$profile/home" \
    XDG_CONFIG_HOME="$work/$profile/config" \
    XDG_DATA_HOME="$work/$profile/data" \
    XDG_STATE_HOME="$work/$profile/state" \
    LIBGL_ALWAYS_SOFTWARE=1 \
    ORCA_PAIRING_CODE="$pairing" \
    timeout 30 "$client" "$@"
}

assert_paired_terminal_io() {
  local pairing="$1" profile="$2" repos create handle='' ready=0 send read_result close
  local error rc deadline echoed=0
  repos="$work/$profile-terminal-repos.json"
  create="$work/$profile-terminal-create.json"
  send="$work/$profile-terminal-send.json"
  read_result="$work/$profile-terminal-read.json"
  close="$work/$profile-terminal-close.json"

  terminal_diagnostic() {
    local stage="$1" exit_status="$2" output="$3" summary
    summary="$(jq -r '
      "ok=\(.ok == true)" +
      " terminal_running=\(.result.terminal.status? == "running")" +
      " terminal_exited=\(.result.terminal.status? == "exited")" +
      " send_accepted=\(.result.send.accepted? == true)" +
      " close_tab=\(.result.close.closeMode? == "tab")" +
      " pty_killed=\(.result.close.ptyKilled? == true)"
    ' "$output" 2>/dev/null)" || summary='structured_status=unavailable'
    printf 'orca-resource: paired terminal %s failed (exit=%s %s)\n' \
      "$stage" "$exit_status" "$summary" >&2
  }

  terminal_fail() {
    local stage="$1" exit_status="$2" output="$3"
    terminal_diagnostic "$stage" "$exit_status" "$output"
    if [ -n "$handle" ]; then
      client_cli "$pairing" "$profile" terminal close --terminal "$handle" --tab --json \
        >"$close" 2>"$work/$profile-terminal-cleanup.err" || true
    fi
    die "paired terminal $stage failed"
  }

  error="$work/$profile-terminal-repos.err"
  if client_cli "$pairing" "$profile" repo list --json >"$repos" 2>"$error"; then
    :
  else
    rc=$?
    terminal_fail 'repository selection' "$rc" "$repos"
  fi
  local repo_id repo_path selector
  repo_path=/srv/workspaces/alpha-12345678/src
  repo_id="$(jq -er --arg path "$repo_path" \
    '.result.repos | map(select(.path == $path)) | select(length == 1) | .[0].id' "$repos")" \
    || terminal_fail 'repository selection' 0 "$repos"
  selector="id:$repo_id::$repo_path"

  error="$work/$profile-terminal-create.err"
  if client_cli "$pairing" "$profile" terminal create --worktree "$selector" \
    --title subyard-terminal-e2e \
    --command "printf '%s%s\\n' 'subyard-terminal-' 'ready'; IFS= read -r value; printf '%s%s%s\\n' 'subyard-terminal-' 'echo:' \"\$value\"" \
    --json >"$create" 2>"$error"; then
    :
  else
    rc=$?
    terminal_fail 'create' "$rc" "$create"
  fi
  handle="$(jq -er 'select(.ok == true) | .result.terminal.handle |
    select(type == "string" and length > 0)' "$create")" \
    || terminal_fail 'create result' 0 "$create"

  deadline=$((SECONDS + 15))
  while ((SECONDS < deadline)); do
    error="$work/$profile-terminal-read-ready.err"
    if client_cli "$pairing" "$profile" terminal read --terminal "$handle" --json \
      >"$read_result" 2>"$error" \
      && jq -e '.ok == true and
        (.result.terminal.tail | any(contains("subyard-terminal-ready")))' \
        "$read_result" >/dev/null; then
      ready=1
      break
    fi
    sleep 0.1
  done
  [ "$ready" -eq 1 ] || terminal_fail 'readiness poll' 0 "$read_result"

  error="$work/$profile-terminal-send.err"
  if client_cli "$pairing" "$profile" terminal send --terminal "$handle" \
    --text subyard-terminal-input --enter --json >"$send" 2>"$error"; then
    :
  else
    rc=$?
    terminal_fail 'send' "$rc" "$send"
  fi
  jq -e '.ok == true and .result.send.accepted == true and .result.send.bytesWritten > 0' \
    "$send" >/dev/null || terminal_fail 'send result' 0 "$send"

  deadline=$((SECONDS + 15))
  while ((SECONDS < deadline)); do
    error="$work/$profile-terminal-read-echo.err"
    if client_cli "$pairing" "$profile" terminal read --terminal "$handle" --json \
      >"$read_result" 2>"$error" \
      && jq -e '.ok == true and
        (.result.terminal.tail | any(contains("subyard-terminal-echo:subyard-terminal-input")))' \
        "$read_result" >/dev/null; then
      echoed=1
      break
    fi
    sleep 0.1
  done
  [ "$echoed" -eq 1 ] || terminal_fail 'output poll' 0 "$read_result"

  error="$work/$profile-terminal-close.err"
  if client_cli "$pairing" "$profile" terminal close --terminal "$handle" --tab --json \
    >"$close" 2>"$error"; then
    :
  else
    rc=$?
    terminal_fail 'close' "$rc" "$close"
  fi
  jq -e '.ok == true and .result.close.closeMode == "tab"' "$close" >/dev/null \
    || terminal_fail 'close result' 0 "$close"
}

assert_paired_desktop_smoke() {
  local pairing="$1" desktop_file desktop_exec gui_root probe pairing_file log
  local -a desktop_files=()
  mapfile -t desktop_files < <(dpkg-query -L orca-ide | \
    awk '/^\/usr\/share\/applications\/[^/]+\.desktop$/ {print}')
  [ "${#desktop_files[@]}" -eq 1 ] || die 'pinned Orca package did not install one desktop entry'
  desktop_file="${desktop_files[0]}"
  desktop_exec="$(python3 -B - "$desktop_file" <<'PY'
import shlex
import sys

values = []
in_desktop_entry = False
with open(sys.argv[1], encoding="utf-8") as source:
    for raw in source:
        if raw.startswith("["):
            in_desktop_entry = raw.strip() == "[Desktop Entry]"
        elif in_desktop_entry and raw.startswith("Exec="):
            values.append(shlex.split(raw.removeprefix("Exec=").strip()))
if len(values) != 1 or not values[0] or not values[0][0].startswith("/"):
    raise SystemExit(1)
print(values[0][0])
PY
)" || die 'pinned Orca desktop entry has no exact absolute executable'
  [ -x "$desktop_exec" ] || die 'pinned Orca desktop executable is unavailable'
  command -v Xvfb >/dev/null 2>&1 || die 'pinned Orca package did not install Xvfb'
  command -v xdotool >/dev/null 2>&1 || die 'pinned Orca package did not install xdotool'

  gui_root="$work/desktop-smoke"
  install -d -m 0700 "$gui_root/home" "$gui_root/config" "$gui_root/data" "$gui_root/state"
  pairing_file="$gui_root/pairing"
  log="$gui_root/desktop.log"
  install -m 0600 /dev/null "$pairing_file"
  install -m 0600 /dev/null "$log"
  printf '%s\n' "$pairing" >"$pairing_file"
  probe="$gui_root/probe.sh"
  cat >"$probe" <<'PROBE'
#!/usr/bin/env bash
set -euo pipefail
desktop_exec=$1
log=$2
pairing_file=$3
driver=$4

group_live() {
  ps -eo pgid=,stat= | awk -v group="$app_pid" \
    '$1 == group && $2 !~ /^Z/ {found=1} END {exit found ? 0 : 1}'
}
stop_group() {
  kill -TERM -- "-$app_pid" 2>/dev/null || true
  for _ in $(seq 1 30); do
    group_live || { wait "$app_pid" 2>/dev/null || true; return 0; }
    sleep 0.1
  done
  kill -KILL -- "-$app_pid" 2>/dev/null || true
  wait "$app_pid" 2>/dev/null || true
  ! group_live
}

setsid "$desktop_exec" \
  --remote-debugging-address=127.0.0.1 --remote-debugging-port=0 >"$log" 2>&1 &
app_pid=$!
trap 'stop_group || true' EXIT INT TERM
visible=0
window=''
for _ in $(seq 1 300); do
  kill -0 "$app_pid" 2>/dev/null || exit 1
  window="$(xdotool search --onlyvisible --class '^orca$' 2>/dev/null | head -n 1)" || true
  if [ -n "$window" ]; then
    visible=1
    break
  fi
  sleep 0.1
done
[ "$visible" -eq 1 ] || exit 1
python3 -B "$driver" --log "$log" --pairing-file "$pairing_file" \
  --expected-repo-label alpha-12345678 \
  --expected-repo-path /srv/workspaces/alpha-12345678/src
for _ in $(seq 1 300); do
  kill -0 "$app_pid" 2>/dev/null || break
  sleep 0.1
done
! kill -0 "$app_pid" 2>/dev/null || exit 1
wait "$app_pid"
! group_live
trap - EXIT INT TERM
PROBE
  chmod 0700 "$probe"
  HOME="$gui_root/home" XDG_CONFIG_HOME="$gui_root/config" \
    XDG_DATA_HOME="$gui_root/data" XDG_STATE_HOME="$gui_root/state" \
    LIBGL_ALWAYS_SOFTWARE=1 NO_AT_BRIDGE=1 \
    timeout --signal=TERM --kill-after=5s 90 \
    xvfb-run -a -s '-screen 0 1280x800x24' \
    "$probe" "$desktop_exec" "$log" "$pairing_file" "$ROOT/tests/helpers/orca-desktop.py" \
    || die 'pinned Orca desktop did not pair, render the remote project, and close cleanly'
}

retarget_pairing() {
  local pairing="$1" endpoint="$2" code payload
  code="${pairing#orca://pair?code=}"
  case $((${#code} % 4)) in
    0) ;;
    2) code="${code}==" ;;
    3) code="${code}=" ;;
    *) die 'stock client pairing payload has invalid base64url length' ;;
  esac
  payload="$(printf '%s' "$code" | tr -- '_-' '/+' | base64 -d)"
  printf 'orca://pair?code='
  jq -c --arg endpoint "$endpoint" '.endpoint = $endpoint' <<<"$payload" |
    base64 -w 0 | tr -- '+/' '-_' | tr -d '='
  printf '\n'
}

assert_repos() {
  local payload="$work/repos.json"
  server_cli repo list --json >"$payload"
  jq -e '
    [.result.repos[].path] as $paths |
    ($paths | index("/srv/workspaces/alpha-12345678/src")) != null and
    ($paths | index("/srv/workspaces/beta-12345678/src")) != null
  ' "$payload" >/dev/null
}

assert_all_repos() {
  local payload="$work/all-repos.json"
  server_cli repo list --json >"$payload"
  jq -e '
    [.result.repos[].path] as $paths |
    ($paths | index("/srv/workspaces/alpha-12345678/src")) != null and
    ($paths | index("/srv/workspaces/beta-12345678/src")) != null and
    ($paths | index("/srv/workspaces/gamma-12345678/src")) != null
  ' "$payload" >/dev/null
}

assert_no_pairing_journal() {
  local logs
  logs="$("${incus[@]}" exec "$instance" -- \
    journalctl -u subyard-orca.service --no-pager)"
  case "$logs" in
    *orca://*|*'"url":"orca:'*) die 'service journal leaked a pairing capability' ;;
  esac
}

stage 'installing and starting the production handler'
run_orca up
"${incus[@]}" exec "$instance" -- systemctl is-active --quiet subyard-orca.service
"${incus[@]}" exec "$instance" -- test -x /usr/bin/orca-ide
[ "$("${incus[@]}" exec "$instance" -- dpkg-query -W -f='${Version}' orca-ide)" = \
  "$ORCA_VERSION" ] || die 'nested yard did not install the pinned deb'
"${incus[@]}" exec "$instance" -- nft list chain inet subyard_orca input |
  grep -Fq 'comment "subyard-orca-managed"'
[ "$("${incus[@]}" exec "$instance" -- stat -c %a /srv/agents/orca/ready.json)" = 600 ] \
  || die 'pairing readiness file is not mode 0600'
assert_no_pairing_journal
assert_repos

stage 'pairing two independent clients and reconnecting the first'
first_pair="$("${incus[@]}" exec "$instance" -- \
  jq -er '.pairing | select(.available == true) | .url' /srv/agents/orca/ready.json)"
client_status "$first_pair" client-a
stage 'exercising terminal input and output through the paired stock client'
assert_paired_terminal_io "$first_pair" client-a
"${incus[@]}" exec "$instance" -- bash -se <<'YARD'
id=gamma-12345678
root="/srv/workspaces/$id"
install -d -o dev -g dev "$root/src"
printf '{"schema":1,"projectId":"%s","name":"%s","mode":"sync"}\n' \
  "$id" "$id" >"$root/.subyard-meta.json"
chown dev:dev "$root/.subyard-meta.json"
runuser -u dev -- git -C "$root/src" init -q
YARD
second_pair="$(run_orca pair | tail -n1)"
[ "$first_pair" != "$second_pair" ] || die 'pair restart reused the old offer'
client_status "$second_pair" client-b
client_status "$first_pair" client-a
assert_all_repos
ENVIRONMENT_PROFILES=orca run_orca status >"$work/status.out"
grep -Fq 'Orca profile selected for yard init' "$work/status.out" \
  || die 'status did not confirm the selected Orca profile'
grep -Fq 'automatic project hook ready' "$work/status.out" \
  || die 'status did not confirm automatic project hook readiness'
grep -Fq 'checkouts registered: 3/3' "$work/status.out" \
  || die 'status did not report canonical project registration counts'
restart_output="$(run_orca restart)"
case "$restart_output" in
  *orca://*) die 'restart returned a pairing capability' ;;
esac
client_status "$first_pair" client-a
client_status "$second_pair" client-b
assert_all_repos

stage 'checking bounded and explicit-follow journal access'
run_orca logs >"$work/logs.out"
[ "$(wc -l <"$work/logs.out")" -le 18000 ] \
  || die 'bounded logs returned more than 18000 lines'
case "$(cat "$work/logs.out")" in
  *orca://*) die 'bounded logs returned a pairing capability' ;;
esac
set +e
timeout --signal=TERM --kill-after=2s 3s \
  env SUBYARD_RESOURCE_MODE=apply SUBYARD_RESOURCE_ACTION=logs \
  SUBYARD_OPERATION_ID=orca-resource-acceptance \
  "$ROOT/config/profiles/orca/resources/orca/handler.sh" logs --follow >"$work/logs-follow.out" 2>&1
follow_status=$?
set -e
[ "$follow_status" -eq 124 ] || die "logs --follow exited with status $follow_status before timeout"
case "$(cat "$work/logs-follow.out")" in
  *orca://*) die 'followed logs returned a pairing capability' ;;
esac

stage 'adding a project through stock repo sync'
"${incus[@]}" exec "$instance" -- bash -se <<'YARD'
id=delta-12345678
root="/srv/workspaces/$id"
install -d -o dev -g dev "$root/src"
printf '{"schema":1,"projectId":"%s","name":"%s","mode":"sync"}\n' \
  "$id" "$id" >"$root/.subyard-meta.json"
chown dev:dev "$root/.subyard-meta.json"
runuser -u dev -- git -C "$root/src" init -q
YARD
run_orca sync >/dev/null
server_cli repo list --json |
  jq -e '.result.repos | any(.path == "/srv/workspaces/delta-12345678/src")' >/dev/null

stage 'verifying exact owner route and SSH-loopback mode'
[ "$("${incus[@]}" config device get "$instance" orca-server listen)" = \
  "tcp:$owner_ip:$host_port" ] || die 'Tailscale route is not exact-address'
run_orca down
export ORCA_ADVERTISE_HOST=127.0.0.1
run_orca up
[ "$("${incus[@]}" config device get "$instance" orca-server listen)" = \
  "tcp:127.0.0.1:$host_port" ] || die 'SSH route is not owner loopback'
"${incus[@]}" exec "$instance" -- jq -e \
  --arg endpoint "ws://127.0.0.1:$host_port" \
  '.type == "orca_server_ready" and
   .schemaVersion == 1 and
   .advertisedEndpoint == $endpoint and
   .pairing.available == true and
   (.pairing.url | type == "string" and startswith("orca://pair?"))' \
  /srv/agents/orca/ready.json >/dev/null
retargeted_first_pair="$(retarget_pairing "$first_pair" "ws://127.0.0.1:$host_port")"
client_status "$retargeted_first_pair" client-a
assert_all_repos
assert_no_pairing_journal

stage 'pairing and closing the pinned desktop under Xvfb'
desktop_pair="$(run_orca pair | tail -n1)"
case "$desktop_pair" in
  orca://pair\?code=*) ;;
  *) die 'Orca pair did not return a private stock pairing link for the desktop' ;;
esac
assert_paired_desktop_smoke "$desktop_pair"

run_orca down
if "${incus[@]}" config device list "$instance" | grep -qx orca-server; then
  die 'owned proxy survived down'
fi
"${incus[@]}" exec "$instance" -- test -d /srv/agents/orca/state
if "${incus[@]}" exec "$instance" -- nft list table inet subyard_orca >/dev/null 2>&1; then
  die 'owned ingress table survived down'
fi

printf 'ok: stock Orca preserved grants/repos, exchanged terminal I/O, repeated exact routes, and paired its desktop\n'

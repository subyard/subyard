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
  "$artifact" file jq nftables zlib1g-dev \
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
cat > /usr/local/libexec/subyard/projects-changed <<'DISPATCH'
#!/usr/bin/env bash
set -euo pipefail
status=0
for hook in /usr/local/libexec/subyard/projects-changed.d/*; do
  [ -x "$hook" ] || continue
  "$hook" || status=1
done
while IFS= read -r hook; do
  [ -n "$hook" ] || continue
  "$hook" || status=1
done < /etc/subyard/agent-project-hooks
exit "$status"
DISPATCH
chmod 0755 /usr/local/libexec/subyard/projects-changed
YARD
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
  "$ROOT/bin/yard" orca "$@" --yes
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
  mkdir -p "$work/$profile/home" "$work/$profile/config" "$work/$profile/data" "$work/$profile/state"
  if ! HOME="$work/$profile/home" \
    XDG_CONFIG_HOME="$work/$profile/config" \
    XDG_DATA_HOME="$work/$profile/data" \
    XDG_STATE_HOME="$work/$profile/state" \
    LIBGL_ALWAYS_SOFTWARE=1 \
    timeout 30 "$client" \
    --pairing-code "$pairing" status --json >"$output" 2>"$error"; then
    sed -n '1,80p' "$error" >&2
    die "$profile stock client command failed"
  fi
  if ! jq -e '.ok == true and .result.runtime.reachable == true' "$output" >/dev/null; then
    jq -c '{ok, result: {app: .result.app, runtime: .result.runtime, graph: .result.graph}}' \
      "$output" >&2 || true
    die "$profile did not reach the paired runtime"
  fi
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
grep -Fq 'projects registered: 3/3' "$work/status.out" \
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
  "$ROOT/bin/yard" orca logs --follow --yes >"$work/logs-follow.out" 2>&1
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
run_orca down

if "${incus[@]}" config device list "$instance" | grep -qx orca-server; then
  die 'owned proxy survived down'
fi
"${incus[@]}" exec "$instance" -- test -d /srv/agents/orca/state
if "${incus[@]}" exec "$instance" -- nft list table inet subyard_orca >/dev/null 2>&1; then
  die 'owned ingress table survived down'
fi

printf 'ok: stock Orca reconnected clients, preserved grants/repos, and repeated exact host routes\n'

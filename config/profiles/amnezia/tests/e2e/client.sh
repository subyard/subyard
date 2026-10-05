#!/usr/bin/env bash
# Disposable VM2 client proof for the profile-owned AmneziaWG endpoint.
# The native application exports each client; credentials are never printed here.
set -euo pipefail

CONFIG_DIR=/var/tmp/subyard-amnezia-client
MARKER=subyard-amnezia-e2e-client-v1

die() { printf 'amnezia-client: %s\n' "$*" >&2; exit 1; }
usage() {
  printf 'Usage: %s probe --owner-ip IPV4 --owner-port PORT [--private-ip IPV4] [--client one|two]\n       %s denied|reboot-traffic|cleanup [--client one|two]\n' "$0" "$0"
}
[ "${SUBYARD_E2E_VM:-}" = 2 ] && [ -r /run/subyard-e2e-lease.json ] \
  || die 'run only on allocated VM2'
[ "$(id -u)" = 0 ] || die 'run as root inside allocated VM2'

mode="${1:-}"
[ "$#" = 0 ] || shift
owner_ip='' owner_port='' private_ip='' client=two
while [ "$#" -gt 0 ]; do
  case "$1" in
    --client) [ "$#" -ge 2 ] || die '--client needs one or two'; client="$2"; shift 2 ;;
    --owner-ip) [ "$#" -ge 2 ] || die '--owner-ip needs a value'; owner_ip="$2"; shift 2 ;;
    --owner-port) [ "$#" -ge 2 ] || die '--owner-port needs a value'; owner_port="$2"; shift 2 ;;
    --private-ip) [ "$#" -ge 2 ] || die '--private-ip needs a value'; private_ip="$2"; shift 2 ;;
    *) usage; die 'unknown argument' ;;
  esac
done
case "$client" in one|two) ;; *) die 'client must be one or two' ;; esac
CONFIG="$CONFIG_DIR/client-$client.conf"
CONTAINER="subyard-amnezia-e2e-client-$client"
case "$mode" in
  probe)
    [ -n "$owner_ip" ] && [ -n "$owner_port" ] || { usage; die 'owner management target is required'; }
    python3 - "$owner_ip" "$private_ip" <<'PYADDR' || die 'probe address must be IPv4'
import ipaddress, sys
for value in sys.argv[1:]:
    if value:
        ipaddress.IPv4Address(value)
PYADDR
    [[ "$owner_port" =~ ^[0-9]{1,5}$ ]] && [ "$owner_port" -ge 1 ] && [ "$owner_port" -le 65535 ] \
      || die 'owner management port is invalid'
    ;;
  denied|reboot-traffic|cleanup) ;;
  *) usage; exit 2 ;;
esac

owned_container() {
  local label
  docker container inspect "$CONTAINER" >/dev/null 2>&1 || return 1
  label="$(docker inspect --format '{{index .Config.Labels "subyard.e2e"}}' "$CONTAINER")" \
    || die 'cannot inspect the client container'
  [ "$label" = "$MARKER" ] || die 'refusing a foreign container with the test name'
}

if [ "$mode" = cleanup ]; then
  command -v docker >/dev/null 2>&1 || exit 0
  docker info >/dev/null 2>&1 || die 'Docker is unavailable for client cleanup'
  if owned_container; then
    docker rm -f "$CONTAINER" >/dev/null || die 'could not remove owned client container'
  fi
  printf 'ok: native client container removed; transferred configuration retained\n'
  exit 0
fi
[ ! -L "$CONFIG_DIR" ] && [ "$(stat -c '%u:%a:%F' "$CONFIG_DIR")" = '0:700:directory' ] \
  || die 'client directory must be root-owned mode 0700'
for metadata in image tunnel-ip; do
  [ ! -L "$CONFIG_DIR/$metadata" ] && [ "$(stat -c '%u:%a:%F' "$CONFIG_DIR/$metadata")" = '0:600:regular file' ] \
    || die 'client metadata must be root-owned mode 0600'
done
AMNEZIA_IMAGE="$(cat "$CONFIG_DIR/image")"
[[ "$AMNEZIA_IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]] || die 'native client image identity is invalid'
TUNNEL_IP="$(cat "$CONFIG_DIR/tunnel-ip")"
python3 - "$TUNNEL_IP" <<'PYADDR' || die 'native server tunnel address is invalid'
import ipaddress, sys
ipaddress.IPv4Address(sys.argv[1])
PYADDR

if [ "$mode" = denied ] || [ "$mode" = reboot-traffic ]; then
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 \
    || die 'Docker is unavailable for retained-client proof'
  owned_container || die 'retained native client container is missing'
  [ "$(docker inspect --format '{{.State.Running}}' "$CONTAINER")" = true ] \
    || die 'retained native client container is stopped'
  docker exec "$CONTAINER" ip link show awg0 >/dev/null 2>&1 \
    || die 'retained native client tunnel is missing'
  if [ "$mode" = reboot-traffic ]; then
    # Exercise packets arriving before the owner restores the NAT route.
    docker exec -d "$CONTAINER" ping -n -q -i 1 -w 300 "$TUNNEL_IP" \
      || die 'could not start bounded client traffic across owner reboot'
    printf 'ok: retained client sends bounded traffic across owner reboot\n'
    exit 0
  fi
  if docker exec "$CONTAINER" ping -n -c 1 -W 3 "$TUNNEL_IP" >/dev/null 2>&1; then
    die 'VPN endpoint still answers after shutdown'
  fi
  printf 'ok: retained client can no longer reach the VPN endpoint\n'
  exit 0
fi

[ ! -L "$CONFIG_DIR" ] && [ "$(stat -c '%u:%a:%F' "$CONFIG_DIR")" = '0:700:directory' ] \
  || die 'client directory must be root-owned mode 0700'
[ ! -L "$CONFIG" ] && [ "$(stat -c '%u:%a:%F' "$CONFIG")" = '0:600:regular file' ] \
  || die 'client configuration must be root-owned mode 0600'
grep -q '^\[Interface\]$' "$CONFIG" && grep -q '^\[Peer\]$' "$CONFIG" \
  || die 'client configuration is incomplete'

if ! command -v docker >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq docker.io curl >/dev/null
fi
systemctl start docker || die 'Docker did not start on VM2'
docker info >/dev/null 2>&1 || die 'Docker is unavailable on VM2'

# A denied connection is evidence only if this same VM could reach it beforehand.
timeout 5 bash -c 'exec 3<>/dev/tcp/"$1"/"$2"' _ "$owner_ip" "$owner_port" 2>/dev/null \
  || die 'VM2 cannot reach the owner management port before connecting the VPN'
if [ -n "$private_ip" ]; then
  ping -n -c 1 -W 2 "$private_ip" >/dev/null 2>&1 \
    || die 'VM2 cannot reach the private probe before connecting the VPN'
fi
curl -4 --noproxy '*' -fsS --max-time 15 --output /dev/null https://example.com/ \
  || die 'VM2 control Internet is unavailable before connecting the VPN'

docker image inspect "$AMNEZIA_IMAGE" >/dev/null 2>&1 || die "native server image was not transferred"
if owned_container; then
  [ "$(docker inspect --format '{{.Config.Image}}' "$CONTAINER")" = "$AMNEZIA_IMAGE" ] \
    || die 'owned client container has a different image'
  [ "$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$CONTAINER")" = bridge ] \
    || die 'owned client container is not on a private Docker bridge'
  [ "$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/etc/amnezia/awg0.conf"}}{{.Source}}:{{.RW}}{{end}}{{end}}' "$CONTAINER")" = "$CONFIG:false" ] \
    || die 'owned client container has a different client configuration mount'
  [ "$(docker inspect --format '{{.State.Running}}' "$CONTAINER")" = true ] \
    || die 'owned client container is stopped; use cleanup before a new probe'
else
  # The native awg-quick writes a namespace-local sysctl unconditionally.
  # Docker's ordinary container makes /proc/sys read-only, even with NET_ADMIN.
  # This disposable VM2 client needs writable sysctls; retain its private network.
  docker run -d --name "$CONTAINER" --label "subyard.e2e=$MARKER" \
    --network bridge --privileged --sysctl net.ipv4.conf.all.src_valid_mark=1 \
    --log-driver none \
    --mount "type=bind,src=$CONFIG,dst=/etc/amnezia/awg0.conf,readonly" \
    --entrypoint /bin/sh "$AMNEZIA_IMAGE" -c \
    'trap "exit 0" TERM INT; while :; do sleep 3600 & wait "$!" || true; done' >/dev/null \
    || die 'could not create the private client container'
fi

# The native server image is unchanged. Only this disposable container gains probe tools.
if ! docker exec "$CONTAINER" sh -c 'for tool in curl dig ping resolvconf nc; do command -v "$tool" >/dev/null || exit 1; done'; then
  docker exec "$CONTAINER" apk add -q --no-cache curl bind-tools iputils openresolv netcat-openbsd \
    >/dev/null 2>&1 || die 'could not install disposable client probe tools'
fi
if ! docker exec "$CONTAINER" ip link show awg0 >/dev/null 2>&1; then
  docker exec -e WG_QUICK_USERSPACE_IMPLEMENTATION=amneziawg-go \
    -e AWG_QUICK_USERSPACE_IMPLEMENTATION=amneziawg-go \
    "$CONTAINER" awg-quick up /etc/amnezia/awg0.conf >/dev/null 2>&1 \
    || die 'the imported AmneziaWG client configuration did not start'
fi
docker cp "$ROOT/config/profiles/amnezia/tests/e2e/dns-probe.sh" "$CONTAINER:/tmp/subyard-amnezia-dns-probe.sh" \
  >/dev/null || die 'could not copy the owned DNS probe'

docker exec -i -e "PROBE_OWNER_IP=$owner_ip" -e "PROBE_OWNER_PORT=$owner_port" \
  -e "PROBE_PRIVATE_IP=$private_ip" -e "PROBE_TUNNEL_IP=$TUNNEL_IP" "$CONTAINER" sh -eu -s <<'CLIENT' \
  || die 'client tunnel acceptance failed'
fail() {
  printf 'client proof failed: %s\n' "$*" >&2
  # Keep failure evidence useful without printing peer keys or configuration.
  awg show awg0 latest-handshakes 2>/dev/null | awk -v now="$(date +%s)" \
    'NF == 2 && $2 ~ /^[0-9]+$/ { printf "client_handshake_epoch=%s age_s=%d\n", $2, now-$2 }' >&2 || true
  awg show awg0 transfer 2>/dev/null | awk \
    'NF == 3 && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ { printf "client_rx_bytes=%s client_tx_bytes=%s\n", $2, $3 }' >&2 || true
  exit 1
}
route() { ip -4 route get "$1" 2>/dev/null | grep -q ' dev awg0 '; }
route 1.1.1.1 || fail 'public IPv4 does not route through awg0'
route "$PROBE_OWNER_IP" || fail 'owner management IPv4 does not route through awg0'
if [ -n "$PROBE_PRIVATE_IP" ]; then
  route "$PROBE_PRIVATE_IP" || fail 'private IPv4 does not route through awg0'
fi
ip -6 route show table all 2>/dev/null | grep -Eq '^default dev awg0( |$)' \
  || fail 'IPv6 default route is not captured by awg0'
# A server restart loses the current session; allow the retained peer to rekey.
for attempt in $(seq 1 15); do
  ping -n -c 1 -W 2 "$PROBE_TUNNEL_IP" >/dev/null 2>&1 && break
  [ "$attempt" -lt 15 ] || fail 'VPN server tunnel address did not recover'
  sleep 1
done
ping_output="$(ping -n -c 2 -W 3 "$PROBE_TUNNEL_IP")" || fail 'VPN server tunnel address did not reply'
ping_avg_ms="$(printf '%s\n' "$ping_output" | awk -F= '/min\/avg\/max/ { split($2, values, "/"); gsub(/ /, "", values[2]); print values[2] }')"
printf '%s\n' "$ping_avg_ms" | grep -Eq '^[0-9]+([.][0-9]+)?$' || fail 'VPN ping average was unavailable'
ping -n -M do -s 1200 -c 1 -W 3 "$PROBE_TUNNEL_IP" >/dev/null 2>&1 \
  || fail '1200-byte ICMP payload failed'
handshake="$(awg show awg0 latest-handshakes | awk 'NF == 2 { if (++n > 1) exit 1; print $2 }')" \
  || fail 'cannot read the single native peer handshake'
case "$handshake" in ''|*[!0-9]*) fail 'peer handshake is missing' ;; esac
age="$(( $(date +%s) - handshake ))"
[ "$handshake" -gt 0 ] && [ "$age" -ge 0 ] && [ "$age" -le 180 ] \
  || fail 'peer handshake is stale'
counts="$(awg show awg0 transfer | awk 'NF == 3 { if (++n > 1) exit 1; print $2, $3 } END { if (n != 1) exit 1 }')" \
  || fail 'cannot read native peer counters'
set -- $counts
[ "$#" = 2 ] || fail 'cannot read native peer counters'
before_rx="$1" before_tx="$2"
download_metrics="$(curl -4 --noproxy '*' -fsS --max-time 45 \
  --write-out '%{time_total} %{speed_download}' --output /tmp/subyard-amnezia-download \
  'https://speed.cloudflare.com/__down?bytes=1048576')" || fail 'HTTPS download through tunnel failed'
set -- $download_metrics
[ "$#" = 2 ] || fail 'HTTPS timing metrics were unavailable'
download_seconds="$1" download_bytes_per_second="$2"
bytes="$(wc -c </tmp/subyard-amnezia-download)"
[ "$bytes" -ge 1048576 ] || fail 'HTTPS download was shorter than 1 MiB'
sh /tmp/subyard-amnezia-dns-probe.sh || fail 'DNS query through tunnel did not recover'
if timeout 5 nc -z -w 3 "$PROBE_OWNER_IP" "$PROBE_OWNER_PORT" >/dev/null 2>&1; then
  fail 'owner management port was reachable through the VPN'
fi
# The native app attaches an additional DNS bridge. Its VM-local gateway must
# not expose SSH through that path, while the native DNS sidecar stays usable.
route 172.29.172.1 || fail 'native DNS bridge management probe bypasses the tunnel'
if timeout 5 nc -z -w 3 172.29.172.1 22 >/dev/null 2>&1; then
  fail 'VPN VM SSH was reachable through the native DNS bridge'
fi
private_result=skipped
if [ -n "$PROBE_PRIVATE_IP" ]; then
  if ping -n -c 1 -W 2 "$PROBE_PRIVATE_IP" >/dev/null 2>&1; then
    fail 'private address was reachable through the VPN'
  fi
  private_result=denied
fi
if curl -6 -k --noproxy '*' -sS --connect-timeout 3 --max-time 6 --output /dev/null \
    'https://[2606:4700:4700::1111]/' >/dev/null 2>&1; then
  fail 'IPv6 escaped through the VPN'
fi
counts="$(awg show awg0 transfer | awk 'NF == 3 { if (++n > 1) exit 1; print $2, $3 } END { if (n != 1) exit 1 }')" \
  || fail 'cannot read native peer counters after traffic'
set -- $counts
[ "$#" = 2 ] && [ "$1" -gt "$before_rx" ] && [ "$2" -gt "$before_tx" ] \
  || fail 'VPN transfer counters did not advance in both directions'
printf 'ok: handshake_age_s=%s tunnel_rx_bytes=%s tunnel_tx_bytes=%s ping_avg_ms=%s https_bytes=%s https_seconds=%s https_bytes_per_second=%s dns=ok payload_bytes=1200 private=%s management=denied ipv6=denied\n' \
  "$age" "$1" "$2" "$ping_avg_ms" "$bytes" "$download_seconds" "$download_bytes_per_second" "$private_result"
CLIENT

curl -4 --noproxy '*' -fsS --max-time 15 --output /dev/null https://example.com/ \
  || die 'VM2 control Internet failed while the private client tunnel was active'
printf 'ok: VM2 control Internet remains available; client container retained for repeat probe or load\n'

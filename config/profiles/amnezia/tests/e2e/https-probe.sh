#!/bin/sh
# The caller applies a 95-second watchdog to both 45-second attempts and diagnostics.
set -eu
peer_counts() {
  awg show awg0 transfer | awk 'NF == 3 { if (++n > 1) exit 1; print $2, $3 } END { if (n != 1) exit 1 }'
}
printf 'amnezia_https_probe budget_seconds=95 attempts=2 attempt_seconds=45\n' >&2
for attempt in 1 2; do
  counts="$(peer_counts)" || exit 1
  set -- $counts
  before_rx="$1" before_tx="$2"
  code=0
  metrics="$(curl -4 --noproxy '*' -fsS --max-time 45 \
    --write-out '%{time_namelookup} %{time_connect} %{time_appconnect} %{time_starttransfer} %{time_total} %{speed_download}' \
    --output /tmp/subyard-amnezia-download \
    'https://speed.cloudflare.com/__down?bytes=1048576')" || code=$?
  set -- $metrics
  printf 'amnezia_https_probe attempt=%s curl_exit=%s dns_seconds=%s connect_seconds=%s tls_seconds=%s first_byte_seconds=%s total_seconds=%s\n' \
    "$attempt" "$code" "${1:-unavailable}" "${2:-unavailable}" "${3:-unavailable}" "${4:-unavailable}" "${5:-unavailable}" >&2
  if [ "$code" = 0 ]; then
    [ "$#" = 6 ] || exit 1
    if [ "$attempt" = 2 ]; then
      printf 'WARN amnezia-client: HTTPS download succeeded only after retry (attempt 1 curl_exit=%s)\n' "$first_code" >&2
    fi
    printf '%s %s\n' "$5" "$6"
    exit 0
  fi
  first_code="$code"
  counts="$(peer_counts)" || exit 1
  set -- $counts
  printf 'amnezia_https_probe attempt=%s rx_bytes_delta=%s tx_bytes_delta=%s\n' \
    "$attempt" "$(($1 - before_rx))" "$(($2 - before_tx))" >&2
done
exit "$code"

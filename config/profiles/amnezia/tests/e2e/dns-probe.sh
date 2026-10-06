#!/bin/sh
# Keep external DNS recovery bounded and report no response payloads.
set -eu

for attempt in 1 2 3; do
  status=0
  response="$(dig +time=5 +tries=1 +noall +comments +answer @1.1.1.1 example.com A 2>/dev/null)" || status=$?
  rcode="$(printf '%s\n' "$response" | awk '/^;; ->>HEADER<<-/ { gsub(/,/, "", $6); print $6; exit }')"
  case "$rcode" in NOERROR|SERVFAIL|NXDOMAIN|REFUSED|FORMERR|NOTIMP) ;; *) rcode=unknown ;; esac
  if [ "$status" -eq 0 ] && [ "$rcode" = NOERROR ] && printf '%s\n' "$response" | \
    awk '$4 == "A" && $5 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { found=1 } END { exit !found }'; then
    printf 'amnezia_dns_probe attempt=%s result=ipv4 rcode=%s query_exit=%s\n' "$attempt" "$rcode" "$status"
    exit 0
  fi
  printf 'amnezia_dns_probe attempt=%s result=unavailable rcode=%s query_exit=%s\n' "$attempt" "$rcode" "$status"
  [ "$attempt" -eq 3 ] || sleep 1
done
exit 1

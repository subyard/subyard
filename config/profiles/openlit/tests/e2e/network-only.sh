#!/usr/bin/env bash
# Called only after the fixture's normal container init/start; no image actions.
set -euo pipefail
[ "$#" = 4 ] || exit 2
root="$1" state="$2" instance="$3" project="$4"
# shellcheck source=config/profiles/openlit/tests/e2e/candidate.sh
. "$root/config/profiles/openlit/tests/e2e/candidate.sh"
helper="$root/config/profiles/openlit/tests/e2e/network-facts.py"
# Reuse the same public targets across one pull diagnostic's before/after probes.
if [ ! -e "$state/network-addresses.json" ]; then
  python3 "$helper" addresses > "$state/network-addresses.json"
  chmod 0600 "$state/network-addresses.json"
fi
[ -f "$state/network-addresses.json" ] && [ ! -L "$state/network-addresses.json" ] || exit 2
python3 - "$state/network-addresses.json" > "$state/network-addresses.lines" <<'PY'
import ipaddress,json,sys
values=json.load(open(sys.argv[1]))
assert isinstance(values,list) and len(values)<=4
for value in values:
    address=ipaddress.ip_address(value)
    assert address.version==4 and address.is_global
    print(str(address))
PY
chmod 0600 "$state/network-addresses.lines"
mapfile -t addresses < "$state/network-addresses.lines"
arguments=()
for address in "${addresses[@]}"; do arguments+=(--registry-ip "$address"); done
python3 "$helper" probe --location owner "${arguments[@]}" &
owner_pid=$!
trap 'kill "$owner_pid" 2>/dev/null || true; wait "$owner_pid" 2>/dev/null || true' EXIT
openlit_candidate_command /usr/bin/incus exec "$instance" --project "$project" -- \
  python3 - probe --location guest "${arguments[@]}" < "$helper"
wait "$owner_pid"
trap - EXIT
printf 'openlit_e2e_network_diagnostic=completed connectivity=not-asserted image-delivery=not-tested\n'

#!/usr/bin/env bash
# Stop the old staging consumer and verify it before an exclusive grant changes.
set -euo pipefail
[ "$#" -eq 3 ] || exit 2
dispatcher="$1"
context="$2"
zone="$3"
status() { "$dispatcher" -Y "$context" staging status "$zone"; }
stopped() {
	[[ "$1" != *'gateway: running'* ]] || return 1
  [[ "$1" == *'gateway: down'* || "$1" == "zone '$zone': (no runner)"* ]]
}
before="$(status)"
if [[ "$before" == *'gateway: running'* ]]; then
  "$dispatcher" -Y "$context" staging stop "$zone" --yes >/dev/null
elif ! stopped "$before"; then
  exit 1
fi
after="$(status)"
stopped "$after"

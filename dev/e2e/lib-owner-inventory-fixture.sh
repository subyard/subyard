#!/usr/bin/env bash
# Explicit cache ageing for fixture-owned snapshots, without waiting for a TTL.

expire_owner_inventory() {
  local cache="$1" owner="$2" temporary
  if [ ! -f "$cache" ] || [ -L "$cache" ]; then
    printf 'owner-inventory-fixture: snapshot is missing or unsafe\n' >&2
    return 1
  fi
  temporary="$(mktemp "${cache%/*}/.expired-inventory.XXXXXX")" || return
  if ! jq -e --arg owner "$owner" \
    'select(.inventory.hostId == $owner) | .fetchedAt = "1970-01-01T00:00:01Z"' \
    "$cache" > "$temporary"; then
    rm -f -- "$temporary"
    printf 'owner-inventory-fixture: unexpected snapshot owner or invalid JSON\n' >&2
    return 1
  fi
  if ! chmod 0600 "$temporary" || ! mv -f -- "$temporary" "$cache"; then
    rm -f -- "$temporary"
    return 1
  fi
}

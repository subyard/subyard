#!/usr/bin/env bash
# Owner-host tailnet publication, using the same device and receipt lifecycle as Orca.

device=herdr-ssh
receipt=user.subyard.resource.herdr-ssh
deferred=user.subyard.deferred_publication.herdr-ssh
connect=tcp:127.0.0.1:22

route_state() {
  incus query "/1.0/instances/$YARD_INSTANCE_NAME?project=$INCUS_PROJECT" |
    jq -ce --arg device "$device" --arg receipt "$receipt" --arg deferred "$deferred" '
      {live: .devices[$device], effective: (.expanded_devices // .devices)[$device],
       receipt: (.config[$receipt] // ""), saved: (.config[$deferred] // "")}'
}

route_receipt() {
  local shape="$1" listen
  listen=$(jq -er --arg connect "$connect" '
    select(keys == ["bind","connect","listen","type"] and .type == "proxy" and
           .bind == "host" and .connect == $connect) | .listen |
    select(test("^tcp:100\\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\\.[0-9]{1,3}\\.[0-9]{1,3}:[1-9][0-9]{0,4}$"))' <<< "$shape") || return 1
  printf 'v1:'
  printf 'v1\nbind=host\nconnect=%s\nlisten=%s\ntype=proxy\n' "$connect" "$listen" | sha256sum | cut -d ' ' -f1
}

route_absent() {
  local state
  state=$(route_state) || return 1
  jq -e '.effective == null and .receipt == "" and .saved == ""' <<< "$state" >/dev/null
}

validate_route() {
  local state="$1" adopt="${2:-}" live saved expected claimed decoded observed
  jq -e '.live == .effective' <<< "$state" >/dev/null || die 'Herdr refuses an inherited proxy device'
  live=$(jq -cS .live <<< "$state")
  claimed=$(jq -r .receipt <<< "$state")
  saved=$(jq -r .saved <<< "$state")
  if [ -n "$claimed" ] && ! [[ "$claimed" =~ ^v1:[a-f0-9]{64}$ ]]; then
    die 'Herdr proxy receipt is invalid'
  fi
  if [ "$live" != null ]; then
    expected=$(route_receipt "$live") || die 'Herdr refuses a foreign proxy device'
    if [ "$expected" != "$claimed" ] && { [ -n "$claimed" ] || [ "$live" != "$adopt" ]; }; then
      die 'Herdr proxy ownership does not match the device'
    fi
  fi
  if [ -n "$saved" ]; then
    decoded=$(jq -ce --arg claimed "$claimed" '
      select(keys == ["device","ownership","version"] and .version == 1 and .ownership == $claimed) | .device' <<< "$saved") || die 'Herdr deferred proxy is invalid'
    observed=$(route_receipt "$decoded") || die 'Herdr deferred proxy shape is invalid'
    [ "$observed" = "$claimed" ] || die 'Herdr deferred proxy ownership does not match'
    if [ "$live" != null ]; then
      jq -e --argjson saved "$decoded" '.live == $saved' <<< "$state" >/dev/null || die 'Herdr proxy differs from its deferred route'
    fi
  fi
}

remove_route() {
  local state="$1"
  validate_route "$state"
  if jq -e '.saved != ""' <<< "$state" >/dev/null; then
    incus config unset "$YARD_INSTANCE_NAME" "$deferred" "${PROJ[@]}" >/dev/null
  fi
  if jq -e '.live != null' <<< "$state" >/dev/null; then
    incus config device remove "$YARD_INSTANCE_NAME" "$device" "${PROJ[@]}" >/dev/null
  fi
  if jq -e '.receipt != ""' <<< "$state" >/dev/null; then
    incus config unset "$YARD_INSTANCE_NAME" "$receipt" "${PROJ[@]}" >/dev/null
  fi
}

endpoint() {
  local address port="${HERDR_HOST_PORT:-}" name="${HERDR_ADVERTISE_HOST:-}" ips
  [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*$ && "$port" =~ ^[1-9][0-9]{0,4}$ ]] &&
    [ "$port" -le 65535 ] || die 'Herdr needs a valid owner Tailscale host and port'
  address=$(tailscale ip -4 2>/dev/null) || die 'Herdr requires an active owner Tailscale IPv4 address'
  [[ "$address" =~ ^100\.([0-9]{1,3})\.[0-9]{1,3}\.[0-9]{1,3}$ ]] &&
    (( 10#${BASH_REMATCH[1]} >= 64 && 10#${BASH_REMATCH[1]} <= 127 )) ||
    die 'Herdr requires an active owner Tailscale IPv4 address'
  ips=$(getent ahostsv4 "$name" | awk '{print $1}' | sort -u) || die 'Herdr owner address does not resolve'
  if ! grep -Fxq -- "$address" <<< "$ips" || ! ip -o -4 addr show up | awk '{sub(/\/.*/, "", $4); print $4}' | grep -Fxq -- "$address"; then
    die 'Herdr advertised host must resolve to the owner Tailscale IPv4 address'
  fi
  printf '%s\n' "$address"
}

route_shape() {
  jq -cn --arg listen "tcp:$1:$HERDR_HOST_PORT" --arg connect "$connect" \
    '{type:"proxy",bind:"host",listen:$listen,connect:$connect}'
}

route_ready() {
  local ip shape state token
  guest_ssh check || return 1
  ip=$(endpoint) || return 1
  shape=$(route_shape "$ip")
  token=$(route_receipt "$shape") || return 1
  state=$(route_state) || return 1
  jq -e --argjson shape "$shape" --arg token "$token" '
    .live == $shape and .effective == $shape and .receipt == $token and .saved == ""' <<< "$state" >/dev/null || return 1
  listener_ready "$ip" "$HERDR_HOST_PORT"
}

listener_ready() {
  timeout 5 bash -c 'exec 3<>"/dev/tcp/$1/$2"; read -r banner <&3; [[ "$banner" == SSH-* ]]' herdr "$1" "$2"
}

ensure_route() {
  local ip shape state token created=false
  guest_ssh check || die 'Herdr SSH drop-in is missing or changed. Run yard init to reconcile the profile.'
  ip=$(endpoint)
  shape=$(route_shape "$ip")
  token=$(route_receipt "$shape")
  state=$(route_state)
  validate_route "$state" "$(jq -cS . <<< "$shape")"
  if ! jq -e --argjson shape "$shape" '.live == $shape' <<< "$state" >/dev/null; then
    remove_route "$state"
    if ss -Hltn "sport = :$HERDR_HOST_PORT" | awk '{print $4}' |
      grep -Fxq -e "$ip:$HERDR_HOST_PORT" -e "0.0.0.0:$HERDR_HOST_PORT" -e "*:$HERDR_HOST_PORT"; then
      die 'Herdr owner port is already in use'
    fi
    incus config device add "$YARD_INSTANCE_NAME" "$device" proxy "${PROJ[@]}" \
      "listen=tcp:$ip:$HERDR_HOST_PORT" "connect=$connect" bind=host >/dev/null
    created=true
  fi
  if ! incus config set "$YARD_INSTANCE_NAME" "$receipt" "$token" "${PROJ[@]}" >/dev/null; then
    if [ "$created" = true ]; then incus config device remove "$YARD_INSTANCE_NAME" "$device" "${PROJ[@]}" >/dev/null; fi
    die 'could not record ownership of the Herdr proxy'
  fi
  if jq -e '.saved != "" and .live != null' <<< "$state" >/dev/null; then
    incus config unset "$YARD_INSTANCE_NAME" "$deferred" "${PROJ[@]}" >/dev/null
  fi
  if ! listener_ready "$ip" "$HERDR_HOST_PORT"; then
    if [ "$created" = true ]; then remove_route "$(route_state)"; fi
    die 'Herdr owner endpoint did not reach guest SSH'
  fi
}

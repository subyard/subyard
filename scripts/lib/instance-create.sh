#!/usr/bin/env bash
# Existing instance creation rendering, shared by ordinary 03 and owner recipes.

subyard_instance_defaults() {
  INCUS_PROJECT="${INCUS_PROJECT:-subyard}"
  YARD_INSTANCE_NAME="${YARD_INSTANCE_NAME:-yard}"
  YARD_KIND="${YARD_KIND:-container}"
  YARD_IMAGE="${YARD_IMAGE:-images:debian/13}"
  YARD_IMAGE_FALLBACK="${YARD_IMAGE_FALLBACK:-images:ubuntu/24.04}"
  SRV_POOL="${SRV_POOL:-default}"
  SRV_VOLUME="${SRV_VOLUME:-yard-srv}"
  DEV_USER="${DEV_USER:-dev}"
  BRIDGE="${INCUS_BRIDGE:-${INCUS_NETWORK:-incusbr0}}"
  YARD_LABEL="${YARD_NAME:-default}"
  desired_power="${SUBYARD_POWER_DESIRED:-}"
  
  PROJ=(--project "$INCUS_PROJECT")
}

subyard_host_timezone() {
  local etc="$1" target zone='' LC_ALL=C
  # Prefer the active zone when the legacy timezone file is stale.
  IFS= read -r -d '' target < <(readlink -z -- "$etc/localtime" 2>/dev/null) || target=''
  case "$target" in
    /usr/share/zoneinfo/* | ../usr/share/zoneinfo/*)
      zone=${target#*/usr/share/zoneinfo/}
      zone=${zone#posix/} ;;
    *)
      [ -f "$etc/timezone" ] || return 1
      # Read the whole file and reject NULs. EOF is normal for a text file.
      if { IFS=$' \t\r\n' read -r -d '' zone < "$etc/timezone"; } 2>/dev/null; then
        return 1
      fi ;;
  esac
  [[ "$zone" =~ ^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$ && "$zone" != UTC && "$zone" != Etc/UTC ]] || return 1
  printf '%s\n' "$zone"
}

subyard_instance_base_flags() {
  LAUNCH_FLAGS=()
  if [ "$YARD_KIND" = vm ]; then
    LAUNCH_FLAGS+=(--vm)
  else
    LAUNCH_FLAGS+=(-c security.nesting=true)
    if [ "${NESTED_E2E_VMS:-0}" = 1 ]; then
      LAUNCH_FLAGS+=(-c security.syscalls.intercept.bpf=true -c security.syscalls.intercept.bpf.devices=true)
    fi
  fi
  [ -z "${LIMITS_CPU:-}" ] || LAUNCH_FLAGS+=(-c "limits.cpu=$LIMITS_CPU")
  [ -z "${LIMITS_MEMORY:-}" ] || LAUNCH_FLAGS+=(-c "limits.memory=$LIMITS_MEMORY")
}

subyard_instance_identity_flags() {
  LAUNCH_FLAGS+=(
    -c boot.autostart=false
    -c "$POWER_KEY_MANAGED=true" -c "$POWER_KEY_NAME=$YARD_LABEL"
    -c "$POWER_KEY_BRIDGE=$BRIDGE" -c "$POWER_KEY_DESIRED=$desired_power"
    -c "$POWER_KEY_INITIALIZED=false"
  )
}

subyard_instance_catalog_available() { # <reference> <image-type>, original catalog on stdin
  local reference="${1:?}" image_type="${2:?}"
  jq -ser --arg reference "$reference" --arg kind "$image_type" '
    if length == 1 and (.[0] | type == "array") then .[0]
      else error("invalid image catalog") end
    | if all(.[];
        type == "object" and (.fingerprint | type == "string")
        and (.fingerprint | test("^[a-f0-9]{64}$"))
        and (.type == "" or .type == "container" or .type == "virtual-machine")
        and has("aliases") and (.aliases == null or (.aliases | type == "array"))
        and all((.aliases // [])[]; type == "object" and (.name | type == "string")))
      then . else error("invalid image catalog") end
    | def kind: if .type == "" then "container" else .type end;
      if any(.[]; kind == $kind and any((.aliases // [])[]; .name == $reference)) then "present"
      else map(select(.fingerprint | startswith($reference)))
        | if length == 0 then "absent"
          elif length == 1 then (if .[0] | kind == $kind then "present" else "absent" end)
          else error("ambiguous image fingerprint") end
      end
  '
}

# Classifies an `incus image list` error read on stdin as a transient cause, or unknown.
subyard_image_metadata_cause() {
  local error
  error=$(LC_ALL=C tr -d '\000') || true
  case "$(printf '%s' "$error" | LC_ALL=C tr '[:upper:]' '[:lower:]')" in
    *'no such host'* | *'temporary failure in name resolution'* | *'server misbehaving'*) printf 'dns\n' ;;
    *'context deadline exceeded'* | *'i/o timeout'* | *'tls handshake timeout'*) printf 'timeout\n' ;;
    *'connection reset by peer'* | *'connection refused'* | *'network is unreachable'*) printf 'connection\n' ;;
    *'unexpected eof'*) printf 'interrupted\n' ;;
    *' 502 bad gateway'* | *' 503 service unavailable'* | *' 504 gateway timeout'*) printf 'server\n' ;;
    *) printf 'unknown\n' ;;
  esac
}

subyard_instance_selected_reference() { # <present|absent>
  case "${1:?}" in
    present) selected_image=$YARD_IMAGE ;;
    absent)
      warn "preferred image is absent from its catalog; trying fallback $YARD_IMAGE_FALLBACK"
      selected_image=$YARD_IMAGE_FALLBACK ;;
    *) return 2 ;;
  esac
}

yard_instance_create() (
  local reference="$YARD_IMAGE" image_type=container metadata attempt rc cause availability selected_image
  local remote=()
  if [[ "$reference" = *:* ]]; then
    remote=("${reference%%:*}:")
    reference=${reference#*:}
  fi
  [ -n "$reference" ] || exit 2
  [ "$YARD_KIND" != vm ] || image_type=virtual-machine
  metadata=$(mktemp -d /tmp/subyard-image-metadata.XXXXXX) || exit 1
  trap 'rm -rf -- "$metadata"' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  chmod 0700 "$metadata" || exit 1
  install -m 0600 /dev/null "$metadata/catalog" || exit 1
  install -m 0600 /dev/null "$metadata/error" || exit 1
  install -m 0600 /dev/null "$metadata/parse-error" || exit 1
  for attempt in 1 2 3; do
    if timeout --signal=TERM --kill-after=1 30 incus image list "${remote[@]}" "${PROJ[@]}" --format=json \
      > "$metadata/catalog" 2> "$metadata/error"; then
      break
    else
      rc=$?
    fi
    # Cancellation and unknown errors are terminal. Retry only bounded read-only
    # metadata requests, with at most 95 seconds across attempts and backoff.
    cause=unknown
    [ "$rc" != 124 ] || cause=timeout
    [ "$rc" != 1 ] || cause=$(subyard_image_metadata_cause < "$metadata/error")
    if [ "$attempt" -lt 3 ] && [ "$cause" != unknown ]; then
      sleep 1 || exit "$?"
    else
      # Name only the cause class: the CLI's own stderr stays private.
      printf 'image metadata unavailable (incus image list exit %s, cause %s)\n' "$rc" "$cause" >&2
      exit "$rc"
    fi
  done
  # Incus emits api.Image arrays. Nil aliases and legacy empty container types
  # are valid. Read the catalog from its private file, never through argv.
  availability=$(subyard_instance_catalog_available "$reference" "$image_type" < "$metadata/catalog" 2> "$metadata/parse-error") || {
    rc=$?
    printf 'image metadata invalid (catalog parse exit %s)\n' "$rc" >&2
    exit "$rc"
  }

  subyard_instance_selected_reference "$availability" || exit $?
  info "creating $YARD_INSTANCE_NAME from $selected_image"
  # Keep progress on stdout, but private CLI stderr inside this scoped capture.
  if incus init "$selected_image" "$YARD_INSTANCE_NAME" "${PROJ[@]}" "${LAUNCH_FLAGS[@]}" 2> "$metadata/error"; then
    ok "created $YARD_INSTANCE_NAME"
  else
    rc=$?
    if incus info "$YARD_INSTANCE_NAME" "${PROJ[@]}" >/dev/null 2>&1; then
      warn "instance '$YARD_INSTANCE_NAME' was created with an initialization warning (incus init exit $rc)"
    else
      # Expose a recognized public reason, never arbitrary private CLI stderr.
      if LC_ALL=C grep -Fxq -- "Error: Image \"$selected_image\" not found" "$metadata/error"; then
        printf 'Error: Image "%s" not found (Incus project "%s")\n' "$selected_image" "$INCUS_PROJECT" >&2
      fi
      printf 'instance creation failed (incus init exit %s)\n' "$rc" >&2
      exit "$rc"
    fi
  fi
)
# Consume the complete list: grep -q can SIGPIPE Incus under pipefail.

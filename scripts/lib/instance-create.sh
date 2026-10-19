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

subyard_instance_selected_reference() { # <present|absent>
  case "${1:?}" in
    present) selected_image=$YARD_IMAGE ;;
    absent)
      warn "preferred image is absent from its catalog; trying fallback $YARD_IMAGE_FALLBACK"
      selected_image=$YARD_IMAGE_FALLBACK ;;
    *) return 2 ;;
  esac
}

# Native preparation supplies its checked SDK catalog on fixed read-only fd3.
# Selection and initial flags use precisely the ordinary helper functions.
subyard_instance_config_render() {
  local reference image_type=container availability selected_image
  subyard_instance_defaults
  subyard_instance_base_flags
  subyard_instance_identity_flags
  reference=$YARD_IMAGE
  [[ "$reference" != *:* ]] || reference=${reference#*:}
  [ "$YARD_KIND" != vm ] || image_type=virtual-machine
  availability="$(subyard_instance_catalog_available "$reference" "$image_type" <&3)" || return
  subyard_instance_selected_reference "$availability" || return
  subyard_instance_approved_config_render "$selected_image"
}

# Reproduce only the already captured selection; this guard never reads a
# catalog or chooses a new alias/fallback.
subyard_instance_approved_config_render() {
  local selected_image="${1:?}"
  subyard_instance_defaults
  [ "$selected_image" = "$YARD_IMAGE" ] || [ "$selected_image" = "$YARD_IMAGE_FALLBACK" ] || return 2
  subyard_instance_base_flags
  subyard_instance_identity_flags
  printf '%s\0' "$selected_image" "$YARD_KIND" "${LAUNCH_FLAGS[@]}"
}

yard_instance_create() (
  local reference="$YARD_IMAGE" image_type=container metadata attempt rc availability selected_image
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
    if [ "$attempt" -lt 3 ] \
      && { [ "$rc" = 124 ] || { [ "$rc" = 1 ] && LC_ALL=C grep -qiE \
        'lookup .*: (no such host|temporary failure in name resolution|server misbehaving)|context deadline exceeded|i/o timeout|TLS handshake timeout|connection (reset by peer|refused)|network is unreachable' "$metadata/error"; }; }; then
      sleep 1 || exit "$?"
    else
      printf 'image metadata unavailable (incus image list exit %s)\n' "$rc" >&2
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
      printf 'instance creation failed (incus init exit %s)\n' "$rc" >&2
      exit "$rc"
    fi
  fi
)
# Consume the complete list: grep -q can SIGPIPE Incus under pipefail.

#!/usr/bin/env bash
# Resolve a default-project cache entry for init in a private image namespace.
p0_container_image_reference() {
  local alias="$1" remote fingerprint info remotes
  local source="${P0_REAL_INCUS_CONTAINER_IMAGE:-images:debian/13}"
  [[ "$source" =~ ^([[:alnum:]_.-]+):[^[:space:]]+$ ]] \
    || die 'fixture requires a public image remote'
  remote="${BASH_REMATCH[1]}"
  [ "$remote" != local ] || die 'fixture requires a public image remote'
  remotes="$(incus remote list --format=json)" || die 'cannot inspect fixture image remote'
  jq -e --arg remote "$remote" 'type=="object" and
    (.[$remote].Public==true) and
    (.[$remote].Protocol=="simplestreams" or .[$remote].Protocol=="incus")' \
    <<<"$remotes" >/dev/null || die 'fixture requires a public Incus or simplestreams image remote'
  info="$(incus image info "$alias" --project default)" \
    || die "test-owned base image alias $alias is unavailable"
  fingerprint="$(awk '/^Fingerprint: / {print $2}' <<<"$info")"
  [[ "$fingerprint" =~ ^[a-f0-9]{64}$ ]] && grep -Fqx 'Type: container' <<<"$info" \
    || die 'fixture cached image identity is invalid'
  info="$(timeout --foreground "${P0_REAL_INCUS_COMMAND_TIMEOUT:-900}" incus image info "$remote:$fingerprint")" \
    || die 'exact cached fixture image is unavailable from its public remote'
  [ "$(awk '/^Fingerprint: / {print $2}' <<<"$info")" = "$fingerprint" ] \
    && grep -Fqx 'Type: container' <<<"$info" || die 'public fixture image differs from the cached image'
  printf '%s:%s\n' "$remote" "$fingerprint"
}

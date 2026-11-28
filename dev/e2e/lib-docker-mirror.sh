#!/usr/bin/env bash
# Docker Hub pull mirror for a lane's test yard. Hosted runners share egress addresses, so their
# anonymous Docker Hub pulls hit its rate limit; dev/ci/periodic-e2e.sh names a mirror, and Docker
# still pulls from Docker Hub when the mirror fails. Without SUBYARD_E2E_DOCKER_MIRROR, a no-op.
# shellcheck shell=bash

# e2e_docker_mirror COMMAND... : COMMAND runs a root shell command in the yard, such as
# `incus exec INSTANCE --project PROJECT --`. Call it after Docker starts, before the first pull.
e2e_docker_mirror() {
  [ -n "${SUBYARD_E2E_DOCKER_MIRROR:-}" ] || return 0
  "$@" sh -eu -c '
    config="${SUBYARD_E2E_DOCKER_CONFIG:-/etc/docker/daemon.json}"
    temporary="$(mktemp "${config%/*}/.daemon.json.XXXXXX")"
    trap "rm -f \"\$temporary\"" EXIT
    jq --arg mirror "$1" ". + {\"registry-mirrors\": [\$mirror]}" "$config" > "$temporary"
    install -m 0644 "$temporary" "$config"
    systemctl restart docker
    docker info --format "{{json .RegistryConfig.Mirrors}}" \
      | jq -e --arg mirror "${1%/}/" "index(\$mirror) != null" >/dev/null
    printf "docker hub mirror=%s\n" "$1"
  ' _ "$SUBYARD_E2E_DOCKER_MIRROR"
}

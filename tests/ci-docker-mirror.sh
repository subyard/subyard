#!/usr/bin/env bash
# Host-free contract for the hosted Docker Hub mirror: periodic-e2e.sh hands it to lane
# controllers, the guest command forwards it only when set, and the lane helper merges it into
# the yard's Docker config, restarts Docker and fails unless Docker reports the mirror.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TMP="$(mktemp -d -t subyard-ci-docker-mirror.XXXXXX)"
trap 'rm -rf -- "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
function_source() { sed -n "/^$2() {/,/^}$/p" "$1"; }
unset SUBYARD_E2E_DOCKER_MIRROR SUBYARD_E2E_CANDIDATE_BUNDLE

# Lane controllers started by the hosted wrapper see the mirror.
eval "$(grep -x 'DOCKER_MIRROR=.*' "$ROOT/dev/ci/periodic-e2e.sh")"
eval "$(function_source "$ROOT/dev/ci/periodic-e2e.sh" controller_env)"
# shellcheck disable=SC2034 # read by the evaluated controller_env
BUNDLE="$TMP/bundle" WORKSPACE="$TMP"
: > "$BUNDLE"
[ "$(controller_env printenv SUBYARD_E2E_DOCKER_MIRROR)" = https://mirror.gcr.io ] \
  || fail 'hosted lane controllers do not get the Docker Hub mirror'
# Hosted release acceptance runs its collector outside controller_env and needs the mirror too.
eval "$(function_source "$ROOT/dev/ci/periodic-e2e.sh" run_collector_selection)"
install -d -m 0700 "$TMP/collector-bin"
printf '#!/bin/sh\nprintf %%s "$SUBYARD_E2E_DOCKER_MIRROR" > "$MIRROR_SEEN"\n' > "$TMP/collector-bin/python3"
chmod 0700 "$TMP/collector-bin/python3"
PATH="$TMP/collector-bin:$PATH" MIRROR_SEEN="$TMP/collector-mirror" run_collector_selection "$TMP/out" check >/dev/null 2>&1 || true
[ "$(cat "$TMP/collector-mirror")" = https://mirror.gcr.io ] \
  || fail 'hosted release acceptance does not get the Docker Hub mirror'

# The guest command forwards the mirror only when the controller has one.
eval "$(function_source "$ROOT/dev/agent-e2e.sh" write_guest_command)"
# shellcheck disable=SC2034 # read by the evaluated write_guest_command
E2E_YARD=y LEASE_PROJECT=p LEASE_RUN=r LEASE_PURPOSE=u LEASE_SLOT=1 LEASE_GENERATION=1 \
  ENVIRONMENT_TYPE=subyard-pair VM_COUNT=1 GRANT_CPU_PER_VM=1 GRANT_MEMORY_PER_VM=1GiB \
  GRANT_DISK_PER_VM=1GiB BASE_FINGERPRINT=f
write_guest_command 1 "$TMP" true > "$TMP/plain.sh"
! grep -q DOCKER_MIRROR "$TMP/plain.sh" || fail 'guest command exports an unset mirror'
SUBYARD_E2E_DOCKER_MIRROR='https://mirror.example/a b' write_guest_command 1 "$TMP" true > "$TMP/run.sh"
[ "$(env -i bash -c '. <(grep "^export SUBYARD_E2E_DOCKER_MIRROR=" "$1"); printf %s "$SUBYARD_E2E_DOCKER_MIRROR"' _ "$TMP/run.sh")" \
  = 'https://mirror.example/a b' ] || fail 'guest command did not forward the mirror'

# The helper, with the yard's root shell replaced by fake systemctl and docker.
bin="$TMP/bin"
mkdir "$bin"
cat > "$bin/systemctl" <<'EOF'
#!/bin/bash
printf 'systemctl %s\n' "$*" >> "$TRACE"
jq -c '."registry-mirrors"' "$SUBYARD_E2E_DOCKER_CONFIG" > "$TMP/running"
EOF
cat > "$bin/docker" <<'EOF'
#!/bin/bash
printf 'docker %s\n' "$1" >> "$TRACE"
[ "${DOCKER_IGNORES_MIRROR:-0}" = 1 ] && { printf '[]\n'; exit; }
jq -c 'map(. + "/")' "$TMP/running"
EOF
chmod 0700 "$bin/systemctl" "$bin/docker"
config="$TMP/docker/daemon.json"
mkdir "$TMP/docker"
ignore=0
yard_root() {
  env PATH="$bin:$PATH" TMP="$TMP" TRACE="$TMP/trace" SUBYARD_E2E_DOCKER_CONFIG="$config" \
    DOCKER_IGNORES_MIRROR="$ignore" "$@"
}
# shellcheck source=dev/e2e/lib-docker-mirror.sh
. "$ROOT/dev/e2e/lib-docker-mirror.sh"

printf '{"ip-forward-no-drop":true,"dns":["1.1.1.1"]}\n' > "$config"
e2e_docker_mirror fail 'ran the yard command without a mirror'
[ ! -e "$TMP/trace" ] || fail 'unset mirror changed the yard'
export SUBYARD_E2E_DOCKER_MIRROR=https://mirror.gcr.io
output="$(e2e_docker_mirror yard_root)" || fail 'mirror helper failed'
[ "$output" = 'docker hub mirror=https://mirror.gcr.io' ] || fail "helper output: $output"
[ "$(jq -cS . "$config")" = '{"dns":["1.1.1.1"],"ip-forward-no-drop":true,"registry-mirrors":["https://mirror.gcr.io"]}' ] \
  || fail "existing Docker settings were not kept: $(cat "$config")"
[ "$(stat -c %a "$config")" = 644 ] || fail 'Docker config mode changed'
[ "$(cat "$TMP/trace")" = $'systemctl restart docker\ndocker info' ] || fail "calls: $(cat "$TMP/trace")"
[ "$(find "$TMP/docker" -mindepth 1 | wc -l)" = 1 ] || fail 'helper left a temporary file'

# A Docker that does not report the mirror, or an unreadable config, fails the lane.
ignore=1
if e2e_docker_mirror yard_root >/dev/null 2>&1; then fail 'helper passed although Docker did not report the mirror'; fi
ignore=0
printf 'not json\n' > "$config"
if e2e_docker_mirror yard_root >/dev/null 2>&1; then fail 'helper passed with an invalid Docker config'; fi
[ "$(cat "$config")" = 'not json' ] || fail 'invalid Docker config was replaced'
printf 'ok: hosted lanes pass a Docker Hub mirror to their yards and fail unless Docker uses it\n'

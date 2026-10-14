#!/usr/bin/env bash
# Execute the owning network-only branch with fixture-owned process doubles.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
if [ ! -x /usr/sbin/sshd ]; then
  printf 'SKIP: owning network-only lifecycle requires the fixture OpenSSH prerequisite\n'
  exit 0
fi
tmp="$(mktemp -d /tmp/subyard-openlit-network.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
root="$tmp/source"
install -d -m 0700 "$root/config/profiles/openlit/tests/e2e" "$tmp/bin" "$tmp/state"
cp "$ROOT/config/profiles/openlit/tests/e2e/"{owner.sh,network-only.sh,network-facts.py} "$root/config/profiles/openlit/tests/e2e/"
# The native platform preflight also uses the absolute CLI path. Bind that
# physical dependency in this copied fixture; never inspect the test host.
python3 - "$root/config/profiles/openlit/tests/e2e/owner.sh" "$tmp/bin/incus" <<'PY'
import pathlib,sys
path=pathlib.Path(sys.argv[1])
path.write_text(path.read_text().replace('/usr/bin/incus',sys.argv[2]))
PY
printf 'services: {}\n' > "$root/config/profiles/openlit/compose.yaml"
export OPENLIT_NETWORK_FIXTURE_ROOT="$tmp"
cat > "$root/config/profiles/openlit/tests/e2e/candidate.sh" <<'SH'
openlit_candidate_install() { printf '%s\n' "$OPENLIT_NETWORK_FIXTURE_ROOT/bin/yard"; }
openlit_candidate_command() {
  if [ "$1" = /usr/bin/incus ]; then shift; "$OPENLIT_NETWORK_FIXTURE_ROOT/bin/incus" "$@"; else "$@"; fi
}
SH
cat > "$tmp/bin/id" <<'SH'
#!/bin/sh
case "${1-}" in
  -un) printf 'dev\n' ;;
  -nG)
    { [ "$#" = 1 ] || { [ "$#" = 2 ] && [ "$2" = dev ]; }; } || exit 99
    printf 'dev incus-admin\n' ;;
  *) exec /usr/bin/id "$@" ;;
esac
SH
cat > "$tmp/bin/mktemp" <<'SH'
#!/bin/sh
if [ "${2-}" = /var/tmp/subyard-openlit.XXXXXX ]; then
  exec /usr/bin/mktemp -d "$OPENLIT_NETWORK_FIXTURE_ROOT/state/yard.XXXXXX"
else exec /usr/bin/mktemp "$@"; fi
SH
cat > "$tmp/bin/sudo" <<'SH'
#!/bin/sh
test "$1" = -n
case "$2" in true|unshare) exit 0 ;; *) exit 99 ;; esac
SH
cat > "$tmp/bin/yard" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >> "$OPENLIT_NETWORK_FIXTURE_ROOT/yard-calls"
test "$1" = -Y
name="$2"; shift 2
case "$*" in
  'init --yes')
    grep -Fxq 'CODING_TOOL_INTEGRATIONS=""' "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
    grep -Fxq 'ENVIRONMENT_PROFILES=' "$SUBYARD_CONFIG_HOME/yards/$name/config.env" ;;
  'start --yes'|'teardown --yes') ;;
  *) exit 99 ;;
esac
SH
cat > "$tmp/bin/incus" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >> "$OPENLIT_NETWORK_FIXTURE_ROOT/incus-calls"
case "$1" in
  query)
    test "$*" = 'query /1.0/instances?project=default' || exit 99
    printf '{}\n' ;;
  storage) test "$*" = 'storage show default --project default' ;;
  list) printf 'STOPPED\n' ;;
  config) test "$2" = get; printf 'stopped\n' ;;
  exec)
    while [ "$1" != -- ]; do shift; done
    shift
    if [ "$1" = test ]; then
      test "$*" = 'test ! -e /var/lib/subyard/openlit'
    elif [ "$1" = python3 ] && [ "$2" = - ] && [ "$3" = probe ]; then
      exec "$@"
    else exit 99; fi ;;
  *) exit 99 ;;
esac
SH
cat > "$tmp/bin/getent" <<'SH'
#!/bin/sh
test "$*" = 'ahostsv4 registry-1.docker.io'
printf '44.1.2.3 STREAM synthetic\n'
SH
cat > "$tmp/bin/curl" <<'SH'
#!/bin/sh
printf '401 0.01 0.02 0.03 0.04 44.1.2.3'
printf 'PRIVATE_NETWORK_SENTINEL\n' >&2
SH
cat > "$tmp/bin/ip" <<'SH'
#!/bin/sh
printf '[]\n'
SH
for command in go ssh ssh-keygen unshare nsenter docker; do
  printf '#!/bin/sh\nexit 99\n' > "$tmp/bin/$command"
done
chmod 0755 "$tmp/bin/"*
export PATH="$tmp/bin:$PATH" SUBYARD_E2E_VM=1 SUBYARD_E2E_MEMORY_PER_VM=8GiB
bash "$root/config/profiles/openlit/tests/e2e/owner.sh" container network-only > "$tmp/output" 2> "$tmp/error"
grep -Fxq 'openlit_e2e_network_diagnostic=completed connectivity=not-asserted image-delivery=not-tested' "$tmp/output"
! grep -q PRIVATE_NETWORK_SENTINEL "$tmp/output" "$tmp/error"
python3 - "$tmp/output" "$tmp/yard-calls" "$tmp/incus-calls" <<'PY'
import json,pathlib,sys
output, yards, incus = map(pathlib.Path,sys.argv[1:])
records=[json.loads(line.split('=',1)[1]) for line in output.read_text().splitlines() if line.startswith('openlit_e2e_network=')]
assert {row['location'] for row in records}=={'owner','guest'}
for location in ('owner','guest'):
    modes={row['mode'] for row in records if row['location']==location}
    assert {'registry-default','registry-ipv4','registry-direct-ipv4','registry-direct-resolved-ipv4','dockerhub-auth','ghcr','ghcr-auth'}<=modes
calls=yards.read_text().splitlines()
assert len(calls)==3 and calls[0].endswith('init --yes') and calls[1].endswith('start --yes') and calls[2].endswith('teardown --yes')
calls=incus.read_text()
assert calls.splitlines().count('query /1.0/instances?project=default')==2
assert calls.splitlines().count('storage show default --project default')==2
assert 'python3 - probe --location guest --registry-ip 44.1.2.3' in calls
assert 'docker' not in calls and 'bash' not in calls
PY
printf 'ok: network-only normal empty-integration lifecycle probes owner and guest before any image/CLI/profile action\n'

#!/usr/bin/env bash
set -euo pipefail
test_root=$(mktemp -d /tmp/subyard-herdr-route.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
die() { printf '%s\n' "$*" >&2; exit 1; }
# shellcheck source=config/profiles/herdr/resources/herdr/route.sh
. config/profiles/herdr/resources/herdr/route.sh
INCUS_PROJECT=fixture
YARD_INSTANCE_NAME=fixture
PROJ=(--project fixture)
HERDR_HOST_PORT=6768
HERDR_ADVERTISE_HOST=fixture.ts.net
tailscale() { printf '100.100.100.100\n'; }
getent() { printf '%s STREAM fixture.ts.net\n' "${resolved:-100.100.100.100}"; }
ip() { printf '12: tailscale0 inet 100.100.100.100/32 scope global\n'; }
ssh_live() { [ "${listener:-up}" = up ] && [ "$1" = 100.100.100.100:6768 ]; }
shape=$(route_shape 100.100.100.100)
token=$(route_receipt "$shape")
pending=$(jq -cn --argjson shape "$shape" --arg token "$token" '{device:$shape,ownership:$token,version:1}')
incus() {
  if [ "$1" = query ]; then
    jq -cn --argjson shape "$shape" --arg token "$token" --arg pending "${saved:-}" --arg receipt "$receipt" --arg deferred "$deferred" \
      '{devices:{"herdr-ssh":$shape},config:{($receipt):$token,($deferred):$pending}}'
  else
    printf '%s\n' "$*" >> "$test_root/calls"
  fi
}
route_ready
if (listener=down; route_ready); then exit 1; fi
if (resolved=192.0.2.1 endpoint) >/dev/null 2>&1; then exit 1; fi
saved="$pending"
remove_route "$(route_state)"
cat > "$test_root/expected" <<EOF
config unset fixture $deferred --project fixture
config device remove fixture $device --project fixture
config unset fixture $receipt --project fixture
EOF
cmp "$test_root/calls" "$test_root/expected"

# Run the guest policy script using fixture commands, never the host sshd.
export HERDR_CONTEXT_FILE="$test_root/contexts"
mkdir "$test_root/bin"
cat > "$test_root/bin/sshd" <<'SSHD'
#!/bin/sh
if [ "$#" -gt 1 ]; then
  printf '%s\n' "$3" >> "$HERDR_CONTEXT_FILE"
else
  printf 'usedns %s\n' "$HERDR_TEST_DNS"
fi
SSHD
cat > "$test_root/bin/getent" <<'GETENT'
#!/bin/sh
case "$*" in
  passwd) printf 'dev:x:1000:1000::/home/dev:/bin/bash\n' ;;
  'hosts 127.0.0.1') printf '127.0.0.1 localhost\n' ;;
  'ahostsv4 localhost') printf '127.0.0.1 STREAM localhost\n' ;;
  *) exit 1 ;;
esac
GETENT
printf '#!/bin/sh\ncat >/dev/null\n' > "$test_root/bin/helper"
chmod 0755 "$test_root/bin/"*
DEV_USER=dev
native="$test_root/bin/helper"
yexec() { sed 's|/usr/sbin/sshd|sshd|g' | PATH="$test_root/bin:$PATH" bash -se; }
HERDR_TEST_DNS=no ssh_policy
HERDR_TEST_DNS=yes ssh_policy
printf 'user=dev,host=127.0.0.1,addr=127.0.0.1,laddr=127.0.0.1,lport=22\nuser=dev,host=localhost,addr=127.0.0.1,laddr=127.0.0.1,lport=22\n' |
  cmp - "$HERDR_CONTEXT_FILE"

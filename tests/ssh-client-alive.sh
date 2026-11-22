#!/usr/bin/env bash
# Exercise the generic guest keepalive policy without changing host SSH.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/subyard-ssh-client-alive.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT
mkdir "$scratch/bin"
export PATH="$scratch/bin:$PATH" CLIENT_ALIVE_TEST_CALLS="$scratch/calls"
cat > "$scratch/bin/sshd" <<'EOF'
#!/bin/sh
case "$*" in
  -t) printf 'check syntax\n' >> "$CLIENT_ALIVE_TEST_CALLS" ;;
  -T) printf 'clientaliveinterval 90\nclientalivecountmax 6\n' ;;
  *) exit 1 ;;
esac
EOF
cat > "$scratch/bin/systemctl" <<'EOF'
#!/bin/sh
[ "$*" = 'reload ssh.service' ] || exit 1
printf 'reload\n' >> "$CLIENT_ALIVE_TEST_CALLS"
EOF
chmod 0755 "$scratch/bin/"*
policy="$scratch/45-subyard-client-alive.conf"
sed -e "s|^policy=.*|policy=$policy|" -e "s|/usr/sbin/sshd|$scratch/bin/sshd|" \
    -e "s/ = 0:644 / = $(id -u):644 /" "$ROOT/scripts/lib/sshd.sh" "$ROOT/scripts/ssh-client-alive.sh" > "$scratch/helper.sh"

if sh "$scratch/helper.sh" check; then exit 1; fi
sh "$scratch/helper.sh" ensure
printf 'ClientAliveInterval 30\nClientAliveCountMax 3\n' | cmp - "$policy"
[ "$(stat -c %a "$policy")" = 644 ]
sh "$scratch/helper.sh" check
sh "$scratch/helper.sh" ensure
printf 'check syntax\nreload\n' | cmp - "$CLIENT_ALIVE_TEST_CALLS"

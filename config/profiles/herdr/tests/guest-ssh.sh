#!/usr/bin/env bash
set -euo pipefail
test_root=$(mktemp -d /tmp/subyard-herdr-policy.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
mkdir "$test_root/bin"
export PATH="$test_root/bin:$PATH" DEV_USER=dev YARD_KIND=container HERDR_TEST_CALLS="$test_root/calls"
cat > "$test_root/bin/sshd" <<'SSHD'
#!/bin/sh
[ "$*" = '-t' ] || exit 1
printf 'check syntax\n' >> "$HERDR_TEST_CALLS"
SSHD
cat > "$test_root/bin/systemctl" <<'SYSTEMCTL'
#!/bin/sh
[ "$*" = 'reload ssh.service' ] || exit 1
printf 'reload\n' >> "$HERDR_TEST_CALLS"
SYSTEMCTL
printf '#!/bin/sh\nprintf "yes\\n"\n' > "$test_root/bin/loginctl"
chmod 0755 "$test_root/bin/"*
policy="$test_root/00-subyard-herdr.conf"
sed -e "s|^policy=.*|policy=$policy|" -e "s|/usr/sbin/sshd|$test_root/bin/sshd|" \
    -e "s/ = 0:644 / = $(id -u):644 /" config/profiles/herdr/guest-ssh.sh > "$test_root/guest-ssh.sh"
cp config/profiles/herdr/provision.sh "$test_root/provision.sh"

rc=0
bash "$test_root/provision.sh" --check || rc=$?
[ "$rc" = 10 ] && [ ! -e "$HERDR_TEST_CALLS" ]
bash "$test_root/provision.sh"
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' | cmp - "$policy"
[ "$(stat -c %a "$policy")" = 644 ]
bash "$test_root/provision.sh" --check
bash "$test_root/provision.sh"
printf 'check syntax\nreload\n' | cmp - "$HERDR_TEST_CALLS"
sh "$test_root/guest-ssh.sh" remove
sh "$test_root/guest-ssh.sh" absent
sh "$test_root/guest-ssh.sh" remove
printf 'check syntax\nreload\ncheck syntax\nreload\n' | cmp - "$HERDR_TEST_CALLS"

printf 'PasswordAuthentication yes\n' > "$policy"
if sh "$test_root/guest-ssh.sh" remove 2>/dev/null; then exit 1; fi
grep -Fxq 'PasswordAuthentication yes' "$policy"

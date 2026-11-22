#!/usr/bin/env bash
set -euo pipefail
test_root=$(mktemp -d /tmp/subyard-herdr-policy.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
mkdir "$test_root/bin"
export PATH="$test_root/bin:$PATH" DEV_USER=dev HERDR_TEST_CALLS="$test_root/calls"
cat > "$test_root/bin/sshd" <<'SSHD'
#!/bin/sh
case "$*" in
  -t) printf 'check syntax\n' >> "$HERDR_TEST_CALLS" ;;
  -T)
    printf 'check effective configuration\n' >> "$HERDR_TEST_CALLS"
    printf 'passwordauthentication %s\nkbdinteractiveauthentication %s\n' \
      "${HERDR_TEST_PASSWORD:-no}" "${HERDR_TEST_INTERACTIVE:-no}"
    printf 'clientaliveinterval 30\nclientalivecountmax 3\n' ;;
  *) exit 1 ;;
esac
SSHD
cat > "$test_root/bin/systemctl" <<'SYSTEMCTL'
#!/bin/sh
[ "$*" = 'reload ssh.service' ] || exit 1
printf 'reload\n' >> "$HERDR_TEST_CALLS"
SYSTEMCTL
printf '#!/bin/sh\nprintf "yes\\n"\n' > "$test_root/bin/loginctl"
chmod 0755 "$test_root/bin/"*
policy="$test_root/00-subyard-herdr.conf"
keepalive="$test_root/45-subyard-client-alive.conf"
render() {
  sed -e "s|^policy=.*|policy=$1|" -e "s|/usr/sbin/sshd|$test_root/bin/sshd|" \
      -e "s/ = 0:644 / = $(id -u):644 /" scripts/lib/sshd.sh "$2" > "$3"
}
render "$policy" config/profiles/herdr/guest-ssh.sh "$test_root/guest-ssh.sh"
render "$keepalive" scripts/ssh-client-alive.sh "$test_root/ssh-client-alive.sh"
cp config/profiles/herdr/provision.sh "$test_root/provision.sh"

bash "$test_root/provision.sh" --check
bash "$test_root/provision.sh"
[ ! -e "$policy" ] && [ ! -e "$HERDR_TEST_CALLS" ]

sh "$test_root/ssh-client-alive.sh" ensure
cp "$HERDR_TEST_CALLS" "$test_root/core-calls"
printf 'check syntax\ncheck effective configuration\nreload\n' | cmp - "$test_root/core-calls"
: > "$HERDR_TEST_CALLS"
sh "$test_root/guest-ssh.sh" ensure
cmp "$HERDR_TEST_CALLS" "$test_root/core-calls"
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' | cmp - "$policy"
[ "$(stat -c %a "$policy")" = 644 ]
sh "$test_root/guest-ssh.sh" check
cmp "$HERDR_TEST_CALLS" "$test_root/core-calls"
sh "$test_root/guest-ssh.sh" ensure
printf 'check syntax\ncheck effective configuration\nreload\ncheck effective configuration\n' | cmp - "$HERDR_TEST_CALLS"
: > "$HERDR_TEST_CALLS"
sh "$test_root/guest-ssh.sh" remove
sh "$test_root/guest-ssh.sh" absent
sh "$test_root/guest-ssh.sh" remove
printf 'check syntax\nreload\n' | cmp - "$HERDR_TEST_CALLS"

: > "$HERDR_TEST_CALLS"
printf 'PasswordAuthentication yes\nKbdInteractiveAuthentication yes\n' > "$test_root/00-operator.conf"
if HERDR_TEST_PASSWORD=yes HERDR_TEST_INTERACTIVE=yes sh "$test_root/guest-ssh.sh" ensure 2> "$test_root/error"; then exit 1; fi
grep -Fq 'Remove the overriding PasswordAuthentication directive' "$test_root/error"
grep -Fq 'Remove the overriding KbdInteractiveAuthentication directive' "$test_root/error"
sh "$test_root/guest-ssh.sh" absent
printf 'check syntax\ncheck effective configuration\n' | cmp - "$HERDR_TEST_CALLS"
printf 'PasswordAuthentication yes\nKbdInteractiveAuthentication yes\n' | cmp - "$test_root/00-operator.conf"

printf 'PasswordAuthentication yes\n' > "$policy"
if sh "$test_root/guest-ssh.sh" remove 2>/dev/null; then exit 1; fi
grep -Fxq 'PasswordAuthentication yes' "$policy"

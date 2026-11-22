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
  '-T -C user=dev,addr=127.0.0.1,host=localhost,laddr=127.0.0.1,lport=22')
    printf 'check effective configuration\n' >> "$HERDR_TEST_CALLS"
    # A Match block for the development user overrides only the connection-specific result.
    printf 'passwordauthentication %s\nkbdinteractiveauthentication %s\n' \
      "${HERDR_TEST_MATCH_PASSWORD:-${HERDR_TEST_PASSWORD:-no}}" "${HERDR_TEST_INTERACTIVE:-no}" ;;
  -T)
    printf 'passwordauthentication %s\nkbdinteractiveauthentication %s\n' \
      "${HERDR_TEST_PASSWORD:-no}" "${HERDR_TEST_INTERACTIVE:-no}" ;;
  *) exit 1 ;;
esac
SSHD
cat > "$test_root/bin/systemctl" <<'SYSTEMCTL'
#!/bin/sh
[ "$*" = 'reload ssh.service' ] || exit 1
printf 'reload\n' >> "$HERDR_TEST_CALLS"
SYSTEMCTL
chmod 0755 "$test_root/bin/"*
policy="$test_root/00-subyard-herdr.conf"
sed -e "s|^policy=.*|policy=$policy|" -e "s|/usr/sbin/sshd|$test_root/bin/sshd|" \
    -e "s/ = 0:644 / = $(id -u):644 /" scripts/lib/sshd.sh config/profiles/herdr/guest-ssh.sh > "$test_root/guest-ssh.sh"
printf 'check syntax\ncheck effective configuration\nreload\n' > "$test_root/install-calls"
printf '# Managed by the Subyard Herdr profile\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n' > "$test_root/expected-policy"
sh "$test_root/guest-ssh.sh" ensure dev
cmp "$HERDR_TEST_CALLS" "$test_root/install-calls"
cmp "$test_root/expected-policy" "$policy"
[ "$(stat -c %a "$policy")" = 644 ]
sh "$test_root/guest-ssh.sh" check
cmp "$HERDR_TEST_CALLS" "$test_root/install-calls"
sh "$test_root/guest-ssh.sh" ensure dev
printf 'check syntax\ncheck effective configuration\nreload\ncheck effective configuration\n' | cmp - "$HERDR_TEST_CALLS"
: > "$HERDR_TEST_CALLS"
sh "$test_root/guest-ssh.sh" remove
sh "$test_root/guest-ssh.sh" absent
sh "$test_root/guest-ssh.sh" remove
printf 'check syntax\nreload\n' | cmp - "$HERDR_TEST_CALLS"

: > "$HERDR_TEST_CALLS"
printf 'PasswordAuthentication yes\nKbdInteractiveAuthentication yes\n' > "$test_root/00-operator.conf"
if HERDR_TEST_PASSWORD=yes HERDR_TEST_INTERACTIVE=yes sh "$test_root/guest-ssh.sh" ensure dev 2> "$test_root/error"; then exit 1; fi
grep -Fq 'Remove the overriding PasswordAuthentication directive' "$test_root/error"
grep -Fq 'Remove the overriding KbdInteractiveAuthentication directive' "$test_root/error"
sh "$test_root/guest-ssh.sh" absent
printf 'check syntax\ncheck effective configuration\n' | cmp - "$HERDR_TEST_CALLS"
printf 'PasswordAuthentication yes\nKbdInteractiveAuthentication yes\n' | cmp - "$test_root/00-operator.conf"

tail -n +2 "$test_root/expected-policy" > "$test_root/original-policy"
install -m 0644 "$test_root/original-policy" "$policy"
if sh "$test_root/guest-ssh.sh" check; then exit 1; fi
if HERDR_TEST_PASSWORD=yes sh "$test_root/guest-ssh.sh" ensure dev 2>/dev/null; then exit 1; fi
cmp "$test_root/original-policy" "$policy"
sh "$test_root/guest-ssh.sh" ensure dev
cmp "$test_root/expected-policy" "$policy"
sh "$test_root/guest-ssh.sh" check

printf 'PasswordAuthentication yes\n' > "$policy"
if sh "$test_root/guest-ssh.sh" remove 2>/dev/null; then exit 1; fi
grep -Fxq 'PasswordAuthentication yes' "$policy"

: > "$HERDR_TEST_CALLS"
rm -f -- "$policy" "$test_root/00-operator.conf"
# A Match block that enables passwords for the development user is a refusal.
if HERDR_TEST_MATCH_PASSWORD=yes sh "$test_root/guest-ssh.sh" ensure dev 2> "$test_root/error"; then exit 1; fi
grep -Fq 'Remove the overriding PasswordAuthentication directive' "$test_root/error"
sh "$test_root/guest-ssh.sh" absent
if sh "$test_root/guest-ssh.sh" ensure 2>/dev/null; then exit 1; fi

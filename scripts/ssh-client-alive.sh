#!/bin/sh
# Configure server-side liveness checks for yard SSH connections.
# The SSH access stage supplies lib/sshd.sh for ensure. The check runs independently.
set -eu

policy=/etc/ssh/sshd_config.d/45-subyard-client-alive.conf
desired='ClientAliveInterval 30
ClientAliveCountMax 3'

effective() {
    current=$(/usr/sbin/sshd -T) || return 1
    printf '%s\n' "$current" | grep -qx 'clientaliveinterval 30' &&
        printf '%s\n' "$current" | grep -qx 'clientalivecountmax 3'
}

matches() {
    [ ! -L "$policy" ] && [ -f "$policy" ] && [ "$(stat -c '%u:%a' "$policy")" = 0:644 ] &&
        printf '%s\n' "$desired" | cmp -s - "$policy"
}

case "${1:-}" in
    check) matches && effective; exit ;;
    ensure) ;;
    *) exit 2 ;;
esac
if matches && effective; then exit 0; fi
if [ -e "$policy" ] || [ -L "$policy" ]; then
    printf 'SSH ClientAlive policy is unsafe or overridden: %s\n' "$policy" >&2
    exit 1
fi
stage=$(mktemp "${policy}.XXXXXX")
trap 'rm -f -- "$stage"' EXIT
printf '%s\n' "$desired" > "$stage"
chmod 0644 "$stage"
mv -T -- "$stage" "$policy"
if ! reload_guest_sshd effective; then
    rm -f -- "$policy"
    printf 'Could not activate guest SSH ClientAlive policy\n' >&2
    exit 1
fi

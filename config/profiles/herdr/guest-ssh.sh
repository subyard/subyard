#!/bin/sh
# The Herdr profile owns this drop-in. Route checks use only the check action.
set -eu

policy=/etc/ssh/sshd_config.d/00-subyard-herdr.conf
body='PasswordAuthentication no
KbdInteractiveAuthentication no'

absent() { [ ! -e "$policy" ] && [ ! -L "$policy" ]; }
matches() {
    [ ! -L "$policy" ] && [ -f "$policy" ] && [ "$(stat -c '%u:%a' "$policy")" = 0:644 ] &&
        printf '%s\n' "$body" | cmp -s - "$policy"
}
reload_ssh() { /usr/sbin/sshd -t && systemctl reload ssh.service; }
refuse() { printf 'Herdr: %s\n' "$*" >&2; exit 1; }

case "${1:-}" in
    check) matches; exit ;;
    absent) absent; exit ;;
    ensure)
        if matches; then exit 0; fi
        absent || refuse "SSH drop-in differs from the owned policy: $policy"
        stage=$(mktemp "${policy}.XXXXXX")
        trap 'rm -f -- "$stage"' EXIT
        printf '%s\n' "$body" > "$stage"
        chmod 0644 "$stage"
        mv -T -- "$stage" "$policy"
        if ! reload_ssh; then
            rm -f -- "$policy"
            refuse 'could not activate the Herdr SSH drop-in'
        fi ;;
    remove)
        if absent; then exit 0; fi
        matches || refuse "refusing to remove a changed SSH drop-in: $policy"
        previous=$(mktemp "${policy}.XXXXXX")
        mv -T -- "$policy" "$previous"
        if ! reload_ssh; then
            mv -T -- "$previous" "$policy"
            refuse 'could not retire the Herdr SSH drop-in'
        fi
        rm -f -- "$previous" ;;
    *) exit 2 ;;
esac

#!/bin/sh
# The Herdr profile owns this drop-in. Route checks use only the check action.
set -eu

policy=/etc/ssh/sshd_config.d/00-subyard-herdr.conf
marker='# Managed by the Subyard Herdr profile'
original='PasswordAuthentication no
KbdInteractiveAuthentication no'
body="$marker
PasswordAuthentication no
KbdInteractiveAuthentication no"

absent() { [ ! -e "$policy" ] && [ ! -L "$policy" ]; }
regular_policy() {
    [ ! -L "$policy" ] && [ -f "$policy" ] && [ "$(stat -c '%u:%a' "$policy")" = 0:644 ]
}
matches() { regular_policy && printf '%s\n' "$body" | cmp -s - "$policy"; }
# Ensure rewrites an owned file, so a drifted mode alone does not disown it.
root_policy() {
    [ ! -L "$policy" ] && [ -f "$policy" ] && [ "$(stat -c '%u' "$policy")" = 0 ] &&
        [ $(( 0$(stat -c '%a' "$policy") & 07022 )) = 0 ]
}
owned() {
    root_policy &&
        { [ "$(head -n 1 "$policy")" = "$marker" ] || printf '%s\n' "$original" | cmp -s - "$policy"; }
}
refuse() { printf 'Herdr: %s\n' "$*" >&2; exit 1; }

effective() {
    # Evaluate Match blocks for a proxied login of the development user.
    current=$(/usr/sbin/sshd -T -C "user=${user:?},addr=127.0.0.1,host=localhost,laddr=127.0.0.1,lport=22") || return 1
    valid=0
    if ! printf '%s\n' "$current" | grep -x 'passwordauthentication no' >/dev/null; then
        printf 'Herdr: Remove the overriding PasswordAuthentication directive from the guest SSH configuration before publishing Herdr.\n' >&2
        valid=1
    fi
    if ! printf '%s\n' "$current" | grep -x 'kbdinteractiveauthentication no' >/dev/null; then
        printf 'Herdr: Remove the overriding KbdInteractiveAuthentication directive from the guest SSH configuration before publishing Herdr.\n' >&2
        valid=1
    fi
    return "$valid"
}

user="${2:-}"
case "${1:-}" in
    check) matches; exit ;;
    absent) absent; exit ;;
    ensure)
        if matches; then effective; exit; fi
        stage=$(mktemp "${policy}.XXXXXX")
        trap 'rm -f -- "$stage"' EXIT
        previous=
        if ! absent; then
            owned || refuse "SSH drop-in differs from the owned policy: $policy"
            previous=$(mktemp "${policy}.XXXXXX")
            cp -p -- "$policy" "$previous"
        fi
        printf '%s\n' "$body" > "$stage"
        chmod 0644 "$stage"
        mv -T -- "$stage" "$policy"
        if ! reload_guest_sshd effective; then
            if [ -n "$previous" ]; then
                mv -T -- "$previous" "$policy"
            else
                rm -f -- "$policy"
            fi
            refuse 'could not activate the Herdr SSH drop-in'
        fi
        if [ -n "$previous" ]; then rm -f -- "$previous"; fi ;;
    remove)
        if absent; then exit 0; fi
        owned || refuse "refusing to remove an unowned SSH drop-in: $policy"
        previous=$(mktemp "${policy}.XXXXXX")
        mv -T -- "$policy" "$previous"
        if ! reload_guest_sshd :; then
            mv -T -- "$previous" "$policy"
            refuse 'could not retire the Herdr SSH drop-in'
        fi
        rm -f -- "$previous" ;;
    *) exit 2 ;;
esac

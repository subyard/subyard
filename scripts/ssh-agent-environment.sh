#!/bin/sh
# Generic guest shell and OpenSSH agent defaults. Profile services own their unit hooks.
set -eu
mode=${1:-}
dev_user=${2:-}
case "$mode" in ensure|check) ;; *) exit 2 ;; esac
case "$dev_user" in ''|[!a-zA-Z_]*|*[!a-zA-Z0-9_-]*) exit 2 ;; esac
[ "$#" -eq 2 ] || exit 2

profile=/etc/profile.d/subyard-ssh-agent.sh
client_dir=/etc/ssh/ssh_config.d
client_config=$client_dir/50-subyard-agent.conf
profile_body=$(cat <<EOF
# Managed by Subyard. Preserve session-forwarded agents when present.
if [ "\${HOME:-}" = /home/$dev_user ] && [ -z "\${SSH_AUTH_SOCK:-}" ]; then
    SSH_AUTH_SOCK=/home/$dev_user/.ssh/subyard-agent.sock
    export SSH_AUTH_SOCK
fi
EOF
)
# OpenSSH 9.6 Match parsing rejects nested escaped quotes. Expand only a constant
# presence marker so socket contents never undergo shell splitting or globbing.
client_body=$(cat <<'EOF'
# Managed by Subyard. User SSH configuration and session-forwarded agents take precedence.
Match exec "test x${SSH_AUTH_SOCK:+set} = x && test -S ~/.ssh/subyard-agent.sock"
    IdentityAgent ~/.ssh/subyard-agent.sock
Match all
EOF
)

safe_directory() {
    [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c %u "$1")" = 0 ] || return 1
    permissions=$(stat -c %a "$1")
    [ "$((0$permissions & 022))" -eq 0 ]
}
safe_file() {
    [ ! -L "$1" ] || return 1
    if [ -e "$1" ]; then
        [ -f "$1" ] && [ "$(stat -c %u "$1")" = 0 ] || return 1
    fi
}
matches() {
    [ -f "$1" ] && [ "$(stat -c %a "$1")" = 644 ] &&
        printf '%s\n' "$2" | cmp -s - "$1"
}
publish() {
    temporary=$(mktemp "${1}.XXXXXX")
    trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
    printf '%s\n' "$2" > "$temporary"
    chown 0:0 "$temporary"
    chmod 644 "$temporary"
    mv -f -- "$temporary" "$1"
    trap - EXIT HUP INT TERM
}

for directory in /etc /etc/profile.d /etc/ssh; do
    safe_directory "$directory" || exit 1
done
safe_file "$profile" || exit 1
safe_file "$client_config" || exit 1
if [ -e "$client_dir" ] || [ -L "$client_dir" ]; then
    safe_directory "$client_dir" || exit 1
elif [ "$mode" = ensure ]; then
    mkdir -m 755 "$client_dir"
else
    exit 1
fi
if [ "$mode" = check ]; then
    matches "$profile" "$profile_body" && matches "$client_config" "$client_body"
    exit $?
fi

matches "$profile" "$profile_body" || publish "$profile" "$profile_body"
matches "$client_config" "$client_body" || publish "$client_config" "$client_body"
matches "$profile" "$profile_body" && matches "$client_config" "$client_body"

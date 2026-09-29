#!/bin/sh
# Orca guest environment: keep its systemd service on the Subyard agent socket.
set -eu
mode=${1:-}
dev_user=${2:-}
case "$mode" in ensure|check) ;; *) exit 2 ;; esac
case "$dev_user" in ''|[!a-zA-Z_]*|*[!a-zA-Z0-9_-]*) exit 2 ;; esac
[ "$#" -eq 2 ] || exit 2

unit_dir=/etc/systemd/system/subyard-orca.service.d
dropin=$unit_dir/50-subyard-ssh-agent.conf
pending=$unit_dir/.subyard-ssh-agent-refresh-pending
dropin_body=$(printf '[Service]\nEnvironment=SSH_AUTH_SOCK=/home/%s/.ssh/subyard-agent.sock' "$dev_user")

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

for directory in /etc /etc/systemd /etc/systemd/system; do
    safe_directory "$directory" || exit 1
done
for file in "$dropin" "$pending"; do safe_file "$file" || exit 1; done
if [ -e "$unit_dir" ] || [ -L "$unit_dir" ]; then
    safe_directory "$unit_dir" || exit 1
elif [ "$mode" = ensure ]; then
    mkdir -m 755 "$unit_dir"
else
    exit 1
fi
if [ "$mode" = check ]; then
    matches "$dropin" "$dropin_body" && [ ! -e "$pending" ]
    exit $?
fi

if ! matches "$dropin" "$dropin_body"; then
    # Persist refresh intent before changing the unit so interrupted repair retries.
    publish "$pending" 'pending'
    publish "$dropin" "$dropin_body"
fi
if [ -e "$pending" ]; then
    systemctl daemon-reload
    if systemctl is-active --quiet subyard-orca.service; then
        systemctl restart subyard-orca.service
    fi
    rm -f -- "$pending"
fi
matches "$dropin" "$dropin_body"

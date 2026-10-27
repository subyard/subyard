
destination=$1
uid=$2
directory=${destination%/*}
install -d -m 0755 -o "$uid" -g "$uid" "$directory"
temporary=$(mktemp "$directory/.subyard-config.XXXXXX")
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
cat > "$temporary"
chown "$uid:$uid" "$temporary"
chmod 0644 "$temporary"
mv -f -- "$temporary" "$destination"
trap - EXIT HUP INT TERM

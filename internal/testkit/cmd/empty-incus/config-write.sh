
destination=$1
uid=$2
digest=$3
directory=${destination%/*}
install -d -m 0755 -o "$uid" -g "$uid" "$directory"
temporary=$(mktemp "$directory/.subyard-config.XXXXXX")
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
cat > "$temporary"
[ "$(sha256sum "$temporary" | cut -d ' ' -f 1)" = "$digest" ]
chown "$uid:$uid" "$temporary"
chmod 0644 "$temporary"
python3 -B -c 'import os,sys; f=os.open(sys.argv[1],os.O_RDONLY); os.fsync(f); os.close(f)' "$temporary"
mv -f -- "$temporary" "$destination"
python3 -B -c 'import os,sys; f=os.open(sys.argv[1],os.O_RDONLY|os.O_DIRECTORY); os.fsync(f); os.close(f)' "$directory"
[ "$(sha256sum "$destination" | cut -d ' ' -f 1)" = "$digest" ]
trap - EXIT HUP INT TERM

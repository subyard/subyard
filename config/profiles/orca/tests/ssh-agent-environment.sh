#!/usr/bin/env bash
# Exercise Orca's systemd agent environment without root or a running manager.
set -euo pipefail
umask 022
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
mkdir -p "$TMP/etc/systemd/system" "$TMP/bin"
sed "s|/etc/|$TMP/etc/|g" "$ROOT/config/profiles/orca/ssh-agent-environment.sh" > "$TMP/setup.sh"
cat > "$TMP/bin/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$TEST_ROOT/systemctl.log"
case "$1" in
  is-active) test -f "$TEST_ROOT/active" ;;
  restart) test ! -f "$TEST_ROOT/restart-fails" ;;
esac
EOF
cat > "$TMP/bin/chown" <<'EOF'
#!/bin/sh
exit 0
EOF
cat > "$TMP/bin/stat" <<'EOF'
#!/bin/sh
if [ "$1" = -c ] && [ "$2" = %u ]; then printf '0\n'; else exec /usr/bin/stat "$@"; fi
EOF
chmod +x "$TMP/bin/"*
export PATH="$TMP/bin:$PATH" TEST_ROOT="$TMP"
run() { sh -eu "$TMP/setup.sh" "$@" dev; }
run check && fail 'missing Orca environment considered converged'
touch "$TMP/active"
run ensure
dropin="$TMP/etc/systemd/system/subyard-orca.service.d/50-subyard-ssh-agent.conf"
pending="$TMP/etc/systemd/system/subyard-orca.service.d/.subyard-ssh-agent-refresh-pending"
run check || fail 'installed Orca environment did not converge'
grep -Fxq 'Environment=SSH_AUTH_SOCK=/home/dev/.ssh/subyard-agent.sock' "$dropin" \
  || fail 'Orca service lacks fixed agent socket'
[ "$(grep -c '^restart subyard-orca.service$' "$TMP/systemctl.log")" = 1 ] \
  || fail 'initial running Orca environment was not refreshed'
run ensure
[ "$(grep -c '^restart subyard-orca.service$' "$TMP/systemctl.log")" = 1 ] \
  || fail 'repeat ensure restarted Orca'
printf 'drift\n' > "$dropin"
touch "$TMP/restart-fails"
run ensure && fail 'failed Orca refresh accepted'
[ -e "$pending" ] || fail 'failed refresh did not leave durable pending marker'
run check && fail 'pending Orca refresh considered converged'
rm "$TMP/restart-fails"
run ensure
run check || fail 'failed refresh did not recover'
[ "$(grep -c '^restart subyard-orca.service$' "$TMP/systemctl.log")" = 3 ] \
  || fail 'failed refresh was not retried'
rm "$TMP/active"
printf 'drift\n' > "$dropin"
run ensure
run check || fail 'stopped Orca service was not repaired'
[ "$(grep -c '^restart subyard-orca.service$' "$TMP/systemctl.log")" = 3 ] \
  || fail 'stopped Orca service was restarted'
chmod 666 "$dropin"
run check && fail 'unsafe drop-in permissions considered converged'
run ensure
[ "$(stat -c %a "$dropin")" = 644 ] || fail 'unsafe permissions not repaired'
mv "$dropin" "$TMP/preserved-dropin"
ln -s "$TMP/preserved-dropin" "$dropin"
run ensure && fail 'drop-in symlink accepted'
cmp "$TMP/preserved-dropin" "$dropin" || fail 'drop-in symlink target changed'
sh -eu "$TMP/setup.sh" ensure 'bad;user' && fail 'unsafe username accepted'
printf 'PASS: Orca SSH agent environment\n'

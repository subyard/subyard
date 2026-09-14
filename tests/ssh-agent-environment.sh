#!/usr/bin/env bash
# Exercise guest environment setup without root or a running systemd manager.
set -euo pipefail
umask 022
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
mkdir -p "$TMP/etc/profile.d" "$TMP/etc/systemd/system" "$TMP/etc/ssh" "$TMP/bin" "$TMP/home/.ssh"
# Redirect the guest's protected paths; all writes and shell evaluation remain real.
sed "s|/etc/|$TMP/etc/|g" "$ROOT/scripts/ssh-agent-environment.sh" > "$TMP/setup.sh"
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
run check && fail 'missing environment considered converged'
touch "$TMP/active"
run ensure
run check || fail 'installed environment did not converge'
profile="$TMP/etc/profile.d/subyard-ssh-agent.sh"
dropin="$TMP/etc/systemd/system/subyard-orca.service.d/50-subyard-ssh-agent.conf"
client_config="$TMP/etc/ssh/ssh_config.d/50-subyard-agent.conf"
cat > "$TMP/ssh_config" <<EOF
Host explicit-user
    IdentityAgent /tmp/subyard-fixture-user-agent.sock
Host *
    Include "$client_config"
EOF
expect_agent_setting() {
  local expected="$1" host="$2" message="$3" settings actual
  settings="$(HOME="$TMP/home" ssh -G -T -F "$TMP/ssh_config" "$host")" \
    || fail 'OpenSSH rejected the managed client configuration'
  actual="$(awk '$1 == "identityagent" {print $2}' <<< "$settings")"
  [ "$actual" = "$expected" ] || fail "$message"
}
unset SSH_AUTH_SOCK
expect_agent_setting "" ordinary-session 'absent yard socket changed OpenSSH defaults'
python3 - "$TMP/home/.ssh/subyard-agent.sock" <<'PY'
import socket
import sys
with socket.socket(socket.AF_UNIX) as agent:
    agent.bind(sys.argv[1])
PY
login_home="$(getent passwd "$(id -u)" | cut -d: -f6)"
expect_agent_setting "$login_home/.ssh/subyard-agent.sock" ordinary-session \
  'existing session without SSH_AUTH_SOCK lacks the OpenSSH fallback'
expect_agent_setting /tmp/subyard-fixture-user-agent.sock explicit-user \
  'system fallback replaced explicit user IdentityAgent'
SSH_AUTH_SOCK="/tmp/session agent" expect_agent_setting "" ordinary-session \
  'system fallback replaced a session-forwarded SSH_AUTH_SOCK'
cat > "$client_config" <<'EOF'
# Legacy managed default from the original provisioning stage.
Match exec "test -S ~/.subyard/run/ssh-agent.sock"
    IdentityAgent ~/.subyard/run/ssh-agent.sock
Match all
EOF
run check && fail 'legacy OpenSSH fallback considered converged'
run ensure
run check || fail 'OpenSSH fallback did not recover from drift'
expect_agent_setting "$login_home/.ssh/subyard-agent.sock" ordinary-session \
  'OpenSSH fallback drift was not repaired'
chmod 666 "$client_config"
run check && fail 'unsafe OpenSSH fallback permissions considered converged'
run ensure
run check || fail 'OpenSSH fallback permissions did not recover'
mv "$client_config" "$TMP/preserved-client-config"
ln -s "$TMP/preserved-client-config" "$client_config"
run ensure && fail 'OpenSSH fallback symlink accepted'
cmp "$TMP/preserved-client-config" "$client_config" || fail 'OpenSSH symlink target changed'
rm "$client_config"
mv "$TMP/preserved-client-config" "$client_config"
socket="$(env -u SSH_AUTH_SOCK HOME=/home/dev sh -eu -c '. "$1"; printf %s "$SSH_AUTH_SOCK"' -- "$profile")"
[ "$socket" = /home/dev/.ssh/subyard-agent.sock ] || fail 'shell lacks fixed agent socket'
socket="$(SSH_AUTH_SOCK=/tmp/session-agent HOME=/home/dev sh -eu -c '. "$1"; printf %s "$SSH_AUTH_SOCK"' -- "$profile")"
[ "$socket" = /tmp/session-agent ] || fail 'forwarded socket was replaced'
socket="$(env -u SSH_AUTH_SOCK HOME=/root sh -eu -c '. "$1"; printf %s "${SSH_AUTH_SOCK:-}"' -- "$profile")"
[ -z "$socket" ] || fail 'fallback leaked to another account'
grep -Fxq 'Environment=SSH_AUTH_SOCK=/home/dev/.ssh/subyard-agent.sock' "$dropin" \
  || fail 'Orca service lacks fixed agent socket'
[ "$(grep -c '^restart subyard-orca.service$' "$TMP/systemctl.log")" = 1 ] \
  || fail 'initial running Orca environment was not refreshed'
run ensure
[ "$(grep -c '^restart subyard-orca.service$' "$TMP/systemctl.log")" = 1 ] \
  || fail 'repeat ensure restarted Orca'
chmod 666 "$profile"
run check && fail 'unsafe profile permissions considered converged'
run ensure
[ "$(stat -c %a "$profile")" = 644 ] || fail 'unsafe permissions not repaired'
printf 'drift\n' > "$dropin"
touch "$TMP/restart-fails"
run ensure && fail 'failed Orca refresh accepted'
run check && fail 'pending Orca refresh considered converged'
rm "$TMP/restart-fails"
run ensure
run check || fail 'failed refresh did not recover'
[ "$(grep -c '^restart subyard-orca.service$' "$TMP/systemctl.log")" = 3 ] \
  || fail 'failed refresh was not retried'
mv "$profile" "$TMP/preserved-profile"
ln -s "$TMP/preserved-profile" "$profile"
run ensure && fail 'profile symlink accepted'
cmp "$TMP/preserved-profile" "$profile" || fail 'symlink target changed'
sh -eu "$TMP/setup.sh" ensure 'bad;user' && fail 'unsafe username accepted'
printf 'PASS: guest SSH agent environment\n'

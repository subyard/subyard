#!/usr/bin/env bash
# Actual HTTP and process contract for the foreground preview helper.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 "$ROOT/tests/preview.py"

TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export HOME="$TMP/home" ASSUME_YES=1
mkdir -p "$HOME" "$TMP/bin"
cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  info) ;;
  list) printf 'RUNNING\n' ;;
  config)
    case "$3" in
      list) printf 'ssh\n' ;;
      get)
        case "$6" in
          type) printf 'proxy\n' ;;
          listen) printf 'tcp:127.0.0.1:%s\n' "$SSH_PORT" ;;
          connect) printf 'tcp:127.0.0.1:22\n' ;;
        esac ;;
      *) exit 90 ;;
    esac ;;
  exec)
    if [[ "$*" == *ssh_host_ed25519_key.pub* ]]; then
      printf 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n'
    else
      cat >/dev/null
    fi ;;
  *) exit 90 ;;
esac
MOCK
printf '#!/bin/sh\nexit 0\n' > "$TMP/bin/ssh"
chmod 0755 "$TMP/bin/incus" "$TMP/bin/ssh"
export PATH="$TMP/bin:$PATH"
for name in default demo demo-code; do
  export YARD_NAME="$name"
  if [ "$name" = default ]; then
    export SSH_HOST=yard
    snippet="$HOME/.ssh/subyard.config"
  else
    export SSH_HOST="yard-$name"
    snippet="$HOME/.ssh/subyard-$name.config"
  fi
  bash "$ROOT/scripts/07-ssh-access.sh" > "$TMP/access.out" 2>&1 \
    || { cat "$TMP/access.out" >&2; fail "$name SSH setup"; }
  normal="$(/usr/bin/ssh -G -F "$snippet" "$SSH_HOST" 2>/dev/null)"
  ! grep -q '^localforward ' <<< "$normal" || fail 'ordinary alias forwards preview'
  ! grep -q '\.code' "$snippet" || fail 'SSH snippet retains a code alias'
  grep -qx 'identitiesonly yes' <<< "$normal" || fail 'ordinary alias lost dedicated transport identity'
  grep -qx 'stricthostkeychecking true' <<< "$normal" || fail 'ordinary alias lost strict host-key checking'
  before="$(sha256sum "$snippet")"
  bash "$ROOT/scripts/07-ssh-access.sh" > "$TMP/access.out" 2>&1 \
    || { cat "$TMP/access.out" >&2; fail "$name repeated SSH setup"; }
  [ "$(sha256sum "$snippet")" = "$before" ] || fail 'repeated SSH setup changed snippet'
done
printf 'ok: local and named SSH options without preview forwarding and repeat setup\n'

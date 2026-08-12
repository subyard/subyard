#!/usr/bin/env bash
# SSH config updates avoid predictable stale temp files and remain atomic/idempotent.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=scripts/lib/ssh-config.sh
. "$ROOT/scripts/lib/ssh-config.sh"

sshdir="$TMP/.ssh"
config="$sshdir/config"
legacy_temp="$config.tmp"
mkdir -m 0700 "$sshdir"
printf 'Host existing\n    HostName example.test\n' > "$config"
printf 'legacy-stale-temp\n' > "$legacy_temp"
legacy_digest="$(sha256sum "$legacy_temp" | awk '{print $1}')"
chmod 000 "$legacy_temp"

ssh_config_prepend_once "$config" 'Include subyard-e2e-yard.config' \
  || fail 'could not prepend with a stale predictable temp present'
ssh_config_prepend_once "$config" 'Include subyard-e2e-yard.config' \
  || fail 'idempotent prepend failed'

[ "$(grep -c '^Include subyard-e2e-yard\.config$' "$config")" -eq 1 ] \
  || fail 'Include was missing or duplicated'
[ "$(sed -n '1p' "$config")" = 'Include subyard-e2e-yard.config' ] \
  || fail 'Include was not prepended'
[ "$(stat -c '%a' "$legacy_temp")" = 0 ] || fail 'legacy temp mode was modified'
chmod 0600 "$legacy_temp"
[ "$(sha256sum "$legacy_temp" | awk '{print $1}')" = "$legacy_digest" ] \
  || fail 'legacy temp content was modified'
[ "$(stat -c '%a' "$config")" = 600 ] || fail 'SSH config mode is not 0600'
if find "$sshdir" -maxdepth 1 -name '.subyard-ssh-config.*' -print -quit | grep -q .; then
  fail 'atomic update left a staging file'
fi
ssh_config_remove_exact "$config" 'Include subyard-e2e-yard.config' \
  || fail 'could not remove the exact managed Include'
! grep -qxF 'Include subyard-e2e-yard.config' "$config" \
  || fail 'managed Include remained after rollback'
grep -qxF 'Host existing' "$config" \
  || fail 'Include rollback removed unrelated SSH config'
ssh_config_remove_exact "$config" 'Include subyard-e2e-yard.config' \
  || fail 'idempotent Include rollback failed'

known="$sshdir/known_hosts"
ssh-keygen -q -t ed25519 -N '' -f "$TMP/host-one"
ssh-keygen -q -t ed25519 -N '' -f "$TMP/host-two"
key_one="$(awk '{print $1 " " $2}' "$TMP/host-one.pub")"
key_two="$(awk '{print $1 " " $2}' "$TMP/host-two.pub")"
printf 'unrelated.example %s\n[127.0.0.1]:2223 %s\n' "$key_one" "$key_one" > "$known"
ssh_known_host_replace "$known" '[127.0.0.1]:2223' "$key_two" \
  || fail 'could not atomically replace a pinned yard host key'
ssh_known_host_replace "$known" '[127.0.0.1]:2223' "$key_two" \
  || fail 'idempotent host-key pin failed'
[ "$(ssh-keygen -F '[127.0.0.1]:2223' -f "$known" | grep -c '^\[127')" -eq 1 ] \
  || fail 'yard host-key pin was missing or duplicated'
grep -Fq "[127.0.0.1]:2223 $key_two" "$known" \
  || fail 'yard host-key pin was not rotated'
grep -Fq "unrelated.example $key_one" "$known" \
  || fail 'host-key rotation removed an unrelated pin'
[ "$(stat -c '%a' "$known")" = 600 ] || fail 'known_hosts mode is not 0600'
ssh_known_host_remove "$known" '[127.0.0.1]:2223' \
  || fail 'could not atomically remove a pinned yard host key'
! ssh-keygen -F '[127.0.0.1]:2223' -f "$known" | grep -q '^\[127' \
  || fail 'yard host-key pin remained after removal'
grep -Fq "unrelated.example $key_one" "$known" \
  || fail 'host-key removal removed an unrelated pin'
[ "$(stat -c '%a' "$known")" = 600 ] || fail 'known_hosts removal changed its mode'
[ ! -e "$known.old" ] || fail 'known_hosts removal left an ssh-keygen backup'

# Teardown runs as root and supplies the operator identity. Its shared lock must be handed
# back with known_hosts so a later unprivileged init is not poisoned by a root-owned lock.
ssh_known_host_remove "$known" 'missing.example' "$(id -un)" "$(id -gn)" \
  || fail 'owned host-key removal failed'
[ "$(stat -c '%u:%g:%a' "$sshdir/.subyard-known-hosts.lock")" \
    = "$(id -u):$(id -g):600" ] \
  || fail 'owned host-key removal did not preserve an operator-owned lock'

printf 'ok: SSH config and host-key pins are atomic, strict and idempotent\n'

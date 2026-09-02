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

# A missing config is a normal teardown no-op and must not leave behind a lock.
absent_dir="$TMP/absent/.ssh"
absent_config="$absent_dir/config"
mkdir -p "$TMP/absent"
mkdir -m 0700 "$absent_dir"
ssh_config_remove_exact "$absent_config" 'Include subyard-e2e-yard.config' \
  || fail 'missing SSH config was not accepted as an idempotent removal'
if [ -e "$absent_dir/.subyard-config.lock" ] || [ -L "$absent_dir/.subyard-config.lock" ]; then
  fail 'missing SSH config removal created a lock'
fi

# A regular config that cannot be read is an error, not an absent Include.
unreadable_dir="$TMP/unreadable/.ssh"
unreadable_config="$unreadable_dir/config"
mkdir -p "$TMP/unreadable"
mkdir -m 0700 "$unreadable_dir"
printf 'Include subyard-e2e-yard.config\nHost existing\n' > "$unreadable_config"
chmod 000 "$unreadable_config"
if ssh_config_remove_exact "$unreadable_config" 'Include subyard-e2e-yard.config'; then
  fail 'unreadable SSH config was accepted as an idempotent removal'
fi
chmod 0600 "$unreadable_config"
grep -qxF 'Include subyard-e2e-yard.config' "$unreadable_config" \
  || fail 'unreadable SSH config was modified'

# A symlink config is never an acceptable write target for teardown.
symlink_dir="$TMP/symlink/.ssh"
symlink_config="$symlink_dir/config"
symlink_canary="$TMP/symlink-canary"
mkdir -p "$TMP/symlink"
mkdir -m 0700 "$symlink_dir"
printf 'do not change\n' > "$symlink_canary"
symlink_digest="$(sha256sum "$symlink_canary" | awk '{print $1}')"
ln -s "$symlink_canary" "$symlink_config"
if ssh_config_remove_exact "$symlink_config" 'Include subyard-e2e-yard.config'; then
  fail 'symlink SSH config was accepted for removal'
fi
[ "$(sha256sum "$symlink_canary" | awk '{print $1}')" = "$symlink_digest" ] \
  || fail 'symlink SSH config target was modified'

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

# A legacy predictable temp name may be a symlink; removal must ignore it entirely.
legacy_canary="$TMP/legacy-temp-canary"
printf 'legacy canary\n' > "$legacy_canary"
legacy_canary_digest="$(sha256sum "$legacy_canary" | awk '{print $1}')"
chmod 000 "$legacy_canary"
legacy_canary_metadata="$(stat -c '%u:%g:%a' "$legacy_canary")"
rm -f -- "$legacy_temp"
ln -s "$legacy_canary" "$legacy_temp"
ssh_config_remove_exact "$config" 'Include subyard-e2e-yard.config' \
  || fail 'could not remove the exact managed Include'
! grep -qxF 'Include subyard-e2e-yard.config' "$config" \
  || fail 'managed Include remained after rollback'
grep -qxF 'Host existing' "$config" \
  || fail 'Include rollback removed unrelated SSH config'
if ! [ -L "$legacy_temp" ] || [ "$(readlink "$legacy_temp")" != "$legacy_canary" ]; then
  fail 'Include removal changed the legacy config.tmp symlink'
fi
[ "$(stat -c '%u:%g:%a' "$legacy_canary")" = "$legacy_canary_metadata" ] \
  || fail 'Include removal changed the legacy config.tmp target'
chmod 0600 "$legacy_canary"
[ "$(sha256sum "$legacy_canary" | awk '{print $1}')" = "$legacy_canary_digest" ] \
  || fail 'Include removal changed the legacy config.tmp target content'
config_after_removal="$(sha256sum "$config" | awk '{print $1}')"
ssh_config_remove_exact "$config" 'Include subyard-e2e-yard.config' \
  || fail 'idempotent Include rollback failed'
[ "$(sha256sum "$config" | awk '{print $1}')" = "$config_after_removal" ] \
  || fail 'repeated Include removal changed unrelated SSH config lines'

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

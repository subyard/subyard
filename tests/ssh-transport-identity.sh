#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=scripts/lib/ssh-config.sh
. "$ROOT/scripts/lib/ssh-config.sh"

data_home="$TMP/data home"
identity="$(ssh_transport_identity_prepare "$data_home")" \
  || fail 'first prepare did not create a transport identity'
[ "$identity" = "$data_home/ssh/id_ed25519" ] \
  || fail "prepare returned a non-canonical identity: $identity"
[ "$(stat -c %a "$data_home/ssh")" = 700 ] || fail 'identity directory mode is not 0700'
[ "$(stat -c %a "$identity")" = 600 ] || fail 'private key mode is not 0600'
[ "$(stat -c %a "$identity.pub")" = 644 ] || fail 'public key mode is not 0644'
[ "$(ssh-keygen -lf "$identity" | awk '{print $NF}')" = '(ED25519)' ] \
  || fail 'transport identity is not Ed25519'

fingerprint="$(ssh-keygen -lf "$identity" | awk '{print $2}')"
[ "$(ssh_transport_identity_prepare "$data_home")" = "$identity" ] \
  || fail 'repeated prepare changed the canonical path'
[ "$(ssh-keygen -lf "$identity" | awk '{print $2}')" = "$fingerprint" ] \
  || fail 'repeated prepare rotated the transport identity'
[ "$(ssh_config_quote '/tmp/a b%slot')" = '"/tmp/a b%%slot"' ] \
  || fail 'OpenSSH path quoting did not preserve spaces and percent literals'
if ssh_config_quote $'/tmp/injected\nHost attacker' >/dev/null; then
  fail 'OpenSSH path quoting accepted a newline'
fi
[ "$(ssh_yard_snippet_name '')" = subyard.config ] \
  && [ "$(ssh_yard_snippet_name default)" = subyard.config ] \
  && [ "$(ssh_yard_snippet_name demo)" = subyard-demo.config ] \
  || fail 'default and named yard snippet naming diverged'

race_home="$TMP/race-data"
ssh_transport_identity_prepare "$race_home" >"$TMP/race-a.out" 2>"$TMP/race-a.err" &
race_a=$!
ssh_transport_identity_prepare "$race_home" >"$TMP/race-b.out" 2>"$TMP/race-b.err" &
race_b=$!
set +e
wait "$race_a"; race_a_status=$?
wait "$race_b"; race_b_status=$?
set -e
[ "$race_a_status" -eq 0 ] || [ "$race_b_status" -eq 0 ] \
  || fail 'parallel first prepare did not publish any canonical pair'
ssh_transport_identity_prepare "$race_home" >/dev/null \
  || fail 'canonical pair was invalid after parallel first prepare'

personal_home="$TMP/personal"
mkdir -p "$personal_home/.ssh"
ssh-keygen -q -t ed25519 -N personal-passphrase -f "$personal_home/.ssh/id_ed25519"
HOME="$personal_home" ssh_transport_identity_prepare "$TMP/separate-data" >/dev/null \
  || fail 'personal passphrase key prevented dedicated identity creation'
[ "$(ssh-keygen -lf "$personal_home/.ssh/id_ed25519" | awk '{print $2}')" = \
  "$(ssh-keygen -lf "$personal_home/.ssh/id_ed25519.pub" | awk '{print $2}')" ] \
  || fail 'personal identity was modified'

assert_rejected() {
  local name="$1" home="$TMP/reject-$1"
  shift
  mkdir -p "$home"
  "$@" "$home"
  if ssh_transport_identity_prepare "$home" >"$TMP/$name.out" 2>"$TMP/$name.err"; then
    fail "$name identity state was accepted"
  fi
  [ ! -s "$TMP/$name.out" ] || fail "$name failure printed an identity path"
}

make_incomplete() {
  mkdir -m 700 "$1/ssh"
  ssh-keygen -q -t ed25519 -N '' -f "$1/temp"
  mv "$1/temp" "$1/ssh/id_ed25519"
  chmod 600 "$1/ssh/id_ed25519"
}
assert_rejected incomplete make_incomplete

make_symlink() {
  mkdir -m 700 "$1/ssh"
  ssh-keygen -q -t ed25519 -N '' -f "$1/real"
  ln -s "$1/real" "$1/ssh/id_ed25519"
  mv "$1/real.pub" "$1/ssh/id_ed25519.pub"
  chmod 644 "$1/ssh/id_ed25519.pub"
}
assert_rejected symlink make_symlink

make_bad_mode() {
  mkdir -m 700 "$1/ssh"
  ssh-keygen -q -t ed25519 -N '' -f "$1/ssh/id_ed25519"
  chmod 640 "$1/ssh/id_ed25519"
  chmod 644 "$1/ssh/id_ed25519.pub"
}
assert_rejected bad-mode make_bad_mode

make_mismatch() {
  mkdir -m 700 "$1/ssh"
  ssh-keygen -q -t ed25519 -N '' -f "$1/ssh/id_ed25519"
  ssh-keygen -q -t ed25519 -N '' -f "$1/other"
  mv "$1/other.pub" "$1/ssh/id_ed25519.pub"
  chmod 600 "$1/ssh/id_ed25519"
  chmod 644 "$1/ssh/id_ed25519.pub"
}
assert_rejected mismatch make_mismatch

access_script="$ROOT/scripts/07-ssh-access.sh"
! grep -Fq 'SUBYARD_SSH_PUBKEY' "$access_script" \
  || fail 'local SSH reconciliation still accepts SUBYARD_SSH_PUBKEY'
! grep -Eq 'HOME/.ssh/id_(ed25519|ecdsa|rsa)' "$access_script" \
  || fail 'local SSH reconciliation still searches personal identities'
grep -Fq 'ssh_transport_identity_prepare "$SUBYARD_HOME"' "$access_script" \
  || fail 'local SSH reconciliation does not prepare the canonical identity'
grep -Fq 'BatchMode=yes' "$access_script" \
  || fail 'local SSH reconciliation does not verify non-interactive login'
grep -Fq 'IdentitiesOnly=yes' "$access_script" \
  || fail 'local SSH reconciliation verification can use another identity'
grep -Fq 'SSH_AUTH_SOCK=' "$access_script" \
  || fail 'local SSH reconciliation verification still depends on an agent'
grep -Fq 'ForwardAgent no' "$access_script" \
  || fail 'local SSH snippet does not explicitly disable agent forwarding by default'
grep -Fq '.subyard-snippet.XXXXXX' "$access_script" \
  || fail 'per-yard SSH snippet is not staged atomically'

teardown_script="$ROOT/scripts/teardown-physical.sh"
! grep -Eq 'rm .*(id_ed25519|/ssh(["[:space:]]|$))' "$teardown_script" \
  || fail 'teardown removes the shared transport identity'
grep -Fq ': "${FORWARD_SSH_AGENT:=0}"' "$ROOT/config/subyard.env" \
  || fail 'ssh-agent forwarding is not opt-in by default'

printf 'ok: host-scoped SSH transport identity lifecycle is fail-closed\n'

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
# The verified runtime can be reachable only through a privileged parent's
# /proc/PID/fd directory. The operator child must use already-loaded code.
cleanup_home="$TMP/operator home"
mkdir -m 0700 "$cleanup_home"
mkdir -m 0700 "$cleanup_home/.ssh"
printf 'Include subyard-demo.config\nHost retained\n' > "$cleanup_home/.ssh/config"
printf 'Host yard-demo\n' > "$cleanup_home/.ssh/subyard-demo.config"
cleanup_block="$(sed -n '/^sshdir=/,/^known=/p' "$teardown_script" | sed '$d')"
[ -n "$cleanup_block" ] || fail 'operator SSH cleanup block is absent'
(
  export SCRIPT_DIR="$TMP/inaccessible-runtime"
  export SUBYARD_OPERATOR_HOME="$cleanup_home"
  OPERATOR_USER="$(id -un)"
  export YARD_SNIP=subyard-demo.config
  # Run the real child bash without host privileges. Its source pathname is
  # deliberately unavailable, as it is after the actual identity drop.
  sudo() {
    [ "$1" = -n ] && [ "$2" = -u ] && [ "$3" = "$OPERATOR_USER" ] && [ "$4" = -- ] \
      || return 1
    shift 4
    "$@"
  }
  ok() { :; }
  die() { fail "$*"; }
  eval "$cleanup_block"
) || fail 'operator SSH cleanup reopened the inaccessible runtime'
[ ! -e "$cleanup_home/.ssh/subyard-demo.config" ] || fail 'operator snippet remains'
[ "$(cat "$cleanup_home/.ssh/config")" = 'Host retained' ] \
  || fail 'operator cleanup did not preserve unrelated SSH config'
[ "$(stat -c '%u:%a' "$cleanup_home/.ssh/config")" = "$(id -u):600" ] \
  || fail 'operator cleanup changed SSH config ownership or mode'

! grep -Eq 'rm .*(id_ed25519|/ssh(["[:space:]]|$))' "$teardown_script" \
  || fail 'teardown removes the shared transport identity'
legacy_config_tmp_pattern="\$cfg.tmp"
typed_ssh_dir="sshdir=\"\$SUBYARD_OPERATOR_HOME/.ssh\""
typed_ssh_paths="snip=\"\$sshdir/\$YARD_SNIP\"; cfg=\"\$sshdir/config\""
root_snippet_remove="rm -f \"\$snip\""
remove_helper_call="rm -f -- \"\$1\" && ssh_config_remove_exact \"\$2\" \"\$3\""
operator_identity_drop="sudo -n -u \"\$OPERATOR_USER\" -- bash -s"
positional_helper_arguments="bash -s -- \"\$snip\" \"\$cfg\" \"Include \$YARD_SNIP\""
! grep -Fq "$legacy_config_tmp_pattern" "$teardown_script" \
  || fail 'teardown still writes or renames through predictable config.tmp'
if ! grep -Fq "$typed_ssh_dir" "$teardown_script" \
  || ! grep -Fq "$typed_ssh_paths" "$teardown_script"; then
  fail 'teardown does not use the typed operator home for SSH cleanup'
fi
! grep -Fq 'OPERATOR_HOME=' "$teardown_script" \
  || fail 'teardown recomputes the typed operator home from passwd'
! grep -Fq "$root_snippet_remove" "$teardown_script" \
  || fail 'teardown root still removes the operator SSH snippet'
grep -Fq "$remove_helper_call" "$teardown_script" \
  || fail 'teardown does not remove the snippet and Include in one operator child'
grep -Fq "$operator_identity_drop" "$teardown_script" \
  || fail 'teardown does not drop non-interactively to the operator for SSH cleanup'
grep -Fq "$positional_helper_arguments" \
  "$teardown_script" \
  || fail 'teardown does not pass both operator SSH paths positionally'
grep -Fq ': "${FORWARD_SSH_AGENT:=0}"' "$ROOT/config/subyard.env" \
  || fail 'ssh-agent forwarding is not opt-in by default'

printf 'ok: host-scoped SSH transport identity lifecycle is fail-closed\n'

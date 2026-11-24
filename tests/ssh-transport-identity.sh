#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-ssh-transport-identity.XXXXXX")"
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

# A default ACL overrides umask for shell redirections. Exercise the real
# filesystem rather than simulating stat output, including adjacent private files.
if command -v setfacl >/dev/null 2>&1; then
  acl_home="$TMP/acl-data"
  mkdir -m 0700 "$acl_home"
  acl_uid="$(id -u)"
  setfacl -d -m "u::rwx,u:$((acl_uid + 1)):rwx,g::rwx,o::r-x" "$acl_home"
  (umask 077; : > "$acl_home/umask-only")
  [ "$(stat -c %a "$acl_home/umask-only")" = 664 ] \
    || fail 'default ACL fixture did not override umask'
  acl_identity="$(ssh_transport_identity_prepare "$acl_home")" \
    || fail 'default ACL prevented transport identity creation'
  [ "$(stat -c %a "$acl_home/ssh")" = 700 ] \
    && [ "$(stat -c %a "$acl_identity")" = 600 ] \
    && [ "$(stat -c %a "$acl_identity.pub")" = 644 ] \
    || fail 'default ACL changed identity permissions'
  acl_fingerprint="$(ssh-keygen -lf "$acl_identity" | awk '{print $2}')"
  # Simulate a lock left by the old umask-only creator under this default ACL.
  chmod 0664 "$acl_home/ssh/.identity.lock"
  acl_lock_inode="$(stat -c '%d:%i' "$acl_home/ssh/.identity.lock")"
  ssh_transport_identity_prepare "$acl_home" >/dev/null \
    || fail 'default ACL prevented repeated identity preparation'
  [ "$(ssh-keygen -lf "$acl_identity" | awk '{print $2}')" = "$acl_fingerprint" ] \
    || fail 'default ACL rotated the transport identity'
  [ "$(stat -c '%d:%i:%a' "$acl_home/ssh/.identity.lock")" = "$acl_lock_inode:600" ] \
    || fail 'default-ACL lock was not repaired in place'
  ssh_config_prepend_once "$acl_home/ssh/config" 'Include subyard.config' \
    || fail 'default ACL prevented SSH config publication'
  ssh_known_host_replace "$acl_home/ssh/known_hosts" '[127.0.0.1]:2223' "$(cat "$acl_identity.pub")" \
    || fail 'default ACL prevented known-host publication'
  for acl_file in .identity.lock .subyard-config.lock .subyard-known-hosts.lock config known_hosts; do
    [ -f "$acl_home/ssh/$acl_file" ] && [ ! -L "$acl_home/ssh/$acl_file" ] \
      && [ "$(stat -c '%u:%a' "$acl_home/ssh/$acl_file")" = "$(id -u):600" ] \
      || fail "default ACL changed private file ownership or mode: $acl_file"
  done
else
  printf 'SKIP: default-ACL transport identity regression (setfacl unavailable)\n'
fi

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
  if timeout 3 bash -c 'source "$1"; ssh_transport_identity_prepare "$2"' \
    identity-refusal "$ROOT/scripts/lib/ssh-config.sh" "$home" >"$TMP/$name.out" 2>"$TMP/$name.err"; then
    fail "$name identity state was accepted"
  fi
  [ ! -s "$TMP/$name.out" ] || fail "$name failure printed an identity path"
}

# Existing locks carry no identity material: normalize their mode in place.
legacy_lock="$data_home/ssh/.identity.lock"
printf 'retained lock contents\n' > "$legacy_lock"
legacy_inode="$(stat -c '%d:%i' "$legacy_lock")"
for legacy_mode in 644 664; do
  chmod "$legacy_mode" "$legacy_lock"
  ssh_transport_identity_prepare "$data_home" >/dev/null \
    || fail "existing mode-$legacy_mode lock was not repaired"
  [ "$(stat -c '%d:%i:%a' "$legacy_lock")" = "$legacy_inode:600" ] \
    && [ "$(cat "$legacy_lock")" = 'retained lock contents' ] \
    || fail 'lock repair replaced its inode or contents'
  [ "$(ssh-keygen -lf "$identity" | awk '{print $2}')" = "$fingerprint" ] \
    || fail 'lock repair rotated the transport identity'
done

# The descriptor owner check must precede fchmod, even if a pathname was checked.
python3 - "$ROOT/scripts/lib/ssh-config.sh" "$TMP/foreign-lock" <<'PY_OWNER'
import os
import pathlib
import subprocess
import sys

source = pathlib.Path(sys.argv[1]).read_text().split("<<'PY_LOCK'", 1)[1]
code = source.split("\n", 1)[1].split("\nPY_LOCK", 1)[0]
lock = pathlib.Path(sys.argv[2])
lock.write_text("foreign lock contents")
lock.chmod(0o664)
before = lock.stat()
result = subprocess.run([sys.executable, "-", str(lock), str(os.getuid() + 1)],
                        input=code, text=True, capture_output=True)
assert result.returncode != 0, result
assert lock.stat().st_mode == before.st_mode and lock.stat().st_ino == before.st_ino
assert lock.read_text() == "foreign lock contents"
PY_OWNER

make_hardlink_lock() {
  mkdir -m 700 "$1/ssh"
  printf 'unrelated operator file\n' > "$1/unrelated"
  chmod 0664 "$1/unrelated"
  cp "$1/unrelated" "$1/expected"
  ln "$1/unrelated" "$1/ssh/.identity.lock"
  stat -c '%d:%i:%a:%h' "$1/unrelated" "$1/ssh/.identity.lock" > "$1/before"
}
assert_rejected hardlink-lock make_hardlink_lock
hardlink_root="$TMP/reject-hardlink-lock"
stat -c '%d:%i:%a:%h' "$hardlink_root/unrelated" "$hardlink_root/ssh/.identity.lock" > "$hardlink_root/after"
cmp -s "$hardlink_root/before" "$hardlink_root/after" \
  || fail 'hardlink lock refusal changed an inode, mode or link count'
for hardlink_path in "$hardlink_root/unrelated" "$hardlink_root/ssh/.identity.lock"; do
  cmp -s "$hardlink_root/expected" "$hardlink_path" \
    || fail "hardlink lock refusal changed contents at $hardlink_path"
done
grep -Fq "$hardlink_root/ssh/.identity.lock" "$TMP/hardlink-lock.err" \
  || fail 'hardlink lock refusal omitted its path'

make_symlink_lock() {
  mkdir -m 700 "$1/ssh"
  : > "$1/target"
  chmod 600 "$1/target"
  ln -s "$1/target" "$1/ssh/.identity.lock"
}
assert_rejected symlink-lock make_symlink_lock
grep -Fq "$TMP/reject-symlink-lock/ssh/.identity.lock" "$TMP/symlink-lock.err" \
  || fail 'symlink lock refusal omitted its path'

make_directory_lock() {
  mkdir -m 700 "$1/ssh"
  mkdir -m 700 "$1/ssh/.identity.lock"
}
assert_rejected directory-lock make_directory_lock

make_fifo_lock() {
  mkdir -m 700 "$1/ssh"
  mkfifo -m 664 "$1/ssh/.identity.lock"
}
assert_rejected fifo-lock make_fifo_lock
for lock_case in directory-lock fifo-lock; do
  grep -Fq "$TMP/reject-$lock_case/ssh/.identity.lock" "$TMP/$lock_case.err" \
    || fail "$lock_case refusal omitted its path"
done


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

make_shared_bad_mode() {
  mkdir -m 700 "$1/ssh"
  ssh-keygen -q -t ed25519 -N '' -f "$1/ssh/id_ed25519"
  chmod 640 "$1/ssh/id_ed25519"
  ln "$1/ssh/id_ed25519" "$1/alias"
}
assert_rejected shared-bad-mode make_shared_bad_mode

# The operator's own broader state is repaired; a read-only key is accepted.
repair_home="$TMP/repair"
mkdir -p "$repair_home/ssh"
chmod 755 "$repair_home/ssh"
ssh-keygen -q -t ed25519 -N '' -f "$repair_home/ssh/id_ed25519"
chmod 640 "$repair_home/ssh/id_ed25519"
chmod 664 "$repair_home/ssh/id_ed25519.pub"
: > "$repair_home/ssh/.identity.lock"
chmod 644 "$repair_home/ssh/.identity.lock"
ssh_transport_identity_prepare "$repair_home" >/dev/null \
  || fail 'operator-owned broader identity state was not repaired'
[ "$(stat -c %a "$repair_home/ssh")" = 700 ] \
  && [ "$(stat -c %a "$repair_home/ssh/id_ed25519")" = 600 ] \
  && [ "$(stat -c %a "$repair_home/ssh/.identity.lock")" = 600 ] \
  || fail 'identity repair left a broader mode'
chmod 400 "$repair_home/ssh/id_ed25519"
ssh_transport_identity_prepare "$repair_home" >/dev/null \
  || fail 'read-only private key was refused'
[ "$(stat -c %a "$repair_home/ssh/id_ed25519")" = 400 ] || fail 'read-only private key was widened'

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
# /proc/PID/fd directory. The operator child must use only already-loaded code.
cleanup_home="$TMP/"'operator home $(touch injected)'
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
  # The privileged leaf owns the dispatcher; the operator child never reopens it.
  engine="${YARD_ENGINE_PATH:-$ROOT/.build/yard}"
  teardown_guard() { "$engine" _teardown-guard "$@"; }
  SUBYARD_TEARDOWN_ARTIFACTS="$(SNIPPET="$cleanup_home/.ssh/$YARD_SNIP" python3 -c 'import json, os; print(json.dumps({"Path": os.environ["SNIPPET"]}))' \
    | teardown_guard capture-artifact \
    | python3 -c 'import json, sys; item = json.load(sys.stdin); print(json.dumps([{"Path": item["Path"], "Binding": item["Binding"]}]))')"
  export SUBYARD_TEARDOWN_ARTIFACTS
  # Run the real child bash without host privileges. Its source pathname is
  # deliberately unavailable, as it is after the actual identity drop.
  sudo() {
    [ "$1" = -n ] && [ "$2" = -u ] && [ "$3" = "$OPERATOR_USER" ] && [ "$4" = -- ] \
      || return 1
    [ ! -e "$SUBYARD_OPERATOR_HOME/.ssh/$YARD_SNIP" ] \
      || fail 'approved snippet was not removed by the pinned guard before the identity drop'
    shift 4
    env -u SUBYARD_DISPATCHER_PATH -u SUBYARD_TEARDOWN_ARTIFACTS "$@"
  }
  ok() { :; }
  die() { fail "$*"; }
  eval "$cleanup_block"
) || fail 'operator SSH cleanup reopened the inaccessible runtime'
[ ! -e "$cleanup_home/.ssh/subyard-demo.config" ] || fail 'operator snippet remains'
[ ! -e "$TMP/injected" ] && [ ! -e "$ROOT/injected" ] \
  || fail 'operator SSH cleanup evaluated a literal path as shell code'
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
remove_helper_call="teardown_guard remove-artifact-as \"\$OPERATOR_USER\" \"\$snip\" || exit 75"
operator_identity_drop="sudo -n -u \"\$OPERATOR_USER\" -- bash -s"
positional_helper_arguments="bash -s -- \"\$cfg\" \"Include \$YARD_SNIP\""
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
  || fail 'teardown does not unlink the snippet with operator credentials through the pinned guard'
grep -Fq "$operator_identity_drop" "$teardown_script" \
  || fail 'teardown does not drop non-interactively to the operator for SSH cleanup'
grep -Fq "$positional_helper_arguments" \
  "$teardown_script" \
  || fail 'teardown does not pass the operator SSH config and line positionally'
grep -Fq ': "${FORWARD_SSH_AGENT:=0}"' "$ROOT/config/subyard.env" \
  || fail 'ssh-agent forwarding is not opt-in by default'
grep -Fq '(docs/security.md)' "$ROOT/README.md" \
  || fail 'README omits the security documentation link'
for forwarding_warning in \
  'git push and other host access from inside the yard' \
  'forwarded, write-enabled credential while the SSH session is active' \
  'No private key is copied into the yard' \
  'any process that can reach the forwarded agent' \
  'agent ask-rules are a UX safeguard, not a security boundary'
do
  grep -Fq "$forwarding_warning" "$access_script" \
    || fail "SSH access plan omits forwarding boundary: $forwarding_warning"
  grep -Fq "$forwarding_warning" "$ROOT/docs/security.md" \
    || fail "security documentation omits forwarding boundary: $forwarding_warning"
done

printf 'ok: host-scoped SSH transport identity lifecycle is fail-closed\n'

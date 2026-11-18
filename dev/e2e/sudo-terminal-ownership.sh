#!/usr/bin/env bash
# Real sudo/PTY acceptance runs on a disposable leased VM. --check-fixture is host-free.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP=''
FIXTURE_USER=''
SUDOERS=''
REAL_PROJECT=''
REAL_INSTANCE=''
REAL_VOLUME=''
E2E_POOL='subyard-sudo-terminal'
POOL_OWNED=0

die() { printf 'sudo-terminal-ownership: %s\n' "$*" >&2; exit 2; }

cleanup_real_project() {
  local fingerprint profile
  if [ -n "$REAL_PROJECT" ] && command -v incus >/dev/null 2>&1 \
    && incus project show "$REAL_PROJECT" >/dev/null 2>&1; then
    if [ -n "$REAL_INSTANCE" ] \
      && incus info "$REAL_INSTANCE" --project "$REAL_PROJECT" >/dev/null 2>&1; then
      incus delete "$REAL_INSTANCE" --project "$REAL_PROJECT" --force >/dev/null 2>&1
    fi
    if [ -n "$REAL_VOLUME" ]; then
      incus storage volume delete "$E2E_POOL" "$REAL_VOLUME" \
        --project "$REAL_PROJECT" >/dev/null 2>&1
    fi
    while IFS= read -r fingerprint; do
      [ -n "$fingerprint" ] \
        && incus image delete "$fingerprint" --project "$REAL_PROJECT" >/dev/null 2>&1
    done < <(incus image list --project "$REAL_PROJECT" -f csv -c f 2>/dev/null)
    while IFS= read -r profile; do
      [ -n "$profile" ] && [ "$profile" != default ] \
        && incus profile delete "$profile" --project "$REAL_PROJECT" >/dev/null 2>&1
    done < <(incus profile list --project "$REAL_PROJECT" -f csv -c n 2>/dev/null)
    if [ -z "$(incus list --project "$REAL_PROJECT" -f csv -c n 2>/dev/null)" ]; then
      incus profile device remove default eth0 --project "$REAL_PROJECT" >/dev/null 2>&1
      incus profile device remove default root --project "$REAL_PROJECT" >/dev/null 2>&1
      incus project delete "$REAL_PROJECT" >/dev/null 2>&1
    fi
  fi
  if [ "$POOL_OWNED" = 1 ]; then
    incus storage delete "$E2E_POOL" --project default >/dev/null 2>&1 || true
  fi
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  cleanup_real_project
  if [ -n "$SUDOERS" ]; then
    case "$SUDOERS" in /etc/sudoers.d/subyard-sudo-terminal-*) rm -f -- "$SUDOERS" ;; esac
  fi
  if [ -n "$FIXTURE_USER" ] && getent passwd "$FIXTURE_USER" >/dev/null 2>&1; then
    case "$FIXTURE_USER" in sy-sudo-*) userdel -r "$FIXTURE_USER" >/dev/null 2>&1 ;; esac
  fi
  if [ -n "$TMP" ] && [[ "$TMP" = /tmp/subyard-sudo-terminal.* ]] && [ -d "$TMP" ]; then
    find "$TMP" -depth -delete
  fi
  exit "$rc"
}
if [ "${1:-}" = --check-fixture ]; then
  [ "$#" -le 2 ] || die 'usage: --check-fixture [runtime-directory]'
  runtime="${2:-$ROOT/.build/runtime/current}"
else
  [ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
  for command in sg sudo script runuser useradd userdel visudo; do
    command -v "$command" >/dev/null 2>&1 || die "$command is required"
  done
  if [ "$(id -u)" -ne 0 ]; then
    # The controller prepares the platform and candidate as dev before
    # switching to root for fixture user and sudoers management.
    bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
    # shellcheck source=tests/helpers/release-candidate.sh
    . "$ROOT/tests/helpers/release-candidate.sh"
    if engine="$(release_candidate_prepare "$ROOT")"; then
      :
    elif [ "$?" = 1 ]; then
      make -C "$ROOT" build
      engine="$ROOT/.build/runtime/current/bin/yard-engine"
    else
      die 'could not verify frozen candidate'
    fi
    exec sudo -n env SUBYARD_E2E_VM="$SUBYARD_E2E_VM" bash "$0" "$engine"
  fi

  engine="${1:-}"
  case "$engine" in
    "$ROOT/.subyard-acceptance/runtime/bin/yard-engine"|"$ROOT/.build/runtime/current/bin/yard-engine") ;;
    *) die 'candidate engine path is required' ;;
  esac
  runtime="${engine%/bin/yard-engine}"
fi
[ -x "$runtime/bin/yard-engine" ] || die 'candidate engine is missing'
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
TMP="$(mktemp -d /tmp/subyard-sudo-terminal.XXXXXX)"
chmod 0750 "$TMP"
fixture_repo="$TMP/repository"
real_repo="$TMP/real-repository"
fixture_home="$TMP/home"

stage_runtime() {
  local destination="$1"
  cp -a --no-preserve=ownership "$runtime/." "$destination"
  (cd "$destination" && sha256sum --check --quiet --strict runtime-files.sha256) \
    || die 'staged candidate runtime is incomplete or changed'
}

stage_runtime "$fixture_repo"
install -d -m 0755 "$fixture_home"

cat > "$fixture_repo/scripts/teardown-physical.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib/runtime.sh"
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
. "$SCRIPT_DIR/lib/ui.sh"
. "$SCRIPT_DIR/lib/host.sh"
require_root "the PTY fixture records one root-owned proof"
rmdir "$SUBYARD_STATE_DIR"
printf 'root-step=%s\n' "$(id -u)"
SH
chmod 0755 "$fixture_repo/scripts/teardown-physical.sh"

cat > "$TMP/run-teardown" <<SH
#!/usr/bin/env bash
set -e
operator_home='$fixture_home'
[ "\$(id -u)" != 0 ] || operator_home='$TMP/root-home'
install -d -m 0700 "\$operator_home" "\$operator_home/.config" \\
  "\$operator_home/.config/subyard" "\$operator_home/.config/subyard/projects"
if [ "\$#" -eq 0 ]; then
  set -- teardown --yes --keep-data
fi
exec env -i PATH="\$PATH" LANG=C.UTF-8 TERM="\${TERM:-dumb}" \\
  HOME="\$operator_home" \\
  SUBYARD_OPERATOR_HOME="\$operator_home" \\
  SUBYARD_CONFIG_HOME="\$operator_home/.config/subyard" \\
  SUBYARD_HOME="\$operator_home/.subyard" \\
  SUBYARD_NO_AUDIT=1 \\
  SUBYARD_REPOSITORY_ROOT='$fixture_repo' \\
  STORAGE_PATH="\$operator_home/.subyard/storage" \\
  HOST_BASE="\$operator_home/host" \\
  RESTRICTED_DISK_PATHS="\$operator_home/host" \\
  SHIFT_MODE=shift \\
  FORWARD_SSH_AGENT=0 \\
  CODING_TOOL_INTEGRATIONS= \\
  DEV_SUDO=0 \\
  DEV_UID=1000 \\
  DEV_USER=dev \\
  SSH_PORT=2222 \\
  '$fixture_repo/bin/yard-engine' "\$@"
SH
chmod 0755 "$TMP/run-teardown"

if [ "${1:-}" = --check-fixture ]; then
  "$TMP/run-teardown" config show CODING_TOOL_INTEGRATIONS >/dev/null
  # Exercise the staged guard used by native teardown planning without Incus or sudo.
  printf '{"Path":"%s"}\n' "$fixture_home" \
    | python3 -B "$fixture_repo/scripts/lib/teardown-plan.py" capture-artifact \
    | python3 -c 'import json, sys; sys.exit(0 if json.load(sys.stdin)["Binding"] else 1)'
  printf 'ok: complete candidate runtime verified and its teardown guard captures an artifact\n'
  exit 0
fi

token="${TMP##*.}"
fixture_user="sy-sudo-$$"
sudoers="/etc/sudoers.d/subyard-sudo-terminal-$token"
getent passwd "$fixture_user" >/dev/null 2>&1 \
  && die "refusing existing fixture user $fixture_user"
[ ! -e "$sudoers" ] && [ ! -L "$sudoers" ] || die "refusing existing sudoers fixture $sudoers"
FIXTURE_USER="$fixture_user"
SUDOERS="$sudoers"
useradd --user-group --groups incus-admin --home-dir "$fixture_home" --shell /bin/bash "$FIXTURE_USER"
# The artifact guard opens each ancestor O_RDONLY before inspecting state.
chgrp "$FIXTURE_USER" "$TMP"
chown -R "$FIXTURE_USER:$FIXTURE_USER" "$fixture_home"
password="$(dd if=/dev/urandom bs=32 count=1 status=none | sha256sum | cut -c1-24)"
printf '%s:%s\n' "$FIXTURE_USER" "$password" | chpasswd
printf '%s ALL=(root) ALL\nDefaults:%s timestamp_timeout=5,passwd_tries=3\n' \
  "$FIXTURE_USER" "$FIXTURE_USER" > "$SUDOERS"
chmod 0440 "$SUDOERS"
visudo -cf "$SUDOERS" >/dev/null
printf '  [ ok ] isolated password-required sudo fixture is ready\n'

run_as_fixture() {
  runuser -u "$FIXTURE_USER" -- "$@"
}

# The platform fixture stops short of yard init, which normally provisions this lock.
"$fixture_repo/bin/yard-engine" _network-lock ensure
run_as_fixture "$fixture_repo/bin/yard-engine" _network-lock check

run_prompted_pty() {
  local output="$1" command_line="$2" input pid input_fd count=0 deadline
  shift 2
  mkfifo -m 0600 "$output.input" || return
  run_as_fixture script --echo never -qefc "$command_line" /dev/null \
    <"$output.input" >"$output" 2>&1 &
  pid=$!
  exec {input_fd}>"$output.input" || return
  for input in "$@"; do
    count=$((count + 1))
    deadline=$((SECONDS + 30))
    # sudo can discard input during the delay before a retry prompt.
    until [ "$(grep -Fc '[sudo] password for' "$output" || true)" -ge "$count" ]; do
      if ! kill -0 "$pid" 2>/dev/null || [ "$SECONDS" -ge "$deadline" ]; then
        exec {input_fd}>&-
        wait "$pid" || return
        return 1
      fi
      sleep 0.1
    done
    printf '%s' "$input" >&"$input_fd"
  done
  local rc=0
  wait "$pid" || rc=$?
  exec {input_fd}>&-
  return "$rc"
}

run_as_fixture sudo -k
pty_output="$TMP/pty.out"
wrong="incorrect-$token"
printf '  [ .. ] checking wrong-password retry, success and cached credential\n'
set +e
run_prompted_pty "$pty_output" "sudo -k; '$TMP/run-teardown'; '$TMP/run-teardown'" \
  "$wrong"$'\n' "$password"$'\n'
pty_rc=$?
set -e
if [ "$pty_rc" -ne 0 ]; then
  tr -d '\r' < "$pty_output" \
    | sed "s/$password/[redacted]/g" \
    | tail -n 120 >&2
  die "interactive PTY command failed with status $pty_rc"
fi
grep -Fq '[sudo] password for' "$pty_output" \
  || die 'standard sudo password prompt was not visible'
grep -Fq 'Sorry, try again.' "$pty_output" \
  || die 'standard sudo retry after an incorrect password was not visible'
[ "$(grep -Fc 'root-step=0' "$pty_output")" -eq 2 ] \
  || die 'the authorized command did not continue twice'
[ "$(grep -Fo '[sudo] password for' "$pty_output" | wc -l)" -eq 2 ] \
  || die 'the cached credential opened an extra password prompt'
grep -Fq "$password" "$pty_output" && die 'password leaked into the PTY transcript'
printf '  [ ok ] standard retry and cached credential verified\n'

run_as_fixture sudo -k
no_tty_output="$TMP/no-tty.out"
printf '  [ .. ] checking no-TTY failure before root mutation\n'
if run_as_fixture "$TMP/run-teardown" </dev/null >"$no_tty_output" 2>&1; then
  die 'no-TTY invocation continued without a cached credential'
fi
grep -Fq 'rerun '\''yard teardown'\'' in an operator terminal' "$no_tty_output" \
  || die 'no-TTY failure was not actionable'
grep -Fq 'root-step=' "$no_tty_output" \
  && die 'no-TTY failure reached the root mutation'
printf '  [ ok ] no-TTY invocation failed before root mutation\n'

run_as_fixture sudo -k
interrupt_output="$TMP/interrupt.out"
printf '  [ .. ] checking Ctrl-C at the standard sudo prompt\n'
set +e
run_prompted_pty "$interrupt_output" "sudo -k; '$TMP/run-teardown'" $'\003'
interrupt_rc=$?
set -e
[ "$interrupt_rc" -ne 0 ] || die 'Ctrl-C did not cancel sudo authorization'
grep -Fq '[sudo] password for' "$interrupt_output" \
  || die 'Ctrl-C fixture did not reach the sudo prompt'
grep -Fq 'root-step=' "$interrupt_output" \
  && die 'Ctrl-C reached the root mutation'
printf '  [ ok ] Ctrl-C cancelled before root mutation\n'

fail_sudo_log="$TMP/root-sudo.log"
install -d -m 0755 "$TMP/fail-bin"
cat > "$TMP/fail-bin/sudo" <<SH
#!/usr/bin/env bash
printf 'invoked\n' > '$fail_sudo_log'
exit 99
SH
chmod 0755 "$TMP/fail-bin/sudo"
PATH="$TMP/fail-bin:/usr/sbin:/usr/bin:/sbin:/bin" "$TMP/run-teardown" \
  >"$TMP/root.out" 2>&1
grep -Fq 'root-step=0' "$TMP/root.out" || die 'EUID 0 did not run the root step directly'
[ ! -e "$fail_sudo_log" ] || die 'EUID 0 invoked sudo'
printf '  [ ok ] EUID 0 bypassed sudo\n'

printf '  [ .. ] preparing isolated real-command acceptance\n'
if ! incus storage show default --project default >/dev/null 2>&1; then
  incus storage create default dir --project default >/dev/null \
    || die 'could not restore the disposable VM default storage pool'
fi
if incus storage show "$E2E_POOL" --project default >/dev/null 2>&1; then
  [ "$(incus storage show "$E2E_POOL" --project default \
    | awk '!found && $1 == "driver:" { print $2; found=1 }')" = dir ] \
    && [ "$(incus storage get "$E2E_POOL" user.subyard.owner --project default)" = \
      sudo-terminal-e2e-v1 ] \
    || die "refusing unexpected existing storage pool $E2E_POOL"
else
  incus storage create "$E2E_POOL" dir user.subyard.owner=sudo-terminal-e2e-v1 \
    --project default >/dev/null \
    || die "could not create marker-owned E2E storage pool"
fi
POOL_OWNED=1
real_token="$(printf '%s' "$token" | tr '[:upper:]' '[:lower:]')"
real_yard="sudo-e2e-$real_token"
real_project="subyard-$real_yard"
incus project show "$real_project" >/dev/null 2>&1 \
  && die "refusing existing real-command project $real_project"
REAL_PROJECT="$real_project"
REAL_INSTANCE="yard-$real_yard"
REAL_VOLUME="yard-srv-$real_yard"
stage_runtime "$real_repo"
install -d -m 0755 "$real_repo/config/profiles/sudo-smoke"
cat > "$real_repo/config/profiles/sudo-smoke/profile.conf" <<'EOF'
# Empty smoke profile: the provision hook only proves lifecycle and adapter continuation.
EOF
cat > "$real_repo/config/profiles/sudo-smoke/provision.sh" <<'SH'
#!/usr/bin/env bash
# subyard-provision-check-v1
set -euo pipefail
[ "${1:-}" != --check ] || exit 0
printf 'sudo-smoke-hook-ok\n'
SH
chmod 0755 "$real_repo/config/profiles/sudo-smoke/provision.sh"

real_config="$fixture_home/.config/subyard"
real_storage_path="$(incus storage get default source)"
install -d -m 0700 -o "$FIXTURE_USER" -g "$FIXTURE_USER" \
  "$fixture_home/.config" "$real_config" "$real_config/yards"
real_ssh_port=$((30000 + ($$ % 20000)))
while ss -H -ltn "sport = :$real_ssh_port" 2>/dev/null | grep >/dev/null .; do
  real_ssh_port=$((real_ssh_port + 1))
done
install -d -m 0700 "$real_config/yards/$real_yard" "$real_config/yards/sudo-e2e-sentinel"
cat > "$real_config/yards/$real_yard/config.env" <<EOF
SSH_PORT=$real_ssh_port
YARD_KIND=container
YARD_IMAGE=images:debian/13/cloud
YARD_IMAGE_FALLBACK=images:debian/13/cloud
DEV_USER=dev
DEV_UID=1001
ENVIRONMENT_PROFILES=sudo-smoke
CODING_TOOL_INTEGRATIONS=
SRV_POOL=$E2E_POOL
HOST_MOUNTS=sudo-host:/mnt/host/sudo:rw:0700
HOST_BASE=$fixture_home/host-real
RESTRICTED_DISK_PATHS=$fixture_home/host-real
FORWARD_SSH_AGENT=0
DEV_SUDO=0
NESTED_E2E_VMS=0
EOF
cat > "$real_config/yards/sudo-e2e-sentinel/config.env" <<'EOF'
SSH_PORT=29999
FORWARD_SSH_AGENT=0
DEV_SUDO=0
NESTED_E2E_VMS=0
EOF
chmod 0600 "$real_config"/yards/*/config.env
install -d -m 0700 -o "$FIXTURE_USER" -g "$FIXTURE_USER" "$real_config/tools/bin"
cat > "$real_config/tools/bin/age" <<'SH'
#!/usr/bin/env bash
[ "${1:-}" = --version ] && { printf 'age 1.3.1\n'; exit 0; }
exit 2
SH
cat > "$real_config/tools/bin/age-keygen" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  --version) printf 'age-keygen 1.3.1\n' ;;
  -o)
    recipient="age1$(printf '%s-%s-%s' "$2" "$$" "$RANDOM" | sha256sum | cut -c1-58)"
    printf 'FAKE:%s\n' "$recipient" > "$2"
    printf 'Public key: %s\n' "$recipient" >&2
    ;;
  -y) sed -n 's/^FAKE://p' "$2" ;;
  *) exit 2 ;;
esac
SH
cat > "$real_config/tools/bin/sops" <<'SH'
#!/usr/bin/env bash
[ "${1:-}" = --version ] && { printf 'sops 3.13.2\n'; exit 0; }
exit 2
SH
chmod 0755 "$real_config/tools/bin/age" \
  "$real_config/tools/bin/age-keygen" "$real_config/tools/bin/sops"
chown -R "$FIXTURE_USER:$FIXTURE_USER" "$real_config"
run_real_as_fixture() {
  run_as_fixture "$@"
}

run_real_checked() {
  local label="$1" output="$2"
  shift 2
  if ! run_real_as_fixture "$TMP/run-real-yard" "$@" >"$output" 2>&1; then
    tail -n 120 "$output" >&2 || true
    die "$label failed"
  fi
}

run_real_as_fixture sg incus-admin -c 'incus info >/dev/null' \
  || die 'fixture user cannot reach Incus through a fresh incus-admin group shell'
printf '  [ ok ] fresh incus-admin group shell reaches the daemon\n'

cat > "$TMP/run-real-yard-inner" <<SH
#!/usr/bin/env bash
exec env -i \\
  PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin' \\
  LANG='C.UTF-8' \\
  TERM='${TERM:-xterm}' \\
  HOME='$fixture_home' \\
  USER='$FIXTURE_USER' \\
  LOGNAME='$FIXTURE_USER' \\
  SHELL='/bin/bash' \\
  SUBYARD_OPERATOR_HOME='$fixture_home' \\
  SUBYARD_CONFIG_HOME='$real_config' \\
  SUBYARD_HOME='$fixture_home/.subyard-real' \\
  SUBYARD_NO_AUDIT=1 \\
  SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 \\
  SUBYARD_REPOSITORY_ROOT='$real_repo' \\
  STORAGE_PATH='$real_storage_path' \\
  MIN_DISK_GIB=1 \\
  HOST_CLAUDE_MD= \\
  HOST_CODEX_AGENTS_MD= \\
  HOST_OPENCODE_AGENTS_MD= \\
  SHIFT_MODE=shift \\
  FORWARD_SSH_AGENT=0 \\
  DEV_SUDO=0 \\
  DEV_UID=1001 \\
  DEV_USER=dev \\
  NESTED_E2E_VMS=0 \\
  '$real_repo/bin/yard-engine' -Y '$real_yard' "\$@"
SH
cat > "$TMP/run-real-yard" <<SH
#!/usr/bin/env bash
printf -v command_line '%q' '$TMP/run-real-yard-inner'
for argument in "\$@"; do
  printf -v quoted '%q' "\$argument"
  command_line+=" \$quoted"
done
exec sg incus-admin -c "\$command_line"
SH
chmod 0755 "$TMP/run-real-yard-inner"
chmod 0755 "$TMP/run-real-yard"
printf '  [ ok ] real-command names are absent and reserved for this run\n'

run_real_pty() {
  local label="$1" output="$2" command_line argument quoted rc
  shift 2
  command_line="$(printf '%q' "$TMP/run-real-yard")"
  for argument in "$@"; do
    printf -v quoted '%q' "$argument"
    command_line+=" $quoted"
  done
  run_real_as_fixture sudo -k
  printf '  [ .. ] %s\n' "$label"
  set +e
  run_prompted_pty "$output" "$command_line" "$password"$'\n'
  rc=$?
  set -e
  if [ "$rc" -ne 0 ]; then
    tr -d '\r' < "$output" \
      | sed "s/$password/[redacted]/g" \
      | tail -n 120 >&2
    if [ -n "$REAL_PROJECT" ] && [ -n "$REAL_INSTANCE" ] \
      && incus info "$REAL_INSTANCE" --project "$REAL_PROJECT" >/dev/null 2>&1; then
      incus info "$REAL_INSTANCE" --project "$REAL_PROJECT" --show-log >&2 || true
    fi
    die "$label failed with status $rc"
  fi
}

real_init_output="$TMP/real-init.out"
run_real_pty 'running real yard init with password-required sudo' "$real_init_output" \
  init --yes
grep -Fq '[sudo] password for' "$real_init_output" \
  || die 'real yard init did not show the standard sudo prompt'
grep -Fq 'Subyard initialized' "$real_init_output" \
  || die 'real yard init did not complete'
incus project show "$REAL_PROJECT" >/dev/null 2>&1 \
  || die 'real yard init did not create its isolated project'
incus info "$REAL_INSTANCE" --project "$REAL_PROJECT" >/dev/null 2>&1 \
  || die 'real yard init did not create its isolated instance'
printf '  [ ok ] real yard init accepted the password and completed\n'

run_real_checked 'real yard stop' "$TMP/real-stop.out" stop --force --yes
run_real_as_fixture sudo -k
run_real_checked 'real yard start' "$TMP/real-start.out" start --yes
grep -Fq 'started' "$TMP/real-start.out" || die 'real lifecycle start did not complete'
grep -Fq '[sudo] password for' "$TMP/real-start.out" \
  && die 'inactive NetworkManager caused an unnecessary lifecycle sudo prompt'
run_real_checked 'second real yard stop' "$TMP/real-restop.out" stop --force --yes
run_real_as_fixture sudo -k
run_real_checked 'real stopped-yard provision' "$TMP/real-provision.out" \
  provision sudo-smoke --yes
grep -Fq 'provisioned sudo-smoke' "$TMP/real-provision.out" \
  || die 'real stopped-yard provision did not complete'
grep -Fq '[sudo] password for' "$TMP/real-provision.out" \
  && die 'inactive NetworkManager caused an unnecessary provision sudo prompt'
printf '  [ ok ] real lifecycle and stopped-yard provision completed without an unneeded prompt\n'

real_teardown_output="$TMP/real-teardown.out"
run_real_pty 'running real yard teardown with password-required sudo' "$real_teardown_output" \
  teardown --yes
grep -Fq '[sudo] password for' "$real_teardown_output" \
  || die 'real yard teardown did not show the standard sudo prompt'
incus project show "$REAL_PROJECT" >/dev/null 2>&1 \
  && die 'real yard teardown left its isolated project'
incus storage show default --project default >/dev/null 2>&1 \
  || die 'real yard teardown removed the shared storage pool'
incus network show incusbr0 --project default >/dev/null 2>&1 \
  || die 'real yard teardown removed the shared bridge'
REAL_PROJECT=''
REAL_INSTANCE=''
REAL_VOLUME=''
printf '  [ ok ] real teardown completed and preserved shared Incus infrastructure\n'

password=''
printf 'ok: password-required sudo owns the PTY and real init/lifecycle/provision/teardown paths\n'

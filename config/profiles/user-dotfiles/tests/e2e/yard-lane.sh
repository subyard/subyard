#!/usr/bin/env bash
# Installed-candidate clone and install: no host instruction copy, nonfatal source failure,
# retry on the next init, one install run whose links every agent reads, and uninstall on disable.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARDS=()
CANDIDATE_ENGINE=''

die() { printf 'user-dotfiles-yard: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
[ "$(id -un)" = dev ] || die 'run as the allocated dev user'
for command in incus jq sg sudo timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
if ! id -nG | tr ' ' '\n' | grep -Fx >/dev/null incus-admin \
  && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fx >/dev/null incus-admin; then
  printf -v reexec 'exec env SUBYARD_E2E_VM=%q bash %q' "$SUBYARD_E2E_VM" "$ROOT/config/profiles/user-dotfiles/tests/e2e/yard-lane.sh"
  exec sg incus-admin -c "$reexec"
fi

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() { # <name> <arguments...>
  local name="$1" command
  shift
  # Init refreshes its child session; the fixture's parent keeps the old groups.
  if ! id -nG | tr ' ' '\n' | grep -Fx >/dev/null incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fx >/dev/null incus-admin; then
    printf -v command '%q ' "$CANDIDATE_ENGINE" -Y "$name" "$@"
    sg incus-admin -c "exec $command"
  else
    "$CANDIDATE_ENGINE" -Y "$name" "$@"
  fi
}
as_dev() { # <yard> <command...>
  local name="$1"
  shift
  incus exec "yard-$name" --project "subyard-$name" --user "$(incus exec "yard-$name" --project "subyard-$name" -- id -u dev)" \
    --group "$(incus exec "yard-$name" --project "subyard-$name" -- id -g dev)" --env HOME=/home/dev -- "$@"
}
ready() { yard "$1" profile status user-dotfiles --json | jq -r .ready; }

cleanup() {
  local original=$? cleanup_rc=0 teardown_rc name
  trap - EXIT INT TERM
  set +e
  if [ "${#YARDS[@]}" -gt 0 ]; then
    # A registered sentinel keeps the last-yard teardown from retiring the shared base platform.
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64997\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
  fi
  for name in "${YARDS[@]}"; do
    [ -f "$SUBYARD_CONFIG_HOME/yards/$name/config.env" ] || continue
    printf 'user-dotfiles_e2e_cleanup=teardown yard=%s state=start\n' "$name" >&2
    teardown_rc=0
    yard "$name" teardown --yes >&2 || teardown_rc=$?
    printf 'user-dotfiles_e2e_cleanup=teardown yard=%s state=end exit_code=%s\n' "$name" "$teardown_rc" >&2
    [ "$teardown_rc" -eq 0 ] || cleanup_rc=3
  done
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-user-dotfiles.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = user-dotfiles-yard-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || cleanup_rc=3
  fi
  printf 'USER_DOTFILES_YARD_RESULT original_exit_code=%s cleanup_exit_code=%s\n' "$original" "$cleanup_rc"
  [ "$original" -eq 0 ] || exit "$original"
  exit "$cleanup_rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

STATE="$(mktemp -d /var/tmp/subyard-user-dotfiles.XXXXXX)"
printf '%s\n' user-dotfiles-yard-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME"

# A release run installs the frozen packaged candidate; development runs package the
# current worktree. Both go through the supported installer.
if [ ! -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
  command -v go >/dev/null 2>&1 || die 'Go is required to package the development candidate'
fi
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
set +e
CANDIDATE_ENGINE="$(release_candidate_prepare "$ROOT")"
candidate_status=$?
set -e
case "$candidate_status" in
  0) ;;
  1) "$ROOT/dev/build-engine.sh" --force ;;
  *) die 'frozen release candidate is invalid' ;;
esac
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$STATE/incus-foundation"
  export SUBYARD_USER=dev SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
  export SUBYARD_DISPATCHER_PATH="${CANDIDATE_ENGINE:-$ROOT/.build/yard}"
  export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
  set -a
  # shellcheck source=config/host.env
  . "$ROOT/config/host.env"
  set +a
  bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
)
timeout 10 /usr/bin/incus query /1.0 >/dev/null
# Keep the standard per-yard host roots distinct during named-yard creation.
printf 'STORAGE_PATH=%s\n' "$STORAGE_PATH" \
  > "$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
export YARD_RUNTIME_ROOT="$STATE/runtime"
unset YARD_ENGINE_PATH
CANDIDATE_ENGINE="$(release_candidate_install "$ROOT" "$STATE")"



first="ud-e2e-$token"
source_url="ssh://dev@localhost/home/dev/lane-dotfiles.git"
clone=/home/dev/.local/share/subyard-user-dotfiles/dev/lane-dotfiles
links=(/home/dev/.claude/CLAUDE.md /home/dev/.codex/AGENTS.md /home/dev/.config/opencode/AGENTS.md)
runs() { as_dev "$first" sh -c 'cat "$HOME/lane-runs" 2>/dev/null || :'; }

printf 'user-dotfiles_e2e_stage=create\n'
YARDS+=("$first")
# Host instructions are never copied into the yard; the disposable VM keeps this file.
if [ ! -e "$HOME/.claude/CLAUDE.md" ]; then
  install -d -m 0700 "$HOME/.claude"
  printf 'Host instructions.\n' > "$HOME/.claude/CLAUDE.md"
fi
yard "$first" init --yes
# Init preserves desired power; guest-side checks require a running yard.
yard "$first" start --yes
for link in "${links[@]}"; do
  ! as_dev "$first" test -e "$link" -o -L "$link" || die "$link exists before the profile is enabled"
done
# The lane repository links its instructions the way subyard/yard-dotfiles does and
# records each run of its scripts.
# shellcheck disable=SC2016
as_dev "$first" sh -euc '
work="$(mktemp -d)"
mkdir -p "$work/agents"
printf "%s\n" "Lane yard instructions." > "$work/agents/AGENTS.md"
cat > "$work/install.sh" <<"SCRIPT"
#!/bin/sh
set -eu
echo install >> "$HOME/lane-runs"
for link in .claude/CLAUDE.md .codex/AGENTS.md .config/opencode/AGENTS.md; do
  mkdir -p "$HOME/${link%/*}"
  ln -sfT "$PWD/agents/AGENTS.md" "$HOME/$link"
done
SCRIPT
cat > "$work/uninstall.sh" <<"SCRIPT"
#!/bin/sh
set -eu
echo uninstall >> "$HOME/lane-runs"
for link in .claude/CLAUDE.md .codex/AGENTS.md .config/opencode/AGENTS.md; do
  [ "$(readlink "$HOME/$link" || :)" != "$PWD/agents/AGENTS.md" ] || rm -f "$HOME/$link"
done
SCRIPT
chmod +x "$work/install.sh" "$work/uninstall.sh"
git -C "$work" init -q -b main
git -C "$work" add -A
git -C "$work" -c user.name=lane -c user.email=lane@example.org commit -q -m dotfiles
git clone -q --bare "$work" "$HOME/lane-dotfiles.git"
'

printf 'user-dotfiles_e2e_stage=enable-unavailable\n'
yard "$first" config set USER_DOTFILES_SOURCE "$source_url" --scope yard --yes
yard "$first" profile enable user-dotfiles --yes 2>"$STATE/enable-unavailable.log"
grep -F >/dev/null 'user-dotfiles: warning: could not clone dev/lane-dotfiles;' "$STATE/enable-unavailable.log" \
  || die 'missing enable retry warning'
[ "$(ready "$first")" = false ] || die 'enable with an unreachable source reported ready'
[ -z "$(runs)" ] || die 'an install ran without a clone'

printf 'user-dotfiles_e2e_stage=retry\n'
# Access comes from an in-yard IdentityFile, not the per-yard agent socket: unlocking
# an owner key into that socket needs owner-side key material the lane does not have.
# shellcheck disable=SC2016
as_dev "$first" sh -euc '
install -d -m 0700 "$HOME/.ssh"
ssh-keygen -q -t ed25519 -N "" -f "$HOME/.ssh/lane_key"
cat "$HOME/.ssh/lane_key.pub" >> "$HOME/.ssh/authorized_keys"
chmod 0600 "$HOME/.ssh/authorized_keys"
ssh-keyscan localhost 2>/dev/null >> "$HOME/.ssh/known_hosts"
printf "Host localhost\n  IdentityFile ~/.ssh/lane_key\n" >> "$HOME/.ssh/config"
'
yard "$first" init --yes
for link in "${links[@]}"; do
  [ "$(as_dev "$first" readlink "$link")" = "$clone/agents/AGENTS.md" ] || die "install did not link $link"
  # Each agent opens its instruction file through the link.
  [ "$(as_dev "$first" cat "$link")" = 'Lane yard instructions.' ] || die "link $link is not readable"
done
# Install runs once: the next init leaves it alone.
yard "$first" init --yes
[ "$(runs)" = install ] || die "install ran other than once: $(runs)"
[ "$(ready "$first")" = true ] || die 'the profile is not ready after retry'

printf 'user-dotfiles_e2e_stage=disable\n'
yard "$first" profile disable user-dotfiles --yes
for link in "${links[@]}"; do
  ! as_dev "$first" test -L "$link" || die "uninstall kept link $link"
done
[ "$(runs)" = $'install\nuninstall' ] || die "disable did not run uninstall.sh once: $(runs)"
as_dev "$first" test -f "$clone/agents/AGENTS.md" || die 'disable removed the clone'
printf 'ok: user-dotfiles no host copy, unavailable source warning, retry, install once and uninstall on disable\n'

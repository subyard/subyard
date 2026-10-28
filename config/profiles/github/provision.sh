#!/usr/bin/env bash
# subyard-provision-check-v1
# Install the ordinary GitHub CLI and the managed, short-lived-token client wiring.
set -euo pipefail

mode=apply
case "${1:-}" in
  --check) mode=check; shift ;;
  --check-removed) mode=check-removed; shift ;;
  --remove) mode=remove; shift ;;
  --refresh-skills) mode=refresh-skills; shift ;;
  --register-skills) mode=register-skills; shift ;;
  '') ;;
  *) printf 'github provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { printf 'github provision: unexpected argument\n' >&2; exit 2; }

DEV_USER="${DEV_USER:-dev}"
DEV_HOME="$(getent passwd "$DEV_USER" | cut -d: -f6)"
[ -n "$DEV_HOME" ] || { printf 'github provision: user %s not found\n' "$DEV_USER" >&2; exit 1; }
DEV_GROUP="$(id -gn "$DEV_USER")"

wrapper=/usr/local/bin/subyard-github
engine=/usr/local/libexec/subyard/github-client
remote=/usr/local/bin/git-remote-subyard-github
remote_source="$(dirname "$0")/bin/git-remote-subyard-github"
wiring_source="$(dirname "$0")/git_wiring.py"
git_mode="${GITHUB_GIT_MODE:-off}"
case "$git_mode" in off|broker-first|broker-last) ;; *) printf 'github provision: invalid Git mode\n' >&2; exit 2 ;; esac
wiring() { python3 "$wiring_source" "$DEV_HOME" "$git_mode" "$1"; }
remote_body() { cat "$remote_source"; }
skill_delivery() {
  local helper
  helper="$(dirname "$0")/agent-skills.py"
  [ -f "$helper" ] || helper="$(dirname "$0")/../../skills/deliver.py"
  [ -f "$helper" ] || helper=/usr/local/libexec/subyard/agent-skills
  python3 "$helper" "$@" --provider profile-github --source "$(dirname "$0")/skills.json" --home "$DEV_HOME" --uid "$(id -u "$DEV_USER")" --gid "$(id -g "$DEV_USER")" --extra
}
skill_dir=/usr/local/share/subyard/skills/subyard-github
skill="$skill_dir/SKILL.md"
socket_dir="$DEV_HOME/.local/share/subyard"
socket="$socket_dir/github.sock"
skill_source="$(dirname "$0")/SKILL.md"
wrapper_source="$(dirname "$0")/bin/subyard-github"

skill_links=(
  "$DEV_HOME/.claude/skills/subyard-github"
  "$DEV_HOME/.agents/skills/subyard-github"
  "$DEV_HOME/.config/opencode/skills/subyard-github"
  "$DEV_HOME/.pi/agent/skills/subyard-github"
  "$DEV_HOME/.hermes/skills/subyard-github"
)

wrapper_body() { cat "$wrapper_source"; }
skill_body() { cat "$skill_source"; }
owned_file() {
  local path=$1
  local generator=$2
  [ -f "$path" ] && [ ! -L "$path" ] \
    && (grep -Fqx '# subyard-managed-v1' "$path" || grep -Fqx '<!-- subyard-managed-v1 -->' "$path" || cmp -s <("$generator") "$path")
}

link_matches() {
  local path=$1
  [ -L "$path" ] && [ "$(readlink "$path")" = "$skill_dir" ]
}

socket_dir_ready() {
  [ -d "$socket_dir" ] && [ ! -L "$socket_dir" ] \
    && [ "$(stat -c '%u:%g:%a' "$socket_dir" 2>/dev/null)" = "$(id -u "$DEV_USER"):$(id -g "$DEV_USER"):700" ]
}

socket_path_safe() {
  local path
  for path in "$DEV_HOME/.local" "$DEV_HOME/.local/share" "$socket_dir"; do
    [ ! -L "$path" ] || return 1
  done
}

# Create only missing directories; never change ownership of existing agent state.
user_directory() {
  local path=$1 index
  local -a missing=()
  while [ "$path" != "$DEV_HOME" ]; do
    [ ! -L "$path" ] || { printf 'github provision: symlinked agent directory\n' >&2; return 1; }
    if [ ! -e "$path" ]; then missing+=("$path");
    elif [ ! -d "$path" ]; then return 1; fi
    path="$(dirname "$path")"
  done
  for ((index=${#missing[@]}-1; index>=0; index--)); do
    install -d -o "$DEV_USER" -g "$DEV_GROUP" -m 0755 "${missing[index]}"
  done
}

# Static refresh cannot register a capability whose full provision never succeeded.
if [ "$mode" = refresh-skills ]; then
  skill_delivery apply --registered
  exit 0
fi
if [ "$mode" = register-skills ]; then
  # The owner invokes this only after its first successful runtime handshake.
  skill_delivery apply
  exit 0
fi

changed=0
if [ "$mode" = check-removed ]; then
  owned_file "$remote" remote_body && changed=1
  python3 "$wiring_source" "$DEV_HOME" off check || changed=1
  owned_file "$wrapper" wrapper_body && changed=1
  skill_delivery check-removed || changed=1
  # Predecessor installations have no shared receipt. Retain their explicit
  # marked-content and exact-target absence checks until cleanup runs.
  owned_file "$skill" skill_body && changed=1
  link_matches "$DEV_HOME/.codex/skills/subyard-github" && changed=1
  for link in "${skill_links[@]}"; do
    link_matches "$link" && changed=1
  done
  socket_path_safe || changed=1
  [ ! -e "$socket" ] && [ ! -L "$socket" ] || changed=1
  [ "$changed" -eq 0 ] && exit 0
  exit 10
fi
if [ "$mode" = check ] || [ "$mode" = remove ]; then
  command -v gh >/dev/null 2>&1 || changed=1
  owned_file "$remote" remote_body && cmp -s <(remote_body) "$remote" \
    && [ "$(stat -c '%a:%u:%g' "$remote")" = 755:0:0 ] || changed=1
  wiring check || changed=1
  [ -x "$engine" ] || changed=1
  owned_file "$wrapper" wrapper_body && cmp -s <(wrapper_body) "$wrapper" || changed=1
  skill_delivery check || changed=1
  socket_dir_ready || changed=1
  for link in "${skill_links[@]}"; do
    link_matches "$link" || changed=1
  done
  if [ "$mode" = check ]; then
    [ "$changed" -eq 0 ] && exit 0
    exit 10
  fi
fi

if [ "$mode" = remove ]; then
  wiring remove
  owned_file "$remote" remote_body && rm -f -- "$remote"
  socket_path_safe || { printf 'github provision: refusing symlinked socket path\n' >&2; exit 1; }
  [ ! -L "$skill_dir" ] || { printf 'github provision: refusing symlinked skill directory\n' >&2; exit 1; }
  if [ -L "$socket" ] || { [ -e "$socket" ] && [ ! -S "$socket" ]; }; then
    printf 'github provision: refusing unmanaged socket path: %s\n' "$socket" >&2
    exit 1
  fi
  [ ! -e "$socket" ] || rm -f -- "$socket"
  for link in "${skill_links[@]}"; do
    link_matches "$link" && rm -f -- "$link"
  done
  owned_file "$wrapper" wrapper_body && rm -f -- "$wrapper"
  # Earlier installers predate shared receipts. Keep their explicit marked-file
  # and exact-target cleanup contract, then let the shared owner retire receipts.
  if [ ! -f /var/lib/subyard/agent-skills/profile-github.json ]; then
    legacy_link="$DEV_HOME/.codex/skills/subyard-github"
    link_matches "$legacy_link" && rm -f -- "$legacy_link"
    owned_file "$skill" skill_body && rm -f -- "$skill"
  fi
  skill_delivery remove
  for directory in \
    "$DEV_HOME/.claude/skills" "$DEV_HOME/.codex/skills" \
    "$DEV_HOME/.config/opencode/skills" "$DEV_HOME/.pi/agent/skills" \
    "$DEV_HOME/.hermes/skills"; do
    rmdir --ignore-fail-on-non-empty "$directory" 2>/dev/null || true
  done
  rmdir --ignore-fail-on-non-empty "$skill_dir" 2>/dev/null || true
  exit 0
fi

[ "$(id -u)" -eq 0 ] || { printf 'github provision: must run as root\n' >&2; exit 1; }
[ -x "$engine" ] || { printf 'github provision: select the github profile and run yard init first\n' >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive
[ ! -L "$skill_dir" ] || { printf 'github provision: refusing symlinked skill directory\n' >&2; exit 1; }
if [ -e "$remote" ] || [ -L "$remote" ]; then
  owned_file "$remote" remote_body || { printf 'github provision: unmanaged remote helper exists\n' >&2; exit 1; }
fi
if [ -e "$wrapper" ] || [ -L "$wrapper" ]; then
  owned_file "$wrapper" wrapper_body \
    || { printf 'github provision: unmanaged wrapper exists: %s\n' "$wrapper" >&2; exit 1; }
fi
if [ -e "$skill" ] || [ -L "$skill" ]; then
  owned_file "$skill" skill_body \
    || { printf 'github provision: unmanaged skill exists: %s\n' "$skill" >&2; exit 1; }
fi
if ! command -v gh >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq gh
fi
install -d -m 0755 /usr/local/bin /usr/local/libexec/subyard /usr/local/share/subyard/skills

install -m 0755 "$wrapper_source" "$wrapper"
install -m 0755 "$remote_source" "$remote"
wiring apply

socket_path_safe || { printf 'github provision: refusing symlinked socket path\n' >&2; exit 1; }
if [ -L "$socket_dir" ]; then
  printf 'github provision: refusing symlinked socket directory: %s\n' "$socket_dir" >&2
  exit 1
elif [ ! -e "$socket_dir" ]; then
  user_directory "$(dirname "$socket_dir")"
  install -d -o "$DEV_USER" -g "$DEV_GROUP" -m 0700 "$socket_dir"
elif ! socket_dir_ready; then
  printf 'github provision: unmanaged socket directory has wrong owner or mode: %s\n' "$socket_dir" >&2
  exit 1
fi

# Ordinary guest provisioning alone cannot prove owner-service activation.
skill_delivery apply --registered

printf 'github provision OK: gh and managed GitHub broker client are ready\n'

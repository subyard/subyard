#!/usr/bin/env bash
# subyard-provision-check-v1
# Install the ordinary GitHub CLI and the managed, short-lived-token client wiring.
set -euo pipefail

mode=apply
case "${1:-}" in
  --check) mode=check; shift ;;
  --check-removed) mode=check-removed; shift ;;
  --remove) mode=remove; shift ;;
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
skill_dir=/usr/local/share/subyard/skills/subyard-github
skill="$skill_dir/SKILL.md"
socket_dir="$DEV_HOME/.local/share/subyard"
socket="$socket_dir/github.sock"
skill_source="$(dirname "$0")/SKILL.md"
wrapper_source="$(dirname "$0")/bin/subyard-github"

skill_links=(
  "$DEV_HOME/.claude/skills/subyard-github"
  "$DEV_HOME/.codex/skills/subyard-github"
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

changed=0
if [ "$mode" = check-removed ]; then
  owned_file "$wrapper" wrapper_body && changed=1
  owned_file "$skill" skill_body && changed=1
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
  [ -x "$engine" ] || changed=1
  owned_file "$wrapper" wrapper_body && cmp -s <(wrapper_body) "$wrapper" || changed=1
  owned_file "$skill" skill_body && cmp -s <(skill_body) "$skill" || changed=1
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
  owned_file "$skill" skill_body && rm -f -- "$skill"
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
if [ ! -e "$skill_dir" ]; then
  install -d -m 0755 "$skill_dir"
elif [ ! -d "$skill_dir" ] || [ -L "$skill_dir" ]; then
  printf 'github provision: unmanaged skill directory exists: %s\n' "$skill_dir" >&2
  exit 1
fi
install -m 0755 "$wrapper_source" "$wrapper"
install -m 0644 "$skill_source" "$skill"
for link in "${skill_links[@]}"; do
  if [ -e "$link" ] || [ -L "$link" ]; then
    link_matches "$link" || { printf 'github provision: unmanaged skill path exists: %s\n' "$link" >&2; exit 1; }
    continue
  fi
  parent="$(dirname "$link")"
  user_directory "$parent"
  ln -s "$skill_dir" "$link"
done

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

printf 'github provision OK: gh and managed GitHub broker client are ready\n'

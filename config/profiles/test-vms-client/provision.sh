#!/usr/bin/env bash
# subyard-provision-check-v1
# Register the shared test-VM broker client and its skill for this yard's agents.
set -euo pipefail

mode=apply
case "${1:-}" in
  --check) mode=check; shift ;;
  '') ;;
  *) printf 'test-vms-client provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { printf 'test-vms-client provision: unexpected argument\n' >&2; exit 2; }

PROFILE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEV_USER="${DEV_USER:-dev}"
# Host-free tests deliver below a private root as the invoking user.
TEST_ROOT="${TEST_VMS_CLIENT_TEST_ROOT:-}"
tools=(jq ssh ssh-keygen setsid flock python3)

die() { printf 'test-vms-client provision: %s\n' "$*" >&2; exit 1; }

tools_ready() {
  local tool
  for tool in "${tools[@]}"; do command -v "$tool" >/dev/null 2>&1 || return 1; done
}

# The client is the skill's supporting file: one shared receipt owns its delivery,
# static refresh and removal on deselection. --extra adds the pi and Hermes links,
# which need no coding-tool integration.
skill_delivery() {
  local helper="$PROFILE_DIR/agent-skills.py" home uid gid
  local -a root=()
  [ -f "$helper" ] || helper="$PROFILE_DIR/../../skills/deliver.py"
  [ -f "$helper" ] || helper=/usr/local/libexec/subyard/agent-skills
  if [ -n "$TEST_ROOT" ]; then
    root=(--root "$TEST_ROOT")
    home="$TEST_ROOT/home/$DEV_USER"
    uid="$(id -u)"
    gid="$(id -g)"
  else
    home="$(getent passwd "$DEV_USER" | cut -d: -f6)"
    [ -n "$home" ] || die "user $DEV_USER not found"
    uid="$(id -u "$DEV_USER")"
    gid="$(id -g "$DEV_USER")"
  fi
  python3 "$helper" "$1" --provider profile-test-vms-client --source "$PROFILE_DIR/skills" \
    --home "$home" --uid "$uid" --gid "$gid" --extra "${root[@]}"
}

if [ "$mode" = check ]; then
  changed=0
  tools_ready || changed=1
  # Static refresh owns content and link drift after successful registration.
  skill_delivery check-registered || changed=1
  [ "$changed" = 0 ] && exit 0
  exit 10
fi

[ -n "$TEST_ROOT" ] || [ "$(id -u)" -eq 0 ] || die 'must run as root'
if ! tools_ready; then
  [ -z "$TEST_ROOT" ] || die 'client tools are missing'
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq jq openssh-client python3 util-linux
fi
tools_ready || die 'client tools are incomplete after package installation'
skill_delivery apply
printf 'test-vms-client provision OK: client and subyard-test-vms skill registered\n'

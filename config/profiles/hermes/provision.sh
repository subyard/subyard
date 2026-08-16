#!/usr/bin/env bash
# subyard-provision-check-v1
# Prepare a private yard and delegate the Hermes runtime installation to the official installer.
set -euo pipefail

check_only=0
case "${1:-}" in
  --check) check_only=1; shift ;;
  '') ;;
  *) printf 'hermes provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { printf 'hermes provision: unexpected argument\n' >&2; exit 2; }

die() { printf 'hermes provision: %s\n' "$*" >&2; exit 1; }
if [ "$(id -u)" -ne 0 ] && [ "${HERMES_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  die 'must run as root'
fi

profile_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repository=https://github.com/NousResearch/hermes-agent.git
repository_ssh=git@github.com:NousResearch/hermes-agent.git
resolver="$profile_dir/hermes-release-resolve.py"
DEV_USER="${DEV_USER:-dev}"
DEV_GROUP="${DEV_GROUP:-$(id -gn "$DEV_USER")}"
DEV_HOME="${HERMES_DEV_HOME:-$(getent passwd "$DEV_USER" | cut -d: -f6)}"
DEV_HOME="${DEV_HOME:-/home/$DEV_USER}"
state_root="$DEV_HOME/.hermes"
source_root="$state_root/hermes-agent"
venv_python="$source_root/venv/bin/python"
source_entrypoint="$source_root/hermes"
launcher="$DEV_HOME/.local/bin/hermes"

[ -d "$DEV_HOME" ] && [ ! -L "$DEV_HOME" ] || die 'dev home is missing or unsafe'
[ -x "$resolver" ] || die 'stable-release resolver is missing'

run_as_dev() {
  runuser -u "$DEV_USER" -- \
    bash -c 'cd "$1" && shift && exec "$@"' _ "$DEV_HOME" "$@"
}

managed_install_structure_ready() {
  local origin
  [ -d "$state_root" ] && [ ! -L "$state_root" ] || return 1
  [ "$(stat -c %u:%g "$state_root" 2>/dev/null)" = "$(id -u "$DEV_USER"):$(id -g "$DEV_USER")" ] \
    || return 1
  [ "$(stat -c %a "$state_root" 2>/dev/null)" = 700 ] || return 1
  [ -d "$source_root/.git" ] && [ ! -L "$source_root" ] || return 1
  [ -f "$source_root/.install_method" ] && [ ! -L "$source_root/.install_method" ] \
    && [ "$(<"$source_root/.install_method")" = git ] || return 1
  [ -x "$venv_python" ] && [ -f "$source_entrypoint" ] && [ -x "$launcher" ] || return 1
  [ "$(stat -c %u:%g "$launcher" 2>/dev/null)" = \
    "$(id -u "$DEV_USER"):$(id -g "$DEV_USER")" ] || return 1
  origin="$(run_as_dev git -C "$source_root" remote get-url origin 2>/dev/null)" || return 1
  [ "$origin" = "$repository" ] || [ "$origin" = "$repository_ssh" ]
}

if [ "$check_only" -eq 1 ]; then
  managed_install_structure_ready && exit 0
  exit 10
fi

if managed_install_structure_ready; then
  printf 'hermes provision OK: official per-user installation already present\n'
  exit 0
fi

export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a
apt-get update -qq
apt-get install -y -qq \
  build-essential ca-certificates curl git libffi-dev python3-dev xz-utils \
  >/dev/null

if [ -e "$state_root" ] && [ -L "$state_root" ]; then
  die "$state_root must not be a symlink"
fi
run_as_dev install -d -m 0700 "$state_root"

IFS=$'\t' read -r release_tag _published_at _release_url < <("$resolver")
[[ "$release_tag" =~ ^v[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}(\.[0-9]+)?$ ]] \
  || die 'release resolver returned an unsafe tag'
release_lookup_timeout=120s
release_lookup_kill_after=5s
if [ "${HERMES_TEST_ALLOW_NON_ROOT:-0}" = 1 ]; then
  release_lookup_timeout="${HERMES_TEST_RELEASE_LOOKUP_TIMEOUT:-$release_lookup_timeout}"
  release_lookup_kill_after="${HERMES_TEST_RELEASE_LOOKUP_KILL_AFTER:-$release_lookup_kill_after}"
fi
if ! remote_refs="$(timeout --kill-after="$release_lookup_kill_after" \
  "$release_lookup_timeout" git ls-remote --tags "$repository" \
  "refs/tags/$release_tag" "refs/tags/$release_tag^{}")"; then
  die "official release tag lookup failed or exceeded $release_lookup_timeout"
fi
release_sha="$(printf '%s\n' "$remote_refs" |
  awk -v ref="refs/tags/$release_tag^{}" '$2 == ref {print $1}')"
if [ -z "$release_sha" ]; then
  release_sha="$(printf '%s\n' "$remote_refs" |
    awk -v ref="refs/tags/$release_tag" '$2 == ref {print $1}')"
fi
[[ "$release_sha" =~ ^[0-9a-f]{40}$ ]] \
  || die 'official release tag did not resolve to one commit'

bootstrap_checkout="$(mktemp -d /var/tmp/subyard-hermes-bootstrap.XXXXXX)"
cleanup() {
  case "$bootstrap_checkout" in
    /var/tmp/subyard-hermes-bootstrap.*) run_as_dev rm -rf -- "$bootstrap_checkout" ;;
  esac
}
trap cleanup EXIT
chmod 0700 "$bootstrap_checkout"
chown "$DEV_USER:$DEV_GROUP" "$bootstrap_checkout"
run_as_dev git clone --quiet --depth 1 --branch "$release_tag" \
  "$repository" "$bootstrap_checkout/source"
bootstrap_sha="$(run_as_dev git -C "$bootstrap_checkout/source" rev-parse HEAD)"
[ "$bootstrap_sha" = "$release_sha" ] \
  || die 'official release checkout did not match the resolved tag commit'
installer="$bootstrap_checkout/source/scripts/install.sh"
if ! run_as_dev test -f "$installer" || ! run_as_dev test ! -L "$installer"; then
  die 'official release checkout did not contain scripts/install.sh'
fi
run_as_dev bash -n "$installer" || die 'official installer is not valid Bash'

# The upstream installer explicitly exposes NODE_DEPS_TIMEOUT for slow links.
# Clean 4 GiB yards can exceed its 600-second default while resolving the full
# browser workspace, so keep the stage bounded but within Subyard's 45-minute
# profile-action deadline.
run_as_dev env \
  HOME="$DEV_HOME" \
  SHELL=/bin/bash \
  HERMES_HOME="$state_root" \
  NODE_DEPS_TIMEOUT=1200 \
  bash "$installer" \
    --branch "$release_tag" \
    --commit "$release_sha" \
    --force-commit \
    --skip-setup \
    --non-interactive

run_as_dev chmod 0700 "$state_root"
managed_install_structure_ready || die 'official installer did not produce the supported per-user layout'

printf 'hermes provision OK: installed official stable release %s\n' "$release_tag"

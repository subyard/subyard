#!/usr/bin/env bash
# Installs Ubuntu packages on a disposable GitHub-hosted runner. Runner images ship a
# packages.microsoft.com source that no job installs from, so its outages must not block a job.
# Hosted runners have no IPv6 route, and apt does not retry "Network is unreachable", so every
# later apt call in the job, including product installs, uses IPv4 with bounded retries.
# Any other apt failure stops this script, so the step that needs the packages fails at once.
set -euo pipefail

die() { printf 'apt-install: %s\n' "$*" >&2; exit 2; }

[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] \
  || die 'refusing to change apt sources outside a disposable GitHub-hosted runner'
[ "$#" -gt 0 ] || die 'usage: dev/ci/apt-install.sh [APT-INSTALL-OPTION...] PACKAGE...'

# Overridable only so host-free tests can use fixture directories.
sources="${SUBYARD_CI_APT_SOURCES:-/etc/apt/sources.list.d}"
disabled="${SUBYARD_CI_APT_DISABLED:-/etc/apt/sources.disabled-by-ci}"
conf="${SUBYARD_CI_APT_CONF:-/etc/apt/apt.conf.d}/80-subyard-ci-network"

# Retries back off 1, 2, 4, 8 and 16 seconds, so a missing file fails within a minute.
printf '%s\n' 'Acquire::ForceIPv4 "true";' 'Acquire::Retries "5";' \
  | sudo -n install -m 0644 /dev/stdin "$conf"

shopt -s nullglob
for source in "$sources"/*.list "$sources"/*.sources; do
  grep -q 'packages\.microsoft\.com' "$source" || continue
  sudo -n install -d -m 0755 "$disabled"
  sudo -n mv -- "$source" "$disabled/"
  printf 'apt-install: disabled unused runner source %s\n' "$source"
done

sudo -n apt-get update -qq
sudo -n apt-get install -y "$@"

#!/usr/bin/env bash
# Real-network smoke for both Zabbly repository callers on one leased disposable VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KEY=/etc/apt/keyrings/zabbly.asc
SOURCE=/etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources
PIN=/etc/apt/preferences.d/zabbly-incus.pref
TMP=''
REPO_FILES=("$KEY" "$SOURCE" "$PIN")
BACKUP_READY=0

die() { printf 'zabbly-download-smoke: %s\n' "$*" >&2; exit 2; }

cleanup() {
  local rc=$? cleanup_rc=0 path saved
  trap - EXIT INT TERM
  set +e
  if [ -n "$TMP" ] && [[ "$TMP" = /tmp/subyard-zabbly-smoke.* ]] && [ -d "$TMP" ]; then
    if [ "$BACKUP_READY" = 1 ]; then
      for path in "${REPO_FILES[@]}"; do
        saved="$TMP/${path##*/}"
        rm -f -- "$path" || { cleanup_rc=3; continue; }
        if [ -f "$saved" ]; then
          cp -a -- "$saved" "$path" || cleanup_rc=3
        fi
      done
    fi
    if [ "$cleanup_rc" = 0 ]; then
      find "$TMP" -depth -delete || cleanup_rc=3
    fi
  fi
  [ "$rc" != 0 ] || rc=$cleanup_rc
  exit "$rc"
}
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
if [ "$(id -u)" -ne 0 ]; then
  exec sudo -n env SUBYARD_E2E_VM="$SUBYARD_E2E_VM" bash "$0"
fi
for command in apt-get curl dpkg timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
TMP="$(mktemp -d /tmp/subyard-zabbly-smoke.XXXXXX)"
chmod 0700 "$TMP"
for path in "${REPO_FILES[@]}"; do
  if [ -f "$path" ] && [ ! -L "$path" ]; then
    cp -a -- "$path" "$TMP/"
  elif [ -e "$path" ] || [ -L "$path" ]; then
    die "refusing unsafe existing repository path $path"
  fi
done
BACKUP_READY=1

# shellcheck source=scripts/lib/ui.sh
. "$ROOT/scripts/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$ROOT/scripts/lib/host.sh"
# shellcheck source=scripts/e2e-lab/provision.sh
. "$ROOT/scripts/e2e-lab/provision.sh"

assert_key() {
  [ -s "$KEY" ] || die 'caller did not install a non-empty key'
  [ "$(stat -c '%a:%u:%g' "$KEY")" = 644:0:0 ] \
    || die 'caller installed the key with the wrong mode or owner'
  [ -z "$(find /etc/apt/keyrings -maxdepth 1 -name '.zabbly.asc.tmp.*' -print -quit)" ] \
    || die 'caller left a temporary key behind'
}

rm -f -- "$KEY" "$SOURCE"
add_zabbly_lts_repo
assert_key
host_hash="$(sha256sum "$KEY")"
add_zabbly_lts_repo
[ "$(sha256sum "$KEY")" = "$host_hash" ] || die 'host caller changed the converged key'

rm -f -- "$KEY" "$SOURCE"
reconcile_inner_zabbly_repo
assert_key
inner_hash="$(sha256sum "$KEY")"
reconcile_inner_zabbly_repo
[ "$(sha256sum "$KEY")" = "$inner_hash" ] || die 'e2e-lab caller changed the converged key'

printf 'PASS: both Zabbly callers install and converge on a leased disposable VM\n'

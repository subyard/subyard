#!/usr/bin/env bash
# Trusted image builder recipe. No checkout, operator settings or lease keys.
set -euo pipefail

base_update_distro() {
  # A refreshed immutable image must contain current distro/security updates,
  # including packages already present in an unchanged upstream fingerprint.
  export DEBIAN_FRONTEND=noninteractive
  apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update -qq
  apt-get -o Dpkg::Options::=--force-confold -y -qq upgrade
}

base_finalize_image() {
  # The runtime validates a new boot, then scrubs regenerated daemon state before publish.
  getent group kvm >/dev/null || groupadd --system kvm
  usermod -aG kvm dev
  install -d -o dev -g dev /home/dev/.cache /home/dev/.cache/subyard-e2e-platform
  printf '%s\n' subyard-e2e-platform-v1 > /home/dev/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker
  chown dev:dev /home/dev/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker
  install -d -m 0755 /var/lib/subyard
  {
    printf 'environment=%s\n' "$SUBYARD_BASE_ENVIRONMENT"
    sha256sum "$recipe_root/manifest.sha256"
    dpkg-query -W -f='${Package}=${Version}\n' | LC_ALL=C sort
  } > /var/lib/subyard/base-manifest.txt
  sha256sum /var/lib/subyard/base-manifest.txt | cut -d ' ' -f1 > /var/lib/subyard/base-manifest.sha256
  apt-get clean
}

base_main() {
  [ "$(id -u)" -eq 0 ] || { echo 'base recipe requires root' >&2; exit 1; }
  case "${SUBYARD_BASE_ENVIRONMENT:-}" in
    subyard-pair | android-test) ;;
    *) echo 'unknown base environment' >&2; exit 2 ;;
  esac
  base_update_distro
  recipe_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1 ASSUME_YES=1
  export SUBYARD_USER=dev SUBYARD_OPERATOR_HOME=/home/dev
  export SUBYARD_CONFIG_DIR=/etc/subyard SUBYARD_CONFIG_HOME=/etc/subyard
  export SUBYARD_HOME=/home/dev/.local/share/subyard STORAGE_PATH=/var/lib/incus/storage-pools/default
  export HOST_BASE=/srv/subyard RESTRICTED_DISK_PATHS='' ACCESS_KIND=local YARD_KIND=container
  export YARD_INSTANCE_NAME=base INCUS_PROJECT=default INCUS_BRIDGE=incusbr0 SSH_HOST=''
  export DEV_USER=dev DEV_UID=1000 DEV_SUDO=1 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0
  bash "$recipe_root/scripts/01-install-incus.sh" --upgrade-only --yes --zabbly
  incus --version >/dev/null
  base_finalize_image
}

# The runtime executes this file; host-free contracts source the same production main.
[ "${BASH_SOURCE[0]:-$0}" = "$0" ] || return 0
base_main "$@"

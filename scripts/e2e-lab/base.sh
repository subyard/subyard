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

base_prepare_platform() {
  install -d -o dev -g dev /home/dev/.cache /home/dev/.cache/subyard-e2e-platform
  printf '%s\n' subyard-e2e-platform-v1 > /home/dev/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker
  chown dev:dev /home/dev/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker
}

base_finalize_image() {
  # The runtime validates a new boot, then scrubs regenerated daemon state before publish.
  getent group kvm >/dev/null || groupadd --system kvm
  usermod -aG kvm dev
  base_prepare_platform
  # Leased guests must not update themselves: a timer-started unattended upgrade changes the
  # system under test and holds guest shutdown past the broker's release stop. A mask also
  # survives the preset pass of a published image's first boot.
  systemctl disable --now apt-daily.timer apt-daily-upgrade.timer
  systemctl mask apt-daily.timer apt-daily-upgrade.timer
  install -d -m 0755 /var/lib/subyard
  {
    printf 'environment=%s\n' "$SUBYARD_BASE_ENVIRONMENT"
    sha256sum "$recipe_root/manifest.sha256"
    dpkg-query -W -f='${Package}=${Version}\n' | LC_ALL=C sort
  } > /var/lib/subyard/base-manifest.txt
  sha256sum /var/lib/subyard/base-manifest.txt | cut -d ' ' -f1 > /var/lib/subyard/base-manifest.sha256
  apt-get clean
}

base_prepare_nested_host() {
  install -d -m 0755 /etc/systemd/system/incus.service.d /var/lib/subyard/product-image
  printf '%s\n' '[Service]' 'Environment=INCUS_SECURITY_APPARMOR=false' > /etc/systemd/system/incus.service.d/nested-host.conf
  systemctl daemon-reload
  # Retain only the public image bytes. Each new Incus daemon owns its database and keys.
  local variant source fingerprint
  for variant in minimal cloud; do
    source=debian/13
    [ "$variant" != cloud ] || source=debian/13/cloud
    install -d -m 0755 "/var/lib/subyard/product-image/$variant"
    (cd "/var/lib/subyard/product-image/$variant"
     fingerprint="$(incus image list images: "$source" type=container --format json | jq -er --arg arch "$(uname -m)" --arg alias "$source" '
       [.[] | select(.architecture == $arch and any(.aliases[]; .name == $alias)) | .fingerprint] | unique | select(length == 1) | .[0] | select(test("^[a-f0-9]{64}$"))')"
     incus image export "images:$fingerprint" yard
     set -- yard*
     [ "$(cat "$@" | sha256sum | cut -d" " -f1)" = "$fingerprint" ]
     printf '%s\n' "$fingerprint" > fingerprint)
  done
  (cd /var/lib/subyard/product-image && sha256sum */yard* */fingerprint > SHA256SUMS)
}

base_install_incus() (
  local recipe_root="$1"
  export SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1 ASSUME_YES=1
  export SUBYARD_USER=dev SUBYARD_OPERATOR_HOME=/home/dev
  export SUBYARD_CONFIG_DIR=/etc/subyard SUBYARD_CONFIG_HOME=/etc/subyard
  export SUBYARD_HOME=/home/dev/.local/share/subyard STORAGE_PATH=/var/lib/incus/storage-pools/default
  export HOST_BASE=/srv/subyard RESTRICTED_DISK_PATHS='' ACCESS_KIND=local YARD_KIND=container
  export YARD_INSTANCE_NAME=base INCUS_PROJECT=default INCUS_BRIDGE=incusbr0 SSH_HOST=''
  export DEV_USER=dev DEV_UID=1000 DEV_SUDO=yes FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0
  SUBYARD_DISPATCHER_PATH="$recipe_root/bin/yard-engine" MIN_INCUS_VER=6.0.7 \
    bash "$recipe_root/scripts/01-install-incus.sh" --upgrade-only --yes --zabbly
  incus --version >/dev/null
)

base_main() {
  [ "$(id -u)" -eq 0 ] || { echo 'base recipe requires root' >&2; exit 1; }
  case "${SUBYARD_BASE_ENVIRONMENT:-}" in
    subyard-pair | android-test | system-container) ;;
    *) echo 'unknown base environment' >&2; exit 2 ;;
  esac
  base_update_distro
  recipe_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  base_install_incus "$recipe_root"
  if [ "$SUBYARD_BASE_ENVIRONMENT" = system-container ]; then base_prepare_nested_host; fi
  base_finalize_image
}

# The runtime executes this file; host-free contracts source the same production main.
[ "${BASH_SOURCE[0]:-$0}" = "$0" ] || return 0
base_main "$@"

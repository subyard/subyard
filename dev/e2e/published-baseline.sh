#!/usr/bin/env bash
# Exact published predecessor and platform shared by installed-runtime payloads.
# Sourced after dev/e2e/power-reconciler-upgrade.sh, whose helpers it uses.
# shellcheck disable=SC2034 # consumed by the sourcing payload
BASE_IMAGE=subyard-e2e-debian-13-cloud-container
BASELINE_VERSION=0.18.6
BASELINE_INSTALLER_SHA256=a78c10910c0886a8d01c0a2e77718b21ce58e45fa7f0de6064bc1f351d56028b
BASELINE_AMD64_SHA256=458950eccd3831df8d4ed059ccc51a5dece6f8a85aedaae38f863b7252f635f2
BASELINE_ARM64_SHA256=a3b52c39edfc4930f0e0ab4c697d60db6b39a00b022d344053bc4f4131fca4fa

ensure_platform() {
  p0_capacity_prepare_root
  p0_capacity_prepare_platform_root
  if ! incus info >/dev/null 2>&1 || ! incus storage show default --project default >/dev/null 2>&1; then
    (
      # shellcheck source=tests/helpers/test-context.sh
      . "$ROOT/tests/helpers/test-context.sh"
      setup_test_context "$P0_CAPACITY_STATE_ROOT/bootstrap"
      export SUBYARD_USER SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$ROOT/config"
      SUBYARD_USER="$(id -un)"
      export STORAGE_PATH="$P0_CAPACITY_PLATFORM_ROOT/incus/incus/storage"
      set -a
      # shellcheck source=config/host.env
      . "$ROOT/config/host.env"
      set +a
      timeout --signal=TERM --kill-after=10 900 bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
    )
  fi
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
  if ! incus image info "$BASE_IMAGE" --project default >/dev/null 2>&1; then
    timeout --signal=TERM --kill-after=10 900 sudo -n /usr/bin/incus image copy \
      images:debian/13/cloud local: --alias "$BASE_IMAGE" --project default >/dev/null
  fi
}

install_baseline() {
  # Exact published predecessor pins shared with reliable-migrations-recovery.sh.
  local archive pin name baseline="$STATE_ROOT/baseline"
  case "$(uname -m)" in
    x86_64) archive="subyard-$BASELINE_VERSION-linux-amd64.tar.gz"; pin=$BASELINE_AMD64_SHA256 ;;
    aarch64) archive="subyard-$BASELINE_VERSION-linux-arm64.tar.gz"; pin=$BASELINE_ARM64_SHA256 ;;
    *) die 'unsupported predecessor architecture' ;;
  esac
  install -d -m 0755 "$baseline"
  for name in "$archive" "$archive.sha256" "$archive.manifest.json" "$archive.provenance.json" \
    subyard-install-runtime-release.sh subyard-install-runtime-release.sh.sha256 subyard-install.sh; do
    curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
      "https://github.com/Subyard/Subyard/releases/download/v$BASELINE_VERSION/$name" -o "$baseline/$name"
  done
  [ "$(sha256sum "$baseline/$archive" | awk '{print $1}')" = "$pin" ] || die 'predecessor runtime checksum mismatch'
  [ "$(sha256sum "$baseline/subyard-install.sh" | awk '{print $1}')" = "$BASELINE_INSTALLER_SHA256" ] \
    || die 'predecessor installer checksum mismatch'
  chmod -R a+rX "$baseline"
  chmod 0755 "$baseline/subyard-install.sh"
  operator_env env YARD_RELEASE_BASE_URL="file://$baseline" "$baseline/subyard-install.sh" --version "$BASELINE_VERSION" --yes
}

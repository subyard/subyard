#!/usr/bin/env bash
# Reconcile the pinned native ccusage binary for L1.
set -euo pipefail

VERSION="${CCUSAGE_VERSION:-}"
SHA256_AMD64="${CCUSAGE_SHA256_AMD64:-}"
SHA256_ARM64="${CCUSAGE_SHA256_ARM64:-}"
INSTALL_PATH="${CCUSAGE_INSTALL_PATH:-/usr/local/bin/ccusage}"
REGISTRY_URL="${CCUSAGE_REGISTRY_URL:-https://registry.npmjs.org}"
ARCH="${CCUSAGE_ARCH:-}"
RECEIPT="$INSTALL_PATH.subyard-runtime.json"

die() { printf 'ccusage provision: %s\n' "$*" >&2; exit 1; }

if [ "$(id -u)" -ne 0 ] && [ "${CCUSAGE_TEST_ALLOW_NON_ROOT:-0}" != 1 ]; then
  die "must run as root"
fi

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]] \
  || die "CCUSAGE_VERSION must be an exact version (not '${VERSION:-empty}')"
[[ "$SHA256_AMD64" =~ ^[0-9a-f]{64}$ ]] \
  || die "CCUSAGE_SHA256_AMD64 must be a lowercase SHA-256"
[[ "$SHA256_ARM64" =~ ^[0-9a-f]{64}$ ]] \
  || die "CCUSAGE_SHA256_ARM64 must be a lowercase SHA-256"
case "$INSTALL_PATH" in /*) ;; *) die "CCUSAGE_INSTALL_PATH must be absolute" ;; esac
[ -n "$REGISTRY_URL" ] || die "CCUSAGE_REGISTRY_URL is empty"

if [ -z "$ARCH" ]; then
  command -v dpkg >/dev/null 2>&1 || die "dpkg is required to detect the architecture"
  ARCH="$(dpkg --print-architecture)"
fi
case "$ARCH" in
  amd64) PACKAGE_ARCH=x64; EXPECTED_SHA256="$SHA256_AMD64" ;;
  arm64) PACKAGE_ARCH=arm64; EXPECTED_SHA256="$SHA256_ARM64" ;;
  *) die "unsupported architecture '$ARCH' (supported: amd64, arm64)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v jq >/dev/null 2>&1 || die "jq is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
command -v od >/dev/null 2>&1 || die "od is required"

is_native_binary() {
  local magic
  magic="$(LC_ALL=C od -An -tx1 -N4 "$1" 2>/dev/null | tr -d '[:space:]')"
  [ "$magic" = 7f454c46 ]
}

is_expected_binary() {
  local path="$1" output owner
  [ -f "$path" ] && [ ! -L "$path" ] && [ -x "$path" ] || return 1
  [ "$(stat -c '%a' "$path" 2>/dev/null)" = 755 ] || return 1
  if [ "$(id -u)" -eq 0 ]; then owner=0:0; else owner="$(id -u):$(id -g)"; fi
  [ "$(stat -c '%u:%g' "$path" 2>/dev/null)" = "$owner" ] || return 1
  is_native_binary "$path" || return 1
  output="$("$path" --version 2>/dev/null)" || return 1
  [ "$output" = "ccusage $VERSION" ]
}

render_receipt() {
  jq -n --arg version "$VERSION" --arg archive "$EXPECTED_SHA256" \
    --arg binary "$(sha256sum "${1:-$INSTALL_PATH}" | cut -d' ' -f1)" \
    '{schema:1,version:$version,archive:$archive,binary:$binary}'
}
receipt_matches() {
  local owner
  owner="$(id -u):$(id -g)"
  [ -f "$RECEIPT" ] && [ ! -L "$RECEIPT" ] \
    && [ "$(stat -c '%a:%u:%g' "$RECEIPT")" = "600:$owner" ] \
    && cmp -s <(render_receipt) "$RECEIPT"
}

if [ "${1:-}" = --check ]; then
  is_expected_binary "$INSTALL_PATH" && receipt_matches
  exit
fi

if is_expected_binary "$INSTALL_PATH" && receipt_matches; then
  printf 'ccusage %s is already installed at %s\n' "$VERSION" "$INSTALL_PATH"
  exit 0
fi

install_dir="$(dirname "$INSTALL_PATH")"
mkdir -p "$install_dir"
tmp="$(mktemp -d)"
stage=""
cleanup() {
  rm -rf "$tmp"
  [ -z "$stage" ] || rm -f "$stage"
}
trap cleanup EXIT

package="ccusage-linux-${PACKAGE_ARCH}"
url="${REGISTRY_URL%/}/@ccusage/${package}/-/${package}-${VERSION}.tgz"
archive="$tmp/${package}-${VERSION}.tgz"
curl --fail --silent --show-error --location --output "$archive" "$url" \
  || die "download failed: $url"

actual_sha256="$(sha256sum "$archive" | cut -d' ' -f1)"
[ "$actual_sha256" = "$EXPECTED_SHA256" ] \
  || die "checksum mismatch for $package@$VERSION"

package_json="$(tar -xOzf "$archive" package/package.json 2>/dev/null)" \
  || die "package/package.json is missing from the archive"
package_name="$(printf '%s' "$package_json" | jq -er '.name | select(type == "string")' 2>/dev/null)" \
  || die "package name metadata is invalid"
package_version="$(printf '%s' "$package_json" | jq -er '.version | select(type == "string")' 2>/dev/null)" \
  || die "package version metadata is invalid"
[ "$package_name" = "@ccusage/$package" ] \
  || die "unexpected package name '$package_name'"
[ "$package_version" = "$VERSION" ] \
  || die "package version '$package_version' does not match pin '$VERSION'"

mkdir "$tmp/extract"
tar -xzf "$archive" -C "$tmp/extract" package/bin/ccusage 2>/dev/null \
  || die "package/bin/ccusage is missing from the archive"
source_bin="$tmp/extract/package/bin/ccusage"
if [ ! -f "$source_bin" ] || [ -L "$source_bin" ]; then
  die "package/bin/ccusage is not a regular file"
fi
is_native_binary "$source_bin" || die "package/bin/ccusage is not a native Linux binary"

# Validate before the atomic rename so failures preserve the current install.
stage="$(mktemp "$install_dir/.ccusage.XXXXXX")"
install -m 0755 "$source_bin" "$stage"
if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$stage"; fi
is_expected_binary "$stage" \
  || die "staged binary did not report the pinned version '$VERSION'"
mv -fT -- "$stage" "$INSTALL_PATH"
stage=""

is_expected_binary "$INSTALL_PATH" || die "installed binary verification failed"
receipt_temp="$(mktemp "$install_dir/.ccusage-receipt.XXXXXX")"
render_receipt "$source_bin" > "$receipt_temp"
chmod 0600 "$receipt_temp"
if [ "$(id -u)" -eq 0 ]; then chown 0:0 "$receipt_temp"; fi
mv -fT -- "$receipt_temp" "$RECEIPT"
receipt_matches || die "installed binary receipt verification failed"
[ "$(stat -c '%a' "$INSTALL_PATH")" = 755 ] || die "installed binary mode is not 0755"
if [ "$(id -u)" -eq 0 ]; then
  [ "$(stat -c '%u:%g' "$INSTALL_PATH")" = 0:0 ] \
    || die "installed binary is not owned by root:root"
fi
printf 'Installed ccusage %s at %s\n' "$VERSION" "$INSTALL_PATH"

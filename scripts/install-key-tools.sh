#!/usr/bin/env bash
# install-key-tools.sh — install pinned age + SOPS into Subyard's operator-owned tool dir.
# No sudo and no system binary replacement. Usage: install-key-tools.sh [--check] [-y].
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/runtime.sh
. "$SCRIPT_DIR/lib/runtime.sh"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/native-installed-stage.sh
. "$SCRIPT_DIR/lib/native-installed-stage.sh"
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"

TOOLS_DIR="${SUBYARD_KEYS_TOOLS_DIR:-$SUBYARD_HOME/tools}"
BIN_DIR="$TOOLS_DIR/bin"
AGE_BIN="$BIN_DIR/age"
AGE_KEYGEN_BIN="$BIN_DIR/age-keygen"
SOPS_BIN="$BIN_DIR/sops"
RECEIPT="$BIN_DIR/.subyard-native-tools.json"
AGE_VERSION="${SUBYARD_AGE_VERSION:-1.3.1}"
SOPS_VERSION="${SUBYARD_SOPS_VERSION:-3.13.2}"

case "$(uname -m)" in
  x86_64|amd64)
    arch=amd64
    age_sha="${SUBYARD_AGE_SHA256_AMD64:-}"
    sops_sha="${SUBYARD_SOPS_SHA256_AMD64:-}" ;;
  aarch64|arm64)
    arch=arm64
    age_sha="${SUBYARD_AGE_SHA256_ARM64:-}"
    sops_sha="${SUBYARD_SOPS_SHA256_ARM64:-}" ;;
  *) die "age/SOPS installer does not support architecture $(uname -m)" ;;
esac

tool_version_matches() { # <path> <version>
  [ -f "$1" ] && [ ! -L "$1" ] && [ -x "$1" ] || return 1
  [ "$(stat -c '%a:%u' "$1")" = "755:$(id -u)" ] || return 1
  "$1" --version 2>&1 | head -n1 | awk -v expected="$2" '
    { for (i=1; i<=NF; i++) { token=$i; sub(/^v/, "", token); if (token==expected) exit 0 } exit 1 }
  '
}

tools_ready() {
  tool_version_matches "$AGE_BIN" "$AGE_VERSION" \
    && tool_version_matches "$AGE_KEYGEN_BIN" "$AGE_VERSION" \
    && tool_version_matches "$SOPS_BIN" "$SOPS_VERSION" \
    && [ -f "$RECEIPT" ] && [ ! -L "$RECEIPT" ] \
    && [ "$(stat -c '%a:%u' "$RECEIPT")" = "600:$(id -u)" ] \
    && cmp -s <(render_receipt) "$RECEIPT"
}

render_receipt() {
  jq -n --arg age "$AGE_VERSION" --arg sops "$SOPS_VERSION" --arg age_archive "$age_sha" --arg sops_archive "$sops_sha" \
    --arg age_binary "$(sha256sum "${1:-$AGE_BIN}" | cut -d' ' -f1)" \
    --arg keygen_binary "$(sha256sum "${2:-$AGE_KEYGEN_BIN}" | cut -d' ' -f1)" \
    --arg sops_binary "$(sha256sum "${3:-$SOPS_BIN}" | cut -d' ' -f1)" \
    '{schema:1,age:$age,sops:$sops,age_archive:$age_archive,sops_archive:$sops_archive,age_binary:$age_binary,keygen_binary:$keygen_binary,sops_binary:$sops_binary}'
}

case "${1:-}" in
  --check) tools_ready; exit ;;
  -h|--help)
    cat <<EOF
Usage: ${PROG:-yard} internal install-key-tools [--check] [-y]

Install pinned age $AGE_VERSION and SOPS $SOPS_VERSION under $BIN_DIR.
EOF
    exit 0 ;;
esac

tools_ready && { ok "age $AGE_VERSION and SOPS $SOPS_VERSION already installed in $BIN_DIR"; exit 0; }

command -v curl >/dev/null 2>&1 || die "curl is required to install age and SOPS"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required to verify age and SOPS"
command -v tar >/dev/null 2>&1 || die "tar is required to install age"

[ "${#age_sha}" = 64 ] && [ "${#sops_sha}" = 64 ] \
  || die "age/SOPS checksum configuration is incomplete for $arch"

announce "Install encrypted-key tools" \
  "Download age v$AGE_VERSION and SOPS v$SOPS_VERSION from their official GitHub releases." \
  "Verify both artifacts against pinned SHA-256 checksums." \
  "Install operator-owned binaries under $BIN_DIR; system binaries are not changed."
proceed_or_die

tmp="$(mktemp -d)"
trap 'result=$?; rm -rf "$tmp"; subyard_prepared_installed_finish "$result"' EXIT
age_archive="age-v$AGE_VERSION-linux-$arch.tar.gz"
age_url="https://github.com/FiloSottile/age/releases/download/v$AGE_VERSION/$age_archive"
sops_asset="sops-v$SOPS_VERSION.linux.$arch"
sops_url="https://github.com/getsops/sops/releases/download/v$SOPS_VERSION/$sops_asset"

curl --fail --silent --show-error --location "$age_url" -o "$tmp/$age_archive" \
  || die "could not download age v$AGE_VERSION"
[ "$(sha256sum "$tmp/$age_archive" | cut -d' ' -f1)" = "$age_sha" ] \
  || die "checksum mismatch for age v$AGE_VERSION ($arch)"
tar -xzf "$tmp/$age_archive" -C "$tmp"
[ -x "$tmp/age/age" ] && [ -x "$tmp/age/age-keygen" ] \
  || die "age archive did not contain the expected binaries"

curl --fail --silent --show-error --location "$sops_url" -o "$tmp/sops" \
  || die "could not download SOPS v$SOPS_VERSION"
[ "$(sha256sum "$tmp/sops" | cut -d' ' -f1)" = "$sops_sha" ] \
  || die "checksum mismatch for SOPS v$SOPS_VERSION ($arch)"
chmod 0755 "$tmp/sops"

install -d -m 700 "$TOOLS_DIR" "$BIN_DIR"
install -m 0755 "$tmp/age/age" "$BIN_DIR/age.new"
install -m 0755 "$tmp/age/age-keygen" "$BIN_DIR/age-keygen.new"
install -m 0755 "$tmp/sops" "$BIN_DIR/sops.new"
mv -f "$BIN_DIR/age.new" "$AGE_BIN"
mv -f "$BIN_DIR/age-keygen.new" "$AGE_KEYGEN_BIN"
mv -f "$BIN_DIR/sops.new" "$SOPS_BIN"

receipt_temp="$(mktemp "$BIN_DIR/.tools-receipt.XXXXXX")"
render_receipt "$tmp/age/age" "$tmp/age/age-keygen" "$tmp/sops" > "$receipt_temp"
chmod 0600 "$receipt_temp"
mv -f -- "$receipt_temp" "$RECEIPT"
tools_ready || die "installed age/SOPS binaries failed identity verification"
ok "installed age $AGE_VERSION and SOPS $SOPS_VERSION in $BIN_DIR"

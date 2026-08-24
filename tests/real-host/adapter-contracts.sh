#!/usr/bin/env bash
# Shared CI/release gate for real pinned crypto and loopback OpenSSH adapters.
set -euo pipefail

selected_check=all
if [ "$#" -ne 0 ]; then
  if [ "$#" -ne 2 ] || [ "$1" != --check ]; then
    printf 'adapter-contracts: usage: %s [--check credential-tools|ssh-rpc|ssh-credential-peer]\n' "$0" >&2
    exit 2
  fi
  case "$2" in
    credential-tools|ssh-rpc|ssh-credential-peer) selected_check=$2 ;;
    *) printf 'adapter-contracts: unknown check: %s\n' "$2" >&2; exit 2 ;;
  esac
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ADAPTER_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/subyard-real-adapters.XXXXXX")"
cleanup() {
  local status=$?
  trap - EXIT
  # Go's module cache is intentionally read-only, even inside this disposable test root.
  chmod -R u+w -- "$ADAPTER_ROOT" 2>/dev/null || true
  rm -rf -- "$ADAPTER_ROOT" || { [ "$status" -ne 0 ] || status=1; }
  exit "$status"
}
trap cleanup EXIT

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$ADAPTER_ROOT"
export HOME="$SUBYARD_OPERATOR_HOME"
export ASSUME_YES=1
export TMPDIR="$ADAPTER_ROOT/tmp"

mkdir -p \
  "$SUBYARD_OPERATOR_HOME" \
  "$SUBYARD_CONFIG_DIR" \
  "$SUBYARD_CONFIG_HOME" \
  "$SUBYARD_HOME" \
  "$TMPDIR"

bash "$ROOT/dev/build-engine.sh" >/dev/null

prepare_key_tools() {
  # Keep the real artifact versions and checksums on the same source of truth as production.
  unset \
    SUBYARD_AGE_VERSION \
    SUBYARD_AGE_SHA256_AMD64 \
    SUBYARD_AGE_SHA256_ARM64 \
    SUBYARD_SOPS_VERSION \
    SUBYARD_SOPS_SHA256_AMD64 \
    SUBYARD_SOPS_SHA256_ARM64
  # shellcheck source=config/host.env
  . "$ROOT/config/host.env"
  export \
    SUBYARD_AGE_VERSION \
    SUBYARD_AGE_SHA256_AMD64 \
    SUBYARD_AGE_SHA256_ARM64 \
    SUBYARD_SOPS_VERSION \
    SUBYARD_SOPS_SHA256_AMD64 \
    SUBYARD_SOPS_SHA256_ARM64

  export SUBYARD_KEYS_TOOLS_DIR="$ADAPTER_ROOT/tools"
  export SUBYARD_REAL_KEYS_TOOLS_DIR="$SUBYARD_KEYS_TOOLS_DIR"
  bash "$ROOT/scripts/install-key-tools.sh" --yes
}

case "$selected_check" in
  all)
    prepare_key_tools
    bash "$ROOT/tests/real-host/credential-tools.sh"
    bash "$ROOT/tests/real-host/ssh-rpc.sh"
    bash "$ROOT/tests/real-host/ssh-credential-peer.sh"
    ;;
  credential-tools)
    prepare_key_tools
    bash "$ROOT/tests/real-host/credential-tools.sh"
    ;;
  ssh-rpc)
    bash "$ROOT/tests/real-host/ssh-rpc.sh"
    ;;
  ssh-credential-peer)
    prepare_key_tools
    bash "$ROOT/tests/real-host/ssh-credential-peer.sh"
    ;;
esac

#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/home" "$tmp/config-home" "$tmp/storage" "$tmp/host"

cat > "$tmp/bin/incus" <<'INCUS'
#!/usr/bin/env bash
tar -tf - >/dev/null
exit "${PROVISION_CHECK_STATUS:?}"
INCUS
chmod +x "$tmp/bin/incus"

engine_env=(
  PATH="$tmp/bin:$PATH"
  SUBYARD_ENGINE_CONTEXT=1
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1
  SUBYARD_OPERATOR_HOME="$tmp/home"
  SUBYARD_CONFIG_DIR="$ROOT/config"
  SUBYARD_CONFIG_HOME="$tmp/config-home"
  SUBYARD_HOME="$ROOT"
  STORAGE_PATH="$tmp/storage"
  HOST_BASE="$tmp/host"
  RESTRICTED_DISK_PATHS=""
  ACCESS_KIND=local
  YARD_KIND=container
  YARD_INSTANCE_NAME=yard-check
  INCUS_PROJECT=subyard-check
  INCUS_BRIDGE=incusbr0
  SSH_HOST=yard-check
  DEV_USER=dev
  DEV_UID=1000
  DEV_SUDO=1
  FORWARD_SSH_AGENT=0
  NESTED_E2E_VMS=0
)

output="$(env "${engine_env[@]}" PROVISION_CHECK_STATUS=0 \
  bash "$ROOT/scripts/provision-profile.sh" --check subyard-dev)"
[ "$output" = converged ] || { printf 'FAIL: converged output=%q\n' "$output" >&2; exit 1; }

output="$(env "${engine_env[@]}" PROVISION_CHECK_STATUS=10 \
  bash "$ROOT/scripts/provision-profile.sh" --check subyard-dev)"
[ "$output" = changed ] || { printf 'FAIL: changed output=%q\n' "$output" >&2; exit 1; }

if env "${engine_env[@]}" PROVISION_CHECK_STATUS=7 \
  bash "$ROOT/scripts/provision-profile.sh" --check subyard-dev >/dev/null 2>&1; then
  printf 'FAIL: malformed hook check status was accepted\n' >&2
  exit 1
fi

printf 'ok: provision profile check protocol\n'

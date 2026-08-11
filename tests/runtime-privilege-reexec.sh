#!/usr/bin/env bash
# Dispatcher identity is single-use, and sudo re-entry preserves operator-owned roots.
# shellcheck disable=SC2034 # fixture context variables are consumed indirectly by sourced host.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export SUBYARD_DISPATCH_PATH="$TMP/yard"
export SUBYARD_DISPATCH_COMMAND=init
export SUBYARD_DISPATCH_ARG0=init
set -- init --yes
# shellcheck source=scripts/lib/runtime.sh
. "$ROOT/scripts/lib/runtime.sh"

[ "$SUBYARD_SCRIPT_PATH" = "$TMP/yard" ] || fail 'top-level dispatcher path was not captured'
[ "${SUBYARD_SCRIPT_ARGV[*]}" = 'init --yes' ] || fail 'top-level dispatcher argv was not captured'
[ -z "${SUBYARD_DISPATCH_PATH+x}${SUBYARD_DISPATCH_COMMAND+x}${SUBYARD_DISPATCH_ARG0+x}" ] \
  || fail 'dispatcher identity remained inherited after capture'

nested_identity="$(RUNTIME="$ROOT/scripts/lib/runtime.sh" /bin/bash -c '
  set -euo pipefail
  . "$RUNTIME"
  printf "%s|%s\n" "$SUBYARD_SCRIPT_PATH" "${SUBYARD_SCRIPT_ARGV[*]}"
' "$TMP/phase.sh" --yes)"
[ "$nested_identity" = "$TMP/phase.sh|--yes" ] \
  || fail 'child phase inherited the top-level dispatcher identity'

install -d "$TMP/bin"
cat > "$TMP/bin/id" <<'SH'
#!/usr/bin/env bash
case "${1:-}" in
  -u) printf '%s\n' "${MOCK_UID:-1000}" ;;
  -un) printf 'operator\n' ;;
  *) exec /usr/bin/id "$@" ;;
esac
SH
cat > "$TMP/bin/sudo" <<'SH'
#!/usr/bin/env bash
if [ "${MOCK_SUDO_EXPIRED:-0}" = 1 ] && [ "${1:-}" = -n ] && [ "${2:-}" = true ]; then
  exit 1
fi
printf '%s\n' "$@" > "$MOCK_SUDO_LOG"
SH
chmod +x "$TMP/bin/id" "$TMP/bin/sudo"

export MOCK_SUDO_LOG="$TMP/sudo.argv"
ROOT_HOME="$(getent passwd root | cut -d: -f6)"
(
  PATH="$TMP/bin:$PATH"
  SUBYARD_USER=operator
  SUBYARD_OPERATOR_HOME="$TMP/operator home"
  SUBYARD_CONFIG_DIR="$TMP/repository/config"
  SUBYARD_CONFIG_HOME="$TMP/operator home/.config/subyard"
  SUBYARD_HOME="$TMP/operator home/.subyard"
  YARD_RUNTIME_ROOT="$TMP/operator home/.subyard/runtime"
  STORAGE_PATH="$TMP/operator home/.subyard/incus/storage"
  HOST_BASE="$TMP/host data"
  RESTRICTED_DISK_PATHS="$TMP/host data"
  SUBYARD_YARD=e2e-yard
  SUBYARD_YARD_EXPLICIT=1
  SUBYARD_SUDO_PREAUTHORIZED=1
  SUBYARD_POWER_ENGINE_SOURCE="$TMP/runtime/yard-engine"
  SUBYARD_ENGINE_CONTEXT=1
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1
  ACCESS_KIND=local
  YARD_KIND=container
  YARD_INSTANCE_NAME=yard-e2e
  INCUS_PROJECT=subyard-e2e
  INCUS_BRIDGE=incusbr0
  SSH_HOST=yard-e2e
  DEV_USER=dev
  DEV_UID=1000
  DEV_SUDO=0
  FORWARD_SSH_AGENT=0
  NESTED_E2E_VMS=1
  E2E_VM_IMAGE=images:debian/13/cloud
  AGENT_CODEX_COMMAND=codex
  AWS_SECRET_ACCESS_KEY=do-not-copy
  SUBYARD_SCRIPT_PATH="$TMP/phase.sh"
  SUBYARD_SCRIPT_ARGV=(--yes)
  warn() { :; }
  info() { :; }
  # shellcheck source=scripts/lib/engine-context.sh
  . "$ROOT/scripts/lib/engine-context.sh"
  subyard_require_engine_context
  # shellcheck source=scripts/lib/host.sh
  . "$ROOT/scripts/lib/host.sh"
  require_root fixture
)

for expected in \
  "HOME=$ROOT_HOME" \
  SUBYARD_ELEVATED=1 \
  SUBYARD_ENGINE_CONTEXT=1 \
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1 \
  SUBYARD_USER=operator \
  "SUBYARD_OPERATOR_HOME=$TMP/operator home" \
  "SUBYARD_CONFIG_DIR=$TMP/repository/config" \
  "SUBYARD_CONFIG_HOME=$TMP/operator home/.config/subyard" \
  "SUBYARD_HOME=$TMP/operator home/.subyard" \
  "YARD_RUNTIME_ROOT=$TMP/operator home/.subyard/runtime" \
  "STORAGE_PATH=$TMP/operator home/.subyard/incus/storage" \
  "HOST_BASE=$TMP/host data" \
  "RESTRICTED_DISK_PATHS=$TMP/host data" \
  SUBYARD_YARD=e2e-yard \
  SUBYARD_YARD_EXPLICIT=1 \
  E2E_VM_IMAGE=images:debian/13/cloud \
  AGENT_CODEX_COMMAND=codex \
  "SUBYARD_POWER_ENGINE_SOURCE=$TMP/runtime/yard-engine" \
  "$TMP/phase.sh" \
  --yes; do
  grep -Fxq -- "$expected" "$MOCK_SUDO_LOG" \
    || fail "sudo re-entry omitted argument: $expected"
done
grep -Fxq -- env "$MOCK_SUDO_LOG" || fail 'sudo re-entry did not use an explicit environment'
grep -Fxq -- -n "$MOCK_SUDO_LOG" \
  || fail 'preauthorized sudo re-entry attempted an interactive password prompt'
if grep -Fq 'AWS_SECRET_ACCESS_KEY' "$MOCK_SUDO_LOG"; then
  fail 'sudo re-entry copied a non-allowlisted variable'
fi
if grep -Fxq -- "HOME=$TMP/operator home" "$MOCK_SUDO_LOG"; then
  fail 'sudo re-entry preserved the operator HOME for root-owned tools'
fi

printf 'ok: child phases own re-exec identity and preauthorized sudo preserves operator roots\n'

DIRECT_SUDO_LOG="$TMP/direct-sudo.argv"
(
  PATH="$TMP/bin:$PATH"
  MOCK_SUDO_LOG="$DIRECT_SUDO_LOG"
  SUBYARD_SCRIPT_PATH="$TMP/direct.sh"
  SUBYARD_SCRIPT_ARGV=(--yes)
  warn() { :; }
  info() { :; }
  die() { printf '%s\n' "$*" >&2; exit 1; }
  # shellcheck source=scripts/lib/engine-context.sh
  . "$ROOT/scripts/lib/engine-context.sh"
  # shellcheck source=scripts/lib/host.sh
  . "$ROOT/scripts/lib/host.sh"
  require_root fixture
)
if grep -Fxq -- -n "$DIRECT_SUDO_LOG"; then
  fail 'direct foreground sudo re-entry was forced non-interactive'
fi

EXPIRED_SUDO_LOG="$TMP/expired-sudo.argv"
if printf 'payload-must-not-be-read\n' | (
  PATH="$TMP/bin:$PATH"
  MOCK_SUDO_LOG="$EXPIRED_SUDO_LOG"
  export MOCK_SUDO_EXPIRED=1
  SUBYARD_SUDO_PREAUTHORIZED=1
  SUBYARD_SCRIPT_PATH="$TMP/expired.sh"
  SUBYARD_SCRIPT_ARGV=(--yes)
  warn() { :; }
  info() { :; }
  die() { printf '%s\n' "$*" >&2; exit 1; }
  # shellcheck source=scripts/lib/engine-context.sh
  . "$ROOT/scripts/lib/engine-context.sh"
  # shellcheck source=scripts/lib/host.sh
  . "$ROOT/scripts/lib/host.sh"
  require_root fixture
) >"$TMP/expired.out" 2>&1; then
  fail 'expired preauthorized sudo credential was accepted'
fi
grep -Fq 'sudo authorization expired' "$TMP/expired.out" \
  || fail 'expired preauthorized sudo error was not actionable'
[ ! -e "$EXPIRED_SUDO_LOG" ] \
  || fail 'expired preauthorized path reached the elevated command'

ROOT_SUDO_LOG="$TMP/root-sudo.argv"
(
  PATH="$TMP/bin:$PATH"
  export MOCK_UID=0
  MOCK_SUDO_LOG="$ROOT_SUDO_LOG"
  SUBYARD_SCRIPT_PATH="$TMP/root.sh"
  SUBYARD_SCRIPT_ARGV=()
  # shellcheck source=scripts/lib/engine-context.sh
  . "$ROOT/scripts/lib/engine-context.sh"
  # shellcheck source=scripts/lib/host.sh
  . "$ROOT/scripts/lib/host.sh"
  require_root fixture
)
[ ! -e "$ROOT_SUDO_LOG" ] || fail 'EUID 0 invoked sudo'

printf 'ok: sudo leaves keep direct prompts, fail closed when preauthorization expires, and skip sudo as root\n'

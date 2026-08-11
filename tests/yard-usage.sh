#!/usr/bin/env bash
# Process smoke for `yard usage` exit status, repair hints, and remote forwarding.
# Exact argv and dev-identity construction belong to internal/cli Go tests.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/public-config" "$tmp/config-home/yards" "$tmp/home"

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$tmp" test-project test-yard
export SUBYARD_CONFIG_DIR="$tmp/public-config"
export SUBYARD_CONFIG_HOME="$tmp/config-home"
export SUBYARD_HOME="$tmp/subyard-home"
export SUBYARD_NO_AUDIT=1
export MOCK_CCUSAGE_BIN="$tmp/mock-ccusage"
export FALLBACK_LOG="$tmp/fallback.log"

cat > "$tmp/bin/incus" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  info) exit 0 ;;
  list) printf 'RUNNING\n' ;;
  exec)
    shift
    while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
    [ "${1:-}" = -- ] && shift
    case "${1:-}" in
      sh)
        [ -f "$MOCK_CCUSAGE_BIN" ] && [ ! -L "$MOCK_CCUSAGE_BIN" ] && [ -x "$MOCK_CCUSAGE_BIN" ]
        ;;
      /usr/local/bin/ccusage)
        shift
        exec "$MOCK_CCUSAGE_BIN" "$@"
        ;;
      *) exit 90 ;;
    esac
    ;;
  *) exit 90 ;;
esac
SH
cat > "$MOCK_CCUSAGE_BIN" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
exit "${MOCK_CCUSAGE_EXIT:-0}"
SH
for command in npx bunx; do
  cat > "$tmp/bin/$command" <<'SH'
#!/usr/bin/env bash
printf 'called\n' >> "$FALLBACK_LOG"
exit 99
SH
done
chmod +x "$tmp/bin/incus" "$MOCK_CCUSAGE_BIN" "$tmp/bin/npx" "$tmp/bin/bunx"
export PATH="$tmp/bin:$PATH"

# shellcheck disable=SC2016 # literal injection-shaped argument must survive unchanged
args=(daily --json 'space arg' '' '$(touch should-not-run)')
export MOCK_CCUSAGE_EXIT=23
set +e
"$ROOT/bin/yard" usage "${args[@]}" >"$tmp/stdout" 2>"$tmp/stderr"
rc=$?
set -e
[ "$rc" -eq 23 ] || fail "ccusage exit status was not preserved (got $rc)"

# Preserve named context in the repair hint.
cat > "$SUBYARD_CONFIG_HOME/yards/usage-test.env" <<'ENV'
ACCESS_KIND=local
SSH_PORT=2299
ENV
export MOCK_CCUSAGE_BIN="$tmp/missing-ccusage"
export MOCK_CCUSAGE_EXIT=0
set +e
"$ROOT/bin/yard" -Y usage-test usage daily >"$tmp/missing.out" 2>"$tmp/missing.err"
missing_rc=$?
set -e
[ "$missing_rc" -eq 1 ] || fail "missing ccusage returned $missing_rc instead of 1"
grep -Fq 'repair with: yard -Y usage-test init' "$tmp/missing.err" \
  || fail "named-yard repair hint lost its context"
[ ! -e "$FALLBACK_LOG" ] || fail "missing ccusage invoked npx or bunx"
[ ! -e "$ROOT/scripts/yard-usage.sh" ] || fail "retired yard-usage.sh returned"

SUBYARD_USAGE_REPAIR_HINT='yard -Y controller init' \
  "$ROOT/bin/yard" usage >"$tmp/controller.out" 2>"$tmp/controller.err" || controller_rc=$?
[ "${controller_rc:-0}" -eq 1 ] || fail "controller repair case returned ${controller_rc:-0}"
grep -Fq 'repair with: yard -Y controller init' "$tmp/controller.err" \
  || fail "forwarded repair hint was ignored"

# Preserve the controller alias across remote forwarding.
cat > "$SUBYARD_CONFIG_HOME/yards/usage-remote.env" <<'ENV'
ACCESS_KIND=remote
OWNER_ENDPOINT=owner.test
OWNER_YARD_NAME=inner
ENV
export SSH_LOG="$tmp/ssh.log"
cat > "$tmp/bin/ssh" <<'SH'
#!/usr/bin/env bash
printf '%s\0' "$@" > "$SSH_LOG"
SH
chmod +x "$tmp/bin/ssh"
env -u ACCESS_KIND "$ROOT/bin/yard" -Y usage-remote usage >/dev/null
mapfile -d '' -t ssh_args < "$SSH_LOG"
payload="${ssh_args[-1]}"
decoded="$(/bin/bash -c "printf '%s' $payload")"
mkdir -p "$tmp/remote-bin"
export REMOTE_YARD_LOG="$tmp/remote-yard.log"
cat > "$tmp/remote-bin/yard" <<'SH'
#!/usr/bin/env bash
printf '%s\0' "${SUBYARD_USAGE_REPAIR_HINT:-}" "$@" > "$REMOTE_YARD_LOG"
SH
chmod +x "$tmp/remote-bin/yard"
PATH="$tmp/remote-bin:$PATH" /bin/bash -c "$decoded"
mapfile -d '' -t remote_call < "$REMOTE_YARD_LOG"
[ "${remote_call[0]:-}" = 'yard -Y usage-remote init' ] || fail "remote repair hint lost controller alias"
if [ "${remote_call[1]:-}" != -Y ] \
  || [ "${remote_call[2]:-}" != inner ] \
  || [ "${remote_call[3]:-}" != usage ]; then
  fail "remote target command changed"
fi

printf 'ok: yard usage dispatch\n'

#!/usr/bin/env bash
# Regression coverage for remote project inventory and target-aware removal.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_contains() { grep -Fq -- "$2" <<<"$1" || fail "output does not contain: $2"; }
assert_not_contains() { ! grep -Fq -- "$2" <<<"$1" || fail "output unexpectedly contains: $2"; }
assert_projects() {
  local output="$1" expected="$2" actual
  actual="$(awk '$1 == "owner-a/default" { print $6; exit }' <<<"$output")"
  [ "$actual" = "$expected" ] || fail "remote PROJECTS is '$actual', expected '$expected'"
}

mkdir -p "$TMP/bin" "$TMP/config/yards" "$TMP/config/yards/remote/projects" \
  "$TMP/shipped" "$TMP/subyard" "$TMP/state"
for f in agents.env host.env ports.env; do : > "$TMP/shipped/$f"; done
printf ': "${INSTANCE_NAME:=yard}"\n: "${INCUS_PROJECT:=subyard}"\n' > "$TMP/shipped/incus.project.env"
printf ': "${SSH_PORT:=2222}"\n' > "$TMP/shipped/subyard.env"

cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  info) exit 0 ;;
  list) printf 'RUNNING\n' ;;
  exec)
    case "${YARD_META_MODE:-empty}" in
      one)
        printf '%s\n' \
          '{"schema":1,"projectId":"demo-12345678","name":"demo","mode":"sync"}' \
          '{"schema":1,"projectId":"demo-12345678","name":"duplicate","mode":"sync"}'
        ;;
      empty) exit 0 ;;
      fail) exit 1 ;;
    esac
    ;;
  *) exit 0 ;;
esac
MOCK
chmod 755 "$TMP/bin/incus"

cat > "$TMP/bin/ssh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
joined="$*"
if [ "${1:-}" = -G ]; then
  printf 'hostname 127.0.0.1\nhostkeyalias subyard-remote-remote\n'
  exit 0
fi
if [[ "$joined" == *yard* && "$joined" == *rpc* && "$joined" == *--stdio* ]]; then
  exec env \
    SUBYARD_OPERATOR_HOME="$REMOTE_TEST_STATE/owner-home" \
    SUBYARD_CONFIG_HOME="$REMOTE_TEST_STATE/owner-config" \
    SUBYARD_HOME="$REMOTE_TEST_STATE/owner-data" \
    SUBYARD_CONFIG_DIR="$REMOTE_TEST_SHIPPED" \
    SUBYARD_NO_AUDIT=1 \
    "$REMOTE_TEST_ROOT/bin/yard" rpc --stdio
fi
if [[ "$joined" == *_info* ]]; then
  case "$(cat "$REMOTE_TEST_STATE/info-mode" 2>/dev/null || printf fail)" in
    one)  printf '%s\n' '{"state":"RUNNING","projects":1}' ;;
    null) printf '%s\n' '{"state":"RUNNING","projects":null}' ;;
    fail) exit 255 ;;
  esac
  exit 0
fi
if [[ "$joined" == *'_project-state'* ]]; then
  printf '%s\n' "$joined" >> "$REMOTE_TEST_STATE/owner-calls"
  exit 0
fi
if [[ "$joined" == *'yard-remote'*"'docker' 'info'"* ]]; then
  [ "$(cat "$REMOTE_TEST_STATE/cleanup-mode" 2>/dev/null || printf ok)" != fail ] || exit 1
  exit 0
fi
if [[ "$joined" == *'yard-remote'*'docker inspect "$1"'* ]]; then
  [[ "$joined" != *'printf present'* ]] || printf 'present'
  exit 0
fi
if [[ "$joined" == *'yard-remote'*"'docker' 'rm'"* || "$joined" == *'/srv/env-secrets/'* ]]; then
  : > "$REMOTE_TEST_STATE/data-cleanup"
  exit 0
fi
if [[ "$joined" == *'yard-remote'*'/srv/workspaces/demo-12345678'* ]]; then
  if [[ "$joined" == *'printf present'* ]]; then
    printf 'present'
    exit 0
  fi
  : > "$REMOTE_TEST_STATE/workspace-delete"
  exit 0
fi
exit 0
MOCK
chmod 755 "$TMP/bin/ssh"

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
# Exercise resolver-owned registry paths and named-yard identity.
unset SUBYARD_STATE_DIR YARD_TYPE INSTANCE_NAME INCUS_PROJECT SSH_HOST
chmod 0700 "$TMP/config/yards/remote/projects"
export PATH="$TMP/bin:$PATH"
export HOME="$TMP/home"
export SUBYARD_CONFIG_DIR="$TMP/shipped"
export SUBYARD_NO_AUDIT=1
export REMOTE_TEST_STATE="$TMP/state"
export REMOTE_TEST_ROOT="$ROOT"
export REMOTE_TEST_SHIPPED="$TMP/shipped"

install -d -m 0700 "$REMOTE_TEST_STATE/owner-home" "$REMOTE_TEST_STATE/owner-config/projects" \
  "$REMOTE_TEST_STATE/owner-data"
printf 'owner-a\n' > "$REMOTE_TEST_STATE/owner-config/host-id"
chmod 0600 "$REMOTE_TEST_STATE/owner-config/host-id"
jq -n '{
  schema:1, projectId:"owner-demo-12345678", name:"owner-demo", hostPath:"/owner/demo",
  yardPath:"/srv/workspaces/owner-demo-12345678/src", mode:"sync", sshHost:"yard",
  importedAt:"test", target:"yard"
}' > "$REMOTE_TEST_STATE/owner-config/projects/owner-demo-12345678.json"
chmod 0600 "$REMOTE_TEST_STATE/owner-config/projects/owner-demo-12345678.json"

cat > "$SUBYARD_CONFIG_HOME/yards/remote.env" <<'ENV'
YARD_TYPE=remote
REMOTE_DEST=owner
REMOTE_YARD=
SSH_PORT=2222
ENV

# Remote overview uses the HostID-keyed owner snapshot and a fresh cache avoids another SSH call.
printf 'one\n' > "$REMOTE_TEST_STATE/info-mode"
output="$($ROOT/bin/yard yards)"
assert_projects "$output" 1
printf 'null\n' > "$REMOTE_TEST_STATE/info-mode"
output="$($ROOT/bin/yard yards)"
assert_projects "$output" 1
printf 'fail\n' > "$REMOTE_TEST_STATE/info-mode"
output="$($ROOT/bin/yard yards)"
assert_projects "$output" 1

state_file="$SUBYARD_CONFIG_HOME/yards/remote/projects/demo-12345678.json"
write_state() {
  local target="$1"
  jq -n --arg target "$target" '{
    schema:1, projectId:"demo-12345678", name:"demo", hostPath:"/controller/demo",
    yardPath:"/srv/workspaces/demo-12345678/src", mode:"sync", sshHost:"yard-remote",
    importedAt:"test", target:$target
  }' > "$state_file"
  chmod 0600 "$state_file"
  jq -n --arg target "$target" '{
    schema:1, projectId:"demo-12345678", name:"demo", hostPath:"",
    yardPath:"/srv/workspaces/demo-12345678/src", mode:"sync", sshHost:"yard",
    importedAt:"test", target:$target, registrySource:"yard"
  }' > "$REMOTE_TEST_STATE/owner-config/projects/demo-12345678.json"
  chmod 0600 "$REMOTE_TEST_STATE/owner-config/projects/demo-12345678.json"
  "$ROOT/bin/yard" list --live >/dev/null
}
run_remove() {
  "$ROOT/bin/yard" -Y owner-a/default remove demo-12345678 "$@" --yes
}

# L1 removal has no L2 promise, warning, or owner-host cleanup call.
write_state yard
rm -f "$REMOTE_TEST_STATE/data-cleanup" "$REMOTE_TEST_STATE/workspace-delete"
output="$(run_remove --soft)"
assert_not_contains "$output" 'L2'
assert_not_contains "$output" 'box teardown'
[ ! -e "$REMOTE_TEST_STATE/data-cleanup" ] || fail 'L1 removal called L2 cleanup'
[ ! -e "$state_file" ] || fail 'native soft removal kept controller state'

# An unreachable in-yard L2 environment fails during read-only removal preflight, before either
# controller state or workspace deletion can change.
write_state openclaw
printf 'fail\n' > "$REMOTE_TEST_STATE/cleanup-mode"
rm -f "$REMOTE_TEST_STATE/data-cleanup" "$REMOTE_TEST_STATE/workspace-delete" "$REMOTE_TEST_STATE/owner-calls"
if output="$(run_remove 2>&1)"; then fail 'remote L2 removal ignored failed environment preflight'; fi
assert_contains "$output" 'prepare remove action: reach project environment before removal'
[ -e "$state_file" ] || fail 'failed L2 preflight removed controller state'
[ ! -e "$REMOTE_TEST_STATE/workspace-delete" ] || fail 'failed L2 preflight deleted the workspace'
[ ! -e "$REMOTE_TEST_STATE/owner-calls" ] || fail 'failed L2 preflight changed owner state'

# Once in-yard cleanup succeeds, native removal commits state after the workspace is gone.
printf 'ok\n' > "$REMOTE_TEST_STATE/cleanup-mode"
rm -f "$REMOTE_TEST_STATE/owner-calls"
output="$(run_remove)"
assert_contains "$output" 'removed demo'
[ -e "$REMOTE_TEST_STATE/data-cleanup" ] || fail 'successful L2 removal skipped in-yard cleanup'
[ -e "$REMOTE_TEST_STATE/workspace-delete" ] || fail 'successful L2 removal skipped workspace deletion'
[ ! -e "$state_file" ] || fail 'native L2 removal kept controller state'
[ -s "$REMOTE_TEST_STATE/owner-calls" ] || fail 'native removal did not converge owner state'

printf 'ok: remote project counts are cached and native removal is target-aware\n'

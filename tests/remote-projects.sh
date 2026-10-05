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
printf ': "${YARD_INSTANCE_NAME:=yard}"\n: "${INCUS_PROJECT:=subyard}"\n' > "$TMP/shipped/incus.project.env"
printf ': "${SSH_PORT:=2222}"\n' > "$TMP/shipped/subyard.env"
printf '%s\n' 'subyard-remote-remote ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fixture' \
  > "$TMP/state/known_hosts"

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
  printf 'hostname 127.0.0.1\nport 22\nhostkeyalias subyard-remote-remote\nuserknownhostsfile %s\n' \
    "$REMOTE_TEST_STATE/known_hosts"
  exit 0
fi
if [[ "$joined" == *yard* && "$joined" == *rpc* && "$joined" == *--stdio* ]]; then
  exec env -u SUBYARD_STATE_DIR -u SUBYARD_CONFIG_LOADED -u SUBYARD_ENGINE_CONTEXT \
    -u OWNER_ENDPOINT -u OWNER_YARD_NAME -u SUBYARD_YARD -u YARD_NAME \
    SUBYARD_OPERATOR_HOME="$REMOTE_TEST_STATE/owner-home" \
    SUBYARD_CONFIG_HOME="$REMOTE_TEST_STATE/owner-config" \
    SUBYARD_HOME="$REMOTE_TEST_STATE/owner-data" \
    SUBYARD_CONFIG_DIR="$REMOTE_TEST_SHIPPED" \
    SUBYARD_NO_AUDIT=1 \
    ACCESS_KIND=local SSH_HOST=yard YARD_INSTANCE_NAME=yard \
    "$REMOTE_TEST_STATE/../bin/project-owner-fixture"
fi
if [[ "$joined" == *_info* ]]; then
  case "$(cat "$REMOTE_TEST_STATE/info-mode" 2>/dev/null || printf fail)" in
    one)  printf '%s\n' '{"state":"RUNNING","projects":1}' ;;
    null) printf '%s\n' '{"state":"RUNNING","projects":null}' ;;
    fail) exit 255 ;;
  esac
  exit 0
fi
if [[ "$joined" == *'_project-state'* ]];then
  printf 'unexpected legacy owner project mutation\n' >&2
  exit 1
fi
if [[ "$joined" == *yard-remote* ]];then
  printf 'unexpected controller-owned project effect\n' >&2
  exit 1
fi
exit 0
MOCK
chmod 755 "$TMP/bin/ssh"

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
setup_test_repository "$TMP" "$ROOT"
# Exercise resolver-owned registry paths and named-yard identity.
unset SUBYARD_STATE_DIR ACCESS_KIND YARD_INSTANCE_NAME INCUS_PROJECT SSH_HOST
chmod 0700 "$TMP/config/yards/remote/projects"
export PATH="$TMP/bin:$PATH"
go build -o "$TMP/bin/project-owner-fixture" "$ROOT/tests/helpers/project-owner-fixture"
export HOME="$TMP/home"
export SUBYARD_CONFIG_DIR="$TMP/shipped"
export SUBYARD_NO_AUDIT=1
export REMOTE_TEST_STATE="$TMP/state"
export REMOTE_TEST_ROOT="$ROOT"
export REMOTE_TEST_SHIPPED="$TMP/shipped"
export PROJECT_OWNER_REPOSITORY="$TMP/runtime"

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
ACCESS_KIND=remote
OWNER_ENDPOINT=owner
OWNER_YARD_NAME=
SSH_PORT=2222
ENV

# Remote overview uses the HostID-keyed owner snapshot and a fresh cache avoids another SSH call.
printf 'one\n' > "$REMOTE_TEST_STATE/info-mode"
output="$($TMP/runtime/bin/yard yards)"
assert_projects "$output" 1
printf 'null\n' > "$REMOTE_TEST_STATE/info-mode"
output="$($TMP/runtime/bin/yard yards)"
assert_projects "$output" 1
printf 'fail\n' > "$REMOTE_TEST_STATE/info-mode"
output="$($TMP/runtime/bin/yard yards)"
assert_projects "$output" 1

state_file="$SUBYARD_CONFIG_HOME/yards/remote/projects/demo-12345678.json"
write_state() {
  local target="$1"
  rm -f "$REMOTE_TEST_STATE/data-cleanup" "$REMOTE_TEST_STATE/staged-delete" "$REMOTE_TEST_STATE/workspace-delete"
  install -d -m 0700 "$REMOTE_TEST_STATE/workspace" "$REMOTE_TEST_STATE/staged"
  printf '%s\n' 'sha256:owned-container' > "$REMOTE_TEST_STATE/container"
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
  "$TMP/runtime/bin/yard" list --live >/dev/null
}
run_remove() {
  "$TMP/runtime/bin/yard" -Y owner-a/default remove demo-12345678 "$@" --yes
}

# L1 removal has no L2 promise, warning, or owner-host cleanup call.
write_state yard
rm -f "$REMOTE_TEST_STATE/data-cleanup" "$REMOTE_TEST_STATE/workspace-delete"
output="$(run_remove --soft 2>&1)"
assert_not_contains "$output" 'L2'
assert_not_contains "$output" 'box teardown'
[ ! -e "$REMOTE_TEST_STATE/data-cleanup" ] || fail 'L1 removal called L2 cleanup'
[ ! -e "$state_file" ] || fail 'native soft removal kept controller state'
[ ! -e "$REMOTE_TEST_STATE/owner-config/projects/demo-12345678.json" ] || fail 'native soft removal retained owner state'
[ -d "$REMOTE_TEST_STATE/workspace" ] || fail 'soft removal deleted the retained workspace'

# An unreachable in-yard L2 environment fails during read-only removal preflight, before either
# controller state or workspace deletion can change.
write_state synthetic
printf 'fail\n' > "$REMOTE_TEST_STATE/cleanup-mode"
rm -f "$REMOTE_TEST_STATE/data-cleanup" "$REMOTE_TEST_STATE/workspace-delete" "$REMOTE_TEST_STATE/owner-calls"
if output="$(run_remove 2>&1)"; then fail 'remote L2 removal ignored failed environment preflight'; fi
assert_contains "$output" 'reach project environment before removal'
[ -e "$state_file" ] || fail 'failed L2 preflight removed controller state'
[ ! -e "$REMOTE_TEST_STATE/workspace-delete" ] || fail 'failed L2 preflight deleted the workspace'
[ -e "$REMOTE_TEST_STATE/owner-config/projects/demo-12345678.json" ] || fail 'failed L2 preflight changed owner registry state'

# Once in-yard cleanup succeeds, native removal commits state after the workspace is gone.
printf 'ok\n' > "$REMOTE_TEST_STATE/cleanup-mode"
rm -f "$REMOTE_TEST_STATE/owner-calls"
output="$(run_remove 2>&1)"
assert_contains "$output" 'removed demo'
[ -e "$REMOTE_TEST_STATE/data-cleanup" ] || fail 'successful L2 removal skipped in-yard cleanup'
[ -e "$REMOTE_TEST_STATE/workspace-delete" ] || fail 'successful L2 removal skipped workspace deletion'
[ ! -e "$REMOTE_TEST_STATE/workspace" ] && [ ! -e "$REMOTE_TEST_STATE/container" ] && [ ! -e "$REMOTE_TEST_STATE/staged" ] || fail 'native verification retained a removed physical target'
[ ! -e "$state_file" ] || fail 'native L2 removal kept controller state'
[ ! -e "$REMOTE_TEST_STATE/owner-config/projects/demo-12345678.json" ] || fail 'native removal did not converge owner registry state'

printf 'ok: remote project counts are cached and native removal is target-aware\n'

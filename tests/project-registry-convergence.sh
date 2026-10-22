#!/usr/bin/env bash
# Remote project mutations must converge the owner host's registry, not only controller state.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-project-registry.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_contains() { grep -Fq -- "$2" <<<"$1" || fail "output does not contain: $2"; }
assert_json() { jq -e "$2" "$1" >/dev/null || fail "unexpected state in $1: $2"; }

mkdir -p "$TMP/bin" "$TMP/config/yards" "$TMP/shipped" "$TMP/subyard" "$TMP/state" "$TMP/home"
for f in agents.env host.env ports.env; do : > "$TMP/shipped/$f"; done
printf ': "${YARD_INSTANCE_NAME:=yard}"\n: "${INCUS_PROJECT:=subyard}"\n' > "$TMP/shipped/incus.project.env"
printf ': "${SSH_PORT:=2222}"\n' > "$TMP/shipped/subyard.env"
printf '%s\n' 'subyard-remote-remote ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fixture' \
  > "$TMP/state/known_hosts"

cat > "$TMP/bin/ssh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
joined="$*"
if [ "${1:-}" = -G ]; then
  printf 'hostname 127.0.0.1\nport 22\nhostkeyalias subyard-remote-remote\nuserknownhostsfile %s\n' \
    "$REGISTRY_TEST_STATE/known_hosts"
  exit 0
fi
if [[ "$joined" == *yard* && "$joined" == *rpc* && "$joined" == *--stdio* ]]; then
  printf '%s\n' rpc >> "$REGISTRY_TEST_STATE/owner-calls"
  exec env -u SUBYARD_STATE_DIR -u SUBYARD_CONFIG_LOADED -u SUBYARD_ENGINE_CONTEXT \
    -u OWNER_ENDPOINT -u OWNER_YARD_NAME -u SUBYARD_YARD -u YARD_NAME \
    -u ENVIRONMENT_PROFILES -u YARD_PROFILES \
    SUBYARD_OPERATOR_HOME="$REGISTRY_TEST_STATE/owner-home" \
    SUBYARD_CONFIG_HOME="$REGISTRY_TEST_STATE/owner-config" \
    SUBYARD_HOME="$REGISTRY_TEST_STATE/owner-data" \
    ACCESS_KIND=local SSH_HOST=yard-inner YARD_INSTANCE_NAME=yard-inner \
    "$(dirname "$0")/project-owner-fixture" -Y inner rpc --stdio
fi
if [[ "$joined" == *'_project-state'* ]];then
  # The controller may read the canonical identity before exact owner admission.
  # Mutating legacy verbs must never satisfy this fixture.
  mapfile -d '' -t owner_arguments < <(python3 -c '
import os,shlex,sys
words=shlex.split(sys.argv[-1])
if words[:2] == ["bash","-lc"]: words=shlex.split(words[2])
while len(words)==1: words=shlex.split(words[0])
words=words[next(i for i,word in enumerate(words) if os.path.basename(word)=="yard")+1:]
assert "preview" in words or "check-role" in words
sys.stdout.buffer.write(b"\0".join(word.encode() for word in words)+b"\0")
' "$@")
  [ "${#owner_arguments[@]}" -gt 0 ] || exit 1
  exec env -u SUBYARD_STATE_DIR -u SUBYARD_CONFIG_LOADED -u SUBYARD_ENGINE_CONTEXT \
    -u OWNER_ENDPOINT -u OWNER_YARD_NAME -u SUBYARD_YARD -u YARD_NAME \
    -u ENVIRONMENT_PROFILES -u YARD_PROFILES \
    SUBYARD_OPERATOR_HOME="$REGISTRY_TEST_STATE/owner-home" \
    SUBYARD_CONFIG_HOME="$REGISTRY_TEST_STATE/owner-config" \
    SUBYARD_HOME="$REGISTRY_TEST_STATE/owner-data" \
    ACCESS_KIND=local SSH_HOST=yard-inner YARD_INSTANCE_NAME=yard-inner \
    "$(dirname "$0")/project-owner-fixture" "${owner_arguments[@]}"
fi
if [[ "$joined" == *'.subyard-meta.json'* ]] && [[ "$joined" == *"'tee'"* || "$joined" == *'exec tee --'* || "$joined" == *'cat >'* ]]; then
  cat > "$REGISTRY_TEST_STATE/yard-meta.json"
  [[ "$joined" =~ /srv/workspaces/([A-Za-z0-9_-]+)/.subyard-meta.json ]] || exit 1
  cp "$REGISTRY_TEST_STATE/yard-meta.json" "$REGISTRY_TEST_STATE/guest/workspaces/${BASH_REMATCH[1]}/.subyard-meta.json"
  exit 0
fi
if [[ "$joined" == *'.subyard-meta.json'* ]]; then
  if [ -e "$REGISTRY_TEST_STATE/yard-meta.json" ];then cat "$REGISTRY_TEST_STATE/yard-meta.json";fi
  exit 0
fi
if [[ "$joined" == *"'-xf' '-'"* ]]; then
  stream_index="$(cat "$REGISTRY_TEST_STATE/stream-count" 2>/dev/null || printf 0)"
  stream_index=$((stream_index + 1))
  printf '%s\n' "$stream_index" > "$REGISTRY_TEST_STATE/stream-count"
  cat > "$REGISTRY_TEST_STATE/tar-stream-$stream_index.tar"
  [[ "$joined" =~ /srv/workspaces/([A-Za-z0-9_-]+)/src ]] || exit 1
  guest_source="$REGISTRY_TEST_STATE/guest/workspaces/${BASH_REMATCH[1]}/src"
  install -d -m 0700 "$guest_source"
  tar -C "$guest_source" -xf "$REGISTRY_TEST_STATE/tar-stream-$stream_index.tar"
  : > "$REGISTRY_TEST_STATE/tar-stream"
  exit 0
fi
exit 0
MOCK
chmod 755 "$TMP/bin/ssh"



# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
# Guest ownership must not depend on the controller's UID.
export DEV_UID="$(( $(id -u) + 1 ))"
setup_test_repository "$TMP" "$ROOT"
# Exercise resolver-owned registry paths and named-yard identity.
unset SUBYARD_STATE_DIR ACCESS_KIND YARD_INSTANCE_NAME INCUS_PROJECT SSH_HOST
export PATH="$TMP/bin:$PATH"
go build -o "$TMP/bin/project-owner-fixture" "$ROOT/tests/helpers/project-owner-fixture"
export HOME="$TMP/home"
export SUBYARD_CONFIG_DIR="$TMP/shipped"
export SUBYARD_NO_AUDIT=1
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
export REGISTRY_TEST_STATE="$TMP/state"
export PROJECT_OWNER_REPOSITORY="$TMP/runtime"
install -d -m 0700 "$REGISTRY_TEST_STATE/owner-home" "$REGISTRY_TEST_STATE/owner-config/yards" "$REGISTRY_TEST_STATE/owner-data"
printf 'SSH_PORT=3333\nENVIRONMENT_PROFILES=synthetic\n' > "$REGISTRY_TEST_STATE/owner-config/yards/inner.env"
chmod 0600 "$REGISTRY_TEST_STATE/owner-config/yards/inner.env"
printf 'owner-a\n' > "$REGISTRY_TEST_STATE/owner-config/host-id"
chmod 0600 "$REGISTRY_TEST_STATE/owner-config/host-id"

# The physical P0 project contract uses its private catalog with the actual
# test-vms owner registration. Exercise that preparation through the real loader
# and project admission, without changing the installed owner's profile selection.
# Globals below are consumed by the extracted owner registration/preparation.
# shellcheck disable=SC2034
(
  OWNER_ROOT="$TMP/p0-owner"
  OWNER_YARD_DIR="$OWNER_ROOT/config/yards"
  MARKER=subyard-p0-profile-selection
  OWNER_BASE_IMAGE_REFERENCE=images:fixture
  OWNER_DIAGNOSTIC_DEV_UID=1001
  OWNER_DIAGNOSTIC_VM_MEMORY=700MiB
  OWNER_DIAGNOSTIC_VM_BOOT_TIMEOUT=600
  install -d -m 0700 "$OWNER_YARD_DIR/test-yard" "$OWNER_ROOT/source/P0Selection" \
    "$OWNER_ROOT/state/guest/workspaces" "$OWNER_ROOT/home" "$OWNER_ROOT/data"
  printf '# %s\n' "$MARKER" > "$OWNER_YARD_DIR/test-yard/config.env"
  chmod 0600 "$OWNER_YARD_DIR/test-yard/config.env"
  eval "$(sed -n '/^write_owner_registration() {/,/^}/p' "$ROOT/dev/e2e/p0-guest.sh")"
  write_owner_registration test-yard test-vms 2224 1
  ! grep -q '^ENVIRONMENT_PROFILES=' "$OWNER_YARD_DIR/test-yard/config.env" \
    || fail 'owner registration unexpectedly forces the synthetic profile'
  export HOME="$OWNER_ROOT/home" SUBYARD_OPERATOR_HOME="$OWNER_ROOT/home"
  export SUBYARD_CONFIG_HOME="$OWNER_ROOT/config" SUBYARD_HOME="$OWNER_ROOT/data"
  export REGISTRY_TEST_STATE="$OWNER_ROOT/state"
  export PROJECT_OWNER_REPOSITORY="$OWNER_ROOT/project-runtime/runtime"
  unset SUBYARD_CONFIG_DIR SUBYARD_STATE_DIR SUBYARD_CONFIG_LOADED SUBYARD_ENGINE_CONTEXT \
    ENVIRONMENT_PROFILES YARD_PROFILES
  printf 'selection fixture\n' > "$OWNER_ROOT/source/P0Selection/file.txt"
  git init -q --initial-branch=main "$OWNER_ROOT/source/P0Selection"
  git -C "$OWNER_ROOT/source/P0Selection" add file.txt
  git -C "$OWNER_ROOT/source/P0Selection" -c user.name=Fixture \
    -c user.email=fixture@example.invalid commit -qm fixture
  owner_project_contract() {
    [ "$1" = synthetic ] || fail 'P0 project target changed'
    YARD_ENGINE_PATH="$TMP/bin/project-owner-fixture" "$PROJECT_CONTRACT_BIN" \
      -Y test-yard clone "$OWNER_ROOT/source/P0Selection" P0Selection --target synthetic --yes >/dev/null
    selection="$(YARD_ENGINE_PATH="$TMP/bin/project-owner-fixture" "$PROJECT_CONTRACT_BIN" \
      -Y test-yard config show ENVIRONMENT_PROFILES)"
    grep -Fxq 'effective: synthetic' <<<"$selection" || fail 'P0 private default was not resolved'
    assert_json "$SUBYARD_CONFIG_HOME/yards/test-yard/projects/P0Selection.json" \
      '.target == "synthetic" and .profile == "synthetic"'
    descriptor="$PROJECT_OWNER_REPOSITORY/config/profiles/synthetic/profile.json"
    [ "$(jq -c 'del(.default_yards)' "$descriptor")" = \
      "$(jq -c . "$TMP/runtime/config/profiles/synthetic/profile.json")" ] \
      || fail 'P0 selection changed the shared synthetic descriptor data'
    grep -Fxq 'PROJECT_ENV_BASE_IMAGE=debian:13' \
      "$PROJECT_OWNER_REPOSITORY/config/profiles/synthetic/profile.conf" \
      || fail 'synthetic project environment lost its base image'
    printf 'ENVIRONMENT_PROFILES=\n' >> "$OWNER_YARD_DIR/test-yard/config.env"
    before="$(find "$REGISTRY_TEST_STATE" "$SUBYARD_CONFIG_HOME" -type f -exec sha256sum {} + | sort | sha256sum)"
    if output="$(YARD_ENGINE_PATH="$TMP/bin/project-owner-fixture" "$PROJECT_CONTRACT_BIN" \
      -Y test-yard sync "$OWNER_ROOT/source/P0Selection" --target synthetic --yes 2>&1)"; then
      fail 'explicitly empty owner selection admitted the P0 target'
    fi
    assert_contains "$output" 'not active'
    after="$(find "$REGISTRY_TEST_STATE" "$SUBYARD_CONFIG_HOME" -type f -exec sha256sum {} + | sort | sha256sum)"
    [ "$before" = "$after" ] || fail 'inactive P0 target changed registry or guest files'
  }
  prepare_p0_contract() {
    eval "$(sed -n '/^  # Exercise L2 ownership/,/  PROJECT_CONTRACT_BIN=.* owner_project_contract synthetic/p' \
      "$ROOT/dev/e2e/p0-guest.sh")"
  }
  prepare_p0_contract
  printf 'ok: P0 private profile defaults admit owner projects and respect explicit deselection\n'
)


# Registry convergence, source-key and named-yard matrices live in
# TestOwnerProjectStateConvergesWithoutLosingLocalSource. Keep actual RPC and bytes here.
# A remote controller maps to that named owner yard.
cat > "$SUBYARD_CONFIG_HOME/yards/remote.env" <<'ENV'
ACCESS_KIND=remote
OWNER_ENDPOINT=owner
OWNER_YARD_NAME=inner
SSH_PORT=2222
ENVIRONMENT_PROFILES=
ENV
mkdir -p "$TMP/projects/RemoteDemo"
printf 'demo\n' > "$TMP/projects/RemoteDemo/file.txt"
remote_id=RemoteDemo
"$TMP/runtime/bin/yard" -Y remote sync "$TMP/projects/RemoteDemo" --target synthetic --yes >/dev/null
remote_state="$SUBYARD_CONFIG_HOME/yards/remote/projects/$remote_id.json"
[ ! -e "$remote_state" ] || fail 'native sync published obsolete controller project state'
[ -s "$REGISTRY_TEST_STATE/owner-calls" ] || fail 'native sync did not converge owner state'
jq -e '.projectId == $id and .target == "synthetic"' --arg id "$remote_id" \
  "$REGISTRY_TEST_STATE/yard-meta.json" >/dev/null || fail 'yard metadata omitted the project target'
[ -e "$REGISTRY_TEST_STATE/tar-stream" ] || fail 'native sync did not stream the project archive'
cp "$REGISTRY_TEST_STATE/yard-meta.json" "$REGISTRY_TEST_STATE/yard-meta-first.json"

# Native owner inventory routes inherit the controller's empty profile selection.
install -d -m 0700 "$SUBYARD_HOME/owner-inventory/connections"
printf '%s\n' '{"hostId":"owner-a","destination":"owner","yards":{"inner":{"sshHost":"yard-remote"}}}' \
  > "$SUBYARD_HOME/owner-inventory/connections/owner-a.json"
chmod 0600 "$SUBYARD_HOME/owner-inventory/connections/owner-a.json"

rm -f "$REGISTRY_TEST_STATE/tar-stream"
printf 'second\n' > "$TMP/projects/RemoteDemo/file.txt"
"$TMP/runtime/bin/yard" -Y owner-a/inner sync "$TMP/projects/RemoteDemo" --target synthetic --yes >/dev/null
[ -e "$REGISTRY_TEST_STATE/tar-stream" ] || fail 'second native sync did not stream the project archive'
jq -e '.projectId == "RemoteDemo-2" and .name == "RemoteDemo-2"' \
  "$REGISTRY_TEST_STATE/yard-meta.json" >/dev/null \
  || fail 'second native sync reused the first owner identity'
printf 'third\n' > "$TMP/projects/RemoteDemo/file.txt"
"$TMP/runtime/bin/yard" -Y remote sync "$TMP/projects/RemoteDemo" --target yard --yes >/dev/null
jq -e '.projectId == "RemoteDemo-3" and .name == "RemoteDemo-3"' \
  "$REGISTRY_TEST_STATE/yard-meta.json" >/dev/null \
  || fail 'third native sync reused an earlier owner identity'
jq -e '.projectId == "RemoteDemo"' "$REGISTRY_TEST_STATE/yard-meta-first.json" >/dev/null \
  || fail 'later native sync changed the first owner identity'
[ "$(tar -xOf "$REGISTRY_TEST_STATE/tar-stream-1.tar" ./file.txt)" = demo ] \
  && [ "$(tar -xOf "$REGISTRY_TEST_STATE/tar-stream-2.tar" ./file.txt)" = second ] \
  && [ "$(tar -xOf "$REGISTRY_TEST_STATE/tar-stream-3.tar" ./file.txt)" = third ] \
  || fail 'same-source sync snapshots did not preserve invocation contents'

# The authoritative owner has two occupied names independently of the stale
# controller record; native admission must choose the next safe identity.
install -d -m 0700 "$REGISTRY_TEST_STATE/owner-config/yards/inner/projects"
for occupied in StaleDemo StaleDemo-2;do
  jq -n --arg id "$occupied" '{schema:1,identityVersion:2,projectId:$id,name:$id,
    hostPath:("/owner/"+$id),yardPath:("/srv/workspaces/"+$id+"/src"),mode:"sync",
    sshHost:"yard-inner",target:"yard"}' \
    > "$REGISTRY_TEST_STATE/owner-config/yards/inner/projects/$occupied.json"
  chmod 0600 "$REGISTRY_TEST_STATE/owner-config/yards/inner/projects/$occupied.json"
done

# A stale controller allocation is discarded before planning; the owner-returned
# identity drives the physical operation and metadata on the first attempt.
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/remote/projects"
stale_controller_state="$SUBYARD_CONFIG_HOME/yards/remote/projects/StaleDemo.json"
jq -n '{
  schema:1, identityVersion:2, projectId:"StaleDemo", name:"StaleDemo",
  hostPath:"/stale/controller/StaleDemo", sourceKey:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  yardPath:"/srv/workspaces/StaleDemo/src", mode:"sync", sshHost:"yard-remote",
  importedAt:"2026-01-01T00:00:00Z", target:"yard"
}' > "$stale_controller_state"
chmod 0600 "$stale_controller_state"
mkdir -p "$TMP/other/StaleDemo"
printf 'stale\n' > "$TMP/other/StaleDemo/file.txt"
"$TMP/runtime/bin/yard" -Y remote sync "$TMP/other/StaleDemo" --target yard --yes >/dev/null
if ! jq -e '.projectId == "StaleDemo-3" and .name == "StaleDemo-3" and
  .target == "yard"' \
  "$REGISTRY_TEST_STATE/yard-meta.json" >/dev/null; then
  sed -n '1,20p' "$REGISTRY_TEST_STATE/yard-meta.json" >&2
  fail 'remote sync did not use the owner-returned canonical identity'
fi
[ ! -e "$SUBYARD_CONFIG_HOME/yards/remote/projects/StaleDemo-3.json" ] \
  || fail 'remote sync published owner identity into controller project state'

# Native remote clone owns the data-plane sequence and then converges both registries.
clone_source="$TMP/clone source.git"
git init -q --initial-branch=main "$clone_source"
printf 'approved content\n' > "$clone_source/file.txt"
git -C "$clone_source" add file.txt
git -C "$clone_source" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm fixture
clone_revision="$(git -C "$clone_source" rev-parse HEAD)"
: > "$REGISTRY_TEST_STATE/owner-calls"
clone_id=ForeignClone
"$TMP/runtime/bin/yard" -Y owner-a/inner clone "$clone_source" ForeignClone \
  --target synthetic --yes >/dev/null
clone_state="$SUBYARD_CONFIG_HOME/yards/remote/projects/$clone_id.json"
[ ! -e "$clone_state" ] || fail 'native clone published obsolete controller project state'
[ -s "$REGISTRY_TEST_STATE/owner-calls" ] || fail 'native clone did not converge owner state'
jq -e '.projectId == $id and .mode == "git" and .target == "synthetic"' --arg id "$clone_id" \
  "$REGISTRY_TEST_STATE/guest/workspaces/$clone_id/.subyard-meta.json" >/dev/null \
  || fail 'clone metadata omitted its target'

for admitted in RemoteDemo RemoteDemo-2 RemoteDemo-3 StaleDemo-3 ForeignClone;do
  assert_json "$REGISTRY_TEST_STATE/owner-config/yards/inner/projects/$admitted.json" \
    '.identityVersion == 2 and .projectId == .name'
done
clone_path="$REGISTRY_TEST_STATE/guest/workspaces/$clone_id/src"
[ "$(git -C "$clone_path" rev-parse HEAD)" = "$clone_revision" ] \
  || fail 'native owner clone did not retain the approved revision'
[ "$(git -C "$clone_path" symbolic-ref HEAD)" = refs/heads/main ] \
  || fail 'native owner clone lost its default branch'
[ "$(git -C "$clone_path" rev-parse --symbolic-full-name '@{upstream}')" = refs/remotes/origin/main ] \
  || fail 'native owner clone lost its default branch upstream'
[ "$(cat "$clone_path/file.txt")" = 'approved content' ] \
  || fail 'native owner clone did not materialize the approved contents'

# A controller selection cannot admit a target deselected on its owner.
printf 'SSH_PORT=3333\nENVIRONMENT_PROFILES=\n' > "$REGISTRY_TEST_STATE/owner-config/yards/inner.env"
printf 'ENVIRONMENT_PROFILES=synthetic\n' > "$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
before="$(find "$REGISTRY_TEST_STATE/guest" "$REGISTRY_TEST_STATE/owner-config/yards/inner/projects" -type f -exec sha256sum {} + | sort | sha256sum)"
for operation in sync clone up; do
  case "$operation" in
    sync) args=(sync "$TMP/projects/RemoteDemo" --target synthetic) ;;
    clone) args=(clone "$clone_source" RejectedClone --target synthetic) ;;
    up) args=(up ForeignClone) ;;
  esac
  if output="$("$TMP/runtime/bin/yard" -Y owner-a/inner "${args[@]}" --yes 2>&1)"; then
    fail "owner-inactive $operation was admitted"
  fi
  assert_contains "$output" 'not active'
done
after="$(find "$REGISTRY_TEST_STATE/guest" "$REGISTRY_TEST_STATE/owner-config/yards/inner/projects" -type f -exec sha256sum {} + | sort | sha256sum)"
[ "$before" = "$after" ] || fail 'owner target refusal changed project files'

printf 'ok: native sync and clone preserve registry ownership\n'

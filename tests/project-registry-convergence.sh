#!/usr/bin/env bash
# Remote project mutations must converge the owner host's registry, not only controller state.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-project-registry.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_contains() { grep -Fq -- "$2" <<<"$1" || fail "output does not contain: $2"; }
assert_not_contains() { ! grep -Fq -- "$2" <<<"$1" || fail "output unexpectedly contains: $2"; }
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
    SUBYARD_OPERATOR_HOME="$REGISTRY_TEST_STATE/owner-home" \
    SUBYARD_CONFIG_HOME="$REGISTRY_TEST_STATE/owner-config" \
    SUBYARD_HOME="$REGISTRY_TEST_STATE/owner-data" \
    ACCESS_KIND=local SSH_HOST=yard-inner YARD_INSTANCE_NAME=yard-inner \
    "$(dirname "$0")/project-owner-fixture" "${owner_arguments[@]}"
fi
if [[ "$joined" == *'.subyard-meta.json'* ]] && [[ "$joined" == *"'tee'"* || "$joined" == *'cat >'* ]]; then
  cat > "$REGISTRY_TEST_STATE/yard-meta.json"
  [[ "$joined" =~ /srv/workspaces/([A-Za-z0-9_-]+)/.subyard-meta.json ]] || exit 1
  cp "$REGISTRY_TEST_STATE/yard-meta.json" "$REGISTRY_TEST_STATE/guest/workspaces/${BASH_REMATCH[1]}/.subyard-meta.json"
  exit 0
fi
if [[ "$joined" == *'.subyard-meta.json'* ]]; then
  if [ -e "$REGISTRY_TEST_STATE/live-meta.json" ];then cat "$REGISTRY_TEST_STATE/live-meta.json"
  elif [ -e "$REGISTRY_TEST_STATE/yard-meta.json" ];then cat "$REGISTRY_TEST_STATE/yard-meta.json";fi
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
printf 'SSH_PORT=3333\n' > "$REGISTRY_TEST_STATE/owner-config/yards/inner.env"
chmod 0600 "$REGISTRY_TEST_STATE/owner-config/yards/inner.env"


# The hidden owner endpoint creates a yard-originated record without importing a foreign path.
owner_id='demo-12345678'
owner_state="$SUBYARD_CONFIG_HOME/projects/$owner_id.json"
"$TMP/runtime/bin/yard" _project-state upsert "$owner_id" Demo sync synthetic
assert_json "$owner_state" \
  '.projectId == "demo-12345678" and .name == "Demo" and .mode == "sync" and
   .target == "synthetic" and .hostPath == "" and .yardPath == "/srv/workspaces/demo-12345678/src" and
   .sshHost == "yard" and .registrySource == "yard"'
output="$("$TMP/runtime/bin/yard" list)"
assert_contains "$output" 'synthetic'
assert_contains "$output" 'OWNER'

# A later foreign upsert may refresh mode/target, but must not erase a real owner-host source path.
jq '.hostPath="/owner/Demo"' "$owner_state" > "$owner_state.tmp" \
  && chmod 600 "$owner_state.tmp" && mv "$owner_state.tmp" "$owner_state"
"$TMP/runtime/bin/yard" _project-state upsert "$owner_id" Demo git yard
assert_json "$owner_state" \
  '.hostPath == "/owner/Demo" and .mode == "git" and .target == "yard" and
   (has("registrySource") | not)'
"$TMP/runtime/bin/yard" _project-state unregister "$owner_id"
[ -e "$owner_state" ] || fail 'foreign unregister removed a full owner-local record'
wrong_source_key="$(printf %s /wrong/source | sha256sum | cut -d' ' -f1)"
if "$TMP/runtime/bin/yard" _project-state remove "$owner_id" "$wrong_source_key" >/dev/null 2>&1; then
  fail 'owner removal accepted a mismatched source'
fi
[ -e "$owner_state" ] || fail 'mismatched owner removal changed project state'
owner_source_key="$(printf %s /owner/Demo | sha256sum | cut -d' ' -f1)"
"$TMP/runtime/bin/yard" _project-state remove "$owner_id" "$owner_source_key"
[ ! -e "$owner_state" ] || fail 'matching owner removal retained project state'

# Synthetic records are removed symmetrically, and validation cannot escape the state directory.
"$TMP/runtime/bin/yard" _project-state upsert "$owner_id" Demo sync yard
"$TMP/runtime/bin/yard" _project-state unregister "$owner_id"
[ ! -e "$owner_state" ] || fail 'foreign unregister kept its synthetic owner record'
if "$TMP/runtime/bin/yard" _project-state upsert ../escape Bad sync yard >/dev/null 2>&1; then
  fail 'owner endpoint accepted an unsafe project id'
fi

# A forced refresh uses the authoritative owner registry and never imports L1 metadata.
cat > "$REGISTRY_TEST_STATE/live-meta.json" <<'JSON'
{"schema":1,"projectId":"legacy-12345678","name":"Legacy","mode":"sync"}
{"schema":1,"projectId":"../escape","name":"Unsafe ID","mode":"sync","target":"yard"}
{"schema":1,"projectId":"unsafe-target-12345678","name":"Unsafe target","mode":"sync","target":"../../tmp"}
JSON
output="$("$TMP/runtime/bin/yard" list --live 2>&1)"
legacy_state="$SUBYARD_CONFIG_HOME/projects/legacy-12345678.json"
[ ! -e "$legacy_state" ] || fail 'live list imported L1 metadata into the owner registry'
assert_not_contains "$output" 'Legacy'
[ ! -e "$SUBYARD_CONFIG_HOME/escape.json" ] || fail 'live metadata escaped the project state directory'
[ ! -e "$SUBYARD_CONFIG_HOME/projects/unsafe-target-12345678.json" ] \
  || fail 'live metadata persisted an unsafe target'
rm -f "$REGISTRY_TEST_STATE/live-meta.json"

# Named owner contexts write into their own registry and derive their local yard ssh alias.
cat > "$SUBYARD_CONFIG_HOME/yards/inner.env" <<'ENV'
SSH_PORT=3333
ENV
"$TMP/runtime/bin/yard" -Y inner _project-state upsert named-12345678 Named sync yard
named_state="$SUBYARD_CONFIG_HOME/yards/inner/projects/named-12345678.json"
assert_json "$named_state" '.sshHost == "yard-inner" and .hostPath == "" and .target == "yard"'

# A remote controller maps to that named owner yard.
cat > "$SUBYARD_CONFIG_HOME/yards/remote.env" <<'ENV'
ACCESS_KIND=remote
OWNER_ENDPOINT=owner
OWNER_YARD_NAME=inner
SSH_PORT=2222
ENV
mkdir -p "$TMP/projects/RemoteDemo"
printf 'demo\n' > "$TMP/projects/RemoteDemo/file.txt"
remote_id=RemoteDemo
"$TMP/runtime/bin/yard" -Y remote sync "$TMP/projects/RemoteDemo" --target yard --yes >/dev/null
remote_state="$SUBYARD_CONFIG_HOME/yards/remote/projects/$remote_id.json"
[ ! -e "$remote_state" ] || fail 'native sync published obsolete controller project state'
[ -s "$REGISTRY_TEST_STATE/owner-calls" ] || fail 'native sync did not converge owner state'
jq -e '.projectId == $id and .target == "yard"' --arg id "$remote_id" \
  "$REGISTRY_TEST_STATE/yard-meta.json" >/dev/null || fail 'yard metadata omitted the project target'
[ -e "$REGISTRY_TEST_STATE/tar-stream" ] || fail 'native sync did not stream the project archive'
cp "$REGISTRY_TEST_STATE/yard-meta.json" "$REGISTRY_TEST_STATE/yard-meta-first.json"

rm -f "$REGISTRY_TEST_STATE/tar-stream"
printf 'second\n' > "$TMP/projects/RemoteDemo/file.txt"
"$TMP/runtime/bin/yard" -Y remote sync "$TMP/projects/RemoteDemo" --target yard --yes >/dev/null
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
"$TMP/runtime/bin/yard" -Y remote clone "$clone_source" ForeignClone \
  --target synthetic --yes >/dev/null
clone_state="$SUBYARD_CONFIG_HOME/yards/remote/projects/$clone_id.json"
[ ! -e "$clone_state" ] || fail 'native clone published obsolete controller project state'
[ -s "$REGISTRY_TEST_STATE/owner-calls" ] || fail 'native clone did not converge owner state'
jq -e '.projectId == $id and .mode == "git" and .target == "synthetic"' --arg id "$clone_id" \
  "$REGISTRY_TEST_STATE/yard-meta.json" >/dev/null || fail 'clone metadata omitted its target'

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

printf 'ok: native sync and clone preserve registry ownership\n'

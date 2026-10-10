#!/usr/bin/env bash
# Full production project-registration lifecycle against pinned Orca. Disposable E2E VM only.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
YARD_BIN="$ROOT/.build/yard"
RUNTIME_ROOT="$ROOT"
STATE=''
YARD_NAME=''
PROJECT=''
INSTANCE=''
SSHD_PID=''
REMOTE_ADDED=0
ORIGINAL_HOME="$HOME"

stage() { printf 'orca-projects-e2e: %s\n' "$*" >&2; }
die() { printf 'orca-projects-e2e: %s\n' "$*" >&2; exit 1; }

[ "${SUBYARD_E2E_ORCA_PROJECTS:-}" = 1 ] \
  || die 'set SUBYARD_E2E_ORCA_PROJECTS=1 inside a disposable test host'
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
if [ "${SUBYARD_E2E_ORCA_LOAD_DIAGNOSTIC:-}" = 1 ]; then
  python3 -B - <<'PYGUARD' || die 'load diagnostic requires the matching disposable VM lease'
import json, os, pathlib, stat
try:
    lease = pathlib.Path('/run/subyard-e2e-lease.json')
    info = lease.lstat()
    if not (stat.S_ISREG(info.st_mode) and info.st_uid == 0 and stat.S_IMODE(info.st_mode) == 0o444):
        raise ValueError('invalid lease file')
    context = json.loads(lease.read_text())
    if not (context['schema_version'] == 2
            and context['run'] == os.environ['SUBYARD_E2E_RUN_ID'] and context['run']
            and context['slot'] == os.environ['SUBYARD_E2E_SLOT'] and context['slot']
            and context['purpose'] == 'orca-load-diagnostic'
            and os.environ['SUBYARD_E2E_VM'] == '1'):
        raise ValueError('lease identity mismatch')
except (OSError, ValueError, KeyError, TypeError):
    raise SystemExit(1)
PYGUARD
fi
for command in git go incus jq python3 ssh ssh-keygen ssh-keyscan sudo; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
[ -x /usr/sbin/sshd ] || die '/usr/sbin/sshd is required'
sudo -n true || die 'passwordless sudo is required on the disposable VM'
platform_marker="$ORIGINAL_HOME/.cache/subyard-e2e-platform/.subyard-e2e-platform-marker"
[ -f "$platform_marker" ] && [ "$(<"$platform_marker")" = subyard-e2e-platform-v1 ] \
  || die 'the shared E2E platform marker is missing or invalid'

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}

yard_engine() {
  local command
  # Init refreshes its child session; this fixture can retain the old groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v command '%q ' "$YARD_BIN" "$@"
    sg incus-admin -c "exec $command"
  else
    "$YARD_BIN" "$@"
  fi
}
yard() { yard_engine -Y "$YARD_NAME" "$@"; }
guest_root() { incus --project "$PROJECT" exec "$INSTANCE" -- "$@"; }
guest_dev() {
  incus --project "$PROJECT" exec "$INSTANCE" --user 1000 --group 1000 \
    --env HOME=/home/dev -- "$@"
}
orca_rpc() {
  guest_dev /usr/bin/python3 -B /tmp/orca-projects-helper.py rpc "$@"
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ "$REMOTE_ADDED" -eq 1 ]; then
    yard_engine remote remove orca-self --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$SSHD_PID" ] && kill -0 "$SSHD_PID" 2>/dev/null; then
    kill -TERM "$SSHD_PID" 2>/dev/null || true
    wait "$SSHD_PID" 2>/dev/null || true
  fi
  if [ -n "$YARD_NAME" ] && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64997\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    yard teardown --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-orca-projects.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-orca-projects-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

catalog() { orca_rpc repo.list; }
groups() { orca_rpc projectGroup.list; }
probe_runtime() {
  # Probe saved-tab protection and cheap native requests without logging payloads.
  guest_dev python3 -B - <<'YARD' || true
import importlib.util, time
spec = importlib.util.spec_from_file_location('fixture', '/tmp/orca-projects-helper.py')
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)
for method in ('session.tabs.listAll', 'status.get', 'projectGroup.list', 'repo.list'):
    started = time.monotonic()
    try:
        response = fixture.call_any_result('/srv/agents/orca/config/orca/orca-runtime.json', method, None)
        result = 'responded'
        if method == 'session.tabs.listAll':
            snapshots = fixture.snapshot_tabs(response)
            result += f' snapshots={len(snapshots)} tabs={sum(len(item["tabs"]) for item in snapshots)}'
    except fixture.SafeRpcError as error:
        result = str(error)
    print(f'orcaprobe: {method}: {result} ({time.monotonic() - started:.1f}s)', flush=True)
YARD
}
repo_id() {
  local path="$1"
  catalog | jq -er --arg path "$path" '
    [.repos[] | select(.path == $path and
      ((.executionHostId // "local") == "local") and (.connectionId // null) == null)] |
    select(length == 1) | .[0].id'
}
group_id() {
  local root="$1"
  groups | jq -er --arg root "$root" '
    [.groups[] | select(.parentPath == $root and .createdFrom == "migration" and
      ((.executionHostId // "local") == "local") and (.connectionId // null) == null)] |
    select(length == 1) | .[0].id'
}
assert_repo() {
  local path="$1" kind="$2" group="$3" name="${4:-}"
  catalog | jq -e --arg path "$path" --arg kind "$kind" --arg group "$group" --arg name "$name" '
    [.repos[] | select(.path == $path and .kind == $kind and .projectGroupId == $group and
      ((.executionHostId // "local") == "local") and (.connectionId // null) == null and
      ($name == "" or .displayName == $name))] | length == 1' >/dev/null \
    || die "Orca repository did not converge: $path"
}
await_repo() {
  local path="$1"
  for _ in $(seq 1 120); do
    if catalog | jq -e --arg path "$path" '.repos | any(.path == $path and .projectGroupId != null)' >/dev/null; then
      return 0
    fi
    sleep 2
  done
  die "periodic Orca discovery did not register: $path"
}

pause_discovery() {
  guest_root systemctl stop subyard-orca-discovery.timer subyard-orca-discovery.service
}
resume_discovery() {
  guest_root systemctl start subyard-orca-discovery.timer
}

assert_absent_repo() {
  local path="$1"
  catalog | jq -e --arg path "$path" '[.repos[] | select(.path == $path)] | length == 0' >/dev/null \
    || die "unexpected Orca repository exists: $path"
}
assert_no_managed_duplicates() {
  catalog | jq -e '
    [.repos[] | select(.path | startswith("/srv/workspaces/")) | .path] as $paths |
    ($paths | length) == ($paths | unique | length)' >/dev/null \
    || die 'Orca contains duplicate managed workspace paths'
  groups | jq -e '
    [.groups[] | select(
      ((.parentPath // "") | startswith("/srv/workspaces/")) and
      ((.executionHostId // "local") == "local") and (.connectionId // null) == null
    ) | .parentPath] as $paths |
    ($paths | length) == ($paths | unique | length)' >/dev/null \
    || die 'Orca contains duplicate managed project groups'
}
rpc_params() { jq -cn "$@"; }
rpc_write() {
  local method="$1" params="$2"
  orca_rpc "$method" "$params" >/dev/null
}
assert_git_checkout() {
  local path="$1" label="$2" base="${3:-$2}" id params response
  id="$(repo_id "$path")"
  params="$(rpc_params --arg selector "id:$id::$path" '{worktree:$selector}')"
  response="$(orca_rpc git.status "$params")"
  jq -e '.entries | any(.filePath == "fixture.txt" or .path == "fixture.txt")' <<<"$response" >/dev/null \
    || die "native git.status did not address $path"
  params="$(rpc_params --arg selector "id:$id::$path" \
    '{worktree:$selector,filePath:"fixture.txt",staged:false}')"
  response="$(orca_rpc git.diff "$params")"
  jq -e --arg old "$base-base"$'\n' --arg new "$label-dirty"$'\n' '
    .kind == "text" and .originalContent == $old and .modifiedContent == $new' \
    <<<"$response" >/dev/null || die "native git.diff did not address $path"
}

STATE="$(mktemp -d /var/tmp/subyard-orca-projects.XXXXXX)"
printf '%s\n' subyard-orca-projects-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="orca-projects-$token"
PROJECT="subyard-$YARD_NAME"
INSTANCE="yard-$YARD_NAME"
SSH_PORT="$(free_port)"
ORCA_PORT="$(free_port)"
OWNER_PORT="$(free_port)"
[ "$SSH_PORT" != "$ORCA_PORT" ] && [ "$SSH_PORT" != "$OWNER_PORT" ] \
  && [ "$ORCA_PORT" != "$OWNER_PORT" ] || die 'failed to allocate distinct fixture ports'

if [ -e "$ROOT/.subyard-acceptance/candidate.json" ]; then
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  YARD_BIN="$(release_candidate_prepare "$ROOT")"
  RUNTIME_ROOT="$(dirname "$(dirname "$YARD_BIN")")"
else
  bash "$ROOT/dev/build-engine.sh" >/dev/null
fi
install -d -m 0700 "$STATE/home" "$STATE/config/yards/$YARD_NAME" "$STATE/data" "$STATE/host"
export HOME="$STATE/home"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$ORIGINAL_HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
SSH_PORT=$SSH_PORT
ORCA_HOST_PORT=$ORCA_PORT
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=orca
ORCA_ADVERTISE_HOST=127.0.0.1
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

stage 'initializing the named yard and starting stock Orca'
yard init --yes >/dev/null
yard start --yes >/dev/null
yard orca up --yes >/dev/null
guest_root systemctl is-active --quiet subyard-orca.service \
  || die 'Orca did not start through the production resource handler'
incus --project "$PROJECT" file push "$ROOT/config/profiles/orca/tests/e2e/orca-projects-helper.py" \
  "$INSTANCE/tmp/orca-projects-helper.py" --mode 0755
# shellcheck source=config/profiles/orca/release.env
. "$RUNTIME_ROOT/config/profiles/orca/release.env"
stage 'probing pinned stock operations in a disposable profile'
guest_dev python3 -B /tmp/orca-projects-helper.py stock-probe --version "$ORCA_VERSION"

stage 'creating projects through clone, sync, and bind production commands'
guest_seed="/tmp/orca-projects-$token"
guest_dev bash -se -- "$guest_seed" <<'YARD'
set -euo pipefail
seed=$1
mkdir -p "$seed/source"
git -C "$seed/source" init -q
printf 'root-base\n' > "$seed/source/fixture.txt"
git -C "$seed/source" add fixture.txt
git -C "$seed/source" -c user.name=Fixture -c user.email=fixture@example.invalid \
  commit -qm initial
git clone -q --bare "$seed/source" "$seed/source.git"
YARD
yard clone "file://$guest_seed/source.git" --name clone-project --yes >/dev/null

folder_source="$STATE/host/folder-source"
bind_source="$STATE/host/bind-source"
mkdir -p "$folder_source" "$bind_source"
printf 'folder source\n' > "$folder_source/content.txt"
printf 'bind source\n' > "$bind_source/content.txt"
sudo -n chown -R 1000:1000 "$bind_source"
yard sync "$folder_source" --name folder-project --yes >/dev/null
yard bind "$bind_source" --name bind-project --target yard --yes >/dev/null

clone_root=/srv/workspaces/clone-project/src
folder_root=/srv/workspaces/folder-project/src
bind_root=/srv/workspaces/bind-project/src
clone_group="$(group_id "$clone_root")"
folder_group="$(group_id "$folder_root")"
bind_group="$(group_id "$bind_root")"
assert_repo "$clone_root" git "$clone_group" clone-project
assert_repo "$folder_root" folder "$folder_group" folder-project
assert_repo "$bind_root" folder "$bind_group" bind-project
assert_no_managed_duplicates
guest_dev /usr/local/libexec/subyard/projects-changed >/dev/null
yard init --yes >/dev/null
assert_no_managed_duplicates

if [ "${SUBYARD_E2E_ORCA_LOAD_DIAGNOSTIC:-}" = 1 ]; then
  # Explicit extended diagnostic; ordinary acceptance never enters this branch.
  # shellcheck source=config/profiles/orca/tests/e2e/load-discovery.sh
  . "$ROOT/config/profiles/orca/tests/e2e/load-discovery.sh"
  exit 0
fi

stage 'pruning missing ungrouped build checkouts through explicit and periodic sync'
pause_discovery
for mode in sync periodic; do
  stale_build="$clone_root/.build/cleanup-trial/$mode/checkout"
  guest_dev git init -q "$stale_build"
  legacy_repo="$(orca_rpc repo.add "$(rpc_params --arg path "$stale_build" '{path:$path,kind:"git"}')")"
  jq -e '.repo.projectGroupId == null' <<<"$legacy_repo" >/dev/null \
    || die 'legacy cleanup fixture already belongs to a group'
  guest_dev rm -rf -- "$stale_build"
  if [ "$mode" = sync ]; then
    yard orca sync --yes >/dev/null
  else
    resume_discovery
    for _ in $(seq 1 60); do
      catalog | jq -e --arg path "$stale_build" '.repos | all(.path != $path)' >/dev/null && break
      sleep 2
    done
  fi
  assert_absent_repo "$stale_build"
done
if [ "${SUBYARD_E2E_ORCA_CLEANUP_ONLY:-}" = 1 ]; then
  sleep 3
  yard orca restart --yes >/dev/null
  for mode in sync periodic; do
    assert_absent_repo "$clone_root/.build/cleanup-trial/$mode/checkout"
  done
  assert_repo "$clone_root" git "$clone_group" clone-project
  assert_repo "$folder_root" folder "$folder_group" folder-project
  assert_repo "$bind_root" folder "$bind_group" bind-project
  printf 'ok: explicit and periodic Orca cleanup persists after restart\n'
  exit 0
fi

stage 'discovering ignored, hidden, vendor, fixture, deep, submodule, and linked worktrees'
guest_dev bash -se -- "$clone_root" "$guest_seed" <<'YARD'
set -euo pipefail
root=$1
seed=$2
identity=(-c user.name=Fixture -c user.email=fixture@example.invalid)
init_repo() {
  path=$1 label=$2
  mkdir -p "$path"
  git -C "$path" init -q
  printf '%s-base\n' "$label" > "$path/fixture.txt"
  git -C "$path" add fixture.txt
  git -C "$path" "${identity[@]}" commit -qm initial
  printf '%s-dirty\n' "$label" > "$path/fixture.txt"
}
printf 'ignored/\n' > "$root/.gitignore"
init_repo "$root/.hidden/repo" hidden
init_repo "$root/ignored/repo" ignored
init_repo "$root/vendor/repo" vendor
init_repo "$root/fixtures/repo" fixtures
init_repo "$root/deep/one/two/repo" deep
mkdir -p "$seed/submodule-source"
git -C "$seed/submodule-source" init -q
printf 'submodule-base\n' > "$seed/submodule-source/fixture.txt"
git -C "$seed/submodule-source" add fixture.txt
git -C "$seed/submodule-source" "${identity[@]}" commit -qm initial
git -C "$root" -c protocol.file.allow=always submodule add -q \
  "$seed/submodule-source" modules/submodule
git -C "$root" add .gitmodules modules/submodule
git -C "$root" "${identity[@]}" commit -qm submodule
printf 'submodule-dirty\n' > "$root/modules/submodule/fixture.txt"
mkdir -p "$seed/linked-main"
git -C "$seed/linked-main" init -q
printf 'linked-base\n' > "$seed/linked-main/fixture.txt"
git -C "$seed/linked-main" add fixture.txt
git -C "$seed/linked-main" "${identity[@]}" commit -qm initial
mkdir -p "$root/linked"
git -C "$seed/linked-main" worktree add -qb linked-one "$root/linked/one"
git -C "$seed/linked-main" worktree add -qb linked-two "$root/linked/two"
printf 'linked-one-dirty\n' > "$root/linked/one/fixture.txt"
printf 'linked-two-dirty\n' > "$root/linked/two/fixture.txt"
init_repo "$root/stale-folder" stale-folder
init_repo "$root/stale-gone" stale-gone
init_repo "$seed/outside" outside
ln -s "$seed/outside" "$root/link-outside"
ln -s "$root/.hidden" "$root/link-inside"
ln -s . "$root/cycle"
mkdir -p "$root/plain/deep/directory"
printf 'root-dirty\n' > "$root/fixture.txt"
YARD
# Project events register mandatory roots; nested roots arrive in periodic portions.
yard sync "$folder_source" --name folder-discovery-event --yes >/dev/null

expected_paths=(
  "$clone_root"
  "$clone_root/.hidden/repo"
  "$clone_root/ignored/repo"
  "$clone_root/vendor/repo"
  "$clone_root/fixtures/repo"
  "$clone_root/deep/one/two/repo"
  "$clone_root/modules/submodule"
  "$clone_root/linked/one"
  "$clone_root/linked/two"
  "$clone_root/stale-folder"
  "$clone_root/stale-gone"
)
for path in "${expected_paths[@]}"; do
  name="${path#"$clone_root"/}"
  [ "$path" = "$clone_root" ] && name=clone-project
  await_repo "$path"
  assert_repo "$path" git "$clone_group" "$name"
done
assert_absent_repo "$clone_root/link-outside"
assert_absent_repo "$clone_root/link-inside/repo"
assert_absent_repo "$clone_root/cycle"
assert_absent_repo "$guest_seed/outside"
assert_absent_repo "$clone_root/plain/deep/directory"
[ "$(repo_id "$clone_root/linked/one")" != "$(repo_id "$clone_root/linked/two")" ] \
  || die 'two linked worktrees were collapsed into one Orca repository'
assert_no_managed_duplicates

assert_git_checkout "$clone_root" root
assert_git_checkout "$clone_root/.hidden/repo" hidden
assert_git_checkout "$clone_root/ignored/repo" ignored
assert_git_checkout "$clone_root/vendor/repo" vendor
assert_git_checkout "$clone_root/fixtures/repo" fixtures
assert_git_checkout "$clone_root/deep/one/two/repo" deep
assert_git_checkout "$clone_root/modules/submodule" submodule
assert_git_checkout "$clone_root/linked/one" linked-one linked
assert_git_checkout "$clone_root/linked/two" linked-two linked

stage 'discovering a newly added nested checkout periodically'
guest_dev bash -se -- "$clone_root/new/nested" <<'YARD'
set -euo pipefail
path=$1
mkdir -p "$path"
git -C "$path" init -q
printf 'new-nested-base\n' > "$path/fixture.txt"
git -C "$path" add fixture.txt
git -C "$path" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm initial
printf 'new-nested-dirty\n' > "$path/fixture.txt"
YARD
await_repo "$clone_root/new/nested"
yard orca sync --yes >/dev/null
assert_repo "$clone_root/new/nested" git "$clone_group" new/nested
assert_git_checkout "$clone_root/new/nested" new-nested

stage 'keeping known-root readiness during an incomplete or inaccessible background scan'
guest_dev git init -q "$clone_root/partial-good"
guest_root install -d -m 0444 "$clone_root/partial-aaa-unsearchable"
guest_root install -d -m 000 "$clone_root/partial-unreadable"
yard sync "$folder_source" --name folder-partial-event --yes >/dev/null
await_repo "$clone_root/partial-good"
assert_repo "$clone_root/partial-good" git "$clone_group" partial-good
yard orca sync --yes >/dev/null
yard orca status >"$STATE/partial-status.out" 2>&1
grep -Fq "$clone_root/partial-aaa-unsearchable" "$STATE/partial-status.out" \
  || die 'status did not diagnose the unsearchable discovery path'
grep -Fq "$clone_root/partial-unreadable" "$STATE/partial-status.out" \
  || die 'status did not diagnose the unreadable discovery path'
grep -Fq 'project groups and kinds verified' "$STATE/partial-status.out" \
  || die 'unfinished discovery blocked known-root readiness'
guest_root chmod 0755 "$clone_root/partial-aaa-unsearchable" "$clone_root/partial-unreadable"
yard orca sync --yes >/dev/null

stage 'preserving manual presentation while repairing owned membership only'
pause_discovery
manual_group_json="$(orca_rpc projectGroup.create \
  '{"name":"Manual mixed group","createdFrom":"manual"}')"
manual_group="$(jq -er '.group.id' <<<"$manual_group_json")"
rpc_write projectGroup.update "$(rpc_params --arg group "$manual_group" \
  '{groupId:$group,updates:{name:"Renamed mixed group",color:"#a855f7",tabOrder:31}}')"
rpc_write projectGroup.update "$(rpc_params --arg group "$clone_group" \
  '{groupId:$group,updates:{name:"Renamed owned group",color:"#3b82f6",tabOrder:19}}')"
guest_dev install -d /tmp/orca-projects-foreign
foreign_json="$(orca_rpc repo.add \
  '{"path":"/tmp/orca-projects-foreign","kind":"folder"}')"
foreign_id="$(jq -er '.repo.id' <<<"$foreign_json")"
clone_id="$(repo_id "$clone_root")"
rpc_write repo.update "$(rpc_params --arg repo "id:$clone_id" \
  '{repo:$repo,updates:{displayName:"Manual root name",badgeColor:"#f97316"}}')"
rpc_write repo.update "$(rpc_params --arg repo "id:$foreign_id" \
  '{repo:$repo,updates:{displayName:"Foreign manual name",badgeColor:"#a855f7"}}')"
rpc_write projectGroup.moveProject "$(rpc_params --arg repo "id:$clone_id" --arg group "$manual_group" \
  '{repo:$repo,groupId:$group,order:42}')"
rpc_write projectGroup.moveProject "$(rpc_params --arg repo "id:$foreign_id" --arg group "$manual_group" \
  '{repo:$repo,groupId:$group,order:43}')"
catalog | jq -e --arg id "$clone_id" --arg group "$manual_group" '
  .repos | any(.id == $id and .projectGroupId == $group and
    .displayName == "Manual root name" and .badgeColor == "#f97316" and .projectGroupOrder == 42)' \
  >/dev/null || die 'stock Orca did not accept the manual-presentation fixture'
yard orca sync --yes >/dev/null
catalog | jq -e --arg id "$clone_id" --arg group "$clone_group" '
  .repos | any(.id == $id and .projectGroupId == $group and
    .displayName == "Manual root name" and .badgeColor == "#f97316" and .projectGroupOrder == 42)' \
  >/dev/null || die 'reconcile did not preserve the owned repository presentation/order'
catalog | jq -e --arg id "$foreign_id" --arg group "$manual_group" '
  .repos | any(.id == $id and .projectGroupId == $group and
    .displayName == "Foreign manual name" and .badgeColor == "#a855f7" and .projectGroupOrder == 43)' \
  >/dev/null || die 'reconcile changed the foreign repository in the mixed group'
groups | jq -e --arg owned "$clone_group" --arg manual "$manual_group" '
  (.groups | any(.id == $owned and .name == "Renamed owned group" and
    .color == "#3b82f6" and .tabOrder == 19)) and
  (.groups | any(.id == $manual and .name == "Renamed mixed group" and
    .color == "#a855f7" and .tabOrder == 31))' >/dev/null \
  || die 'reconcile changed manual project-group presentation'

stage 'adopting an existing project record from a mixed manual group for the first time'
adopt_source="$STATE/host/adopt-source"
mkdir -p "$adopt_source"
printf 'existing project\n' > "$adopt_source/content.txt"
# Create a real Subyard project while the optional resource hook is unavailable.
# Then seed the pre-existing Orca representation through its supported API.
guest_root chmod 0644 /usr/local/libexec/subyard/projects-changed.d/orca
yard sync "$adopt_source" --name adopt-project --yes >/dev/null
guest_root chmod 0755 /usr/local/libexec/subyard/projects-changed.d/orca
adopt_root=/srv/workspaces/adopt-project/src
assert_absent_repo "$adopt_root"
adopt_json="$(orca_rpc repo.add "$(rpc_params --arg path "$adopt_root" '{path:$path,kind:"folder"}')")"
adopt_id="$(jq -er '.repo.id' <<<"$adopt_json")"
rpc_write repo.update "$(rpc_params --arg repo "id:$adopt_id" \
  '{repo:$repo,updates:{displayName:"Existing manual root"}}')"
rpc_write projectGroup.moveProject "$(rpc_params --arg repo "id:$adopt_id" --arg group "$manual_group" \
  '{repo:$repo,groupId:$group,order:44}')"
yard orca sync --yes >/dev/null
adopt_group="$(group_id "$adopt_root")"
[ "$adopt_group" != "$manual_group" ] || die 'first adoption reused a mixed manual group'
[ "$(repo_id "$adopt_root")" = "$adopt_id" ] || die 'first adoption replaced the existing Orca record'
assert_repo "$adopt_root" folder "$adopt_group" 'Existing manual root'
catalog | jq -e --arg id "$foreign_id" --arg group "$manual_group" '
  .repos | any(.id == $id and .projectGroupId == $group and .displayName == "Foreign manual name")' \
  >/dev/null || die 'first adoption changed the unrelated mixed-group repository'

stage 'recreating manually deleted repository and project group records'
deleted_path="$clone_root/new/nested"
deleted_id="$(repo_id "$deleted_path")"
rpc_write repo.rm "$(rpc_params --arg repo "id:$deleted_id" '{repo:$repo}')"
assert_absent_repo "$deleted_path"
yard orca sync --yes >/dev/null
assert_repo "$deleted_path" git "$clone_group" new/nested
[ "$(repo_id "$deleted_path")" != "$deleted_id" ] \
  || die 'manual repository deletion did not create a fresh record'
old_clone_group="$clone_group"
rpc_write projectGroup.delete "$(rpc_params --arg group "$clone_group" '{groupId:$group}')"
yard orca sync --yes >/dev/null
clone_group="$(group_id "$clone_root")"
[ "$clone_group" != "$old_clone_group" ] || die 'manual project-group deletion was not recreated'
for path in "${expected_paths[@]}" "$clone_root/new/nested"; do
  assert_repo "$path" git "$clone_group"
done
catalog | jq -e --arg id "$foreign_id" --arg group "$manual_group" '
  .repos | any(.id == $id and .projectGroupId == $group)' >/dev/null \
  || die 'project-group recreation changed the foreign repository'
resume_discovery

stage 'keeping root identity and terminal session across folder/git/folder transitions'
folder_id="$(repo_id "$folder_root")"
rpc_write repo.update "$(rpc_params --arg repo "id:$folder_id" \
  '{repo:$repo,updates:{displayName:"Kept folder name",badgeColor:"#ec4899"}}')"
rpc_write projectGroup.moveProject "$(rpc_params --arg repo "id:$folder_id" --arg group "$folder_group" \
  '{repo:$repo,groupId:$group,order:17}')"
session_json="$(orca_rpc session.tabs.createTerminal \
  "$(rpc_params --arg selector "id:$folder_id::$folder_root" \
    '{worktree:$selector,activate:false,clientMutationId:"orca-projects-root-session"}')")"
session_id="$(jq -er '.tab.id' <<<"$session_json")"
guest_dev bash -se -- "$folder_root" <<'YARD'
set -euo pipefail
root=$1
git -C "$root" init -q
printf 'folder-git-base\n' > "$root/fixture.txt"
git -C "$root" add fixture.txt
git -C "$root" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm initial
printf 'folder-git-dirty\n' > "$root/fixture.txt"
YARD
yard orca sync --yes >/dev/null
assert_repo "$folder_root" git "$folder_group" 'Kept folder name'
[ "$(repo_id "$folder_root")" = "$folder_id" ] || die 'folder-to-git changed the Orca repository ID'
assert_git_checkout "$folder_root" folder-git
tabs="$(orca_rpc session.tabs.list "$(rpc_params --arg selector "id:$folder_id::$folder_root" '{worktree:$selector}')")"
jq -e --arg id "$session_id" '.tabs | any(.id == $id)' <<<"$tabs" >/dev/null \
  || die 'folder-to-git lost the root terminal tab'
guest_dev rm -rf -- "$folder_root/.git"
yard orca sync --yes >/dev/null
assert_repo "$folder_root" folder "$folder_group" 'Kept folder name'
[ "$(repo_id "$folder_root")" = "$folder_id" ] || die 'git-to-folder changed the Orca repository ID'
catalog | jq -e --arg id "$folder_id" '
  .repos | any(.id == $id and .badgeColor == "#ec4899" and .projectGroupOrder == 17)' >/dev/null \
  || die 'root kind changes lost presentation or group order'
tabs="$(orca_rpc session.tabs.list "$(rpc_params --arg selector "id:$folder_id::$folder_root" '{worktree:$selector}')")"
jq -e --arg id "$session_id" '.tabs | any(.id == $id)' <<<"$tabs" >/dev/null \
  || die 'git-to-folder lost the root terminal tab'

stage 'pruning gone records while preserving existing directories and saved session tabs'
stale_folder="$clone_root/stale-folder"
stale_gone="$clone_root/stale-gone"
stale_folder_id="$(repo_id "$stale_folder")"
saved_gone="$clone_root/saved-gone"
guest_dev git init -q "$saved_gone"
await_repo "$saved_gone"
yard orca sync --yes >/dev/null
saved_gone_id="$(repo_id "$saved_gone")"
saved_session="$(guest_dev python3 -B /tmp/orca-projects-helper.py cleanup-terminal \
  create "id:$saved_gone_id::$saved_gone")"
saved_session_id="$(jq -er '.tab_id' <<<"$saved_session")"
saved_session_pty_id="$(jq -er '.pty_id' <<<"$saved_session")"
guest_dev rm -rf -- "$stale_folder/.git" "$stale_gone"
guest_dev rm -rf -- "$saved_gone"
if ! yard orca sync --yes >"$STATE/stale.out" 2>"$STATE/stale.err"; then
  die 'explicit sync failed for missing checkout cleanup'
fi
grep -Fq "$stale_folder" "$STATE/stale.out" "$STATE/stale.err" \
  || die 'former-Git record warning was not reported'
[ "$(repo_id "$stale_folder")" = "$stale_folder_id" ] \
  || die 'existing former-Git directory lost its record'
assert_absent_repo "$stale_gone"
[ "$(repo_id "$saved_gone")" = "$saved_gone_id" ] \
  || die 'missing checkout with session tabs was removed'
orca_rpc session.tabs.listAll | jq -e --arg id "$saved_session_id" \
  '.snapshots | any(.tabs | any(.id == $id))' >/dev/null \
  || die 'cleanup lost a missing checkout session tab'
guest_dev test ! -e "$stale_folder/.git" \
  && guest_dev test ! -e "$stale_gone" \
  || die 'registration repaired project filesystem content'

stage 'pruning missing checkouts retained only by stopped terminal layouts'
pause_discovery
clone_id="$(repo_id "$clone_root")"
pending_selectors=()
for mode in sync periodic; do
  pending_checkout="$clone_root/cleanup-pending-$mode"
  guest_dev git init -q "$pending_checkout"
  # Use ordinary grouped discovery, rather than injecting a legacy repo record.
  resume_discovery
  await_repo "$pending_checkout"
  pause_discovery
  yard orca sync --yes >/dev/null
  pending_id="$(repo_id "$pending_checkout")" \
    || die "pending terminal checkout was not registered: $pending_checkout"
  assert_repo "$pending_checkout" git "$clone_group" "cleanup-pending-$mode"
  pending_selector="id:$pending_id::$pending_checkout"
  pending_selectors+=("$pending_selector")
  pending_session="$(guest_dev python3 -B /tmp/orca-projects-helper.py cleanup-terminal \
    create "$pending_selector")"
  pending_tab="$(jq -er '.tab_id' <<<"$pending_session")"
  pending_pty="$(jq -er '.pty_id' <<<"$pending_session")"
  # Native close removes the layout. A reversible exact PTY stop retains it.
  stop_result="$(orca_rpc terminal.stopExact "$(rpc_params \
    --arg selector "$pending_selector" --arg pty "$pending_pty" \
    '{worktree:$selector,expectedPtyIds:[$pty],keepHistory:true}')")"
  jq -e '.stopped == 1 and .postStopVerified == true' <<<"$stop_result" >/dev/null \
    || die 'native terminal stop did not verify the stopped PTY'
  guest_dev python3 -B /tmp/orca-projects-helper.py cleanup-terminal \
    pending "$pending_selector" --tab-id "$pending_tab" --pty-id "$pending_pty"
  guest_dev rm -rf -- "$pending_checkout"
  if [ "$mode" = sync ]; then
    yard orca sync --yes >/dev/null
  else
    resume_discovery
    for _ in $(seq 1 60); do
      catalog | jq -e --arg path "$pending_checkout" '.repos | all(.path != $path)' >/dev/null && break
      sleep 2
    done
    pause_discovery
  fi
  assert_absent_repo "$pending_checkout"
  guest_dev python3 -B /tmp/orca-projects-helper.py cleanup-terminal \
    absent "$pending_selector" --tab-id "$pending_tab" --pty-id "$pending_pty"
  [ "$(repo_id "$clone_root")" = "$clone_id" ] \
    && [ "$(repo_id "$folder_root")" = "$folder_id" ] \
    && [ "$(repo_id "$saved_gone")" = "$saved_gone_id" ] \
    && [ "$(group_id "$clone_root")" = "$clone_group" ] \
    && [ "$(group_id "$folder_root")" = "$folder_group" ] \
    && [ "$(group_id "$bind_root")" = "$bind_group" ] \
    || die 'pending-layout cleanup changed unrelated project identities'
  orca_rpc session.tabs.listAll | jq -e --arg root "$session_id" --arg live "$saved_session_id" \
    '[.snapshots[].tabs[].id] | index($root) != null and index($live) != null' >/dev/null \
    || die 'pending-layout cleanup removed unrelated saved or live tabs'
done
sleep 3
yard orca restart --yes >/dev/null
for mode in sync periodic; do
  assert_absent_repo "$clone_root/cleanup-pending-$mode"
done
for pending_selector in "${pending_selectors[@]}"; do
  guest_dev python3 -B /tmp/orca-projects-helper.py cleanup-terminal absent "$pending_selector"
done
# Restart stops this bare PTY; its missing checkout now has only a stale layout.
# Reconcile explicitly so the assertion does not race periodic discovery.
yard orca sync --yes >/dev/null
assert_absent_repo "$saved_gone"
guest_dev python3 -B /tmp/orca-projects-helper.py cleanup-terminal \
  absent "id:$saved_gone_id::$saved_gone" --tab-id "$saved_session_id" \
  --pty-id "$saved_session_pty_id"
orca_rpc session.tabs.listAll | jq -e --arg root "$session_id" \
  '.snapshots | any(.tabs | any(.id == $root))' >/dev/null \
  || die 'restart lost the existing project terminal tab after pending-layout cleanup'
resume_discovery

stage 'preserving a changed hook list and repairing the missing dispatcher through explicit init'
pause_discovery
recovery_source="$STATE/host/recovery-source"
mkdir -p "$recovery_source"
printf 'recovery\n' > "$recovery_source/content.txt"
guest_root cp /etc/subyard/agent-project-hooks /tmp/orca-project-hooks-original
guest_root rm -f -- /usr/local/libexec/subyard/projects-changed
guest_root sh -c 'printf "%s\n" stale-hook > /etc/subyard/agent-project-hooks'
if ! yard sync "$recovery_source" --name recovery-project --yes \
  >"$STATE/recovery.out" 2>"$STATE/recovery.err"; then
  die 'project sync failed because its optional hook was unavailable'
fi
grep -Fq "optional agent project hook failed; run 'yard init'" "$STATE/recovery.out" "$STATE/recovery.err" \
  || die 'project success did not include the hook recovery warning'
assert_absent_repo /srv/workspaces/recovery-project/src
if yard init --yes >"$STATE/changed-hooks.out" 2>"$STATE/changed-hooks.err"; then
  die 'init silently overwrote the changed owned hook list'
fi
grep -Fq 'owned artifact drift at "/etc/subyard/agent-project-hooks"' "$STATE/changed-hooks.err" \
  || die 'init did not report the changed hook ownership conflict'
guest_root grep -Fxq stale-hook /etc/subyard/agent-project-hooks \
  || die 'init changed the conflicting hook list'
# Undo only this fixture's injected edit; missing owned files remain repairable.
guest_root cp /tmp/orca-project-hooks-original /etc/subyard/agent-project-hooks
if ! yard init --yes >/dev/null; then
  expected_helpers="$(python3 - "$RUNTIME_ROOT" <<'PY'
import hashlib, json, sys
from pathlib import Path
root = Path(sys.argv[1]) / 'config/profiles/orca/resources/orca/registration'
print(json.dumps({path.name: hashlib.sha256(path.read_bytes()).hexdigest()
                  for path in root.glob('*.py')}, sort_keys=True))
PY
)"
  expected_dispatcher="$(sha256sum "$RUNTIME_ROOT/config/projects-changed.sh" | cut -d ' ' -f 1)"
  guest_dev python3 -B /tmp/orca-projects-helper.py init-failure-diagnostics \
    --expected-registration "$expected_helpers" --expected-dispatcher "$expected_dispatcher" \
    || true
  die 'restored-wiring init failed; safe read-only diagnostics collected before teardown'
fi
guest_root test -x /usr/local/libexec/subyard/projects-changed \
  && [ "$(guest_root sha256sum /etc/subyard/agent-project-hooks | cut -d ' ' -f 1)" = \
    "$(printf '\n' | sha256sum | cut -d ' ' -f 1)" ] \
  || die 'explicit init did not repair the dispatcher while preserving the selected-hook list'
local_dispatcher_hash="$(sha256sum "$RUNTIME_ROOT/config/projects-changed.sh" | cut -d ' ' -f 1)"
guest_dispatcher_hash="$(guest_root sha256sum /usr/local/libexec/subyard/projects-changed | cut -d ' ' -f 1)"
[ "$local_dispatcher_hash" = "$guest_dispatcher_hash" ] \
  || die 'explicit init installed a stale project dispatcher'
recovery_group="$(group_id /srv/workspaces/recovery-project/src)"
assert_repo /srv/workspaces/recovery-project/src folder "$recovery_group" recovery-project
resume_discovery

stage 'keeping stopped Orca stopped through project actions and explicit init'
yard orca down --yes >/dev/null
guest_root systemctl is-active --quiet subyard-orca.service \
  && die 'Orca remained active after down'
stopped_source="$STATE/host/stopped-source"
mkdir -p "$stopped_source"
printf 'stopped\n' > "$stopped_source/content.txt"
yard sync "$stopped_source" --name stopped-project --yes >/dev/null
guest_root systemctl is-active --quiet subyard-orca.service \
  && die 'a project action implicitly started Orca'
yard init --yes >/dev/null
guest_root systemctl is-active --quiet subyard-orca.service \
  && die 'explicit init implicitly started intentionally stopped Orca'
yard orca up --yes >/dev/null
stopped_group="$(group_id /srv/workspaces/stopped-project/src)"
assert_repo /srv/workspaces/stopped-project/src folder "$stopped_group" stopped-project

stage 'running the project removal hook to prune Orca records while retaining bind source data'
guest_dev git init -q "$clone_root/remove-event-child"
yard remove bind-project --yes >/dev/null
[ -f "$bind_source/content.txt" ] || die 'bind removal deleted the owner source data'
assert_absent_repo "$bind_root"
groups | jq -e --arg id "$bind_group" '.groups | all(.id != $id)' >/dev/null \
  || die 'project removal retained its empty Orca group'
await_repo "$clone_root/remove-event-child"
assert_repo "$clone_root/remove-event-child" git "$clone_group" remove-event-child
guest_dev test ! -e "$bind_root" || die 'project removal left the bound workspace mounted'

stage 'registering a remote sync through the real AccessRemote SSH path'
remote_source="$STATE/host/remote-source"
mkdir -p "$remote_source"
printf 'remote sync\n' > "$remote_source/content.txt"
install -d -m 0700 "$HOME/.ssh"
ssh-keygen -q -t ed25519 -N '' -f "$STATE/owner-host-key"
ssh-keygen -q -t ed25519 -N '' -f "$STATE/bootstrap-key"
ssh-keyscan -T 2 -p "$OWNER_PORT" 127.0.0.1 >/dev/null 2>&1 || true
public_key="$(<"$STATE/bootstrap-key.pub")"
printf 'command="/usr/bin/python3 %s forced-command %s %s %s %s %s %s %s %s",no-agent-forwarding,no-X11-forwarding,no-pty,permitopen="127.0.0.1:%s" %s\n' \
  "$ROOT/config/profiles/orca/tests/e2e/orca-projects-helper.py" "$YARD_BIN" "$RUNTIME_ROOT" \
  "$HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME" "$YARD_NAME" "$SSH_PORT" "$STORAGE_PATH" \
  "$SSH_PORT" "$public_key" > "$STATE/authorized_keys"
chmod 0600 "$STATE/authorized_keys"
cat > "$STATE/sshd_config" <<EOF
Port $OWNER_PORT
ListenAddress 127.0.0.1
HostKey $STATE/owner-host-key
PidFile $STATE/sshd.pid
AuthorizedKeysFile $STATE/authorized_keys
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
UsePAM no
PermitRootLogin no
AllowUsers $(id -un)
AllowTcpForwarding local
PermitOpen 127.0.0.1:$SSH_PORT
GatewayPorts no
LogLevel VERBOSE
EOF
/usr/sbin/sshd -t -f "$STATE/sshd_config"
# The owner RPC child needs the Incus group added by init, just like yard().
printf -v sshd_command '%q ' /usr/sbin/sshd -D -e -f "$STATE/sshd_config"
sg incus-admin -c "exec $sshd_command" >"$STATE/sshd.log" 2>&1 &
SSHD_PID=$!
for _ in $(seq 1 50); do
  kill -0 "$SSHD_PID" 2>/dev/null || {
    sed -n '1,80p' "$STATE/sshd.log" >&2
    die 'fixture sshd exited before becoming ready'
  }
  if ssh-keyscan -T 1 -p "$OWNER_PORT" 127.0.0.1 > "$STATE/owner-known-hosts" 2>/dev/null; then
    break
  fi
  sleep 0.1
done
[ -s "$STATE/owner-known-hosts" ] || die 'fixture sshd did not become ready'
cat > "$HOME/.ssh/config" <<EOF
Host orca-owner-self
    HostName 127.0.0.1
    Port $OWNER_PORT
    User $(id -un)
    IdentityFile $STATE/bootstrap-key
    IdentitiesOnly yes
    StrictHostKeyChecking yes
    UserKnownHostsFile $STATE/owner-known-hosts
    GlobalKnownHostsFile /dev/null
    UpdateHostKeys no
EOF
chmod 0600 "$HOME/.ssh/config"
# OpenSSH resolves its default config from passwd, independently of HOME.
# Keep the production transport on the isolated fixture config and avoid
# persistent control sockets outside its marker-owned state directory.
install -d -m 0700 "$STATE/bin"
cat > "$STATE/bin/ssh" <<'SSH'
#!/usr/bin/env bash
exec /usr/bin/ssh -F "$ORCA_E2E_SSH_CONFIG" \
  -o ControlMaster=no -o ControlPath=none -o ControlPersist=no "$@"
SSH
chmod 0755 "$STATE/bin/ssh"
export ORCA_E2E_SSH_CONFIG="$HOME/.ssh/config"
export PATH="$STATE/bin:$PATH"
yard_engine remote add orca-self orca-owner-self --yard "$YARD_NAME" --yes >/dev/null
REMOTE_ADDED=1
yard_engine -Y orca-self sync "$remote_source" --name remote-sync --yes >/dev/null
remote_group="$(group_id /srv/workspaces/remote-sync/src)"
assert_repo /srv/workspaces/remote-sync/src folder "$remote_group" remote-sync

stage 'verifying persisted IDs, groups, kinds, memberships, and root session after restart'
catalog | jq -S '[.repos[] | select(.path | startswith("/srv/workspaces/")) |
  {id,path,kind,displayName,projectGroupId,projectGroupOrder,badgeColor}] | sort_by(.path)' \
  > "$STATE/catalog-before.json"
groups | jq -S '[.groups[] | select((.parentPath // "") | startswith("/srv/workspaces/")) |
  {id,name,parentPath,createdFrom,parentGroupId}] | sort_by(.parentPath)' \
  > "$STATE/groups-before.json"
sleep 3
yard orca restart --yes >/dev/null
catalog | jq -S '[.repos[] | select(.path | startswith("/srv/workspaces/")) |
  {id,path,kind,displayName,projectGroupId,projectGroupOrder,badgeColor}] | sort_by(.path)' \
  > "$STATE/catalog-after.json"
groups | jq -S '[.groups[] | select((.parentPath // "") | startswith("/srv/workspaces/")) |
  {id,name,parentPath,createdFrom,parentGroupId}] | sort_by(.parentPath)' \
  > "$STATE/groups-after.json"
cmp -s "$STATE/catalog-before.json" "$STATE/catalog-after.json" \
  || die 'Orca restart changed persisted repository identity or metadata'
cmp -s "$STATE/groups-before.json" "$STATE/groups-after.json" \
  || die 'Orca restart changed persisted project-group identity or metadata'
tabs="$(orca_rpc session.tabs.list "$(rpc_params --arg selector "id:$folder_id::$folder_root" '{worktree:$selector}')")"
jq -e --arg id "$session_id" '.tabs | any(.id == $id)' <<<"$tabs" >/dev/null \
  || die 'Orca restart lost the root terminal tab'
assert_no_managed_duplicates

printf 'ok: production Orca project lifecycle, recovery, discovery, native Git, persistence, and remote sync\n'

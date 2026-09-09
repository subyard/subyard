#!/usr/bin/env bash
# Full production project-registration lifecycle against stock Orca. Disposable E2E VM only.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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

yard() { "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"; }
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
    "$ROOT/.build/yard" remote remove orca-self --yes >/dev/null 2>&1 || rc=3
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

bash "$ROOT/dev/build-engine.sh" >/dev/null
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
incus --project "$PROJECT" file push "$ROOT/tests/real-host/orca-projects-helper.py" \
  "$INSTANCE/tmp/orca-projects-helper.py" --mode 0755

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
# A real project event for another project performs the global registration pass.
yard sync "$folder_source" --name folder-project --yes >/dev/null

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

stage 'discovering a newly added nested checkout on explicit sync'
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
assert_absent_repo "$clone_root/new/nested"
yard orca sync --yes >/dev/null
assert_repo "$clone_root/new/nested" git "$clone_group" new/nested
assert_git_checkout "$clone_root/new/nested" new-nested

stage 'reporting a partial scan while continuing other checkout registrations'
guest_dev git init -q "$clone_root/partial-good"
guest_root install -d -m 0444 "$clone_root/partial-aaa-unsearchable"
guest_root install -d -m 000 "$clone_root/partial-unreadable"
assert_absent_repo "$clone_root/partial-good"
if ! yard sync "$folder_source" --name folder-project --yes \
  >"$STATE/partial-project.out" 2>"$STATE/partial-project.err"; then
  die 'partial Orca discovery undid a successful project sync'
fi
grep -Fq 'optional agent project hook failed' "$STATE/partial-project.out" "$STATE/partial-project.err" \
  || die 'project sync did not report the partial discovery warning'
assert_repo "$clone_root/partial-good" git "$clone_group" partial-good
if yard orca sync --yes >"$STATE/partial-sync.out" 2>"$STATE/partial-sync.err"; then
  die 'explicit sync accepted an incomplete workspace scan'
fi
yard orca status >"$STATE/partial-status.out" 2>&1
grep -Fq "$clone_root/partial-aaa-unsearchable" "$STATE/partial-status.out" \
  || die 'status did not diagnose the unsearchable discovery path'
grep -Fq "$clone_root/partial-unreadable" "$STATE/partial-status.out" \
  || die 'status did not diagnose the unreadable discovery path'
grep -Fq 'registration incomplete' "$STATE/partial-status.out" \
  || die 'status did not report incomplete registration'
if grep -Fq 'project groups and kinds verified' "$STATE/partial-status.out"; then
  die 'status falsely reported readiness after an incomplete scan'
fi
guest_root chmod 0755 "$clone_root/partial-aaa-unsearchable" "$clone_root/partial-unreadable"
yard orca sync --yes >/dev/null

stage 'preserving manual presentation while repairing owned membership only'
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

stage 'retaining gone and former-Git records with diagnostics and no filesystem repair'
stale_folder="$clone_root/stale-folder"
stale_gone="$clone_root/stale-gone"
stale_folder_id="$(repo_id "$stale_folder")"
stale_gone_id="$(repo_id "$stale_gone")"
guest_dev rm -rf -- "$stale_folder/.git" "$stale_gone"
if ! yard orca sync --yes >"$STATE/stale.out" 2>"$STATE/stale.err"; then
  die 'explicit sync failed for additive stale-record diagnostics'
fi
grep -Fq "$stale_folder" "$STATE/stale.out" "$STATE/stale.err" \
  || die 'former-Git record warning was not reported'
grep -Fq "$stale_gone" "$STATE/stale.out" "$STATE/stale.err" \
  || die 'gone-directory record warning was not reported'
[ "$(repo_id "$stale_folder")" = "$stale_folder_id" ] \
  && [ "$(repo_id "$stale_gone")" = "$stale_gone_id" ] \
  || die 'stale Orca records were removed or replaced'
guest_dev test ! -e "$stale_folder/.git" \
  && guest_dev test ! -e "$stale_gone" \
  || die 'registration repaired project filesystem content'

stage 'repairing missing dispatcher and stale selected-hook list through explicit init'
recovery_source="$STATE/host/recovery-source"
mkdir -p "$recovery_source"
printf 'recovery\n' > "$recovery_source/content.txt"
guest_root rm -f -- /usr/local/libexec/subyard/projects-changed
guest_root sh -c 'printf "%s\n" stale-hook > /etc/subyard/agent-project-hooks'
if ! yard sync "$recovery_source" --name recovery-project --yes \
  >"$STATE/recovery.out" 2>"$STATE/recovery.err"; then
  die 'project sync failed because its optional hook was unavailable'
fi
grep -Fq "optional agent project hook failed; run 'yard init'" "$STATE/recovery.out" "$STATE/recovery.err" \
  || die 'project success did not include the hook recovery warning'
assert_absent_repo /srv/workspaces/recovery-project/src
yard init --yes >/dev/null
guest_root test -x /usr/local/libexec/subyard/projects-changed \
  && [ "$(guest_root sha256sum /etc/subyard/agent-project-hooks | cut -d ' ' -f 1)" = \
    "$(printf '\n' | sha256sum | cut -d ' ' -f 1)" ] \
  || die 'explicit init did not repair dispatcher and selected-hook list'
local_dispatcher_hash="$(sha256sum "$ROOT/config/projects-changed.sh" | cut -d ' ' -f 1)"
guest_dispatcher_hash="$(guest_root sha256sum /usr/local/libexec/subyard/projects-changed | cut -d ' ' -f 1)"
[ "$local_dispatcher_hash" = "$guest_dispatcher_hash" ] \
  || die 'explicit init installed a stale project dispatcher'
recovery_group="$(group_id /srv/workspaces/recovery-project/src)"
assert_repo /srv/workspaces/recovery-project/src folder "$recovery_group" recovery-project

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

stage 'running the project removal hook while retaining Orca records and bind source data'
bind_id="$(repo_id "$bind_root")"
guest_dev git init -q "$clone_root/remove-event-child"
assert_absent_repo "$clone_root/remove-event-child"
yard remove bind-project --yes >/dev/null
[ -f "$bind_source/content.txt" ] || die 'bind removal deleted the owner source data'
[ "$(repo_id "$bind_root")" = "$bind_id" ] || die 'project removal deleted the additive Orca record'
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
  "$ROOT/tests/real-host/orca-projects-helper.py" "$ROOT/.build/yard" "$ROOT" \
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
/usr/sbin/sshd -D -e -f "$STATE/sshd_config" >"$STATE/sshd.log" 2>&1 &
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
"$ROOT/.build/yard" remote add orca-self orca-owner-self --yard "$YARD_NAME" --yes >/dev/null
REMOTE_ADDED=1
"$ROOT/.build/yard" -Y orca-self sync "$remote_source" --name remote-sync --yes >/dev/null
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

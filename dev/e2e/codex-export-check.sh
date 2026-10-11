#!/usr/bin/env bash
# Sourced by the marker-owned disposable preview pair after SSH registration.

CODEX_SOURCES="/tmp/subyard-codex-$RUN_ID-$VM"
[ ! -e "$CODEX_SOURCES" ] && [ ! -L "$CODEX_SOURCES" ] || fail 'Codex source fixture already exists'
install -d -m 0700 "$CODEX_SOURCES" "$STATE/codex-app"
printf '%s\n' "$MARKER" > "$CODEX_SOURCES/.marker"
for name in CodexOne CodexTwo; do
  install -d -m 0700 "$CODEX_SOURCES/$name"
  git -C "$CODEX_SOURCES/$name" init -q
  printf '%s\n' "$name" > "$CODEX_SOURCES/$name/project.txt"
done
# A synthetic target records the L1 workspace without installing a shipped profile.
[ ! -e "$ROOT/config/profiles/codex-fixture" ] && [ ! -L "$ROOT/config/profiles/codex-fixture" ] \
  || fail 'synthetic Codex profile already exists'
install -d -m 0755 "$ROOT/config/profiles/codex-fixture"
printf 'PROFILE_NAME=codex-fixture\n' > "$ROOT/config/profiles/codex-fixture/profile.conf"
chmod 0644 "$ROOT/config/profiles/codex-fixture/profile.conf"
peer bash -s -- "$STATE" "$RUN_ID" <<'EOS'
set -euo pipefail
state="$1" run="$2"
[ "$(cat "$state/.marker")" = "subyard-preview-acceptance-v1:$run:2" ]
root="$(cat "$state/source-root")"
case "$root" in /tmp/subyard-worktree.*/src) ;; *) exit 2 ;; esac
[ ! -e "$root/config/profiles/codex-fixture" ] && [ ! -L "$root/config/profiles/codex-fixture" ]
install -d -m 0755 "$root/config/profiles/codex-fixture"
printf 'PROFILE_NAME=codex-fixture\n' > "$root/config/profiles/codex-fixture/profile.conf"
chmod 0644 "$root/config/profiles/codex-fixture/profile.conf"
EOS

export CODEX_TEST_STATE="$STATE"
cat > "$STATE/bin/xdg-open" <<'OPEN'
#!/usr/bin/env bash
set -euo pipefail
[ "$#" = 1 ] && [ "$1" = codex://codex-app/apply-config ]
printf '%s\n' "$1" >> "$CODEX_TEST_STATE/codex-open.calls"
[ ! -f "$CODEX_TEST_STATE/codex-open.fail" ]
OPEN
chmod 0700 "$STATE/bin/xdg-open"
CODEX_CONFIG="$STATE/codex-app/config.json"
cat > "$CODEX_CONFIG" <<'JSON'
{"version":1,"sshConnectTimeoutSeconds":23,"remoteConnections":[{"sshAlias":"retained-connection","projects":[{"remotePath":"/srv/workspaces/Retained/src","label":"Keep this label"}]}]}
JSON
chmod 0600 "$CODEX_CONFIG"
codex_fingerprint() {
  stat -c '%i:%s:%Y:%a' "$CODEX_CONFIG"
  sha256sum "$CODEX_CONFIG"
  find "$STATE/codex-app" -maxdepth 1 -type f -printf '%f\n' | sort
}
codex_no_gui() { [ ! -f "$STATE/codex-open.calls" ] || fail 'export or check launched a desktop handler'; }
codex_unchanged() { [ "$(codex_fingerprint)" = "$1" ] || fail 'Codex declaration or backup changed unexpectedly'; }
codex_projects() {
  local alias="$1" host_id="$2" yard_name="$3" count="${4:-2}"
  jq -e --arg alias "$alias" --arg label "$host_id/$yard_name" --argjson count "$count" '
    .version == 1 and .sshConnectTimeoutSeconds == 23
    and ([.remoteConnections[] | select(.sshAlias == "retained-connection")][0].projects
      == [{remotePath:"/srv/workspaces/Retained/src",label:"Keep this label"}])
    and ([.remoteConnections[] | select(.sshAlias == $alias)] | length == 1)
    and ([.remoteConnections[] | select(.sshAlias == $alias)][0].projects | length == $count)
    and ([.remoteConnections[] | select(.sshAlias == $alias)][0].projects
      | all(.remotePath | startswith("/srv/workspaces/") and endswith("/src")))
    and ([.remoteConnections[] | select(.sshAlias == $alias)][0].projects
      | all(.label | endswith(" / " + $label)))
    and ([.remoteConnections[] | select(.sshAlias == $alias)][0].projects
      | map(.remotePath) | index("/srv/workspaces/CodexOne/src") != null
        and index("/srv/workspaces/CodexTwo/src") != null)
  ' "$CODEX_CONFIG" >/dev/null || fail 'Codex projects, labels or retained connection are incorrect'
}
local_id="$(cat "$STATE/config/host-id")"
owner_id="$(peer cat "$STATE/config/host-id")"
[[ "$local_id:$owner_id" =~ ^[a-zA-Z0-9_-]+:[a-zA-Z0-9_-]+$ ]] || fail 'invalid owner identities'
for selected in default "$local_id/$NAME" preview-remote; do
  yard -Y "$selected" sync "$CODEX_SOURCES/CodexOne" --yes > "$STATE/codex-sync-one-${selected//\//_}.log" 2>&1 \
    || fail 'first Codex project sync failed'
  yard -Y "$selected" sync "$CODEX_SOURCES/CodexTwo" --target codex-fixture --yes > "$STATE/codex-sync-two-${selected//\//_}.log" 2>&1 \
    || fail 'profile-target Codex project sync failed'
done
local_instances="$(incus list --project subyard -f json | jq -c 'map(.name) | sort')"
named_instances="$(incus list --project "subyard-$NAME" -f json | jq -c 'map(.name) | sort')"
remote_instances="$(peer incus list --project "subyard-$NAME" -f json | jq -c 'map(.name) | sort')"
[ "$local_instances" = '["yard"]' ] && [ "$named_instances" = "[\"yard-$NAME\"]" ] \
  && [ "$remote_instances" = "$named_instances" ] || fail 'sync provisioned an unexpected L2 project environment'

# A missing destination must stay absent, even for the open-action preview.
yard -Y default codex open --check --config "$STATE/codex-check/config.json" > "$STATE/codex-check-absent.log" 2>&1 \
  || fail 'Codex absent-file check failed'
[ ! -e "$STATE/codex-check" ] || fail 'Codex check created files or locks'
before="$(codex_fingerprint)"
yard -Y default codex open --check --config "$CODEX_CONFIG" > "$STATE/codex-check.log" 2>&1 || fail 'Codex check failed'
codex_unchanged "$before"
codex_no_gui

for selected in default "$local_id/$NAME" preview-remote "$owner_id/$NAME"; do
  yard -Y "$selected" codex export --config "$CODEX_CONFIG" > "$STATE/codex-export-${selected//\//_}.log" 2>&1 \
    || fail 'Codex selected-yard export failed'
  before="$(codex_fingerprint)"
  yard -Y "$selected" codex export --config "$CODEX_CONFIG" > "$STATE/codex-repeat-${selected//\//_}.log" 2>&1 \
    || fail 'Codex repeat export failed'
  codex_unchanged "$before"
done
codex_projects yard "$local_id" default
codex_projects "yard-$NAME" "$local_id" "$NAME"
codex_projects yard-preview-remote "$owner_id" "$NAME"
[ "$(jq '.remoteConnections | length' "$CODEX_CONFIG")" = 4 ] || fail 'exports lost or duplicated yard connections'
codex_no_gui

# Seed the controller cache, then add a project directly on the owner. Export
# must use the new inventory while preserving the deliberately older cache.
yard list --live > "$STATE/codex-cache-seed.log" 2>&1 || fail 'owner cache seed failed'
cache="$STATE/data/owner-inventory/owners/$owner_id.json"
[ -f "$cache" ] || fail 'owner inventory cache was not seeded'
cache_before="$(sha256sum "$cache")"
peer bash -s -- "$STATE" "$RUN_ID" <<'EOS'
set -euo pipefail
state="$1" run="$2" sources="/tmp/subyard-codex-$2-2"
marker="subyard-preview-acceptance-v1:$run:2"
[ "$(cat "$state/.marker")" = "$marker" ]
[ ! -e "$sources" ] && [ ! -L "$sources" ]
install -d -o dev -g dev -m 0700 "$sources" "$sources/CodexThree"
printf '%s\n' "$marker" > "$sources/.marker"
chown dev:dev "$sources/.marker"
runuser -u dev -- git -C "$sources/CodexThree" init -q
printf 'owner-added project\n' > "$sources/CodexThree/project.txt"
chown dev:dev "$sources/CodexThree/project.txt"
EOS
ssh -T "$OWNER_ALIAS" -- "yard -Y '$NAME' sync '/tmp/subyard-codex-$RUN_ID-2/CodexThree' --yes" \
  > "$STATE/codex-owner-sync.log" 2>&1 || fail 'owner-local project sync failed'
yard -Y "$owner_id/$NAME" codex open --config "$CODEX_CONFIG" > "$STATE/codex-open-changed.log" 2>&1 \
  || fail 'changed Codex open failed'
codex_projects yard-preview-remote "$owner_id" "$NAME" 3
jq -e '.remoteConnections[] | select(.sshAlias == "yard-preview-remote") | .projects | map(.remotePath) | index("/srv/workspaces/CodexThree/src") != null' \
  "$CODEX_CONFIG" >/dev/null || fail 'Codex export used stale cached inventory'
[ "$(sha256sum "$cache")" = "$cache_before" ] || fail 'Codex export rewrote the owner inventory cache'
before="$(codex_fingerprint)"
yard -Y preview-remote codex open --config "$CODEX_CONFIG" > "$STATE/codex-open-unchanged.log" 2>&1 || fail 'unchanged Codex open failed'
codex_unchanged "$before"
touch "$STATE/codex-open.fail"
if yard -Y preview-remote codex open --config "$CODEX_CONFIG" > "$STATE/codex-open-failed.log" 2>&1; then
  fail 'Codex open accepted a failed desktop handler'
fi
codex_unchanged "$before"
grep -Fq 'export is saved' "$STATE/codex-open-failed.log" || fail 'failed handler did not report saved export'
rm "$STATE/codex-open.fail"
yard -Y preview-remote codex open --config "$CODEX_CONFIG" > "$STATE/codex-open-retry.log" 2>&1 || fail 'Codex open retry failed'
codex_unchanged "$before"
[ "$(wc -l < "$STATE/codex-open.calls")" = 4 ] || fail 'changed, unchanged, failed and retry opens did not each request import'

codex_refused() {
  local selector="$1" reason="$2" before calls
  before="$(codex_fingerprint)"; calls="$(sha256sum "$STATE/codex-open.calls")"
  if yard -Y "$selector" codex open --config "$CODEX_CONFIG" > "$STATE/codex-refused-$reason.log" 2>&1; then
    fail "Codex accepted $reason"
  fi
  codex_unchanged "$before"
  [ "$(sha256sum "$STATE/codex-open.calls")" = "$calls" ] || fail 'refused export launched the desktop handler'
}
# Override only the fixture-owned managed L1 alias. Registration and owner
# trust stay valid, so these exercise existing data-plane checks separately.
cp "$HOME/.ssh/config" "$STATE/codex-ssh-working"
codex_owner_route() {
  ssh -G "$OWNER_ALIAS" 2>/dev/null | awk '$1 == "hostname" || $1 == "port" || $1 == "user" {print}'
}
owner_route="$(codex_owner_route)"
{
  printf 'Host yard-preview-remote\n    HostName %s\n    Port 25222\n    User dev\nHost *\n' "$PEER_IP"
  cat "$STATE/codex-ssh-working"
} > "$STATE/ssh-config.next"
install -m 0600 "$STATE/ssh-config.next" "$HOME/.ssh/config"
[ "$(codex_owner_route)" = "$owner_route" ] || fail 'L1 override changed the owner SSH route'
codex_refused preview-remote wrong-L0-route
grep -Fq 'existing L1 connection' "$STATE/codex-refused-wrong-L0-route.log" || fail 'wrong route failed outside the L1 guard'
# Get the real port from the working snippet, after removing the L0 override.
install -m 0600 "$STATE/codex-ssh-working" "$HOME/.ssh/config"
remote_port="$(ssh -G yard-preview-remote 2>/dev/null | awk '$1 == "port" {print $2}')"
{
  printf 'Host yard-preview-remote\n    HostName 127.0.0.1\n    Port %s\n    ProxyJump none\nHost *\n' "$remote_port"
  cat "$STATE/codex-ssh-working"
} > "$STATE/ssh-config.next"
install -m 0600 "$STATE/ssh-config.next" "$HOME/.ssh/config"
[ "$(codex_owner_route)" = "$owner_route" ] || fail 'L1 override changed the owner SSH route'
codex_refused "$owner_id/$NAME" unavailable-L1-route
install -m 0600 "$STATE/codex-ssh-working" "$HOME/.ssh/config"
{
  printf 'Host %s\n    HostName 127.0.0.1\n    Port 1\nHost *\n' "$OWNER_ALIAS"
  cat "$STATE/codex-ssh-working"
} > "$STATE/ssh-config.next"
install -m 0600 "$STATE/ssh-config.next" "$HOME/.ssh/config"
codex_refused preview-remote unavailable-owner
# An unrelated unavailable owner must not block a selected local default.
yard -Y default codex export --config "$CODEX_CONFIG" > "$STATE/codex-local-unrelated-owner.log" 2>&1 \
  || fail 'unavailable unrelated owner blocked local default export'
install -m 0600 "$STATE/codex-ssh-working" "$HOME/.ssh/config"
[ "$(incus list --project subyard -f json | jq -c 'map(.name) | sort')" = "$local_instances" ] \
  && [ "$(incus list --project "subyard-$NAME" -f json | jq -c 'map(.name) | sort')" = "$named_instances" ] \
  && [ "$(peer incus list --project "subyard-$NAME" -f json | jq -c 'map(.name) | sort')" = "$remote_instances" ] \
  || fail 'Codex export provisioned an L2 project environment'
printf 'ok: Codex check, additive yard exports, fresh remote inventory, no-op identity, import retries and pre-write route refusals\n'

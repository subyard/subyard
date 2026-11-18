#!/usr/bin/env bash
# Shared physical project identity, snapshot, and L2 lifecycle contract.

project_contract_clean_tree() { # <fixture-root> <marker>
  local path="$1" marker="$2"
  case "$path" in /tmp/subyard-p0-project-*) ;; *) die 'unsafe project fixture root' ;; esac
  [ ! -e "$path" ] || [ "$(cat "$path/.subyard-p0-marker" 2>/dev/null)" = "$marker" ] \
    || die 'refusing to clean an unmarked project fixture'
  [ ! -e "$path" ] || sudo -n find "$path" -depth -delete
}

project_contract_sync_busy() {
  [ "$1" = 1 ] \
    && grep -Eq '^yard: release gate: .+ \(plan-stale\): .+; this command is not run$' "$2" \
    && grep -Fxq 'yard: cause: release state is changing; retry' "$2"
}

project_contract_sync_pair() (
  local root="$1" one two one_status=0 two_status=0
  shift
  # Drain both launched copies before the caller tears down the installed runtime.
  trap 'wait' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  "$@" >/dev/null 2>"$root/sync-one.stderr" &
  one=$!
  "$@" >/dev/null 2>"$root/sync-two.stderr" &
  two=$!
  wait "$one" || one_status=$?
  wait "$two" || two_status=$?
  if [ "$one_status" = 0 ] && [ "$two_status" = 0 ]; then
    printf '2\n'
    return 0
  fi
  # A busy release gate refuses before dispatch. Require the other original
  # launch to succeed so a later sequential recovery cannot hide this outcome.
  if { [ "$one_status" = 0 ] && project_contract_sync_busy "$two_status" "$root/sync-two.stderr"; } \
    || { [ "$two_status" = 0 ] && project_contract_sync_busy "$one_status" "$root/sync-one.stderr"; }; then
    printf '1\n'
    return 0
  fi
  printf 'concurrent project sync failed (exits=%s,%s)\n' "$one_status" "$two_status" >&2
  cat "$root/sync-one.stderr" "$root/sync-two.stderr" >&2
  [ "$one_status" = 0 ] || return "$one_status"
  return "$two_status"
)

owner_project_contract() {
  local target="${1:?project target required}"
  local yard_name="${PROJECT_CONTRACT_YARD:-test-yard}"
  local instance="${PROJECT_CONTRACT_INSTANCE:-yard-$yard_name}"
  local incus_project="${PROJECT_CONTRACT_INCUS_PROJECT:-subyard-$yard_name}"
  local dev_uid="${PROJECT_CONTRACT_UID:-$OWNER_DIAGNOSTIC_DEV_UID}"
  local -a project_yard=("${PROJECT_CONTRACT_BIN:-./bin/yard}" -Y "$yard_name")
  local root="/tmp/subyard-p0-project-$TOKEN"
  local source="$root/one/P0Project"
  local bound="$root/two/P0Project"
  local rejected="$root/three/P0Project"
  local git_url='file:///tmp/P0Project.git'
  local completions copies patch project projects reservation replay retried
  project_contract_clean_tree "$root" "$MARKER"
  install -d -m 0700 "$source" "$bound" "$rejected"
  printf '%s\n' "$MARKER" > "$root/.subyard-p0-marker"
  printf '%s\nbase\n' "$MARKER" > "$source/result.txt"
  printf 'bound\n' > "$bound/result.txt"
  printf 'rejected\n' > "$rejected/result.txt"
  # Bind sources must belong to the configured yard UID so shift=true preserves
  # private permissions. Stage this test-owned tree with that configured identity.
  sudo -n chown -R "$dev_uid:$dev_uid" "$bound"
  incus exec "$instance" --project "$incus_project" -- \
    runuser -u dev -- git init --bare /tmp/P0Project.git >/dev/null
  "${project_yard[@]}" bind "$bound" --yes >/dev/null
  "${project_yard[@]}" clone "$git_url" --yes >/dev/null
  "${project_yard[@]}" sync "$source" --target "$target" --yes >/dev/null
  printf '%s\nsecond\n' "$MARKER" > "$source/result.txt"
  "${project_yard[@]}" sync "$source" --target "$target" --yes >/dev/null
  printf '%s\nthird\n' "$MARKER" > "$source/result.txt"
  "${project_yard[@]}" sync "$source" --target "$target" --yes >/dev/null
  projects="$("${project_yard[@]}" list)"
  [ "$(awk '$1 == "P0Project" { count++ } END { print count+0 }' <<<"$projects")" = 1 ] \
    && [ "$(awk '$1 == "P0Project-2" { count++ } END { print count+0 }' <<<"$projects")" = 1 ] \
    && [ "$(awk '$1 == "P0Project-3" { count++ } END { print count+0 }' <<<"$projects")" = 1 ] \
    && [ "$(awk '$1 == "P0Project-4" { count++ } END { print count+0 }' <<<"$projects")" = 1 ] \
    && [ "$(awk '$1 == "P0Project-5" { count++ } END { print count+0 }' <<<"$projects")" = 1 ] \
    || die 'same-source syncs did not receive independent canonical names'
  completions="$("${project_yard[@]}" list --complete-projects)"
  for project in P0Project P0Project-2 P0Project-3 P0Project-4 P0Project-5; do
    grep -Fxq "$project" <<<"$completions" \
      || die "project completion omitted $project"
  done
  incus exec "$instance" --project "$incus_project" -- \
    jq --arg yard "$yard_name" -e '
      .identityVersion == 2 and .projectId == "P0Project-5" and
      .name == "P0Project-5" and .yard == $yard
    ' /srv/workspaces/P0Project-5/.subyard-meta.json >/dev/null \
    || die 'canonical project metadata was not published'
  "${project_yard[@]}" shell P0Project-3 --yes -- grep -Fxq base result.txt
  "${project_yard[@]}" shell P0Project-4 --yes -- grep -Fxq second result.txt
  "${project_yard[@]}" shell P0Project-5 --yes -- grep -Fxq third result.txt
  if "${project_yard[@]}" sync "$rejected" --name P0Project --yes \
    >/dev/null 2>&1; then
    die 'explicit colliding project name reached physical mutation'
  fi
  [ "$("${project_yard[@]}" list | awk '
    $1 ~ /^P0Project(-[2-5])?$/ { count++ }
    END { print count+0 }
  ')" = 5 ] \
    || die 'explicit collision changed the project inventory'
  "${project_yard[@]}" bind "$bound" --yes >/dev/null
  if "${project_yard[@]}" sync "$bound" --yes >/dev/null 2>&1; then
    die 'same source changed mode from bind to sync'
  fi
  printf '%s\nconcurrent\n' "$MARKER" > "$source/result.txt"
  copies="$(project_contract_sync_pair "$root" "${project_yard[@]}" sync "$source" --target "$target" --yes)"
  projects="$("${project_yard[@]}" list)"
  awk -v copies="$copies" '
    $1 == "P0Project-6" { first++ }
    $1 == "P0Project-7" { second++ }
    END { exit !(first == 1 && second == copies - 1) }
  ' <<<"$projects" || die 'concurrent same-source sync inventory disagrees with the launch results'
  if [ "$copies" = 1 ]; then
    "${project_yard[@]}" sync "$source" --target "$target" --yes >/dev/null
    "${project_yard[@]}" shell P0Project-7 --yes -- grep -Fxq concurrent result.txt
  fi
  "${project_yard[@]}" remove P0Project-5 --soft --yes >/dev/null
  "${project_yard[@]}" sync "$source" --target "$target" --yes >/dev/null
  "${project_yard[@]}" shell P0Project-8 --yes -- grep -Fxq concurrent result.txt
  incus exec "$instance" --project "$incus_project" -- \
    test -d /srv/workspaces/P0Project-5/src \
    || die 'soft-removed workspace was not retained'
  for project in P0Project-9 P0Project-10; do
    "${project_yard[@]}" clone "$git_url" --yes >/dev/null
    "${project_yard[@]}" shell "$project" --yes -- test -d .git
  done
  "${project_yard[@]}" shell P0Project-9 --yes -- touch independent-copy
  "${project_yard[@]}" shell P0Project-2 --yes -- test ! -e independent-copy
  "${project_yard[@]}" shell P0Project-10 --yes -- test ! -e independent-copy
  "${project_yard[@]}" clone "$git_url" --name P0CloneNamed --yes >/dev/null
  "${project_yard[@]}" shell P0CloneNamed --yes -- test -d .git
  if "${project_yard[@]}" clone "$git_url" --name P0CloneNamed --yes >/dev/null 2>&1; then
    die 'explicit clone collision was accepted'
  fi
  "${project_yard[@]}" remove P0Project-9 --yes >/dev/null
  "${project_yard[@]}" shell P0Project-10 --yes -- test -d .git
  "${project_yard[@]}" shell P0CloneNamed --yes -- test -d .git
  reservation="$("${project_yard[@]}" _project-state reserve \
    "p0-interrupted-$TOKEN" "/tmp/p0-interrupted-$TOKEN" sync P0Interrupted 0)"
  replay="$("${project_yard[@]}" _project-state reserve \
    "p0-interrupted-$TOKEN" "/tmp/p0-interrupted-$TOKEN" sync P0Interrupted 0)"
  [ "$reservation" = "$replay" ] \
    && jq -e '.projectId == "P0Interrupted" and .reserved == true' <<<"$reservation" >/dev/null \
    || die 'owner reservation replay changed canonical identity'
  "${project_yard[@]}" _project-state abort "p0-interrupted-$TOKEN"
  retried="$("${project_yard[@]}" _project-state reserve \
    "p0-retried-$TOKEN" "/tmp/p0-interrupted-$TOKEN" sync P0Interrupted 0)"
  jq -e '.projectId == "P0Interrupted" and .reserved == true' <<<"$retried" >/dev/null \
    || die 'owner reservation abort did not release canonical identity'
  "${project_yard[@]}" _project-state abort "p0-retried-$TOKEN"
  "${project_yard[@]}" shell P0Project --yes -- \
    grep -Fxq bound result.txt
  "${project_yard[@]}" remove P0Project --yes >/dev/null
  "${project_yard[@]}" shell P0Project-2 --yes -- \
    test -d .git
  "${project_yard[@]}" remove P0Project-2 --yes >/dev/null
  "${project_yard[@]}" up P0Project-3 --yes >/dev/null
  "${project_yard[@]}" info P0Project-3 | jq -e --arg target "$target" ' .profile == $target' >/dev/null
  "${project_yard[@]}" down P0Project-3 --yes >/dev/null
  env PATH=/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin "${project_yard[@]}" code P0Project-3 --yes >/dev/null
  "${project_yard[@]}" shell P0Project-3 --yes -- sh -c 'printf "mutated\n" >> result.txt'
  "${project_yard[@]}" shell P0Project-4 --yes -- grep -Fxq second result.txt
  "${project_yard[@]}" export P0Project-3 --yes >/dev/null
  patch="$(grep -RIl -- 'mutated' "${SUBYARD_HOME:-$HOME/.subyard}/exports" | sed -n '1p')"
  [ -n "$patch" ] || die 'project export did not contain the guest change'
  "${project_yard[@]}" remove P0Project-3 --yes >/dev/null
  "${project_yard[@]}" shell P0Project-4 --yes -- grep -Fxq second result.txt
  find "$patch" -delete
  project_contract_clean_tree "$root" "$MARKER"
}

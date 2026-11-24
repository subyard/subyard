#!/usr/bin/env bash
# guest.sh against local sources and unreachable ones, as the current user.
set -euo pipefail
guest="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/guest.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/subyard-user-skills-test.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT
fail() { printf 'user-skills guest test: %s\n' "$*" >&2; exit 1; }
run() { HOME="$work/home" bash "$guest" "$@"; }
harnesses=(.agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills)
repo() { # <owner/repo> <skill...>
  local dir="$work/$1"
  shift
  for skill in "$@"; do mkdir -p "$dir/skills/$skill" && printf -- '---\nname: %s\n---\n' "$skill" >"$dir/skills/$skill/SKILL.md"; done
  git -C "$dir" init -q && git -C "$dir" add -A
  git -C "$dir" -c user.name=t -c user.email=t@example.org commit -qm skills
}
repo owner/skills demo taken subyard-x
repo owner/more extra
mkdir -p "$work/home/.claude/skills" "$work/home/.config/opencode" "$work/outside" "$work/elsewhere"
ln -s "$work/outside" "$work/home/.config/opencode/skills" # a symlinked harness is used as given
printf 'mine\n' >"$work/home/.claude/skills/taken"
good="file://$work/owner/skills/.git" more="file://$work/owner/more" bad="file://$work/missing/other.git"
store="$work/home/.local/share/subyard-user-skills" clone="$work/home/.local/share/subyard-user-skills/owner/skills"
links_are() { # <expected readlink of demo, empty for none>
  for harness in "${harnesses[@]}"; do
    [ "$(readlink "$work/home/$harness/demo" || :)" = "$1" ] || fail "$harness demo link is not '$1'"
  done
}

[ "$(run check "$good" "$bad")" = work ] || fail 'check missed missing clones'
run apply "$good" "$bad" 2>"$work/stderr"
grep -q 'warning: could not clone missing/other; .*retry yard profile enable user-skills or yard init' "$work/stderr" || fail 'missing warning'
links_are "$clone/skills/demo"
[ "$(readlink "$work/outside/demo")" = "$clone/skills/demo" ] || fail 'symlinked harness not followed'
for harness in "${harnesses[@]}"; do [ ! -e "$work/home/$harness/subyard-x" ] || fail 'linked a shipped name'; done
[ "$(cat "$work/home/.claude/skills/taken")" = mine ] || fail 'replaced an existing entry'
[ -z "$(run verify "$good" "$bad")" ] || fail 'verify rejected an unreachable source'
[ "$(run check "$good" "$bad")" = work ] || fail 'check stopped retrying'
[ -z "$(find "$store" -name '.tmp-*')" ] || fail 'left a temporary clone'
# Credentials in a source never reach a warning, error or path; invalid sources warn by position.
run apply "$good" "https://user:canary-pw@example.invalid/" "file://$work/missing/repo.git?access_token=canary-token" \
  "https://example.invalid?access_token=/canary/leaf" "https://example.invalid/single-canary" 2>"$work/stderr"
for expected in 'skipped source 2: its path' 'could not clone missing/repo;' 'skipped source 4: its path' 'skipped source 5: its path'; do
  grep -q "$expected" "$work/stderr" || fail "missing warning: $expected"
done
! grep -q canary "$work/stderr" || fail 'a warning shows URL text'
[ -z "$(find "$work/home" -name '*canary*')" ] || fail 'a path contains URL text'
[ "$(run check "$good" "https://example.invalid/single")" = work ] || fail 'check accepted an invalid source'
[ -z "$(run verify "$good" "https://example.invalid/single")" ] || fail 'verify rejected an invalid source'
printf 'local\n' >"$clone/skills/demo/SKILL.md"
run apply "$good"
[ "$(cat "$clone/skills/demo/SKILL.md")" = local ] || fail 'touched an existing clone'
[ -z "$(run check "$good")" ] || fail 'converged sources report drift'
# Two sources for one name: warn, keep the first, stay not ready.
run apply "$good" "$good/" 2>"$work/stderr"
grep -q 'warning: skipped source 2: owner/skills duplicates an earlier source' "$work/stderr" || fail 'missing duplicate warning'
[ "$(run check "$good" "$good/")" = work ] || fail 'duplicate source reported ready'
links_are "$clone/skills/demo"
# A source removed from the list loses its links and keeps its clone.
run apply "$good" "$more"
[ -L "$work/home/.agents/skills/extra" ] || fail 'second source not linked'
run apply "$good"
[ ! -L "$work/home/.agents/skills/extra" ] && [ -f "$store/owner/more/skills/extra/SKILL.md" ] || fail 'removed source handling'
# A deleted skill folder loses its links.
mkdir -p "$clone/skills/gone" && printf 'gone\n' >"$clone/skills/gone/SKILL.md"
run apply "$good"
mv -T "$clone/skills/gone" "$work/gone"
[ "$(run check "$good")" = work ] || fail 'check missed a dangling link'
run apply "$good"
[ ! -L "$work/home/.agents/skills/gone" ] || fail 'kept a deleted skill link'
# Disable unlinks only links spelled exactly as ours and keeps the clones.
skills="$work/home/.agents/skills"
ln -s "$work/elsewhere" "$skills/foreign"
ln -s "$work/missing" "$skills/foreign-dangling"
ln -s "$store/../x/skills/foreign-dotdot" "$skills/foreign-dotdot"
ln -s "$store/redirect/x/y/skills/foreign-redirect" "$skills/foreign-redirect"
ln -s "$clone/skills/demo" "$skills/mismatch"
ln -s "$store/x/skills/onesegment" "$skills/onesegment"
ln -s ../../.local/share/subyard-user-skills/owner/skills/skills/demo "$skills/relative"
[ "$(run check)" = work ] || fail 'disable check missed our links'
run apply
[ -z "$(run check)" ] || fail 'links remain after disable'
links_are ''
for kept in foreign foreign-dangling foreign-dotdot foreign-redirect mismatch onesegment relative; do
  [ -L "$skills/$kept" ] || fail "removed foreign link $kept"
done
[ "$(cat "$work/home/.claude/skills/taken")" = mine ] && [ -f "$clone/skills/demo/SKILL.md" ] || fail 'removed a file or clone'
# With the store moved away, disable still removes our dangling links.
run apply "$good"
mv -T "$store" "$work/store-moved"
run apply
links_are ''
printf 'ok: user-skills guest hook\n'

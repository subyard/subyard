#!/usr/bin/env bash
# guest.sh against a local source and an unreachable one, as the current user.
set -euo pipefail
guest="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/guest.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/subyard-user-skills-test.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT
fail() { printf 'user-skills guest test: %s\n' "$*" >&2; exit 1; }
run() { HOME="$work/home" PATH="$work/bin:$PATH" bash "$guest" "$@"; }
mkdir -p "$work/home/.claude/skills" "$work/bin" "$work/src/skills/demo" "$work/src/skills/taken" "$work/src/skills/subyard-x"
printf '#!/bin/sh\nexit 1\n' >"$work/bin/subyard-github" && chmod +x "$work/bin/subyard-github"
for skill in demo taken subyard-x; do printf -- '---\nname: %s\n---\n' "$skill" >"$work/src/skills/$skill/SKILL.md"; done
git -C "$work/src" init -q && git -C "$work/src" add -A
git -C "$work/src" -c user.name=t -c user.email=t@example.org commit -qm skills
printf 'mine\n' >"$work/home/.claude/skills/taken"
good="file://$work/src/.git" bad="file://$work/missing/other.git"

[ "$(run check "$good" "$bad")" = work ] || fail 'check missed missing clones'
run apply "$good" "$bad" 2>"$work/stderr"
grep -q 'warning: could not clone .*/missing/other; .*retry yard profile enable user-skills or yard init' "$work/stderr" || fail 'missing warning'
store="$work/home/.local/share/subyard-user-skills" clone="$work/home/.local/share/subyard-user-skills/${work#/}/src"
for harness in .agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills; do
  [ "$(readlink "$work/home/$harness/demo")" = "$clone/skills/demo" ] || fail "missing $harness link"
  [ ! -e "$work/home/$harness/subyard-x" ] || fail 'linked a shipped name'
done
[ "$(cat "$work/home/.claude/skills/taken")" = mine ] || fail 'replaced an existing entry'
[ -z "$(run verify "$good" "$bad")" ] || fail 'verify rejected an unreachable source'
[ "$(run check "$good" "$bad")" = work ] || fail 'check stopped retrying'
[ -z "$(find "$store" -name '.tmp-*')" ] || fail 'left a temporary clone'
# Credentials in a source never reach a warning, error or path.
run apply "https://user:canary-pw@example.invalid/" "file://$work/missing/repo.git?access_token=canary-token" 2>"$work/stderr"
grep -q 'could not clone source;' "$work/stderr" && grep -q 'could not clone .*/missing/repo;' "$work/stderr" || fail 'missing source warnings'
! grep -q canary "$work/stderr" || fail 'a warning shows a credential'
[ -z "$(find "$work/home" -name '*canary*')" ] || fail 'a path contains a credential'
printf 'local\n' >"$clone/skills/demo/SKILL.md"
run apply "$good" 2>/dev/null
[ "$(cat "$clone/skills/demo/SKILL.md")" = local ] || fail 'touched an existing clone'
[ -z "$(run check "$good")" ] || fail 'converged sources report drift'
# Two sources for one clone directory: warn, keep the first, stay not ready.
run apply "$good" "$good/" 2>"$work/stderr"
grep -q 'warning: skipped .*/src: another source uses the same clone directory' "$work/stderr" || fail 'missing duplicate warning'
[ "$(run check "$good" "$good/")" = work ] || fail 'duplicate source reported ready'
# A deleted skill folder loses its links; others' links and entries stay.
mkdir -p "$clone/skills/gone" "$work/elsewhere"
ln -s "$clone/skills/gone" "$work/home/.agents/skills/gone"
ln -s "$work/elsewhere" "$work/home/.agents/skills/foreign"
ln -s "$work/missing" "$work/home/.agents/skills/foreign-dangling"
rmdir "$clone/skills/gone"
[ "$(run check "$good")" = work ] || fail 'check missed a dangling link'
run apply "$good"
[ ! -L "$work/home/.agents/skills/gone" ] && [ -L "$work/home/.agents/skills/demo" ] || fail 'dangling link handling'
# Disable unlinks only our links and keeps the clone.
[ "$(run check-remove)" = work ] || fail 'check-remove missed our links'
run remove
[ -z "$(run check-remove)" ] || fail 'links remain after remove'
for harness in .agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills; do
  [ ! -L "$work/home/$harness/demo" ] || fail "kept $harness link"
done
[ -L "$work/home/.agents/skills/foreign" ] && [ -L "$work/home/.agents/skills/foreign-dangling" ] || fail 'removed a foreign link'
[ "$(cat "$work/home/.claude/skills/taken")" = mine ] && [ -f "$clone/skills/demo/SKILL.md" ] || fail 'removed a file or clone'
printf 'ok: user-skills guest hook\n'

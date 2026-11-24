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
grep -q 'warning: could not clone other;' "$work/stderr" || fail 'missing warning'
clone="$work/home/.local/share/subyard-user-skills/src"
for harness in .agents/skills .claude/skills .config/opencode/skills .pi/agent/skills .hermes/skills; do
  [ "$(readlink "$work/home/$harness/demo")" = "$clone/skills/demo" ] || fail "missing $harness link"
  [ ! -e "$work/home/$harness/subyard-x" ] || fail 'linked a shipped name'
done
[ "$(cat "$work/home/.claude/skills/taken")" = mine ] || fail 'replaced an existing entry'
[ -z "$(run verify "$good" "$bad")" ] || fail 'verify rejected an unreachable source'
[ "$(run check "$good" "$bad")" = work ] || fail 'check stopped retrying'
[ "$(ls -A "$work/home/.local/share/subyard-user-skills")" = src ] || fail 'left a temporary clone'
# Credentials in a source never reach a warning, error or path.
run apply "https://user:canary-pw@example.invalid/" "file://$work/missing/repo.git?access_token=canary-token" 2>"$work/stderr"
grep -q 'could not clone source;' "$work/stderr" && grep -q 'could not clone repo;' "$work/stderr" || fail 'missing source warnings'
! grep -q canary "$work/stderr" || fail 'a warning shows a credential'
[ -z "$(find "$work/home" -name '*canary*')" ] || fail 'a path contains a credential'
printf 'local\n' >"$clone/skills/demo/SKILL.md"
run apply "$good" 2>/dev/null
[ "$(cat "$clone/skills/demo/SKILL.md")" = local ] || fail 'touched an existing clone'
[ -z "$(run check "$good")" ] || fail 'converged sources report drift'
printf 'ok: user-skills guest hook\n'

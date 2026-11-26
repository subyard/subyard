#!/usr/bin/env bash
# guest.sh against local sources and unreachable ones, as the current user.
set -euo pipefail
guest="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/guest.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/subyard-user-dotfiles-test.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT
fail() { printf 'user-dotfiles guest test: %s\n' "$*" >&2; exit 1; }
run() { HOME="$work/home" bash "$guest" "$@"; }
links=(.claude/CLAUDE.md .codex/AGENTS.md .config/opencode/AGENTS.md)
repo() { # <owner/repo> <file...>: each file holds its own path
  local dir="$work/$1"
  shift
  for file in "$@"; do mkdir -p "$(dirname "$dir/$file")" && printf '%s\n' "$file" >"$dir/$file"; done
  git -C "$dir" init -q && git -C "$dir" add -A
  git -C "$dir" -c user.name=t -c user.email=t@example.org commit -qm dotfiles
}
repo owner/dotfiles AGENTS.md agents/AGENTS.md yard/agents/AGENTS.md
repo owner/bare README.md
mkdir -p "$work/home" "$work/elsewhere"
good="file://$work/owner/dotfiles/.git" bare="file://$work/owner/bare" bad="file://$work/missing/other.git"
store="$work/home/.local/share/subyard-user-dotfiles" clone="$work/home/.local/share/subyard-user-dotfiles/owner/dotfiles"
links_are() { # <expected readlink of every agent path, empty for none>
  for link in "${links[@]}"; do
    [ "$(readlink "$work/home/$link" || :)" = "$1" ] || fail "$link is not '$1'"
  done
}

# Nothing configured is converged and touches nothing.
[ -z "$(run check)" ] && [ -z "$(run check '' '')" ] || fail 'empty source reported drift'
run apply '' ''
[ ! -e "$store" ] || fail 'empty source created the store'
# Clone once; every agent links the shared agents/AGENTS.md, never the root AGENTS.md.
[ "$(run check "$good")" = work ] || fail 'check missed a missing clone'
run apply "$good"
links_are "$clone/agents/AGENTS.md"
[ "$(cat "$work/home/.claude/CLAUDE.md")" = agents/AGENTS.md ] || fail 'link does not reach agents/AGENTS.md'
[ -z "$(run check "$good")" ] && [ -z "$(run verify "$good")" ] || fail 'converged source reports drift'
[ -z "$(find "$store" -name '.tmp-*')" ] || fail 'left a temporary clone'
printf 'local\n' >"$clone/agents/AGENTS.md"
run apply "$good"
[ "$(cat "$clone/agents/AGENTS.md")" = local ] || fail 'touched an existing clone'
# A subdirectory moves every link in one apply.
for subdir in yard /yard/; do
  run apply "$good" "$subdir"
  links_are "$clone/yard/agents/AGENTS.md"
  [ -z "$(run check "$good" "$subdir")" ] || fail "subdirectory $subdir reports drift"
done
run apply "$good"
links_are "$clone/agents/AGENTS.md"
# Invalid sources and subdirectories warn without URL text, remove our links and stay not ready.
for args in "https://user:canary-pw@example.invalid/" "https://example.invalid?access_token=/canary/leaf" \
  "$good ../canary" "$good yard/../canary" "$good .hidden"; do
  read -r -a parts <<<"$args"
  run apply "${parts[@]}" 2>"$work/stderr"
  grep -q 'warning: skipped the source' "$work/stderr" || fail "missing warning for '$args'"
  ! grep -q canary "$work/stderr" || fail 'a warning shows URL text'
  links_are ''
  [ "$(run check "${parts[@]}")" = work ] || fail "check accepted '$args'"
  [ -z "$(run verify "${parts[@]}")" ] || fail "verify rejected '$args'"
done
[ -z "$(find "$work/home" -name '*canary*')" ] || fail 'a path contains URL text'
# An unreachable source warns, links nothing, and check keeps retrying.
run apply "$bad" 2>"$work/stderr"
grep -q 'warning: could not clone missing/other; .*retry yard profile enable user-dotfiles or yard init' "$work/stderr" \
  || fail 'missing clone warning'
links_are ''
[ "$(run check "$bad")" = work ] && [ -z "$(run verify "$bad")" ] || fail 'unreachable source handling'
[ -z "$(find "$store" -name '.tmp-*')" ] || fail 'left a temporary clone after a failure'
# A clone without agents/AGENTS.md warns and links nothing.
run apply "$bare" 2>"$work/stderr"
grep -q 'warning: owner/bare has no agents/AGENTS.md' "$work/stderr" || fail 'missing file warning'
links_are ''
[ "$(run check "$bare")" = work ] && [ -z "$(run verify "$bare")" ] || fail 'missing file handling'
# Occupied paths are left alone with a warning; free paths are still linked.
mkdir -p "$work/home/.codex" "$work/home/.config/opencode"
printf 'mine\n' >"$work/home/.codex/AGENTS.md"
ln -s "$work/elsewhere/AGENTS.md" "$work/home/.config/opencode/AGENTS.md"
run apply "$good" 2>"$work/stderr"
for relative in .codex/AGENTS.md .config/opencode/AGENTS.md; do
  grep -qF "warning: left ~/$relative in place" "$work/stderr" || fail "missing occupied warning for $relative"
done
[ "$(readlink "$work/home/.claude/CLAUDE.md")" = "$clone/agents/AGENTS.md" ] || fail 'free path not linked'
[ "$(cat "$work/home/.codex/AGENTS.md")" = mine ] || fail 'replaced a foreign file'
[ "$(readlink "$work/home/.config/opencode/AGENTS.md")" = "$work/elsewhere/AGENTS.md" ] || fail 'replaced a foreign link'
[ "$(run check "$good")" = work ] && [ -z "$(run verify "$good")" ] || fail 'occupied path handling'
rm -f -- "$work/home/.codex/AGENTS.md" "$work/home/.config/opencode/AGENTS.md"
run apply "$good"
links_are "$clone/agents/AGENTS.md"
# Disable unlinks only links spelled exactly as ours and keeps the clone.
run apply
links_are ''
[ -f "$clone/agents/AGENTS.md" ] || fail 'disable removed the clone'
ln -s "$store/x/agents/AGENTS.md" "$work/home/.claude/CLAUDE.md"
ln -s ../.local/share/subyard-user-dotfiles/owner/dotfiles/agents/AGENTS.md "$work/home/.codex/AGENTS.md"
python3 -c 'import os, sys; os.symlink(sys.argv[1] + "\n", sys.argv[2])' "$clone/agents/AGENTS.md" "$work/home/.config/opencode/AGENTS.md"
[ -z "$(run check)" ] || fail 'foreign links reported as ours'
run apply
for link in "${links[@]}"; do [ -L "$work/home/$link" ] || fail "removed foreign link $link"; done
# A trailing-newline spelling is foreign, never the configured link.
run apply "$good" 2>"$work/stderr"
grep -qF 'warning: left ~/.config/opencode/AGENTS.md in place' "$work/stderr" || fail 'newline spelling treated as ours'
rm -f -- "$work/home/.claude/CLAUDE.md" "$work/home/.codex/AGENTS.md" "$work/home/.config/opencode/AGENTS.md"
# With the store moved away, disable still removes our dangling links.
run apply "$good"
mv -T "$store" "$work/store-moved"
run apply
links_are ''
printf 'ok: user-dotfiles guest hook\n'

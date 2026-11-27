#!/usr/bin/env bash
# guest.sh against local sources and unreachable ones, as the current user.
set -euo pipefail
guest="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/guest.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/subyard-user-dotfiles-test.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT
fail() { printf 'user-dotfiles guest test: %s\n' "$*" >&2; exit 1; }
run() { HOME="$work/home" bash "$guest" "$@"; }
repo() { # <owner/repo> <file=content...>
  local dir="$work/$1" pair
  shift
  for pair in "$@"; do mkdir -p "$(dirname "$dir/${pair%%=*}")" && printf '%s\n' "${pair#*=}" >"$dir/${pair%%=*}"; done
  git -C "$dir" init -q && git -C "$dir" add -A
  git -C "$dir" -c user.name=t -c user.email=t@example.org commit -qm dotfiles
}
# Each script appends its name to ~/runs and its output reaches a failure warning.
script='printf "%s\n" "$0" >>"$HOME/runs"; echo "output of $0"; exit "${FAIL:-0}"'
python=$'#!/usr/bin/env python3\nimport os\nopen(os.environ["HOME"] + "/runs", "a").write("python\\n")'
repo owner/scripts "install.sh=$script" "setup.sh=$script" "uninstall.sh=$script" "custom/run=$script" "custom/py=$python"
chmod +x "$work/owner/scripts/install.sh"
git -C "$work/owner/scripts" -c user.name=t -c user.email=t@example.org commit -qam executable
repo owner/plain "script/setup=$script" ".vimrc=vim" ".config/tool=tool" "README.md=readme"
mkdir -p "$work/home"
scripts="file://$work/owner/scripts/.git" plain="file://$work/owner/plain" bad="file://$work/missing/other.git"
store="$work/home/.local/share/subyard-user-dotfiles"
runs_are() { [ "$(cat "$work/home/runs" 2>/dev/null || :)" = "$1" ] || fail "runs are '$(cat "$work/home/runs" 2>/dev/null)', not '$1'"; }

# Nothing configured is converged and touches nothing.
[ -z "$(run check)" ] && [ -z "$(run check '' '')" ] || fail 'empty source reported drift'
run apply '' ''
[ ! -e "$store" ] || fail 'empty source created the store'
# The first clone runs the first existing script once, in the clone.
[ "$(run check "$scripts")" = work ] || fail 'check missed a missing clone'
run apply "$scripts"
runs_are ./install.sh
[ -z "$(run check "$scripts")" ] && [ -z "$(run verify "$scripts")" ] || fail 'installed source reports drift'
[ -z "$(find "$store" -name '.tmp-*')" ] || fail 'left a temporary clone'
run apply "$scripts"
runs_are ./install.sh
# Disable runs uninstall.sh, forgets the install and keeps the clone with local edits.
printf 'local\n' >"$store/owner/scripts/setup.sh"
[ "$(run check)" = work ] || fail 'check missed a pending uninstall'
run apply
runs_are $'./install.sh\n./uninstall.sh'
[ -z "$(run check)" ] || fail 'disabled source reports drift'
[ "$(cat "$store/owner/scripts/setup.sh")" = local ] || fail 'disable touched the clone'
# Re-enable reuses the clone and installs again; a failure warns with the script output.
: >"$work/home/runs"
FAIL=3 run apply "$scripts" 2>"$work/stderr"
grep -qF "warning: owner/scripts/install.sh exited with status 3; once fixed, run it again in $store/owner/scripts." "$work/stderr" \
  || fail 'missing install failure warning'
grep -qF '  output of ./install.sh' "$work/stderr" || fail 'the warning lacks the script output'
[ "$(cat "$store/owner/scripts/setup.sh")" = local ] || fail 're-enable replaced the clone'
# A failed install is not retried.
[ -z "$(run check "$scripts")" ] || fail 'a failed install reports drift'
run apply "$scripts"
runs_are ./install.sh
# A configured command replaces the list; a missing one warns and installs nothing.
run apply
: >"$work/home/runs"
run apply "$scripts" custom/run
runs_are ./custom/run
run apply
# A missing command is not recorded, so fixing the setting installs; #! lines are honoured.
: >"$work/home/runs"
for command in custom/missing ../outside; do
  run apply "$scripts" "$command" 2>"$work/stderr"
  grep -qF 'warning: USER_DOTFILES_INSTALL_COMMAND names no file in owner/scripts; fix it' "$work/stderr" \
    || fail "missing warning for $command"
  [ "$(run check "$scripts" "$command")" = work ] && [ -z "$(run verify "$scripts" "$command")" ] \
    || fail "missing command $command handling"
done
[ ! -e "$store/.installed" ] || fail 'recorded an install for a missing command'
run apply "$scripts" custom/py
runs_are python
run apply
# A non-executable script without #! runs through bash; without uninstall.sh disable keeps its files.
: >"$work/home/runs"
run apply "$plain"
runs_are ./script/setup
run apply 2>"$work/stderr"
grep -qF 'warning: owner/plain has no uninstall.sh; left the files it installed in place' "$work/stderr" \
  || fail 'missing no-uninstall warning'
# Without a script, top-level dot entries except .git are linked into home; others are left.
git -C "$work/owner/plain" rm -q script/setup
git -C "$work/owner/plain" -c user.name=t -c user.email=t@example.org commit -qm noscript
mkdir -p "$work/home/.config"
plain_clone="$store/owner/noscript"
git clone -q "$work/owner/plain" "$work/owner/noscript"
run apply "file://$work/owner/noscript" 2>"$work/stderr"
[ "$(readlink "$work/home/.vimrc")" = "$plain_clone/.vimrc" ] || fail 'did not link .vimrc'
[ ! -L "$work/home/.git" ] && [ ! -e "$work/home/README.md" ] || fail 'linked .git or a plain entry'
[ -d "$work/home/.config" ] && [ ! -L "$work/home/.config" ] || fail 'replaced an existing ~/.config'
grep -qF "warning: left ~/.config in place; move it away and link it to $plain_clone/.config yourself" "$work/stderr" \
  || fail 'missing occupied warning'
# Re-enable leaves the fallback's own links alone.
run apply 2>/dev/null
run apply "file://$work/owner/noscript" 2>"$work/stderr"
! grep -qF 'left ~/.vimrc' "$work/stderr" || fail 'warned about its own fallback link'
# Moving the source uninstalls the old clone before installing the new one.
: >"$work/home/runs"
run apply "$scripts" 2>"$work/stderr"
grep -qF 'warning: owner/noscript has no uninstall.sh' "$work/stderr" || fail 'source move skipped the uninstall'
runs_are ./install.sh
[ "$(run check "$plain")" = work ] || fail 'check missed a source move'
run apply
# Invalid sources warn without URL text and install nothing.
: >"$work/home/runs"
for url in "https://user:canary-pw@example.invalid/" "https://example.invalid?access_token=/canary/leaf"; do
  run apply "$url" 2>"$work/stderr"
  grep -q 'warning: skipped the source' "$work/stderr" || fail "missing warning for '$url'"
  ! grep -q canary "$work/stderr" || fail 'a warning shows URL text'
  [ "$(run check "$url")" = work ] || fail "check accepted '$url'"
  [ -z "$(run verify "$url")" ] || fail "verify rejected '$url'"
done
[ -z "$(find "$work/home" -name '*canary*')" ] || fail 'a path contains URL text'
runs_are ''
# An unreachable source warns, installs nothing, and check keeps retrying.
run apply "$bad" 2>"$work/stderr"
grep -q 'warning: could not clone missing/other; .*retry yard profile enable user-dotfiles or yard init' "$work/stderr" \
  || fail 'missing clone warning'
[ "$(run check "$bad")" = work ] && [ -z "$(run verify "$bad")" ] || fail 'unreachable source handling'
[ -z "$(find "$store" -name '.tmp-*')" ] || fail 'left a temporary clone after a failure'
[ ! -e "$store/.installed" ] || fail 'recorded an install without a clone'
printf 'ok: user-dotfiles guest hook\n'

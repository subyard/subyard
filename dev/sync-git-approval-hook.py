#!/usr/bin/env python3
"""Sync the shared hook into the standalone provision scripts streamed to guests."""

import argparse
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1] / "config/agents"
BEGIN = "# BEGIN generated Git approval hook"
END = "# END generated Git approval hook"

HELPERS = r'''# Publish each file atomically and preserve an already converged inode.
publish_approval_file() {
  local path="$1" mode="$2" stage
  stage="$(mktemp "$(dirname "$path")/.approval.XXXXXX")"
  cat > "$stage"
  chmod "$mode" "$stage"
  if [ "$(id -u)" = 0 ]; then chown "$expected_owner" "$stage"; fi
  if [ -f "$path" ] && [ ! -L "$path" ] \
    && [ "$(stat -c '%a:%u:%g' "$path")" = "$mode:$expected_owner" ] && cmp -s "$stage" "$path"; then
    rm -f -- "$stage"
  else
    mv -fT -- "$stage" "$path"
  fi
}

reconcile_approval_file() {
  local path="$1" content="$2"
  if [ -n "$content" ]; then
    printf '%s\n' "$content" | publish_approval_file "$path" 644
  else
    rm -f -- "$path"
  fi
}

# Shared with Codex, which passes --plain; Claude needs no extra argument.
# shellcheck disable=SC2120
approval_command() {
  jq -n --args '$ARGS.positional | map(@sh) | join(" ")' -- \
    "$git_hook" "$@" --policies "$git_policy" "$sudo_policy"
}

# A content check can still run when permissions drifted.
approval_file_present() {
  local path="$1" expected="$2" mode="$3" label="$4"
  if [ -z "$expected" ]; then
    [ ! -e "$path" ] && [ ! -L "$path" ] || refuse "$label should be absent; run yard init"
  elif [ -f "$path" ] && [ ! -L "$path" ]; then
    [ "$(stat -c '%a:%u:%g' "$path" 2>/dev/null)" = "$mode:$expected_owner" ] \
      || refuse "$label permissions drifted; run yard init"
    return 0
  else
    refuse "$label is missing; run yard init"
  fi
  return 1
}

check_approval_hash() {
  local actual
  if approval_file_present "$1" "$2" "$3" "$4"; then
    if actual="$(sha256sum 2>/dev/null < "$1")"; then
      [ "${actual%% *}" = "$2" ] || refuse "$4 content drifted; run yard init"
    else
      refuse "$4 could not be read; run yard init"
    fi
  fi
}

check_git_approval_hook() {
  [ -x /usr/bin/python3 ] || refuse 'managed approval hook interpreter is missing; run yard init'
  # The generated policy check assigns the expected hash before calling this helper.
  # shellcheck disable=SC2154
  check_approval_hash "$git_hook" "$git_hook_sha" 755 'managed approval hook'
}
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    hook = (ROOT / "shared/git-approval-hook.py").read_text()
    block = "git_approval_hook() {\n  cat <<'GIT_APPROVAL_HOOK'\n" + hook + "GIT_APPROVAL_HOOK\n}\n"
    block += HELPERS
    stale = []
    for agent in ("claude", "codex"):
        path = ROOT / agent / "provision.sh"
        original = path.read_text()
        before, start, tail = original.partition(BEGIN + "\n")
        _, end, after = tail.partition(END + "\n")
        if not start or not end:
            raise ValueError(f"missing generated hook markers in {path}")
        updated = before + BEGIN + "\n" + block + END + "\n" + after
        if updated != original:
            if args.check:
                stale.append(agent)
            else:
                path.write_text(updated)
    if stale:
        print(f"Run python3 dev/sync-git-approval-hook.py to update {', '.join(stale)}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

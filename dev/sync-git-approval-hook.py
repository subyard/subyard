#!/usr/bin/env python3
"""Sync the shared hook into the standalone provision scripts streamed to guests."""

import argparse
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1] / "config/agents"
BEGIN = "# BEGIN generated Git approval hook"
END = "# END generated Git approval hook"

HELPERS = r'''# Publish the helper before the policy that references it.
install_git_approval_hook() {
  local hook_stage
  [ "$GIT_APPROVAL" = prompt ] || return 0
  hook_stage="$(mktemp "$(dirname "$git_hook")/.git-approval.XXXXXX")"
  git_approval_hook > "$hook_stage"
  chmod 0755 "$hook_stage"
  mv -fT -- "$hook_stage" "$git_hook"
}

check_git_approval_hook() {
  local git_hook="$1" git_hook_sha="$2" expected_owner="$3"
  [ -x /usr/bin/python3 ] || refuse 'managed Git approval hook interpreter is missing; run yard init'
  if [ -f "$git_hook" ] && [ ! -L "$git_hook" ]; then
    [ "$(stat -c '%a:%u:%g' "$git_hook" 2>/dev/null)" = "755:$expected_owner" ] \
      || refuse 'managed Git approval hook permissions drifted; run yard init'
    [ "$(sha256sum < "$git_hook" | cut -d' ' -f1)" = "$git_hook_sha" ] \
      || refuse 'managed Git approval hook content drifted; run yard init'
  else
    refuse 'managed Git approval hook is missing; run yard init'
  fi
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

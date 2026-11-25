#!/usr/bin/env python3
"""Compare the Go string literals of non-test code between a base and a head.

A behavior-preserving refactor slice moves message literals with the code and edits none.
This counts every string literal as written, skipping comments, rune literals and import
paths, and lists each literal whose count differs. Exit 1 when any differs.
"""
import argparse
import collections
import os
from pathlib import Path
import re
import subprocess
import sys

DEFAULT_DIRS = ("internal/cli", "internal/application")
# Go tokens that can contain a quote, plus words and parentheses for import declarations.
TOKEN = re.compile(r'//[^\n]*|/\*.*?\*/|"(?:[^"\\\n]|\\.)*"|`[^`]*`|\'(?:[^\'\\\n]|\\.)*\'|\w+|[()]', re.S)


def literals(source):
    """Yield the string literals of Go source as written, without import paths."""
    imports = None  # None, "pending" after the keyword, or "block" inside import ( ... )
    for match in TOKEN.finditer(source):
        token = match.group()
        if token.startswith(("//", "/*", "'")):
            continue
        if token[0] in "\"`":
            if imports == "pending":
                imports = None
            elif imports is None:
                yield token
        elif token == "import":
            imports = "pending"
        elif token == "(" and imports == "pending":
            imports = "block"
        elif token == ")" and imports == "block":
            imports = None


def git(*arguments):
    return subprocess.run(["git", *arguments], check=True, stdout=subprocess.PIPE).stdout


def counted(paths, read):
    counts = collections.Counter()
    for path in paths:
        if path.endswith(".go") and not path.endswith("_test.go"):
            counts.update(literals(read(path).decode(errors="surrogateescape")))
    return counts


def at_revision(revision, dirs):
    paths = git("ls-tree", "-r", "-z", "--name-only", revision, "--", *dirs).decode().split("\0")
    return counted(filter(None, paths), lambda path: git("show", f"{revision}:{path}"))


def in_worktree(dirs):
    paths = git("ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", *dirs).decode().split("\0")
    return counted((path for path in paths if os.path.isfile(path)), lambda path: Path(path).read_bytes())


def shown(token):
    return repr(token) if "\n" in token else token


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="origin/master", help="base revision (default: origin/master)")
    parser.add_argument("--head", help="head revision (default: the working tree, tracked and untracked files)")
    parser.add_argument("dirs", nargs="*", default=DEFAULT_DIRS,
                        help=f"directories to compare (default: {' '.join(DEFAULT_DIRS)})")
    args = parser.parse_args(argv)
    os.chdir(git("rev-parse", "--show-toplevel").decode().strip())
    revisions = [args.base] + ([args.head] if args.head else [])
    unresolved = [revision for revision in revisions
                  if subprocess.run(["git", "rev-parse", "--verify", "--quiet", f"{revision}^{{commit}}"],
                                    stdout=subprocess.DEVNULL).returncode]
    if unresolved:
        for revision in unresolved:
            print(f"litdiff: refused: cannot resolve revision {revision!r}", file=sys.stderr)
        return 2
    before = at_revision(args.base, args.dirs)
    after = at_revision(args.head, args.dirs) if args.head else in_worktree(args.dirs)
    removed, added = before - after, after - before
    print(f"literals: {before.total()} before, {after.total()} after; "
          f"{removed.total()} removed, {added.total()} added")
    for token, count in sorted(removed.items()):
        print(f"  -{count} {shown(token)}")
    for token, count in sorted(added.items()):
        print(f"  +{count} {shown(token)}")
    return 1 if removed or added else 0


if __name__ == "__main__":
    sys.exit(main())

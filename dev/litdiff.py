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


def go_files(listing):
    return [path for path in listing.decode().split("\0") if path.endswith(".go") and not path.endswith("_test.go")]


def counted(paths, read):
    counts = collections.Counter()
    for path in paths:
        counts.update(literals(read(path).decode(errors="surrogateescape")))
    return counts


def shown(token):
    return repr(token) if "\n" in token else token


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="origin/master", help="base revision (default: origin/master)")
    parser.add_argument("--head", help="head revision (default: the working tree, tracked and untracked files)")
    parser.add_argument("dirs", nargs="*", help="directories relative to the current directory "
                        f"(default: {' '.join(DEFAULT_DIRS)} at the repository root)")
    args = parser.parse_args(argv)
    top = git("rev-parse", "--show-toplevel").decode().strip()
    given = args.dirs or DEFAULT_DIRS
    dirs = [os.path.relpath(os.path.realpath(path), top) for path in args.dirs] or list(DEFAULT_DIRS)
    os.chdir(top)
    refusals = [f"directory {name!r} is outside the repository"
                for name, path in zip(given, dirs) if path.split(os.sep)[0] == ".."]
    revisions = [args.base] + ([args.head] if args.head else [])
    refusals += [f"cannot resolve revision {revision!r}" for revision in revisions
                 if subprocess.run(["git", "rev-parse", "--verify", "--quiet", f"{revision}^{{commit}}"],
                                   stdout=subprocess.DEVNULL).returncode]
    if not refusals:
        base = go_files(git("ls-tree", "-r", "-z", "--name-only", args.base, "--", *dirs))
        head = go_files(git("ls-tree", "-r", "-z", "--name-only", args.head, "--", *dirs) if args.head else
                        git("ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", *dirs))
        if not args.head:
            head = [path for path in head if os.path.isfile(path)]
        refusals += [f"no non-test Go files under {name!r}" for name, path in zip(given, dirs)
                     if not any(path == "." or file.startswith(path + "/") for file in base + head)]
    if refusals:
        for reason in refusals:
            print(f"litdiff: refused: {reason}", file=sys.stderr)
        return 2
    before = counted(base, lambda path: git("show", f"{args.base}:{path}"))
    after = counted(head, lambda path: git("show", f"{args.head}:{path}") if args.head else Path(path).read_bytes())
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

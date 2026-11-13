#!/usr/bin/env python3
"""Print the pull request statistics block for BASE...HEAD; Git supplies every number."""
import argparse
import fnmatch
import subprocess
import sys

AREAS = ("product", "tests", "fixtures and tools", "documentation")
# First match wins; `*` also matches `/`. A path that matches no rule is refused.
AREA_RULES = (
    ("fixtures and tools", ("tests/fixtures/*",)),
    ("tests", ("tests/*", "*/tests/*", "dev/e2e/*", "*_test.go", "*-test.py", "*-test.sh", "*-test.mjs",
               "*_tests.rs")),
    ("product", ("config/skills/*", "config/*SKILL.md")),
    ("fixtures and tools", ("*/testdata/*", "*/fixtures/*", "dev/*", "internal/testkit/*", ".github/*",
                            "Makefile", ".golangci.yml", ".gitignore", "rust-toolchain.toml", "sync-repos.sh")),
    ("documentation", (".agents/*", ".claude/*", "docs/*", "*.md", "LICENSE")),
    ("product", ("cmd/*", "internal/*", "scripts/*", "config/*", "bin/*", "api/*", "completions/*",
                 "veranda/*", "go.mod", "go.sum")),
)


def area(path):
    for name, patterns in AREA_RULES:
        if any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns):
            return name
    return None


def one_line(text):
    return " ".join(text.split()).replace("|", "\\|")


def pairs(values, label, refusals):
    result = {}
    for value in values:
        name, separator, text = value.partition("=")
        if not separator or not one_line(name) or not one_line(text):
            refusals.append(f"{label} must be NAME=TEXT: {one_line(value)!r}")
        elif one_line(name) in result:
            refusals.append(f"duplicate {label} name {one_line(name)!r}")
        else:
            result[one_line(name)] = one_line(text)
    return result


def git(*arguments):
    result = subprocess.run(["git", *arguments], text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return result.stdout if result.returncode == 0 else None


def numstat(base):
    """Lines per area over base...HEAD, plus unclassified paths; None if Git cannot diff."""
    output = git("diff", "--numstat", "-z", "-M", f"{base}...HEAD")
    if output is None:
        return None, []
    totals, unclassified, fields, index = {name: [0, 0] for name in AREAS}, [], output.split("\0"), 0
    while index < len(fields) and fields[index]:
        added, removed, path = fields[index].split("\t", 2)
        index += 1
        if path == "":  # A rename lists its old and new paths as separate fields.
            path, index = fields[index + 1], index + 2
        name = area(path)
        if name is None:
            unclassified.append(path)
            continue
        totals[name][0] += 0 if added == "-" else int(added)
        totals[name][1] += 0 if removed == "-" else int(removed)
    return totals, unclassified


def render(head, totals, review_cycles, checks, purposes):
    lines = ["### Statistics", "", f"Computed for `{head}`.", "",
             "| Area | Added | Removed | Purpose |", "| --- | ---: | ---: | --- |"]
    lines += [f"| {name} | {totals[name][0]} | {totals[name][1]} | {purposes.get(name, '')} |"
              for name in AREAS if any(totals[name])]
    lines += [f"| total | {sum(v[0] for v in totals.values())} | {sum(v[1] for v in totals.values())} | |",
              "", f"Review cycles: {review_cycles}", "", "Checks:"]
    lines += [f"- {name}: {url}" for name, url in checks.items()] or ["- none"]
    return "\n".join(lines) + "\n"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True, help="base ref, for example origin/master")
    parser.add_argument("--review-cycles", required=True)
    parser.add_argument("--check", action="append", default=[], help="NAME=run URL; repeatable")
    parser.add_argument("--purpose", action="append", default=[], help="AREA=one-line purpose; repeatable")
    args = parser.parse_args(argv)
    refusals = []
    if not args.review_cycles.isdigit():
        refusals.append("--review-cycles must be a non-negative integer")
    checks = pairs(args.check, "--check", refusals)
    refusals += [f"--check {name} needs an https URL" for name, url in checks.items() if not url.startswith("https://")]
    purposes = pairs(args.purpose, "--purpose", refusals)
    refusals += [f"unknown --purpose area {name!r}" for name in purposes if name not in AREAS]
    status = git("status", "--porcelain", "--untracked-files=normal")
    if status is None or status.strip():
        refusals.append("working tree is not clean; statistics describe committed HEAD only")
    head = (git("rev-parse", "--verify", "HEAD^{commit}") or "").strip()
    totals = None
    if not head or git("rev-parse", "--verify", "--quiet", f"{args.base}^{{commit}}") is None:
        refusals.append(f"cannot resolve HEAD and base {one_line(args.base)!r}")
    else:
        totals, unclassified = numstat(args.base)
        if totals is None:
            refusals.append(f"cannot diff {one_line(args.base)}...HEAD")
        refusals += [f"unclassified path {path!r}; add an area rule" for path in unclassified]
        refusals += [f"missing --purpose for changed area {name!r}" for name in AREAS
                     if totals and any(totals[name]) and name not in purposes]
    if refusals:
        for reason in refusals:
            print(f"pr-stats: refused: {reason}", file=sys.stderr)
        return 1
    sys.stdout.write(render(head, totals, args.review_cycles, checks, purposes))
    return 0


if __name__ == "__main__":
    sys.exit(main())

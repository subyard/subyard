"""Proof helpers for dev/cli-fixture-slice-check.sh; see that script for the contract.

Go test exit statuses decide success; go test -json output is read only for test names.
"""
import json
import re
import sys
from pathlib import Path

TEST_NAME = re.compile(r"^(Test|Fuzz|Example|Benchmark)\w*$")


def read_rows(path, fields):
    """Yield (line number, fields) of a tab-separated file; # starts a comment."""
    for number, line in enumerate(Path(path).read_text().splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        row = line.split("\t")
        if len(row) != fields:
            sys.exit(f"{path}:{number}: want {fields} tab-separated fields, got {len(row)}")
        yield number, row


def passed_names(*paths):
    """Return the names of the tests that passed in go test -json files."""
    names = set()
    for path in paths:
        for line in Path(path).read_text().splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get("Test") and event.get("Action") == "pass":
                names.add(event["Test"])
    return names


def split_run_pattern(pattern):
    """Split a -run pattern on the slashes go test splits on: outside brackets,
    parentheses and escapes.

    A top-level | together with a top-level / would make go test alternate whole
    per-level elements, which this matcher does not model; such a pattern is refused.
    """
    elements, current, depth, in_class, escaped, alternation = [], "", 0, False, False, False
    for char in pattern:
        if escaped:
            escaped = False
        elif char == "\\":
            escaped = True
        elif in_class:
            in_class = char != "]"
        elif char == "[":
            in_class = True
        elif char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
        elif char == "|" and depth == 0:
            alternation = True
        elif char == "/" and depth == 0:
            elements.append(current)
            current = ""
            continue
        current += char
    if alternation and elements:
        raise ValueError(f"-run pattern {pattern!r} has a top-level | across subtest levels; "
                         "group the alternatives inside one level")
    return [re.compile(element) for element in elements + [current]]


def selected(name, patterns):
    """Report whether go test selects name: each element of its slash-separated
    name matches the pattern element at the same level, where one exists."""
    return all(pattern.search(element) for pattern, element in zip(patterns, name.split("/")))


def failed_selected(path, pattern):
    """Return the selected tests that a go test -json run reports as failed.

    A failure before any test ran, such as a fork/exec error, names none.
    """
    patterns = split_run_pattern(pattern)
    names = set()
    for line in Path(path).read_text().splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        test = event.get("Test")
        if test and event.get("Action") == "fail" and selected(test, patterns):
            names.add(test)
    return names


def tests(base_list, candidate_list, base_runs, candidate_runs, allowed):
    """Proof 1: the test lists and pass sets are equal except for allowed added
    tests, which must pass. The caller has checked that every run exited 0."""
    allow = [re.compile(pattern) for pattern in allowed]
    ok = True

    def names(path):
        return {line.strip() for line in Path(path).read_text().splitlines() if TEST_NAME.match(line.strip())}

    def allowed_name(name):
        return any(pattern.search(name.split("/")[0]) for pattern in allow)

    before, after = names(base_list), names(candidate_list)
    print(f"test list: base {len(before)}, candidate {len(after)}")
    for name in sorted(before - after):
        ok = False
        print(f"  FAIL removed test {name}")
    added = after - before
    for name in sorted(added):
        if allowed_name(name):
            print(f"  added test {name} (allowed)")
        else:
            ok = False
            print(f"  FAIL added test {name}")
    base_passed, candidate_passed = passed_names(*base_runs), passed_names(*candidate_runs)
    print(f"pass set: base {len(base_passed)}, candidate {len(candidate_passed)}")
    for name in sorted(base_passed - candidate_passed):
        ok = False
        print(f"  FAIL no longer passes {name}")
    for name in sorted(candidate_passed - base_passed):
        if name.split("/")[0] in added and allowed_name(name):
            print(f"  newly passes {name} (allowed)")
        else:
            ok = False
            print(f"  FAIL newly passes {name}")
    for name in sorted(added - candidate_passed):
        ok = False
        print(f"  FAIL added test {name} did not pass")
    return ok


def coverage(base_profile, *candidate_profiles):
    """Proof 3: the non-test blocks covered on the base stay covered.

    Blocks behind a lost race are covered only in some runs, so a candidate
    rerun that exited 0 counts; every block that only a rerun covered is printed.
    """
    def covered(path):
        blocks = {}
        for line in Path(path).read_text().splitlines()[1:]:
            location, _, count = line.rpartition(" ")
            block = location.rpartition(" ")[0]
            if "/internal/testkit/" in block or "_test.go:" in block:
                continue
            blocks[block] = max(blocks.get(block, 0), int(count))
        return {block for block, count in blocks.items() if count > 0}, len(blocks)
    before, total_before = covered(base_profile)
    runs = [covered(path) for path in candidate_profiles]
    first, total_after = runs[0]
    after = set().union(*(blocks for blocks, _ in runs))
    lost = sorted(before - after)
    print(f"covered blocks: base {len(before)}/{total_before}, candidate {len(first)}/{total_after} "
          f"in {len(runs)} run(s), lost {len(lost)}, gained {len(after - before)}")
    for block in sorted((before - first) & after):
        print(f"  covered only by a candidate rerun: {block}")
    for block in lost[:100]:
        print(f"  FAIL lost {block}")
    return not lost


def unescape(text):
    return re.sub(r"\\(.)", lambda match: {"n": "\n", "t": "\t", "\\": "\\"}.get(match.group(1), match.group(0)), text)


def mutants(path, candidate, changed, select_all):
    """Validate the mutant list and print the selected rows NUL-framed.

    Every row must name existing files and every changed test file needs a row.
    Diagnostics go to stderr; the exit status reports a validation failure.
    """
    ok, selected, covered = True, [], set()
    for number, row in read_rows(path, 5):
        test_file, _, target = row[:3]
        missing = [name for name in (test_file, target) if not (Path(candidate) / name).is_file()]
        if missing:
            ok = False
            print(f"  FAIL mutant line {number} names missing files {missing}", file=sys.stderr)
            continue
        try:
            split_run_pattern(row[1])
        except (ValueError, re.error) as error:
            ok = False
            print(f"  FAIL mutant line {number} is invalid: {error}", file=sys.stderr)
            continue
        if not select_all and test_file not in changed:
            print(f"  skipped  mutant line {number}: {test_file} is unchanged", file=sys.stderr)
            continue
        covered.add(test_file)
        selected.append([str(number), *row])
    for name in sorted(set(changed) - covered):
        ok = False
        print(f"  FAIL changed test file {name} has no mutant", file=sys.stderr)
    for row in selected:
        sys.stdout.write("".join(field + "\0" for field in row))
    return ok


def apply(source, old, new):
    text = Path(source).read_text()
    old, new = unescape(old), unescape(new)
    if text.count(old) != 1:
        sys.exit(f"mutant text occurs {text.count(old)} times in {source}")
    Path(source).write_text(text.replace(old, new))


def main(arguments):
    command, rest = arguments[0], arguments[1:]
    if command == "tests":
        # tests BASE_LIST CANDIDATE_LIST BASE_JSON,... CANDIDATE_JSON,... [ALLOWED...]
        return 0 if tests(rest[0], rest[1], rest[2].split(","), rest[3].split(","), rest[4:]) else 1
    if command == "coverage":
        return 0 if coverage(*rest) else 1
    if command == "mutants":
        return 0 if mutants(rest[0], rest[1], set(rest[3:]), rest[2] == "all") else 1
    if command == "apply":
        apply(*rest[:3])
        return 0
    if command == "failed":
        names = failed_selected(rest[0], rest[1])
        print(" ".join(sorted(names)))
        return 0 if names else 1
    if command == "passed":
        print(len(passed_names(rest[0])))
        return 0
    sys.exit(f"unknown command {command}")


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

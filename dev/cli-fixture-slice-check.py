"""Proof helpers for dev/cli-fixture-slice-check.sh; see that script for the contract."""
import collections
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


def outcomes(path):
    """Return the final test actions and whether the package failed without a failing test."""
    result, package_failed = {}, False
    for line in Path(path).read_text().splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
            result[event["Test"]] = event["Action"]
        elif not event.get("Test") and event.get("Action") in ("fail", "build-fail"):
            package_failed = True
    return result, package_failed


def tests(base_list, candidate_list, base_json, candidate_json, base_plain, candidate_plain, plain_names, allowed):
    """Proof 1: the test lists and pass sets are equal except for allowed added tests, which must pass."""
    allow = [re.compile(pattern) for pattern in allowed]
    plain = set(plain_names)
    ok = True

    def names(path):
        return {line.strip() for line in Path(path).read_text().splitlines() if TEST_NAME.match(line.strip())}

    def merged(covered_path, plain_path):
        covered, package_failed = outcomes(covered_path)
        uncovered, plain_failed = outcomes(plain_path) if plain else ({}, False)
        result = {name: action for name, action in covered.items() if name.split("/")[0] not in plain}
        result.update({name: action for name, action in uncovered.items() if name.split("/")[0] in plain})
        # A failing test already fails the package; only an unexplained package failure counts here.
        covered_explained = any(action == "fail" for action in covered.values())
        return result, (package_failed and not covered_explained) or (plain_failed and not uncovered)

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
    for name in sorted(plain):
        print(f"  run without coverage: {name}")
    (before, base_broken), (after, candidate_broken) = merged(base_json, base_plain), merged(candidate_json, candidate_plain)
    for side, broken in (("base", base_broken), ("candidate", candidate_broken)):
        if broken:
            ok = False
            print(f"  FAIL {side} package failed without a failing test (build, panic or timeout)")
    passed = lambda outcome: {name for name, action in outcome.items() if action == "pass"}  # noqa: E731
    print(f"pass set: base {len(passed(before))}, candidate {len(passed(after))}")
    for side, outcome in (("base", before), ("candidate", after)):
        for name in sorted(name for name, action in outcome.items() if action == "fail"):
            ok = False
            print(f"  FAIL {side} failed {name}")
    for name in sorted(passed(before) - passed(after)):
        ok = False
        print(f"  FAIL no longer passes {name} ({after.get(name, 'missing')})")
    for name in sorted(passed(after) - passed(before)):
        if name.split("/")[0] in added and allowed_name(name):
            print(f"  newly passes {name} (allowed)")
        else:
            ok = False
            print(f"  FAIL newly passes {name} ({before.get(name, 'missing')})")
    for name in sorted(plain):
        for side, outcome in (("base", before), ("candidate", after)):
            if outcome.get(name) != "pass":
                ok = False
                print(f"  FAIL {side} did not pass {name} without coverage ({outcome.get(name, 'missing')})")
    for name in sorted(added):
        if after.get(name) != "pass":
            ok = False
            print(f"  FAIL added test {name} did not pass ({after.get(name, 'missing')})")
    return ok


ASSERT = re.compile(r"\bt\.(?:Fatal|Fatalf|Error|Errorf|Fail|FailNow)\(|\.AssertCalls\(")
FUNC = re.compile(r"^func (?:\(([^)]*)\) )?(\w+)")
ASSIGN = re.compile(r"(?:^|[\s,(])err\s*:?=\s*(.*)")
ERROR_CHECK = re.compile(r"^if err != nil \{ \[([^\]]+)\] => t\.Fatal\(err\)$")


def normalize(text):
    return re.sub(r"\s+", " ", text).strip()


def indent(line):
    return len(line) - len(line.lstrip("\t"))


def callee(expression):
    match = re.match(r"\s*&?([A-Za-z_][\w.]*)\(", expression)
    return match.group(1) if match else "<expression>"


def statement(lines, start):
    """Return the normalized statement that starts at lines[start], up to balanced parentheses."""
    text, depth = [], 0
    for line in lines[start:]:
        text.append(line)
        depth += line.count("(") - line.count(")")
        if depth <= 0:
            break
    return normalize(" ".join(text))


def functions(directory):
    """Map each test-file function to its assertion units and normalized body.

    A unit is the guard that encloses an assertion, normalized, then " => " and
    the assertion statement. An `if err != nil {` guard names the callee whose
    error it checks. A statement at function level has the guard <func>.
    """
    result = {}
    for path in sorted(Path(directory).glob("*_test.go")):
        lines = path.read_text().splitlines()
        index = 0
        while index < len(lines):
            match = FUNC.match(lines[index])
            if not match:
                index += 1
                continue
            receiver = match.group(1).split()[-1:] if match.group(1) else []
            key = (receiver[0] + "." if receiver else "") + match.group(2)
            end = index
            while end < len(lines) and lines[end] != "}" and not (end == index and lines[end].endswith("}")):
                end += 1
            units = []
            for line_index in range(index, end + 1):
                if not ASSERT.search(lines[line_index]):
                    continue
                level, start = indent(lines[line_index]), line_index - 1
                while start > index and (not lines[start].strip() or indent(lines[start]) >= level):
                    start -= 1
                stop = start
                while stop < line_index - 1 and not lines[stop].rstrip().endswith("{"):
                    stop += 1
                guard = normalize(" ".join(lines[start:stop + 1])) if start > index else "<func>"
                if guard == "if err != nil {":
                    source = "<unknown>"
                    for previous in reversed(lines[index:start]):
                        assigned = ASSIGN.search(previous)
                        if assigned:
                            source = callee(assigned.group(1))
                            break
                    guard = f"if err != nil {{ [{source}]"
                units.append((guard + " => " + statement(lines, line_index), f"{path.name}:{line_index + 1}"))
            result[key] = (units, normalize(" ".join(lines[index:end + 1])))
            index = end + 1
    return result


def asserts(base_dir, candidate_dir, renames_path, waivers_path, replacements):
    """Proof 2: every base assertion unit survives unchanged, renamed by a reviewed
    rule, as an error check replaced by its named helper, or under a reviewed waiver."""
    before, after = functions(base_dir), functions(candidate_dir)
    renames = [(number, re.compile(pattern), replacement) for number, (pattern, replacement) in read_rows(renames_path, 2)]
    waivers = collections.defaultdict(list)
    for number, (function, unit, target, replacement, reason) in read_rows(waivers_path, 5):
        waivers[(function, normalize(unit))].append([number, target, normalize(replacement), reason, False])
    allowed = dict(item.split("=", 1) for item in replacements)
    budgets = collections.Counter()

    def spend(function, text):
        """Consume one occurrence of text that the candidate function adds over its base."""
        base_body = before.get(function, ([], ""))[1]
        candidate_body = after.get(function, ([], ""))[1]
        available = candidate_body.count(text) - base_body.count(text) - budgets[(function, text)]
        if available <= 0:
            return False
        budgets[(function, text)] += 1
        return True

    ok, counts = True, collections.Counter()
    total_before = sum(len(units) for units, _ in before.values())
    total_after = sum(len(units) for units, _ in after.values())
    for function, (units, _) in sorted(before.items()):
        remaining = collections.Counter(unit for unit, _ in after.get(function, ([], ""))[0])
        for unit, location in units:
            if remaining[unit] > 0:
                remaining[unit] -= 1
                counts["unchanged"] += 1
                continue
            renamed, rules = unit, []
            for number, pattern, replacement in renames:
                renamed, changes = pattern.subn(replacement, renamed)
                if changes:
                    rules.append(str(number))
            if rules and remaining[renamed] > 0:
                remaining[renamed] -= 1
                counts["renamed"] += 1
                print(f"  renamed {function} ({location}) by rule {','.join(rules)}: {unit}")
                continue
            check = ERROR_CHECK.match(unit)
            if check and check.group(1) in allowed and spend(function, allowed[check.group(1)]):
                counts["replaced error checks"] += 1
                print(f"  replaced {function} ({location}): error check of {check.group(1)} by {allowed[check.group(1)]}")
                continue
            waiver = next((row for row in waivers.get((function, unit), []) if not row[4]), None)
            if waiver and spend(waiver[1], waiver[2]):
                waiver[4] = True
                counts["waived"] += 1
                print(f"  waived {function} ({location}) by waiver line {waiver[0]}: {unit}")
                print(f"    replacement in {waiver[1]}: {waiver[2]} ({waiver[3]})")
                continue
            ok = False
            print(f"  FAIL removed {function} ({location}): {unit}")
            if waiver:
                print(f"    waiver line {waiver[0]} names a replacement that {waiver[1]} does not add: {waiver[2]}")
    for rows in waivers.values():
        for number, target, replacement, _, used in rows:
            if not used:
                ok = False
                print(f"  FAIL waiver line {number} matches no removed unit")
    summary = ", ".join(f"{name} {counts[name]}" for name in ("unchanged", "renamed", "replaced error checks", "waived"))
    print(f"assertion units: base {total_before}, candidate {total_after}; {summary}")
    return ok


def coverage(base_profile, candidate_profile):
    """Proof 3: the non-test blocks covered on the base stay covered."""
    def covered(path):
        blocks = {}
        for line in Path(path).read_text().splitlines()[1:]:
            location, _, count = line.rpartition(" ")
            block = location.rpartition(" ")[0]
            if "/internal/testkit/" in block or "_test.go:" in block:
                continue
            blocks[block] = max(blocks.get(block, 0), int(count))
        return {block for block, count in blocks.items() if count > 0}, len(blocks)
    (before, total_before), (after, total_after) = covered(base_profile), covered(candidate_profile)
    lost = sorted(before - after)
    print(f"covered blocks: base {len(before)}/{total_before}, candidate {len(after)}/{total_after}, lost {len(lost)}, gained {len(after - before)}")
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
        test_file, _, product = row[:3]
        missing = [name for name in (test_file, product) if not (Path(candidate) / name).is_file()]
        if missing:
            ok = False
            print(f"  FAIL mutant line {number} names missing files {missing}", file=sys.stderr)
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


def verdict(path):
    """Classify a go test -json run: passed, failed, invalid (did not build) or no-tests."""
    failed = ran = 0
    broken = False
    for line in Path(path).read_text().splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            broken = broken or "build failed" in line or "setup failed" in line
            continue
        broken = broken or event.get("Action") == "build-fail" or "[build failed]" in event.get("Output", "")
        if event.get("Test") and event.get("Action") == "fail":
            failed += 1
        if event.get("Test") and event.get("Action") in ("pass", "fail"):
            ran += 1
    if broken:
        print("invalid")
    elif failed:
        print("failed")
    else:
        print("passed" if ran else "no-tests")


def main(arguments):
    command, rest = arguments[0], arguments[1:]
    if command == "tests":
        plain = rest[6].split(",") if rest[6] else []
        return 0 if tests(*rest[:6], plain, rest[7:]) else 1
    if command == "asserts":
        return 0 if asserts(rest[0], rest[1], rest[2], rest[3], rest[4:]) else 1
    if command == "coverage":
        return 0 if coverage(rest[0], rest[1]) else 1
    if command == "mutants":
        return 0 if mutants(rest[0], rest[1], set(rest[3:]), rest[2] == "all") else 1
    if command == "apply":
        apply(*rest[:3])
        return 0
    if command == "verdict":
        verdict(rest[0])
        return 0
    sys.exit(f"unknown command {command}")


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

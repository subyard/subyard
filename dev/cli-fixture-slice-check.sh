#!/usr/bin/env bash
# Prove that a CLI test fixture migration slice keeps its coverage, against a base ref:
#   1. the go test -list names and the -json pass set are identical;
#   2. the only removed assertions are error checks of New (or of --allow-error-check callees);
#   3. the coverprofile blocks of non-test code covered on the base stay covered;
#   4. every listed mutant of a changed test file makes its tests fail.
# Both sides run from exports in /tmp without Git metadata: the base from git archive,
# the candidate from the current tracked and untracked, non-ignored files.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export TMPDIR=/tmp

usage() {
  cat <<'EOF'
Usage: dev/cli-fixture-slice-check.sh [options]
  --base REF                 base ref (default: origin/master)
  --package PKG              Go package to check (default: ./internal/cli)
  --mutants FILE             mutant list (default: dev/cli-fixture-mutants.tsv)
  --all-mutants              run every listed mutant, not only those of changed test files
  --skip-mutants             skip proof 4
  --allow-added-test REGEX   accept added top-level tests matching REGEX (repeatable)
  --allow-error-check CALLEE accept removed `if err != nil { t.Fatal(err) }` checks of CALLEE (repeatable)
  --keep                     keep the work directory
Source admission tests need a normal runtime: SUBYARD_SOURCE_QUERY_TOOLING_ROOT, or
.build/runtime/current from `make build VERSION=0.29.0-test`.

Mutant list: tab-separated test file, -run regex, product file, old text, new text; old
must occur exactly once. In the text fields \n, \t and \\ are escapes. # starts a comment.
EOF
}

base=origin/master package=./internal/cli mutants="$ROOT/dev/cli-fixture-mutants.tsv"
all_mutants=0 skip_mutants=0 keep=0
allowed_tests=() allowed_checks=(New)
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base) base="${2:?}"; shift 2 ;;
    --package) package="${2:?}"; shift 2 ;;
    --mutants) mutants="${2:?}"; shift 2 ;;
    --all-mutants) all_mutants=1; shift ;;
    --skip-mutants) skip_mutants=1; shift ;;
    --allow-added-test) allowed_tests+=("${2:?}"); shift 2 ;;
    --allow-error-check) allowed_checks+=("${2:?}"); shift 2 ;;
    --keep) keep=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
case "$package" in ./*) ;; *) printf 'FAIL: --package must be a ./relative package\n' >&2; exit 2 ;; esac

tooling="${SUBYARD_SOURCE_QUERY_TOOLING_ROOT:-$ROOT/.build/runtime/current}"
[ -e "$tooling/bin/yard-engine" ] \
  || { printf 'FAIL: no normal runtime at %s; run make build VERSION=0.29.0-test\n' "$tooling" >&2; exit 2; }
export SUBYARD_SOURCE_QUERY_TOOLING_ROOT
SUBYARD_SOURCE_QUERY_TOOLING_ROOT="$(cd "$tooling" && pwd -P)"
base_commit="$(git -C "$ROOT" rev-parse --verify "$base^{commit}")"

work="$(mktemp -d /tmp/cli-fixture-slice.XXXXXX)"
cleanup() {
  if [ "$keep" = 1 ]; then
    printf 'work directory kept: %s\n' "$work"
  else
    chmod -R u+rwX -- "$work" 2>/dev/null || true
    rm -rf -- "$work"
  fi
}
trap cleanup EXIT

mkdir "$work/base" "$work/candidate"
git -C "$ROOT" archive --format=tar "$base_commit" | tar -xf - -C "$work/base"
git -C "$ROOT" ls-files -z --cached --others --exclude-standard \
  | ROOT="$ROOT" python3 -I -c '
import os, sys
root = os.environ["ROOT"]
names = [n for n in sys.stdin.buffer.read().split(b"\0") if n and os.path.lexists(os.path.join(root.encode(), n))]
sys.stdout.buffer.write(b"".join(n + b"\0" for n in names))' \
  | tar -C "$ROOT" --null -T - -cf - | tar -xf - -C "$work/candidate"
printf 'base %s (%s); candidate: current worktree of %s\n' "$base" "$base_commit" "$(git -C "$ROOT" rev-parse --short HEAD)"

cat > "$work/check.py" <<'PYTHON'
import collections, json, re, sys
from pathlib import Path

def tests(base_list, candidate_list, base_json, candidate_json, allowed):
    allow = [re.compile(pattern) for pattern in allowed]
    ok = True
    def names(path):
        return {line.strip() for line in Path(path).read_text().splitlines() if re.match(r"^(Test|Fuzz|Example|Benchmark)\w*$", line.strip())}
    def outcomes(path):
        result = {}
        for line in Path(path).read_text().splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
                result[event["Test"]] = event["Action"]
        return result
    def allowed_name(name):
        return any(pattern.search(name.split("/")[0]) for pattern in allow)
    before, after = names(base_list), names(candidate_list)
    print(f"test list: base {len(before)}, candidate {len(after)}")
    for name in sorted(before - after):
        ok = False; print(f"  FAIL removed test {name}")
    for name in sorted(after - before):
        if allowed_name(name):
            print(f"  added test {name} (allowed)")
        else:
            ok = False; print(f"  FAIL added test {name}")
    before, after = outcomes(base_json), outcomes(candidate_json)
    passed = lambda outcome: {name for name, action in outcome.items() if action == "pass"}
    print(f"pass set: base {len(passed(before))}, candidate {len(passed(after))}")
    for side, outcome in (("base", before), ("candidate", after)):
        for name in sorted(name for name, action in outcome.items() if action == "fail"):
            print(f"  {side} failed {name}")
    for name in sorted(passed(before) - passed(after)):
        ok = False; print(f"  FAIL no longer passes {name} ({after.get(name, 'missing')})")
    for name in sorted(passed(after) - passed(before)):
        if allowed_name(name):
            print(f"  newly passes {name} (allowed)")
        else:
            ok = False; print(f"  FAIL newly passes {name} ({before.get(name, 'missing')})")
    return ok

ASSERT = re.compile(r"\bt\.(?:Fatal|Fatalf|Error|Errorf|Fail|FailNow)\(|\.AssertCalls\(")
FUNC = re.compile(r"^func (?:\(([^)]*)\) )?(\w+)")
ASSIGN = re.compile(r"(?:^|[\s,(])err\s*:?=\s*(.*)")
INLINE = re.compile(r"^\s*if (?:[\w, ]*\berr\s*:?=\s*)(.*);\s*err != nil \{$")

def callee(expression):
    match = re.match(r"\s*&?([A-Za-z_][\w.]*)\(", expression)
    return match.group(1) if match else "<expression>"

def functions(directory):
    result = {}
    for path in sorted(Path(directory).glob("*_test.go")):
        lines = path.read_text().splitlines()
        index = 0
        while index < len(lines):
            match = FUNC.match(lines[index])
            if not match:
                index += 1
                continue
            receiver = (match.group(1) or "").split()[-1:] if match.group(1) else []
            key = (receiver[0] + "." if receiver else "") + match.group(2)
            end = index
            while end < len(lines) and lines[end] != "}" and not (end == index and lines[end].endswith("}")):
                end += 1
            body = lines[index:end + 1]
            asserts = sum(len(ASSERT.findall(line)) for line in body)
            checks = collections.Counter()
            for offset, line in enumerate(body):
                if offset + 2 >= len(body) or body[offset + 1].strip() != "t.Fatal(err)" or body[offset + 2].strip() != "}":
                    continue
                inline = INLINE.match(line)
                if inline:
                    checks[callee(inline.group(1))] += 1
                elif line.strip() == "if err != nil {":
                    source = "<unknown>"
                    for previous in reversed(body[:offset]):
                        assigned = ASSIGN.search(previous)
                        if assigned:
                            source = callee(assigned.group(1))
                            break
                    checks[source] += 1
            result[key] = (f"{path.name}:{index + 1}", asserts, checks)
            index = end + 1
    return result

def asserts(base_dir, candidate_dir, allowed):
    ok = True
    before, after = functions(base_dir), functions(candidate_dir)
    removed_checks = collections.Counter()
    observed_before = observed_after = 0
    for key, (location, count, checks) in sorted(before.items()):
        _, after_count, after_checks = after.get(key, ("", 0, collections.Counter()))
        observations = count - sum(checks.values())
        after_observations = after_count - sum(after_checks.values())
        observed_before += observations
        if after_observations < observations:
            ok = False
            print(f"  FAIL {key} ({location}): observation asserts {observations} -> {after_observations}")
        removed_checks.update(checks - after_checks)
    for key, (_, count, checks) in after.items():
        observed_after += count - sum(checks.values())
    print(f"observation asserts: base {observed_before}, candidate {observed_after}")
    for name, count in sorted(removed_checks.items()):
        status = "allowed" if name in allowed else "FAIL"
        ok = ok and name in allowed
        print(f"  removed error checks of {name}: {count} ({status})")
    return ok

def coverage(base_profile, candidate_profile):
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

def mutants(path, changed):
    rows = []
    for number, line in enumerate(Path(path).read_text().splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        fields = line.split("\t")
        if len(fields) != 5:
            sys.exit(f"{path}:{number}: want 5 tab-separated fields, got {len(fields)}")
        if changed is None or fields[0] in changed:
            rows.append([str(number), fields[0], fields[1], fields[2], fields[3], fields[4]])
    for row in rows:
        sys.stdout.write("".join(field + "\0" for field in row))

def apply(source, old, new):
    text = Path(source).read_text()
    old, new = unescape(old), unescape(new)
    if text.count(old) != 1:
        sys.exit(f"mutant text occurs {text.count(old)} times in {source}")
    Path(source).write_text(text.replace(old, new))

def verdict(path):
    failed = ran = 0
    for line in Path(path).read_text().splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("Test") and event.get("Action") == "fail":
            failed += 1
        if event.get("Test") and event.get("Action") in ("pass", "fail"):
            ran += 1
    print("killed" if failed else ("survived" if ran else "no-tests"))

command = sys.argv[1]
if command == "tests":
    sys.exit(0 if tests(*sys.argv[2:6], sys.argv[6:]) else 1)
if command == "asserts":
    sys.exit(0 if asserts(sys.argv[2], sys.argv[3], set(sys.argv[4:])) else 1)
if command == "coverage":
    sys.exit(0 if coverage(sys.argv[2], sys.argv[3]) else 1)
if command == "mutants":
    mutants(sys.argv[2], None if sys.argv[3] == "-" else set(sys.argv[4:]))
elif command == "apply":
    apply(*sys.argv[2:5])
elif command == "verdict":
    verdict(sys.argv[2])
PYTHON
check() { python3 -I "$work/check.py" "$@"; }

status=0
for side in base candidate; do
  go -C "$work/$side" test -list '.*' "$package" > "$work/$side.list"
  printf 'running %s %s with coverage\n' "$side" "$package"
  go -C "$work/$side" test -count=1 -timeout=30m -json -coverpkg=./... -coverprofile="$work/$side.cover" \
    "$package" > "$work/$side.json" || true
  [ -s "$work/$side.cover" ] || { printf 'FAIL: %s produced no coverprofile\n' "$side" >&2; exit 1; }
done

printf '\n== proof 1: test list and pass set\n'
if check tests "$work/base.list" "$work/candidate.list" "$work/base.json" "$work/candidate.json" "${allowed_tests[@]}"; then
  printf 'PASS\n'
else status=1; printf 'FAIL\n'; fi

printf '\n== proof 2: removed assertions are only error checks of %s\n' "${allowed_checks[*]}"
if check asserts "$work/base/$package" "$work/candidate/$package" "${allowed_checks[@]}"; then
  printf 'PASS\n'
else status=1; printf 'FAIL\n'; fi

printf '\n== proof 3: covered non-test blocks are equal or a superset\n'
if check coverage "$work/base.cover" "$work/candidate.cover"; then
  printf 'PASS\n'
else status=1; printf 'FAIL\n'; fi

printf '\n== proof 4: mutants\n'
if [ "$skip_mutants" = 1 ]; then
  printf 'skipped\n'
else
  changed=()
  for file in "$work/candidate/$package"/*_test.go; do
    name="${package#./}/$(basename "$file")"
    cmp -s "$file" "$work/base/$name" || changed+=("$name")
  done
  selection=-
  [ "$all_mutants" = 1 ] || selection=changed
  mutant_status=0 count=0
  while IFS= read -r -d '' line && IFS= read -r -d '' test_file && IFS= read -r -d '' pattern \
    && IFS= read -r -d '' product && IFS= read -r -d '' old && IFS= read -r -d '' new; do
    count=$((count + 1))
    target="$work/candidate/$product"
    cp -p -- "$target" "$work/original"
    if ! check apply "$target" "$old" "$new"; then
      result=invalid
    else
      go -C "$work/candidate" test -count=1 -timeout=10m -json -run "$pattern" "$package" > "$work/mutant.json" 2>&1 || true
      result="$(check verdict "$work/mutant.json")"
    fi
    cp -p -- "$work/original" "$target"
    [ "$result" = killed ] || mutant_status=1
    printf '  %-8s %s:%s %s (%s) -run %s\n' "$result" "${mutants#"$ROOT"/}" "$line" "$product" "$test_file" "$pattern"
  done < <(check mutants "$mutants" "$selection" "${changed[@]}")
  printf 'mutants run: %d\n' "$count"
  if [ "$mutant_status" = 0 ] && [ "$count" -gt 0 ]; then
    printf 'PASS\n'
  else status=1; printf 'FAIL\n'; fi
fi
exit "$status"

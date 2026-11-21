#!/usr/bin/env bash
# Host-free contract: CI and Release must use the same real-adapter entrypoint.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNNER='tests/real-host/adapter-contracts.sh'
CI_WORKFLOW="$ROOT/.github/workflows/ci.yml"
RELEASE_WORKFLOW="$ROOT/.github/workflows/release.yml"
DEEP_WORKFLOW="$ROOT/.github/workflows/deep-ci.yml"

fail() {
  printf 'workflow real-adapter gate: %s\n' "$*" >&2
  exit 1
}

[ -x "$ROOT/$RUNNER" ] || fail "shared runner is missing or not executable: $RUNNER"

# Guard the two small host-free workflows, not the publishing workflow.
for workflow in "$CI_WORKFLOW" "$DEEP_WORKFLOW"; do
  [ -f "$workflow" ] || fail "missing host-free workflow: $(basename "$workflow")"
  grep -Fq 'runs-on: ubuntu-24.04' "$workflow" \
    || fail 'host-free workflow must use a standard Ubuntu 24.04 runner'
  grep -Fxq 'permissions:' "$workflow" \
    && grep -Fxq '  contents: read' "$workflow" \
    || fail 'host-free workflow must declare read-only contents permission'
  grep -Fxq '          persist-credentials: false' "$workflow" \
    || fail 'host-free checkout must not persist credentials'
  if grep -Eq 'self-hosted|pull_request_target|secrets[.[]|:[[:space:]]*write|/dev/kvm|dev/agent-e2e|dev/e2e/' "$workflow"; then
    fail 'host-free workflow contains a privileged CI mechanism'
  fi
  mapfile -t actions < <(sed -nE 's/^[[:space:]]*(- )?uses: //p' "$workflow")
  for action in "${actions[@]}"; do
    [[ "$action" =~ ^[^@[:space:]]+@[0-9a-f]{40}([[:space:]]|$) ]] \
      || fail 'host-free actions must use full commit SHAs'
  done
  grep -Eq 'uses: actions/checkout@[0-9a-f]{40}' "$workflow" \
    && grep -Eq 'uses: actions/setup-go@[0-9a-f]{40}' "$workflow" \
    || fail 'host-free workflow must check out sources and set up Go'
  grep -Eq '^[[:space:]]+timeout-minutes: [1-9][0-9]*$' "$workflow" \
    || fail 'host-free workflow must have a bounded timeout'
done
# The compatibility parser must come from the Ubuntu host, not a job image.
! grep -Eq '^[[:space:]]*container:' "$CI_WORKFLOW" \
  || fail 'core CI must use the host systemd 255 parser'
grep -Fxq '  verify:' "$CI_WORKFLOW" && grep -Fxq '  compatibility:' "$CI_WORKFLOW" \
  && grep -Fxq '  deep-go:' "$DEEP_WORKFLOW" \
  || fail 'core and deep CI must use distinct stable job names'

# A push to a release/** branch with an open PR to master triggers both push and pull_request CI.
ci_triggers="$(awk '/^on:$/ {inside=1; next} inside && /^[^[:space:]]/ {exit} inside {print}' "$CI_WORKFLOW")"
[[ "$ci_triggers" == "$(cat <<'YAML'
  push:
    branches:
      - master
      - 'release/**'
  pull_request:
YAML
)" ]] || fail 'core CI must run on master and release branch pushes and unfiltered pull requests'

veranda_workflow="$ROOT/.github/workflows/veranda.yml"
veranda_pr="$(awk '/^  pull_request:$/ {getline; print}' "$veranda_workflow")"
veranda_push="$(awk '/^  push:$/ {getline; print; getline; print; getline; print}' "$veranda_workflow")"
[[ "$veranda_pr" == '    paths: ['* ]] \
  && [[ "$veranda_push" == "$(printf '%s\n' "    branches: [master, 'release/**']" "    tags: ['v*']" "$veranda_pr")" ]] \
  && grep -Fxq '  workflow_dispatch:' "$veranda_workflow" \
  || fail 'Veranda CI must share path filters for PRs and allowed branch pushes and retain tag and manual runs'

grep -Fxq '  workflow_dispatch:' "$DEEP_WORKFLOW" \
  && grep -Fxq '  schedule:' "$DEEP_WORKFLOW" \
  || fail 'deep CI must support manual and nightly runs'
grep -Fxq '        run: make build VERSION=0.29.0-test' "$DEEP_WORKFLOW" \
  && grep -Fxq '        run: umask 0022 && go test -race -shuffle=on -count=2 -timeout=60m ./...' "$DEEP_WORKFLOW" \
  && grep -Fxq "        run: go test ./internal/command -run '^\$' -fuzz '^FuzzParseDoesNotPanic\$' -fuzztime=60s" "$DEEP_WORKFLOW" \
  && grep -Fxq "        run: go test ./internal/observation -run '^\$' -fuzz '^FuzzValidateJSON\$' -fuzztime=60s" "$DEEP_WORKFLOW" \
  || fail 'deep CI must build the runtime, run shuffled repeated race tests and bounded fuzzing'

for workflow in "$CI_WORKFLOW" "$RELEASE_WORKFLOW"; do
  native_engine_job="$(awk '/^  native-engine-arm64:/ {inside=1; next} inside && /^  [^[:space:]]/ {exit} inside {print}' "$workflow")"
  grep -Fxq '    runs-on: ubuntu-24.04-arm' <<<"$native_engine_job" \
    && grep -Fxq '          go-version-file: go.mod' <<<"$native_engine_job" \
    && grep -Fxq '          persist-credentials: false' <<<"$native_engine_job" \
    && grep -Fxq '          make build VERSION=0.29.0-test' <<<"$native_engine_job" \
    && grep -Fxq '          .build/yard version' <<<"$native_engine_job" \
    && grep -Fxq '        run: go test -count=1 -timeout=20m ./cmd/... ./internal/...' <<<"$native_engine_job" \
    || fail "$(basename "$workflow") must build, run and test the native ARM64 engine"
  mapfile -t native_runners < <(sed -nE 's/^[[:space:]]+run: bash ([^[:space:]]+).*/\1/p' <<<"$native_engine_job")
  for runner in "${native_runners[@]}"; do
    [ -f "$ROOT/$runner" ] \
      || fail "$(basename "$workflow") references a missing native ARM64 runner: $runner"
  done
  ! grep -Fq 'scripts/install-key-tools.sh' "$workflow" \
    || fail "$(basename "$workflow") bypasses the prepared-context runner"
done
grep -Fxq '    needs: [host-free-verify, native-engine-arm64]' "$RELEASE_WORKFLOW" \
  || fail 'Release publication must require host-free and native ARM64 engine checks'

grep -Fq 'bash dev/shellcheck.sh' "$RELEASE_WORKFLOW" \
  && grep -Fq 'run: make cli-docs-check' "$RELEASE_WORKFLOW" \
  && grep -Fq 'bash dev/test-profiles.sh --e2e --list' "$RELEASE_WORKFLOW" \
  && grep -Fq '.build/test-runs/' "$RELEASE_WORKFLOW" \
  && grep -Fq 'make verify 2>&1 | tee .build/ci-logs/verify.log' "$RELEASE_WORKFLOW" \
  && grep -Fq 'if: failure()' "$RELEASE_WORKFLOW" \
  && grep -Fq "bash $RUNNER" "$RELEASE_WORKFLOW" \
  || fail 'Release must run full verification and preserve its failure diagnostics'
verify_recipe="$(awk '/^verify:/ {inside=1; next} inside && /^[^[:space:]]/ {exit} inside {print}' "$ROOT/Makefile")"
[[ "$verify_recipe" == *'./tests/run.sh'* && "$verify_recipe" == *'bash dev/test-profiles.sh'* ]] \
  || fail 'shared verification gate must include core and profile checks'
line_of() {
  grep -nF "$2" "$1" | sed -n '1p' | cut -d: -f1
}

release_adapter_line="$(line_of "$RELEASE_WORKFLOW" "run: bash $RUNNER")"
release_build_line="$(line_of "$RELEASE_WORKFLOW" 'name: Build final-version release assets once')"
release_compat_line="$(line_of "$RELEASE_WORKFLOW" 'name: Verify released updater compatibility')"
release_publish_line="$(line_of "$RELEASE_WORKFLOW" 'name: Freeze prepared source and exact assets')"
[ "$release_adapter_line" -lt "$release_publish_line" ] \
  || fail 'Release must pass the real-adapter gate before retaining candidate assets'
[ "$release_build_line" -lt "$release_publish_line" ] \
  || fail 'Release must build assets before retaining candidate assets'
[ "$release_build_line" -lt "$release_compat_line" ] \
  && [ "$release_compat_line" -lt "$release_publish_line" ] \
  || fail 'Release must verify built assets against released updaters before retaining candidate assets'
grep -Fq '2>&1 | tee .build/ci-logs/verify-release-upgrades.log' "$RELEASE_WORKFLOW" \
  || fail 'Release must preserve failed released-updater compatibility diagnostics'
grep -Fq 'python3 dev/verify-release-upgrades.py --release-dir .build/release --version 99.0.0' "$CI_WORKFLOW" \
  || fail 'CI must verify compatibility with released updaters on pull requests and pushes to master and release/**'
! grep -Fq 'runs-on: ubuntu-latest' "$RELEASE_WORKFLOW" \
  || fail 'Release must use the same explicit Ubuntu baseline as CI'
grep -Fq 'unshare --user --map-root-user --mount --pid --fork --kill-child=SIGKILL true' "$RELEASE_WORKFLOW" \
  || fail 'Release must preflight producer namespaces'

printf 'ok: CI and Release share the prepared-context real-adapter gate\n'

# Source-level security contract for the exact-candidate hosted VM evidence producer.
HOSTED_WORKFLOW="$ROOT/.github/workflows/release-vm.yml"
for contract in '  workflow_dispatch:' '  actions: read' '    runs-on: ubuntu-24.04' \
  '    timeout-minutes: 350' '      max-parallel: 12' \
  '          run-id: ${{ inputs.candidate_run }}' '          name: release-candidate-${{ inputs.candidate_attempt }}' \
  '          name: ci-hosted-${{ strategy.job-index }}' '        if: always()'; do
  grep -Fq "$contract" "$HOSTED_WORKFLOW" || fail "hosted workflow lacks exact-candidate contract: $contract"
done
! grep -Eq 'self-hosted|pull_request|secrets[.[]|:[[:space:]]*write' "$HOSTED_WORKFLOW" \
  || fail 'hosted VM producer must be manual and read-only'
grep -Fq 'bash dev/ci/periodic-e2e.sh acceptance' "$HOSTED_WORKFLOW" \
  && grep -Fq 'python3 -B dev/ci/release-vm.py' "$HOSTED_WORKFLOW" \
  || fail 'hosted VM workflow must reuse pool setup and verify terminal collector selection'
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import importlib.util
import json
import os
import re
import subprocess
import sys
import tempfile
from itertools import product
from pathlib import Path
from textwrap import dedent
root = Path(sys.argv[1])

# Source-level contract for the repository's block-style workflow/job/step layout;
# actionlint validates YAML separately. Cache settings never make an input ref safe.
def workflow_jobs(text):
    return re.findall(r"(?ms)^  ([\w-]+):\n(.*?)(?=^  [\w-]+:|\Z)", text.split("\njobs:\n", 1)[1])

ci = dict(workflow_jobs((root / ".github/workflows/ci.yml").read_text()))
for lane, command, extra in (
    ("go", "./tests/run.sh --go", ("make lint 2>&1 | tee .build/ci-logs/lint.log", "run: make cli-docs-check", ".build/test-runs/")),
    ("shell", "./tests/run.sh --shell", ("run: bash dev/shellcheck.sh", "bash tests/real-host/adapter-contracts.sh", ".build/test-runs/")),
    ("profiles", "}", ("run: bash dev/test-profiles.sh --e2e --list",
                       "bash dev/test-profiles.sh --permissions-only &&\n"
                       "              make build VERSION=0.29.0-test &&\n"
                       "              bash dev/test-profiles.sh --after-permissions")),
):
    job = ci[f"verify-{lane}"]
    for contract in (*extra, "runs-on: ubuntu-24.04", "set -o pipefail", "if: failure()",
                     f"{command} 2>&1 | tee .build/ci-logs/verify-{lane}.log",
                     "name: ci-host-free-" + lane + "-${{ github.run_attempt }}"):
        assert contract in job, f"verify-{lane} lacks {contract}"

gate = ci["verify"]
assert "    needs: [verify-go, verify-shell, verify-profiles]\n" in gate
assert "    if: always()" in gate.splitlines() and "        if: always()" in gate.splitlines()
for name in ("go", "shell", "profiles"):
    assert "          " + name.upper() + "_RESULT: ${{ needs.verify-" + name + ".result }}\n" in gate
assert "        if: cancelled()\n        run: exit 1\n" in gate
gate_script = re.search(r"(?ms)^        run: \|\n((?:^          [^\n]*\n)+)", gate)[1]
for results in product(("success", "failure", "cancelled", "skipped"), repeat=3):
    env = dict(os.environ, **dict(zip(("GO_RESULT", "SHELL_RESULT", "PROFILES_RESULT"), results)))
    rc = subprocess.run(["bash", "-e", "-c", gate_script], env=env, stdout=subprocess.DEVNULL, check=False).returncode
    assert (rc == 0) == (results == ("success",) * 3), (results, rc)

def workflow_steps(job):
    for step in re.split(r"(?m)^      - ", job)[1:]:
        step = "        " + step
        action = re.search(r"(?m)^        uses: [\"']?([^@\s\"']+)@", step)
        arguments = re.search(r"(?ms)^        with:\n(.*?)(?=^        \S|\Z)", step)
        yield action[1] if action else "", arguments[1] if arguments else "", step

def literal(arguments, key, value, indent=10):
    setting = re.search(r"(?m)^" + " " * indent + re.escape(key) + r":[ \t]*([^#\n]+)", arguments)
    return setting and setting[1].strip() in (value, f"'{value}'", f'"{value}"')

def blocking(block, indent):
    return not re.search(r"(?m)^" + " " * indent + r"if:", block) and (
        not re.search(r"(?m)^" + " " * indent + r"continue-on-error:", block)
        or literal(block, "continue-on-error", "false", indent))

EXACT_GUARD = '        run: test "$WORKFLOW_SHA" = "$COMMIT" && test "$(git rev-parse HEAD)" = "$COMMIT"\n'

def dispatch_contract(name, text):
    for job, body in workflow_jobs(text):
        steps = list(workflow_steps(body))
        for index, (action, arguments, step) in enumerate(steps):
            context = f"{name}/{job}: {action or 'run'}"
            run = re.search(r"(?ms)^        run:(.*?)(?=^        \S|\Z)", step)
            # All revision changes use reviewed checkout actions. Banning shell Git
            # mutations also covers refs hidden behind env variables or step outputs.
            assert not run or not re.search(r"\bgit\b[^\n;&|]*\b(?:fetch|checkout|switch|reset|pull|clone|worktree)\b",
                run[1].replace("\\\n", " ")), context + " must not change revisions through shell Git"
            if action == "actions/checkout":
                # The publisher selects a candidate-headed branch; its validated fetch
                # returns the immutable preparation's commit. Only this output is safe.
                publication = name == "release-publish.yml" and job == "publish" and literal(
                    arguments, "ref", "${{ steps.candidate.outputs.commit }}")
                assert not re.search(r"(?m)^          ref:", arguments) or literal(
                    arguments, "ref", "${{ github.sha }}") or publication, context + " must not check out an input- or output-derived ref"
                if publication:
                    producer = steps[index - 1][2] if index else ""
                    assert blocking(body, 4) and blocking(producer, 8), context + " requires a blocking validated fetch"
                    assert all(line in producer for line in (
                        '        id: candidate\n',
                        '          identity = module.fetch(module.GitHub(), os.environ["CANDIDATE"], prepared)\n',
                        '              stream.write("commit=" + identity["commit"] + "\\n")\n',
                    )), context + " requires the verified preparation commit output"
                if name in ("release.yml", "release-vm.yml"):
                    guard = steps[index + 1][2] if index + 1 < len(steps) else ""
                    assert blocking(body, 4) and blocking(guard, 8), context + " requires an unconditional blocking job and guard"
                    assert all(line in guard for line in (
                        '          COMMIT: ${{ inputs.commit }}\n',
                        '          WORKFLOW_SHA: ${{ github.sha }}\n', EXACT_GUARD)), context + " must immediately require exact workflow and candidate commit"
            if action == "actions/setup-go" and name == "release.yml":
                assert literal(arguments, "cache", "false"), context + " must set cache: false"

for workflow in sorted((root / ".github/workflows").iterdir()):
    if workflow.suffix in (".yml", ".yaml"):
        text = workflow.read_text()
        if re.search(r"(?m)^  workflow_dispatch:", text):
            dispatch_contract(workflow.name, text)

# A disabled Go cache must never excuse an input checkout or a missing/late guard.
release = (root / ".github/workflows/release.yml").read_text()
for unsafe in (
    release.replace('          persist-credentials: false\n', '          ref: ${{ inputs.commit }}\n          persist-credentials: false\n', 1),
    release.replace(EXACT_GUARD, '        run: true\n', 1),
    release.replace('          WORKFLOW_SHA: ${{ github.sha }}\n' + EXACT_GUARD,
                    '          WORKFLOW_SHA: ${{ inputs.commit }}\n' + EXACT_GUARD, 1),
    release.replace('      - name: Require exact workflow and candidate commit\n',
                    '      - uses: actions/setup-go@abc\n      - name: Require exact workflow and candidate commit\n', 1),
    *(release.replace('      - name: Require exact workflow and candidate commit\n',
                      '      - name: Require exact workflow and candidate commit\n        ' + flag + '\n', 1)
      for flag in ('continue-on-error: true', 'if: false')),
    *(release.replace('  native-engine-arm64:\n', '  native-engine-arm64:\n    ' + flag + '\n', 1)
      for flag in ('continue-on-error: true', 'if: false')),
    *(release.replace(EXACT_GUARD, EXACT_GUARD + '      - run: ' + command + '\n', 1)
      for command in ('git fetch origin "${{ inputs.commit }}"',
                      'git checkout "${{ steps.ref.outputs.commit }}"',
                      'git -C "$RUNNER_TEMP/source" checkout "${{ inputs.commit }}"')),
    release.replace(EXACT_GUARD, EXACT_GUARD + '      - env:\n          REF: ${{ inputs.commit }}\n        run: git fetch origin "$REF"\n', 1),
    release.replace(EXACT_GUARD, EXACT_GUARD + '      - uses: actions/checkout@abc\n        with:\n          ref: ${{ steps.ref.outputs.commit }}\n', 1),
):
    try:
        dispatch_contract("release.yml", unsafe)
    except AssertionError:
        pass
    else:
        raise AssertionError("dispatch contract accepted an unsafe release checkout")
dispatch_contract("release.yml", release.replace('          persist-credentials: false\n',
                  '          ref: ${{ github.sha }}\n          persist-credentials: false\n'))
dispatch_contract("release.yml", release.replace(EXACT_GUARD, '        continue-on-error: false\n' + EXACT_GUARD))
publication = (root / ".github/workflows/release-publish.yml").read_text()
for name, unsafe in (
    ("other.yml", publication),
    ("release-publish.yml", publication.replace('steps.candidate.outputs.commit', 'steps.other.outputs.commit')),
    ("release-publish.yml", publication.replace('identity = module.fetch', 'identity = unchecked_fetch')),
):
    try:
        dispatch_contract(name, unsafe)
    except AssertionError:
        pass
    else:
        raise AssertionError("dispatch contract accepted an unverified publication checkout")

def module(name, path):
    spec = importlib.util.spec_from_file_location(name, root / path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result
# Token access is limited to these step-scoped transport/control uses. VM and
# diagnostic steps must never receive repository secrets or github.token.
token_sites = {
    "release-vm.yml": {("check", "actions/download-artifact", "github-token: ${{ github.token }}")},
    "periodic-e2e.yml": {
        ("changes", "Compare with the last finished run", "GH_TOKEN: ${{ github.token }}"),
        ("supersede", "Replace earlier runs on this branch", "GH_TOKEN: ${{ github.token }}"),
        ("reschedule", "Retry nested VM checks that landed on AMD", "GH_TOKEN: ${{ github.token }}"),
    },
}
for filename, allowed in token_sites.items():
    workflow = (root / ".github/workflows" / filename).read_text()
    assert not re.search(r"\bsecrets\s*(?:\.|\[)", workflow, re.I), filename
    job = step = None
    sites = set()
    for line in workflow.split("\njobs:\n", 1)[1].splitlines():
        if match := re.fullmatch(r"  ([a-z][a-z0-9-]*):", line):
            job = match[1]
        if line.startswith("      - name: "):
            step = line.removeprefix("      - name: ")
        elif line.startswith("      - uses: "):
            step = line.removeprefix("      - uses: ").split("@", 1)[0]
        if re.search(r"github\s*(?:\.\s*token|\[\s*['\"]token['\"]\s*\])", line):
            site = (job, step, line.strip())
            assert site in allowed, (filename, site)
            sites.add(site)
    assert sites == allowed, filename
# Explicit workflow source contract: diagnostics run/upload independently of PASS,
# and timeout reserves leave room for the always() steps before the job limit.
for filename, count in (("release-vm.yml", 1), ("periodic-e2e.yml", 3)):
    workflow = (root / ".github/workflows" / filename).read_text()
    steps = re.split(r"(?m)^      - ", workflow)
    snapshots = [step for step in steps if step.startswith("name: Snapshot hosted check diagnostics\n")]
    uploads = [step for step in steps if step.startswith("name: Upload hosted check diagnostics\n")]
    assert len(snapshots) == len(uploads) == count
    for step in snapshots + uploads:
        assert "        if: always()" in step
        assert "failure()" not in step
        assert "continue-on-error: true" in step
    for step in snapshots:
        assert "periodic-e2e.sh diagnostics" in step
        assert "--diagnostics --status" in step
        assert '--logs "$RUNNER_TEMP/hosted-logs"' in step
        assert '--evidence "$HOME/.subyard/e2e"' in step
        assert '--destination "$RUNNER_TEMP/hosted-diagnostics"' in step
    for step in uploads:
        assert "path: ${{ runner.temp }}/hosted-diagnostics/" in step
        assert "if-no-files-found: error" in step
        assert "name: ci-hosted-" not in step
    runs = [step for step in steps if step.startswith(("name: Run the check\n", "name: Run exact collector selection", "name: Install the pool and hold one lease"))]
    assert len(runs) == count
    for step in runs:
        assert "timeout-minutes: 340" in step or "timeout-minutes: 110" in step
    if filename == "release-vm.yml":
        evidence = next(step for step in steps if "name: ci-hosted-" in step)
        assert "if: always() && steps.evidence.outcome == 'success'" in evidence
        assert "id: evidence" in workflow
        assert "continue-on-error" not in evidence
        exporter = next(step for step in steps if step.startswith("name: Preserve native evidence"))
        assert "continue-on-error" not in exporter
a = module("acceptance", "dev/release-acceptance.py")
assert a.CI_HOSTED_POLICIES == {"911e6588dded3367817a8c21c5a849dd5b945b2e",
                                "836bbd8229d65149c75731a2f9ea46f2aa45e241",
                                a.git_blob_sha((root / ".github/workflows/release-vm.yml").read_bytes())}
p = module("periodic", "dev/ci/plan-periodic-checks.py")
workflow = (root / ".github/workflows/periodic-e2e.yml").read_text()
planner = ["python3", "-B", "dev/ci/plan-periodic-checks.py"]
default_plan = json.loads(subprocess.check_output(planner, cwd=root, text=True))
assert default_plan == p.plan(root)
images = ("debian-13", "ubuntu-24.04", "ubuntu-26.04")
assert f"        type: choice\n        default: {images[0]}\n        options:\n" + "".join(
    f"          - {image}\n" for image in images
) in workflow
debian, ubuntu, _ = images
assert re.findall(r"(?m)^    - cron: '([^']+)'", workflow) == ["23 0,12 * * *", "23 6,18 * * *"]
assert "  push:\n    branches:\n      - 'ci-probe/**'\n" in workflow
lease_image = "inputs.image || (github.event.schedule == '23 6,18 * * *' && 'ubuntu-24.04') || 'debian-13'"
assert f"LEASE_IMAGE: ${{{{ {lease_image} }}}}" in workflow
assert f"github.ref == 'refs/heads/master' && format(' ({{0}})',\n  {lease_image})" in workflow
assert f"github.ref == 'refs/heads/master' && format(':image:{{0}}',\n    {lease_image})" in workflow
assert "inputs.origin && format('-retry-{0}', inputs.origin)" in workflow
assert "cancel-in-progress: ${{ !inputs.origin }}" in workflow
assert '--image "$LEASE_IMAGE"\n          bash dev/e2e/p0-acceptance.sh --lane boundary --slot 1' in workflow
assert '-f attempt="$attempt" -f image="$LEASE_IMAGE"' in workflow
assert workflow.count('CHECK_COMMAND: ${{ toJSON(matrix.command) }}') == 2
assert workflow.count('tar.gz" "${command[@]}"') == 2

# Run both read/cancel steps with fake GitHub metadata and the real jq filters.
periodic_jobs = dict(workflow_jobs(workflow))
gh = r'''
gh() {
  case "$1 $2" in
    'run list')
      if [ "$TEST_JOB" = changes ]; then
        [[ " $* " == *" --commit $HEAD --status completed "* ]] || return 90
      else
        [[ " $* " == *" --branch $REF "* ]] || return 91
      fi
      printf '%s\n' "$RUNS"
      return "$LIST_EXIT" ;;
    'run view')
      printf 'view %s\n' "$3" >> "$TRACE"
      [[ "$4" == --json && "$6" == --jq ]] || return 94
      jq -c --arg id "$3" '.[$id] // error("unexpected run")' <<<"$JOB_RESULTS" | jq -r "$7" ;;
    'run cancel') printf 'cancel %s\n' "$3" >> "$TRACE" ;;
    *) return 92 ;;
  esac
}
'''

def run_metadata(run_id, event, title, status="completed", conclusion="success"):
    return {"databaseId": run_id, "event": event, "displayTitle": title,
            "status": status, "conclusion": conclusion}

def job_result(name, conclusion):
    return {"status": "completed", "jobs": [{"name": name, "status": "completed", "conclusion": conclusion}]}

def exercise_step(job, image, runs, job_results, event="schedule", ref="master", origin="", run_id=50, list_exit=0):
    script = dedent(periodic_jobs[job].split("        run: |\n", 1)[1])
    with tempfile.TemporaryDirectory(dir="/tmp") as directory:
        trace, output, summary = (Path(directory) / name for name in ("trace", "output", "summary"))
        trace.touch()
        output.touch()
        result = subprocess.run(["bash", "-euo", "pipefail", "-c", gh + script], cwd=root,
                                env=dict(os.environ, TEST_JOB=job, RUNS=json.dumps(runs), LIST_EXIT=str(list_exit),
                                         JOB_RESULTS=json.dumps(job_results), EVENT=event, REF=ref, HEAD="ab" * 20,
                                         GITHUB_REF=f"refs/heads/{ref}", RUN_ID=str(run_id), ORIGIN=origin,
                                         LEASE_IMAGE=image, TRACE=str(trace),
                                         GITHUB_OUTPUT=str(output), GITHUB_STEP_SUMMARY=str(summary)))
        return result.returncode, output.read_text().strip(), trace.read_text().splitlines()

debian_run = run_metadata(10, "schedule", f"Periodic E2E ({debian})", conclusion="failure")
ubuntu_run = run_metadata(20, "schedule", f"Periodic E2E ({ubuntu})")
cancelled = run_metadata(9, "schedule", f"Periodic E2E ({ubuntu})", conclusion="cancelled")
manual = run_metadata(8, "workflow_dispatch", f"Periodic E2E ({ubuntu})")
probe = run_metadata(7, "push", f"Periodic E2E ({ubuntu})")  # Push titles are commit messages.
legacy = run_metadata(6, "schedule", "Periodic E2E")
assert exercise_step("changes", ubuntu, [debian_run, cancelled, manual, probe, legacy], {}) == (0, "run=true", [])
pool_ok = job_result("pool", "success")
assert exercise_step("changes", ubuntu, [ubuntu_run, debian_run], {"20": pool_ok}) == (0, "run=false", ["view 20"])
assert exercise_step("changes", debian, [ubuntu_run, debian_run], {"10": pool_ok}) == (0, "run=false", ["view 10"])
assert exercise_step("changes", ubuntu, [ubuntu_run], {"20": job_result("pool", "skipped")}) == (0, "run=true", ["view 20"])
assert exercise_step("changes", debian, [probe, legacy], {"6": job_result("pool", "failure"), "7": pool_ok}) == (
    0, "run=false", ["view 6", "view 7"])
for event in ("push", "workflow_dispatch"):
    assert exercise_step("changes", ubuntu, [], {}, event=event, list_exit=93) == (0, "run=true", [])
assert exercise_step("changes", ubuntu, [], {}, list_exit=93) == (93, "", [])

retry_title = "Nested VM checks on Intel, attempt 1, for run 10"
debian_retry = run_metadata(30, "workflow_dispatch", f"{retry_title} ({debian})", "in_progress")
ubuntu_retry = run_metadata(40, "workflow_dispatch", f"{retry_title} ({ubuntu})", "in_progress")
legacy_retry = run_metadata(25, "workflow_dispatch", retry_title, "in_progress")
retries = [ubuntu_retry, debian_retry, legacy_retry]
assert exercise_step("supersede", ubuntu, retries, {}) == (0, "", ["cancel 40"])
assert exercise_step("supersede", debian, retries, {}) == (0, "", ["cancel 30"])
assert exercise_step("supersede", debian, [legacy_retry], {}, ref="ci-probe/test") == (0, "", ["cancel 25"])
assert exercise_step("supersede", debian, [ubuntu_run], {}, origin="10") == (0, "", [])
supersede_ok = job_result("supersede", "success")
assert exercise_step("supersede", ubuntu, [ubuntu_run, debian_run], {"20": supersede_ok}, origin="5") == (
    0, "", ["view 20", "cancel 50"])
assert exercise_step("supersede", ubuntu, [ubuntu_run], {"20": job_result("supersede", "skipped")}, origin="5") == (
    0, "", ["view 20"])
assert exercise_step("supersede", ubuntu, [manual], {"8": supersede_ok}, origin="5") == (
    0, "", ["view 8", "cancel 50"])

# While its replacement was queued, a retry of run 10 could have started with a later run ID.
late_retry = run_metadata(50, "workflow_dispatch", f"{retry_title} ({debian})", "in_progress")
new_chain = run_metadata(55, "workflow_dispatch", f"{retry_title.replace('run 10', 'run 21')} ({debian})", "in_progress")
assert exercise_step("supersede", debian, [new_chain, late_retry, ubuntu_retry], {}, run_id=20) == (
    0, "", ["cancel 50"])

# The old concurrency group is still active. Only the scheduled origin proves the image.
legacy_main = run_metadata(10, "schedule", "Periodic E2E", "in_progress")
legacy_manual = run_metadata(11, "workflow_dispatch", "Periodic E2E", "in_progress")
manual_retry = run_metadata(26, "workflow_dispatch", retry_title.replace("run 10", "run 11"), "in_progress")
legacy_runs = [manual_retry, legacy_retry, legacy_manual, legacy_main]
assert exercise_step("supersede", debian, legacy_runs, {}, run_id=20) == (0, "", ["cancel 10", "cancel 25"])
assert exercise_step("supersede", ubuntu, legacy_runs, {}, run_id=20) == (0, "", [])
print("ok: periodic schedule tracks commit coverage and supersedes retries per lease image")

for image in images:
    selected = json.loads(subprocess.check_output(planner + ["--image", image], cwd=root, text=True))
    assert selected == {
        kind: {"include": [entry | {"command": ["--image", image, *entry["command"][2:]]}
                           for entry in group["include"]]}
        for kind, group in default_plan.items()
    }
for group in default_plan.values():
    assert all(entry["command"][:2] == ["--image", "debian-13"] for entry in group["include"])
# Run the hosted dispatch and controller environment with VM provisioning stubbed.
with tempfile.TemporaryDirectory(dir="/tmp") as directory:
    bundle = Path(directory) / "bundle"
    bundle.touch()
    wrapper = (root / "dev/ci/periodic-e2e.sh").read_text()
    wrapper = wrapper.replace("WORKSPACE=/srv/workspaces/Subyard/src", f"WORKSPACE={directory}")
    wrapper, count = re.subn(r"(?ms)^check_probes\(\) \{.*?^\}",
                            'check_probes() { BUNDLE="$2"; shift 2; run_check "$@"; }', wrapper)
    assert count == 1
    for image in images:
        assert subprocess.check_output(
            ["bash", "-c", wrapper, "periodic-e2e.sh", "check", "fixture", str(bundle),
             "--image", image, "printenv", "SUBYARD_E2E_LEASE_IMAGE"],
            env=dict(os.environ, RUNNER_TEMP=directory, GITHUB_ACTIONS="true", RUNNER_ENVIRONMENT="github-hosted"),
            text=True).strip() == image
entries = {entry["id"]: entry["command"][2:] for group in default_plan.values() for entry in group["include"]}
plan = {kind: {"include": [entry | {"command": entries[entry["id"]]} for entry in group["include"]]}
        for kind, group in default_plan.items()}
commands = a.commands(root, "0.0.0", a.profile_inventory(root))
config_sync = {"id": "periodic:config-sync",
               "command": ["timeout", "--signal=TERM", "--kill-after=600s", "45m",
                           "bash", "dev/e2e/config-sync-acceptance.sh", "--slot", "1"]}
assert plan["checks"]["include"].count(config_sync) == 1
# Collector commands wrap core VM checks, so also inspect their underlying catalog commands.
release_entries = [{"id": name, "command": command or []} for name, command in commands.items()]
release_entries += json.loads((root / a.CORE_VM_CATALOG).read_text())["checks"]
assert not any(entry["id"].split(":")[-1] == "config-sync" or
               any(Path(arg).name == "config-sync-acceptance.sh" for arg in entry["command"])
               for entry in release_entries), "config sync leaked into release acceptance"
apparmor_id = "vm:incus-apparmor-probe"
apparmor = [entry for group in plan.values() for entry in group["include"] if entry["id"] == apparmor_id]
assert apparmor == [{"id": apparmor_id, "command": [
    "dev/agent-e2e.sh", "--slot", "1", "--vm-count", "1", "--vm", "1",
    "--purpose", "apparmor-probe", "--", "bash", "dev/e2e/incus-apparmor-probe.sh"]}]
assert apparmor[0] in plan["checks"]["include"]  # Containers need no Intel-only nested VM job.
assert apparmor_id not in commands  # Periodic-only; no new release acceptance gate.
optional_vm = {}
for name, duration, guest_env in (
    ("cleanup-acceptance", "60m", ["env",
        "SUBYARD_E2E_CLEANUP_CACHE_HELPER=config/profiles/subyard-dev/tests/e2e/cleanup-cache.sh"]),
    ("sudo-terminal-ownership", "45m", []),
    ("zabbly-download-smoke", "15m", []),
):
    check_id = "periodic:" + name
    optional_vm[check_id] = {"id": check_id, "optional": True, "command": [
        "timeout", "--signal=TERM", "--kill-after=600s", duration,
        "dev/agent-e2e.sh", "--slot", "1", "--vm-count", "1", "--vm", "1",
        "--purpose", name, "--", *guest_env, "bash", f"dev/e2e/{name}.sh",
    ]}
    assert [entry for group in plan.values() for entry in group["include"]
            if entry["id"] == check_id] == [optional_vm[check_id]]
    assert optional_vm[check_id] in plan["checks"]["include"]
nm_checks = [entry for entry in plan["checks"]["include"] if entry["id"].startswith("periodic:nm-guard/")]
assert nm_checks == [{"id": f"periodic:nm-guard/{lane}", "optional": True,
                      "command": ["timeout", "--signal=TERM", "--kill-after=600s", "90m",
                                  "bash", "dev/e2e/nm-admin-uplink.sh", "--slot", "1",
                                  "--lane", lane]}
                     for lane in ("fresh", "upgrade")]
assert not {entry["id"] for entry in nm_checks} & set(commands)
# Adding periodic-only checks must preserve every existing matrix entry, including Veranda.
p.PERIODIC_ONLY = []
added_ids = {config_sync["id"], apparmor_id, *optional_vm, *(entry["id"] for entry in nm_checks)}
assert {kind: {"include": [entry for entry in group["include"] if entry["id"] not in added_ids]}
        for kind, group in default_plan.items()} == p.plan(root)
# Execute the workflow's actual default/dispatch filtering, without running any VM commands.
plan = default_plan
selection = workflow.split('          plan="$(python3 -B dev/ci/plan-periodic-checks.py --image "$LEASE_IMAGE")"\n', 1)[1]
selection = selection.split('          if [ -n "$RETRY" ]; then', 1)[0]
for only in ("", config_sync["id"], "p0:source-upgrade", "published:0.11.0", "published:0.17.3", "published:0.27.0",
             *optional_vm, *(entry["id"] for entry in nm_checks)):
    selected = json.loads(subprocess.check_output(
        ["bash", "-euc", selection + '\nprintf "%s\\n" "$plan"'], cwd=root, text=True,
        env=dict(os.environ, ONLY=only, plan=json.dumps(plan))))
    expected = ({kind: {"include": [entry for entry in group["include"] if entry["id"] == only]}
                 for kind, group in plan.items()} if only else
                {kind: {"include": [entry for entry in group["include"] if not entry.get("manual")]}
                 for kind, group in plan.items()})
    assert selected == expected
print("ok: periodic config sync runs by default and by dispatch, outside release acceptance")
# Periodic-only profile lanes are planned for Periodic E2E but are never release checks.
periodic_only = {"profile:" + name + "/" + lane["id"] for name, entry in a.profile_inventory(root).items()
                 for lane in entry.get("lanes", []) if lane.get("periodic_only")}
assert periodic_only <= set(entries) and not periodic_only & set(commands)
assert entries["vm:integration-selection/default"] == commands["vm:integration-selection/default"] + ["--slot", "1"]
for name, command in commands.items():
    if name.startswith("vm:published-upgrade/"):
        assert entries[name] == command + ["--slot", "1"]
# Baselines outside the catalog keep one agent-free periodic upgrade each.
baselines = [(fields[0], fields[3]) for line in (root / "dev/e2e/published-upgrade-baselines.tsv").read_text().splitlines()
             if line and (fields := line.split("\t"))[3] != "pin"]
assert {name for name in entries if name.startswith("published:")} == {
    "published:" + version for version, role in baselines
    if "vm:published-upgrade/" + (role if role in ("anchor", "hop") else version) not in commands}
# Unsupported-source VM checks run by default and remain nonblocking on release and PR paths.
optional = {entry["id"] for group in plan.values() for entry in group["include"] if entry.get("optional")}
assert optional == {"p0:source-upgrade", "published:0.11.0", "published:0.17.3", "published:0.27.0", *optional_vm, *(entry["id"] for entry in nm_checks)}
assert not {"vm:p0-source-upgrade", *("vm:published-upgrade/" + v for v in ("0.11.0", "0.17.3", "0.27.0"))} & set(commands)
assert entries["p0:source-upgrade"] == ["bash", "dev/e2e/p0-acceptance.sh", "--lane", "source-upgrade", "--slot", "1"]
assert "    continue-on-error: ${{ matrix.optional || false }}\n" in workflow.split("  check:\n", 1)[1].split("    steps:", 1)[0]
# Full P0 lanes that release acceptance runs as separate hosted checks keep one periodic lane run each.
for lane in ("power-systemd", "legacy-continuation"):
    assert "vm:p0-" + lane in a.CI_HOSTED_CHECKS and "vm:p0-" + lane not in entries
    assert entries["p0:" + lane] == ["bash", "dev/e2e/p0-acceptance.sh", "--lane", lane, "--slot", "1"]
import subprocess
from unittest.mock import patch
lanes_command = ["bash", "dev/e2e/p0-acceptance.sh", "--list-lanes"]
listing = subprocess.check_output(lanes_command, cwd=root, text=True)
native_output = subprocess.check_output
for broken in (listing.replace("split-covered\t", "split-covered\tboundary "),
               "".join(line for line in listing.splitlines(True) if not line.startswith("split-covered\t"))):
    with patch.object(p.subprocess, "check_output",
                      side_effect=lambda command, *args, **kwargs: broken if command == lanes_command else native_output(command, *args, **kwargs)):
        try:
            p.plan(root)
        except SystemExit as error:
            assert "split lane" in str(error)
        else:
            raise AssertionError("periodic plan accepted a split lane without exactly one release catalog check")
PY
printf 'ok: hosted VM workflow binds exact candidates and periodic upgrades retain catalog coverage\n'

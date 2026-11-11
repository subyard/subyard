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
grep -Fxq '  verify:' "$CI_WORKFLOW" && grep -Fxq '  deep-go:' "$DEEP_WORKFLOW" \
  || fail 'core and deep CI must use distinct stable job names'

grep -Fxq '  push:' "$CI_WORKFLOW" && grep -Fxq '  pull_request:' "$CI_WORKFLOW" \
  && grep -Fxq '    branches:' "$CI_WORKFLOW" && grep -Fxq "      - '**'" "$CI_WORKFLOW" \
  || fail 'core CI must run on branch pushes and pull requests'
! grep -Eq '^[[:space:]]+tags(-ignore)?:' "$CI_WORKFLOW" \
  || fail 'core CI must not run on tag pushes'

grep -Fxq '  workflow_dispatch:' "$DEEP_WORKFLOW" \
  && grep -Fxq '  schedule:' "$DEEP_WORKFLOW" \
  || fail 'deep CI must support manual and nightly runs'
grep -Fxq '        run: make build VERSION=0.22.0-test' "$DEEP_WORKFLOW" \
  && grep -Fxq '        run: umask 0022 && go test -race -shuffle=on -count=2 -timeout=60m ./...' "$DEEP_WORKFLOW" \
  && grep -Fxq "        run: go test ./internal/command -run '^\$' -fuzz '^FuzzParseDoesNotPanic\$' -fuzztime=60s" "$DEEP_WORKFLOW" \
  && grep -Fxq "        run: go test ./internal/observation -run '^\$' -fuzz '^FuzzValidateJSON\$' -fuzztime=60s" "$DEEP_WORKFLOW" \
  || fail 'deep CI must build the runtime, run shuffled repeated race tests and bounded fuzzing'

for workflow in "$CI_WORKFLOW" "$RELEASE_WORKFLOW"; do
  native_engine_job="$(awk '/^  native-engine-arm64:/ {inside=1; next} inside && /^  [^[:space:]]/ {exit} inside {print}' "$workflow")"
  grep -Fxq '    runs-on: ubuntu-24.04-arm' <<<"$native_engine_job" \
    && grep -Fxq '          go-version-file: go.mod' <<<"$native_engine_job" \
    && grep -Fxq '          persist-credentials: false' <<<"$native_engine_job" \
    && grep -Fxq '          make build VERSION=0.22.0-test' <<<"$native_engine_job" \
    && grep -Fxq '          .build/yard --version' <<<"$native_engine_job" \
    && grep -Fxq '        run: go test -count=1 -timeout=20m ./cmd/... ./internal/...' <<<"$native_engine_job" \
    || fail "$(basename "$workflow") must build, run and test the native ARM64 engine"
  mapfile -t native_runners < <(sed -nE 's/^[[:space:]]+run: bash ([^[:space:]]+).*/\1/p' <<<"$native_engine_job")
  for runner in "${native_runners[@]}"; do
    [ -f "$ROOT/$runner" ] \
      || fail "$(basename "$workflow") references a missing native ARM64 runner: $runner"
  done
  grep -Fq 'make verify' "$workflow" \
    || fail "$(basename "$workflow") must verify core and shipped profiles"
  grep -Fq 'run: make cli-docs-check' "$workflow" \
    && grep -Fq 'bash dev/test-profiles.sh --e2e --list' "$workflow" \
    || fail "$(basename "$workflow") must check CLI docs and shipped profile E2E inventory"
  grep -Fq '.build/test-runs/' "$workflow" \
    && grep -Fq 'make verify 2>&1 | tee .build/ci-logs/verify.log' "$workflow" \
    && grep -Fq 'if: failure()' "$workflow" \
    || fail "$(basename "$workflow") must preserve failed host-free diagnostics"
  grep -Fq "bash $RUNNER" "$workflow" \
    || fail "$(basename "$workflow") must invoke the shared runner"
  ! grep -Fq 'scripts/install-key-tools.sh' "$workflow" \
    || fail "$(basename "$workflow") bypasses the prepared-context runner"
done
grep -Fxq '    needs: [host-free-verify, native-engine-arm64]' "$RELEASE_WORKFLOW" \
  || fail 'Release publication must require host-free and native ARM64 engine checks'

grep -Fq 'make verify 2>&1 | tee .build/ci-logs/verify.log' "$CI_WORKFLOW" \
  && grep -Fq 'shellcheck -x -S warning' "$CI_WORKFLOW" \
  || fail 'CI must run the core and ShellCheck gates'
verify_recipe="$(awk '/^verify:/ {inside=1; next} inside && /^[^[:space:]]/ {exit} inside {print}' "$ROOT/Makefile")"
[[ "$verify_recipe" == *'./tests/run.sh'* && "$verify_recipe" == *'bash dev/test-profiles.sh'* ]] \
  || fail 'shared verification gate must include core and profile checks'
line_of() {
  grep -nF "$2" "$1" | head -n1 | cut -d: -f1
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
  || fail 'CI must verify every push against released updaters, not only release preparation'
! grep -Fq 'runs-on: ubuntu-latest' "$RELEASE_WORKFLOW" \
  || fail 'Release must use the same explicit Ubuntu baseline as CI'
grep -Fq 'unshare --user --map-root-user --mount --pid --fork --kill-child=SIGKILL true' "$RELEASE_WORKFLOW" \
  || fail 'Release must preflight producer namespaces'

printf 'ok: CI and Release share the prepared-context real-adapter gate\n'

# Source-level security contract for the exact-candidate hosted VM evidence producer.
HOSTED_WORKFLOW="$ROOT/.github/workflows/release-vm.yml"
for contract in '  workflow_dispatch:' '  actions: read' '    runs-on: ubuntu-24.04' \
  '    timeout-minutes: 350' '      max-parallel: 12' '          ref: ${{ inputs.commit }}' \
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
from pathlib import Path
import sys
root = Path(sys.argv[1])
def module(name, path):
    spec = importlib.util.spec_from_file_location(name, root / path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result
a = module("acceptance", "dev/release-acceptance.py")
assert a.git_blob_sha((root / ".github/workflows/release-vm.yml").read_bytes()) in a.CI_HOSTED_POLICIES
p = module("periodic", "dev/ci/plan-periodic-checks.py")
plan = p.plan(root)
entries = {entry["id"]: entry["command"] for group in plan.values() for entry in group["include"]}
commands = a.commands(root, "0.0.0", a.profile_inventory(root))
assert entries["vm:integration-selection/default"] == commands["vm:integration-selection/default"] + ["--slot", "1"]
for name, command in commands.items():
    if name.startswith("vm:published-upgrade/"):
        assert entries[name] == command + ["--slot", "1"]
# Baselines outside the catalog keep one agent-free periodic upgrade each.
baselines = [line.split("\t", 1)[0] for line in (root / "dev/e2e/published-upgrade-baselines.tsv").read_text().splitlines() if line]
assert {name for name in entries if name.startswith("published:")} == {
    "published:" + version for version in baselines if "vm:published-upgrade/" + version not in commands}
PY
printf 'ok: hosted VM workflow binds exact candidates and periodic upgrades retain catalog coverage\n'

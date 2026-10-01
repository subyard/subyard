#!/usr/bin/env bash
# Host-free contract: CI and Release must use the same real-adapter entrypoint.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNNER='tests/real-host/adapter-contracts.sh'
CI_WORKFLOW="$ROOT/.github/workflows/ci.yml"
RELEASE_WORKFLOW="$ROOT/.github/workflows/release.yml"
DEEP_WORKFLOW="$ROOT/.github/workflows/deep-ci.yml"
PASEO_WORKFLOW="$ROOT/.github/workflows/paseo-headless.yml"

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

for workflow in "$CI_WORKFLOW" "$PASEO_WORKFLOW"; do
  grep -Fxq '  push:' "$workflow" && grep -Fxq '  pull_request:' "$workflow" \
    && grep -Fxq '    branches:' "$workflow" && grep -Fxq "      - '**'" "$workflow" \
    || fail "$(basename "$workflow") must run on branch pushes and pull requests"
  ! grep -Eq '^[[:space:]]+tags(-ignore)?:' "$workflow" \
    || fail "$(basename "$workflow") must not run on tag pushes"
done

grep -Fxq '  workflow_dispatch:' "$DEEP_WORKFLOW" \
  && grep -Fxq '  schedule:' "$DEEP_WORKFLOW" \
  || fail 'deep CI must support manual and nightly runs'
grep -Fq 'run: go test -race -shuffle=on -count=3 ./...' "$DEEP_WORKFLOW" \
  && grep -Fq "run: go test ./internal/command -run '^\$' -fuzz '^FuzzParseDoesNotPanic\$' -fuzztime=60s" "$DEEP_WORKFLOW" \
  || fail 'deep CI must run repeated race tests and bounded parser fuzzing'

for workflow in "$CI_WORKFLOW" "$RELEASE_WORKFLOW"; do
  native_engine_job="$(awk '/^  native-engine-arm64:/ {inside=1; next} inside && /^  [^[:space:]]/ {exit} inside {print}' "$workflow")"
  grep -Fxq '    runs-on: ubuntu-24.04-arm' <<<"$native_engine_job" \
    && grep -Fxq '          go-version-file: go.mod' <<<"$native_engine_job" \
    && grep -Fxq '          persist-credentials: false' <<<"$native_engine_job" \
    && grep -Fxq '          dev/build-engine.sh' <<<"$native_engine_job" \
    && grep -Fxq '          .build/yard --version' <<<"$native_engine_job" \
    && grep -Fxq '        run: go test -count=1 ./cmd/... ./internal/...' <<<"$native_engine_job" \
    || fail "$(basename "$workflow") must build, run and test the native ARM64 engine"
  grep -Fq 'make verify' "$workflow" \
    || fail "$(basename "$workflow") must verify core and shipped profiles"
  grep -Fq "bash $RUNNER" "$workflow" \
    || fail "$(basename "$workflow") must invoke the shared runner"
  ! grep -Fq 'scripts/install-key-tools.sh' "$workflow" \
    || fail "$(basename "$workflow") bypasses the prepared-context runner"
done
grep -Fxq '    needs: [native-engine-arm64, paseo-headless]' "$RELEASE_WORKFLOW" \
  || fail 'Release publication must require native ARM64 engine and Paseo checks'

grep -Fq 'run: make verify' "$CI_WORKFLOW" \
  && grep -Fq 'shellcheck -x -S warning' "$CI_WORKFLOW" \
  || fail 'CI must run the core and ShellCheck gates'
verify_recipe="$(awk '/^verify:/ {inside=1; next} inside && /^[^[:space:]]/ {exit} inside {print}' "$ROOT/Makefile")"
[[ "$verify_recipe" == *'./tests/run.sh'* && "$verify_recipe" == *'bash dev/test-profiles.sh'* ]] \
  || fail 'shared verification gate must include core and profile checks'
line_of() {
  grep -nF "$2" "$1" | head -n1 | cut -d: -f1
}

release_adapter_line="$(line_of "$RELEASE_WORKFLOW" "run: bash $RUNNER")"
release_build_line="$(line_of "$RELEASE_WORKFLOW" 'name: Build release assets')"
release_publish_line="$(line_of "$RELEASE_WORKFLOW" 'name: Publish GitHub Release')"
[ "$release_adapter_line" -lt "$release_publish_line" ] \
  || fail 'Release must pass the real-adapter gate before publishing assets'
[ "$release_build_line" -lt "$release_publish_line" ] \
  || fail 'Release must build assets before publishing'
grep -Fq 'bash dev/test-profiles.sh --e2e --list' "$CI_WORKFLOW" \
  || fail 'CI must preflight the shipped profile E2E inventory'

printf 'ok: CI and Release share the prepared-context real-adapter gate\n'

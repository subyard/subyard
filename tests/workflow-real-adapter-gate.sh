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
  [ "$(grep -Fc 'runs-on: ubuntu-24.04' "$workflow")" -eq 1 ] \
    || fail 'host-free workflow must have one standard Ubuntu 24.04 job'
  grep -Fxq 'permissions:' "$workflow" \
    && grep -Fxq '  contents: read' "$workflow" \
    || fail 'host-free workflow must declare read-only contents permission'
  grep -Fxq '          persist-credentials: false' "$workflow" \
    || fail 'host-free checkout must not persist credentials'
  if grep -Eq 'self-hosted|pull_request_target|secrets[.[]|:[[:space:]]*write|upload-artifact|/dev/kvm|dev/agent-e2e|dev/e2e/|^[[:space:]]*(services|container|matrix|paths|paths-ignore|concurrency):' "$workflow"; then
    fail 'host-free workflow contains a privileged, filtered or unnecessary CI mechanism'
  fi
  mapfile -t actions < <(sed -nE 's/^[[:space:]]*(- )?uses: //p' "$workflow")
  [ "${#actions[@]}" -eq 2 ] || fail 'host-free job must use only checkout and setup-go'
  [[ "${actions[0]}" == actions/checkout@* && "${actions[1]}" == actions/setup-go@* ]] \
    || fail 'host-free job must check out sources and set up Go'
  for action in "${actions[@]}"; do
    [[ "$action" =~ ^actions/(checkout|setup-go)@[0-9a-f]{40}\ \#\ v[0-9]+\.[0-9]+\.[0-9]+$ ]] \
      || fail 'host-free Actions must use full commit SHAs with version comments'
  done
done
grep -Fxq '  verify:' "$CI_WORKFLOW" && grep -Fxq '  deep-go:' "$DEEP_WORKFLOW" \
  || fail 'core and deep CI must use distinct stable job names'
grep -Fxq '    timeout-minutes: 15' "$CI_WORKFLOW" \
  && grep -Fxq '    timeout-minutes: 20' "$DEEP_WORKFLOW" \
  || fail 'host-free jobs must have bounded timeouts'
grep -Fxq '  push:' "$CI_WORKFLOW" && grep -Fxq '  pull_request:' "$CI_WORKFLOW" \
  || fail 'core CI must run on pushes and pull requests'
grep -Fxq '  workflow_dispatch:' "$DEEP_WORKFLOW" \
  && grep -Fxq "    - cron: '17 3 * * *'" "$DEEP_WORKFLOW" \
  || fail 'deep CI must support manual and nightly runs'
grep -Fq 'run: go test -race -shuffle=on -count=3 ./...' "$DEEP_WORKFLOW" \
  && grep -Fq "run: go test ./internal/command -run '^\$' -fuzz '^FuzzParseDoesNotPanic\$' -fuzztime=60s" "$DEEP_WORKFLOW" \
  || fail 'deep CI must run repeated race tests and bounded parser fuzzing'

for workflow in "$CI_WORKFLOW" "$RELEASE_WORKFLOW"; do
  [ "$(grep -Fc "bash $RUNNER" "$workflow")" -eq 1 ] \
    || fail "$(basename "$workflow") must invoke the shared runner exactly once"
  ! grep -Fq 'scripts/install-key-tools.sh' "$workflow" \
    || fail "$(basename "$workflow") bypasses the prepared-context runner"
done

[ "$(grep -Fc 'runs-on: ubuntu-24.04' "$CI_WORKFLOW")" -eq 1 ] \
  || fail 'CI verify must run the systemd parser contract on Ubuntu 24.04'
grep -A4 '^  publish:' "$RELEASE_WORKFLOW" \
  | grep -Fq 'runs-on: ubuntu-latest' \
  || fail 'tag publish must verify the release on the latest Ubuntu runner'

line_of() {
  grep -nF "$2" "$1" | head -n1 | cut -d: -f1
}

ci_suite_line="$(line_of "$CI_WORKFLOW" 'run: ./tests/run.sh')"
ci_adapter_line="$(line_of "$CI_WORKFLOW" "run: bash $RUNNER")"
[ "$ci_suite_line" -lt "$ci_adapter_line" ] \
  || fail 'CI must run the host-free suite before the real-adapter gate'

release_adapter_line="$(line_of "$RELEASE_WORKFLOW" "run: bash $RUNNER")"
release_build_line="$(line_of "$RELEASE_WORKFLOW" 'name: Build release assets')"
release_publish_line="$(line_of "$RELEASE_WORKFLOW" 'name: Publish GitHub Release')"
[ "$release_adapter_line" -lt "$release_build_line" ] \
  && [ "$release_build_line" -lt "$release_publish_line" ] \
  || fail 'Release must pass the real-adapter gate before building and publishing assets'

printf 'ok: CI and Release share the prepared-context real-adapter gate\n'

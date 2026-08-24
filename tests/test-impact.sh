#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_TMP="$(mktemp -d /tmp/subyard-test-impact-contract.XXXXXX)"
trap 'rm -rf -- "$TEST_TMP"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

for tool in cmp env git go jq mktemp; do need "$tool"; done

test_targeted_adapter_runner() {
  fake_bin="$TEST_TMP/adapter-fake-bin"
  mkdir -p "$fake_bin"
  cat > "$fake_bin/bash" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_ADAPTER_LOG"
case "$*" in
  */dev/build-engine.sh)
    mkdir -p "$HOME/go/pkg/mod/example.invalid/module@v1.0.0"
    : > "$HOME/go/pkg/mod/example.invalid/module@v1.0.0/go.mod"
    chmod -R a-w "$HOME/go/pkg/mod"
    ;;
esac
EOF
  chmod 0755 "$fake_bin/bash"

  for selected in credential-tools ssh-rpc ssh-credential-peer; do
    log="$TEST_TMP/adapter-$selected.log"
    : > "$log"
    PATH="$fake_bin:/usr/bin:/bin" FAKE_ADAPTER_LOG="$log" \
      /bin/bash "$ROOT/tests/real-host/adapter-contracts.sh" --check "$selected"

    [ "$(grep -Fxc "$ROOT/dev/build-engine.sh" "$log")" -eq 1 ] \
      || fail "prepared adapter runner did not build the development engine for $selected"
    selected_script="$ROOT/tests/real-host/$selected.sh"
    grep -Fxq "$selected_script" "$log" \
      || fail "prepared adapter runner did not invoke $selected_script"
    [ "$(grep -Fc '/tests/real-host/' "$log")" -eq 1 ] \
      || fail "$selected adapter runner invoked unrelated real-host checks"
  done

  aggregate_log="$TEST_TMP/adapter-all.log"
  : > "$aggregate_log"
  PATH="$fake_bin:/usr/bin:/bin" FAKE_ADAPTER_LOG="$aggregate_log" \
    /bin/bash "$ROOT/tests/real-host/adapter-contracts.sh"
  [ "$(grep -Fxc "$ROOT/dev/build-engine.sh" "$aggregate_log")" -eq 1 ] \
    || fail 'aggregate adapter runner did not build the development engine exactly once'
  for selected in credential-tools ssh-rpc ssh-credential-peer; do
    [ "$(grep -Fxc "$ROOT/tests/real-host/$selected.sh" "$aggregate_log")" -eq 1 ] \
      || fail "aggregate adapter runner did not invoke $selected exactly once"
  done
  [ "$(grep -Fc '/tests/real-host/' "$aggregate_log")" -eq 3 ] \
    || fail 'aggregate adapter runner invoked an unexpected real-host check'

  invalid_index=0
  for invalid_arguments in '--check' '--check ssh-rpc extra' '--unknown ssh-rpc' '--check unknown'; do
    invalid_index=$((invalid_index + 1))
    invalid_log="$TEST_TMP/adapter-invalid-$invalid_index.log"
    : > "$invalid_log"
    read -r -a invalid_argv <<< "$invalid_arguments"
    set +e
    PATH="$fake_bin:/usr/bin:/bin" FAKE_ADAPTER_LOG="$invalid_log" \
      /bin/bash "$ROOT/tests/real-host/adapter-contracts.sh" "${invalid_argv[@]}" \
      > "$TEST_TMP/adapter-invalid-$invalid_index.out" \
      2> "$TEST_TMP/adapter-invalid-$invalid_index.err"
    invalid_status=$?
    set -e
    [ "$invalid_status" -eq 2 ] \
      || fail "invalid adapter invocation status = $invalid_status, want 2: $invalid_arguments"
    [ ! -s "$invalid_log" ] \
      || fail "invalid adapter invocation executed a child script: $invalid_arguments"
  done
}

test_wrapper_bootstrap_isolation() {
  probe_repository="$TEST_TMP/bootstrap-probe"
  mkdir -p "$probe_repository/cmd/test-impact" "$probe_repository/dev"
  cp "$ROOT/dev/test-impact.sh" "$probe_repository/dev/test-impact.sh"
  chmod 0755 "$probe_repository/dev/test-impact.sh"
  printf '%s\n' 'module example.com/bootstrap-probe' '' 'go 1.24' > "$probe_repository/go.mod"
  cat > "$probe_repository/cmd/test-impact/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"
)

func main() {
	for _, name := range []string{"HOME", "TMPDIR", "GOCACHE", "GOMODCACHE"} {
		fmt.Println(os.Getenv(name))
	}
	fmt.Fprintln(os.Stderr, "started binary stderr")
	os.Exit(23)
}
EOF

  hostile_target="$TEST_TMP/hostile-cache-target"
  mkdir -p "$hostile_target"
  printf 'untouched\n' > "$hostile_target/sentinel"
  for name in home tmp go-cache go-mod-cache; do
    ln -s "$hostile_target" "$TEST_TMP/hostile-$name"
  done

  hostile_startup_bin="$TEST_TMP/hostile-startup-bin"
  mkdir -p "$hostile_startup_bin"
  cat > "$hostile_startup_bin/bash" <<'EOF'
#!/bin/sh
: > "$FAKE_BASH_MARKER"
exec /bin/bash "$@"
EOF
  chmod 0755 "$hostile_startup_bin/bash"
  cat > "$TEST_TMP/hostile-bash-env" <<'EOF'
: > "$BASH_ENV_MARKER"
EOF

  set +e
  env -i \
    PATH="$hostile_startup_bin:/usr/bin:/bin" \
    HOME="$TEST_TMP/hostile-home" TMPDIR="$TEST_TMP/hostile-tmp" \
    GOCACHE="$TEST_TMP/hostile-go-cache" GOMODCACHE="$TEST_TMP/hostile-go-mod-cache" \
    GOFLAGS='-invalid-hostile-flag' GOWORK="$TEST_TMP/hostile.work" GOENV="$TEST_TMP/hostile.goenv" \
    LC_ALL=definitely_invalid_locale LANG=another_invalid_locale \
    SUBYARD_SECRET_DO_NOT_READ='hostile-value' \
    test_impact_toolchain_cache_parent="$hostile_target" \
    BASH_ENV="$TEST_TMP/hostile-bash-env" \
    FAKE_BASH_MARKER="$TEST_TMP/fake-bash.marker" BASH_ENV_MARKER="$TEST_TMP/bash-env.marker" \
    'BASH_FUNC_cd%%=() { printf "native launch setup diagnostic\n" >&2; builtin cd "$@"; }' \
    "$probe_repository/dev/test-impact.sh" \
    > "$TEST_TMP/bootstrap-probe.out" 2> "$TEST_TMP/bootstrap-probe.err"
  probe_status=$?
  set -e
  [ "$probe_status" -eq 23 ] || fail "wrapper changed started binary status to $probe_status"
  [ "$(<"$TEST_TMP/bootstrap-probe.err")" = 'started binary stderr' ] \
    || fail 'wrapper suppressed or mixed native bootstrap output with started binary stderr'
  [ ! -e "$TEST_TMP/fake-bash.marker" ] || fail 'wrapper startup executed caller PATH bash'
  [ ! -e "$TEST_TMP/bash-env.marker" ] || fail 'wrapper startup executed caller BASH_ENV'

  mapfile -t bootstrap_environment < "$TEST_TMP/bootstrap-probe.out"
  [ "${#bootstrap_environment[@]}" -eq 4 ] || fail 'bootstrap probe did not report four environment paths'
  private_invocation_directory="${bootstrap_environment[0]%/home}"
  case "$private_invocation_directory" in
    /tmp/subyard-test-impact.*) ;;
    *) fail "HOME is not in the private invocation directory: ${bootstrap_environment[0]}" ;;
  esac
  [ "${bootstrap_environment[0]}" = "$private_invocation_directory/home" ] \
    || fail 'HOME escaped the private invocation directory'
  [ "${bootstrap_environment[1]}" = "$private_invocation_directory/tmp" ] \
    || fail 'TMPDIR escaped the private invocation directory'
  [ "${bootstrap_environment[2]}" = "$private_invocation_directory/go-cache" ] \
    || fail 'GOCACHE escaped the private invocation directory'
  [ "${bootstrap_environment[3]}" = "$private_invocation_directory/go-mod-cache" ] \
    || fail 'GOMODCACHE escaped the private invocation directory'
  [ ! -e "$private_invocation_directory" ] || fail 'private invocation directory survived cleanup'
  [ "$(<"$hostile_target/sentinel")" = 'untouched' ] || fail 'hostile cache target was mutated'
  [ "$(find "$hostile_target" -mindepth 1 -maxdepth 1 -printf '%f\n')" = 'sentinel' ] \
    || fail 'wrapper followed a hostile caller cache or setup symlink'

  broken_repository="$TEST_TMP/bootstrap-native-stderr"
  mkdir -p "$broken_repository/dev" "$TEST_TMP/launcher-home"
  cp "$ROOT/dev/test-impact.sh" "$broken_repository/dev/test-impact.sh"
  chmod 0755 "$broken_repository/dev/test-impact.sh"
  set +e
  env -i PATH=/usr/bin:/bin HOME="$TEST_TMP/launcher-home" \
    LC_ALL=definitely_invalid_locale LANG=another_invalid_locale \
    'BASH_FUNC_mktemp%%=() { printf "native mktemp diagnostic\n" >&2; command mktemp "$@"; }' \
    'BASH_FUNC_mkdir%%=() { printf "native mkdir diagnostic\n" >&2; command mkdir "$@"; }' \
    'BASH_FUNC_rm%%=() { printf "native cleanup diagnostic\n" >&2; command rm "$@"; }' \
    "$broken_repository/dev/test-impact.sh" --format json \
    > "$TEST_TMP/bootstrap-native.json" 2> "$TEST_TMP/bootstrap-native.err"
  native_status=$?
  set -e
  [ "$native_status" -eq 0 ] || fail "bootstrap failure status = $native_status, want 0"
  jq -s -e 'length == 1 and .[0].status == "fallback" and
    .[0].errors == [{"code":"BOOTSTRAP_FAILURE","message":"test-impact command could not be started"}]' \
    "$TEST_TMP/bootstrap-native.json" >/dev/null || fail 'bootstrap failure did not emit one fallback document'
  [ "$(<"$TEST_TMP/bootstrap-native.err")" = 'test-impact: BOOTSTRAP_FAILURE: test-impact command could not be started' ] \
    || fail 'bootstrap operations or cleanup exposed native stderr'

  capture_failure_path="$TEST_TMP/capture-open-failure"
  mkdir -p "$capture_failure_path" "$TEST_TMP/capture-seam-broken/dev"
  cp "$ROOT/dev/test-impact.sh" "$TEST_TMP/capture-seam-broken/dev/test-impact.sh"
  chmod 0755 "$TEST_TMP/capture-seam-broken/dev/test-impact.sh"
  set +e
  env -i PATH=/usr/bin:/bin HOME="$TEST_TMP/launcher-home" /bin/bash -c '
    source "$1"
    printf reached > "$3"
    output_format=json
    invoke_started_binary "$2"
  ' capture-seam "$TEST_TMP/capture-seam-broken/dev/test-impact.sh" \
    "$capture_failure_path" "$TEST_TMP/capture-seam.marker" \
    > "$TEST_TMP/capture-seam.json" 2> "$TEST_TMP/capture-seam.err"
  capture_status=$?
  set -e
  [ -s "$TEST_TMP/capture-seam.marker" ] || fail 'capture-open failure did not reach the controlled launch seam'
  [ "$capture_status" -eq 0 ] || fail "capture-open fallback status = $capture_status, want 0"
  jq -s -e 'length == 1 and .[0].status == "fallback" and
    .[0].errors == [{"code":"BOOTSTRAP_FAILURE","message":"test-impact command could not be started"}]' \
    "$TEST_TMP/capture-seam.json" >/dev/null || fail 'capture-open failure did not emit one fallback document'
  [ "$(<"$TEST_TMP/capture-seam.err")" = 'test-impact: BOOTSTRAP_FAILURE: test-impact command could not be started' ] \
    || fail 'capture-open failure exposed a native path diagnostic'

  telemetry_bin="$TEST_TMP/telemetry-bin"
  mkdir -p "$telemetry_bin"
  cat > "$telemetry_bin/go" <<'EOF'
#!/bin/sh
if [ "$(cat "$HOME/.config/go/telemetry/mode" 2>/dev/null)" != off ]; then
  (
    sleep 0.05
    mkdir -p "$HOME/.config/go/telemetry/local"
    : > "$HOME/.config/go/telemetry/local/recreated"
  ) &
fi
exit 1
EOF
  chmod 0755 "$telemetry_bin/go"

  set +e
  env -i PATH=/usr/bin:/bin HOME="$TEST_TMP/launcher-home" /bin/bash -c '
    source "$1"
    SAFE_PATH="$2:/usr/bin:/bin"
    output_format=json
    run_main --format json >/dev/null 2>/dev/null
    owned_directory=$temp_directory
    cleanup
    sleep 0.2
    if [[ -e "$owned_directory" ]]; then
      cleanup
      trap - EXIT HUP INT TERM
      exit 91
    fi
    trap - EXIT HUP INT TERM
  ' cleanup-seam "$TEST_TMP/capture-seam-broken/dev/test-impact.sh" "$telemetry_bin"
  cleanup_status=$?
  set -e
  [ "$cleanup_status" -eq 0 ] || fail 'Go bootstrap telemetry recreated the private directory after cleanup'

  telemetry_failure_root="$TEST_TMP/telemetry-setup-failure"
  mkdir -p \
    "$telemetry_failure_root/home/.config/go" \
    "$telemetry_failure_root/tmp" \
    "$telemetry_failure_root/go-cache" \
    "$telemetry_failure_root/go-mod-cache"
  : > "$telemetry_failure_root/home/.config/go/telemetry"
  set +e
  env -i PATH=/usr/bin:/bin HOME="$TEST_TMP/launcher-home" /bin/bash -c '
    source "$1"
    fixture_directory=$2
    initialize_bootstrap() {
      temp_directory=$fixture_directory
      repository_root=/nonexistent
    }
    output_format=json
    run_main --format json
    status=$?
    cleanup
    trap - EXIT HUP INT TERM
    exit "$status"
  ' telemetry-failure "$TEST_TMP/capture-seam-broken/dev/test-impact.sh" "$telemetry_failure_root" \
    > "$TEST_TMP/telemetry-failure.json" 2> "$TEST_TMP/telemetry-failure.err"
  telemetry_status=$?
  set -e
  [ "$telemetry_status" -eq 0 ] || fail "telemetry setup failure status = $telemetry_status, want 0"
  jq -s -e 'length == 1 and .[0].status == "fallback" and
    .[0].errors == [{"code":"BOOTSTRAP_FAILURE","message":"test-impact command could not be started"}]' \
    "$TEST_TMP/telemetry-failure.json" >/dev/null || fail 'telemetry setup failure did not emit one fallback document'
  [ "$(<"$TEST_TMP/telemetry-failure.err")" = 'test-impact: BOOTSTRAP_FAILURE: test-impact command could not be started' ] \
    || fail 'telemetry setup failure exposed native stderr'

  toolchain_fixture="$TEST_TMP/toolchain-resolution"
  toolchain_repository="$toolchain_fixture/repository"
  toolchain_bin="$toolchain_fixture/base-bin"
  toolchain_cache_parent="$toolchain_fixture/cache-parent"
  mkdir -p "$toolchain_repository/dev" "$toolchain_repository/cmd/test-impact" \
    "$toolchain_bin" "$toolchain_cache_parent"
  cp "$ROOT/dev/test-impact.sh" "$toolchain_repository/dev/test-impact.sh"
  cp "$ROOT/go.mod" "$toolchain_repository/go.mod"
  cp "$ROOT/go.sum" "$toolchain_repository/go.sum"
  chmod 0755 "$toolchain_repository/dev/test-impact.sh"
  cat > "$toolchain_repository/cmd/test-impact/main.go" <<'EOF'
package main

func main() {}
EOF
  cat > "$toolchain_bin/go" <<'EOF'
#!/bin/bash
set -eu
[ "$1 $2" = 'env GOROOT' ] || exit 81
[ -f go.mod ] && [ -f cmd/test-impact/main.go ] || exit 85
grep -Eq '^go [0-9]+\.[0-9]+' go.mod || exit 86
grep -Eq '^toolchain go[0-9]+\.[0-9]+' go.mod || exit 87
selected_root="$GOMODCACHE/example.invalid/toolchain@v0.0.1"
if [ ! -x "$selected_root/bin/go" ]; then
  mkdir -p "$selected_root/bin"
  printf 'download\n' >> "$GOMODCACHE/downloads"
  cat > "$selected_root/bin/go" <<'SELECTED_GO'
#!/bin/bash
set -eu
[ "${GOTOOLCHAIN-}" = local ] || exit 82
case "$HOME:$TMPDIR:$GOCACHE:$GOMODCACHE" in
  /tmp/subyard-test-impact.*/home:/tmp/subyard-test-impact.*/tmp:/tmp/subyard-test-impact.*/go-cache:/tmp/subyard-test-impact.*/go-mod-cache) ;;
  *) exit 83 ;;
esac
selected_root="$(cd "$(dirname "$0")/.." && pwd -P)"
printf 'build\n' >> "$selected_root/builds"
output=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    output=$2
    break
  fi
  shift
done
[ -n "$output" ] || exit 84
cat > "$output" <<'BUILT_COMMAND'
#!/bin/sh
printf 'selected toolchain command\n'
BUILT_COMMAND
chmod 0700 "$output"
SELECTED_GO
  chmod 0700 "$selected_root/bin/go"
fi
printf '%s\n' "$selected_root"
EOF
  chmod 0755 "$toolchain_bin/go"

  for invocation in cold warm; do
    env -i PATH=/usr/bin:/bin HOME="$TEST_TMP/launcher-home" /bin/bash -c '
      source "$1"
      SAFE_PATH="$2:/usr/bin:/bin"
      test_impact_toolchain_cache_parent=$3
      run_main --format json
    ' toolchain-resolution "$toolchain_repository/dev/test-impact.sh" "$toolchain_bin" \
      "$toolchain_cache_parent" \
      > "$toolchain_fixture/$invocation.out" 2> "$toolchain_fixture/$invocation.err"
    [ "$(<"$toolchain_fixture/$invocation.out")" = 'selected toolchain command' ] \
      || fail "$invocation toolchain resolution did not start the selected command"
    [ ! -s "$toolchain_fixture/$invocation.err" ] \
      || fail "$invocation toolchain resolution exposed a bootstrap diagnostic"
  done
  persistent_toolchain_cache="$toolchain_cache_parent/subyard-test-impact-go-toolchain-$EUID"
  [ "$(stat -c '%u:%a' -- "$persistent_toolchain_cache")" = "$EUID:700" ] \
    || fail 'persistent toolchain cache owner or mode is not exact'
  [ "$(wc -l < "$persistent_toolchain_cache/downloads")" -eq 1 ] \
    || fail 'warm wrapper invocation repeated the selected toolchain download'
  [ "$(wc -l < "$persistent_toolchain_cache/example.invalid/toolchain@v0.0.1/builds")" -eq 2 ] \
    || fail 'wrapper did not build twice with the resolved selected toolchain'

  hostile_toolchain_parent="$toolchain_fixture/hostile-parent"
  hostile_toolchain_target="$toolchain_fixture/hostile-target"
  mkdir -p "$hostile_toolchain_parent" "$hostile_toolchain_target"
  printf 'untouched\n' > "$hostile_toolchain_target/sentinel"
  ln -s "$hostile_toolchain_target" \
    "$hostile_toolchain_parent/subyard-test-impact-go-toolchain-$EUID"
  env -i PATH=/usr/bin:/bin HOME="$TEST_TMP/launcher-home" /bin/bash -c '
    source "$1"
    SAFE_PATH="$2:/usr/bin:/bin"
    test_impact_toolchain_cache_parent=$3
    output_format=json
    run_main --format json
  ' hostile-toolchain-cache "$toolchain_repository/dev/test-impact.sh" "$toolchain_bin" \
    "$hostile_toolchain_parent" \
    > "$toolchain_fixture/hostile.json" 2> "$toolchain_fixture/hostile.err"
  jq -e '.status == "fallback" and .errors[0].code == "BOOTSTRAP_FAILURE"' \
    "$toolchain_fixture/hostile.json" >/dev/null \
    || fail 'hostile toolchain cache did not fail closed'
  [ "$(<"$toolchain_fixture/hostile.err")" = 'test-impact: BOOTSTRAP_FAILURE: test-impact command could not be started' ] \
    || fail 'hostile toolchain cache exposed native diagnostics'
  [ "$(<"$hostile_toolchain_target/sentinel")" = untouched ] \
    || fail 'wrapper followed a hostile persistent toolchain cache symlink'
  [ "$(find "$hostile_toolchain_target" -mindepth 1 -maxdepth 1 -printf '%f\n')" = sentinel ] \
    || fail 'wrapper mutated a hostile persistent toolchain cache target'

  wrong_mode_parent="$toolchain_fixture/wrong-mode-parent"
  wrong_mode_cache="$wrong_mode_parent/subyard-test-impact-go-toolchain-$EUID"
  mkdir -p "$wrong_mode_cache"
  chmod 0755 "$wrong_mode_cache"
  env -i PATH=/usr/bin:/bin HOME="$TEST_TMP/launcher-home" /bin/bash -c '
    source "$1"
    SAFE_PATH="$2:/usr/bin:/bin"
    test_impact_toolchain_cache_parent=$3
    output_format=json
    run_main --format json
  ' wrong-mode-toolchain-cache "$toolchain_repository/dev/test-impact.sh" "$toolchain_bin" \
    "$wrong_mode_parent" \
    > "$toolchain_fixture/wrong-mode.json" 2> "$toolchain_fixture/wrong-mode.err"
  jq -e '.status == "fallback" and .errors[0].code == "BOOTSTRAP_FAILURE"' \
    "$toolchain_fixture/wrong-mode.json" >/dev/null \
    || fail 'wrong-mode toolchain cache did not fail closed'
  [ "$(stat -c %a -- "$wrong_mode_cache")" = 755 ] \
    || fail 'wrapper changed permissions on a hostile persistent toolchain cache'
  [ "$(<"$toolchain_fixture/wrong-mode.err")" = 'test-impact: BOOTSTRAP_FAILURE: test-impact command could not be started' ] \
    || fail 'wrong-mode toolchain cache exposed native diagnostics'
}

if [ "${1-}" = --bootstrap-only ]; then
  test_wrapper_bootstrap_isolation
  printf 'PASS: test-impact bootstrap isolation\n'
  exit 0
fi

fixture="$TEST_TMP/repository"
mkdir -p "$fixture/internal" "$fixture/cmd" "$fixture/dev" "$fixture/tests"
printf '%s\n' 'module github.com/Subyard/Subyard' '' 'go 1.24' > "$fixture/go.mod"
cp -R "$ROOT/internal/testimpact" "$fixture/internal/"
cp -R "$ROOT/cmd/test-impact" "$fixture/cmd/"
cp "$ROOT/dev/test-impact.sh" "$fixture/dev/test-impact.sh"
cp "$ROOT/tests/impact-map.json" "$fixture/tests/impact-map.json"
chmod 0755 "$fixture/dev/test-impact.sh"

git -C "$fixture" init --quiet
git -C "$fixture" config user.name 'Test User'
git -C "$fixture" config user.email test@example.com
printf 'fixture\n' > "$fixture/README.md"
git -C "$fixture" add --all
git -C "$fixture" commit --quiet -m base
base="$(git -C "$fixture" rev-parse HEAD)"

mkdir -p "$fixture/internal/configsync"
printf 'package configsync\n' > "$fixture/internal/configsync/sync.go"
git -C "$fixture" add --all
git -C "$fixture" commit --quiet -m head
head="$(git -C "$fixture" rev-parse HEAD)"

direct_binary="$TEST_TMP/test-impact"
(cd "$fixture" && go build -mod=readonly -buildvcs=false -trimpath -o "$direct_binary" ./cmd/test-impact)
run_cli() {
  (cd "$fixture" && "$direct_binary" "$@")
}

change_file="$TEST_TMP/change set.json"
printf '%s\n' '{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":"internal/configsync/sync.go","old_mode":"000000","new_mode":"100644"}]}' > "$change_file"

current_json="$TEST_TMP/current.json"
commit_json="$TEST_TMP/commit.json"
file_json="$TEST_TMP/file.json"
stdin_json="$TEST_TMP/stdin.json"

run_cli --format json --current-base "$base" > "$current_json" 2> "$TEST_TMP/current.err"
run_cli --head "$head" --base "$base" --format json > "$commit_json" 2> "$TEST_TMP/commit.err"
run_cli --changes-from "$change_file" --format json > "$file_json" 2> "$TEST_TMP/file.err"
run_cli --format json --changes-from - < "$change_file" > "$stdin_json" 2> "$TEST_TMP/stdin.err"

for stderr_file in "$TEST_TMP/current.err" "$TEST_TMP/commit.err" "$TEST_TMP/file.err" "$TEST_TMP/stdin.err"; do
  [ ! -s "$stderr_file" ] || fail "selected source wrote a diagnostic"
done
cmp -s "$current_json" "$commit_json" || fail 'current and commit results differ'
cmp -s "$current_json" "$file_json" || fail 'current and file results differ'
cmp -s "$current_json" "$stdin_json" || fail 'current and stdin results differ'
jq -s -e 'length == 1 and .[0].schema_version == 1 and .[0].status == "selected" and (.[0].changes | length) == 1' \
  "$current_json" >/dev/null || fail 'selected stdout is not exactly one valid JSON result'

repeat_json="$TEST_TMP/repeat.json"
run_cli --current-base "$base" --format json > "$repeat_json" 2> "$TEST_TMP/repeat.err"
cmp -s "$current_json" "$repeat_json" || fail 'repeated output is not byte-identical'

fake_bin="$TEST_TMP/hostile-bin"
mkdir -p "$fake_bin"
printf '%s\n' '#!/bin/sh' 'printf "hostile go executed\n" >&2' 'exit 97' > "$fake_bin/go"
chmod 0755 "$fake_bin/go"
hostile_json="$TEST_TMP/hostile.json"
env \
  PATH="$fake_bin:/usr/bin:/bin" \
  HOME="$fixture/hostile-home" TMPDIR="$fixture/hostile-tmp" \
  GOFLAGS='-invalid-hostile-flag' GOWORK="$fixture/hostile.work" GOENV="$fixture/hostile.goenv" \
  GOCACHE="$fixture/hostile-cache" \
  GIT_DIR="$fixture/hostile.git" GIT_WORK_TREE="$fixture/hostile-tree" \
  GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.hostile.textconv GIT_CONFIG_VALUE_0=false \
  GIT_EXTERNAL_DIFF=false LC_ALL=definitely_invalid_locale LANG=another_invalid_locale \
  SUBYARD_SECRET_DO_NOT_READ='hostile-value' \
  "$fixture/dev/test-impact.sh" --changes-from "$change_file" --format json \
  > "$hostile_json" 2> "$TEST_TMP/hostile.err"
[ ! -s "$TEST_TMP/hostile.err" ] || fail 'hostile caller environment affected diagnostics'
cmp -s "$current_json" "$hostile_json" || fail 'hostile caller environment changed selection'

test_wrapper_bootstrap_isolation
test_targeted_adapter_runner

(cd "$ROOT" && go test ./internal/testimpact -run '^TestHistoricalCorpus' >/dev/null) \
  || fail 'strict historical corpus decoder failed'

corpus_conforms() {
  jq -e '
    def unique_strings:
      type == "array" and all(.[]; type == "string" and length > 0) and
      (length == (unique | length));
    def valid_change:
      (keys | sort) == ["new_mode", "new_path", "old_mode", "old_path", "similarity", "status"] and
      (.status | IN("A", "D", "M", "T", "R", "C")) and
      (.old_mode | type == "string" and test("^[0-7]{6}$")) and
      (.new_mode | type == "string" and test("^[0-7]{6}$")) and
      if .status == "A" then
        .similarity == null and .old_path == null and
        (.new_path | type == "string" and length > 0) and .old_mode == "000000"
      elif .status == "D" then
        .similarity == null and (.old_path | type == "string" and length > 0) and
        .new_path == null and .new_mode == "000000"
      elif (.status == "M" or .status == "T") then
        .similarity == null and (.old_path | type == "string" and length > 0) and
        .new_path == .old_path
      else
        (.similarity | type == "number" and floor == . and . >= 0 and . <= 100) and
        (.old_path | type == "string" and length > 0) and
        (.new_path | type == "string" and length > 0) and .old_path != .new_path
      end;
    def valid_case:
      (keys | sort) == ["category", "changes", "expected_full", "id", "required_check_sets", "required_e2e_checks"] and
      (.id | type == "string" and test("^history-[0-9]{2}$")) and
      (.category | IN("known_high_risk", "leaf")) and
      (.expected_full | type == "boolean") and
      (.changes | type == "array" and length > 0 and all(.[]; valid_change)) and
      (.required_check_sets | unique_strings) and
      (.required_e2e_checks | unique_strings) and
      (.category != "known_high_risk" or .expected_full == true);
    (keys | sort) == ["cases", "schema_version"] and
    .schema_version == 1 and
    (.cases | type == "array" and length >= 30 and length <= 50 and all(.[]; valid_case)) and
    ([.cases[].id] | length == (unique | length)) and
    ([.cases[].category] | index("known_high_risk") != null and index("leaf") != null)
  ' "$1" >/dev/null 2>&1
}

corpus_file="$ROOT/tests/fixtures/test-impact/corpus.json"
corpus_conforms "$corpus_file" || fail 'historical corpus is malformed, duplicate, empty, or out of contract'

jq '{schema_version, cases: []}' "$corpus_file" > "$TEST_TMP/corpus-empty.json"
jq '.cases += [.cases[0]]' "$corpus_file" > "$TEST_TMP/corpus-duplicate.json"
jq 'del(.cases[0].required_e2e_checks)' "$corpus_file" > "$TEST_TMP/corpus-missing-field.json"
jq '.cases[0].unexpected = true' "$corpus_file" > "$TEST_TMP/corpus-unknown-field.json"
jq '.cases[0].changes[0].status = "U"' "$corpus_file" > "$TEST_TMP/corpus-invalid-change.json"
for invalid_corpus in \
  "$TEST_TMP/corpus-empty.json" \
  "$TEST_TMP/corpus-duplicate.json" \
  "$TEST_TMP/corpus-missing-field.json" \
  "$TEST_TMP/corpus-unknown-field.json" \
  "$TEST_TMP/corpus-invalid-change.json"; do
  if corpus_conforms "$invalid_corpus"; then
    fail "corpus loader accepted invalid fixture $(basename "$invalid_corpus")"
  fi
done

known_high_risk_total=0
known_high_risk_full=0
leaf_total=0
leaf_avoids_full_and_universal=0
false_negative_full=0
missing_required=0
leaf_result_is_focused() {
  jq -e '
    .full_p0.required == false and
    ((.check_sets | index("host-free:all")) == null)
  ' "$1" >/dev/null
}

printf '%s\n' '{"full_p0":{"required":false},"check_sets":["host-free:all"],"host_free_checks":[]}' \
  > "$TEST_TMP/composite-selected.json"
if leaf_result_is_focused "$TEST_TMP/composite-selected.json"; then
  fail 'leaf focus predicate ignored an explicit host-free:all selection'
fi
printf '%s\n' '{"full_p0":{"required":false},"check_sets":["host-free:core"],"host_free_checks":[]}' \
  > "$TEST_TMP/composite-absent.json"
leaf_result_is_focused "$TEST_TMP/composite-absent.json" \
  || fail 'leaf focus predicate rejected a focused result without host-free:all'

while IFS= read -r corpus_case; do
  case_id="$(jq -r '.id' <<<"$corpus_case")"
  category="$(jq -r '.category' <<<"$corpus_case")"
  expected_full="$(jq -r '.expected_full' <<<"$corpus_case")"
  case_oracle="$TEST_TMP/$case_id-oracle.json"
  case_changes="$TEST_TMP/$case_id-changes.json"
  case_result="$TEST_TMP/$case_id-result.json"
  printf '%s\n' "$corpus_case" > "$case_oracle"
  jq '{schema_version: 1, changes}' "$case_oracle" > "$case_changes"
  run_cli --changes-from "$case_changes" --format json \
    > "$case_result" 2> "$TEST_TMP/$case_id.err"
  [ ! -s "$TEST_TMP/$case_id.err" ] || fail "$case_id emitted a selector diagnostic"
  jq -e '.status == "selected" and .errors == []' "$case_result" >/dev/null \
    || fail "$case_id did not produce a selected result"

  missing_sets="$(jq -r --slurpfile oracle "$case_oracle" '
    [$oracle[0].required_check_sets[] as $required |
      select(any(.check_sets[]; . == $required) | not) | $required] | join(",")
  ' "$case_result")"
  missing_e2e="$(jq -r --slurpfile oracle "$case_oracle" '
    [.e2e_checks[].id] as $actual |
    [$oracle[0].required_e2e_checks[] | select(. as $required | $actual | index($required) == null)] | join(",")
  ' "$case_result")"
  if [ -n "$missing_sets$missing_e2e" ]; then
    missing_required=$((missing_required + 1))
    fail "$case_id is missing required check sets [$missing_sets] or T3 checks [$missing_e2e]"
  fi

  actual_full="$(jq -r '.full_p0.required' "$case_result")"
  if [ "$expected_full" = true ] && [ "$actual_full" != true ]; then
    false_negative_full=$((false_negative_full + 1))
    fail "$case_id is a false-negative full-P0 decision"
  fi
  if [ "$category" = known_high_risk ]; then
    known_high_risk_total=$((known_high_risk_total + 1))
    if [ "$actual_full" = true ]; then
      known_high_risk_full=$((known_high_risk_full + 1))
    fi
  else
    leaf_total=$((leaf_total + 1))
    if leaf_result_is_focused "$case_result"; then
      leaf_avoids_full_and_universal=$((leaf_avoids_full_and_universal + 1))
    fi
  fi
done < <(jq -c '.cases[]' "$corpus_file")

[ "$missing_required" -eq 0 ] || fail "historical corpus has $missing_required missing-check false negatives"
[ "$false_negative_full" -eq 0 ] || fail "historical corpus has $false_negative_full full-P0 false negatives"
[ "$known_high_risk_full" -eq "$known_high_risk_total" ] \
  || fail "known high risk full-P0 coverage is $known_high_risk_full/$known_high_risk_total"
[ $((leaf_avoids_full_and_universal * 10)) -ge $((leaf_total * 7)) ] \
  || fail "focused leaf avoidance is $leaf_avoids_full_and_universal/$leaf_total; require at least 70%"

printf 'PASS: historical impact corpus (high-risk full %d/%d, leaf focused %d/%d, false negatives 0)\n' \
  "$known_high_risk_full" "$known_high_risk_total" "$leaf_avoids_full_and_universal" "$leaf_total"

unmatched_file="$TEST_TMP/unmatched.json"
printf '%s\n' '{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":"product/unmapped.go","old_mode":"000000","new_mode":"100644"}]}' > "$unmatched_file"
fallback_json="$TEST_TMP/fallback.json"
run_cli --format json --changes-from "$unmatched_file" \
  > "$fallback_json" 2> "$TEST_TMP/fallback.err"
jq -s -e 'length == 1 and .[0].status == "fallback" and .[0].check_sets == ["host-free:all"] and
  .[0].risk_domains == [] and .[0].e2e_checks == [] and .[0].full_p0.required == true and
  .[0].errors == [{"code":"UNMATCHED_PATH","message":"a changed repository path is not covered by impact policy"}]' \
  "$fallback_json" >/dev/null || fail 'analysis fallback JSON contract failed'
[ "$(<"$TEST_TMP/fallback.err")" = 'test-impact: UNMATCHED_PATH: a changed repository path is not covered by impact policy' ] \
  || fail 'analysis fallback diagnostic was not sanitized'
if grep -Fq 'product/unmapped.go' "$TEST_TMP/fallback.err"; then
  fail 'analysis fallback diagnostic leaked a repository path'
fi

control_file="$TEST_TMP/control.json"
printf '%s\n' '{"schema_version":1,"changes":[{"status":"A","similarity":null,"old_path":null,"new_path":"product/line\nnext\t\u001b[31m.go","old_mode":"000000","new_mode":"100644"}]}' > "$control_file"
run_cli --changes-from "$control_file" > "$TEST_TMP/control.out" 2> "$TEST_TMP/control.err"
if LC_ALL=C grep -q $'\033' "$TEST_TMP/control.out" || LC_ALL=C grep -q $'\033' "$TEST_TMP/control.err"; then
  fail 'human output emitted a raw terminal escape'
fi
grep -Fq '"product/line\nnext\t\x1b[31m.go"' "$TEST_TMP/control.out" \
  || fail 'human output did not visibly escape controls'

set +e
run_cli --unknown --format json > "$TEST_TMP/misuse.json" 2> "$TEST_TMP/misuse.err"
misuse_status=$?
set -e
[ "$misuse_status" -eq 2 ] || fail "CLI misuse status changed to $misuse_status"
[ "$(<"$TEST_TMP/misuse.json")" = '{"schema_version":1,"status":"error","errors":[{"code":"CLI_MISUSE","message":"invalid command line"}]}' ] \
  || fail 'CLI misuse did not emit the minimal JSON result'
[ "$(<"$TEST_TMP/misuse.err")" = 'test-impact: CLI_MISUSE: invalid command line' ] \
  || fail 'CLI misuse diagnostic changed'

broken="$TEST_TMP/broken-repository"
mkdir -p "$broken/dev"
cp "$ROOT/dev/test-impact.sh" "$broken/dev/test-impact.sh"
chmod 0755 "$broken/dev/test-impact.sh"
set +e
"$broken/dev/test-impact.sh" --unknown --format json > "$TEST_TMP/bootstrap.json" 2> "$TEST_TMP/bootstrap.err"
bootstrap_status=$?
set -e
[ "$bootstrap_status" -eq 0 ] || fail "bootstrap fallback status = $bootstrap_status, want 0"
jq -s -e 'length == 1 and .[0].schema_version == 1 and .[0].status == "fallback" and
  .[0].changes == [] and .[0].check_sets == ["host-free:all"] and .[0].risk_domains == [] and
  .[0].e2e_checks == [] and .[0].full_p0.required == true and
  .[0].errors == [{"code":"BOOTSTRAP_FAILURE","message":"test-impact command could not be started"}] and
  (.[0].host_free_checks | length) == 5 and
  all(.[0].host_free_checks[]; has("id") and has("tier") and has("budget_seconds") and has("rationale"))' \
  "$TEST_TMP/bootstrap.json" >/dev/null || fail 'bootstrap fallback JSON contract failed'
[ "$(<"$TEST_TMP/bootstrap.err")" = 'test-impact: BOOTSTRAP_FAILURE: test-impact command could not be started' ] \
  || fail 'bootstrap stderr exposed compiler or path details'

go_fallback_ids="$(jq -c '[.host_free_checks[].id]' "$fallback_json")"
shell_fallback_ids="$(jq -c '[.host_free_checks[].id]' "$TEST_TMP/bootstrap.json")"
[ "$go_fallback_ids" = "$shell_fallback_ids" ] \
  || fail "shell fallback leaves drifted from Go composite: $shell_fallback_ids != $go_fallback_ids"

"$broken/dev/test-impact.sh" --format human --unknown > "$TEST_TMP/bootstrap-human.out" 2> "$TEST_TMP/bootstrap-human.err"
grep -Fq 'status: "fallback"' "$TEST_TMP/bootstrap-human.out" || fail 'bootstrap human fallback is missing status'
grep -Fq 'code="BOOTSTRAP_FAILURE"' "$TEST_TMP/bootstrap-human.out" || fail 'bootstrap human fallback is missing error code'
[ "$(<"$TEST_TMP/bootstrap-human.err")" = 'test-impact: BOOTSTRAP_FAILURE: test-impact command could not be started' ] \
  || fail 'bootstrap human stderr changed'

[ -z "$(git -C "$fixture" status --porcelain=v1 --untracked-files=all)" ] \
  || fail 'wrapper changed its trusted worktree'

printf 'PASS: test-impact wrapper and CLI contract\n'

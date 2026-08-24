#!/usr/bin/env -S -i PATH=/usr/bin:/bin LC_ALL=C LANG=C /bin/bash
# shellcheck shell=bash
set -uo pipefail
umask 077

SAFE_PATH='/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin'
test_impact_toolchain_cache_parent=/var/tmp
PATH="$SAFE_PATH"
LC_ALL=C
LANG=C
export PATH LC_ALL LANG
unset CDPATH

output_format=human
expect_format=false
for argument in "$@"; do
  if $expect_format; then
    case "$argument" in
      json) output_format=json ;;
      human) output_format=human ;;
    esac
    expect_format=false
  elif [ "$argument" = --format ]; then
    expect_format=true
  fi
done

emit_bootstrap_fallback() {
  if [ "$output_format" = json ]; then
    printf '%s\n' '{"schema_version":1,"status":"fallback","changes":[],"check_sets":["host-free:all"],"risk_domains":[],"host_free_checks":[{"id":"host-free:core","tier":"T2","budget_seconds":1800,"rationale":"required core host-free merge gate"},{"id":"veranda:build","tier":"T1","budget_seconds":180,"rationale":"Veranda production build"},{"id":"veranda:check","tier":"T1","budget_seconds":180,"rationale":"Veranda static checks"},{"id":"veranda:rust-test","tier":"T1","budget_seconds":300,"rationale":"Veranda Rust tests without desktop dependencies"},{"id":"veranda:test","tier":"T1","budget_seconds":180,"rationale":"Veranda unit tests"}],"e2e_checks":[],"full_p0":{"required":true,"reasons":[{"code":"universal_fallback","risk_domains":[]}]},"reasons":[],"errors":[{"code":"BOOTSTRAP_FAILURE","message":"test-impact command could not be started"}]}'
  else
    printf '%s\n' \
      'schema version: 1' \
      'status: "fallback"' \
      'changes:' \
      'check sets:' \
      '  - "host-free:all"' \
      'risk domains:' \
      'host-free checks:' \
      '  - id="host-free:core" tier="T2" budget_seconds=1800 rationale="required core host-free merge gate"' \
      '  - id="veranda:build" tier="T1" budget_seconds=180 rationale="Veranda production build"' \
      '  - id="veranda:check" tier="T1" budget_seconds=180 rationale="Veranda static checks"' \
      '  - id="veranda:rust-test" tier="T1" budget_seconds=300 rationale="Veranda Rust tests without desktop dependencies"' \
      '  - id="veranda:test" tier="T1" budget_seconds=180 rationale="Veranda unit tests"' \
      'e2e checks:' \
      'full P0 required: true' \
      'full P0 reasons:' \
      '  - code="universal_fallback" risk_domains=[]' \
      'selection reasons:' \
      'errors:' \
      '  - code="BOOTSTRAP_FAILURE" message="test-impact command could not be started"'
  fi
  printf '%s\n' 'test-impact: BOOTSTRAP_FAILURE: test-impact command could not be started' >&2
}

temp_directory=''
cleanup() {
  if [ -n "$temp_directory" ]; then
    command rm -rf -- "$temp_directory" >/dev/null 2>&1 || :
  fi
}

initialize_bootstrap() {
  script_directory="$(builtin cd -- "$(command dirname -- "${BASH_SOURCE[0]}")" && builtin pwd -P)" || return
  repository_root="$(builtin cd -- "$script_directory/.." && builtin pwd -P)" || return
  temp_directory="$(command mktemp -d /tmp/subyard-test-impact.XXXXXX)" || return
  command mkdir -p \
    "$temp_directory/home" \
    "$temp_directory/tmp" \
    "$temp_directory/go-cache" \
    "$temp_directory/go-mod-cache" || return
}

initialize_toolchain_cache() {
  local cache_metadata

  toolchain_cache_directory="$test_impact_toolchain_cache_parent/subyard-test-impact-go-toolchain-$EUID"
  if ! command mkdir -m 0700 -- "$toolchain_cache_directory" 2>/dev/null; then
    [ ! -L "$toolchain_cache_directory" ] && [ -d "$toolchain_cache_directory" ] || return
  fi
  [ ! -L "$toolchain_cache_directory" ] || return
  cache_metadata="$(command stat -c '%u:%a' -- "$toolchain_cache_directory")" || return
  [ "$cache_metadata" = "$EUID:700" ]
}

invoke_started_binary() {
  local binary_stderr=$1
  local capture_fd
  local status
  shift

  if ! { exec {capture_fd}> "$binary_stderr"; } 2>/dev/null; then
    emit_bootstrap_fallback
    return 0
  fi

  (
    builtin cd -- "$repository_root" || exit 127
    /usr/bin/env -i \
      PATH="$SAFE_PATH" \
      HOME="$temp_directory/home" \
      TMPDIR="$temp_directory/tmp" \
      GOCACHE="$cache_directory" \
      GOMODCACHE="$module_cache_directory" \
      LC_ALL=C LANG=C \
      GOFLAGS= GOWORK=off GOENV=off CGO_ENABLED=0 \
      "$temp_directory/test-impact" "$@"
  ) 2>&"$capture_fd"
  status=$?
  { exec {capture_fd}>&-; } 2>/dev/null || :

  if [ "$status" -eq 126 ] || [ "$status" -eq 127 ]; then
    emit_bootstrap_fallback
    return 0
  fi
  command cat -- "$binary_stderr" >&2 2>/dev/null || :
  return "$status"
}

run_base_go_for_toolchain() {
  /usr/bin/env -i \
    PATH="$SAFE_PATH" \
    HOME="$temp_directory/home" \
    TMPDIR="$temp_directory/tmp" \
    GOCACHE="$cache_directory" \
    GOMODCACHE="$toolchain_cache_directory" \
    LC_ALL=C LANG=C \
    GOFLAGS= GOWORK=off GOENV=off CGO_ENABLED=0 \
    go "$@"
}

resolve_selected_go() {
  local selected_goroot
  local canonical_goroot
  local canonical_go

  selected_goroot="$(
    builtin cd -- "$repository_root" &&
      run_base_go_for_toolchain env GOROOT
  )" || return
  case "$selected_goroot" in
    /*) ;;
    *) return 1 ;;
  esac
  [ -d "$selected_goroot" ] && [ ! -L "$selected_goroot" ] || return
  canonical_goroot="$(builtin cd -- "$selected_goroot" && builtin pwd -P)" || return
  [ "$selected_goroot" = "$canonical_goroot" ] || return
  canonical_go="$(command readlink -f -- "$canonical_goroot/bin/go")" || return
  case "$canonical_go" in
    "$canonical_goroot"/*) ;;
    *) return 1 ;;
  esac
  [ -f "$canonical_go" ] && [ -x "$canonical_go" ] || return
  selected_go=$canonical_go
}

run_selected_go() {
  /usr/bin/env -i \
    PATH="$SAFE_PATH" \
    HOME="$temp_directory/home" \
    TMPDIR="$temp_directory/tmp" \
    GOCACHE="$cache_directory" \
    GOMODCACHE="$module_cache_directory" \
    LC_ALL=C LANG=C \
    GOFLAGS= GOWORK=off GOENV=off CGO_ENABLED=0 GOTOOLCHAIN=local \
    "$selected_go" "$@"
}

initialize_go_telemetry() {
  local telemetry_directory="$temp_directory/home/.config/go/telemetry"
  command mkdir -p -- "$telemetry_directory" &&
    builtin printf '%s' off > "$telemetry_directory/mode"
}

run_main() {
  temp_directory=''
  trap cleanup EXIT HUP INT TERM

  if ! initialize_bootstrap 2>/dev/null; then
    emit_bootstrap_fallback
    return 0
  fi

  cache_directory="$temp_directory/go-cache"
  module_cache_directory="$temp_directory/go-mod-cache"

  if ! initialize_go_telemetry 2>/dev/null; then
    emit_bootstrap_fallback
    return 0
  fi

  if ! initialize_toolchain_cache 2>/dev/null; then
    emit_bootstrap_fallback
    return 0
  fi

  if ! resolve_selected_go >/dev/null 2>&1; then
    emit_bootstrap_fallback
    return 0
  fi

  if ! (
    builtin cd -- "$repository_root" &&
      run_selected_go build -mod=readonly -buildvcs=false -trimpath \
        -o "$temp_directory/test-impact" ./cmd/test-impact
  ) >/dev/null 2>&1; then
    emit_bootstrap_fallback
    return 0
  fi

  invoke_started_binary "$temp_directory/test-impact.stderr" "$@"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  run_main "$@"
  exit $?
fi

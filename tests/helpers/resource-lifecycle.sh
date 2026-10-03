#!/usr/bin/env bash
# Generic assertions for profile-owned resource fixtures. Callers supply handlers and transport state.

check_resource_usage() {
  local handler="$1" resource_rc=0
  shift
  : > "$RESOURCE_TEST_LOG"
  SUBYARD_RESOURCE_MODE=prepare "$handler" "$@" >"$TMP/resource-usage.out" 2>&1 || resource_rc=$?
  [ "$resource_rc" -eq 2 ] || fail "$handler $* returned $resource_rc instead of usage code 2"
  [ ! -s "$RESOURCE_TEST_LOG" ] || fail "$handler invalid arguments reached a physical probe"
}

check_resource_help() {
  local handler="$1" verb="${2:-status}"
  : > "$RESOURCE_TEST_LOG"
  "$handler" --help >"$TMP/resource-help.out" 2>&1 || fail "$handler help failed"
  [ ! -s "$RESOURCE_TEST_LOG" ] || fail "$handler help reached a physical probe"
  check_resource_usage "$handler" "$verb" unexpected
}

check_resource_stopped() {
  local handler="$1" resource_rc=0
  mv "$TMP/up" "$TMP/stopped"
  SUBYARD_RESOURCE_MODE=prepare "$handler" down >"$TMP/resource-stopped.out" 2>&1 || resource_rc=$?
  [ "$resource_rc" -eq 1 ] || fail "$handler precondition returned $resource_rc instead of 1"
  mv "$TMP/stopped" "$TMP/up"
}

check_resource_probe() {
  local handler="$1" output
  output="$("$handler" is-up)"
  [ -z "$output" ] || fail "$handler is-up probe was not silent"
  rm -f "$TMP/up"
  if "$handler" is-up >"$TMP/resource-probe.out" 2>&1; then
    fail "$handler probe accepted a down resource"
  fi
  [ ! -s "$TMP/resource-probe.out" ] || fail "$handler down probe emitted output"
  touch "$TMP/up"
}

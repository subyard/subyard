#!/usr/bin/env bash
# The emulator lifecycle must not mistake unrelated commands for its process tree.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../" && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=config/profiles/android/resources/emulator/process-identity.sh
. "$ROOT/config/profiles/android/resources/emulator/process-identity.sh"

expect_match() {
  emulator_process_cmdline_matches "$1" || fail "real emulator command did not match: $1"
}
expect_no_match() {
  if emulator_process_cmdline_matches "$1"; then
    fail "unrelated command matched emulator identity: $1"
  fi
}

# Every real phase is covered, including the short launcher-to-cage transition.
expect_match 'bash /tmp/subyard-android/emulator-run.sh'
expect_match '/usr/bin/bash /tmp/subyard-android/emulator-run.sh yardtest -wipe-data'
expect_match 'cage -- /srv/cache/android-sdk/emulator/emulator -avd yardtest'
expect_match '/usr/bin/cage -- /srv/cache/android-sdk/emulator/emulator -avd yardtest'
expect_match '/srv/cache/android-sdk/emulator/emulator -avd yardtest'
expect_match '/srv/cache/android-sdk/emulator/qemu/linux-x86_64/qemu-system-x86_64 -avd yardtest'

# Regression: this was observed in the yard. Mentioning a source path is not process identity.
expect_no_match 'shellcheck -x -S warning bin/yard config/profiles/android/emulator-run.sh tests/run.sh'
expect_no_match '/usr/bin/shellcheck -x -S warning config/profiles/android/emulator-run.sh'
expect_no_match 'rg emulator-run.sh config/profiles/android'
expect_no_match 'bash -lc cat /tmp/subyard-android/emulator-run.sh'
expect_no_match 'tail -n 20 /tmp/subyard-android-emu.log'
expect_no_match 'qemu-system-x86_64 -machine none'

printf 'ok: emulator process identity is exact and ignores path mentions\n'

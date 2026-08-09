#!/usr/bin/env bash
# Static contract for the real-VM Hermes boundary lane.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
E2E="$ROOT/dev/e2e/hermes-profile.sh"
PRESET="$ROOT/config/profiles/hermes/yard.env"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

for setting in \
  'YARD_PROFILES=hermes' \
  'AGENTS=codex' \
  'HOST_CLAUDE_MD=' \
  'HOST_CODEX_AGENTS_MD=' \
  'HOST_OPENCODE_AGENTS_MD=' \
  'HOST_MOUNTS=' \
  'HOST_LINKS=' \
  'YARD_CAPABILITIES=' \
  'YARD_CAPS=' \
  'YARD_DEVICES=' \
  'YARD_MOUNTS=' \
  'FORWARD_SSH_AGENT=0' \
  'DEV_SUDO=0' \
  'NESTED_E2E_VMS=0'; do
  grep -Fxq "$setting" "$PRESET" || fail "Hermes preset omits $setting"
done

grep -Fq 'mktemp -d /var/tmp/subyard-hermes-profile.XXXXXX' "$E2E" \
  || fail "Hermes E2E keeps Incus storage on the bounded /tmp filesystem"
grep -Fq '/tmp/subyard-hermes-profile.*/storage|/var/tmp/subyard-hermes-profile.*/storage' "$E2E" \
  || fail "Hermes E2E cannot recover legacy and current fixture pools"
grep -Fq '[ -z "$want" ] && [ "$got" = "<unset>" ]' "$E2E" \
  || fail "Hermes E2E rejects the CLI representation of an empty setting"

grep -Fq 'yard "$SOURCE_YARD" init --profile hermes --yes' "$E2E" \
  || fail "source lane does not bootstrap from the shipped Hermes preset"
grep -Fq 'yard "$RESTORE_YARD" init --profile hermes --yes' "$E2E" \
  || fail "restore lane does not bootstrap from the shipped Hermes preset"
grep -Fq 'cmp "$ROOT/config/profiles/hermes/yard.env" "$source_definition"' "$E2E" \
  || fail "real lane does not verify the persisted yard preset"
grep -Fq 'stat -c %a "$source_definition"' "$E2E" \
  || fail "real lane does not verify the persisted yard definition mode"
if grep -Eq 'write_yard_config|cat >.*yards/.*/config\.env' "$E2E"; then
  fail "Hermes E2E writes a named-yard definition directly"
fi
if grep -Fq 'SSH_PORT="$port"' "$E2E"; then
  fail "Hermes E2E leaks a selected-yard SSH port into all preflight yard contexts"
fi
grep -Fq "'YARD_PROFILES=openclaw'" "$E2E" \
  || fail "Hermes E2E does not seed a hostile host profile selection"
grep -Fq "'AGENTS=claude'" "$E2E" \
  || fail "Hermes E2E does not seed a hostile host agent selection"
grep -Fq 'HOST_MOUNTS=host-fixture:/mnt/host-fixture:ro:0755' "$E2E" \
  || fail "Hermes E2E does not seed a hostile host mount"
grep -Fq "'YARD_CAPABILITIES=android'" "$E2E" \
  || fail "Hermes E2E does not seed hostile host capabilities"
if grep -Eq '^export (YARD_PROFILES|AGENTS|DEV_SUDO|FORWARD_SSH_AGENT|NESTED_E2E_VMS)=' \
  "$E2E"; then
  fail "Hermes E2E masks the shipped preset with command environment values"
fi

grep -Fq 'yard "$SOURCE_YARD" provision --yes' "$E2E" \
  || fail "source lane bypasses no-argument profile selection"
grep -Fq 'yard "$RESTORE_YARD" provision --yes' "$E2E" \
  || fail "restore lane bypasses no-argument profile selection"
grep -Fq 'codex login status' "$E2E" \
  || fail "secure lane does not verify yard-local Codex login"
grep -Fq 'codex-check' "$E2E" \
  || fail "real lane does not exercise the Codex package check"
grep -Fq 'registry.dispatch(' "$E2E" \
  || fail "real lane does not dispatch the browser through Hermes"
grep -Fq 'Playwright Chromium not installed' "$E2E" \
  || fail "real lane does not reject Hermes doctor when Chromium is hidden"
grep -Fq 'assert_hermes_browser_dispatch "$source_instance" "$source_project"' "$E2E" \
  || fail "source lane omits the Hermes browser dispatch proof"
grep -Fq 'assert_hermes_browser_dispatch "$restore_instance" "$restore_project"' "$E2E" \
  || fail "restore lane omits the Hermes browser dispatch proof"
grep -Fq 'shell --root -- hermes-provider-ready --inference-ok' "$E2E" \
  || fail "provider approval bypasses the public root shell"
grep -Fq "case \"\$device\" in host-*|yx-*" "$E2E" \
  || fail "real lane does not reject host and extras devices"
grep -Fq 'command -v tailscale' "$E2E" \
  || fail "real lane does not reject an in-yard Tailscale client"
grep -Fq '/home/dev/.config/opencode' "$E2E" \
  || fail "real lane does not reject unselected OpenCode state"
grep -Fq 'restic ls "$2" > "$3"' "$E2E" \
  || fail "real lane does not capture the complete restic listing"
if grep -Fq 'restic ls "$2" | grep' "$E2E"; then
  fail "real lane exposes restic verification to SIGPIPE"
fi
grep -Fq 'install -m 0600 -o "$(id -u)" -g "$(id -g)"' "$E2E" \
  || fail "restore archive is not safely staged for the unprivileged Incus client"

printf 'ok: Hermes E2E boundary contract\n'

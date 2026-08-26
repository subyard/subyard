#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

agents="$(
  SUBYARD_CONFIG_DIR="$ROOT/config" \
    bash -c '. "$1"; printf "%s\n%s\n%s\n%s\n%s\n" \
      "$CODING_TOOL_INTEGRATIONS" "$AGENT_paseo_PROVISION" "$AGENT_paseo_COMMAND" \
      "$AGENT_paseo_CHECK" "$AGENT_paseo_PROJECTS_CHANGED"' _ "$ROOT/config/agents.env"
)"
mapfile -t values <<<"$agents"
[[ " ${values[0]} " != *" paseo "* ]] || fail "Paseo entered the shipped default CODING_TOOL_INTEGRATIONS"
[ "${values[1]}" = "$ROOT/config/agents/paseo/provision.sh" ] || fail "wrong Paseo provision hook"
[ "${values[2]}" = paseo ] || fail "wrong Paseo command"
[ "${values[3]}" = paseo-check ] || fail "wrong Paseo check"
[ "${values[4]}" = paseo-sync-projects ] || fail "wrong Paseo project hook"
[ "$(SUBYARD_CONFIG_DIR="$ROOT/config" bash -c '. "$1"; printf "%s" "$AGENT_paseo_DEPENDS"' _ "$ROOT/config/agents.env")" = codex ] \
  || fail "Paseo does not declare its Codex dependency"

jq -e '
  .version == 1 and .daemon.listen == "127.0.0.1:6767" and
  .daemon.relay == {
    enabled: true,
    endpoint: "relay.paseo.sh:443",
    publicEndpoint: "relay.paseo.sh:443",
    useTls: true,
    publicUseTls: true
  } and
  .app.baseUrl == "https://app.paseo.sh" and
  .features.webUi.enabled == false
' "$ROOT/config/agents/paseo/config.json" >/dev/null || fail "Paseo config contract drift"

lock="$ROOT/config/agents/paseo/bundle/package-lock.json"
[ "$(jq -r '.packages["node_modules/@getpaseo/cli"].version' "$lock")" = 0.2.1 ] \
  || fail "Paseo CLI lock drift"
[ "$(jq -r '.packages["node_modules/node-pty"].version' "$lock")" = 1.2.0-beta.11 ] \
  || fail "node-pty lock drift"
[ "$(jq -r '.packages["node_modules/sherpa-onnx-node"].version' "$lock")" = 1.12.28 ] \
  || fail "Sherpa lock drift"
jq -e '
  [.packages | to_entries[] |
    select(.key | endswith("/@anthropic-ai/claude-agent-sdk")) |
    .value.version] == ["0.3.214"]
' "$lock" >/dev/null || fail "Claude Agent SDK lock drift"

for workflow in ci.yml release.yml; do
  grep -Eq 'apt-get install .* ripgrep( |$)' "$ROOT/.github/workflows/$workflow" \
    || fail "$workflow does not install the ripgrep test dependency"
done

rg -q '^ExecStart=.*--listen 127[.]0[.]0[.]1:6767 .*--relay-use-tls .*--no-web-ui$' \
  "$ROOT/config/agents/paseo/paseo.service.in" || fail "unit listener/relay contract drift"
rg -q '^ExecStartPost=/usr/local/bin/paseo-sync-projects --wait --force$' \
  "$ROOT/config/agents/paseo/paseo.service.in" || fail "startup sync has no bounded readiness wait"
rg -q '^ReadWritePaths=@DEV_HOME@ /srv/agents/paseo /srv/workspaces$' \
  "$ROOT/config/agents/paseo/paseo.service.in" || fail "provider state is not writable"
! rg -qi 'updat|0[.]0[.]0[.]0|publicBaseUrl|serviceProxy' \
  "$ROOT/config/agents/paseo/paseo.service.in" || fail "unit enabled a forbidden surface"
rg -q 'PASEO_RELEASE_VERSION' "$ROOT/config/agents/paseo/provision.sh" \
  || fail "provision is not tied to the Subyard release"
rg -q 'PASEO_RELEASE_REPOSITORY:-Subyard/Subyard' "$ROOT/config/agents/paseo/provision.sh" \
  || fail "Paseo does not use the canonical release repository"
rg -q 'files[.]sha256' "$ROOT/config/agents/paseo/provision.sh" \
  || fail "provision does not verify the deploy closure"
rg -q 'PASEO_HEALTH_WAIT_SECONDS' "$ROOT/config/agents/paseo/bin/paseo-check" \
  || fail "readiness has no bounded local health wait"
rg -q 'PASEO_UNIT_WAIT_SECONDS' "$ROOT/config/agents/paseo/bin/paseo-check" \
  || fail "readiness has no bounded systemd activation wait"
rg -q 'until systemctl is-active --quiet' "$ROOT/config/agents/paseo/bin/paseo-check" \
  || fail "readiness rejects the normal systemd activating state"
rg -q 'as_dev /usr/local/bin/codex-check' "$ROOT/config/agents/paseo/bin/paseo-check" \
  || fail "readiness does not verify the canonical Codex package"
rg -q 'CODEX_HOME=' "$ROOT/config/agents/paseo/bin/paseo-check" \
  || fail "readiness does not give Codex its exact user state directory"
codex_check_line="$(rg -n 'as_dev /usr/local/bin/codex-check' \
  "$ROOT/config/agents/paseo/bin/paseo-check" | cut -d: -f1)"
pair_check_line="$(rg -n 'daemon pair .*--json' \
  "$ROOT/config/agents/paseo/bin/paseo-check" | cut -d: -f1)"
[ "$codex_check_line" -lt "$pair_check_line" ] \
  || fail "Paseo generates a pairing offer before Codex capability readiness"
rg -q 'ubuntu-24[.]04-arm' "$ROOT/.github/workflows/release.yml" \
  || fail "release has no native arm64 Paseo lane"
! rg -qi 'paseo' "$ROOT/config/commands.registry" "$ROOT/internal/cli" \
  "$ROOT/internal/domain" "$ROOT/internal/rpc" "$ROOT/scripts/04-provision-subyard.sh" \
  || fail "Paseo-specific core CLI/domain/RPC plumbing appeared"

wrapper_temp="$(mktemp -d)"
cleanup_wrapper() { rm -rf -- "$wrapper_temp"; }
trap cleanup_wrapper EXIT HUP INT TERM
mkdir -p "$wrapper_temp/bin" "$wrapper_temp/check-bin" "$wrapper_temp/runtime/node/bin" \
  "$wrapper_temp/runtime/app/libexec" "$wrapper_temp/home"

# The documented SSH helper runs directly as dev, whose PATH need not include /usr/sbin/runuser.
for command in bash curl grep id journalctl jq mktemp readlink sha256sum ss stat systemctl; do
  executable="$(command -v "$command")"
  ln -s "$executable" "$wrapper_temp/check-bin/$command"
done
set +e
check_output="$(PATH="$wrapper_temp/check-bin" PASEO_DEV_USER="$(id -un)" \
  PASEO_INSTALL_ROOT="$wrapper_temp/missing" \
  "$ROOT/config/agents/paseo/bin/paseo-check" 2>&1)"
check_status=$?
set -e
[ "$check_status" -ne 0 ] || fail "incomplete Paseo fixture unexpectedly passed readiness"
[[ "$check_output" == *"active runtime link is missing"* ]] \
  || fail "dev-side paseo-check incorrectly requires runuser: $check_output"

ln -s "$wrapper_temp/runtime" "$wrapper_temp/current"
touch "$wrapper_temp/runtime/app/libexec/paseo-sync-projects.mjs"
cat >"$wrapper_temp/runtime/node/bin/node" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >"$PASEO_FAKE_NODE_LOG"
SH
cat >"$wrapper_temp/bin/curl" <<'SH'
#!/bin/sh
count=0
[ ! -r "$PASEO_FAKE_CURL_STATE" ] || count="$(cat "$PASEO_FAKE_CURL_STATE")"
count=$((count + 1))
printf '%s\n' "$count" >"$PASEO_FAKE_CURL_STATE"
[ "${PASEO_FAKE_CURL_NEVER:-0}" != 1 ] && [ "$count" -ge 3 ]
SH
chmod 0755 "$wrapper_temp/runtime/node/bin/node" "$wrapper_temp/bin/curl"
PASEO_FAKE_CURL_STATE="$wrapper_temp/curl-count" \
PASEO_FAKE_NODE_LOG="$wrapper_temp/node.log" \
PASEO_INSTALL_ROOT="$wrapper_temp/current" PASEO_HOME="$wrapper_temp/home" \
PASEO_SYNC_WAIT_SECONDS=5 PATH="$wrapper_temp/bin:$PATH" \
  "$ROOT/config/agents/paseo/bin/paseo-sync-projects" --wait --force
[ "$(cat "$wrapper_temp/curl-count")" -eq 3 ] \
  && grep -Fq 'paseo-sync-projects.mjs --force' "$wrapper_temp/node.log" \
  || fail "startup sync did not wait for health or consumed the wrong arguments"
rm -f "$wrapper_temp/node.log" "$wrapper_temp/curl-count"
if PASEO_FAKE_CURL_NEVER=1 PASEO_FAKE_CURL_STATE="$wrapper_temp/curl-count" \
  PASEO_FAKE_NODE_LOG="$wrapper_temp/node.log" \
  PASEO_INSTALL_ROOT="$wrapper_temp/current" PASEO_HOME="$wrapper_temp/home" \
  PASEO_SYNC_WAIT_SECONDS=1 PATH="$wrapper_temp/bin:$PATH" \
    "$ROOT/config/agents/paseo/bin/paseo-sync-projects" --wait --force >/dev/null 2>&1; then
  fail "startup sync accepted a daemon that never became healthy"
fi
[ ! -e "$wrapper_temp/node.log" ] || fail "startup sync ran after its health deadline"

printf 'PASS: Paseo remains an opt-in generic agent package\n'

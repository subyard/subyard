#!/usr/bin/env bash
# Host-free release artifact, checksum, atomic upgrade and rollback contract.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
release="$TMP/release"
export SUBYARD_INCUS_SOCKET="$TMP/missing-incus.socket"
export SUBYARD_OPERATOR_HOME="$TMP/home"
export SUBYARD_CONFIG_HOME="$TMP/config"
export SUBYARD_HOME="$TMP/data"
export SUBYARD_POWER_RECONCILER_PATH="$TMP/missing-power-reconciler"
export SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit"

fail() { printf 'engine release: %s\n' "$*" >&2; exit 1; }
yard_update() { YARD_ENGINE_PATH="$release_engine" "$ROOT/bin/yard" update --yes "$@"; }
installed_update() {
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/cache" \
    "$runtime_root/current/bin/yard" update --yes "$@"
}

workflow="$ROOT/.github/workflows/release.yml"
[ -r "$workflow" ] || fail 'tag release workflow is missing'
grep -Fq 'contents: write' "$workflow" \
  && grep -Fq 'for arch in amd64 arm64' "$workflow" \
  && grep -Fq 'dev/release-assets.sh --release-dir .build/release' "$workflow" \
  && grep -Fq 'gh release create "$GITHUB_REF_NAME"' "$workflow" \
  || fail 'tag workflow does not publish both supported runtime architectures'

rpc_negotiate() { # <engine> <engine-version> <protocol-version> <compatible|incompatible> <label>
  local engine="$1" engine_version="$2" protocol="$3" expectation="$4" label="$5"
  local payload hex request response header size body
  payload="{\"version\":$protocol,\"type\":\"request\",\"id\":\"negotiate\",\"method\":\"rpc.negotiate\"}"
  hex="$(printf '%08x' "${#payload}")"
  request="$TMP/rpc-$label.request"; response="$TMP/rpc-$label.response"; body="$TMP/rpc-$label.json"
  {
    printf '%b' "\\x${hex:0:2}\\x${hex:2:2}\\x${hex:4:2}\\x${hex:6:2}"
    printf '%s' "$payload"
  } > "$request"
  SUBYARD_REPOSITORY_ROOT="$ROOT" SUBYARD_OPERATOR_HOME="$TMP/home" \
    SUBYARD_CONFIG_HOME="$TMP/config" SUBYARD_NO_AUDIT=1 \
    "$engine" rpc --stdio < "$request" > "$response"
  header="$(od -An -tx1 -N4 "$response" | tr -d ' \n')"
  [ "${#header}" -eq 8 ] || fail "$label returned no complete RPC frame header"
  case "$header" in *[!0-9a-f]*) fail "$label returned an invalid RPC frame header" ;; esac
  size=$((16#$header))
  [ "$(stat -c '%s' "$response")" -eq $((size + 4)) ] \
    || fail "$label returned a truncated or multi-frame negotiation response"
  dd if="$response" bs=1 skip=4 count="$size" status=none > "$body"
  case "$expectation" in
    compatible)
      jq -e '.version == 1 and .id == "negotiate" and .error == null and
        .result.version == 1 and .result.protocolMin == 1 and .result.protocolMax == 1 and
        .result.engineVersion == $engineVersion and
        (.result.capabilities | index("snapshot") != null)' \
        --arg engineVersion "$engine_version" "$body" >/dev/null \
        || fail "$label rejected the supported rolling RPC version" ;;
    incompatible)
      jq -e '.version == 1 and .id == "negotiate" and
        .error.code == "incompatible_version"' "$body" >/dev/null \
        || fail "$label did not reject an unsupported RPC version deterministically" ;;
    *) fail "invalid RPC expectation $expectation" ;;
  esac
}

staging_canary="$(mktemp --suffix=.env "$ROOT/config/staging/.package-canary.XXXXXX")"
qa_canary="$(mktemp "$ROOT/config/qa-pool/.package-canary.XXXXXX")"
untracked_canary="$(mktemp --suffix=.txt "$ROOT/config/staging/.package-untracked-canary.XXXXXX")"
printf 'ignored staging secret\n' > "$staging_canary"
printf 'ignored qa secret\n' > "$qa_canary"
printf 'untracked local input\n' > "$untracked_canary"
chmod 0600 "$staging_canary" "$qa_canary" "$untracked_canary"
trap 'rm -f -- "$staging_canary" "$qa_canary" "$untracked_canary"; rm -rf "$TMP"' EXIT
legacy_installer="$ROOT/tests/fixtures/migrations/v0.1.0-install-runtime-release.sh"
[ "$(sha256sum "$legacy_installer" | cut -d' ' -f1)" = \
  168dbaa00dfe3d86471358993e63d6b50c02d5af4bb136a6da3c4e39229780dd ] \
  || fail 'the pinned v0.1.0 runtime installer fixture changed'
legacy_031_installer="$ROOT/tests/fixtures/migrations/v0.3.1-install-runtime-release.sh"
[ "$(sha256sum "$legacy_031_installer" | cut -d' ' -f1)" = \
  04673421c42ac8a1bfed1e8fa547fd6aabd05011bc82bad951856df1e9c87193 ] \
  || fail 'the pinned v0.3.1 runtime installer fixture changed'
if grep -Eq '_migrate[[:space:]]+(apply|finalize|rollback|cleanup)' \
  "$ROOT/scripts/install-runtime-release.sh"; then
  fail 'current runtime installer still owns superseded migration choreography'
fi
artifact_one="$("$ROOT/dev/package-engine.sh" --output-dir "$release" --version 1.0.0-test \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-1.json")"
bundle_one="$release/subyard-1.0.0-test-linux-amd64.tar.gz"
[ -x "$release/subyard-install.sh" ] \
  && [ -x "$release/subyard-install-runtime-release.sh" ] \
  && [ -r "$release/subyard-install-runtime-release.sh.sha256" ] \
  || fail 'standalone first-install assets are missing'
grep -Fq 'YARD_RELEASE_REPOSITORY:-Subyard/Subyard' "$release/subyard-install.sh" \
  || fail 'standalone installer does not use the canonical release repository'
[ -x "$artifact_one" ] || { printf 'release artifact is not executable\n' >&2; exit 1; }
[ -r "$artifact_one.sha256" ] && [ -r "$artifact_one.manifest.json" ] && [ -r "$artifact_one.provenance.json" ] \
  || { printf 'release checksum, manifest or provenance missing\n' >&2; exit 1; }
[ -r "$bundle_one" ] && [ -r "$bundle_one.sha256" ] \
  && [ -r "$bundle_one.manifest.json" ] && [ -r "$bundle_one.provenance.json" ] \
  || fail 'self-contained runtime bundle contract is missing'
jq -e '.schemaVersion == 1 and .kind == "runtime" and .version == "1.0.0-test" and
  .rpc.min == 1 and .rpc.max == 1 and .migrationSchema == 1 and
  .minimumConfigLayout == 1 and .configLayout == 1' "$bundle_one.manifest.json" >/dev/null \
  || fail 'runtime bundle manifest is incompatible'
bundle_list="$TMP/runtime-bundle.list"
tar -tzf "$bundle_one" > "$bundle_list"
grep -Fxq './bin/yard' "$bundle_list" \
  && grep -Fxq './bin/yard-engine' "$bundle_list" \
  && grep -Fxq './scripts/install-runtime-release.sh' "$bundle_list" \
  && grep -Fxq './scripts/install-ssh-relay.sh' "$bundle_list" \
  && grep -Fxq './scripts/install-test-vms-host-sink.sh' "$bundle_list" \
  && grep -Fxq './config/systemd/subyard-test-vms-host-sink.service.in' "$bundle_list" \
  && grep -Fxq './config/systemd/subyard-test-vms-host-sink.timer.in' "$bundle_list" \
  && grep -Fxq './config/commands.registry' "$bundle_list" \
  && grep -Fxq './config/migrations.json' "$bundle_list" \
  && grep -Fxq './config/release-transition.json' "$bundle_list" \
  && grep -Fxq './config/agents/codex/provision.sh' "$bundle_list" \
  && grep -Fxq './config/profiles/hermes/resources/dashboard.res' "$bundle_list" \
  && grep -Fxq './config/profiles/hermes/resources/dashboard/handler.sh' "$bundle_list" \
  && grep -Fxq './config/profiles/hermes/yard.env' "$bundle_list" \
  || fail 'runtime bundle does not contain the complete launcher contract'
! grep -Fxq './config/profiles/hermes/hermes-release-resolve.py' "$bundle_list" \
  || fail 'runtime bundle contains the retired Hermes release resolver'
grep -Fxq './runtime-files.sha256' "$bundle_list" \
  || fail 'runtime bundle exact file manifest is missing'
! grep -Fq "$(basename "$staging_canary")" "$bundle_list" \
  && ! grep -Fq "$(basename "$qa_canary")" "$bundle_list" \
  && ! grep -Fq "$(basename "$untracked_canary")" "$bundle_list" \
  || fail 'runtime bundle contains an untracked host-local canary'
bundle_extract="$TMP/bundle-extract"
install -d "$bundle_extract"
tar -xzf "$bundle_one" -C "$bundle_extract"
(
  cd "$bundle_extract"
  sha256sum -c runtime-files.sha256 >/dev/null
  find . -type f ! -name runtime-files.sha256 -print | sort > "$TMP/bundle-actual.list"
  sed -E 's/^[0-9a-fA-F]{64}  //' runtime-files.sha256 | sort > "$TMP/bundle-declared.list"
)
cmp -s "$TMP/bundle-actual.list" "$TMP/bundle-declared.list" \
  || fail 'runtime bundle file manifest is not exact'
for excluded in update-engine.sh power-state.sh bootstrap-runtime.sh build-engine.sh package-engine.sh install-cli.sh; do
  ! grep -Fq "/$excluded" "$bundle_list" \
    || fail "runtime bundle contains non-runtime script $excluded"
done
jq -e '.schemaVersion == 1 and .version == "1.0.0-test" and .rpc.min == 1 and .rpc.max == 1 and
  .projectStateSchema == 1 and .credentialSchema == 1' "$artifact_one.manifest.json" >/dev/null
jq -e '.schemaVersion == 1 and .version == "1.0.0-test" and
  .sourceRepository == "github.com/Dmitry-Borodin/Subyard" and
  .canonicalRepository == "github.com/Subyard/Subyard" and (.sha256 | length == 64)' \
  "$artifact_one.provenance.json" >/dev/null
rpc_negotiate "$artifact_one" 1.0.0-test 1 compatible artifact-one-v1
rpc_negotiate "$artifact_one" 1.0.0-test 2 incompatible artifact-one-v2

standalone_home="$TMP/standalone-home"
standalone_bin="$TMP/standalone-bin"
standalone_no_go="$TMP/standalone-no-go"
install -d "$standalone_home" "$standalone_bin" "$standalone_no_go"
cat > "$standalone_no_go/go" <<EOF
#!/bin/sh
touch '$TMP/standalone-go-invoked'
exit 99
EOF
chmod 0700 "$standalone_no_go/go"
if HOME="$TMP/unconfirmed-home" YARD_RUNTIME_ROOT="$TMP/unconfirmed-runtime" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
  "$release/subyard-install.sh" </dev/null >/dev/null 2>&1; then
  fail 'standalone bootstrap changed the host without confirmation'
fi
[ ! -e "$TMP/unconfirmed-runtime" ] \
  || fail 'declined standalone bootstrap created a runtime'
HOME="$standalone_home" SUBYARD_HOME="$standalone_home/.subyard" \
  SUBYARD_CONFIG_HOME="$standalone_home/.config/subyard" YARD_BIN_DIR="$standalone_bin" \
  YARD_SHELL_RC="$standalone_home/.bashrc" YARD_LOGIN_RC="$standalone_home/.profile" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
  PATH="$standalone_no_go:$PATH" \
  "$release/subyard-install.sh" --yes >/dev/null
[ "$($standalone_bin/yard --version)" = 'yard 1.0.0-test' ] \
  && [ -L "$standalone_bin/sy" ] && [ ! -e "$TMP/standalone-go-invoked" ] \
  || fail 'standalone installer did not publish a usable runtime without a checkout'
grep -Fq 'Subyard CLI login PATH' "$standalone_home/.profile" \
  && grep -Fq 'Subyard CLI interactive PATH' "$standalone_home/.bashrc" \
  && grep -Fq 'Subyard CLI completion' "$standalone_home/.bashrc" \
  || fail 'standalone installer did not configure new login and interactive shells'
HOME="$standalone_home" PATH=/usr/bin:/bin SHELL=/bin/bash bash -lc \
  'command -v yard >/dev/null && yard --version >/dev/null' \
  || fail 'standalone installer is not available to a new login shell'
HOME="$standalone_home" PATH=/usr/bin:/bin SHELL=/bin/bash \
  bash --noprofile --rcfile "$standalone_home/.bashrc" -ic \
  'command -v yard >/dev/null && complete -p yard >/dev/null' >/dev/null 2>&1 \
  || fail 'standalone installer did not activate Bash completion'
zsh -f "$ROOT/tests/helpers/zsh-completion-buffers.zsh" \
  "$standalone_home/.subyard/runtime/current/completions/yard.zsh" \
  "$standalone_home/.subyard/runtime/current" \
  || fail 'standalone runtime Zsh completion corrupted the command buffer'
HOME="$standalone_home" SUBYARD_HOME="$standalone_home/.subyard" \
  SUBYARD_CONFIG_HOME="$standalone_home/.config/subyard" YARD_BIN_DIR="$standalone_bin" \
  YARD_SHELL_RC="$standalone_home/.bashrc" YARD_LOGIN_RC="$standalone_home/.profile" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
  PATH="$standalone_no_go:$PATH" \
  "$release/subyard-install.sh" --yes >/dev/null
[ "$(grep -cF 'Subyard CLI login PATH' "$standalone_home/.profile")" -eq 1 ] \
  && [ "$(grep -cF 'Subyard CLI interactive PATH' "$standalone_home/.bashrc")" -eq 1 ] \
  && [ "$(grep -cF 'Subyard CLI completion' "$standalone_home/.bashrc")" -eq 1 ] \
  || fail 'standalone shell integration is not idempotent'

bad_release="$TMP/bad-standalone-release"
cp -a "$release" "$bad_release"
printf 'corrupt\n' >> "$bad_release/subyard-install-runtime-release.sh"
if HOME="$TMP/bad-standalone-home" YARD_RUNTIME_ROOT="$TMP/bad-standalone-runtime" \
  YARD_RELEASE_BASE_URL="file://$bad_release" YARD_RELEASE_VERSION=1.0.0-test \
  "$bad_release/subyard-install.sh" --yes >/dev/null 2>&1; then
  fail 'standalone bootstrap accepted a corrupt installer'
fi
[ ! -e "$TMP/bad-standalone-runtime/current" ] \
  || fail 'corrupt standalone installer changed the current runtime'

artifact_arm="$("$ROOT/dev/package-engine.sh" --output-dir "$release" --version 1.0.0-test --arch arm64)"
jq -e '.os == "linux" and .arch == "arm64" and .version == "1.0.0-test"' \
  "$artifact_arm.manifest.json" >/dev/null \
  || fail 'arm64 release contract was not published'
for paseo_arch in amd64 arm64; do
  paseo_asset="$release/paseo-headless-0.2.1-linux-$paseo_arch.tar.gz"
  printf 'paseo test fixture for %s\n' "$paseo_arch" > "$paseo_asset"
  (cd "$release" && sha256sum "$(basename "$paseo_asset")" > "$(basename "$paseo_asset").sha256")
done
printf 'must not be published\n' > "$release/unexpected-build-note"
publish_list="$TMP/publish-assets.list"
"$ROOT/dev/release-assets.sh" --release-dir "$release" --version 1.0.0-test > "$publish_list"
[ "$(wc -l < "$publish_list")" -eq 23 ] \
  && ! grep -Fq '.build.lock' "$publish_list" \
  && ! grep -Fq 'unexpected-build-note' "$publish_list" \
  || fail 'release publishing does not use the exact 23-asset allowlist'
while IFS= read -r publish_asset; do
  [ -f "$publish_asset" ] && [ ! -L "$publish_asset" ] \
    || fail "release allowlist contains an invalid asset: $publish_asset"
done < "$publish_list"

artifact_two="$("$ROOT/dev/package-engine.sh" --output-dir "$release" --version 1.1.0-test \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-1.json")"
bundle_two="$release/subyard-1.1.0-test-linux-amd64.tar.gz"
release_engine="$artifact_two"
jq -e '.version == "1.1.0-test" and .rpc.min == 1 and .rpc.max == 1 and
  .migrationSchema == 1 and .minimumConfigLayout == 1 and .configLayout == 1' \
  "$artifact_two.manifest.json" >/dev/null
rpc_negotiate "$artifact_two" 1.1.0-test 1 compatible artifact-two-v1

# A verified standalone bootstrap is the one-time bridge for an active runtime
# whose installed release installer predates immutable publish-only support.
# The candidate must own the transition; the legacy installer is never asked
# to run the superseded mutating _migrate protocol.
legacy_bridge_release="$TMP/legacy-bridge-release"
"$ROOT/dev/package-engine.sh" \
  --output-dir "$legacy_bridge_release" --version 0.3.1-test \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-1.json" \
  --runtime-installer "$legacy_031_installer" >/dev/null
legacy_bridge_bundle="$legacy_bridge_release/subyard-0.3.1-test-linux-amd64.tar.gz"
legacy_bridge_home="$TMP/legacy-bridge-home"
legacy_bridge_runtime="$legacy_bridge_home/.subyard/runtime"
legacy_bridge_bin="$legacy_bridge_home/bin"
install -d "$legacy_bridge_home" "$legacy_bridge_bin" \
  "$legacy_bridge_home/.config/subyard"
"$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$legacy_bridge_runtime" \
  --bundle "$legacy_bridge_bundle" --checksum "$legacy_bridge_bundle.sha256" \
  --manifest "$legacy_bridge_bundle.manifest.json" \
  --provenance "$legacy_bridge_bundle.provenance.json" >/dev/null
legacy_bridge_initial="$(readlink "$legacy_bridge_runtime/current")"
grep -Fq -- '--publish-only' \
  "$legacy_bridge_runtime/current/scripts/install-runtime-release.sh" \
  && fail 'legacy bridge fixture unexpectedly supports immutable publication'
HOME="$legacy_bridge_home" SUBYARD_HOME="$legacy_bridge_home/.subyard" \
  SUBYARD_CONFIG_HOME="$legacy_bridge_home/.config/subyard" \
  YARD_RUNTIME_ROOT="$legacy_bridge_runtime" YARD_BIN_DIR="$legacy_bridge_bin" \
  YARD_SHELL_RC="$legacy_bridge_home/.bashrc" YARD_LOGIN_RC="$legacy_bridge_home/.profile" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.1.0-test \
  "$release/subyard-install.sh" --yes >/dev/null
[ "$("$legacy_bridge_runtime/current/bin/yard" --version | awk '{print $2}')" = 1.1.0-test ] \
  && [ "$(readlink "$legacy_bridge_runtime/previous")" = "$legacy_bridge_initial" ] \
  || fail 'standalone bootstrap did not bridge a pre-transition runtime through ReleaseTransition'

# Publication is intentionally separate from activation. The transition module
# must be able to inspect an immutable verified candidate while the old runtime
# remains the exact active release.
publish_only_root="$TMP/publish-only-runtime"
published_release="$("$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$publish_only_root" --publish-only \
  --bundle "$bundle_two" --checksum "$bundle_two.sha256" \
  --manifest "$bundle_two.manifest.json" --provenance "$bundle_two.provenance.json")"
case "$published_release" in
  releases/1.1.0-test-*) ;;
  *) fail "publish-only returned an invalid release identity: $published_release" ;;
esac
[ -d "$publish_only_root/$published_release" ] \
  && [ ! -e "$publish_only_root/current" ] && [ ! -L "$publish_only_root/current" ] \
  && [ ! -e "$publish_only_root/previous" ] && [ ! -L "$publish_only_root/previous" ] \
  || fail 'publish-only changed stable runtime links or omitted the immutable release'

# Installed release ingress: bootstrap creates only the first link; every
# subsequent activation, same-version retry, and explicit rollback is owned by
# the candidate ReleaseTransition module.
runtime_root="$TMP/update-runtime"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/hermes"
printf '%s\n' '# retained' 'YARD_TEMPLATE=e2e-vms' 'NESTED_E2E_VMS=0' 'SSH_PORT=2224' \
  > "$SUBYARD_CONFIG_HOME/yards/hermes/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/hermes/config.env"
"$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$runtime_root" \
  --bundle "$bundle_one" --checksum "$bundle_one.sha256" \
  --manifest "$bundle_one.manifest.json" --provenance "$bundle_one.provenance.json" >/dev/null
initial_target="$(readlink "$runtime_root/current")"
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.0-test ] \
  && [ ! -e "$runtime_root/previous" ] && [ ! -L "$runtime_root/previous" ] \
  || fail 'verified bootstrap did not create the initial runtime boundary'

YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/cache" \
  installed_update --runtime-root "$runtime_root" --version 1.1.0-test >/dev/null
candidate_target="$(readlink "$runtime_root/current")"
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.1.0-test ] \
  && [ "$(readlink "$runtime_root/previous")" = "$initial_target" ] \
  && [ "$candidate_target" != "$initial_target" ] \
  || fail 'release transition did not activate and retain the exact release pair'
grep -Fxq '# retained' "$SUBYARD_CONFIG_HOME/yards/hermes/config.env" \
  && grep -Fxq "YARD_TEMPLATE='test-vms'" "$SUBYARD_CONFIG_HOME/yards/hermes/config.env" \
  && grep -Fxq 'SSH_PORT=2224' "$SUBYARD_CONFIG_HOME/yards/hermes/config.env" \
  && ! grep -Fq 'NESTED_E2E_VMS' "$SUBYARD_CONFIG_HOME/yards/hermes/config.env" \
  || fail 'planned v2 canonicalize/reset did not reach its declared result'
ledger="$SUBYARD_CONFIG_HOME/release-transition/v2/ledger.json"
journal="$SUBYARD_CONFIG_HOME/release-transition/v2/journal.json"
jq -e '.schemaVersion == 2 and .domains.settings.epoch == 2 and
  .domains.settings.applied == ["canonicalize-test-vms-settings-v2"]' "$ledger" >/dev/null \
  && jq -e '.schemaVersion == 2 and .checkpoint == "complete"' "$journal" >/dev/null \
  || fail 'v2 transition did not record a completed per-domain fixed point'
[ ! -e "$SUBYARD_CONFIG_HOME/migrations/state.json" ] \
  || fail 'production release transition wrote the superseded global layout'

same_output="$(YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/cache" \
  installed_update --runtime-root "$runtime_root" --version 1.1.0-test)"
[ "$(readlink "$runtime_root/current")" = "$candidate_target" ] \
  && [ "$(readlink "$runtime_root/previous")" = "$initial_target" ] \
  || fail 'same-version transition changed the retained release pair'
! grep -Fq '{"schemaVersion"' <<<"$same_output" \
  || fail 'candidate process protocol leaked into public update output'

installed_update --runtime-root "$runtime_root" --rollback >/dev/null
[ "$(readlink "$runtime_root/current")" = "$initial_target" ] \
  && [ "$(readlink "$runtime_root/previous")" = "$candidate_target" ] \
  && [ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.0-test ] \
  || fail 'explicit rollback did not assess and activate the retained runtime'

YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/cache" \
  installed_update --runtime-root "$runtime_root" --version 1.1.0-test >/dev/null
[ "$(readlink "$runtime_root/current")" = "$candidate_target" ] \
  && [ "$(readlink "$runtime_root/previous")" = "$initial_target" ] \
  || fail 'forward retry after rollback did not converge to the exact pair'

YARD_RELEASE_CACHE="$TMP/cache" installed_update \
  --runtime-root "$runtime_root" --version 1.1.0-test --offline --check >/dev/null \
  || fail 'offline transition inspection did not use the verified cache'
cached_bundle="$TMP/cache/1.1.0-test/$(basename "$bundle_two")"
printf 'truncated\n' >> "$cached_bundle"
if YARD_RELEASE_CACHE="$TMP/cache" installed_update \
  --runtime-root "$runtime_root" --version 1.1.0-test --offline --check >/dev/null 2>&1; then
  fail 'offline transition inspection accepted a corrupt cached bundle'
fi
[ "$(readlink "$runtime_root/current")" = "$candidate_target" ] \
  && [ "$(readlink "$runtime_root/previous")" = "$initial_target" ] \
  || fail 'failed inspection changed stable runtime links'

printf 'ok: release publication and resumable v2 activation ingress are verified\n'

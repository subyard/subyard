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

# v0.11.1 could durably activate and then strand an exact transition at
# reconciliation when the next process observed a different activation scope.
# Seed that state through the public transition API, then exercise the same
# standalone bootstrap operators use: immutable publication followed by the
# exact packaged candidate runtime.
recovery_release="$TMP/v0111-recovery-release"
recovery_previous_bundle="$recovery_release/subyard-0.9.1-linux-amd64.tar.gz"
recovery_older_source_bundle="$recovery_release/subyard-0.11.0-linux-amd64.tar.gz"
recovery_source_bundle="$recovery_release/subyard-0.11.1-linux-amd64.tar.gz"
recovery_candidate_bundle="$recovery_release/subyard-0.11.2-linux-amd64.tar.gz"
"$ROOT/dev/package-engine.sh" --output-dir "$recovery_release" --version 0.9.1 >/dev/null
"$ROOT/dev/package-engine.sh" --output-dir "$recovery_release" --version 0.11.0 >/dev/null
"$ROOT/dev/package-engine.sh" --output-dir "$recovery_release" --version 0.11.1 >/dev/null
"$ROOT/dev/package-engine.sh" --output-dir "$recovery_release" --version 0.11.2 >/dev/null
recovery_fixture="$TMP/release-transition-fixture"
go build -o "$recovery_fixture" "$ROOT/tests/helpers/release-transition-fixture"
recovery_home="$TMP/v0111-recovery-home"
recovery_runtime="$recovery_home/runtime"
recovery_config="$recovery_home/config"
recovery_cache="$recovery_home/cache"
install -d -m 0700 "$recovery_home" "$recovery_config/yards/recovery-yard"
printf '%s\n' 'YARD_TEMPLATE=test-vms' 'SSH_PORT=2293' 'AGENTS=' \
  > "$recovery_config/yards/recovery-yard/config.env"
chmod 0600 "$recovery_config/yards/recovery-yard/config.env"
"$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$recovery_runtime" \
  --bundle "$recovery_previous_bundle" --checksum "$recovery_previous_bundle.sha256" \
  --manifest "$recovery_previous_bundle.manifest.json" \
  --provenance "$recovery_previous_bundle.provenance.json" >/dev/null
recovery_previous_path="$(readlink "$recovery_runtime/current")"
recovery_previous="${recovery_previous_path#releases/}"
recovery_source_path="$("$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$recovery_runtime" --publish-only \
  --bundle "$recovery_source_bundle" --checksum "$recovery_source_bundle.sha256" \
  --manifest "$recovery_source_bundle.manifest.json" \
  --provenance "$recovery_source_bundle.provenance.json")"
recovery_source="${recovery_source_path#releases/}"
"$recovery_fixture" seed \
  "$recovery_runtime" "$recovery_config" "$recovery_previous" "$recovery_source" >/dev/null
recovery_source_journal="$recovery_config/release-transition/v2/journal.json"
recovery_ledger="$recovery_config/release-transition/v2/ledger.json"
jq -e '.checkpoint == "reconciling" and .transaction == "tx-source-v0111" and
  (.steps | length) == 2 and all(.steps[]; .checkpoint == "verified")' \
  "$recovery_source_journal" >/dev/null \
  || fail 'public fixture did not produce the exact v0.11.1 post-activation journal'
source_blocker="$TMP/v0111-source-blocker.json"
HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" SUBYARD_YARD=recovery-yard \
  YARD_RELEASE_CACHE="$recovery_cache" \
  SUBYARD_POWER_RECONCILER_PATH="$TMP/missing-power-reconciler" \
  SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit" \
  "$recovery_runtime/current/bin/yard" update --check --offline --version 0.11.1 \
    --runtime-root "$recovery_runtime" > "$source_blocker"
jq -s -e 'map(select(type == "object" and has("blockers"))) as $inspections |
  ($inspections | length) == 1 and ($inspections[0].blockers | length) == 1 and
  $inspections[0].blockers[0].resource == "transition.observation-scope"' \
  "$source_blocker" >/dev/null \
  || fail 'v0.11.1 process did not reproduce the post-activation scope blocker'
recovery_baseline="$TMP/v0111-recovery-baseline"
install -d "$recovery_baseline"
cp -a "$recovery_runtime" "$recovery_baseline/runtime"
cp -a "$recovery_config" "$recovery_baseline/config"
cp "$recovery_ledger" "$recovery_baseline/ledger.json"

# Source-version admission is a runtime fact, not a journal field. Build a
# second otherwise-equivalent protected fixture whose verified source process
# reports v0.11.0, so the standalone bridge must reject it independently of
# the newer candidate version.
source_version_home="$TMP/v0110-source-home"
source_version_runtime="$source_version_home/runtime"
source_version_config="$source_version_home/config"
install -d -m 0700 "$source_version_home" "$source_version_config/yards/recovery-yard"
cp "$recovery_config/yards/recovery-yard/config.env" \
  "$source_version_config/yards/recovery-yard/config.env"
"$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$source_version_runtime" \
  --bundle "$recovery_previous_bundle" --checksum "$recovery_previous_bundle.sha256" \
  --manifest "$recovery_previous_bundle.manifest.json" \
  --provenance "$recovery_previous_bundle.provenance.json" >/dev/null
source_version_previous_path="$(readlink "$source_version_runtime/current")"
source_version_previous="${source_version_previous_path#releases/}"
source_version_source_path="$("$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$source_version_runtime" --publish-only \
  --bundle "$recovery_older_source_bundle" --checksum "$recovery_older_source_bundle.sha256" \
  --manifest "$recovery_older_source_bundle.manifest.json" \
  --provenance "$recovery_older_source_bundle.provenance.json")"
source_version_source="${source_version_source_path#releases/}"
"$recovery_fixture" seed \
  "$source_version_runtime" "$source_version_config" \
  "$source_version_previous" "$source_version_source" >/dev/null
source_version_baseline="$TMP/v0110-source-baseline"
install -d "$source_version_baseline"
cp -a "$source_version_runtime" "$source_version_baseline/runtime"
cp -a "$source_version_config" "$source_version_baseline/config"

# Inspect the already-published candidate directly before the standalone
# bootstrap mutates links or transition state. This is the exact Plan the
# terminal replacement journal and immutable archive must retain.
recovery_candidate_path="$("$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$recovery_runtime" --publish-only \
  --bundle "$recovery_candidate_bundle" --checksum "$recovery_candidate_bundle.sha256" \
  --manifest "$recovery_candidate_bundle.manifest.json" \
  --provenance "$recovery_candidate_bundle.provenance.json")"
case "$recovery_candidate_path" in
  releases/0.11.2-*) ;;
  *) fail "publish-only returned an invalid recovery candidate: $recovery_candidate_path" ;;
esac
recovery_source_snapshot="$TMP/v0111-source-journal.json"
cp "$recovery_source_journal" "$recovery_source_snapshot"
recovery_plan_output="$TMP/v0111-recovery-plan.json"
HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" SUBYARD_YARD=recovery-yard \
  YARD_RELEASE_CACHE="$recovery_cache" \
  SUBYARD_POWER_RECONCILER_PATH="$TMP/missing-power-reconciler" \
  SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit" \
  "$recovery_runtime/$recovery_candidate_path/bin/yard" update --check --offline \
    --version 0.11.2 --runtime-root "$recovery_runtime" > "$recovery_plan_output"
cmp -s "$recovery_source_journal" "$recovery_source_snapshot" \
  || fail 'candidate recovery inspection changed the protected source journal'
recovery_plan="$(jq -sr '
  map(select(type == "object") | (.inspection // .)) |
  map(select(has("plan") and .assessment.changed == true and
    ((.blockers // []) | length) == 0)) |
  select(length == 1) | .[0].plan' "$recovery_plan_output")"
case "$recovery_plan" in
  plan-v1-*) ;;
  *) fail 'candidate recovery inspection did not return one unblocked exact Plan' ;;
esac

trace_environment="$TMP/v0111-xtrace-env"
trace_output="$TMP/v0111-bootstrap.trace"
printf '%s\n' "PS4='TRACE '" 'set -x' > "$trace_environment"
HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" SUBYARD_YARD=recovery-yard \
  YARD_RUNTIME_ROOT="$recovery_runtime" \
  YARD_RELEASE_CACHE="$recovery_cache" YARD_BIN_DIR="$recovery_home/bin" \
  YARD_SHELL_RC="$recovery_home/.bashrc" YARD_LOGIN_RC="$recovery_home/.profile" \
  YARD_RELEASE_BASE_URL="file://$recovery_release" YARD_RELEASE_VERSION=0.11.2 \
  SUBYARD_POWER_RECONCILER_PATH="$TMP/missing-power-reconciler" \
  SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit" \
  BASH_ENV="$trace_environment" BASH_XTRACEFD=9 \
  "$recovery_release/subyard-install.sh" --yes >/dev/null 9>"$trace_output"
recovery_candidate_target="$(readlink "$recovery_runtime/current")"
recovery_candidate="${recovery_candidate_target#releases/}"
grep -F -- "--runtime-root $recovery_runtime --publish-only" "$trace_output" >/dev/null \
  || fail 'standalone bootstrap did not use immutable publish-only installation'
grep -F -- "$recovery_runtime/$recovery_candidate_target/bin/yard update --runtime-root $recovery_runtime --version 0.11.2 --offline --yes" \
  "$trace_output" >/dev/null \
  || fail 'standalone bootstrap did not invoke the exact candidate runtime and arguments'
! grep -Fq "$recovery_config/release-transition/v2" "$trace_output" \
  && ! grep -Eq '_migrate[[:space:]]+(apply|finalize|rollback|cleanup)' "$trace_output" \
  || fail 'standalone bootstrap touched protected transition state or a mutating legacy endpoint'
recovery_terminal_journal="$recovery_config/release-transition/v2/journal.json"
recovery_transaction="$(jq -er '.transaction' "$recovery_terminal_journal")"
recovery_archive="$recovery_config/release-transition/v2/transactions/$recovery_transaction/superseded-journal.json"
[ "$(readlink "$recovery_runtime/previous")" = "releases/$recovery_source" ] \
  && [ "$recovery_candidate" != "$recovery_source" ] \
  && jq -e --arg plan "$recovery_plan" \
    '.checkpoint == "complete" and (.steps | length) == 0 and
    .authorizationPlan == $plan' \
    "$recovery_terminal_journal" >/dev/null \
  && jq -e --arg plan "$recovery_plan" \
    '.journal.transaction == "tx-source-v0111" and
    .replacement.reason == "post-activation-scope-v0.11.1" and
    .authorizationPlan == $plan' "$recovery_archive" >/dev/null \
  && [ -d "$recovery_config/release-transition/v2/transactions/tx-source-v0111/evidence" ] \
  && [ "$(find "$recovery_config/release-transition/v2/transactions" \
    -type f -name superseded-journal.json | wc -l)" -eq 1 ] \
  && cmp -s "$recovery_ledger" "$recovery_baseline/ledger.json" \
  || fail 'standalone v0.11.1 recovery did not preserve the terminal journal, archive, CAS, and links'
recovery_repeat_output="$TMP/v0111-repeat-check.json"
HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" SUBYARD_YARD=recovery-yard \
  YARD_RELEASE_CACHE="$recovery_cache" \
  SUBYARD_POWER_RECONCILER_PATH="$TMP/missing-power-reconciler" \
  SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit" \
  "$recovery_runtime/current/bin/yard" update --check --offline --version 0.11.2 \
    --runtime-root "$recovery_runtime" > "$recovery_repeat_output" \
  || fail 'same-version recovery check did not remain readable and terminal'
jq -s -e --arg active "$recovery_candidate" '
  map(select(type == "object") | (.inspection // .)) |
  map(select(has("outcome") and has("assessment"))) as $reports |
  ($reports | length) == 1 and
  $reports[0].outcome.status == "ready" and
  $reports[0].outcome.reachedGoal == true and
  $reports[0].outcome.active == $active and
  $reports[0].outcome.target == $active and
  $reports[0].assessment.changed == false and
  (($reports[0].blockers // []) | length) == 0' "$recovery_repeat_output" >/dev/null \
  || fail 'same-version recovery check did not report the structured ready fixed point'
[ "$("$recovery_runtime/current/bin/yard" --version)" = 'yard 0.11.2' ] \
  || fail 'terminal recovery runtime is not readable'

snapshot_recovery_state() { # <runtime-root> <config-home>
  local snapshot_runtime="$1" snapshot_config="$2"

  printf 'current\t%s\nprevious\t%s\n' \
    "$(snapshot_runtime_link "$snapshot_runtime/current")" \
    "$(snapshot_runtime_link "$snapshot_runtime/previous")"
  find "$snapshot_config/release-transition/v2" -type f -print0 \
    | sort -z | xargs -0 sha256sum
}

snapshot_runtime_link() { # <link>
  if [ -L "$1" ]; then
    readlink "$1"
  else
    printf '<absent>\n'
  fi
}

recovery_negative_mutations='source-version candidate-version checkpoint topology-current
topology-previous direction source-ingress step-checkpoint step-resource embedded-evidence
evidence-captured evidence-applied evidence-verified evidence-extra ledger recovery
recovery-extra registry catalog replacement-transaction replacement-fingerprint
transaction-artifact blocker-count blocker-code'
recovery_baseline_before="$TMP/v0111-recovery-baseline.sha256"
source_version_baseline_before="$TMP/v0111-source-version-baseline.sha256"
snapshot_recovery_state "$recovery_baseline/runtime" "$recovery_baseline/config" \
  > "$recovery_baseline_before"
snapshot_recovery_state "$source_version_baseline/runtime" "$source_version_baseline/config" \
  > "$source_version_baseline_before"
for recovery_mutation in $recovery_negative_mutations; do
  negative_root="$TMP/v0111-negative-$recovery_mutation"
  negative_baseline="$recovery_baseline"
  negative_release_version=0.11.2
  if [ "$recovery_mutation" = source-version ]; then
    negative_baseline="$source_version_baseline"
  fi
  # Runtime verification requires every protected release file to have one
  # link. Keep each case a real copy, then discard it before the next case so
  # the matrix has bounded disk use on constrained E2E /tmp filesystems.
  cp -a "$negative_baseline" "$negative_root"
  negative_runtime="$negative_root/runtime"
  negative_config="$negative_root/config"
  negative_power_reconciler="$TMP/missing-power-reconciler"
  process_guard_only=0
  process_guard_candidate=
  case "$recovery_mutation" in
    source-version) ;;
    candidate-version)
      negative_release_version=0.11.0
      ;;
    blocker-count)
      negative_power_reconciler="$negative_root/power-reconciler"
      ln -s "$negative_root/missing-power-reconciler-target" "$negative_power_reconciler"
      blocker_count_output="$negative_root/blocker-count.json"
      HOME="$negative_root/home" SUBYARD_HOME="$negative_root/data" \
        SUBYARD_CONFIG_HOME="$negative_config" SUBYARD_YARD=recovery-yard \
        YARD_RELEASE_CACHE="$negative_root/cache" \
        SUBYARD_POWER_RECONCILER_PATH="$negative_power_reconciler" \
        SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit" \
        "$negative_runtime/current/bin/yard" update --check --offline --version 0.11.1 \
          --runtime-root "$negative_runtime" > "$blocker_count_output" \
        || fail 'blocker-count fixture could not be inspected by the verified source'
      jq -s -e 'map(select(type == "object" and has("blockers"))) as $inspections |
        ($inspections | length) == 1 and ($inspections[0].blockers | length) > 1' \
        "$blocker_count_output" >/dev/null \
        || fail 'blocker-count fixture did not independently add a second blocker'
      ;;
    replacement-transaction|replacement-fingerprint|blocker-code)
      if [ "$recovery_mutation" = blocker-code ]; then
        "$recovery_fixture" mutate \
          "$negative_runtime" "$negative_config" "$recovery_mutation"
      fi
      process_guard_candidate="$("$ROOT/scripts/install-runtime-release.sh" \
        --runtime-root "$negative_runtime" --publish-only \
        --bundle "$recovery_candidate_bundle" \
        --checksum "$recovery_candidate_bundle.sha256" \
        --manifest "$recovery_candidate_bundle.manifest.json" \
        --provenance "$recovery_candidate_bundle.provenance.json")"
      process_guard_candidate="${process_guard_candidate#releases/}"
      process_guard_only=1
      ;;
    *)
      "$recovery_fixture" mutate \
        "$negative_runtime" "$negative_config" "$recovery_mutation"
      ;;
  esac
  negative_before="$negative_root/before.sha256"
  snapshot_recovery_state "$negative_runtime" "$negative_config" > "$negative_before"
  if [ "$process_guard_only" = 1 ]; then
    # The production runtime constructs replacement identity itself, so its
    # safe bootstrap cannot emit a corrupt request. Exercise those typed
    # request guards, and an alternate sole blocker code, in a separate
    # candidate transition process after the exact immutable publication.
    "$recovery_fixture" guard \
      "$negative_runtime" "$negative_config" \
      "$process_guard_candidate" "$recovery_mutation"
    find "$negative_runtime/releases" -mindepth 1 -maxdepth 1 -type d \
      -name '0.11.2-*' -print -quit | grep -q . \
      || fail "ineligible v0.11.1 $recovery_mutation fixture was not publication-only"
    snapshot_recovery_state "$negative_runtime" "$negative_config" \
      | cmp -s "$negative_before" - \
      || fail "ineligible v0.11.1 $recovery_mutation fixture changed protected state"
    rm -rf -- "$negative_root"
    continue
  fi
  negative_run_output="$negative_root/standalone.out"
  if HOME="$negative_root/home" SUBYARD_HOME="$negative_root/data" \
    SUBYARD_CONFIG_HOME="$negative_config" SUBYARD_YARD=recovery-yard \
    YARD_RUNTIME_ROOT="$negative_runtime" \
    YARD_RELEASE_CACHE="$negative_root/cache" YARD_BIN_DIR="$negative_root/bin" \
    YARD_SHELL_RC="$negative_root/.bashrc" YARD_LOGIN_RC="$negative_root/.profile" \
    YARD_RELEASE_BASE_URL="file://$recovery_release" \
    YARD_RELEASE_VERSION="$negative_release_version" \
    SUBYARD_POWER_RECONCILER_PATH="$negative_power_reconciler" \
    SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit" \
    "$recovery_release/subyard-install.sh" --yes > "$negative_run_output" 2>&1; then
    fail "ineligible v0.11.1 $recovery_mutation fixture was recovered"
  fi
  find "$negative_runtime/releases" -mindepth 1 -maxdepth 1 -type d \
    -name "$negative_release_version-*" -print -quit | grep -q . \
    || fail "ineligible v0.11.1 $recovery_mutation fixture was not publication-only"
  snapshot_recovery_state "$negative_runtime" "$negative_config" \
    | cmp -s "$negative_before" - \
    || fail "ineligible v0.11.1 $recovery_mutation fixture changed protected state"
  rm -rf -- "$negative_root"
done
snapshot_recovery_state "$recovery_baseline/runtime" "$recovery_baseline/config" \
  | cmp -s "$recovery_baseline_before" - \
  || fail 'v0.11.1 recovery negative matrix changed its shared baseline'
snapshot_recovery_state "$source_version_baseline/runtime" "$source_version_baseline/config" \
  | cmp -s "$source_version_baseline_before" - \
  || fail 'v0.11.1 source-version negative case changed its shared baseline'

printf 'ok: release publication and resumable v2 activation ingress are verified\n'

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
artifact_one="$("$ROOT/dev/package-engine.sh" --output-dir "$release" --version 1.0.0-test \
  --runtime-installer "$legacy_installer" \
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

legacy_state="$SUBYARD_CONFIG_HOME/projects/legacy-12345678.json"
install -d -m 0700 "$(dirname "$legacy_state")"
printf '%s\n' '{"schema":1,"projectId":"legacy-12345678","name":"Legacy","hostPath":"/host/Legacy","yardPath":"/srv/workspaces/legacy-12345678/src","mode":"sync","sshHost":"yard"}' > "$legacy_state"
chmod 0664 "$legacy_state"

artifact_two="$("$ROOT/dev/package-engine.sh" --output-dir "$release" --version 1.1.0-test \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-2.json")"
bundle_two="$release/subyard-1.1.0-test-linux-amd64.tar.gz"
release_engine="$artifact_two"
jq -e '.version == "1.1.0-test" and .rpc.min == 1 and .rpc.max == 1 and
  .migrationSchema == 1 and .minimumConfigLayout == 1 and .configLayout == 2' \
  "$artifact_two.manifest.json" >/dev/null
rpc_negotiate "$artifact_two" 1.1.0-test 1 compatible artifact-two-v1

# The public updater publishes a complete runtime, atomically switches stable links and can reuse
# its exact cache offline without touching a working current release.
runtime_root="$TMP/update-runtime"
YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/cache" \
  yard_update --runtime-root "$runtime_root" --version 1.0.0-test >/dev/null
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.0-test ] \
  || fail 'yard update did not install the selected release'
[ "$(stat -c '%a' "$legacy_state")" = 600 ] \
  || fail 'runtime install did not migrate legacy project permissions'
[ -x "$runtime_root/current/scripts/install-runtime-release.sh" ] \
  && [ -r "$runtime_root/current/config/commands.registry" ] \
  && [ -r "$runtime_root/current/completions/yard.bash" ] \
  || fail 'yard update installed an incomplete runtime'
YARD_RELEASE_CACHE="$TMP/cache" yard_update \
  --runtime-root "$runtime_root" --version 1.0.0-test --offline --check >/dev/null \
  || fail 'offline update check did not use the verified cache'
cached_bundle="$TMP/cache/1.0.0-test/$(basename "$bundle_one")"
printf 'truncated\n' >> "$cached_bundle"
if YARD_RELEASE_CACHE="$TMP/cache" yard_update \
  --runtime-root "$runtime_root" --version 1.0.0-test --offline --check >/dev/null 2>&1; then
  fail 'offline update check accepted a corrupt cached bundle'
fi
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.0-test ] \
  || fail 'failed offline check changed the current runtime'

# A historical updater knows only the generic candidate-owned _migrate
# contract. It must still apply the production typed transition without any
# migration-specific installer hook.
typed_base_artifact="$("$ROOT/dev/package-engine.sh" --output-dir "$release" \
  --version 1.0.0-typed-base --runtime-installer "$legacy_031_installer" \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-1.json")"
typed_artifact="$("$ROOT/dev/package-engine.sh" --output-dir "$release" \
  --version 1.0.1-test \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-2-production.json")"
jq -e '.version == "1.0.1-test" and .migrationSchema == 1 and
  .minimumConfigLayout == 1 and .configLayout == 2' \
  "$typed_artifact.manifest.json" >/dev/null \
  || fail 'typed migration candidate changed the v0.3.1-compatible manifest contract'
typed_runtime_root="$TMP/typed-update-runtime"
typed_config_home="$TMP/typed-config"
typed_data_home="$TMP/typed-data"
SUBYARD_CONFIG_HOME="$typed_config_home" SUBYARD_HOME="$typed_data_home" \
  YARD_ENGINE_PATH="$typed_base_artifact" YARD_RELEASE_BASE_URL="file://$release" \
  YARD_RELEASE_CACHE="$TMP/typed-cache" \
  "$ROOT/bin/yard" update --yes --runtime-root "$typed_runtime_root" \
  --version 1.0.0-typed-base >/dev/null
SUBYARD_CONFIG_HOME="$typed_config_home" SUBYARD_HOME="$typed_data_home" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/typed-cache" \
  "$typed_runtime_root/current/bin/yard" update --yes \
  --runtime-root "$typed_runtime_root" --version 1.0.1-test >/dev/null
[ "$("$typed_runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.1-test ] \
  && jq -e '.layout == 2 and .applied == ["migrate-test-yard-owner"]' \
    "$typed_config_home/migrations/state.json" >/dev/null \
  || fail 'historical updater did not commit the candidate-owned typed migration'
SUBYARD_CONFIG_HOME="$typed_config_home" SUBYARD_HOME="$typed_data_home" \
  "$typed_runtime_root/current/bin/yard" update --yes \
  --runtime-root "$typed_runtime_root" --rollback >/dev/null
[ "$("$typed_runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.0-typed-base ] \
  && jq -e '.layout == 1 and (.applied // []) == []' \
    "$typed_config_home/migrations/state.json" >/dev/null \
  || fail 'typed migration rollback did not restore the historical layout'
SUBYARD_CONFIG_HOME="$typed_config_home" SUBYARD_HOME="$typed_data_home" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/typed-cache" \
  "$typed_runtime_root/current/bin/yard" update --yes \
  --runtime-root "$typed_runtime_root" --version 1.0.1-test >/dev/null
[ "$("$typed_runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.1-test ] \
  && jq -e '.layout == 2 and .applied == ["migrate-test-yard-owner"]' \
    "$typed_config_home/migrations/state.json" >/dev/null \
  || fail 'historical updater did not roll the typed migration forward again'
SUBYARD_CONFIG_HOME="$typed_config_home" SUBYARD_HOME="$typed_data_home" \
  "$typed_runtime_root/current/bin/yard-engine" _migrate-test-yard >/dev/null \
  || fail 'typed migration candidate dropped the v0.4.0 installer compatibility shim'

# A later candidate must preserve every already-applied registry definition as
# an exact prefix before appending a new transition.
typed_history_artifact="$("$ROOT/dev/package-engine.sh" --output-dir "$release" \
  --version 1.0.2-typed-history \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-3-production-prefix.json")"
jq -e '.version == "1.0.2-typed-history" and .configLayout == 3' \
  "$typed_history_artifact.manifest.json" >/dev/null \
  || fail 'production-prefix fixture did not publish layout 3'
typed_history_source="$typed_config_home/migration-fixture/legacy/config.env"
typed_history_destination="$typed_config_home/migration-fixture/current/config.env"
install -d -m 0700 "$(dirname "$typed_history_source")"
printf 'TOKEN=production-prefix\n' > "$typed_history_source"
chmod 0600 "$typed_history_source"
typed_state_before="$(sha256sum "$typed_config_home/migrations/state.json")"
if SUBYARD_CONFIG_HOME="$typed_config_home" SUBYARD_HOME="$typed_data_home" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/typed-cache" \
  "$typed_runtime_root/current/bin/yard" update --yes --check \
  --runtime-root "$typed_runtime_root" --version 1.1.0-test >/dev/null 2>&1; then
  fail 'candidate accepted rewritten applied migration history'
fi
[ "$typed_state_before" = "$(sha256sum "$typed_config_home/migrations/state.json")" ] \
  && [ -f "$typed_history_source" ] && [ ! -e "$typed_history_destination" ] \
  || fail 'rejected registry history changed typed migration state'
SUBYARD_CONFIG_HOME="$typed_config_home" SUBYARD_HOME="$typed_data_home" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/typed-cache" \
  "$typed_runtime_root/current/bin/yard" update --yes \
  --runtime-root "$typed_runtime_root" --version 1.0.2-typed-history >/dev/null
[ "$("$typed_runtime_root/current/bin/yard" --version | awk '{print $2}')" = \
    1.0.2-typed-history ] \
  && [ ! -e "$typed_history_source" ] \
  && grep -Fxq 'TOKEN=production-prefix' "$typed_history_destination" \
  && jq -e '
    .layout == 3 and
    .applied == ["migrate-test-yard-owner", "move-legacy-assignments"]
  ' "$typed_config_home/migrations/state.json" >/dev/null \
  || fail 'extended registry did not preserve and append migration history'
SUBYARD_CONFIG_HOME="$typed_config_home" SUBYARD_HOME="$typed_data_home" \
  "$typed_runtime_root/current/bin/yard" update --yes \
  --runtime-root "$typed_runtime_root" --rollback >/dev/null
[ "$("$typed_runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.1-test ] \
  && [ -f "$typed_history_source" ] && [ ! -e "$typed_history_destination" ] \
  && jq -e '
    .layout == 2 and .applied == ["migrate-test-yard-owner"]
  ' "$typed_config_home/migrations/state.json" >/dev/null \
  || fail 'extended registry rollback did not restore the applied prefix'

migration_source="$SUBYARD_CONFIG_HOME/migration-fixture/legacy/config.env"
migration_destination="$SUBYARD_CONFIG_HOME/migration-fixture/current/config.env"
migration_final="$SUBYARD_CONFIG_HOME/migration-fixture/final/config.env"
install -d -m 0700 "$(dirname "$migration_source")"
printf 'TOKEN=synthetic-layout\n' > "$migration_source"
chmod 0600 "$migration_source"
state_before_check="$(sha256sum "$SUBYARD_CONFIG_HOME/migrations/state.json")"
YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/cache" \
  installed_update --runtime-root "$runtime_root" --version 1.1.0-test --check >/dev/null
[ "$state_before_check" = "$(sha256sum "$SUBYARD_CONFIG_HOME/migrations/state.json")" ] \
  && [ -f "$migration_source" ] && [ ! -e "$migration_destination" ] \
  || fail 'update check changed config or migration state'

partial="$TMP/partial"; install -d "$partial"
install -m 0644 "$bundle_two" "$partial/$(basename "$bundle_two")"
install -m 0644 "$bundle_two.sha256" "$bundle_two.manifest.json" "$partial/"
if YARD_RELEASE_BASE_URL="file://$partial" YARD_RELEASE_CACHE="$TMP/partial-cache" \
  yard_update --runtime-root "$runtime_root" --version 1.1.0-test >/dev/null 2>&1; then
  fail 'incomplete release unexpectedly installed'
fi
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.0-test ] \
  || fail 'interrupted/incomplete update replaced the current runtime'

# Exercise the pinned v0.1 updater; candidate startup must finalize its prepared migration.
installed_update --runtime-root "$runtime_root" --version 1.1.0-test >/dev/null
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.1.0-test ] \
  || fail 'runtime upgrade did not switch current'
[ "$("$runtime_root/previous/bin/yard" --version | awk '{print $2}')" = 1.0.0-test ] \
  || fail 'runtime upgrade did not retain previous'
[ ! -e "$migration_source" ] && [ ! -L "$migration_source" ] \
  && grep -Fxq 'TOKEN=synthetic-layout' "$migration_destination" \
  && jq -e '.layout == 2' "$SUBYARD_CONFIG_HOME/migrations/state.json" >/dev/null \
  || fail 'v0.1-style update did not commit the synthetic layout migration'
installed_update --runtime-root "$runtime_root" --rollback >/dev/null
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.0.0-test ] \
  || fail 'runtime rollback did not restore previous'
[ "$("$runtime_root/previous/bin/yard" --version | awk '{print $2}')" = 1.1.0-test ] \
  || fail 'runtime rollback did not retain the replaced release'
[ -f "$migration_source" ] && [ ! -e "$migration_destination" ] \
  && jq -e '.layout == 1' "$SUBYARD_CONFIG_HOME/migrations/state.json" >/dev/null \
  || fail 'runtime rollback did not restore the previous data layout'

installed_update --runtime-root "$runtime_root" --version 1.1.0-test >/dev/null
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.1.0-test ] \
  && [ ! -e "$migration_source" ] && [ -f "$migration_destination" ] \
  && jq -e '.layout == 2' "$SUBYARD_CONFIG_HOME/migrations/state.json" >/dev/null \
  || fail 'roll-forward through the same release pair was not idempotent'
same_current_target="$(readlink "$runtime_root/current")"
same_previous_target="$(readlink "$runtime_root/previous")"
same_version_output="$(installed_update --runtime-root "$runtime_root" --version 1.1.0-test)"
[ "$(readlink "$runtime_root/current")" = "$same_current_target" ] \
  && [ "$(readlink "$runtime_root/previous")" = "$same_previous_target" ] \
  || fail 'same-version update changed the current/previous rollback pair'
grep -Fxq 'runtime yard-engine 1.1.0-test and migrations are current' \
  <<<"$same_version_output" \
  && ! grep -Fq 'installed runtime' <<<"$same_version_output" \
  && ! grep -Fq '{"schema_version"' <<<"$same_version_output" \
  || fail "clean same-version update was noisy or misleading: $same_version_output"

chmod 0664 "$legacy_state"
same_reconcile_output="$(installed_update --runtime-root "$runtime_root" --version 1.1.0-test)"
grep -Fxq 'reconciled runtime yard-engine 1.1.0-test' <<<"$same_reconcile_output" \
  && [ "$(stat -c '%a' "$legacy_state")" = 600 ] \
  || fail "same-version update did not report its real repair: $same_reconcile_output"
[ "$(readlink "$runtime_root/current")" = "$same_current_target" ] \
  && [ "$(readlink "$runtime_root/previous")" = "$same_previous_target" ] \
  || fail 'same-version reconcile changed the current/previous rollback pair'

artifact_three="$("$ROOT/dev/package-engine.sh" --output-dir "$release" --version 1.2.0-test \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-3.json")"
jq -e '.version == "1.2.0-test" and .configLayout == 3' \
  "$artifact_three.manifest.json" >/dev/null \
  || fail 'third runtime did not publish layout 3'
installed_update --runtime-root "$runtime_root" --version 1.2.0-test >/dev/null
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.2.0-test ] \
  && [ ! -e "$migration_destination" ] && [ ! -L "$migration_destination" ] \
  && grep -Fxq 'TOKEN=synthetic-layout' "$migration_final" \
  && jq -e '.layout == 3' "$SUBYARD_CONFIG_HOME/migrations/state.json" >/dev/null \
  || fail 'second migration rotation did not commit layout 3'
[ "$(find "$SUBYARD_CONFIG_HOME/migrations/transactions" -mindepth 1 -maxdepth 1 -type d | wc -l)" -eq 1 ] \
  || fail 'stale recovery was not removed after the next successful rotation'

installed_update --runtime-root "$runtime_root" --rollback >/dev/null
[ "$("$runtime_root/current/bin/yard" --version | awk '{print $2}')" = 1.1.0-test ] \
  && [ -f "$migration_destination" ] && [ ! -e "$migration_final" ] \
  && jq -e '.layout == 2' "$SUBYARD_CONFIG_HOME/migrations/state.json" >/dev/null \
  || fail 'layout 3 rollback did not restore runtime and data layout 2'

printf 'ok: release runtimes and versioned migrations are verified, offline-safe and rollback-capable\n'

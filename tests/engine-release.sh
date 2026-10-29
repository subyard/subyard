#!/usr/bin/env bash
# Host-free release artifact, checksum, atomic upgrade and rollback contract.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-engine-release.XXXXXX)"
incus_pid=''
cleanup() {
  local rc=$?
  trap - EXIT
  if [ -n "$incus_pid" ]; then
    kill "$incus_pid" 2>/dev/null || true
    wait "$incus_pid" 2>/dev/null || true
  fi
  if [ "$rc" -eq 0 ]; then rm -rf "$TMP"; else printf 'engine-release: failed fixture retained at %s\n' "$TMP" >&2; fi
  exit "$rc"
}
trap cleanup EXIT

# Copy today's public inputs because the packaging checks add local fixtures.
bash "$ROOT/tests/helpers/source-files.sh" > "$TMP/source-files"
mkdir "$TMP/source"
tar -C "$ROOT" --null -T "$TMP/source-files" -cf - | tar -C "$TMP/source" -xpf -
ROOT="$TMP/source"
cd "$ROOT"
# A restrictive checkout must still produce public executable runtime assets.
chmod 0700 "$ROOT/config/preview/subyard-preview"
chmod 0600 "$ROOT/config/preview/instructions.md"

release="$TMP/release"
# Release activation observes a reachable native API with an absent instance,
# rather than confusing a missing transport with an absent managed yard.
go build -o "$TMP/empty-incus" ./internal/testkit/cmd/empty-incus
"$TMP/empty-incus" "$TMP" > "$TMP/empty-incus.log" 2>&1 &
incus_pid=$!
for ((attempt = 0; attempt < 100; attempt++)); do
  if [ -S "$TMP/incus.socket" ] && grep -Fxq ready "$TMP/empty-incus.log"; then
    break
  fi
  kill -0 "$incus_pid" 2>/dev/null || { printf 'engine release: Incus fixture exited\n' >&2; exit 1; }
  sleep 0.1
done
[ -S "$TMP/incus.socket" ] && grep -Fxq ready "$TMP/empty-incus.log" \
  || { printf 'engine release: Incus fixture startup timed out\n' >&2; exit 1; }
export SUBYARD_INCUS_SOCKET="$TMP/incus.socket"
export SUBYARD_OPERATOR_HOME="$TMP/home"
export SUBYARD_CONFIG_HOME="$TMP/config"
export SUBYARD_HOME="$TMP/data"
export SUBYARD_POWER_RECONCILER_PATH="$TMP/missing-power-reconciler"
export SUBYARD_POWER_UNIT_PATH="$TMP/missing-power-unit"

fail() { printf 'engine release: %s\n' "$*" >&2; exit 1; }
# The installer owns startup-file additions and launcher links, not the
# permissions, ownership or existing files of their containing directories.
directory_identity() {
  stat -c '%a:%u:%g:%d:%i' "$1"
  sha256sum "$1/.operator-sentinel"
  stat -c '%a:%u:%g' "$1/.operator-sentinel"
}
directory_sentinel() {
  printf 'existing operator directory content\n' > "$1/.operator-sentinel"
  chmod 0600 "$1/.operator-sentinel"
}
yard_update() { YARD_ENGINE_PATH="$release_engine" "$ROOT/bin/yard" update --yes "$@"; }
installed_update() {
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/cache" \
    "$runtime_root/current/bin/yard" update --yes "$@"
}

workflow="$ROOT/.github/workflows/release.yml"
publish_workflow="$ROOT/.github/workflows/release-publish.yml"
[ -r "$workflow" ] && [ -r "$publish_workflow" ] || fail 'release workflows are missing'
grep -Fq 'contents: read' "$workflow" \
  && grep -Fq 'for arch in amd64 arm64' "$workflow" \
  && grep -Fq 'dev/release-publication.py export-ci' "$workflow" \
  && grep -Fq 'contents: write' "$publish_workflow" \
  && grep -Fq 'dev/release-publication.py publish-ci' "$publish_workflow" \
  || fail 'release preparation/publication must retain both supported runtime architectures'

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
printf 'untracked public input\n' > "$untracked_canary"
chmod 0600 "$staging_canary" "$qa_canary" "$untracked_canary"
# A newly added profile must package without requiring a staged Git entry.
fixture_profile="$ROOT/config/profiles/package-fixture"
mkdir -p "$fixture_profile/cmd/worker" "$fixture_profile/tests"
printf '%s\n' '{"schema_version":1,"native":[{"package":"cmd/worker","path":"bin/worker"}]}' \
  > "$fixture_profile/profile.json"
printf '%s\n' '# Synthetic profile' > "$fixture_profile/profile.conf"
printf '%s\n' 'package main' 'import "fmt"' 'func main() { fmt.Println("profile native fixture") }' \
  > "$fixture_profile/cmd/worker/main.go"
printf '%s\n' 'runtime fixture' > "$fixture_profile/asset.txt"
chmod 0600 "$fixture_profile/asset.txt"
printf '%s\n' 'nested runtime manifest fixture' > "$fixture_profile/runtime-files.sha256"
printf '%s\n' '# Test-only fixture' > "$fixture_profile/tests/run.sh"
fixture_integration="$ROOT/config/agents/package-fixture"
mkdir -p "$fixture_integration/cmd/worker" "$fixture_integration/tests"
printf '%s\n' '{"schema_version":1,"native":[{"package":"cmd/worker","path":"bin/worker"}]}' \
  > "$fixture_integration/integration.json"
printf '%s\n' 'package main' 'import "fmt"' 'func main() { fmt.Println("integration native fixture") }' \
  > "$fixture_integration/cmd/worker/main.go"
printf '%s\n' '# Test-only fixture' > "$fixture_integration/tests/run.sh"
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
# Checkout and packaging umasks must not change public runtime permissions.
chmod 0700 "$ROOT/config/preview/subyard-preview" "$ROOT/scripts/prepare-preview-route.sh"
artifact_one="$(umask 0077; "$ROOT/dev/package-engine.sh" --output-dir "$release" --version 1.0.0-test \
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
! grep -Eq '^\./(bin/subyard-release-owner|config/release-owner(\.schema|\-build)?\.json)$' "$bundle_list" \
  || fail 'ordinary Go runtime unexpectedly advertises the experimental Rust owner'
! grep -Eq '^\./config/profiles/[^/]+/(tests/|.*\.(go|rs)$)' "$bundle_list" \
  || fail 'runtime bundle contains profile-owned tests or Go source'
! grep -Eq '^\./config/(agents|retired-integrations)/[^/]+/(tests/|.*\.go$)' "$bundle_list" \
  || fail 'runtime bundle contains integration-owned tests or Go source'
grep -Fxq './config/agents/package-fixture/bin/worker' "$bundle_list" \
  || fail 'runtime bundle omitted an untracked integration native executable'
! grep -Eq '^\./config/agents/aiobserver/' "$bundle_list" || fail 'runtime bundle retained the retired active integration'
grep -Fxq './config/profiles/package-fixture/asset.txt' "$bundle_list" \
  && grep -Fxq './config/profiles/package-fixture/bin/worker' "$bundle_list" \
  || fail 'runtime bundle omitted untracked profile assets or native executable'
grep -Fxq './bin/yard' "$bundle_list" \
  && grep -Fxq './bin/yard-engine' "$bundle_list" \
  && grep -Fxq './internal/adapters/configmaterial/tomli_w.py' "$bundle_list" \
  && grep -Fxq './internal/adapters/configmaterial/TOMLI_W_LICENSE' "$bundle_list" \
  && grep -Fxq './scripts/install-runtime-release.sh' "$bundle_list" \
  && grep -Fxq './scripts/install-ssh-relay.sh' "$bundle_list" \
  && grep -Fxq './scripts/install-test-vms-host-sink.sh' "$bundle_list" \
  && grep -Fxq './scripts/reconcile-integrations.sh' "$bundle_list" \
  && grep -Fxq './scripts/prepare-preview-route.sh' "$bundle_list" \
  && grep -Fxq './scripts/e2e-lab/base.sh' "$bundle_list" \
  && grep -Fxq './config/systemd/subyard-test-vms-host-sink.service.in' "$bundle_list" \
  && grep -Fxq './config/systemd/subyard-test-vms-host-sink.timer.in' "$bundle_list" \
  && grep -Fxq './config/commands.registry' "$bundle_list" \
  && grep -Fxq './config/migrations.json' "$bundle_list" \
  && grep -Fxq './config/release-transition.json' "$bundle_list" \
  && grep -Fxq './config/preview/subyard-preview' "$bundle_list" \
  && grep -Fxq './config/preview/instructions.md' "$bundle_list" \
  && grep -Fxq './config/agents/codex/provision.sh' "$bundle_list" \
  && grep -Fxq './config/retired-integrations/aiobserver/retire.py' "$bundle_list" \
  && grep -Fxq './config/profiles/openlit/dashboard.py' "$bundle_list" \
  && grep -Fxq './scripts/lib/preview-proxy.sh' "$bundle_list" \
  || fail 'runtime bundle does not contain the complete launcher contract'
# Every profile owns its asset inventory; packaging must preserve all runtime
# inputs, including hooks, regardless of the user's selected profiles.
while IFS= read -r -d '' profile_asset; do
  case "$profile_asset" in */tests/*|*.go|*.rs|*/Cargo.toml|*/Cargo.lock|*/go.mod|*/go.sum) continue ;; esac
  grep -Fxq "./$profile_asset" "$bundle_list" \
    || fail "runtime bundle omitted profile asset: $profile_asset"
done < <(find config/profiles -type f -print0)
grep -Fxq './runtime-files.sha256' "$bundle_list" \
  || fail 'runtime bundle exact file manifest is missing'
! grep -Fq "$(basename "$staging_canary")" "$bundle_list" \
  && ! grep -Fq "$(basename "$qa_canary")" "$bundle_list" \
  || fail 'runtime bundle contains an ignored host-local canary'
grep -Fq "$(basename "$untracked_canary")" "$bundle_list" \
  || fail 'runtime bundle omitted an untracked public input'
bundle_extract="$TMP/bundle-extract"
install -d "$bundle_extract"
tar -xpzf "$bundle_one" -C "$bundle_extract"
for directory in config/profiles/package-fixture/bin config/agents/package-fixture/bin internal/adapters/configmaterial; do
  [ "$(stat -c '%a' "$bundle_extract/$directory")" = 755 ] \
    || fail 'runtime native or helper directory does not have public mode'
done
python3 - "$bundle_one" <<'PY'
import sys
import tarfile

with tarfile.open(sys.argv[1], 'r:gz') as bundle:
    assert all(member.mode == 0o755 for member in bundle.getmembers() if member.isdir()), \
        'runtime bundle directory permissions depend on packaging umask'
PY
for support in tomli_w.py TOMLI_W_LICENSE; do
  [ "$(stat -c '%a' "$bundle_extract/internal/adapters/configmaterial/$support")" = 644 ] \
    && cmp -s "$ROOT/internal/adapters/configmaterial/$support" "$bundle_extract/internal/adapters/configmaterial/$support" \
    || fail 'runtime bundle changed shared TOML support bytes or public data mode'
done
[ "$(stat -c '%a' "$bundle_extract/config/preview/subyard-preview")" = 755 ] \
  && cmp -s "$ROOT/config/preview/subyard-preview" "$bundle_extract/config/preview/subyard-preview" \
  || fail 'runtime bundle changed preview helper bytes or executable mode'
[ "$(stat -c '%a' "$bundle_extract/config/preview/instructions.md")" = 644 ] \
  && cmp -s "$ROOT/config/preview/instructions.md" "$bundle_extract/config/preview/instructions.md" \
  || fail 'runtime bundle changed preview instruction bytes or public data mode'
[ "$(stat -c '%a' "$bundle_extract/scripts/prepare-preview-route.sh")" = 755 ] \
  && cmp -s "$ROOT/scripts/prepare-preview-route.sh" "$bundle_extract/scripts/prepare-preview-route.sh" \
  && [ "$(stat -c '%a' "$bundle_extract/config/profiles/package-fixture/asset.txt")" = 644 ] \
  && cmp -s "$fixture_profile/asset.txt" "$bundle_extract/config/profiles/package-fixture/asset.txt" \
  || fail 'runtime bundle permissions depend on checkout or packaging umask'
[ "$(stat -c '%a' "$ROOT/config/preview/subyard-preview")" = 700 ] \
  && [ "$(stat -c '%a' "$ROOT/scripts/prepare-preview-route.sh")" = 700 ] \
  && [ "$(stat -c '%a' "$fixture_profile/asset.txt")" = 600 ] \
  || fail 'packaging changed source permissions'
release_umask="$TMP/release-0022"
(umask 0022; "$ROOT/dev/package-engine.sh" --output-dir "$release_umask" --version 1.0.0-test \
  --migration-registry "$ROOT/tests/fixtures/migrations/layout-1.json") >/dev/null
cmp -s "$bundle_one" "$release_umask/$(basename "$bundle_one")" \
  && cmp -s "$artifact_one" "$release_umask/$(basename "$artifact_one")" \
  || fail 'runtime or engine bytes depend on packaging umask'
[ "$("$bundle_extract/config/profiles/package-fixture/bin/worker")" = "profile native fixture" ] \
  || fail 'packaged native profile executable does not run'
[ "$("$bundle_extract/config/agents/package-fixture/bin/worker")" = "integration native fixture" ] \
  && [ "$(stat -c '%a' "$bundle_extract/config/agents/package-fixture/bin/worker")" = 755 ] \
  || fail 'packaged native integration executable does not run or has an invalid mode'
(
  cd "$bundle_extract"
  sha256sum -c runtime-files.sha256 >/dev/null
  find . -type f ! -path './runtime-files.sha256' -print | sort > "$TMP/bundle-actual.list"
  sed -E 's/^[0-9a-fA-F]{64}  //' runtime-files.sha256 | sort > "$TMP/bundle-declared.list"
)
cmp -s "$TMP/bundle-actual.list" "$TMP/bundle-declared.list" \
  || fail 'runtime bundle file manifest is not exact'
for excluded in update-engine.sh power-state.sh bootstrap-runtime.sh build-engine.sh package-engine.sh install-cli.sh; do
  ! grep -Fq "/$excluded" "$bundle_list" \
    || fail "runtime bundle contains non-runtime script $excluded"
done

# The inventory leaf is opt-in for retained older engines. A declared but
# malformed/unknown capability must never enter that compatibility path.
capability_bundle="$TMP/installation-capability.tar.gz"
for capability_mutant in unknown malformed directory; do
  case "$capability_mutant" in
    unknown) printf '%s\n' '{"schemaVersion":1,"contract":"unknown"}' > "$bundle_extract/config/installation-capabilities.json" ;;
    malformed) printf '{' > "$bundle_extract/config/installation-capabilities.json" ;;
    directory) rm -f -- "$bundle_extract/config/installation-capabilities.json"; mkdir "$bundle_extract/config/installation-capabilities.json" ;;
  esac
  (cd "$bundle_extract" && find . -type f ! -path './runtime-files.sha256' -print0 | sort -z | xargs -0 sha256sum > runtime-files.sha256)
  tar -czf "$capability_bundle" -C "$bundle_extract" .
  sha256sum "$capability_bundle" > "$capability_bundle.sha256"
  cp "$bundle_one.manifest.json" "$capability_bundle.manifest.json"
  capability_digest="$(sha256sum "$capability_bundle" | cut -d' ' -f1)"
  jq --arg artifact "$(basename "$capability_bundle")" --arg digest "$capability_digest" \
    '.artifact=$artifact | .sha256=$digest' "$bundle_one.provenance.json" > "$capability_bundle.provenance.json"
  capability_root="$TMP/capability-$capability_mutant"
  if "$ROOT/scripts/install-runtime-release.sh" --runtime-root "$capability_root" \
    --bundle "$capability_bundle" --checksum "$capability_bundle.sha256" \
    --manifest "$capability_bundle.manifest.json" --provenance "$capability_bundle.provenance.json" \
    > "$TMP/capability-$capability_mutant.log" 2>&1; then
    fail 'invalid installation capability was accepted'
  fi
  grep -Fq 'unsupported installation capability' "$TMP/capability-$capability_mutant.log" \
    && [ ! -e "$capability_root/current" ] && [ ! -L "$capability_root/current" ] \
    && [ ! -e "$capability_root/installation.json" ] \
    || fail 'invalid installation capability changed activation or inventory'
done
jq -e '.schemaVersion == 1 and .version == "1.0.0-test" and .rpc.min == 1 and .rpc.max == 1 and
  .projectStateSchema == 1 and .credentialSchema == 1' "$artifact_one.manifest.json" >/dev/null
jq -e '.schemaVersion == 1 and .version == "1.0.0-test" and
  .sourceRepository == "github.com/Dmitry-Borodin/Subyard" and
  .canonicalRepository == "github.com/Subyard/Subyard" and .sourceRevision == "unknown" and
  (.sha256 | length == 64)' \
  "$artifact_one.provenance.json" >/dev/null
rpc_negotiate "$artifact_one" 1.0.0-test 1 compatible artifact-one-v1
rpc_negotiate "$artifact_one" 1.0.0-test 2 incompatible artifact-one-v2

standalone_home="$TMP/standalone-home"
standalone_bin="$TMP/standalone-bin"
standalone_no_go="$TMP/standalone-no-go"
install -d "$standalone_home" "$standalone_bin" "$standalone_no_go"
chmod 0700 "$standalone_home"
chmod 0750 "$standalone_bin"
directory_sentinel "$standalone_home"
directory_sentinel "$standalone_bin"
standalone_home_before="$(directory_identity "$standalone_home")"
standalone_bin_before="$(directory_identity "$standalone_bin")"
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
staging_home="$TMP/staging-home"
staging_runtime="$staging_home/runtime"
mkdir -p "$staging_home/.config/subyard" "$staging_home/.subyard/recovery/pre-go-source"
printf 'YARD_PROFILES=one\nENVIRONMENT_PROFILES=two\n' > "$staging_home/.config/subyard/config.env"
printf 'unsupported source recovery fixture\n' > "$staging_home/.subyard/recovery/pre-go-source/transaction"
chmod 0600 "$staging_home/.config/subyard/config.env" "$staging_home/.subyard/recovery/pre-go-source/transaction"
staging_config_before="$(sha256sum "$staging_home/.config/subyard/config.env")"
if HOME="$staging_home" YARD_RELEASE_VERSION='' \
  "$release/subyard-install.sh" --publish-only >/dev/null 2>&1; then
  fail 'staging accepted an unpinned latest release'
fi
# shellcheck disable=SC2034 # repetition is the supported staging no-op check
for stage_repeat in 1 2; do
  staging_output="$(HOME="$staging_home" SUBYARD_HOME="$staging_home/.subyard" \
    SUBYARD_CONFIG_HOME="$staging_home/.config/subyard" YARD_RUNTIME_ROOT="$staging_runtime" \
    YARD_RELEASE_BASE_URL="file://$release" \
    "$release/subyard-install.sh" --publish-only --version 1.0.0-test </dev/null)"
  [[ "$staging_output" == *"staged release=releases/1.0.0-test-"*"candidate=$staging_runtime/releases/1.0.0-test-"* ]] \
    || fail 'staging did not report the exact verified candidate identity and path'
  [ ! -e "$staging_runtime/current" ] && [ ! -L "$staging_runtime/current" ] \
    && [ ! -e "$staging_runtime/previous" ] && [ ! -L "$staging_runtime/previous" ] \
    && [ ! -e "$staging_home/.local/bin/yard" ] && [ ! -e "$staging_home/.bashrc" ] \
    && [ ! -e "$staging_home/.profile" ] && [ ! -e "$staging_home/.config/subyard/migrations" ] \
    && [ "$staging_config_before" = "$(sha256sum "$staging_home/.config/subyard/config.env")" ] \
    || fail 'staging changed activation, entrypoints, shell or configuration state'
done
HOME="$standalone_home" SUBYARD_HOME="$standalone_home/.subyard" \
  SUBYARD_CONFIG_HOME="$standalone_home/.config/subyard" YARD_BIN_DIR="$standalone_bin" \
  SHELL=/bin/bash ZDOTDIR="$standalone_home" YARD_SHELL_RC='' YARD_LOGIN_RC='' \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
  PATH="$standalone_no_go:$PATH" \
  "$release/subyard-install.sh" --yes >/dev/null
[ "$(directory_identity "$standalone_home")" = "$standalone_home_before" ] \
  && [ "$(directory_identity "$standalone_bin")" = "$standalone_bin_before" ] \
  || fail 'bootstrap changed existing default shell parent or binary directory identity'
[ "$($standalone_bin/yard --version)" = 'yard 1.0.0-test' ] \
  && [ -L "$standalone_bin/sy" ] && [ ! -e "$TMP/standalone-go-invoked" ] \
  || fail 'standalone installer did not publish a usable runtime without a checkout'
standalone_ledger="$standalone_home/.config/subyard/release-transition/v2/ledger.json"
jq -e '.domains["power-metadata"] == {"epoch": 2, "applied": ["introduce-owner-publication-state-v1"]}' \
  "$standalone_ledger" >/dev/null \
  || fail 'fresh bootstrap published entrypoints before the optional publication reader boundary'
standalone_ledger_before="$(sha256sum "$standalone_ledger")"
# Exercise the same generic bootstrap used by fresh optional-profile fixtures,
# including real installer publication and its protected reader admission.
(
  fixture="$TMP/profile-bootstrap-source"
  state="$TMP/profile-bootstrap-state"
  install -d "$fixture/.subyard-acceptance/release" "$state/home"
  cp -a "$release/." "$fixture/.subyard-acceptance/release/"
  case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) exit 1 ;; esac
  runtime_file="subyard-1.0.0-test-linux-$arch.tar.gz"
  checksum="$(sha256sum "$release/$runtime_file" | awk '{print $1}')"
  printf '{"version":"1.0.0-test","runtime_file":"%s","runtime_sha256":"%s"}\n' \
    "$runtime_file" "$checksum" > "$fixture/.subyard-acceptance/candidate.json"
  export HOME="$state/home" SUBYARD_OPERATOR_HOME="$state/home"
  export SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
  export YARD_RUNTIME_ROOT="$state/runtime" SHELL=/bin/bash
  export SUBYARD_NO_AUDIT=1
  unset YARD_ENGINE_PATH
  [ ! -e "$SUBYARD_HOME" ] || fail 'fresh profile fixture data directory already exists'
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  launcher="$(release_candidate_install "$fixture" "$state")"
  [ "$launcher" = "$state/bin/yard" ] && [ -x "$launcher" ] \
    || fail 'profile fixture bootstrap did not publish its isolated launcher'
  [ "$(stat -c '%a' "$SUBYARD_HOME")" = 700 ] \
    && [ "$(stat -c '%a' "$SUBYARD_HOME/installation.json")" = 600 ] \
    || fail 'profile fixture bootstrap did not publish private installation ownership'
  jq -e --arg state "$state" --argjson uid "$(id -u)" \
    '.schema == 1 and .operatorUid == $uid and .dataHome == ($state + "/data")
      and .configHome == ($state + "/config") and .runtimeRoot == ($state + "/runtime")
      and .cacheRoot == ($state + "/releases") and .binDir == ($state + "/bin")
      and .shellFiles == [($state + "/bashrc"), ($state + "/profile")]' \
    "$SUBYARD_HOME/installation.json" >/dev/null \
    || fail 'profile fixture bootstrap installation ownership has incorrect path bindings'
  "$launcher" _publication-contracts >/dev/null \
    || fail 'profile fixture bootstrap left the protected publication reader uncommitted'
  [ -f "$state/bashrc" ] && [ -f "$state/profile" ] \
    && [ ! -e "$HOME/.bashrc" ] && [ ! -e "$HOME/.profile" ] \
    && [ ! -e "$HOME/.local/bin/yard" ] \
    || fail 'profile fixture bootstrap wrote outside its explicit startup and binary paths'
)
grep -Fq 'Subyard CLI login PATH' "$standalone_home/.profile" \
  && grep -Fq 'Subyard CLI interactive PATH' "$standalone_home/.bashrc" \
  && grep -Fq 'Subyard CLI completion' "$standalone_home/.bashrc" \
  || fail 'standalone installer did not configure new login and interactive shells'
HOME="$standalone_home" PATH=/usr/bin:/bin SHELL=/bin/bash bash -lc \
  'command -v yard >/dev/null && yard --version >/dev/null' \
  || fail 'standalone installer is not available to a new login shell'
HOME="$standalone_home" PATH=/usr/bin:/bin SHELL=/bin/bash bash -lic \
  'complete -p yard >/dev/null' >/dev/null 2>&1 \
  || fail 'standalone installer did not automatically activate login Bash completion'
HOME="$standalone_home" PATH=/usr/bin:/bin SHELL=/bin/bash \
  bash --noprofile --rcfile "$standalone_home/.bashrc" -ic \
  'command -v yard >/dev/null && complete -p yard >/dev/null' >/dev/null 2>&1 \
  || fail 'standalone installer did not activate Bash completion'
HOME="$standalone_home" ZDOTDIR="$standalone_home" PATH=/usr/bin:/bin SHELL=/bin/zsh \
  zsh -ic 'command -v yard >/dev/null && (( $+functions[_yard] ))' >/dev/null 2>&1 \
  || fail 'standalone installer did not automatically activate Zsh completion'
zsh -f "$ROOT/tests/helpers/zsh-completion-buffers.zsh" \
  "$standalone_home/.subyard/runtime/current/completions/yard.zsh" \
  "$standalone_home/.subyard/runtime/current" \
  || fail 'standalone runtime Zsh completion corrupted the command buffer'
HOME="$standalone_home" SUBYARD_HOME="$standalone_home/.subyard" \
  SUBYARD_CONFIG_HOME="$standalone_home/.config/subyard" YARD_BIN_DIR="$standalone_bin" \
  SHELL=/bin/bash ZDOTDIR="$standalone_home" YARD_SHELL_RC='' YARD_LOGIN_RC='' \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
  PATH="$standalone_no_go:$PATH" \
  "$release/subyard-install.sh" --yes >/dev/null
[ "$(sha256sum "$standalone_ledger")" = "$standalone_ledger_before" ] \
  || fail 'bootstrap retry rewrote the committed reader history'
[ "$(directory_identity "$standalone_home")" = "$standalone_home_before" ] \
  && [ "$(directory_identity "$standalone_bin")" = "$standalone_bin_before" ] \
  || fail 'bootstrap retry changed existing default shell parent or binary directory identity'
# An authentic retained publisher cannot bypass the current full-bootstrap API
# boundary. The same verified archive remains stageable by a current publisher.
boundary_assets="$TMP/boundary-assets"
mkdir -m 0700 "$boundary_assets"
boundary_bundle="$boundary_assets/$(basename "$bundle_one")"
cp "$legacy_031_installer" "$boundary_assets/subyard-install-runtime-release.sh"
(cd "$boundary_assets" && sha256sum subyard-install-runtime-release.sh > subyard-install-runtime-release.sh.sha256)
rmdir "$bundle_extract/config/installation-capabilities.json"
for boundary_case in absent malformed api; do
  case "$boundary_case" in
    absent) ;;
    malformed) printf '{' > "$bundle_extract/config/installation-capabilities.json" ;;
    api)
      cp "$ROOT/config/installation-capabilities.json" "$bundle_extract/config/installation-capabilities.json"
      cat > "$bundle_extract/bin/yard-engine" <<'NO_INSTALLATION_API'
#!/bin/sh
[ "${1:-}" = --version ] && { echo 'yard-engine 1.0.0-test'; exit 0; }
exit 2
NO_INSTALLATION_API
      chmod 0755 "$bundle_extract/bin/yard-engine" ;;
  esac
  (cd "$bundle_extract" && find . -type f ! -path './runtime-files.sha256' -print0 | sort -z | xargs -0 sha256sum > runtime-files.sha256)
  tar -czf "$boundary_bundle" -C "$bundle_extract" .
  (cd "$boundary_assets" && sha256sum "$(basename "$boundary_bundle")" > "$(basename "$boundary_bundle").sha256")
  cp "$bundle_one.manifest.json" "$boundary_bundle.manifest.json"
  jq --arg digest "$(sha256sum "$boundary_bundle" | cut -d' ' -f1)" \
    '.sha256=$digest' "$bundle_one.provenance.json" > "$boundary_bundle.provenance.json"
  for boundary_kind in initial-absent initial-existing existing; do
    boundary_home="$TMP/boundary-$boundary_case-$boundary_kind"
    boundary_bin="$boundary_home/bin"
    if [ "$boundary_kind" = existing ]; then
      boundary_home="$standalone_home"
      boundary_bin="$standalone_bin"
      boundary_before="$(readlink "$standalone_home/.subyard/runtime/current"):$(readlink "$standalone_home/.subyard/runtime/previous" || true):$(readlink "$standalone_bin/yard"):$(sha256sum "$standalone_home/.subyard/installation.json" "$standalone_home/.subyard/runtime/installation.json" "$standalone_home/.bashrc" "$standalone_home/.profile" "$standalone_ledger")"
    else
      mkdir -m 0700 "$boundary_home"
      if [ "$boundary_kind" = initial-existing ]; then
        mkdir -p "$boundary_home/.config/subyard"
        chmod 0755 "$boundary_home/.config/subyard"
        directory_sentinel "$boundary_home/.config/subyard"
        boundary_config_before="$(directory_identity "$boundary_home/.config/subyard")"
      fi
    fi
    if HOME="$boundary_home" SUBYARD_HOME="$boundary_home/.subyard" \
      SUBYARD_CONFIG_HOME="$boundary_home/.config/subyard" YARD_BIN_DIR="$boundary_bin" \
      SHELL=/bin/bash ZDOTDIR="$boundary_home" YARD_SHELL_RC='' YARD_LOGIN_RC='' \
      YARD_RELEASE_BASE_URL="file://$boundary_assets" YARD_RELEASE_VERSION=1.0.0-test \
      "$release/subyard-install.sh" --yes > "$TMP/boundary-$boundary_case-$boundary_kind.log" 2>&1; then
      fail "full bootstrap accepted $boundary_case inventory API"
    fi
    case "$boundary_case" in
      absent) grep -Fq 'full bootstrap requires installation-inventory-v1 capability' "$TMP/boundary-$boundary_case-$boundary_kind.log" ;;
      malformed) grep -Fq 'unsupported installation capability' "$TMP/boundary-$boundary_case-$boundary_kind.log" ;;
    esac
    if [ "$boundary_kind" = existing ]; then
      [ "$boundary_before" = "$(readlink "$standalone_home/.subyard/runtime/current"):$(readlink "$standalone_home/.subyard/runtime/previous" || true):$(readlink "$standalone_bin/yard"):$(sha256sum "$standalone_home/.subyard/installation.json" "$standalone_home/.subyard/runtime/installation.json" "$standalone_home/.bashrc" "$standalone_home/.profile" "$standalone_ledger")" ] \
        || fail 'refused bootstrap changed current release, inventory or shell files'
    else
      [ ! -e "$boundary_home/.subyard/runtime/current" ] && [ ! -L "$boundary_home/.subyard/runtime/current" ] \
        && [ ! -e "$boundary_home/.subyard/runtime/previous" ] && [ ! -L "$boundary_home/.subyard/runtime/previous" ] \
        && [ ! -e "$boundary_home/.subyard/runtime/installation.json" ] \
        && [ ! -e "$boundary_home/.subyard/installation.json" ] && [ ! -e "$boundary_home/bin/yard" ] && [ ! -L "$boundary_home/bin/yard" ] \
        && [ ! -e "$boundary_home/bin/sy" ] && [ ! -L "$boundary_home/bin/sy" ] \
        && [ ! -e "$boundary_home/.bashrc" ] && [ ! -e "$boundary_home/.profile" ] \
        || fail 'refused initial bootstrap published inventory or entrypoints'
      if [ "$boundary_kind" = initial-existing ]; then
        [ "$boundary_config_before" = "$(directory_identity "$boundary_home/.config/subyard")" ] \
          || fail 'refused initial bootstrap changed existing config directory'
      else
        [ ! -e "$boundary_home/.config/subyard" ] && [ ! -L "$boundary_home/.config/subyard" ] \
          || fail 'refused initial bootstrap created config directory'
      fi
    fi
  done
  if [ "$boundary_case" = absent ]; then
    cp "$ROOT/scripts/install-runtime-release.sh" "$boundary_assets/subyard-install-runtime-release.sh"
    (cd "$boundary_assets" && sha256sum subyard-install-runtime-release.sh > subyard-install-runtime-release.sh.sha256)
    HOME="$TMP/boundary-stage" SUBYARD_HOME="$TMP/boundary-stage/data" \
      SUBYARD_CONFIG_HOME="$TMP/boundary-stage/config" YARD_RUNTIME_ROOT="$TMP/boundary-stage/runtime" \
      YARD_RELEASE_BASE_URL="file://$boundary_assets" \
      "$release/subyard-install.sh" --version 1.0.0-test --publish-only --yes > "$TMP/boundary-stage.log" \
      || fail 'publication-only rejected a verified legacy archive'
    [ ! -e "$TMP/boundary-stage/runtime/current" ] && [ ! -L "$TMP/boundary-stage/runtime/current" ] \
      || fail 'publication-only activated a legacy archive'
    cp "$legacy_031_installer" "$boundary_assets/subyard-install-runtime-release.sh"
    (cd "$boundary_assets" && sha256sum subyard-install-runtime-release.sh > subyard-install-runtime-release.sh.sha256)
  fi
done
# A capable archive delivered alongside an old helper uses the embedded current
# publisher and records the exact downloaded helper, not the temporary publisher.
cp "$bundle_one" "$boundary_bundle"
cp "$bundle_one.sha256" "$boundary_bundle.sha256"
cp "$bundle_one.manifest.json" "$boundary_bundle.manifest.json"
cp "$bundle_one.provenance.json" "$boundary_bundle.provenance.json"
boundary_home="$TMP/boundary-capable"
mkdir -m 0700 "$boundary_home"
HOME="$boundary_home" SUBYARD_HOME="$boundary_home/.subyard" \
  SUBYARD_CONFIG_HOME="$boundary_home/.config/subyard" YARD_BIN_DIR="$boundary_home/bin" \
  SHELL=/bin/bash ZDOTDIR="$boundary_home" YARD_SHELL_RC='' YARD_LOGIN_RC='' \
  YARD_RELEASE_BASE_URL="file://$boundary_assets" YARD_RELEASE_VERSION=1.0.0-test \
  "$release/subyard-install.sh" --yes > "$TMP/boundary-capable.log" 2>&1 \
  || fail 'capable full bootstrap with historical helper failed'
jq -e '[.cacheFiles[].Path] | index("1.0.0-test/subyard-install-runtime-release.sh") != null and index("1.0.0-test/subyard-install-runtime-release.sh.sha256") != null' \
  "$boundary_home/.subyard/runtime/installation.json" >/dev/null \
  || fail 'historical downloaded helper was not recorded as a verified cache input'
# Full-scope metadata is assessed by the verified candidate before activation.
full_inventory="$standalone_home/.subyard/installation.json"
established_cache="$(jq -er '.cacheRoot' "$full_inventory")"
rm -f -- "$standalone_home/.subyard/runtime/installation.json"
HOME="$standalone_home" SUBYARD_HOME="$standalone_home/.subyard" \
  SUBYARD_CONFIG_HOME="$standalone_home/.config/subyard" YARD_RELEASE_CACHE="$TMP/staging-download-cache" \
  YARD_RELEASE_BASE_URL="file://$release" \
  "$release/subyard-install.sh" --publish-only --runtime-root "$standalone_home/.subyard/runtime" --version 1.0.0-test --yes >/dev/null
jq -e --arg cache "$established_cache" '.cacheRoot==$cache and .binDir=="" and (.shellFiles|length)==0' \
  "$standalone_home/.subyard/runtime/installation.json" >/dev/null \
  || fail 'canonical staging lost the established full-scope cache identity'
cp "$full_inventory" "$TMP/full-inventory.before"
bootstrap_link_before="$(readlink "$standalone_home/.subyard/runtime/current")"
bootstrap_entrypoints_before="$(sha256sum "$standalone_home/.bashrc" "$standalone_home/.profile")"
for invalid_inventory in malformed unknown-field conflicting-root; do
  case "$invalid_inventory" in
    malformed) printf '{' > "$full_inventory" ;;
    unknown-field) jq '.unexpected = true' "$TMP/full-inventory.before" > "$full_inventory" ;;
    conflicting-root) jq --arg path "$TMP/foreign-config" '.configHome=$path' "$TMP/full-inventory.before" > "$full_inventory" ;;
  esac
  invalid_inventory_before="$(sha256sum "$full_inventory")"
  if HOME="$standalone_home" SUBYARD_HOME="$standalone_home/.subyard" \
    SUBYARD_CONFIG_HOME="$standalone_home/.config/subyard" YARD_BIN_DIR="$standalone_bin" \
    SHELL=/bin/bash ZDOTDIR="$standalone_home" YARD_SHELL_RC='' YARD_LOGIN_RC='' \
    YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
    "$release/subyard-install.sh" --yes > "$TMP/bootstrap-$invalid_inventory.log" 2>&1; then
    fail "bootstrap accepted $invalid_inventory installation metadata"
  fi
  [ "$(sha256sum "$full_inventory")" = "$invalid_inventory_before" ] \
    && [ "$(readlink "$standalone_home/.subyard/runtime/current")" = "$bootstrap_link_before" ] \
    && [ "$(sha256sum "$standalone_home/.bashrc" "$standalone_home/.profile")" = "$bootstrap_entrypoints_before" ] \
    || fail 'invalid full inventory changed activation, entrypoints or ownership'
done
cp "$TMP/full-inventory.before" "$full_inventory"
# A first publisher can finish before config directory creation is interrupted.
# Its next bootstrap must complete the reader transition and publish entrypoints.
(
  export HOME="$TMP/bootstrap-no-config-home" SUBYARD_OPERATOR_HOME="$TMP/bootstrap-no-config-home"
  export SUBYARD_HOME="$HOME/.subyard" SUBYARD_CONFIG_HOME="$HOME/.config/subyard"
  export YARD_RUNTIME_ROOT="$SUBYARD_HOME/runtime" YARD_BIN_DIR="$HOME/.local/bin"
  export YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test
  mkdir -m 0700 "$HOME"
  "$release/subyard-install-runtime-release.sh" --runtime-root "$YARD_RUNTIME_ROOT" \
    --bundle "$bundle_one" --checksum "$bundle_one.sha256" --manifest "$bundle_one.manifest.json" \
    --provenance "$bundle_one.provenance.json" > "$TMP/bootstrap-no-config-stage.log"
  [ -L "$YARD_RUNTIME_ROOT/current" ] && [ ! -e "$SUBYARD_CONFIG_HOME" ] \
    || fail 'missing-config retry precondition was not established'
  "$release/subyard-install.sh" --yes > "$TMP/bootstrap-no-config-retry.log" 2>&1 \
    || { cat "$TMP/bootstrap-no-config-retry.log" >&2; fail 'bootstrap could not resume without config directory'; }
  [ -f "$SUBYARD_CONFIG_HOME/release-transition/v2/ledger.json" ] && [ -x "$YARD_BIN_DIR/yard" ] \
    || fail 'missing-config retry omitted protected transition or entrypoints'
)
# A rejected first transition keeps its installed owner, withholds entrypoints,
# and remains recoverable through the existing-runtime supported transition.
(
  export HOME="$TMP/bootstrap-retry-home" SUBYARD_OPERATOR_HOME="$TMP/bootstrap-retry-home"
  export SUBYARD_HOME="$HOME/.subyard" SUBYARD_CONFIG_HOME="$HOME/.config/subyard"
  export YARD_RUNTIME_ROOT="$SUBYARD_HOME/runtime" YARD_BIN_DIR="$HOME/.local/bin"
  export YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test
  install -d -m 0700 "$SUBYARD_CONFIG_HOME"
  printf 'YARD_TEMPLATE=unknown-bootstrap-template\n' > "$SUBYARD_CONFIG_HOME/config.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
  if "$release/subyard-install.sh" --yes > "$TMP/bootstrap-blocked.log" 2>&1; then
    fail 'fresh bootstrap accepted invalid scoped configuration'
  fi
  [ -L "$YARD_RUNTIME_ROOT/current" ] && [ ! -e "$YARD_BIN_DIR/yard" ] \
    || fail 'failed first transition published entrypoints or discarded its owner'
  : > "$SUBYARD_CONFIG_HOME/config.env"
  "$release/subyard-install.sh" --yes > "$TMP/bootstrap-retry.log" 2>&1
  jq -e '.domains["power-metadata"] == {"epoch": 2, "applied": ["introduce-owner-publication-state-v1"]}' \
    "$SUBYARD_CONFIG_HOME/release-transition/v2/ledger.json" >/dev/null
  [ -x "$YARD_BIN_DIR/yard" ] || fail 'supported bootstrap retry did not publish entrypoints'
)
# A proven cold host needs neither Incus, Go nor privilege escalation to install
# the CLI and commit its reader boundary. Retained state is tested by Incus's probe.
(
  export HOME="$TMP/bootstrap-cold-home" SUBYARD_OPERATOR_HOME="$TMP/bootstrap-cold-home"
  export SUBYARD_HOME="$HOME/.subyard" SUBYARD_CONFIG_HOME="$HOME/.config/subyard"
  export YARD_RUNTIME_ROOT="$SUBYARD_HOME/runtime" YARD_BIN_DIR="$HOME/.local/bin"
  export YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test
  export SUBYARD_INCUS_SOCKET='' INCUS_SOCKET='' INCUS_DIR="$TMP/bootstrap-cold-incus-state"
  native_tools="$TMP/bootstrap-cold-tools"
  mkdir "$native_tools"
  for tool in bash dirname uname jq sha256sum tar gzip cp chmod mv mktemp cut install readlink basename ln sed awk sort find env date id getent python3 cat head stat tr grep wc realpath flock rm cmp comm mkdir rmdir sync; do
    ln -s "$(command -v "$tool")" "$native_tools/$tool"
  done
  export PATH="$native_tools"
  "$release/subyard-install.sh" --yes > "$TMP/bootstrap-cold.log" 2>&1
  jq -e '.domains["power-metadata"] == {"epoch": 2, "applied": ["introduce-owner-publication-state-v1"]}' \
    "$SUBYARD_CONFIG_HOME/release-transition/v2/ledger.json" >/dev/null
  [ -x "$YARD_BIN_DIR/yard" ] || fail 'cold bootstrap did not publish entrypoints'
)
[ "$(grep -cF 'Subyard CLI login PATH' "$standalone_home/.profile")" -eq 1 ] \
  && [ "$(grep -cF 'Subyard CLI interactive PATH' "$standalone_home/.bashrc")" -eq 1 ] \
  && [ "$(grep -cF 'Subyard CLI completion' "$standalone_home/.bashrc")" -eq 1 ] \
  && [ "$(grep -cF 'Subyard CLI completion' "$standalone_home/.zshrc")" -eq 1 ] \
  && [ "$(grep -cF 'Subyard CLI interactive PATH' "$standalone_home/.zshrc")" -eq 1 ] \
  && [ "$(grep -cF 'Subyard CLI login PATH' "$standalone_home/.zprofile")" -eq 1 ] \
  && [ "$(grep -cF 'Subyard CLI login completion' "$standalone_home/.profile")" -eq 1 ] \
  || fail 'standalone shell integration is not idempotent'
for completion_shell in bash zsh; do
  zsh -f "$ROOT/tests/helpers/shell-completion-interaction.zsh" "$completion_shell" \
    "$standalone_home/.subyard/runtime/current/completions/yard.$completion_shell" \
    "$standalone_home/.subyard/runtime/current" "$standalone_home" \
    || fail "installed $completion_shell Tab selection or Enter execution failed"
done

# Selecting Zsh still prepares Bash, including an existing Bash login file. Startup PATH must
# work independently of whether the installation process already had the bin directory on PATH.
zsh_home="$TMP/zsh-home"
zsh_startup="$zsh_home/.config/zsh"
install -d -m 0711 "$zsh_home"
install -d -m 0750 "$zsh_startup"
directory_sentinel "$zsh_home"
directory_sentinel "$zsh_startup"
zsh_home_before="$(directory_identity "$zsh_home")"
zsh_startup_before="$(directory_identity "$zsh_startup")"
printf '%s\n' '# Existing operator login settings' > "$zsh_home/.bash_profile"
HOME="$zsh_home" SUBYARD_HOME="$zsh_home/.subyard" \
  SUBYARD_CONFIG_HOME="$zsh_home/.config/subyard" YARD_BIN_DIR="$zsh_home/.local/bin" \
  SHELL=/bin/zsh ZDOTDIR="$zsh_startup" YARD_SHELL_RC='' YARD_LOGIN_RC='' \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
  PATH="$zsh_home/.local/bin:$PATH" "$release/subyard-install.sh" --yes >/dev/null
[ "$(directory_identity "$zsh_home")" = "$zsh_home_before" ] \
  && [ "$(directory_identity "$zsh_startup")" = "$zsh_startup_before" ] \
  || fail 'bootstrap changed existing ZDOTDIR or alternate shell parent identity'
HOME="$zsh_home" ZDOTDIR="$zsh_startup" PATH=/usr/bin:/bin SHELL=/bin/zsh \
  zsh -lic 'command -v yard >/dev/null && (( $+functions[_yard] ))' >/dev/null 2>&1 \
  || fail 'standalone installer did not honor ZDOTDIR for login Zsh'
HOME="$zsh_home" PATH=/usr/bin:/bin SHELL=/bin/bash bash -lic \
  'command -v yard >/dev/null && complete -p yard >/dev/null' >/dev/null 2>&1 \
  || fail 'Zsh installation did not prepare login Bash'
grep -Fxq '# Existing operator login settings' "$zsh_home/.bash_profile" \
  || fail 'standalone installer replaced existing login settings'

override_home="$TMP/override-home"
override_rc="$override_home/interactive/custom.rc"
override_login="$override_home/login/custom.profile"
install -d -m 0700 "$override_home" "$override_home/.local/bin" "$override_home/login"
install -d -m 0711 "$override_home/interactive"
directory_sentinel "$override_home/.local/bin"
directory_sentinel "$override_home/login"
directory_sentinel "$override_home/interactive"
printf '# Existing operator interactive settings\n' > "$override_rc"
printf '# Existing operator login settings\n' > "$override_login"
chmod 0640 "$override_rc"
chmod 0600 "$override_login"
override_bin_before="$(directory_identity "$override_home/.local/bin")"
override_login_before="$(directory_identity "$override_home/login")"
override_rc_before="$(directory_identity "$override_home/interactive")"
override_rc_mode_before="$(stat -c '%a:%u:%g' "$override_rc")"
override_login_mode_before="$(stat -c '%a:%u:%g' "$override_login")"
# shellcheck disable=SC2034 # repeat proves metadata and startup bytes remain fixed.
for override_repeat in 1 2; do
HOME="$override_home" SUBYARD_HOME="$override_home/.subyard" \
  SUBYARD_CONFIG_HOME="$override_home/.config/subyard" YARD_BIN_DIR="$override_home/.local/bin" \
  SHELL=/bin/zsh ZDOTDIR="$override_home" \
  YARD_SHELL_RC="$override_rc" YARD_LOGIN_RC="$override_login" \
  YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
  "$release/subyard-install.sh" --yes >/dev/null
  [ "$(directory_identity "$override_home/.local/bin")" = "$override_bin_before" ] \
    && [ "$(directory_identity "$override_home/login")" = "$override_login_before" ] \
    && [ "$(directory_identity "$override_home/interactive")" = "$override_rc_before" ] \
    && [ "$(stat -c '%a:%u:%g' "$override_rc")" = "$override_rc_mode_before" ] \
    && [ "$(stat -c '%a:%u:%g' "$override_login")" = "$override_login_mode_before" ] \
    && grep -Fxq '# Existing operator interactive settings' "$override_rc" \
    && grep -Fxq '# Existing operator login settings' "$override_login" \
    || fail 'bootstrap changed existing explicit shell parents or binary directory identity'
  if [ "$override_repeat" = 1 ]; then
    override_startup_before="$(sha256sum "$override_rc" "$override_login")"
  else
    [ "$(sha256sum "$override_rc" "$override_login")" = "$override_startup_before" ] \
      || fail 'bootstrap retry rewrote explicit startup bytes'
  fi
done
[ -s "$override_rc" ] && [ -s "$override_login" ] \
  && [ ! -e "$override_home/.bashrc" ] && [ ! -e "$override_home/.zshrc" ] \
  && [ ! -e "$override_home/.profile" ] && [ ! -e "$override_home/.zprofile" ] \
  || fail 'explicit startup overrides unexpectedly changed default shell files'
HOME="$override_home" ZDOTDIR="$override_home" SHELL=/bin/zsh \
  zsh -fic 'source "$1"; (( $+functions[_yard] ))' zsh "$override_rc" \
  >/dev/null 2>&1 || fail 'neutral startup override filename selected the wrong shell completion'

# Missing startup parents are private even under a permissive umask; creating
# them must leave their existing shared ancestor unchanged.
# Use a fresh installation: the override fixture already owns a different bin directory.
absent_parent="$TMP/absent-shell-parents"
install -d -m 0750 "$absent_parent"
directory_sentinel "$absent_parent"
absent_before="$(directory_identity "$absent_parent")"
(
  umask 0000
  HOME="$absent_parent/home" SUBYARD_HOME="$absent_parent/home/.subyard" \
    SUBYARD_CONFIG_HOME="$absent_parent/home/.config/subyard" YARD_BIN_DIR="$absent_parent/bin"
  export HOME SUBYARD_HOME SUBYARD_CONFIG_HOME YARD_BIN_DIR
  SHELL=/bin/bash YARD_SHELL_RC="$absent_parent/interactive/nested/bashrc" \
    YARD_LOGIN_RC="$absent_parent/login/profile" \
    YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION=1.0.0-test \
    "$release/subyard-install.sh" --yes >/dev/null
)
[ "$(directory_identity "$absent_parent")" = "$absent_before" ] \
  && [ "$(stat -c '%a' "$absent_parent/bin")" = 755 ] \
  && [ "$(stat -c '%a' "$absent_parent/interactive")" = 700 ] \
  && [ "$(stat -c '%a' "$absent_parent/interactive/nested")" = 700 ] \
  && [ "$(stat -c '%a' "$absent_parent/login")" = 700 ] \
  || fail 'bootstrap did not create absent shell parents and binary directory with exact modes'

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
tar -xOf "$release/subyard-1.0.0-test-linux-arm64.tar.gz" \
  ./config/profiles/package-fixture/bin/worker > "$TMP/profile-arm64"
[ "$(od -An -tu2 -j18 -N2 "$TMP/profile-arm64" | tr -d ' ')" = 183 ] \
  || fail 'native profile executable was not cross-compiled for arm64'
tar -xOf "$release/subyard-1.0.0-test-linux-arm64.tar.gz" \
  ./config/agents/package-fixture/bin/worker > "$TMP/integration-arm64"
[ "$(od -An -tu2 -j18 -N2 "$TMP/integration-arm64" | tr -d ' ')" = 183 ] \
  || fail 'native integration executable was not cross-compiled for arm64'
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
HOME="$legacy_bridge_home" SUBYARD_HOME="$legacy_bridge_home/.subyard" \
  SUBYARD_CONFIG_HOME="$legacy_bridge_home/.config/subyard" \
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
  "$release/subyard-install.sh" --yes > "$TMP/legacy-refusal.log" 2>&1 \
  && fail 'standalone bootstrap admitted a pre-v0.11.0 source'
grep -Fq 'v0.11.0' "$TMP/legacy-refusal.log" \
  && grep -Fq 'uninstall --full' "$TMP/legacy-refusal.log" \
  && [ "$(readlink "$legacy_bridge_runtime/current")" = "$legacy_bridge_initial" ] \
  || fail 'legacy refusal changed active state or omitted removal guidance'

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

publish_again() {
  "$ROOT/scripts/install-runtime-release.sh" \
    --runtime-root "$publish_only_root" --publish-only \
    --bundle "$bundle_two" --checksum "$bundle_two.sha256" \
    --manifest "$bundle_two.manifest.json" --provenance "$bundle_two.provenance.json"
}
assert_publication_cleanup() {
  [ -z "$(find "$publish_only_root/releases" -maxdepth 1 \
    \( -name '.candidate.*' -o -name '.actual-files.*' -o -name '.listed-files.*' \) -print)" ] \
    || fail 'runtime publication left temporary files'
}
[ "$(publish_again)" = "$published_release" ] || fail 'intact publication reuse failed'
assert_publication_cleanup
# Corrupting a nested file preserves names and the original manifest, exposing
# checksum failures hidden by conditional errexit in the reuse branch.
ln -s "$published_release" "$publish_only_root/current"
ln -s "$published_release" "$publish_only_root/previous"
damaged_file="$publish_only_root/$published_release/config/profiles/package-fixture/asset.txt"
printf '%s\n' 'damaged runtime fixture' >> "$damaged_file"
damaged_digest="$(sha256sum "$damaged_file")"
if publish_again >"$TMP/damaged-reuse.out" 2>"$TMP/damaged-reuse.err"; then
  fail 'publication reuse accepted a damaged nested file'
fi
grep -Fq 'published release identity conflicts' "$TMP/damaged-reuse.err" \
  && [ "$(readlink "$publish_only_root/current")" = "$published_release" ] \
  && [ "$(readlink "$publish_only_root/previous")" = "$published_release" ] \
  && [ "$(sha256sum "$damaged_file")" = "$damaged_digest" ] \
  || fail 'failed publication reuse changed retained bytes or stable links'
assert_publication_cleanup

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
jq -e '.schemaVersion == 2 and .domains.settings.epoch == 3 and
  .domains.settings.applied == ["canonicalize-test-vms-settings-v2", "localize-ssh-port-settings-v3"]' "$ledger" >/dev/null \
  && jq -e '.schemaVersion == 2 and .checkpoint == "complete"' "$journal" >/dev/null \
  || fail 'v2 transition did not record a completed per-domain fixed point'
[ ! -e "$SUBYARD_CONFIG_HOME/migrations/state.json" ] \
  || fail 'production release transition wrote the superseded global layout'

# The canonical candidate already exists. Enrollment belongs to its activation,
# independently of whether this caller has a contemporary publisher helper.
rm -f -- "$runtime_root/installation.json"
same_output="$(YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$TMP/cache" \
  installed_update --runtime-root "$runtime_root" --version 1.1.0-test)"
[ "$(readlink "$runtime_root/current")" = "$candidate_target" ] \
  && [ "$(readlink "$runtime_root/previous")" = "$initial_target" ] \
  || fail 'same-version transition changed the retained release pair'
[ -f "$runtime_root/installation.json" ] \
  && jq -e --arg root "$runtime_root" '.schema == 1 and .runtimeRoot == $root and .binDir == "" and (.shellFiles|length)==0' "$runtime_root/installation.json" >/dev/null \
  || fail 'same-version activation omitted scoped live runtime enrollment'
installation_identity="$(stat -c '%d:%i:%Y:%Z' "$runtime_root/installation.json")"
installed_update --runtime-root "$runtime_root" --version 1.1.0-test >/dev/null
[ "$(stat -c '%d:%i:%Y:%Z' "$runtime_root/installation.json")" = "$installation_identity" ] \
  || fail 'matching activation rewrote installation inventory'
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
# The authentic-Go/Rust-candidate scenario is an explicit experimental gate.
if [ -n "${SUBYARD_RUST_PREDECESSOR_ASSETS:-}" ]; then
recovery_previous_installer="$recovery_release/subyard-install-runtime-release-0.9.1.sh"
# Historical source binaries remain exact published Go owners. The candidate is
# current Rust tooling with the exact historical two-step registry. Its current
# declaration and sealed helper remain bound to that newly built candidate.
. "$ROOT/tests/helpers/release-candidate.sh"
for historical_version in 0.9.1 0.11.0 0.11.1; do
  release_candidate_historical "$ROOT" "$SUBYARD_RUST_PREDECESSOR_ASSETS" \
    "$historical_version" "$recovery_release"
done
(
cp "$ROOT/config/release-transition.json" "$TMP/release-transition.current.json"
trap 'cp "$TMP/release-transition.current.json" "$ROOT/config/release-transition.json"' EXIT
tar -xOzf "$recovery_source_bundle" ./config/release-transition.json > "$ROOT/config/release-transition.json"
"$ROOT/dev/package-engine.sh" --experimental-release-owner --output-dir "$recovery_release" --version 0.11.2 >/dev/null
)
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
HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" YARD_RELEASE_CACHE="$recovery_cache" \
  bash "$recovery_previous_installer" \
  --runtime-root "$recovery_runtime" \
  --bundle "$recovery_previous_bundle" --checksum "$recovery_previous_bundle.sha256" \
  --manifest "$recovery_previous_bundle.manifest.json" \
  --provenance "$recovery_previous_bundle.provenance.json" >/dev/null
recovery_source_path="$(HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" YARD_RELEASE_CACHE="$recovery_cache" \
  "$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$recovery_runtime" --publish-only \
  --bundle "$recovery_source_bundle" --checksum "$recovery_source_bundle.sha256" \
  --manifest "$recovery_source_bundle.manifest.json" \
  --provenance "$recovery_source_bundle.provenance.json")"
recovery_source="${recovery_source_path#releases/}"
recovery_source_transaction="$(HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" YARD_RELEASE_CACHE="$recovery_cache" \
  release_candidate_seed_historical "$ROOT" "$recovery_runtime" "$recovery_config" "$recovery_source" recovery-yard)"
else
# Normal Go-owner compatibility fixtures preserve the existing public transition path.
(
cp "$ROOT/config/release-transition.json" "$TMP/release-transition.current.json"
trap 'cp "$TMP/release-transition.current.json" "$ROOT/config/release-transition.json"' EXIT
jq '.currentEpochs["power-metadata"] = 1 | .currentEpochs.settings = 2 |
  .migrations |= map(select(.kind != "owner-publication-state-v1" and .kind != "local-ssh-port-settings-v2-to-v3"))' \
  "$TMP/release-transition.current.json" > "$TMP/release-transition.historical.json"
cp "$TMP/release-transition.historical.json" "$ROOT/config/release-transition.json"
"$ROOT/dev/package-engine.sh" --output-dir "$recovery_release" --version 0.9.1 >/dev/null
"$ROOT/dev/package-engine.sh" --output-dir "$recovery_release" --version 0.11.0 >/dev/null
"$ROOT/dev/package-engine.sh" --output-dir "$recovery_release" --version 0.11.1 >/dev/null
"$ROOT/dev/package-engine.sh" --output-dir "$recovery_release" --version 0.11.2 >/dev/null
)
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
HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" YARD_RELEASE_CACHE="$recovery_cache" \
  "$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$recovery_runtime" \
  --bundle "$recovery_previous_bundle" --checksum "$recovery_previous_bundle.sha256" \
  --manifest "$recovery_previous_bundle.manifest.json" \
  --provenance "$recovery_previous_bundle.provenance.json" >/dev/null
recovery_previous_path="$(readlink "$recovery_runtime/current")"
recovery_previous="${recovery_previous_path#releases/}"
recovery_source_path="$(HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" YARD_RELEASE_CACHE="$recovery_cache" \
  "$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$recovery_runtime" --publish-only \
  --bundle "$recovery_source_bundle" --checksum "$recovery_source_bundle.sha256" \
  --manifest "$recovery_source_bundle.manifest.json" \
  --provenance "$recovery_source_bundle.provenance.json")"
recovery_source="${recovery_source_path#releases/}"
recovery_source_transaction="$("$recovery_fixture" seed \
  "$recovery_runtime" "$recovery_config" "$recovery_previous" "$recovery_source")"
fi
recovery_source_journal="$recovery_config/release-transition/v2/journal.json"
recovery_ledger="$recovery_config/release-transition/v2/ledger.json"
jq -e --arg transaction "$recovery_source_transaction" '.checkpoint == "reconciling" and .transaction == $transaction and
  [.steps[].migration] == ["canonicalize-test-vms-settings-v2", "canonicalize-test-yard-owner-v2"] and
  all(.steps[]; .checkpoint == "verified")' \
  "$recovery_source_journal" >/dev/null \
  || fail 'public fixture did not produce the exact v0.11.1 post-activation journal'
jq -e '.domains["power-metadata"] == {"epoch": 1, "applied": []}' \
  "$recovery_ledger" >/dev/null \
  || fail 'historical recovery fixture advanced the publication reader boundary'
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
if [ -n "${SUBYARD_RUST_PREDECESSOR_ASSETS:-}" ]; then
source_version_home="$TMP/v0110-source-home"
source_version_runtime="$source_version_home/runtime"
source_version_config="$source_version_home/config"
install -d -m 0700 "$source_version_home" "$source_version_config/yards/recovery-yard"
cp "$recovery_config/yards/recovery-yard/config.env" \
  "$source_version_config/yards/recovery-yard/config.env"
HOME="$source_version_home" SUBYARD_HOME="$source_version_home/data" \
  SUBYARD_CONFIG_HOME="$source_version_config" YARD_RELEASE_CACHE="$source_version_home/cache" \
  bash "$recovery_previous_installer" \
  --runtime-root "$source_version_runtime" \
  --bundle "$recovery_previous_bundle" --checksum "$recovery_previous_bundle.sha256" \
  --manifest "$recovery_previous_bundle.manifest.json" \
  --provenance "$recovery_previous_bundle.provenance.json" >/dev/null
source_version_source_path="$(HOME="$source_version_home" SUBYARD_HOME="$source_version_home/data" \
  SUBYARD_CONFIG_HOME="$source_version_config" YARD_RELEASE_CACHE="$source_version_home/cache" \
  "$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$source_version_runtime" --publish-only \
  --bundle "$recovery_older_source_bundle" --checksum "$recovery_older_source_bundle.sha256" \
  --manifest "$recovery_older_source_bundle.manifest.json" \
  --provenance "$recovery_older_source_bundle.provenance.json")"
source_version_source="${source_version_source_path#releases/}"
HOME="$source_version_home" SUBYARD_HOME="$source_version_home/data" \
  SUBYARD_CONFIG_HOME="$source_version_config" YARD_RELEASE_CACHE="$source_version_home/cache" \
  release_candidate_seed_historical "$ROOT" "$source_version_runtime" "$source_version_config" "$source_version_source" recovery-yard >/dev/null
else
source_version_home="$TMP/v0110-source-home"
source_version_runtime="$source_version_home/runtime"
source_version_config="$source_version_home/config"
install -d -m 0700 "$source_version_home" "$source_version_config/yards/recovery-yard"
cp "$recovery_config/yards/recovery-yard/config.env" \
  "$source_version_config/yards/recovery-yard/config.env"
HOME="$source_version_home" SUBYARD_HOME="$source_version_home/data" \
  SUBYARD_CONFIG_HOME="$source_version_config" YARD_RELEASE_CACHE="$source_version_home/cache" \
  "$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$source_version_runtime" \
  --bundle "$recovery_previous_bundle" --checksum "$recovery_previous_bundle.sha256" \
  --manifest "$recovery_previous_bundle.manifest.json" \
  --provenance "$recovery_previous_bundle.provenance.json" >/dev/null
source_version_previous_path="$(readlink "$source_version_runtime/current")"
source_version_previous="${source_version_previous_path#releases/}"
source_version_source_path="$(HOME="$source_version_home" SUBYARD_HOME="$source_version_home/data" \
  SUBYARD_CONFIG_HOME="$source_version_config" YARD_RELEASE_CACHE="$source_version_home/cache" \
  "$ROOT/scripts/install-runtime-release.sh" \
  --runtime-root "$source_version_runtime" --publish-only \
  --bundle "$recovery_older_source_bundle" --checksum "$recovery_older_source_bundle.sha256" \
  --manifest "$recovery_older_source_bundle.manifest.json" \
  --provenance "$recovery_older_source_bundle.provenance.json")"
source_version_source="${source_version_source_path#releases/}"
"$recovery_fixture" seed \
  "$source_version_runtime" "$source_version_config" \
  "$source_version_previous" "$source_version_source" >/dev/null
fi
source_version_baseline="$TMP/v0110-source-baseline"
install -d "$source_version_baseline"
cp -a "$source_version_runtime" "$source_version_baseline/runtime"
cp -a "$source_version_config" "$source_version_baseline/config"

# Inspect the already-published candidate directly before the standalone
# bootstrap mutates links or transition state. This is the exact Plan the
# terminal replacement journal and immutable archive must retain.
# Stage through bootstrap so exact ownership includes the verified cache inputs
# before preview. Offline continuation below preserves those native identities.
recovery_candidate_path="$(HOME="$recovery_home" SUBYARD_HOME="$recovery_home/data" \
  SUBYARD_CONFIG_HOME="$recovery_config" YARD_RELEASE_CACHE="$recovery_cache" \
  YARD_RELEASE_BASE_URL="file://$recovery_release" \
  "$recovery_release/subyard-install.sh" --runtime-root "$recovery_runtime" \
  --version 0.11.2 --publish-only --yes | sed -n 's/^staged release=\([^ ]*\) candidate=.*/\1/p')"
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
  "$recovery_release/subyard-install.sh" --offline --yes >/dev/null 9>"$trace_output"
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
  && jq -e --arg plan "$recovery_plan" --arg transaction "$recovery_source_transaction" \
    '.journal.transaction == $transaction and
    .replacement.reason == "post-activation-scope-v0.11.1" and
    .authorizationPlan == $plan' "$recovery_archive" >/dev/null \
  && [ -d "$recovery_config/release-transition/v2/transactions/$recovery_source_transaction/evidence" ] \
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
  # This is a new disposable installation, not a reuse of the original
  # installation's absolute ownership record. Publish through its own context
  # before capturing the negative case's protected state.
  rm -f -- "$negative_runtime/installation.json"
  HOME="$negative_root/home" SUBYARD_HOME="$negative_root/data" \
    SUBYARD_CONFIG_HOME="$negative_config" YARD_RELEASE_CACHE="$negative_root/cache" \
    "$ROOT/scripts/install-runtime-release.sh" \
    --runtime-root "$negative_runtime" --publish-only \
    --bundle "$recovery_previous_bundle" --checksum "$recovery_previous_bundle.sha256" \
    --manifest "$recovery_previous_bundle.manifest.json" \
    --provenance "$recovery_previous_bundle.provenance.json" >/dev/null
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
      process_guard_candidate="$(HOME="$negative_root/home" SUBYARD_HOME="$negative_root/data" \
        SUBYARD_CONFIG_HOME="$negative_config" YARD_RELEASE_CACHE="$negative_root/cache" \
        "$ROOT/scripts/install-runtime-release.sh" \
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
recovery_baseline_after="$TMP/v0111-recovery-baseline-after.sha256"
snapshot_recovery_state "$recovery_baseline/runtime" "$recovery_baseline/config" \
  > "$recovery_baseline_after"
if ! cmp -s "$recovery_baseline_before" "$recovery_baseline_after"; then
  # These snapshots contain link targets and file fingerprints, never payloads.
  diff -u "$recovery_baseline_before" "$recovery_baseline_after" | head -n 60 >&2 || true
  fail 'v0.11.1 recovery negative matrix changed its shared baseline'
fi
snapshot_recovery_state "$source_version_baseline/runtime" "$source_version_baseline/config" \
  | cmp -s "$source_version_baseline_before" - \
  || fail 'v0.11.1 source-version negative case changed its shared baseline'

printf 'ok: release publication and resumable v2 activation ingress are verified\n'

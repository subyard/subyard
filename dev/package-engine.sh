#!/usr/bin/env bash
# Build versioned Linux release artifacts.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
OUTPUT_DIR="$REPO/.build/release"
VERSION="${YARD_BUILD_VERSION:-0.1.0-dev}"
TARGET_ARCH="$(go env GOARCH 2>/dev/null || true)"
RUNTIME_DIR=""
PREBUILT_ENGINE=""
EXPERIMENTAL_RELEASE_OWNER=0
MIGRATION_REGISTRY="$REPO/config/migrations.json"
RELEASE_TRANSITION_REGISTRY="$REPO/config/release-transition.json"
RUNTIME_INSTALLER="$REPO/scripts/install-runtime-release.sh"

while [ $# -gt 0 ]; do
  case "$1" in
    --experimental-release-owner) EXPERIMENTAL_RELEASE_OWNER=1; shift ;;
    --runtime-dir) [ $# -ge 2 ] || exit 2; RUNTIME_DIR="$2"; shift 2 ;;
    --engine) [ $# -ge 2 ] || exit 2; PREBUILT_ENGINE="$2"; shift 2 ;;
    --output-dir) [ $# -ge 2 ] || { printf 'package-engine: --output-dir needs a path\n' >&2; exit 2; }; OUTPUT_DIR="$2"; shift 2 ;;
    --version) [ $# -ge 2 ] || { printf 'package-engine: --version needs a value\n' >&2; exit 2; }; VERSION="$2"; shift 2 ;;
    --arch) [ $# -ge 2 ] || { printf 'package-engine: --arch needs amd64 or arm64\n' >&2; exit 2; }; TARGET_ARCH="$2"; shift 2 ;;
    --migration-registry) [ $# -ge 2 ] || { printf 'package-engine: --migration-registry needs a path\n' >&2; exit 2; }; MIGRATION_REGISTRY="$2"; shift 2 ;;
    --runtime-installer) [ $# -ge 2 ] || { printf 'package-engine: --runtime-installer needs a path\n' >&2; exit 2; }; RUNTIME_INSTALLER="$2"; shift 2 ;;
    *) printf 'package-engine: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

case "$VERSION" in ''|*[!A-Za-z0-9._+-]*) printf 'package-engine: unsafe version: %s\n' "$VERSION" >&2; exit 2 ;; esac
command -v go >/dev/null 2>&1 || { printf 'package-engine: Go is required\n' >&2; exit 2; }
command -v rg >/dev/null 2>&1 || { printf 'package-engine: ripgrep is required\n' >&2; exit 2; }
command -v jq >/dev/null 2>&1 || { printf 'package-engine: jq is required\n' >&2; exit 2; }
command -v sha256sum >/dev/null 2>&1 || { printf 'package-engine: sha256sum is required\n' >&2; exit 2; }
if [[ "${GOFLAGS:-}" == *-tags* ]]; then
  printf 'package-engine: normal runtime builds do not accept Go build tags\n' >&2
  exit 2
fi
case "$TARGET_ARCH" in amd64 | arm64) ;; *) printf 'package-engine: unsupported architecture: %s\n' "$TARGET_ARCH" >&2; exit 2 ;; esac
for package_input in "$MIGRATION_REGISTRY" "$RELEASE_TRANSITION_REGISTRY" "$RUNTIME_INSTALLER"; do
  [ -f "$package_input" ] && [ ! -L "$package_input" ] \
    || { printf 'package-engine: package override must be a regular non-symlink file: %s\n' "$package_input" >&2; exit 2; }
done
read -r migration_schema minimum_layout current_layout < <(
  jq -er '
    select(.schemaVersion == 1 and
      (.minimumLayout | type == "number" and . >= 1 and floor == .) and
      (.currentLayout | type == "number" and floor == .)) |
    select(.currentLayout >= .minimumLayout) |
    [.schemaVersion, .minimumLayout, .currentLayout] | @tsv
  ' "$MIGRATION_REGISTRY"
) || { printf 'package-engine: invalid migration registry\n' >&2; exit 2; }

goos=linux; goarch="$TARGET_ARCH"
if [ -n "$RUNTIME_DIR" ]; then
  [ -n "$PREBUILT_ENGINE" ] && [ -f "$PREBUILT_ENGINE" ] && [ ! -L "$PREBUILT_ENGINE" ] && [ -x "$PREBUILT_ENGINE" ] || {
    printf 'package-engine: runtime staging requires its exact prebuilt engine\n' >&2; exit 2;
  }
  engine_source="$(realpath "$PREBUILT_ENGINE")"
  engine_identity="$(stat -c '%d:%i:%f:%u:%g:%h' "$engine_source")"
  exec {stage_engine_fd}<"$engine_source"
  artifact="/proc/self/fd/$stage_engine_fd"
  [ "$(stat -Lc '%d:%i:%f:%u:%g:%h' "$artifact")" = "$engine_identity" ] || exit 2
  engine_before="$(sha256sum "$artifact" | cut -d' ' -f1)"
  [ "$(bash -c 'exec -a yard-engine "$1" --version' stage-version "$artifact")" = "yard-engine $VERSION" ] || { printf 'package-engine: prebuilt engine version differs\n' >&2; exit 2; }
  build_info="$(go version -m "$artifact")"
  if grep -Eq -- '(-tags=|GOFLAGS=)' <<< "$build_info"; then
    printf 'package-engine: development tooling requires an untagged normal engine\n' >&2; exit 2
  fi
  machine="$(readelf -h "$artifact" | sed -n 's/.*Machine: *//p')"
  case "$goarch:$machine" in amd64:*X86-64*|arm64:AArch64) ;; *) printf 'package-engine: prebuilt engine architecture differs\n' >&2; exit 2 ;; esac
  install -d -m 0755 "$RUNTIME_DIR/releases"
  RUNTIME_DIR="$(cd "$RUNTIME_DIR" && pwd)"
  OUTPUT_DIR="$RUNTIME_DIR"
else
  [ -z "$PREBUILT_ENGINE" ] || { printf 'package-engine: --engine requires --runtime-dir\n' >&2; exit 2; }
install -d "$OUTPUT_DIR"
install -m 0755 "$REPO/dev/bootstrap-runtime.sh" "$OUTPUT_DIR/subyard-install.sh"
install -m 0755 "$RUNTIME_INSTALLER" \
  "$OUTPUT_DIR/subyard-install-runtime-release.sh"
( cd "$OUTPUT_DIR" && sha256sum subyard-install-runtime-release.sh \
    > subyard-install-runtime-release.sh.sha256 )
artifact="$OUTPUT_DIR/yard-$VERSION-$goos-$goarch"
GOOS="$goos" GOARCH="$goarch" YARD_BUILD_VERSION="$VERSION" \
  "$SCRIPT_DIR/build-engine.sh" --force --output "$artifact"
(
  cd "$OUTPUT_DIR"
  sha256sum "$(basename "$artifact")" > "$(basename "$artifact").sha256"
)
printf '{"schemaVersion":1,"version":"%s","os":"%s","arch":"%s","rpc":{"min":1,"max":1},"projectStateSchema":1,"credentialSchema":1,"migrationSchema":%s,"minimumConfigLayout":%s,"configLayout":%s}\n' \
  "$VERSION" "$goos" "$goarch" "$migration_schema" "$minimum_layout" "$current_layout" \
  > "$artifact.manifest.json"
artifact_hash="$(cut -d' ' -f1 "$artifact.sha256")"
revision=unknown
if [ -e "$REPO/.git" ] \
  && candidate_revision="$(git -C "$REPO" rev-parse --verify HEAD 2>/dev/null)" \
  && [[ "$candidate_revision" =~ ^[0-9a-f]{40,64}$ ]]; then
  revision="$candidate_revision"
fi
generated="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Keep the v0.1 sourceRepository wire value so installed v0.1 runtimes accept the handoff release.
printf '{"schemaVersion":1,"artifact":"%s","sha256":"%s","version":"%s","sourceRepository":"github.com/Dmitry-Borodin/Subyard","canonicalRepository":"github.com/Subyard/Subyard","sourceRevision":"%s","generatedAt":"%s"}\n' \
  "$(basename "$artifact")" "$artifact_hash" "$VERSION" "$revision" "$generated" \
  > "$artifact.provenance.json"
chmod 0644 "$artifact.sha256" "$artifact.manifest.json" "$artifact.provenance.json"

fi

# Publish a self-contained runtime. The installed launcher resolves this directory as its
# repository root, so production never reads scripts/config from a source checkout.
bundle="$OUTPUT_DIR/subyard-$VERSION-$goos-$goarch.tar.gz"
bundle_stage="$(mktemp -d "$OUTPUT_DIR/.runtime-bundle.XXXXXX")"
cleanup_bundle() { rm -rf -- "$bundle_stage"; }
trap cleanup_bundle EXIT
install -d "$bundle_stage/bin"
install -m 0755 "$artifact" "$bundle_stage/bin/yard-engine"
install -m 0755 "$REPO/bin/yard" "$bundle_stage/bin/yard"
if [ "$EXPERIMENTAL_RELEASE_OWNER" = 1 ]; then
"$SCRIPT_DIR/build-release-owner.sh" --arch "$goarch" \
  --output "$bundle_stage/bin/subyard-release-owner"
install -d "$bundle_stage/config"
mv "$bundle_stage/bin/subyard-release-owner.build.json" "$bundle_stage/config/release-owner-build.json"
# Build locks are development coordination, never runtime assets.
rm -f -- "$bundle_stage/bin/.release-owner-build.lock"
fi
runtime_list="$bundle_stage/.runtime-inputs"
runtime_extras=(
  internal/adapters/configmaterial/tomli_w.py
  internal/adapters/configmaterial/TOMLI_W_LICENSE
  scripts/e2e-lab/base.sh
  scripts/lib/owner-address.sh
  scripts/lib/preview-proxy.sh
  scripts/prepare-preview-route.sh
  scripts/lib/engine-context.sh
  scripts/install-ssh-relay.sh
  scripts/install-test-vms-host-sink.sh
  scripts/reconcile-integrations.sh
  scripts/profile-services.sh
  config/agents/codex/provision.sh
  config/preview/subyard-preview
  config/systemd/subyard-test-vms-host-sink.service.in
  config/systemd/subyard-test-vms-host-sink.timer.in
)
{
  bash "$REPO/tests/helpers/source-files.sh" \
    | while IFS= read -r -d '' relative; do
        case "$relative" in scripts/*|config/*|completions/*) printf '%s\0' "$relative" ;; esac
      done
  for relative in "${runtime_extras[@]}"; do
    [ -f "$REPO/$relative" ] && printf '%s\0' "$relative"
  done
} | sort -zu > "$runtime_list"
while IFS= read -r -d '' relative; do
  [ -e "$REPO/$relative" ] || continue
  if [ "$EXPERIMENTAL_RELEASE_OWNER" = 0 ]; then
    case "$relative" in config/release-owner.json|config/release-owner.schema.json|config/release-owner-build.json) continue ;; esac
  fi
  case "$relative" in
    config/release-owner-build.json|config/profiles/*/tests/*|config/profiles/*/*.go|config/profiles/*/*.rs|config/profiles/*/Cargo.toml|config/profiles/*/Cargo.lock|config/profiles/*/go.mod|config/profiles/*/go.sum|config/agents/*/tests/*|config/agents/*/*.go|config/agents/*/*.rs|config/retired-integrations/*/tests/*) continue ;;
  esac
  case "$relative" in
    scripts/*|config/*|completions/*|internal/adapters/configmaterial/tomli_w.py|internal/adapters/configmaterial/TOMLI_W_LICENSE) ;;
    *) printf 'package-engine: runtime allowlist escaped: %s\n' "$relative" >&2; exit 1 ;;
  esac
  case "$relative" in *\\*|*$'\n'*|*$'\r'*|*$'\t'*) printf 'package-engine: unsafe runtime path\n' >&2; exit 1 ;; esac
  [ -f "$REPO/$relative" ] && [ ! -L "$REPO/$relative" ] \
    || { printf 'package-engine: runtime input must be a regular file: %s\n' "$relative" >&2; exit 1; }
  install -d "$bundle_stage/$(dirname "$relative")"
  # Public runtime assets need fixed modes regardless of the checkout's umask.
  mode=0644
  [ ! -x "$REPO/$relative" ] || mode=0755
  [ "$relative" != internal/adapters/configmaterial/tomli_w.py ] || mode=0644
  install -m "$mode" -- "$REPO/$relative" "$bundle_stage/$relative"
done < "$runtime_list"
rm -f -- "$runtime_list"
GOOS="$goos" GOARCH="$goarch" "$SCRIPT_DIR/build-profiles.sh" \
  --output-dir "$bundle_stage/config/profiles"
install -m 0755 "$RUNTIME_INSTALLER" "$bundle_stage/scripts/install-runtime-release.sh"
install -m 0644 "$MIGRATION_REGISTRY" "$bundle_stage/config/migrations.json"
install -m 0644 "$RELEASE_TRANSITION_REGISTRY" "$bundle_stage/config/release-transition.json"
for required in \
  internal/adapters/configmaterial/tomli_w.py \
  internal/adapters/configmaterial/TOMLI_W_LICENSE \
  scripts/e2e-lab/base.sh \
  scripts/install-runtime-release.sh \
  scripts/install-ssh-relay.sh \
  scripts/install-test-vms-host-sink.sh \
  scripts/reconcile-integrations.sh \
  scripts/prepare-preview-route.sh \
  config/systemd/subyard-test-vms-host-sink.service.in \
  config/systemd/subyard-test-vms-host-sink.timer.in \
  config/commands.registry \
  config/migrations.json \
  config/release-transition.json \
  config/preview/subyard-preview \
  config/preview/instructions.md \
  completions/yard.bash; do
  [ -f "$bundle_stage/$required" ] \
    || { printf 'package-engine: runtime allowlist omitted %s\n' "$required" >&2; exit 1; }
done
if [ "$EXPERIMENTAL_RELEASE_OWNER" = 1 ]; then
  for required in config/release-owner.json config/release-owner.schema.json config/release-owner-build.json bin/subyard-release-owner; do
    [ -f "$bundle_stage/$required" ] || { printf 'package-engine: experimental owner omitted %s\n' "$required" >&2; exit 1; }
  done
fi
(
  cd "$bundle_stage"
  find . -type f ! -path './runtime-files.sha256' -print0 | sort -z | xargs -0 sha256sum \
    > runtime-files.sha256
)
chmod 0644 "$bundle_stage/runtime-files.sha256"
# Native builders also create directories; normalize only the public staging tree.
find "$bundle_stage" -type d -exec chmod 0755 {} +
if [ -n "$RUNTIME_DIR" ]; then
  [ "$(stat -c '%d:%i:%f:%u:%g:%h' "$engine_source")" = "$engine_identity" ] \
    && [ "$(sha256sum "$engine_source" | cut -d' ' -f1)" = "$engine_before" ] \
    && [ "$(sha256sum "$artifact" | cut -d' ' -f1)" = "$engine_before" ] \
    && [ "$(sha256sum "$bundle_stage/bin/yard-engine" | cut -d' ' -f1)" = "$engine_before" ] || {
      printf 'package-engine: prebuilt engine changed during staging\n' >&2; exit 1;
    }
  digest="$(sha256sum "$bundle_stage/runtime-files.sha256" | cut -c1-12)"
  release="$VERSION-$digest"
  (
    flock 9
    if [ -e "$RUNTIME_DIR/releases/$release" ]; then
      diff -qr -- "$bundle_stage" "$RUNTIME_DIR/releases/$release" >/dev/null \
        && (cd "$RUNTIME_DIR/releases/$release" && sha256sum --check --status runtime-files.sha256) || {
          printf 'package-engine: existing immutable tooling stage differs\n' >&2; exit 1;
        }
    else
      mv -- "$bundle_stage" "$RUNTIME_DIR/releases/$release"
    fi
    link="$(mktemp -u "$RUNTIME_DIR/.current.XXXXXX")"
    ln -s "releases/$release" "$link"
    mv -Tf -- "$link" "$RUNTIME_DIR/current"
  ) 9>"$RUNTIME_DIR/.build.lock"
  cleanup_bundle
  trap - EXIT
  printf '%s\n' "$RUNTIME_DIR/current"
  exit 0
fi
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C "$bundle_stage" -cf - . \
  | gzip -n > "$bundle"
chmod 0644 "$bundle"
(
  cd "$OUTPUT_DIR"
  sha256sum "$(basename "$bundle")" > "$(basename "$bundle").sha256"
)
printf '{"schemaVersion":1,"kind":"runtime","version":"%s","os":"%s","arch":"%s","rpc":{"min":1,"max":1},"projectStateSchema":1,"credentialSchema":1,"migrationSchema":%s,"minimumConfigLayout":%s,"configLayout":%s}\n' \
  "$VERSION" "$goos" "$goarch" "$migration_schema" "$minimum_layout" "$current_layout" \
  > "$bundle.manifest.json"
bundle_hash="$(cut -d' ' -f1 "$bundle.sha256")"
printf '{"schemaVersion":1,"artifact":"%s","sha256":"%s","version":"%s","sourceRepository":"github.com/Dmitry-Borodin/Subyard","canonicalRepository":"github.com/Subyard/Subyard","sourceRevision":"%s","generatedAt":"%s"}\n' \
  "$(basename "$bundle")" "$bundle_hash" "$VERSION" "$revision" "$generated" \
  > "$bundle.provenance.json"
chmod 0644 "$bundle.sha256" "$bundle.manifest.json" "$bundle.provenance.json"
cleanup_bundle
trap - EXIT
printf '%s\n' "$artifact"

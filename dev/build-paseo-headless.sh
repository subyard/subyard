#!/usr/bin/env bash
# Build a deploy-only Paseo headless bundle on its native Linux architecture.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
PACKAGE_DIR="$REPO/config/agents/paseo"
OUTPUT_DIR="$REPO/.build/paseo"
TARGET_ARCH=''
PASEO_VERSION=0.2.1
PASEO_REVISION=36f38245cab51bbe0b43b6ac42fd41aa757064d9
NODE_VERSION=22.20.0
UPSTREAM_LOCK_SHA256=c55787df3ef7f119d7fed0c404e57e19f168ae8bb636f4a4861b1a0227976840

while [ $# -gt 0 ]; do
  case "$1" in
    --output-dir) [ $# -ge 2 ] || { printf 'build-paseo-headless: --output-dir needs a path\n' >&2; exit 2; }; OUTPUT_DIR="$2"; shift 2 ;;
    --arch) [ $# -ge 2 ] || { printf 'build-paseo-headless: --arch needs amd64 or arm64\n' >&2; exit 2; }; TARGET_ARCH="$2"; shift 2 ;;
    *) printf 'build-paseo-headless: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

case "$(uname -m)" in
  x86_64) native_arch=amd64; node_arch=x64; node_sha=00bbd05e306ea68b6e13e17360d0e2f680b493ef95f2fea1c4296ff7437530bc ;;
  aarch64|arm64) native_arch=arm64; node_arch=arm64; node_sha=06907b9c088ce62305bc1530e5c1ae1510245114645768f7750c349c5b6fe667 ;;
  *) printf 'build-paseo-headless: unsupported native architecture\n' >&2; exit 2 ;;
esac
: "${TARGET_ARCH:=$native_arch}"
[ "$TARGET_ARCH" = "$native_arch" ] \
  || { printf 'build-paseo-headless: native %s runner cannot build %s\n' "$native_arch" "$TARGET_ARCH" >&2; exit 2; }

for command in curl jq sha256sum tar xz readelf git; do
  command -v "$command" >/dev/null 2>&1 \
    || { printf 'build-paseo-headless: %s is required\n' "$command" >&2; exit 2; }
done
for input in \
  "$PACKAGE_DIR/bundle/package.json" "$PACKAGE_DIR/bundle/package-lock.json" \
  "$PACKAGE_DIR/config.json" "$PACKAGE_DIR/paseo.service.in" \
  "$PACKAGE_DIR/sync-projects.mjs"; do
  [ -f "$input" ] && [ ! -L "$input" ] \
    || { printf 'build-paseo-headless: missing regular input %s\n' "$input" >&2; exit 2; }
done

temporary="$(mktemp -d /tmp/subyard-paseo-build.XXXXXX)"
cleanup() { rm -rf -- "$temporary"; }
trap cleanup EXIT HUP INT TERM
npm_cache="$REPO/.build/paseo-npm-cache"
mkdir -p "$REPO/.build/paseo-diagnostics"
npm_diagnostics="$(mktemp -d "$REPO/.build/paseo-diagnostics/$TARGET_ARCH.XXXXXX")"
mkdir -p "$npm_cache" "$temporary/npm-home" "$temporary/npm-logs"
: >"$temporary/npm-user.npmrc"
: >"$temporary/npm-global.npmrc"

# The public bundle must not inherit developer credentials or private npm settings.
run_npm() {
  env -i HOME="$temporary/npm-home" PATH="$node_root/bin:$PATH" TMPDIR=/tmp \
    "$node_root/bin/node" "$node_root/lib/node_modules/npm/bin/npm-cli.js" \
    --userconfig="$temporary/npm-user.npmrc" --globalconfig="$temporary/npm-global.npmrc" \
    --registry=https://registry.npmjs.org --cache="$npm_cache" \
    --logs-dir="$temporary/npm-logs" "$@"
}

npm_ci() {
  local attempt status error_code console diagnostic log
  local -a logs
  for attempt in 1 2 3; do
    console="$temporary/npm-attempt-$attempt.log"
    diagnostic="$npm_diagnostics/attempt-$attempt.log"
    if run_npm ci --omit=dev --no-audit --no-fund \
      --fetch-retries=2 --fetch-timeout=300000 \
      --fetch-retry-mintimeout=10000 --fetch-retry-maxtimeout=60000 >"$console" 2>&1; then
      status=0
    else
      status=$?
    fi
    # Retain only structured records, never arbitrary script output or raw npm debug logs.
    printf 'attempt=%s exit_code=%s\n' "$attempt" "$status" >"$diagnostic"
    logs=("$console")
    for log in "$temporary/npm-logs/"*; do
      [ ! -f "$log" ] || logs+=("$log")
    done
    awk '
      /^npm (error|ERR!) (code|syscall) [A-Za-z0-9_]+$/ { print }
      /^[0-9]+ verbose (exit|code) -?[0-9]+$/ { print }
      {
        if (match($0, /https:\/\/registry[.]npmjs[.]org\/[A-Za-z0-9@%._~+\/-]+/)) {
          url = substr($0, RSTART, RLENGTH)
          if ($0 ~ /http fetch (GET|POST) [0-9][0-9][0-9] /) {
            record = $0
            sub(/^.*http fetch /, "", record)
            split(record, fields, " ")
            print "http", fields[1], fields[2], url
          } else {
            print "registry_url=" url
          }
        }
      }
    ' "${logs[@]}" >"$diagnostic.records"
    sort -u "$diagnostic.records" >>"$diagnostic"
    rm -f -- "$diagnostic.records"
    cat "$diagnostic" >&2
    printf 'build-paseo-headless: npm diagnostics: %s\n' "$diagnostic" >&2
    [ "$status" -ne 0 ] || return 0
    error_code="$(awk '/^npm (error|ERR!) code [A-Za-z0-9_]+$/ { code = $NF } END { print code }' "$console")"
    case "$error_code" in
      ETIMEDOUT|ESOCKETTIMEDOUT|ECONNRESET|ECONNREFUSED|EAI_AGAIN|ENETUNREACH|EHOSTUNREACH|EPIPE|E429|E500|E502|E503|E504) ;;
      *) return "$status" ;;
    esac
    [ "$attempt" -lt 3 ] || return "$status"
    printf 'build-paseo-headless: retrying npm ci after %s (attempt %s/3)\n' "$error_code" "$attempt" >&2
    sleep "$((attempt * 2))"
  done
}

node_archive="node-v$NODE_VERSION-linux-$node_arch.tar.xz"
curl --disable --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --retry 3 --retry-connrefused --retry-delay 2 --retry-max-time 600 \
  --connect-timeout 60 --max-time 300 \
  "https://nodejs.org/download/release/v$NODE_VERSION/$node_archive" \
  -o "$temporary/$node_archive"
[ "$(sha256sum "$temporary/$node_archive" | cut -d' ' -f1)" = "$node_sha" ] \
  || { printf 'build-paseo-headless: Node checksum mismatch\n' >&2; exit 1; }
tar -xJf "$temporary/$node_archive" -C "$temporary"
node_root="$temporary/node-v$NODE_VERSION-linux-$node_arch"

upstream_archive="$temporary/paseo-$PASEO_REVISION.tar.gz"
curl --disable --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --retry 3 --retry-connrefused --retry-delay 2 --retry-max-time 600 \
  --connect-timeout 60 --max-time 300 \
  "https://github.com/getpaseo/paseo/archive/$PASEO_REVISION.tar.gz" \
  -o "$upstream_archive"
mkdir "$temporary/upstream"
tar -xzf "$upstream_archive" -C "$temporary/upstream" --strip-components=1
[ "$(sha256sum "$temporary/upstream/package-lock.json" | cut -d' ' -f1)" = "$UPSTREAM_LOCK_SHA256" ] \
  || { printf 'build-paseo-headless: upstream lock checksum mismatch\n' >&2; exit 1; }
jq -e '
  .version == "0.2.1" and
  .dependencies["node-pty"] == "1.2.0-beta.11" and
  .dependencies["sherpa-onnx-node"] == "1.12.28" and
  .dependencies["@anthropic-ai/claude-agent-sdk"] == "^0.3.214"
' "$temporary/upstream/packages/server/package.json" >/dev/null \
  || { printf 'build-paseo-headless: upstream server manifest drift\n' >&2; exit 1; }
jq -e '.version == "0.2.1"' "$temporary/upstream/packages/cli/package.json" >/dev/null \
  || { printf 'build-paseo-headless: upstream CLI manifest drift\n' >&2; exit 1; }

app="$temporary/app"
mkdir "$app"
cp "$PACKAGE_DIR/bundle/package.json" "$PACKAGE_DIR/bundle/package-lock.json" "$app/"
(
  cd "$app"
  npm_ci
)

for package_spec in \
  '@getpaseo/cli:0.2.1' \
  '@getpaseo/server:0.2.1' \
  '@getpaseo/client:0.2.1' \
  '@getpaseo/protocol:0.2.1' \
  'node-pty:1.2.0-beta.11' \
  'sherpa-onnx-node:1.12.28'; do
  package_name="${package_spec%:*}"
  package_version="${package_spec##*:}"
  installed="$(jq -r .version "$app/node_modules/$package_name/package.json")"
  [ "$installed" = "$package_version" ] \
    || { printf 'build-paseo-headless: %s version drift: %s\n' "$package_name" "$installed" >&2; exit 1; }
done
sdk_package="$(find "$app/node_modules" -type f \
  -path '*/@anthropic-ai/claude-agent-sdk/package.json' -print -quit)"
[ -n "$sdk_package" ] \
  || { printf 'build-paseo-headless: Claude Agent SDK is missing\n' >&2; exit 1; }
[ "$(jq -r .version "$sdk_package")" = 0.3.214 ] \
  || { printf 'build-paseo-headless: Claude Agent SDK version drift\n' >&2; exit 1; }

node_pty_prebuilds="$app/node_modules/node-pty/prebuilds"
for prebuild in "$node_pty_prebuilds"/*; do
  [ -d "$prebuild" ] || continue
  [ "$(basename "$prebuild")" = "linux-$node_arch" ] || rm -rf -- "$prebuild"
done

expected_machine='Advanced Micro Devices X86-64'
[ "$TARGET_ARCH" != arm64 ] || expected_machine='AArch64'
native_module_count=0
while IFS= read -r native_module; do
  native_module_count=$((native_module_count + 1))
  machine="$(readelf -h "$native_module" 2>/dev/null \
    | awk -F: '/Machine:/{sub(/^[[:space:]]+/, "", $2); print $2}')" || true
  [ -n "$machine" ] \
    || { printf 'build-paseo-headless: native module is not ELF: %s\n' "$native_module" >&2; exit 1; }
  [ "$machine" = "$expected_machine" ] \
    || { printf 'build-paseo-headless: wrong ELF machine: %s\n' "$native_module" >&2; exit 1; }
done < <(find "$app/node_modules" -type f -name '*.node' -print | sort)
[ "$native_module_count" -gt 0 ] \
  || { printf 'build-paseo-headless: native dependency closure is empty\n' >&2; exit 1; }

(
  cd "$app"
  "$node_root/bin/node" -e 'import("node-pty").then((module) => { if (typeof module.spawn !== "function") process.exit(1); })'
  "$node_root/bin/node" -e 'import("sherpa-onnx-node").then((module) => { if (!module) process.exit(1); })'
  "$node_root/bin/node" -e '
    const { createRequire } = require("node:module");
    const { pathToFileURL } = require("node:url");
    const requireFromServer = createRequire(require.resolve("@getpaseo/server"));
    import(pathToFileURL(requireFromServer.resolve("@anthropic-ai/claude-agent-sdk")).href)
      .then((module) => { if (!module) process.exit(1); });
  '
  "$node_root/bin/node" "$app/node_modules/@getpaseo/cli/bin/paseo" --version \
    | grep -Fxq "$PASEO_VERSION"
  run_npm sbom --sbom-format spdx >"$temporary/sbom.raw.spdx.json"
)
jq --arg arch "$TARGET_ARCH" '
  .creationInfo.created = "1970-01-01T00:00:00Z" |
  .documentNamespace =
    ("https://github.com/Subyard/Subyard/sbom/paseo-headless-0.2.1-linux-" + $arch)
' "$temporary/sbom.raw.spdx.json" >"$temporary/sbom.spdx.json"

stage="$temporary/stage"
install -d "$stage/node/bin" "$stage/app/libexec" "$stage/package/bin"
install -m 0755 "$node_root/bin/node" "$stage/node/bin/node"
install -m 0644 "$node_root/LICENSE" "$stage/node/LICENSE"
cp -a "$app/package.json" "$app/package-lock.json" "$app/node_modules" "$stage/app/"
install -m 0755 "$PACKAGE_DIR/sync-projects.mjs" "$stage/app/libexec/paseo-sync-projects.mjs"
install -m 0644 "$PACKAGE_DIR/config.json" "$stage/package/config.json"
install -m 0644 "$PACKAGE_DIR/paseo.service.in" "$stage/package/paseo.service.in"
for helper in "$PACKAGE_DIR"/bin/*; do
  install -m 0755 "$helper" "$stage/package/bin/$(basename "$helper")"
done
install -m 0644 "$temporary/sbom.spdx.json" "$stage/sbom.spdx.json"

# npm's command shims and any package links are build conveniences. Runtime uses exact files.
find "$stage" -type l -delete
if find "$stage" -type f \( -name 'gcc' -o -name 'g++' -o -name 'make' -o -name 'patchelf' -o -name 'node-gyp' \) -print -quit | grep -q .; then
  printf 'build-paseo-headless: compiler or patch tool leaked into deploy bundle\n' >&2
  exit 1
fi

package_lock_sha="$(sha256sum "$stage/app/package-lock.json" | cut -d' ' -f1)"
subyard_revision="$(git -C "$REPO" rev-parse --verify HEAD 2>/dev/null || printf unknown)"
jq -n \
  --arg paseo "$PASEO_VERSION" --arg upstream "$PASEO_REVISION" \
  --arg node "$NODE_VERSION" --arg nodeSha "$node_sha" --arg arch "$TARGET_ARCH" \
  --arg lock "$package_lock_sha" --arg upstreamLock "$UPSTREAM_LOCK_SHA256" \
  --arg subyard "$subyard_revision" \
  '{
    schemaVersion: 1,
    kind: "paseo-headless",
    paseoVersion: $paseo,
    upstreamRevision: $upstream,
    upstreamLockSha256: $upstreamLock,
    subyardRevision: $subyard,
    nodeVersion: $node,
    nodeArchiveSha256: $nodeSha,
    packageLockSha256: $lock,
    os: "linux",
    arch: $arch,
    dependencies: {
      nodePty: "1.2.0-beta.11",
      sherpaOnnxNode: "1.12.28",
      claudeAgentSdk: "0.3.214"
    }
  }' >"$stage/manifest.json"

(
  cd "$stage"
  find . -type f ! -name files.sha256 -print0 | sort -z | xargs -0 sha256sum >files.sha256
)
install -d "$OUTPUT_DIR"
artifact="$OUTPUT_DIR/paseo-headless-$PASEO_VERSION-linux-$TARGET_ARCH.tar.gz"
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C "$stage" -cf - . \
  | gzip -n >"$artifact"
chmod 0644 "$artifact"
(cd "$OUTPUT_DIR" && sha256sum "$(basename "$artifact")" >"$(basename "$artifact").sha256")
printf '%s\n' "$artifact"

#!/usr/bin/env bash
# Build the locked, static release owner; installed runtimes need no compiler.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUTPUT="$REPO/.build/subyard-release-owner"
ARCH="${GOARCH:-$(uname -m)}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) [ "$#" -ge 2 ] || exit 2; OUTPUT="$2"; shift 2 ;;
    --arch) [ "$#" -ge 2 ] || exit 2; ARCH="$2"; shift 2 ;;
    *) printf 'build-release-owner: unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done
case "$ARCH" in
  amd64|x86_64) target=x86_64-unknown-linux-musl ;;
  arm64|aarch64) target=aarch64-unknown-linux-musl ;;
  *) printf 'build-release-owner: unsupported architecture: %s\n' "$ARCH" >&2; exit 2 ;;
esac
for tool in cargo rustc jq sha256sum readelf; do
  command -v "$tool" >/dev/null || { printf 'build-release-owner: %s is required\n' "$tool" >&2; exit 2; }
done
cd "$REPO"
export TMPDIR=/tmp
cargo_environment="$(python3 "$REPO/dev/cargo_cache.py" --target-dir "$REPO/.build/release-owner/target" --snapshot --run-target --manifest-path release-owner/Cargo.toml)"
eval "$cargo_environment"
mkdir -p "$(dirname "$OUTPUT")"
(
  flock 9
  compiler="$("$RUSTC" -vV)"
  cargo_version="$(cargo --version)"
  sysroot="$("$RUSTC" --print sysroot)"
  host="$(sed -n 's/^host: //p' <<< "$compiler")"
  linker="$sysroot/lib/rustlib/$host/bin/rust-lld"
  [ -x "$linker" ] || { printf 'build-release-owner: rust-lld is required\n' >&2; exit 2; }
  # Ignore caller feature/flag selection: fixture-cuts must never enter a shipped image.
  export RUSTFLAGS="-C linker=$linker -C target-feature=+crt-static --remap-path-prefix=$REPO=."
  unset CARGO_ENCODED_RUSTFLAGS
  source_inputs() {
    bash tests/helpers/source-files.sh \
      | while IFS= read -r -d '' relative; do
          case "$relative" in
            release-owner/Cargo.toml|release-owner/Cargo.lock|release-owner/build.rs|release-owner/src/*.rs|config/profiles/*.rs|rust-toolchain.toml|dev/build-release-owner.sh|dev/cargo_cache.py|dev/cargo-workspace-rustc.sh)
              [ -f "$relative" ] && [ ! -L "$relative" ] || {
                printf 'build-release-owner: source input must be a regular non-symlink file\n' >&2; exit 1;
              }
              printf '%s\0' "$relative" ;;
          esac
        done | sort -zu | xargs -0 sha256sum \
      | jq -Rn '[inputs | capture("^(?<sha256>[0-9a-f]{64})  (?<path>.*)$")]'
  }
  for required in release-owner/Cargo.toml release-owner/Cargo.lock release-owner/src/lib.rs \
    release-owner/src/bin/subyard-release-owner.rs rust-toolchain.toml; do
    [ -f "$required" ] && [ ! -L "$required" ] || {
      printf 'build-release-owner: required source input is unavailable\n' >&2; exit 2;
    }
  done
  tmp="$(mktemp "$(dirname "$OUTPUT")/.release-owner.XXXXXX")"
  trap 'rm -f -- "$tmp" "$tmp.inputs" "$tmp.after" "$tmp.json"' EXIT
  source_inputs > "$tmp.inputs"
  cargo build --locked --release --no-default-features --manifest-path release-owner/Cargo.toml \
    --target "$target" --bin subyard-release-owner
  source_inputs > "$tmp.after"
  cmp -s "$tmp.inputs" "$tmp.after" || {
    printf 'build-release-owner: source inputs changed during the build\n' >&2; exit 1;
  }
  image="$CARGO_TARGET_DIR/$target/release/subyard-release-owner"
  readelf -h "$image" >/dev/null
  program_headers="$(readelf -l "$image")"
  dynamic_entries="$(readelf -d "$image")"
  if grep -q INTERP <<< "$program_headers" || grep -q '(NEEDED)' <<< "$dynamic_entries"; then
    printf 'build-release-owner: owner must be a static ELF executable\n' >&2
    exit 1
  fi
  install -m 0755 "$image" "$tmp"
  jq -n --arg compiler "$compiler" --arg cargo "$cargo_version" --arg target "$target" \
    --arg imageSHA256 "$(sha256sum "$tmp" | cut -d' ' -f1)" \
    --arg cargoLockSHA256 "$(sha256sum release-owner/Cargo.lock | cut -d' ' -f1)" \
    --slurpfile inputs "$tmp.inputs" \
    '{schemaVersion:1,kind:"rust-release-owner-build",compiler:$compiler,cargo:$cargo,
      target:$target,profile:"release",features:[],locked:true,staticELF:true,
      codegen:["linker=rust-lld","target-feature=+crt-static","remap-path-prefix=repository=."],
      imageSHA256:$imageSHA256,cargoLockSHA256:$cargoLockSHA256,inputs:$inputs[0]}' > "$tmp.json"
  chmod 0644 "$tmp.json"
  mv -f -- "$tmp.json" "$OUTPUT.build.json"
  mv -f -- "$tmp" "$OUTPUT"
  rm -f -- "$tmp.inputs" "$tmp.after"
  trap - EXIT
) 9>"$(dirname "$OUTPUT")/.release-owner-build.lock"

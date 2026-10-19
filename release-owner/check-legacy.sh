#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export TMPDIR=/tmp
out="$PWD/.build/release-owner-legacy"
build="$PWD/.build/release-owner-recovery/target"
mkdir -p "$out"
if [[ -x .build/migrationhistory-pilot/toolchain/cargo/bin/cargo ]]; then
 export CARGO_HOME="$PWD/.build/migrationhistory-pilot/toolchain/cargo"
 export RUSTUP_HOME="$PWD/.build/migrationhistory-pilot/toolchain/rustup"
 export PATH="$CARGO_HOME/bin:$PATH"
fi
export SUBYARD_RUST_CODEC_HELPER="$out/yard"
export SUBYARD_RUST_CODEC_FAULT_PEER="$out/codec-faults"
export SUBYARD_RUST_SOURCE_PROBE="$build/debug/examples/source_probe"
export SUBYARD_RUST_LEGACY_CORPUS="$out/go-corpus.json"
export SUBYARD_RUST_OWNER_CORPUS="$out/owner-corpus.json"
bash -n release-owner/check-legacy.sh
go version
rustc --version
cargo fmt --manifest-path release-owner/Cargo.toml -- --check
go build -o "$SUBYARD_RUST_CODEC_HELPER" ./cmd/yard
go build -o "$SUBYARD_RUST_CODEC_FAULT_PEER" release-owner/tests/fixtures/codec_faults.go
cargo build --locked --manifest-path release-owner/Cargo.toml --target-dir "$build" --example source_probe
# The existing Go oracle remains the sole canonical/store policy source. Its
# capacity fixtures export without race instrumentation; new codec ownership
# and direct same-inode admission checks run with race separately.
go test -count=1 -v -run '^TestRustOwnerFoundationCorpus$' ./internal/releasetransition
go test -race -count=1 -v -run '^Test(LegacyCodecFrames|LegacyCodecProjectionBudget|RustOwnerLegacyCorpus|RustOwnerDirectSourceProof)$' ./internal/releasetransition
cargo test --locked --manifest-path release-owner/Cargo.toml --target-dir "$build" -- --nocapture
cargo clippy --locked --manifest-path release-owner/Cargo.toml --target-dir "$build" --all-targets -- -D warnings
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target-dir "$build" --target x86_64-unknown-linux-musl
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target-dir "$build" --target aarch64-unknown-linux-musl
git diff --check

#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export TMPDIR=/tmp
out="$PWD/.build/release-owner-legacy"
build="$PWD/.build/release-owner-recovery/target"
mkdir -p "$out"
# Keep downloads and intermediates shared; final artifacts belong to this check.
cargo_environment="$(python3 dev/cargo_cache.py --target-dir "${build:-$out/target}" --snapshot --run-target --manifest-path release-owner/Cargo.toml)"
eval "$cargo_environment"
export SUBYARD_RUST_CODEC_HELPER="$out/yard"
export SUBYARD_RUST_CODEC_FAULT_PEER="$out/codec-faults"
export SUBYARD_RUST_SOURCE_PROBE="$CARGO_TARGET_DIR/debug/examples/source_probe"
export SUBYARD_RUST_LEGACY_CORPUS="$out/go-corpus.json"
export SUBYARD_RUST_OWNER_CORPUS="$out/owner-corpus.json"
bash -n release-owner/check-legacy.sh
go version
"$RUSTC" --version
export SUBYARD_RUST_HISTORY_CORPUS="$out/history-corpus.json"
go test -race -count=1 -v ./internal/migrationhistory -run '^TestRustHistoryOracleCorpus$'
cargo test --locked --manifest-path release-owner/Cargo.toml --test history_oracle -- --ignored --nocapture
cargo fmt --manifest-path release-owner/Cargo.toml -- --check
go build -o "$SUBYARD_RUST_CODEC_HELPER" ./cmd/yard
go build -o "$SUBYARD_RUST_CODEC_FAULT_PEER" release-owner/tests/fixtures/codec_faults.go
cargo build --locked --manifest-path release-owner/Cargo.toml --example source_probe
# The existing Go oracle remains the sole canonical/store policy source. Its
# capacity fixtures export without race instrumentation; new codec ownership
# and direct same-inode admission checks run with race separately.
go test -count=1 -v -run '^TestRustOwnerFoundationCorpus$' ./internal/releasetransition
go test -race -count=1 -v -run '^Test(LegacyCodecFrames|LegacyCodecProjectionBudget|RustOwnerLegacyCorpus|RustOwnerDirectSourceProof)$' ./internal/releasetransition
cargo test --locked --manifest-path release-owner/Cargo.toml -- --nocapture
cargo clippy --locked --manifest-path release-owner/Cargo.toml --all-targets -- --remap-path-prefix="$PWD=." -D warnings
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target x86_64-unknown-linux-musl
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target aarch64-unknown-linux-musl
git diff --check

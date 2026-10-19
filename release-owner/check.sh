#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export TMPDIR=/tmp
out="$PWD/.build/release-owner-foundation"
mkdir -p "$out"
# Keep downloads and intermediates shared; final artifacts belong to this check.
cargo_environment="$(python3 dev/cargo_cache.py --target-dir "${build:-$out/target}" --snapshot --run-target --manifest-path release-owner/Cargo.toml)"
eval "$cargo_environment"
export SUBYARD_RUST_OWNER_CORPUS="$out/go-corpus.json"
go version
"$RUSTC" --version
export SUBYARD_RUST_HISTORY_CORPUS="$out/history-corpus.json"
go test -race -count=1 -v ./internal/migrationhistory -run '^TestRustHistoryOracleCorpus$'
cargo test --locked --manifest-path release-owner/Cargo.toml --test history_oracle -- --ignored --nocapture
go test -race -count=1 ./internal/releasetransition/protocol/v1 ./internal/releasetransition/journal/v2
go test -race -count=1 -v -run 'Test(RustOwnerFoundationCorpus|JournalV2|ParseJournal|ParseRegistryV2|RegistryV2|CapabilityCatalog|BuiltinCapabilityCatalog|PublishedReleaseTransitionRegistry|PublishedRollbackJournal|CompatibilityEvidence|SupersededJournal|LedgerCheckpointV1|MigrationLedgerCheckpoint)' ./internal/releasetransition
cargo fmt --manifest-path release-owner/Cargo.toml -- --check
cargo test --locked --manifest-path release-owner/Cargo.toml -- --nocapture
cargo clippy --locked --manifest-path release-owner/Cargo.toml --all-targets -- --remap-path-prefix="$PWD=." -D warnings
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target x86_64-unknown-linux-musl
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target aarch64-unknown-linux-musl

#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export TMPDIR=/tmp
out="$PWD/.build/release-owner-foundation"
mkdir -p "$out"
# Prefer the configured stable toolchain; retain the existing task cache only
# as a compatibility fallback. This cache has no source-tree pilot dependency.
if ! cargo --version >/dev/null 2>&1 || ! rustc --version >/dev/null 2>&1; then
 if [[ -x .build/migrationhistory-pilot/toolchain/cargo/bin/cargo ]]; then
  export CARGO_HOME="$PWD/.build/migrationhistory-pilot/toolchain/cargo"
  export RUSTUP_HOME="$PWD/.build/migrationhistory-pilot/toolchain/rustup"
  export PATH="$CARGO_HOME/bin:$PATH"
 fi
fi
export SUBYARD_RUST_OWNER_CORPUS="$out/go-corpus.json"
go version
rustc --version
export SUBYARD_RUST_HISTORY_CORPUS="$out/history-corpus.json"
go test -race -count=1 -v ./internal/migrationhistory -run '^TestRustHistoryOracleCorpus$'
cargo test --locked --manifest-path release-owner/Cargo.toml --target-dir "$out/target" --test history_oracle -- --ignored --nocapture
go test -race -count=1 ./internal/releasetransition/protocol/v1 ./internal/releasetransition/journal/v2
go test -race -count=1 -v -run 'Test(RustOwnerFoundationCorpus|JournalV2|ParseJournal|ParseRegistryV2|RegistryV2|CapabilityCatalog|BuiltinCapabilityCatalog|PublishedReleaseTransitionRegistry|PublishedRollbackJournal|CompatibilityEvidence|SupersededJournal|LedgerCheckpointV1|MigrationLedgerCheckpoint)' ./internal/releasetransition
cargo fmt --manifest-path release-owner/Cargo.toml -- --check
cargo test --locked --manifest-path release-owner/Cargo.toml --target-dir "$out/target" -- --nocapture
cargo clippy --locked --manifest-path release-owner/Cargo.toml --target-dir "$out/target" --all-targets -- -D warnings
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target-dir "$out/target" --target x86_64-unknown-linux-musl
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target-dir "$out/target" --target aarch64-unknown-linux-musl

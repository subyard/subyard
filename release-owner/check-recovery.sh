#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export TMPDIR=/tmp
out="$PWD/.build/release-owner-recovery/orchestration"
build="$PWD/.build/release-owner-recovery/target"
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
cargo test --locked --manifest-path release-owner/Cargo.toml --target-dir "$build" --test history_oracle -- --ignored --nocapture
cargo fmt --manifest-path release-owner/Cargo.toml -- --check
cargo build --locked --manifest-path release-owner/Cargo.toml --target-dir "$build" --example source_probe
export SUBYARD_RUST_SOURCE_PROBE="$build/debug/examples/source_probe"
go test -race -count=1 -v -run 'Test(RustOwnerFoundationCorpus|RustOwnerDirectSourceProof|RecoveryReceiptPreservesCanonicalJournalsAndRequiresFreshBindings|RecoveryLifecycleFrontierStrictShapeAndGenerationIdentity|RecoveryLifecycleCancellationResumesEveryDurableBoundary|RecoveryLifecycleCancellationInvalidatesOnlyExactPendingCAS|RecoveryLifecycleExactResumeIgnoresUnrelatedHistoricalCorruption|RecoveryLifecycleCleanupRequiresTerminalProofAndPreservesReferences|RecoveryLifecycleGenerationOverflowPreservesEvidence|RecoveryLifecycleCompletedPredecessorClosesUnselectedReservations|RecoveryLifecyclePredecessorClosureCaptureAtOverwriteAndFault|RecoveryProcessV2StrictShapesLeaveFrozenV1Unchanged|MigrationOwnerProofAndRequestOwnNestedInputs|MigrationOwnerReplacementBindsRetainedTerminalHistory|POSIXV2StoreCleanupRejectsInvalidPredecessorGraphBeforeDeletion|POSIXV2StoreCleanupKeepsWholeComponentAtDeletionBound|PlanToken.*|ResumePlan.*|SourceMetadataAcceptsRetainedCompletedResourceCheckpoints|SourceMetadataAcceptsAuthenticatedHistoricalDomainPrefix|RecoveryLifecycleNamespaceCountsPairsAndRejectsAggregateBytes|RecoveryLifecycleLegacyCapacityAdmitsAfterProvenRetirement)$' ./internal/releasetransition
cargo test --locked --manifest-path release-owner/Cargo.toml --target-dir "$build" -- --nocapture
cargo clippy --locked --manifest-path release-owner/Cargo.toml --target-dir "$build" --all-targets -- -D warnings
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target-dir "$build" --target x86_64-unknown-linux-musl
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target-dir "$build" --target aarch64-unknown-linux-musl

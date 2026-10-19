#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export TMPDIR=/tmp
out="$PWD/.build/release-owner-recovery/orchestration"
build="$PWD/.build/release-owner-recovery/target"
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
cargo fmt --manifest-path release-owner/Cargo.toml -- --check
cargo build --locked --manifest-path release-owner/Cargo.toml --example source_probe
export SUBYARD_RUST_SOURCE_PROBE="$CARGO_TARGET_DIR/debug/examples/source_probe"
go test -race -count=1 -v -run 'Test(RustOwnerFoundationCorpus|RustOwnerDirectSourceProof|RecoveryReceiptPreservesCanonicalJournalsAndRequiresFreshBindings|RecoveryLifecycleFrontierStrictShapeAndGenerationIdentity|RecoveryLifecycleCancellationResumesEveryDurableBoundary|RecoveryLifecycleCancellationInvalidatesOnlyExactPendingCAS|RecoveryLifecycleExactResumeIgnoresUnrelatedHistoricalCorruption|RecoveryLifecycleCleanupRequiresTerminalProofAndPreservesReferences|RecoveryLifecycleGenerationOverflowPreservesEvidence|RecoveryLifecycleCompletedPredecessorClosesUnselectedReservations|RecoveryLifecyclePredecessorClosureCaptureAtOverwriteAndFault|RecoveryProcessV2StrictShapesLeaveFrozenV1Unchanged|MigrationOwnerProofAndRequestOwnNestedInputs|MigrationOwnerReplacementBindsRetainedTerminalHistory|POSIXV2StoreCleanupRejectsInvalidPredecessorGraphBeforeDeletion|POSIXV2StoreCleanupKeepsWholeComponentAtDeletionBound|PlanToken.*|ResumePlan.*|SourceMetadataAcceptsRetainedCompletedResourceCheckpoints|SourceMetadataAcceptsAuthenticatedHistoricalDomainPrefix|RecoveryLifecycleNamespaceCountsPairsAndRejectsAggregateBytes|RecoveryLifecycleLegacyCapacityAdmitsAfterProvenRetirement)$' ./internal/releasetransition
cargo test --locked --manifest-path release-owner/Cargo.toml -- --nocapture
cargo clippy --locked --manifest-path release-owner/Cargo.toml --all-targets -- --remap-path-prefix="$PWD=." -D warnings
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target x86_64-unknown-linux-musl
cargo build --locked --release --manifest-path release-owner/Cargo.toml --target aarch64-unknown-linux-musl

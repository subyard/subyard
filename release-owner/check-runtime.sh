#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export TMPDIR=/tmp
out="$PWD/.build/release-owner-runtime"
mkdir -p "$out"
# Keep downloads and intermediates shared; final artifacts belong to this check.
cargo_environment="$(python3 dev/cargo_cache.py --target-dir "${build:-$out/target}" --snapshot --run-target --manifest-path release-owner/Cargo.toml)"
eval "$cargo_environment"
export SUBYARD_RUST_RUNTIME_CORPUS="$out/runtime-corpus.json"
export SUBYARD_RUST_SETTINGS_CORPUS="$out/settings-corpus.json"
export SUBYARD_RUST_LINKS_CORPUS="$out/links-corpus.json"
export SUBYARD_RUST_REDUCER_CORPUS="$out/reducer-corpus.json"
export SUBYARD_RUST_INSPECTION_CORPUS="$out/inspection-corpus.json"
export SUBYARD_RUST_ACTIVATION_CORPUS="$out/activation-corpus.json"
export SUBYARD_RUST_ACTIVATION_FAILURE_CORPUS="$out/activation-failure-corpus.json"
export SUBYARD_RUST_ROLLBACK_CORPUS="$out/rollback-corpus.json"
export SUBYARD_RUST_MIGRATION_ENTRY_CORPUS="$out/migration-entry-corpus.json"
export SUBYARD_RUST_INSTALLED_RECIPE_CORPUS="$out/installed-recipes-corpus.json"
unset SUBYARD_RUST_OPERATION_REPLAY_OUTPUT
export SUBYARD_RUST_OPERATION_TRACE_CORPUS="$out/operation-trace-corpus.json"
export SUBYARD_RUST_OPERATION_PEER="$out/operation-peer"
export SUBYARD_RUST_CODEC_FAULT_PEER="$out/codec-faults"
go build -o "$SUBYARD_RUST_CODEC_FAULT_PEER" release-owner/tests/fixtures/codec_faults.go
go build -o "$SUBYARD_RUST_OPERATION_PEER" release-owner/tests/fixtures/operation_peer.go
go version
"$RUSTC" --version
export SUBYARD_RUST_HISTORY_CORPUS="$out/history-corpus.json"
go test -race -count=1 -v ./internal/migrationhistory -run '^TestRustHistoryOracleCorpus$'
cargo test --locked --manifest-path release-owner/Cargo.toml --test history_oracle -- --ignored --nocapture
cargo fmt --manifest-path release-owner/Cargo.toml -- --check
go test -count=1 -v ./internal/releasetransition -run '^TestRustOwner((RuntimeFacts|SettingsFacts|LinksFacts|ReducerFacts|InspectionFacts)Corpus|ActivationFailureCorpus|MigrationEntryFixture)$'
go test -count=1 -v ./internal/cli -run '^TestRustOwnerActivationFactsCorpus$'
go test -race -count=1 -v ./internal/releasetransition -run '^TestRustOwnerOperationTraceCorpus$'
go test -count=1 -v ./internal/adapters/releaseruntime -run '^TestRollbackSupportLeafOracle$'
go test -count=1 -v ./internal/adapters/reconcileruntime -run '^TestPreparedRuntimeRecipeCorpus$'
cargo test --locked --manifest-path release-owner/Cargo.toml --lib --bins -- --nocapture
cargo test --locked --manifest-path release-owner/Cargo.toml --doc
cargo clippy --locked --manifest-path release-owner/Cargo.toml --all-targets --all-features -- --remap-path-prefix="$PWD=." -D warnings
go test -race -count=1 ./internal/config ./internal/adapters/incusclient ./internal/adapters/reconcileruntime ./internal/releaseowner
go build -o "$out/yard" ./cmd/yard
cargo build --locked --manifest-path release-owner/Cargo.toml --bin subyard-release-owner
CARGO_TARGET_DIR="$CARGO_TARGET_DIR-fixture" cargo build --locked --manifest-path release-owner/Cargo.toml --features fixture-cuts --bin subyard-release-owner
export SUBYARD_RUST_PACKAGED_OWNER="$CARGO_TARGET_DIR/debug/subyard-release-owner"
export SUBYARD_RUST_OWNER_EXECUTABLE="$CARGO_TARGET_DIR-fixture/debug/subyard-release-owner"
export SUBYARD_RUST_FIXTURE_OWNER="$CARGO_TARGET_DIR-fixture/debug/subyard-release-owner"
export SUBYARD_RUST_NATIVE_HELPER="$out/yard"
export SUBYARD_RUST_OPERATION_REPLAY_OUTPUT="$out/operation-replay"
go test -race -count=1 -v ./internal/cli -run '^TestRustOwnerWholeOperationReplay$'
cargo test --locked --manifest-path release-owner/Cargo.toml --lib operation_oracle:: -- --nocapture
go test -race -count=1 -v ./internal/cli -run '^TestRustOwner(OperationNativeLocalSSH|DurableProcessCuts)$'
if [[ "$(go env GOARCH)" != amd64 ]]; then
 printf 'Closed official linux-amd64 migration-owner replacement: native unsupported-contract/no-effect control ran in the operation matrix.\n'
elif [[ -n "${SUBYARD_RUST_PREDECESSOR_ASSETS:-}" ]]; then
 go build -ldflags '-X github.com/Subyard/Subyard/internal/cli.Version=1.0.0' -o "$out/yard-migration-1.0.0" ./cmd/yard
 SUBYARD_RUST_NATIVE_HELPER="$out/yard-migration-1.0.0" go test -race -count=1 -v ./internal/cli -run '^TestRustOwnerMigrationOwner(Operation|ProcessCuts)$'
else
 printf 'Authentic migration-owner process check not run: set SUBYARD_RUST_PREDECESSOR_ASSETS to the verified baseline archive directory.\n' >&2
 exit 1
fi
# rust-lld ships with rustc; these are cross-build checks, never ARM execution.
sysroot="$("$RUSTC" --print sysroot)"
host="$("$RUSTC" -vV | sed -n 's/^host: //p')"
linker="$sysroot/lib/rustlib/$host/bin/rust-lld"
test -x "$linker"
for target in x86_64-unknown-linux-musl aarch64-unknown-linux-musl; do
 RUSTFLAGS="-C linker=$linker -C target-feature=+crt-static" cargo build --locked --release --manifest-path release-owner/Cargo.toml --target "$target" --bin subyard-release-owner
done

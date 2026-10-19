GO ?= go
PYTHON ?= python3
VERSION ?= 0.1.0-dev

.PHONY: build package test verify verify-veranda clean cli-docs cli-docs-check

build:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/build-engine.sh
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" ./dev/build-profiles.sh

test:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" ./dev/build-profiles.sh
	TMPDIR=/tmp $(GO) test -count=1 ./cmd/... ./internal/...
	bash dev/test-profiles.sh

cli-docs: build
	python3 dev/generate-cli-docs.py

cli-docs-check: build
	python3 dev/generate-cli-docs.py --check

verify:
	./tests/run.sh
	bash dev/test-profiles.sh

verify-veranda:
	$(PYTHON) dev/check-veranda.py

clean:
	@find .build -maxdepth 1 -type f -name 'yard' -delete 2>/dev/null || true

package:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/package-engine.sh

# Explicit delivery build; ordinary Go build/test targets need no Rust toolchain.
.PHONY: build-release-owner
build-release-owner:
	./dev/build-release-owner.sh

.PHONY: verify-release-owner-delivery
verify-release-owner-delivery:
	./dev/check-release-owner-delivery.sh

# Dev-only comparison; production Go verification does not require Rust.
.PHONY: verify-migrationhistory-rust
verify-migrationhistory-rust:
	TMPDIR=/tmp cargo fmt --manifest-path dev/migrationhistory-pilot/Cargo.toml -- --check
	TMPDIR=/tmp cargo test --locked --manifest-path dev/migrationhistory-pilot/Cargo.toml --target-dir .build/migrationhistory-pilot/target
	TMPDIR=/tmp cargo clippy --locked --manifest-path dev/migrationhistory-pilot/Cargo.toml --target-dir .build/migrationhistory-pilot/target --all-targets -- -D warnings
	TMPDIR=/tmp cargo build --locked --release --manifest-path dev/migrationhistory-pilot/Cargo.toml --target-dir .build/migrationhistory-pilot/target
	TMPDIR=/tmp SUBYARD_MIGRATIONHISTORY_RUST="$(CURDIR)/.build/migrationhistory-pilot/target/release/migrationhistory-pilot" $(GO) test -race -count=1 -v ./internal/migrationhistory
	TMPDIR=/tmp SUBYARD_MIGRATIONHISTORY_MEASURE=1 SUBYARD_MIGRATIONHISTORY_RUST="$(CURDIR)/.build/migrationhistory-pilot/target/release/migrationhistory-pilot" $(GO) test -count=1 -run TestRustPilotCorpus -v ./internal/migrationhistory

.PHONY: verify-release-owner-pilot
verify-release-owner-pilot:
	bash dev/release-owner-pilot/check.sh --local

.PHONY: verify-release-owner-foundation
verify-release-owner-foundation:
	bash release-owner/check.sh

.PHONY: verify-release-owner-recovery
verify-release-owner-recovery:
	./release-owner/check-recovery.sh

.PHONY: verify-release-owner-legacy
verify-release-owner-legacy:
	./release-owner/check-legacy.sh

.PHONY: verify-release-owner-runtime
verify-release-owner-runtime:
	./release-owner/check-runtime.sh

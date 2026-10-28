GO ?= go
PYTHON ?= python3
VERSION ?= 0.1.0-dev
TEST_VERSION ?= 0.22.0-test

.PHONY: build package test verify verify-veranda clean cli-docs cli-docs-check

build:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/build-engine.sh
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" ./dev/build-profiles.sh
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" ./dev/package-engine.sh --runtime-dir .build/runtime --engine .build/yard --version "$(VERSION)"

test:
	bash dev/test-profiles.sh --permissions-only
	bash tests/agent-skills.sh
	umask 0022 && $(MAKE) build VERSION="$(TEST_VERSION)"
	umask 0022 && TMPDIR=/tmp $(GO) test -count=1 -timeout=20m ./cmd/... ./internal/...
	bash dev/test-profiles.sh --after-permissions

cli-docs: build
	python3 dev/generate-cli-docs.py

cli-docs-check: build
	python3 dev/generate-cli-docs.py --check

verify:
	bash dev/test-profiles.sh --permissions-only
	./tests/run.sh
	bash dev/test-profiles.sh --after-permissions

verify-veranda:
	$(PYTHON) dev/check-veranda.py

clean:
	@find .build -maxdepth 1 -type f -name 'yard' -delete 2>/dev/null || true

package:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/package-engine.sh

# Experimental Rust tooling is explicit; ordinary build/package remain Go-only.
.PHONY: build-release-owner
build-release-owner:
	./dev/build-release-owner.sh

.PHONY: verify-release-owner-delivery
verify-release-owner-delivery:
	./dev/check-release-owner-delivery.sh

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

.PHONY: build-experimental-release-owner
build-experimental-release-owner: build
	./dev/package-engine.sh --experimental-release-owner --runtime-dir .build/runtime --engine .build/yard --version "$(VERSION)"

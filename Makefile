GO ?= go
PYTHON ?= python3
VERSION ?= 0.1.0-dev
TEST_VERSION ?= 0.22.0-test

.PHONY: build package test lint verify verify-veranda clean cli-docs cli-docs-check

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

lint:
	bash dev/lint-go.sh

verify:
	bash dev/test-profiles.sh --permissions-only
	$(MAKE) lint
	./tests/run.sh
	bash dev/test-profiles.sh --after-permissions

verify-veranda:
	$(PYTHON) dev/check-veranda.py

clean:
	@find .build -maxdepth 1 -type f -name 'yard' -delete 2>/dev/null || true

package:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/package-engine.sh

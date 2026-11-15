GO ?= go
PYTHON ?= python3
VERSION ?= 0.1.0-dev
TEST_VERSION ?= 0.29.0-test

.PHONY: build package test lint verify verify-veranda clean cli-docs cli-docs-engine cli-docs-check

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

# Help generation uses its own engine so it never republishes the runtime under test.
CLI_DOCS_ENGINE := .build/cli-docs/yard

cli-docs-engine:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/build-engine.sh --output "$(CLI_DOCS_ENGINE)"

cli-docs: cli-docs-engine
	python3 dev/generate-cli-docs.py --binary "$(CLI_DOCS_ENGINE)"

cli-docs-check: cli-docs-engine
	python3 dev/generate-cli-docs.py --check --binary "$(CLI_DOCS_ENGINE)"

lint:
	bash dev/lint.sh

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

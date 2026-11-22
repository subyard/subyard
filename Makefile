GO ?= go
PYTHON ?= python3
VERSION ?= 0.1.0-dev
TEST_VERSION ?= 0.29.0-test

.PHONY: build package test test-in-run lint verify verify-in-run verify-veranda clean cli-docs cli-docs-engine cli-docs-check rpc-vocabulary rpc-vocabulary-check

build: rpc-vocabulary-check
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/build-engine.sh
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" ./dev/build-profiles.sh
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" ./dev/package-engine.sh --runtime-dir .build/runtime --engine .build/yard --version "$(VERSION)"

test verify:
	bash dev/with-test-temp.sh $(MAKE) $@-in-run

test-in-run:
	bash dev/test-profiles.sh --permissions-only
	bash tests/agent-skills.sh
	umask 0022 && $(MAKE) build VERSION="$(TEST_VERSION)"
	umask 0022 && $(GO) test -count=1 -timeout=20m ./cmd/... ./internal/...
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

# Profiles skip the installer check that the core suite already passed in this run.
verify-in-run: rpc-vocabulary-check
	bash dev/test-profiles.sh --permissions-only
	$(MAKE) lint
	./tests/run.sh
	SUBYARD_CORE_INSTALLER_CHECKED=1 bash dev/test-profiles.sh --after-permissions

verify-veranda:
	$(PYTHON) dev/check-veranda.py

clean:
	@find .build -maxdepth 1 -type f -name 'yard' -delete 2>/dev/null || true

package:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/package-engine.sh

rpc-vocabulary:
	$(PYTHON) dev/generate-rpc-vocabulary.py

rpc-vocabulary-check:
	$(PYTHON) dev/generate-rpc-vocabulary.py --check

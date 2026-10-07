GO ?= go
PYTHON ?= python3
VERSION ?= 0.1.0-dev

.PHONY: build package test verify verify-veranda clean cli-docs cli-docs-check

build:
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" YARD_BUILD_VERSION="$(VERSION)" ./dev/build-engine.sh
	@PATH="$$(dirname "$$(command -v $(GO))"):$${PATH}" ./dev/build-profiles.sh

test:
	TMPDIR=/tmp $(GO) test ./cmd/... ./internal/...
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

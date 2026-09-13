.DEFAULT_GOAL := check

PYTHON ?= python3
BUN ?= bun
NPM ?= npm

.PHONY: check test contracts protocol help

.PHONY: check-demo backend-demo web-demo runtime-smoke runtime-shutdown-gate postgres-integration

check-demo: check backend-demo web-demo runtime-smoke

# Explicit opt-in only: the helper requires PORTUNEX_TEST_PG_BIN and refuses DSNs.
# Missing runtime is a failure, not a successful skipped integration gate.
postgres-integration:
	cd ../backend && go test -tags portunex_integration -race -count=1 -timeout=180s ./internal/portunex/...
	cd ../backend && go vet -tags portunex_integration ./internal/portunex/...

runtime-smoke:
	cd ../execution-plane/isthmus-runtime && $(BUN) run test/loopback.smoke.ts

# Explicit release gate: known failing on Bun 1.3.9, never reported as demo PASS.
runtime-shutdown-gate:
	cd ../execution-plane/isthmus-runtime && $(BUN) run test/shutdown-regression.smoke.ts

backend-demo:
	cd ../backend && go test -race -count=1 ./internal/portunex/... ./cmd/portunex-demo
	cd ../backend && go vet ./internal/portunex/... ./cmd/portunex-demo

web-demo:
	cd ../portunex-web && $(NPM) run typecheck
	cd ../portunex-web && $(NPM) test
	cd ../portunex-web && $(NPM) run build

check: test contracts protocol

test:
	PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=tooling $(PYTHON) -m unittest discover -s tests -t . -v

contracts:
	PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=tooling $(PYTHON) -m recoverykit contracts --root contracts

protocol:
	cd ../execution-plane/isthmus-runtime && $(BUN) test

help:
	PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=tooling $(PYTHON) -m recoverykit --help

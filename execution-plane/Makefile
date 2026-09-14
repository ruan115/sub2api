.DEFAULT_GOAL := test

BUF_VERSION := v1.72.0
BUF := go run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

.PHONY: fmt test test-race vet proto-lint proto-generate proto-check docker-e2e dataplane-check check

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

test:
	go test ./...

test-race:
	go test -race ./...

# In-memory authority and loopback RPC only; no Docker/DB/production credentials.
dataplane-check:
	go test -race -count=1 -timeout=120s ./internal/dataplane ./internal/hostagent ./internal/worker
	go vet ./internal/dataplane ./internal/hostagent ./internal/worker

vet:
	go vet ./...

proto-lint:
	$(BUF) lint

proto-generate:
	$(BUF) generate
	./scripts/sync-ccmax-proto.sh

proto-check: proto-lint
	./scripts/check-generated.sh

docker-e2e:
	./scripts/docker-e2e.sh

check: test test-race vet proto-check

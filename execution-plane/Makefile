.DEFAULT_GOAL := test

BUF_VERSION := v1.72.0
BUF := go run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

.PHONY: fmt test test-race vet proto-lint proto-generate proto-check docker-e2e check

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

test:
	go test ./...

test-race:
	go test -race ./...

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

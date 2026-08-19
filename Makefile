GOCACHE ?= $(CURDIR)/.cache/go-build
BIN_DIR ?= bin

.PHONY: build fmt-check test test-race mcp-integration vet mod-verify lima verify

build:
	mkdir -p $(BIN_DIR)
	GOCACHE=$(GOCACHE) go build -trimpath -o $(BIN_DIR)/repoworker ./cmd/repoworker

fmt-check:
	test -z "$(shell gofmt -l $$(find cmd internal -name '*.go' -print))"

test:
	GOCACHE=$(GOCACHE) go test ./...

test-race:
	GOCACHE=$(GOCACHE) go test -race ./...

mcp-integration:
	GOCACHE=$(GOCACHE) go test ./cmd/repoworker -run '^TestMCP(Repository|Task)ToolsAndSanitizedRejection$$' -count=1

vet:
	GOCACHE=$(GOCACHE) go vet ./...

mod-verify:
	go mod verify

lima:
	limactl validate repoworker-prod.yaml

verify: fmt-check vet test test-race mcp-integration mod-verify build lima

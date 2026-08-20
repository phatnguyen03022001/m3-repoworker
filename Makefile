GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod
GOPATH ?= $(CURDIR)/.cache/go-path
BIN_DIR ?= bin
GO_ENV = GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) GOPATH=$(GOPATH)
INTERNAL_GUARD = env -u M3_REPOWORKER_INTERNAL_FIXED_PRESET -u REPOWORKER_RUN_PRESET_SEQUENCE

.PHONY: bootstrap build fmt-check test test-race mcp-integration vet mod-verify offline-verify lima ci verify

bootstrap:
	mkdir -p $(GOMODCACHE)
	GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org $(GO_ENV) go mod download

build:
	mkdir -p $(BIN_DIR)
	$(INTERNAL_GUARD) $(GO_ENV) go build -trimpath -o $(BIN_DIR)/repoworker ./cmd/repoworker

fmt-check:
	test -z "$(shell gofmt -l $$(find cmd internal -name '*.go' -print))"

test:
	$(INTERNAL_GUARD) $(GO_ENV) go test ./...

test-race:
	$(INTERNAL_GUARD) $(GO_ENV) go test -race ./...

mcp-integration:
	$(INTERNAL_GUARD) $(GO_ENV) go test ./cmd/repoworker -run '^Test(MCP((Repository|Task)ToolsAndSanitizedRejection|VerificationReturnsBoundedSanitizedDiagnostic)|ProductionMCPToolSurface)$$' -count=1

vet:
	$(INTERNAL_GUARD) $(GO_ENV) go vet ./...

mod-verify:
	$(INTERNAL_GUARD) $(GO_ENV) go mod verify

offline-verify:
	GOPROXY=off GOSUMDB=off $(MAKE) GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) GOPATH=$(GOPATH) verify

lima:
	limactl validate repoworker-prod.yaml

ci: fmt-check vet test test-race mcp-integration mod-verify build

verify: ci lima

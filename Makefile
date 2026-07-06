# Shevet build entry points. All Go builds are CGO-free by design (ADR-0002).

MODULE  := github.com/irodion/shevet
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(MODULE)/internal/version.version=$(VERSION)

GO := CGO_ENABLED=0 go

# Cross-compile targets: every supported Host and Client platform pair.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build
build: ## Build the shevet binary for the current platform
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/shevet .

.PHONY: cross
cross: ## Verify the binary builds for every supported platform
	@for platform in $(PLATFORMS); do \
		GOOS=$${platform%/*} GOARCH=$${platform#*/} $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o /dev/null . \
			&& echo "ok  $$platform" || exit 1; \
	done

.PHONY: test
test: ## Run all tests with the race detector
	go test -race ./...

.PHONY: lint
lint: ## gofmt, go vet, staticcheck (module packages only; docs/research has its own modules)
	@fmt_out=$$(gofmt -l $$(git ls-files '*.go' ':!docs/')); if [ -n "$$fmt_out" ]; then echo "gofmt needed:"; echo "$$fmt_out"; exit 1; fi
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...

.PHONY: proto
proto: ## Regenerate gRPC/protobuf code (requires protoc, protoc-gen-go, protoc-gen-go-grpc)
	protoc --proto_path=proto \
		--go_out=proto --go_opt=paths=source_relative \
		--go-grpc_out=proto --go-grpc_opt=paths=source_relative \
		proto/shevet/v1/shevet.proto

.PHONY: help
help: ## List targets
	@grep -E '^[a-z]+:.*##' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

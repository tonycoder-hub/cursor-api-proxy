GO ?= go
PROTOC ?= protoc
PROTOC_GEN_GO ?= protoc-gen-go
VERSION ?= dev

.PHONY: build test vet fmt-check generate

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/cursor-api-proxy ./cmd/cursor-api-proxy

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt-check:
	@test -z "$$(gofmt -l cmd internal gen)"

generate:
	$(PROTOC) --plugin=protoc-gen-go=$(PROTOC_GEN_GO) --go_out=. --go_opt=module=github.com/tonycoder-hub/cursor-api-proxy proto/cursor_v1.proto

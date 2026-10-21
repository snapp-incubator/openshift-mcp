.PHONY: build test lint run-mcp run-http cli docker

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -o bin/openshift-mcp ./cmd/openshift-mcp
	go build -o bin/mcp-cli ./cmd/mcp-cli

test:
	go test ./internal/...

lint:
	golangci-lint run

run-mcp:
	go run ./cmd/openshift-mcp -mcp

run-http:
	go run ./cmd/openshift-mcp -http-addr :8080

cli:
	go build -o bin/mcp-cli ./cmd/mcp-cli

docker:
	TAG=$(VERSION) docker buildx bake -f build/package/docker-bake.json

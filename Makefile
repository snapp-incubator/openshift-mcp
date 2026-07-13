.PHONY: build test lint run-mcp run-http docker-build docker-push

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= registry.snapp.tech/snappcloud/openshift-mcp

build:
	go build -o bin/openshift-mcp ./cmd/openshift-mcp

test:
	go test ./internal/...

lint:
	golangci-lint run

run-mcp:
	go run ./cmd/openshift-mcp -mcp

run-http:
	go run ./cmd/openshift-mcp -http-addr :8080

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

docker-push:
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest


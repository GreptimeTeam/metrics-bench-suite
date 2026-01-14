# Go parameters
GOCMD=GO111MODULE=on go
GOBUILD=$(GOCMD) build
GOINSTALL=$(GOCMD) install
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt

.PHONY: all fmt lint docker-build-push

BINS := $(patsubst ./cmd/%/main.go,%,$(wildcard ./cmd/*/main.go))

# Build all binaries
all: $(BINS)

lint-all: fmt lint

# Pattern rule to build each binary
$(BINS):
	$(GOGET) ./cmd/$@
	$(GOBUILD) -o bin/$@ ./cmd/$@

# Format code
fmt:
	$(GOFMT) ./...

# Lint code
lint:
	golint ./...

go-lint:
	golangci-lint run ./...

# Docker build and push
REGISTRY ?= greptime-registry.cn-hangzhou.cr.aliyuncs.com
REPO ?= tools/metrics-bench-suite
TAG ?= 0.2.3
IMAGE_NAME := $(REGISTRY)/$(REPO):$(TAG)

docker-build-push:
	docker build -t $(IMAGE_NAME) . && docker push $(IMAGE_NAME)

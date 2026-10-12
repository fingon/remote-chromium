ENGINE ?= podman
IMAGE ?= localhost/hermes-chromium:stable

.DEFAULT_GOAL := help

.PHONY: build start test proxy-lint proxy-vet proxy-test proxy-race proxy-build help

build:
	$(ENGINE) build $(if $(CONTENT_KEY),--build-arg CONTENT_KEY=$(CONTENT_KEY),) -t $(IMAGE) .

start:
	$(ENGINE) compose up

proxy-vet:
	cd cdp-proxy && go vet ./...

proxy-lint:
	cd cdp-proxy && golangci-lint fmt --no-config -E gofmt
	cd cdp-proxy && golangci-lint run --no-config

proxy-test:
	cd cdp-proxy && go test ./...

proxy-race:
	cd cdp-proxy && go test -race ./...

proxy-build:
	cd cdp-proxy && go build -o /dev/null .

test: proxy-vet proxy-test build
	ENGINE='$(ENGINE)' IMAGE='$(IMAGE)' ./test-integration.sh

help:
	@echo "Targets:"
	@echo "  build       Build the container image ($(IMAGE))"
	@echo "  start       Start the container in the foreground via compose (requires VNC_PASSWORD)"
	@echo "  proxy-vet   go vet the cdp-proxy module"
	@echo "  proxy-lint  Format and lint cdp-proxy with golangci-lint"
	@echo "  proxy-test  Run cdp-proxy unit tests"
	@echo "  proxy-race  Run cdp-proxy tests with the race detector"
	@echo "  proxy-build Build the cdp-proxy binary without keeping an artifact"
	@echo "  test        Unit tests + integration test: temp-profile container, verify CDP chain, auto-cleanup"

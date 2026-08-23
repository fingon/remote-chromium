ENGINE ?= podman
IMAGE ?= localhost/hermes-chromium:trixie

.DEFAULT_GOAL := help

.PHONY: build start test proxy-vet proxy-test help

build:
	$(ENGINE) build $(if $(CONTENT_KEY),--build-arg CONTENT_KEY=$(CONTENT_KEY),) -t $(IMAGE) .

start:
	$(ENGINE) compose up

proxy-vet:
	cd cdp-proxy && go vet ./...

proxy-test:
	cd cdp-proxy && go test ./...

test: proxy-vet proxy-test build
	ENGINE='$(ENGINE)' IMAGE='$(IMAGE)' ./test-integration.sh

help:
	@echo "Targets:"
	@echo "  build       Build the container image ($(IMAGE))"
	@echo "  start       Start the container in the foreground via compose (requires VNC_PASSWORD)"
	@echo "  proxy-vet   go vet the cdp-proxy module"
	@echo "  proxy-test  Run cdp-proxy unit tests"
	@echo "  test        Unit tests + integration test: temp-profile container, verify CDP chain, auto-cleanup"

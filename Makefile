ENGINE ?= podman
IMAGE ?= localhost/hermes-chromium:trixie

.DEFAULT_GOAL := help

.PHONY: build start test help

build:
	$(ENGINE) build -t $(IMAGE) .

start:
	$(ENGINE) compose up

test: build
	ENGINE='$(ENGINE)' IMAGE='$(IMAGE)' ./test-integration.sh

help:
	@echo "Targets:"
	@echo "  build  Build the container image ($(IMAGE))"
	@echo "  start  Start the container in the foreground via compose (requires VNC_PASSWORD)"
	@echo "  test   Integration test: temp-profile container, verify CDP chain, auto-cleanup"

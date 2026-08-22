ENGINE ?= podman
IMAGE ?= localhost/hermes-chromium:trixie

.DEFAULT_GOAL := help

.PHONY: build start help

build:
	$(ENGINE) build -t $(IMAGE) .

start:
	$(ENGINE) compose up

help:
	@echo "Targets:"
	@echo "  build  Build the container image ($(IMAGE))"
	@echo "  start  Start the container in the foreground via compose (requires VNC_PASSWORD)"

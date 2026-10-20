.DEFAULT_GOAL := help

# Optional overrides for the integration tests; see README.
-include integration/local.mk
export CONTAINER_RUNTIME IT_BASE_IMAGE IT_APT_MIRROR IT_BUILD_FLAGS

.PHONY: help build install test test-integration

help:
	@printf '%s\n' \
	  'make build             build bin/gst' \
	  'make install           install gst with go install' \
	  'make test              run the unit tests' \
	  'make test-integration  run the integration tests against sshd in a container'

build:
	go build -o bin/gst ./cmd/gst

install:
	go install ./cmd/gst

test:
	go test ./...

test-integration:
	cd integration && go test -tags integration -count=1 -v .

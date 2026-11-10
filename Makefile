.DEFAULT_GOAL := help

# Optional overrides for the integration tests; see integration/README.md.
-include integration/local.mk
export CONTAINER_RUNTIME IT_BASE_IMAGE IT_APT_MIRROR IT_BUILD_FLAGS

.PHONY: help build install test test-integration release ios

help:
	@printf '%s\n' \
	  'make build             build bin/gst' \
	  'make install           install gst with go install' \
	  'make test              run the unit tests' \
	  'make test-integration  run the integration tests against sshd in a container' \
	  'make release V=X.Y.Z   set the version, test, commit, and tag' \
	  'make ios               build the Go code for the iOS app and generate ios/Gst.xcodeproj (on a Mac)'

build:
	go build -o bin/gst ./cmd/gst

install:
	go install ./cmd/gst

test:
	go test ./...

test-integration:
	cd integration && go test -tags integration -count=1 -v .

release:
	scripts/release.sh "$(V)"

ios:
	ios/build-core.sh
	cd ios && xcodegen generate

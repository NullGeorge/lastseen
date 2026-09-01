.PHONY: build docker-test clean version

IMAGE ?= lastseen:dev
DIST ?= dist

# Identity of the build. The date is the commit date rather than "now", so
# rebuilding the same commit produces the same binary and does not bust the
# Docker layer cache.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
STAMP = --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE)

build:
	docker build $(STAMP) --target export --output type=local,dest=$(DIST) .

version:
	@echo "$(VERSION) (commit $(COMMIT), $(DATE))"

# Run the test suite in the toolchain the binary is built with, no local Go
# needed. The source reaches the container through the build context rather
# than a bind mount: a mount resolves against whatever the daemon can see,
# and one stale path is enough to test a tree that is not this one.
docker-test:
	docker build --target build -t $(IMAGE)-build .
	docker run --rm $(IMAGE)-build go test ./...

clean:
	rm -rf $(DIST)

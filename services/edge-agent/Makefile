# Local build helpers for the standalone edge-agent binary.
#
#   make build        # build for the host into dist/
#   make dist         # cross-compile all PLATFORMS into dist/ (+ .sha256)
#   make vet test     # static analysis and unit tests
#   make docker       # build the standalone agent image
#   make clean        # remove dist/
#
# Artifact names mirror the release workflow so local packaging matches the
# published Release assets (onekccdn-edge-agent-<os>-<arch>).

BINARY      := onekccdn-edge-agent
PKG         := ./cmd/edge-agent
DIST        := dist
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS     := -trimpath
LDFLAGS     := -s -w -X main.version=$(VERSION)
PLATFORMS   ?= linux/amd64 linux/arm64 linux/arm darwin/arm64
IMAGE       ?= onekccdn-edge-agent
SHASUM      := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo 'shasum -a 256')

export CGO_ENABLED := 0

.PHONY: all build dist vet test docker clean

all: build

build:
	@mkdir -p $(DIST)
	go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY) $(PKG)

dist:
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
	  os=$${platform%/*}; arch=$${platform#*/}; \
	  out=$(BINARY)-$$os-$$arch; \
	  echo "building $(DIST)/$$out (version $(VERSION))"; \
	  GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(DIST)/$$out $(PKG) || exit 1; \
	  ( cd $(DIST) && $(SHASUM) $$out > $$out.sha256 ); \
	done
	@echo "artifacts written to $(DIST)/"

vet:
	go vet ./...

test:
	go test ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

clean:
	rm -rf $(DIST)

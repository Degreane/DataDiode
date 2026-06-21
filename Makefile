# DataDiode — Makefile
#
# One-word entry points for the common workflows so operators and
# contributors don't have to memorise the underlying go/lxc commands.
# Run `make help` for the full list.

SHELL       := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

# ---- versioning -----------------------------------------------------------

VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE      := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# ---- paths ----------------------------------------------------------------

BIN_DIR   := bin
DIST_DIR  := dist
BINARY    := $(BIN_DIR)/diode
PKG       := ./cmd/diode

# ---- build flags ----------------------------------------------------------

LDFLAGS   := -s -w \
             -X 'main.version=$(VERSION)' \
             -X 'main.commit=$(COMMIT)' \
             -X 'main.buildDate=$(DATE)'
GOFLAGS   := -trimpath -ldflags "$(LDFLAGS)"
GO_ENV    := CGO_ENABLED=0

# Cross-compile matrix mirrored from .github/workflows/ci.yml.
CROSS_TARGETS := linux/amd64 linux/arm64 \
                 darwin/amd64 darwin/arm64 \
                 windows/amd64 windows/arm64 \
                 freebsd/amd64

# ---- defaults -------------------------------------------------------------

.DEFAULT_GOAL := help
.PHONY: help build test test-unit e2e fuzz bench lint fmt vet \
        cross clean demo demo-teardown prereqs install uninstall \
        ci tidy

# ---- self-documenting help ------------------------------------------------

## help: Print this help (default target).
help:
	@printf '\nDataDiode — make targets\n\n'
	@awk 'BEGIN {FS = ":.*?## "} \
	     /^##[[:space:]]*[a-zA-Z0-9_-]+:/ { \
	       sub(/^## /, "", $$0); \
	       split($$0, a, ":"); \
	       printf "  \033[1;34m%-16s\033[0m %s\n", a[1], substr($$0, length(a[1])+2); \
	     }' $(MAKEFILE_LIST)
	@printf '\nversion: %s   commit: %s\n\n' '$(VERSION)' '$(COMMIT)'

# ---- build ----------------------------------------------------------------

## build: Build the diode binary into bin/diode (static, stripped).
build: $(BINARY)

$(BINARY): $(shell find cmd internal -name '*.go' 2>/dev/null) go.mod
	@mkdir -p $(BIN_DIR)
	$(GO_ENV) go build $(GOFLAGS) -o $(BINARY) $(PKG)
	@echo "built: $(BINARY) ($(VERSION))"

## install: Install bin/diode into /usr/local/bin (requires sudo).
install: build
	sudo install -m 0755 $(BINARY) /usr/local/bin/diode

## uninstall: Remove /usr/local/bin/diode (requires sudo).
uninstall:
	sudo rm -f /usr/local/bin/diode

# ---- test -----------------------------------------------------------------

## test: Run the full test suite (unit + E2E).
test:
	go test ./... -timeout 180s

## test-unit: Run only the unit tests (skip ./test/e2e/).
test-unit:
	go test $$(go list ./... | grep -v '/test/e2e$$') -timeout 60s

## e2e: Run only the E2E suite (spawns the binary on 127.0.0.1).
e2e:
	go test ./test/e2e/ -v -timeout 180s

## fuzz: Run a 30-second fuzz of framing.Decode.
fuzz:
	go test ./internal/framing -run=NONE -fuzz=FuzzDecode -fuzztime=30s

## bench: Run all benchmarks (allocations + throughput).
bench:
	go test ./... -bench=. -benchmem -run=NONE

# ---- lint / format --------------------------------------------------------

## lint: gofmt + go vet + go mod tidy no-op check (mirrors CI lint job).
lint: vet
	@out=$$(gofmt -l .); \
	if [[ -n "$$out" ]]; then \
	  echo "files need gofmt:"; echo "$$out"; exit 1; \
	fi
	@before=$$(sha256sum go.mod go.sum 2>/dev/null | sort || true); \
	go mod tidy; \
	after=$$(sha256sum go.mod go.sum 2>/dev/null | sort || true); \
	if [[ "$$before" != "$$after" ]]; then \
	  echo "go.mod / go.sum changed by 'go mod tidy' — commit the updates"; \
	  exit 1; \
	fi

## fmt: Run gofmt -w on all Go files.
fmt:
	gofmt -w .

## vet: Run go vet against the project.
vet:
	go vet ./...

## tidy: Run go mod tidy.
tidy:
	go mod tidy

# ---- cross-compile --------------------------------------------------------

## cross: Build for every (GOOS, GOARCH) in the matrix into dist/.
cross:
	@mkdir -p $(DIST_DIR)
	@for target in $(CROSS_TARGETS); do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  ext=""; [[ "$$os" == "windows" ]] && ext=".exe"; \
	  out="$(DIST_DIR)/diode-$$os-$$arch$$ext"; \
	  echo "  → $$out"; \
	  GOOS=$$os GOARCH=$$arch $(GO_ENV) \
	    go build $(GOFLAGS) -o "$$out" $(PKG) || exit 1; \
	done
	@ls -la $(DIST_DIR)/

# ---- LXC demo wrappers ----------------------------------------------------

## prereqs: Check host prerequisites for the LXC demo (no root needed).
prereqs:
	./scripts/check-prereqs.sh

## demo: Run the full two-LXC live demo (REQUIRES SUDO).
demo:
	sudo ./scripts/demo.sh

## demo-teardown: Remove the LXC containers and bridge (REQUIRES SUDO).
demo-teardown:
	sudo ./scripts/lxc-teardown.sh

# ---- maintenance ----------------------------------------------------------

## clean: Remove built binaries and dist artifacts.
clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)
	@echo "cleaned: $(BIN_DIR)/, $(DIST_DIR)/"

## ci: Run the same checks CI runs (lint + test + fuzz).
ci: lint test fuzz

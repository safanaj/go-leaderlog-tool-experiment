# Makefile for go-leaderlog-tool
#
# Builds the `llt` executable (a pure-Go PRAOS leader-log tool) and provides
# the usual build/test/clean targets.
#
# Common targets:
#   make            # build ./llt
#   make test       # run unit tests
#   make ci         # fmt-check + vet + test + build (full check)
#   make run ARGS="..."   # build then run llt with args
#   make clean      # remove build artifacts
#   make help       # list targets

BINARY     := llt
PKG        := .
GO         := go

# gouroboros v0.220.4 requires go >= 1.26.11; let the toolchain auto-select it.
export GOTOOLCHAIN := auto

.PHONY: all build test vet fmt fmt-check tidy clean run help

all: build

## build: compile the llt executable
build:
	$(GO) build -o $(BINARY) $(PKG)

## test: run unit tests (E34 / consensus parity)
test:
	$(GO) test -count=1 ./...

## vet: run go vet on the package
vet:
	$(GO) vet ./...

## fmt: gofmt -w the package source
fmt:
	gofmt -w .

## fmt-check: fail if any top-level file is unformatted (ignores unused/)
fmt-check:
	@out=$$(gofmt -l . | grep -v '^unused/' || true); \
	if [ -n "$$out" ]; then echo "gofmt: unformatted files:"; echo "$$out"; exit 1; fi

## tidy: go mod tidy
tidy:
	$(GO) mod tidy

## clean: remove build artifacts
clean:
	rm -f $(BINARY)

## run: build then run llt with ARGS="..."
run: build
	./$(BINARY) $(ARGS)

## ci: full local check (format + vet + test + build)
ci: fmt-check vet test build

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/^## /  /'

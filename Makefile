# fornax — one static binary, stdlib only. `make verify` is the pre-push gate.

GO ?= go
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

build:
	$(GO) build -o fornax .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

# fmt-check fails if any file is not gofmt-clean.
verify: fmt-check vet test
	@echo verify ok

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

# Build every release target to /dev/null — catches per-platform breakage.
matrix:
	@for t in $(PLATFORMS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -o /dev/null . || exit 1; \
		echo "$$t ok"; \
	done

install: build
	install -m755 fornax "$${FORNAX_INSTALL:-$$HOME/.local/bin}/fornax"

clean:
	rm -f fornax

.PHONY: build test vet fmt verify fmt-check matrix install clean

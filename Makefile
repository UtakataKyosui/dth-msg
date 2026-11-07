.PHONY: test build wasm fmt fmt-check lint

GOLANGCI_LINT ?= golangci-lint

fmt:
	git ls-files -z -- '*.go' | xargs -0 gofmt -w

fmt-check:
	@unformatted=$$(git ls-files -z -- '*.go' | xargs -0 gofmt -l) || exit 1; \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "$$unformatted"; \
		printf '%s\n' 'Go files need formatting. Run make fmt.'; \
		exit 1; \
	fi

lint:
	$(GOLANGCI_LINT) run ./...
	GOOS=js GOARCH=wasm $(GOLANGCI_LINT) run ./...

test:
	go test ./...
	go vet ./...

build:
	mkdir -p build
	go build -o build/dth-msg ./cmd/server

wasm:
	mkdir -p build
	GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o build/monitor.wasm ./cmd/worker
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" build/wasm_exec.js

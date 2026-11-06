.PHONY: test build wasm

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

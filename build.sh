#!/usr/bin/env bash
# Build the static site into dist/: run the engine tests, compile Go to WebAssembly,
# and inline everything else into index.html.
set -euo pipefail
cd "$(dirname "$0")"
go test ./sim/
mkdir -p dist
GOOS=js GOARCH=wasm go build -ldflags="-s -w" -trimpath -o dist/main.wasm ./wasm
WASM_EXEC="$(go env GOROOT)/lib/wasm/wasm_exec.js"
[ -f "$WASM_EXEC" ] || WASM_EXEC="$(go env GOROOT)/misc/wasm/wasm_exec.js"
cp "$WASM_EXEC" web/src/wasm_exec.js
python3 scripts/compose.py .

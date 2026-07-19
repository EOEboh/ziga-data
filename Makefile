# web/app.js is a committed build artifact: //go:embed web needs it present,
# so `go build` works without Node. Rebuild it after touching ui/*.ts.
.PHONY: ui ui-check build test run smoke

ui:
	npx esbuild ui/main.ts --bundle --format=iife --target=es2020 --outfile=web/app.js

ui-check:
	npx tsc --noEmit -p ui

build: ui
	go build ./...

test:
	go vet ./...
	go test ./...

run:
	go run ./cmd/server

# Live end-to-end check (real LLM call; real sheet append when SHEET_ID is
# configured). See scripts/smoke.sh and the README's "Live smoke test".
smoke:
	bash scripts/smoke.sh

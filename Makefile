# web/dist is a committed build artifact: //go:embed all:web/dist needs it
# present, so `go build` works without Node. Rebuild it (make ui-build) after
# touching web/src and commit the result.
.PHONY: ui-build ui-check build test run smoke

ui-build:
	npm --prefix web run build

ui-check:
	npx tsc --noEmit -p web

build: ui-build
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

# web/dist is a committed build artifact: //go:embed all:web/dist needs it
# present, so `go build` works without Node. Rebuild it (make ui-build) after
# touching web/src and commit the result.
.PHONY: ui-build ui-check build test run smoke tunnel tunnel-down

# Staging tunnel (see the README's "Local staging access"). The real host and
# user live in deploy/tunnel.env, which is untracked (*.env is gitignored), so
# this public repo carries no real IP or username. Override per-invocation with
# e.g. `make tunnel TUNNEL_HOST=1.2.3.4`.
TUNNEL_ENV  ?= deploy/tunnel.env
-include $(TUNNEL_ENV)
TUNNEL_USER ?=
TUNNEL_HOST ?=
LOCAL_PORT  ?= 8080
REMOTE_PORT ?= 8090
FWD          = $(LOCAL_PORT):localhost:$(REMOTE_PORT)

ui-build:
	npm --prefix web run build

ui-check:
	npm --prefix web run check

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

# Open a verified SSH tunnel to the staging app and hold it open (Ctrl+C to
# close). The local port must stay 8080: the Google OAuth redirect URI is
# registered as http://localhost:8080 and Google matches the port exactly.
# Only a local dev build of *this* app is cleared off the port; anything else
# holding it is reported, not killed.
tunnel:
	@if [ -z "$(TUNNEL_HOST)" ] || [ -z "$(TUNNEL_USER)" ]; then \
		echo "TUNNEL_HOST/TUNNEL_USER are not set. Create $(TUNNEL_ENV) (untracked) with:"; \
		echo ""; \
		echo "    TUNNEL_USER=<deploy user>"; \
		echo "    TUNNEL_HOST=<server host or IP>"; \
		echo ""; \
		exit 1; \
	fi; \
	for pid in $$(lsof -ti tcp:$(LOCAL_PORT) -sTCP:LISTEN 2>/dev/null); do \
		comm=$$(ps -o comm= -p $$pid 2>/dev/null); \
		cwd=$$(lsof -a -p $$pid -d cwd -Fn 2>/dev/null | sed -n 's/^n//p'); \
		what=""; \
		case "$$comm" in \
			*go-build*) [ "$$cwd" = "$(CURDIR)" ] && what="local dev server";; \
			ssh|*/ssh) case "$$(ps -o args= -p $$pid 2>/dev/null)" in \
				*"$(FWD)"*) what="stale tunnel";; \
			esac;; \
		esac; \
		if [ -n "$$what" ]; then \
			echo "clearing $$what off :$(LOCAL_PORT) (PID $$pid)"; \
			kill $$pid 2>/dev/null || true; \
		else \
			echo "port $(LOCAL_PORT) is held by PID $$pid ($$comm), which is not a local dev build of this app - not killing it."; \
			echo "free the port yourself, then re-run make tunnel."; \
			exit 1; \
		fi; \
	done; \
	for i in 1 2 3 4 5 6 7 8 9 10; do \
		lsof -ti tcp:$(LOCAL_PORT) -sTCP:LISTEN >/dev/null 2>&1 || break; \
		sleep 0.2; \
	done; \
	for p in $$(pgrep -f "$(FWD)" 2>/dev/null); do \
		case "$$(ps -o comm= -p $$p 2>/dev/null)" in \
			ssh|*/ssh) echo "closing stale tunnel (PID $$p)"; kill $$p 2>/dev/null || true;; \
		esac; \
	done; \
	ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
		-L $(FWD) $(TUNNEL_USER)@$(TUNNEL_HOST) & \
	ssh_pid=$$!; \
	trap 'kill $$ssh_pid 2>/dev/null; exit 0' INT TERM; \
	up=0; \
	for i in 1 2 3 4 5 6 7 8 9 10; do \
		kill -0 $$ssh_pid 2>/dev/null || break; \
		if curl -fsS --max-time 2 http://localhost:$(LOCAL_PORT)/healthz 2>/dev/null | grep -q '^ok'; then up=1; break; fi; \
		sleep 0.5; \
	done; \
	if [ "$$up" != 1 ]; then \
		kill $$ssh_pid 2>/dev/null || true; \
		echo "tunnel failed: no 'ok' from http://localhost:$(LOCAL_PORT)/healthz within 5s"; \
		echo "(ssh could not connect, could not forward the port, or the app is down on :$(REMOTE_PORT))"; \
		exit 1; \
	fi; \
	echo "tunnel live → app.zigadata.com staging at http://localhost:$(LOCAL_PORT)"; \
	echo "keep this terminal open; Ctrl+C closes the tunnel"; \
	wait $$ssh_pid || { echo "tunnel dropped (ssh exited); re-run make tunnel"; exit 1; }

# Close a tunnel left running from somewhere else. pgrep -f on the forward spec
# also matches this recipe's own shell (the pattern is in its argv), so only
# real ssh processes are killed.
tunnel-down:
	@found=0; \
	for p in $$(pgrep -f "$(FWD)" 2>/dev/null); do \
		case "$$(ps -o comm= -p $$p 2>/dev/null)" in \
			ssh|*/ssh) kill $$p 2>/dev/null && { echo "closed tunnel (PID $$p)"; found=1; };; \
		esac; \
	done; \
	[ "$$found" = 1 ] || echo "no tunnel running"

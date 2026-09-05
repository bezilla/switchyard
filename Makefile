# Switchyard
#
# Start here:  make init     (once per clone -- installs the pre-push gate)
#              make up       (the demo)
#
# The failure-injection targets below are the demo. They post to the gateway's
# admin endpoint; nothing about them is privileged, and by default none of them
# touch a real provider, because by default there are no real providers. The
# `ollama` profile adds one; see up-ollama.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

SWITCHYARD_PORT ?= 8080
GRAFANA_PORT ?= 3000
PROMETHEUS_PORT ?= 9090
export SWITCHYARD_PORT GRAFANA_PORT PROMETHEUS_PORT

GATEWAY ?= http://localhost:$(SWITCHYARD_PORT)
GRAFANA ?= http://localhost:$(GRAFANA_PORT)

# Pinned tool versions. Renovate proposes bumps on the dependency dashboard;
# see renovate.json5 for why it never opens a pull request to do it.
GOVULNCHECK_VERSION ?= v1.7.0

# The opt-in real-provider path. UPSTREAMS points at a file inside the gateway
# container, mounted read-only from ./deploy/upstreams.
OLLAMA_MODEL ?= qwen2.5:0.5b
OLLAMA_UPSTREAMS ?= @/etc/switchyard/upstreams/ollama.json
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# curl, quiet, failing the make target on an HTTP error rather than printing
# the error body and carrying on.
CURL := curl --silent --show-error --fail-with-body

.PHONY: help
help: ## Show this help
	@echo 'Switchyard -- an AI provider gateway that survives its providers'
	@echo
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ── setup ─────────────────────────────────────────────────────────────────────

.PHONY: init
init: ## Step 1 for any clone: install the pre-push gate
	@git config core.hooksPath .githooks
	@git config user.name  'Paul Bezilla'
	@git config user.email 'bezilla@protonmail.com'
	@if ! command -v gitleaks >/dev/null 2>&1; then \
		echo 'warning: gitleaks is not on PATH. The pre-push hook fails closed'; \
		echo '         without it. Install with: brew install gitleaks'; \
	fi
	@echo 'hooks installed: core.hooksPath=.githooks'
	@echo 'identity: '"$$(git config user.name)"' <'"$$(git config user.email)"'>'

.PHONY: test-hook
test-hook: ## Prove the pre-push gate rejects bad history
	@bash .githooks/selftest.sh

# Pushing to main when its required checks have not run yet is a hand
# procedure, written down in docs/maintainer-notes.md. There is deliberately no
# target for it: the two steps that carry its whole safety are the ones a target
# would encourage skipping.

# ── the demo ──────────────────────────────────────────────────────────────────

.PHONY: up
up: ## Start the stack: gateway, Prometheus, Grafana on :3000
	@SWITCHYARD_VERSION=$(VERSION) docker compose up --build -d
	@echo
	@echo 'Grafana:    $(GRAFANA)   (no login; the dashboard is the home page)'
	@echo 'Gateway:    $(GATEWAY)'
	@echo 'Prometheus: http://localhost:$(PROMETHEUS_PORT)'
	@echo
	@echo 'Traffic is already flowing. Try: make break-apex'

.PHONY: up-ollama
up-ollama: ## Start the stack with a real local model as the primary provider
	@# Two things the bare `docker compose --profile ollama up` cannot do on its
	@# own: a profile decides which services exist, not what another service's
	@# environment says, so the gateway still has to be told where the upstream
	@# is. Everything here is one variable and the profile.
	@SWITCHYARD_VERSION=$(VERSION) \
		SWITCHYARD_UPSTREAMS='$(OLLAMA_UPSTREAMS)' \
		OLLAMA_MODEL='$(OLLAMA_MODEL)' \
		docker compose --profile ollama up --build -d
	@echo
	@echo 'Grafana:    $(GRAFANA)'
	@echo 'Gateway:    $(GATEWAY)   (primary provider: ollama, model $(OLLAMA_MODEL))'
	@echo
	@echo 'The three simulated providers are still there, behind it, as the'
	@echo 'failover path. Try: make ask, then make break-ollama'

.PHONY: break-ollama
break-ollama: ## Take the real model down for real: stop its container
	@# There is no admin/inject for a real provider, and that is the point --
	@# you cannot reproducibly break somebody else's service, which is the
	@# whole argument for the simulated three. Breaking this one means actually
	@# stopping it.
	@docker compose --profile ollama stop ollama
	@echo 'ollama is gone. Its probe fails, its circuit opens within a few'
	@echo 'seconds, and traffic moves to apex. Watch the breaker panel.'

.PHONY: heal-ollama
heal-ollama: ## Start the real model again and watch its circuit close
	@docker compose --profile ollama start ollama
	@echo 'ollama is back. The breaker will not hand it everything at once:'
	@echo 'watch the admit ratio ramp, exactly as it does for a simulated one.'

.PHONY: down
down: ## Stop the stack and remove its containers
	@# --profile ollama so this also stops the opt-in services when they are
	@# running. The named volume holding the model weights is kept: `down`
	@# should not cost a gigabyte of download to undo.
	@docker compose --profile ollama down --remove-orphans

.PHONY: logs
logs: ## Follow the gateway log
	@docker compose logs -f switchyard

.PHONY: demo
demo: ## The loop: ask, break whoever answered, ask again, see failover
	@bash scripts/demo.sh

.PHONY: break-apex
break-apex: ## Take apex down: watch traffic move and availability hold
	@$(CURL) -X POST $(GATEWAY)/admin/inject \
		-H 'content-type: application/json' \
		-d '{"provider":"apex","mode":"error","rate":1}' | $(FORMAT)
	@echo 'apex is returning 503. Watch the traffic panel: the apex band should'
	@echo 'collapse into bargain within a few seconds, and the total should not dip.'

.PHONY: heal-apex
heal-apex: ## Bring apex back: watch the gradual ramp, not a stampede
	@# Probe-driven early recovery is off by default, so an open circuit serves
	@# its full backoff -- which after repeated trips is tens of seconds of
	@# nothing happening. The demo turns it on here so the heal is visible on
	@# the timescale of a person watching. See DESIGN.md for what that trades.
	@$(CURL) -X POST $(GATEWAY)/admin/recovery \
		-H 'content-type: application/json' \
		-d '{"probe_early_recovery":true}' >/dev/null
	@$(CURL) -X POST $(GATEWAY)/admin/inject \
		-H 'content-type: application/json' \
		-d '{"provider":"apex","mode":"healthy"}' | $(FORMAT)
	@echo 'apex is answering again. The breaker will not hand it all the traffic'
	@echo 'at once: watch the admit ratio climb through the middle before closing.'

.PHONY: ratelimit-bargain
ratelimit-bargain: ## Make bargain return 429s: a healthy provider shedding load
	@$(CURL) -X POST $(GATEWAY)/admin/inject \
		-H 'content-type: application/json' \
		-d '{"provider":"bargain","mode":"ratelimit","rate":1}' | $(FORMAT)
	@echo 'bargain is rate limiting. Note its breaker stays closed: a 429 is a'
	@echo 'working provider shedding our load, not a broken one.'

.PHONY: heal-bargain
heal-bargain: ## Clear the fault on bargain
	@$(CURL) -X POST $(GATEWAY)/admin/inject \
		-H 'content-type: application/json' \
		-d '{"provider":"bargain","mode":"healthy"}' | $(FORMAT)

.PHONY: slow-apex
slow-apex: ## Make apex slow but not broken: the failure a health check misses
	@$(CURL) -X POST $(GATEWAY)/admin/inject \
		-H 'content-type: application/json' \
		-d '{"provider":"apex","mode":"slow","slow_factor":12}' | $(FORMAT)
	@echo 'apex is twelve times slower and still passing its health check.'
	@echo 'Watch the time-to-first-token panel; nothing else will tell you.'

.PHONY: spike-traffic
spike-traffic: ## Triple the offered load: find the edge of the failover capacity
	@$(CURL) -X POST $(GATEWAY)/admin/traffic \
		-H 'content-type: application/json' \
		-d '{"rps":45}' | $(FORMAT)
	@echo 'Offered load is now 45 rps. With apex up this is fine. Combine it with'
	@echo 'make break-apex to see availability fall: failover cannot conjure'
	@echo 'capacity that the surviving providers never had.'

.PHONY: normal-traffic
normal-traffic: ## Return the offered load to its default
	@$(CURL) -X POST $(GATEWAY)/admin/traffic \
		-H 'content-type: application/json' \
		-d '{"rps":10}' | $(FORMAT)

.PHONY: policy-cost
policy-cost: ## Route cheapest-first instead of primary-first
	@$(CURL) -X POST $(GATEWAY)/admin/policy \
		-H 'content-type: application/json' \
		-d '{"policy":"cost"}' | $(FORMAT)

.PHONY: policy-failover
policy-failover: ## Route primary-first (the default)
	@$(CURL) -X POST $(GATEWAY)/admin/policy \
		-H 'content-type: application/json' \
		-d '{"policy":"failover"}' | $(FORMAT)

.PHONY: reset
reset: ## Clear every injected fault, restore the default load and the safe defaults
	@for p in apex bargain local; do \
		$(CURL) -X POST $(GATEWAY)/admin/inject \
			-H 'content-type: application/json' \
			-d "{\"provider\":\"$$p\",\"mode\":\"healthy\"}" >/dev/null; \
	done
	@$(CURL) -X POST $(GATEWAY)/admin/traffic \
		-H 'content-type: application/json' -d '{"rps":10}' >/dev/null
	@$(CURL) -X POST $(GATEWAY)/admin/policy \
		-H 'content-type: application/json' -d '{"policy":"failover"}' >/dev/null
	@$(CURL) -X POST $(GATEWAY)/admin/recovery \
		-H 'content-type: application/json' -d '{"probe_early_recovery":false}' >/dev/null
	@echo 'all providers healthy, load 10 rps, policy failover, early recovery off'

.PHONY: state
state: ## Print the gateway's current routing and health state
	@$(CURL) $(GATEWAY)/admin/state | $(FORMAT)

.PHONY: ask
ask: ## Send one request through the gateway and stream the answer
	@$(CURL) -N -X POST $(GATEWAY)/v1/chat \
		-H 'content-type: application/json' \
		-d '{"prompt":"Why does a gateway need to know the difference between a 429 and a 503?","max_tokens":80}'

# ── checks ────────────────────────────────────────────────────────────────────

.PHONY: build
build: ## Build the binary
	@go build -trimpath -ldflags="-X main.version=$(VERSION)" -o switchyard ./cmd/switchyard

.PHONY: test
test: ## Run the unit tests under the race detector
	@go test -race ./...

.PHONY: lint
lint: ## Run golangci-lint
	@golangci-lint run ./...

.PHONY: vet
vet: ## Run go vet and check formatting
	@go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi

.PHONY: vuln
vuln: ## Check dependencies for known vulnerabilities
	@# Pinned, and the same version CI runs. @latest lets the scanner change
	@# between two runs of the same commit, which turns a green build red with
	@# no diff to point at.
	@go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: leaks
leaks: ## Scan the full history for secrets
	@gitleaks git --no-banner --redact .

.PHONY: identity
identity: ## Verify every commit carries the canonical identity
	@bash scripts/check-identity.sh

.PHONY: e2e
e2e: ## Start the stack, break a provider, and assert from metrics that traffic moved
	@GATEWAY=$(GATEWAY) PROM=http://localhost:$(PROMETHEUS_PORT) bash test/e2e/failover.sh

.PHONY: check
check: vet lint test identity ## Everything CI runs, except the end-to-end test

.PHONY: clean
clean: ## Remove build output
	@rm -f switchyard

# python3 is present on every machine this runs on, including the CI image, and
# a readable JSON response is worth more than avoiding the dependency. If it is
# missing, cat still shows the answer.
FORMAT := $(shell command -v python3 >/dev/null 2>&1 && echo 'python3 -m json.tool' || echo cat)

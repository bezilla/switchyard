# Switchyard
#
# Start here:  make init     (once per clone -- installs the pre-push gate)
#              make up       (the demo)
#
# The failure-injection targets below are the demo. They post to the gateway's
# admin endpoint; nothing about them is privileged, and none of them touch a
# real provider because there are no real providers.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

SWITCHYARD_PORT ?= 8080
GRAFANA_PORT ?= 3000
PROMETHEUS_PORT ?= 9090
export SWITCHYARD_PORT GRAFANA_PORT PROMETHEUS_PORT

GATEWAY ?= http://localhost:$(SWITCHYARD_PORT)
GRAFANA ?= http://localhost:$(GRAFANA_PORT)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# curl, quiet, failing the make target on an HTTP error rather than printing
# the error body and carrying on.
CURL := curl --silent --show-error --fail-with-body

.PHONY: help
help: ## Show this help
	@echo 'Switchyard -- an AI provider gateway that survives its providers'
	@echo
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*21659' $(MAKEFILE_LIST) \
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

.PHONY: down
down: ## Stop the stack and remove its containers
	@docker compose down --remove-orphans

.PHONY: logs
logs: ## Follow the gateway log
	@docker compose logs -f switchyard

.PHONY: break-apex
break-apex: ## Take apex down: watch traffic move and availability hold
	@$(CURL) -X POST $(GATEWAY)/admin/inject \
		-H 'content-type: application/json' \
		-d '{"provider":"apex","mode":"error","rate":1}' | $(FORMAT)
	@echo 'apex is returning 503. Watch the traffic panel: the apex band should'
	@echo 'collapse into bargain within a few seconds, and the total should not dip.'

.PHONY: heal-apex
heal-apex: ## Bring apex back: watch the gradual ramp, not a stampede
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
reset: ## Clear every injected fault and restore the default load
	@for p in apex bargain local; do \
		$(CURL) -X POST $(GATEWAY)/admin/inject \
			-H 'content-type: application/json' \
			-d "{\"provider\":\"$$p\",\"mode\":\"healthy\"}" >/dev/null; \
	done
	@$(CURL) -X POST $(GATEWAY)/admin/traffic \
		-H 'content-type: application/json' -d '{"rps":10}' >/dev/null
	@$(CURL) -X POST $(GATEWAY)/admin/policy \
		-H 'content-type: application/json' -d '{"policy":"failover"}' >/dev/null
	@echo 'all providers healthy, load 10 rps, policy failover'

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
	@go run golang.org/x/vuln/cmd/govulncheck@latest ./...

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

BINARY  = tf-agent-server
VERSION = $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -ldflags "-X main.Version=$(VERSION) -s -w"
LOCALBIN = .bin

# ── Infra containers ──────────────────────────────────────────────────────────
PG_CONTAINER  = tf-agent-postgres
PG_PORT       = 5432
PG_DB         = tfagent
PG_USER       = tfagent
PG_PASS       = tfagent

NATS_CONTAINER = tf-agent-nats
NATS_PORT      = 4222
NATS_MON_PORT  = 8222

# ?= lets an environment variable (or command-line override) take precedence
DB_URL   ?= postgres://$(PG_USER):$(PG_PASS)@localhost:$(PG_PORT)/$(PG_DB)?sslmode=disable
NATS_URL ?= nats://localhost:$(NATS_PORT)
export DB_URL
export NATS_URL

.PHONY: build build-server build-ui dev dev-ui run-server run test test-unit test-unit-v test-integration test-all lint vuln clean install tidy doctor infra infra-stop infra-status infra-clean

## build — builds UI + server binary
build: build-ui build-server

## build-server — compiles the Go server → .bin/tf-agent-server (embeds client/dist)
build-server: build-ui
	@mkdir -p $(LOCALBIN)
	@mkdir -p cmd/server/web
	@cp -r client/dist/. cmd/server/web/
	go build $(LDFLAGS) -o $(LOCALBIN)/$(BINARY) ./cmd/server
	@rm -rf cmd/server/web
	@echo "→ $(LOCALBIN)/$(BINARY)"

## build-ui — compiles the React client → client/dist/
build-ui:
	cd client && npm install && npm run build
	@echo "→ client/dist/"

## dev — runs server + React dev server in parallel
dev:
	@trap 'kill 0' SIGINT; \
	make run-server & \
	make dev-ui & \
	wait

## dev-ui — React dev server on :3000 with hot reload (proxies /v1 → :8080)
dev-ui:
	cd client && npm install && npm run dev

## run-server — runs the Go server directly (Postgres, memory queue)
run-server:
	go run ./cmd/server $(ARGS)

## run — alias for run-server
run: run-server

## test — alias for test-unit
test: test-unit

## test-unit — runs unit tests only (no external dependencies)
test-unit:
	go test ./... -count=1 -timeout 60s

## test-unit-v — unit tests with verbose output
test-unit-v:
	go test ./... -count=1 -v -timeout 60s

## test-integration — runs integration tests (requires running infra: make infra)
test-integration:
	@echo "Running integration tests against Postgres + NATS..."
	DB_URL=$(DB_URL) NATS_URL=$(NATS_URL) \
	  go test -tags=integration ./... -count=1 -timeout 120s -v

## test-all — runs unit tests then integration tests
test-all: test-unit test-integration

## lint — run go vet on all packages
lint:
	go vet ./...

## vuln — scan for known vulnerabilities (requires: go install golang.org/x/vuln/cmd/govulncheck@latest)
vuln:
	govulncheck ./...

## clean — remove build artifacts (preserves node_modules)
clean:
	rm -rf $(LOCALBIN) client/dist

## install — install server binary to GOPATH/bin
install:
	go install $(LDFLAGS) ./cmd/server

## tidy — tidy go.mod and go.sum
tidy:
	go mod tidy

## infra — start Postgres + NATS Docker containers for local dev / integration tests
infra:
	@echo "Starting Postgres $(PG_CONTAINER)..."
	@docker run -d --name $(PG_CONTAINER) \
	  -e POSTGRES_DB=$(PG_DB) \
	  -e POSTGRES_USER=$(PG_USER) \
	  -e POSTGRES_PASSWORD=$(PG_PASS) \
	  -p $(PG_PORT):5432 \
	  -v tf-agent-postgres-data:/var/lib/postgresql/data \
	  --restart unless-stopped \
	  postgres:16-alpine 2>/dev/null || docker start $(PG_CONTAINER)
	@echo "Starting NATS $(NATS_CONTAINER) with JetStream..."
	@docker run -d --name $(NATS_CONTAINER) \
	  -p $(NATS_PORT):4222 \
	  -p $(NATS_MON_PORT):8222 \
	  --restart unless-stopped \
	  nats:2.10-alpine -js -m 8222 2>/dev/null || docker start $(NATS_CONTAINER)
	@echo "Waiting for Postgres to be ready..."
	@until docker exec $(PG_CONTAINER) pg_isready -U $(PG_USER) -d $(PG_DB) >/dev/null 2>&1; do \
	  printf '.'; sleep 1; \
	done
	@echo ""
	@echo "✓ Postgres  :$(PG_PORT)   ($(PG_CONTAINER))"
	@echo "✓ NATS      :$(NATS_PORT)   ($(NATS_CONTAINER)) — monitor: http://localhost:$(NATS_MON_PORT)"
	@echo ""
	@echo "To run with Postgres + NATS:"
	@echo "  DB_DRIVER=postgres DB_URL='$(DB_URL)' QUEUE_DRIVER=nats NATS_URL=$(NATS_URL) make run-server"
	@echo ""
	@echo "To run integration tests:"
	@echo "  make test-integration"

## infra-stop — stop infra containers (data is preserved)
infra-stop:
	@docker stop $(PG_CONTAINER) $(NATS_CONTAINER) 2>/dev/null || true
	@echo "✓ Infra containers stopped (data preserved)"

## infra-status — show running state of infra containers
infra-status:
	@echo "=== tf-agent infra status ==="
	@docker ps -a --filter name=$(PG_CONTAINER) --filter name=$(NATS_CONTAINER) \
	  --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"

## infra-clean — remove infra containers AND volumes (destructive)
infra-clean:
	@docker rm -f $(PG_CONTAINER) $(NATS_CONTAINER) 2>/dev/null || true
	@docker volume rm tf-agent-postgres-data 2>/dev/null || true
	@echo "✓ Infra containers and volumes removed"

# Doctor 
doctor:
	@echo "=== tf-agent doctor ==="
	@echo -n "Go:            "; go version
	@echo -n "Node:          "; node --version 2>/dev/null || echo "not found (required for UI)"
	@echo -n "Docker:        "; docker --version 2>/dev/null || echo "not found (required for make infra)"
	@echo -n "ANTHROPIC_KEY: "; [ -n "$$ANTHROPIC_API_KEY" ] && echo "set" || echo "NOT SET"
	@echo -n "AWS creds:     "; aws sts get-caller-identity --query Account --output text 2>/dev/null || echo "not configured"
	@echo -n "govulncheck:   "; which govulncheck 2>/dev/null || echo "not found (go install golang.org/x/vuln/cmd/govulncheck@latest)"
	@echo -n "terraform:     "; which terraform 2>/dev/null || echo "not found"
	@echo -n "tflint:        "; which tflint 2>/dev/null || echo "not found"
	@echo -n "checkov:       "; which checkov 2>/dev/null || echo "not found (pip install checkov)"
	@echo -n "GITHUB_TOKEN:  "; [ -n "$$GITHUB_TOKEN" ] && echo "set" || echo "not set"
	@echo -n "Postgres:      "; docker exec $(PG_CONTAINER) pg_isready -U $(PG_USER) 2>/dev/null && echo "running" || echo "not running (make infra)"
	@echo -n "NATS:          "; docker inspect -f '{{.State.Status}}' $(NATS_CONTAINER) 2>/dev/null || echo "not running (make infra)"
	@echo "=== done ==="

.PHONY: help test test-race test-cover test-eval bench lint tidy \
        example-sql example-yaml example-demo example-media example-creative eval-example \
        build clean

# ── colours ──────────────────────────────────────────────────────────────────
GREEN  := \033[0;32m
YELLOW := \033[0;33m
CYAN   := \033[0;36m
RESET  := \033[0m

## help: show this message
help:
	@echo ""
	@echo "$(CYAN)GopherAgent$(RESET) — available targets:"
	@echo ""
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/{printf "  $(GREEN)%-20s$(RESET) %s\n",$$1,$$2}' $(MAKEFILE_LIST)
	@echo ""

# ── examples ─────────────────────────────────────────────────────────────────

## example-sql: run multi-agent SQL analytics example
example-sql: ## run multi-agent SQL analytics example
	cd examples/multi_agent_data && go run .

## example-yaml: run YAML-driven dynamic builder example
example-yaml: ## run YAML-driven dynamic builder example
	cd examples/dynamic_builder && go run .

## example-demo: run SSE streaming web chat demo
example-demo: ## run SSE streaming web chat demo
	cd examples/demo && go run .

## example-media: run media chat demo (upload images/docs, ask questions)
example-media: ## run media chat demo
	cd examples/media_chat && go run .

## example-creative: run AI Creative Studio (generate images with DALL-E 3 and videos with Veo 2)
example-creative: ## run AI creative studio
	cd examples/creative_studio && go run .

# ── testing ───────────────────────────────────────────────────────────────────

## test: run all unit tests
test: ## run all unit tests
	go test ./... -count=1

## test-race: run tests with race detector
test-race: ## run tests with race detector
	go test ./... -race -count=1

## test-cover: run tests and open HTML coverage report
test-cover: ## run tests and open HTML coverage report
	go test ./... -coverprofile=coverage.out -covermode=atomic
	go tool cover -html=coverage.out -o coverage.html
	@echo "$(GREEN)Coverage report:$(RESET) coverage.html"
	@open coverage.html 2>/dev/null || xdg-open coverage.html 2>/dev/null || true

## test-builder: run only builder/YAML validation tests
test-builder: ## run only builder/YAML validation tests
	go test ./pkg/builder/ -v -count=1

## test-agent: run only core agent loop tests
test-agent: ## run only core agent loop tests
	go test ./pkg/agent/ -v -count=1

## test-eval: run only the eval harness tests
test-eval: ## run only the eval harness tests
	go test ./pkg/eval/ -v -count=1

## eval-example: run the deterministic example eval suite (no API keys)
eval-example: ## run the deterministic example eval suite
	go test ./examples/agent_eval/ -v -count=1

# ── benchmarks ────────────────────────────────────────────────────────────────

## bench: run all benchmarks
bench: ## run all benchmarks
	go test ./... -run='^$$' -bench=. -benchmem

## bench-cache: benchmark trigram cache only
bench-cache: ## benchmark trigram cache only
	go test ./pkg/cache/ -run='^$$' -bench=. -benchmem -v

## bench-agent: benchmark agent hot paths (pruning, anti-loop, middleware)
bench-agent: ## benchmark agent hot paths (pruning, anti-loop, middleware)
	go test ./pkg/agent/ ./pkg/tools/ -run='^$$' -bench=. -benchmem -v

# ── code quality ──────────────────────────────────────────────────────────────

## lint: run golangci-lint (requires: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
lint: ## run golangci-lint
	@which golangci-lint > /dev/null || (echo "$(YELLOW)golangci-lint not found. Install:$(RESET) go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run ./...

## vet: run go vet
vet: ## run go vet
	go vet ./...

## tidy: tidy and verify go.mod / go.sum
tidy: ## tidy and verify go.mod / go.sum
	go mod tidy
	go mod verify

# ── build ─────────────────────────────────────────────────────────────────────

## build: compile all packages (catches errors without running)
build: ## compile all packages
	go build ./...

# ── cleanup ───────────────────────────────────────────────────────────────────

## clean: remove generated files
clean: ## remove generated files
	rm -f coverage.out coverage.html

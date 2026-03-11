VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MODULE    = github.com/gonkalabs/gonka-agent
LDFLAGS   = -s -w \
            -X $(MODULE)/internal/version.Version=$(VERSION) \
            -X $(MODULE)/internal/version.Commit=$(COMMIT) \
            -X $(MODULE)/internal/version.Date=$(DATE)

PLATFORMS = linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
BIN_DIR   = dist

.PHONY: build dev install clean release checksum test vet doctor \
        build-voice build-slim seed-dev seed-research seed-bot \
        release-full docker docker-run help

# ─── Development ─────────────────────────────────────────────────────────────

dev: ## Build for current platform (fast, with debug info)
	go build -o bin/gonka ./cmd/gonka

build: ## Production build for current platform (stripped, no CGO)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gonka ./cmd/gonka

build-voice: ## Production build with voice support (CGO, requires whisper.cpp)
	CGO_ENABLED=1 go build -trimpath -tags voice -ldflags "$(LDFLAGS)" -o bin/gonka ./cmd/gonka

build-slim: ## Minimal build without TUI/n8n (pipeline/bot use)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -tags slim -o bin/gonka-slim ./cmd/gonka

install: build ## Install to $GOPATH/bin
	cp bin/gonka $(shell go env GOPATH)/bin/gonka

# ─── Testing & Analysis ──────────────────────────────────────────────────────

test: ## Run tests with race detector + vet
	go vet ./...
	go test -race -count=1 -timeout=120s ./...

vet: ## Static analysis
	go vet ./...

doctor: build ## Run self-diagnostics
	./bin/gonka doctor

# ─── Seed Patterns ───────────────────────────────────────────────────────────

seed-dev: ## Bake developer seed patterns into binary
	@echo "  SEED   developer patterns"
	@mkdir -p internal/profile/seeds
	@echo '{"role":"developer","version":"$(VERSION)"}' > internal/profile/seeds/developer.json

seed-research: ## Bake researcher seed patterns into binary
	@echo "  SEED   researcher patterns"
	@mkdir -p internal/profile/seeds
	@echo '{"role":"researcher","version":"$(VERSION)"}' > internal/profile/seeds/researcher.json

seed-bot: ## Bake bot seed patterns into binary
	@echo "  SEED   bot patterns"
	@mkdir -p internal/profile/seeds
	@echo '{"role":"bot","version":"$(VERSION)"}' > internal/profile/seeds/bot.json

# ─── Release: cross-compile for all platforms ─────────────────────────────────

release: clean ## Build release binaries for all platforms
	@mkdir -p $(BIN_DIR)
	@for platform in $(PLATFORMS); do \
		os=$$(echo $$platform | cut -d/ -f1); \
		arch=$$(echo $$platform | cut -d/ -f2); \
		out=$(BIN_DIR)/gonka-$$os-$$arch; \
		[ "$$os" = "windows" ] && out="$$out.exe"; \
		echo "  BUILD  $$os/$$arch → $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/gonka; \
	done
	@$(MAKE) checksum

release-full: seed-dev seed-research seed-bot release ## Release with all seed patterns baked in

checksum: ## Generate SHA256 checksums for all binaries in dist/
	@cd $(BIN_DIR) && sha256sum gonka-* > checksums.txt
	@echo "  SHA256  $(BIN_DIR)/checksums.txt"
	@cat $(BIN_DIR)/checksums.txt

# ─── GitHub Release ──────────────────────────────────────────────────────────

gh-release: release-full ## Create GitHub Release with all binaries
	@echo "  RELEASE  $(VERSION)"
	gh release create $(VERSION) $(BIN_DIR)/gonka-* $(BIN_DIR)/checksums.txt \
		--title "Gonka Agent $(VERSION)" \
		--notes "Release $(VERSION) — $(DATE)"

clean: ## Remove build artifacts
	rm -rf bin/gonka bin/gonka-slim $(BIN_DIR)

# ─── Docker ──────────────────────────────────────────────────────────────────

docker: ## Build multi-stage Docker image
	docker build -t gonka-agent:$(VERSION) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-f Dockerfile .

docker-run: ## Run agent in Docker with opengnk signing proxy
	docker compose run --rm agent

# ─── Help ────────────────────────────────────────────────────────────────────

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[38;2;0;212;170m%-18s\033[0m %s\n", $$1, $$2}'

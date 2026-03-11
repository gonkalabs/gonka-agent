# ──────────────────────────────────────────────────────────────────────────────
# Gonka Agent — Multi-stage Docker build
# ──────────────────────────────────────────────────────────────────────────────

# Stage 1: Build Go binary
FROM golang:1.24-bookworm AS builder

ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/gonkalabs/gonka-agent/internal/version.Version=${VERSION} \
      -X github.com/gonkalabs/gonka-agent/internal/version.Commit=${COMMIT} \
      -X github.com/gonkalabs/gonka-agent/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o bin/gonka ./cmd/gonka

# Stage 2: Runtime with fastembed + chromium for web tools
FROM python:3.11-slim-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium git ca-certificates curl jq ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir fastembed==0.4.1

COPY --from=builder /build/bin/gonka /usr/local/bin/gonka

COPY scripts/embed-server.py /opt/embed-server.py
COPY scripts/docker-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

COPY .env.example /app/.env.example

WORKDIR /workspace

ENV AGENT_WORKSPACE=/workspace \
    AGENT_EMBED_URL=http://localhost:8001/v1 \
    AGENT_EMBED_MODEL=all-MiniLM-L6-v2 \
    GONKA_DIRECT_URL=http://opengnk:8090/v1 \
    GONKA_SOURCE_URL=http://opengnk:8090/v1 \
    AGENT_ALLOW_SHELL=true \
    AGENT_COMMAND_TIMEOUT=120s \
    CHROME_BIN=/usr/bin/chromium \
    CHROME_FLAGS="--headless --no-sandbox --disable-gpu"

ENTRYPOINT ["/entrypoint.sh"]

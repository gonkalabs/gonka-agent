# Stage 1: build Go binary
FROM golang:1.22-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/gonka ./cmd/gonka

# Stage 2: runtime with fastembed for local embeddings
FROM python:3.11-slim-bookworm

# Install fastembed (CPU-only, all-MiniLM-L6-v2, ~60MB)
RUN pip install --no-cache-dir fastembed==0.4.1

# Copy agent binary
COPY --from=builder /build/bin/gonka /usr/local/bin/gonka

# Copy embed server and entrypoint
COPY scripts/embed-server.py /opt/embed-server.py
COPY scripts/docker-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Config template
COPY .env.example /app/.env.example

WORKDIR /workspace

# GONKA_API_KEY must be set at runtime.
# AGENT_WORKSPACE defaults to /workspace (mount your project here).
ENV AGENT_WORKSPACE=/workspace \
    AGENT_EMBED_URL=http://localhost:8001/v1 \
    AGENT_EMBED_MODEL=all-MiniLM-L6-v2 \
    GONKA_DIRECT_URL=http://localhost:9090/v1 \
    GONKA_SOURCE_URL=https://gonka.gg/api/public \
    AGENT_ALLOW_SHELL=true \
    AGENT_COMMAND_TIMEOUT=120s

ENTRYPOINT ["/entrypoint.sh"]

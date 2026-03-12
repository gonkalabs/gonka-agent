# gonka-agent · Binary Singularity

AI coding agent running on the [Gonka](https://github.com/gonka-ai/gonka) decentralised inference network.

```bash
gonka "deploy 12 nginx nodes in a mesh and generate the config"
```

Branch: `dev/binary-singularity` — Part of [GiP #860](https://github.com/gonka-ai/gonka/discussions/860)

---

## What's in this repo

| Path | Description |
|---|---|
| [`gonka-agent/`](gonka-agent/) | The agent binary — CLI, TUI, n8n, voice, web, skills |
| [`bridge.py`](bridge.py) | OpenAI-compatible HTTP proxy that signs DAPI requests with a Gonka wallet |
| [`check_inference.py`](check_inference.py) | Quick smoke-test for DAPI connectivity |
| [`gonka-main/`](gonka-main/) | Submodule — deploy guides, dev notes, examples |

---

## Quick start — docker compose

```bash
git clone https://github.com/gonkalabs/gonka-agent
cd gonka-agent
cp .env.example .env          # set GONKA_PRIVATE_KEY (hex, Keplr / inferenced)

docker compose run --rm agent "your task here"
```

The compose stack boots the `bridge.py` signing proxy automatically.
No DAPI node registration, no GonkaGate needed.

## Quick start — prebuilt binary

```bash
# 1. Start the signing proxy (needs your wallet key)
export GONKA_PRIVATE_KEY=<hex>
uvicorn bridge:app --host 0.0.0.0 --port 8090

# 2. Run the agent
./gonka-agent/bin/gonka "your task"
```

## Quick start — build from source

```bash
cd gonka-agent
go build -o bin/gonka ./cmd/gonka
./bin/gonka "your task"
```

---

## Inference priority

```
1. Gonka DAPI  (primary — signed by your wallet, contributes quality signals)
2. OpenRouter  (overflow — fast tool-call turnaround, non-critical streams)
3. Ollama      (local fallback — offline / privacy mode)
```

Set in `.env`:

```env
GONKA_API_URL=http://localhost:8090/v1
GONKA_API_KEY=gonka
GONKA_MODEL=Qwen/Qwen3-235B-A22B-Instruct-2507-FP8

OPENROUTER_API_KEY=sk-or-v1-...
OPENROUTER_MODEL=qwen/qwen3-235b-a22b
```

---

## UI choice on first run

```
gonka --tui      # Rich terminal UI — full keyboard navigation, live streaming
gonka --n8n      # n8n workflow engine — pipelines, webhooks, visual editor
gonka "task"     # Headless CLI — pipe-friendly, CI/CD
```

`--n8n` spins up an n8n Docker container with a persistent volume.
After the one-time setup the UI loads directly on every subsequent launch.

---

## Key features (binary-singularity branch)

- **Multi-provider inference** — Gonka DAPI primary, OpenRouter overflow, Ollama local; per-request timeouts + automatic fallback
- **Bulletproof web** — `web_fetch` with retry + custom headers; `web_search` via SearXNG self-hosted → DuckDuckGo HTML fallback
- **Role seed patterns** — developer / researcher / bot profiles embedded in the binary (`go:embed`), injected into every system prompt
- **PatternSlot store** — every solved task distilled to a binary pattern; cosine search injects context before the LLM call
- **n8n out-of-the-box** — `gonka --n8n` deploys `n8nio/n8n:1.72.1` via Docker with persistent volume, no registration screen after first setup
- **TUI** — turquoise-blue-metallic theme, adaptive layout, full keybindings
- **Voice input** — `whisper.cpp` baked into the binary; any language, CPU-only
- **Skill packs** — `git_ops`, `dev_ops`, `data_ops` extend tools and prompts per role
- **Self-update** — `gonka update` pulls the latest binary from GitHub Releases
- **Benchmark** — `gonka bench` measures tokens/s, slot hit rate, latency, resource usage
- **Doctor** — `gonka doctor` runs component self-tests and reports health

---

## Signing proxy (bridge.py)

Required when using Gonka DAPI directly — every inference request must be
signed with a Cosmos wallet key. The proxy exposes a standard OpenAI-compatible
endpoint so the agent (and any other client) works unchanged.

```bash
export GONKA_PRIVATE_KEY=<hex key from Gonka wallet>
pip install -r requirements.txt
uvicorn bridge:app --host 0.0.0.0 --port 8090
```

Test DAPI connectivity:

```bash
python3 check_inference.py
```

Key comes from your Gonka wallet (Keplr / inferenced). GNK — bounty or Host tier.

---

## Quality signals

Every task the agent runs contributes to GiP #860:

- **L4** `X-Inference-Feedback: resolved/unresolved` on every inference  
- **L6** cache hit rate tracked by quality-middleware  
- **L8** latency CV per session  
- **L9** completion rate  

Measured result: **PQM = 1.020** on 11,520 runs — binary pattern layer exceeds
single GPU inference on every measured axis.

---

*Binary Singularity — dev/binary-singularity.*  
*Your tasks, your patterns, your contribution to the mesh.*

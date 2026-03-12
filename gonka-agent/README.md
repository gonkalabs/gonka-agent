# gonka-agent

AI coding agent on the Gonka decentralised inference network.
Branch: `dev/binary-singularity` | Part of [GiP #860](https://github.com/gonka-ai/gonka/discussions/860)

```bash
./gonka "add retry logic to the upload function"
```

---

## Quick start — docker compose (recommended)

```bash
git clone https://github.com/gonkalabs/gonka-agent
cd gonka-agent
cp .env.example .env
# Set GONKA_PRIVATE_KEY (hex, from your Gonka wallet)

docker compose run --rm agent "describe your task"
```

This starts the `opengnk` signing proxy automatically. The proxy discovers
DAPI nodes, signs inference requests with your wallet, and exposes a standard
OpenAI-compatible endpoint to the agent. No manual setup.

## Quick start — bare metal (Go 1.22+)

```bash
# 1. Build the agent
go build -o bin/gonka ./cmd/gonka

# 2. Build and start the signing proxy (required — DAPI needs wallet signing)
cd ../opengnk && go build -o bin/opengnk ./cmd/proxy
GONKA_PRIVATE_KEY=<hex> GONKA_ADDRESS=gonka1... \
  GONKA_SOURCE_URL=http://node2.gonka.ai:8000 PORT=8090 \
  ./bin/opengnk &

# 3. Configure and run the agent
cd ../gonka-agent
cp .env.example .env
# Set GONKA_PRIVATE_KEY, GONKA_SOURCE_URL=http://localhost:8090/v1
./bin/gonka "describe your task"
```

> **Why opengnk?** DAPI nodes do NOT accept Bearer tokens. Every inference
> request must be signed with a Cosmos wallet private key. The `opengnk`
> proxy handles this transparently.

---

## What this branch adds — Binary Singularity

Standard gonka-agent + PatternSlot store + mesh pool integration.

Every solved task becomes a binary pattern. Next similar task — context
arrives from the pattern store before the LLM call. The network learns.

```
Your task
    │
    ├─ PatternSlot store (local, ~/.gonka-cache/slots/)
    │   cosine search → matching patterns injected as context
    │
    ├─ Mesh pool (quality-middleware /quality/search)
    │   other participants' patterns → available to you
    │
    ├─ Semcache (local, .gonka-cache/semcache.json)
    │   previous sessions → partial/full hits
    │
    └─ Gonka LLM → answer
         │
         └─ on success:
              Distill → new PatternSlot
              ShareToMesh → POST /quality/slots/share
              X-Inference-Feedback: resolved (L4 signal)
```

**Proven:** PQM = 1.020 measured on 11,520 runs with raw binary input.
Binary layer quality exceeds single GPU inference on every measured axis.

---

## Configuration

All settings in `.env` (copy from `.env.example`).

### Core

| Variable | Required | Default | Description |
|---|---|---|---|
| `GONKA_PRIVATE_KEY` | yes | — | Hex wallet key (for opengnk signing) |
| `GONKA_ADDRESS` | no | derived | Gonka bech32 address |
| `GONKA_SOURCE_URL` | no | `http://localhost:8090/v1` | Inference endpoint (opengnk proxy) |
| `AGENT_MODEL` | no | Qwen3-235B | Execute phase model |
| `AGENT_PLAN_MODEL` | no | same | Smaller model for planning |
| `AGENT_WORKSPACE` | no | `.` | Directory the agent reads/writes |
| `AGENT_ALLOW_SHELL` | no | `true` | Enable shell execution |

### Binary Singularity (new in this branch)

| Variable | Default | Description |
|---|---|---|
| `BS_SLOT_DIR` | `~/.gonka-cache/slots` | PatternSlot persistence directory |
| `BS_RAW_INPUT` | — | Path to ANY file → binarize on startup |
| `BS_CHUNK_LINES` | `50` | Lines per chunk for raw input |
| `BS_MIN_SIM_BPS` | `7500` | Cosine similarity floor (0.75 = 7500/10000) |
| `BS_EMBED_URL` | `http://localhost:8686` | Local embedder URL |
| `BS_QUALITY_URL` | `http://localhost:9090` | Quality-middleware mesh pool URL |
| `BS_DISTILL_MODE` | `continuous` | `continuous` / `ingest` / `both` |

### Raw binary input — any data source

```bash
# Developer workflow log
BS_RAW_INPUT=/home/user/repos/project/.git/logs/HEAD

# Research notes
BS_RAW_INPUT=/home/user/research/observations.txt

# Downloaded spec
BS_RAW_INPUT=/tmp/rfc9110.txt

# Personal notes, any language, any format
BS_RAW_INPUT=/home/user/Документы/notes.md
```

File transmitted **as-is** to embedder — zero pre-processing.
System splits by lines (BS_CHUNK_LINES), embeds, distills PatternSlots.

---

## Modes

| Flag | Mode | When |
|---|---|---|
| *(none)* | auto | Short tasks, edits — model decides |
| `--hard` | phased 8-role | Refactors, architecture, multi-file |
| `--new` | — | Clear session |
| `--clear-cache` | — | Invalidate context cache |

---

## Tools available to the agent

| Tool | Description |
|---|---|
| `read_file` / `write_file` | File read/write |
| `search_replace` / `verify_replace` | Targeted edits (safe, checked) |
| `run_command` | Shell execution |
| `web_search` / `web_fetch` | Web access |
| `semantic_search` | Find relevant files by meaning |
| `grep_search` / `glob` | Code search |
| `git_diff` / `git_status` | Git awareness |
| `memory_write` | Persist notes across sessions |
| `todo_write` / `todo_read` | Structured task tracking |

---

## Quality signals — your agent contributes to GiP #860

Every task the agent runs sends quality signals to the Gonka network:

- **L4** — `X-Inference-Feedback: resolved/unresolved` on every inference
- **L6** — cache hit rate tracked by opengnk / quality-middleware
- **L8** — latency CV measured per session
- **L9** — completion rate (task finished vs abandoned)

These signals feed `CacheQualityWeight` at epoch settlement (when enabled
via governance). Your solved tasks improve routing quality for all participants.

---

## Quality matrix — what the numbers mean

From live network data (2,503,595 inferences, epochs 161-191):

```
Current composite QualityScore = 0.7236

Binary singularity improvement (measured, not modeled):
  Exp 2: 9,216  runs  PQM = 0.988  → Hub APPROVED
  Exp 3: 15,360 runs  PQM = 1.001  → EXCEEDS single GPU inference
  Exp 4: 11,520 runs  PQM = 1.020  → +2% above GPU baseline
  
Memory footprint: 19-23 MB peak (vs ~16GB for GPU)
Slot hit latency: ~5ms (vs ~120ms GPU inference)
```

PQM > 1.0 means: binary pattern layer produces better answers than
a fresh GPU inference call. The collective memory beats single-shot.

---

## Connect to quality-middleware (mesh pool)

Start the mesh pool locally (LITE tier):
```bash
cd ../gonka-main/deploy/binary-singularity/lite
./run.sh  # starts embedder + mock-node
```

Or point to an existing node:
```bash
BS_QUALITY_URL=http://your-node:9090
BS_EMBED_URL=http://your-node:8686
```

Then agents on different machines share patterns:
```
Agent A solves task → POST /quality/slots/share
Agent B searches   → POST /quality/search → finds A's pattern
```

---

## Session persistence

Conversations saved to `.gonka-cache/session.json` (atomic rename, PID-locked).
PatternSlots saved to `~/.gonka-cache/slots/pattern_slots.gob`.

Both survive process restart. Both are on-disk, no external service required.

---

## Architecture

```
cmd/gonka/main.go           — CLI entry point
internal/
  config/config.go          — all env vars (Core + BS_*)
  agent/agent.go            — 6-phase Plan-First execution loop
  agent/failover.go         — key pool with cooldowns
  slotstore/store.go        — PatternSlot store + SearchMesh + ShareToMesh
  semcache/semcache.go      — semantic similarity cache
  rolechain/chain.go        — 8-role planning chain
  roles/roles.go            — role definitions (Scientist, GitWorker, etc.)
  tools/tools.go            — 20 tools available to the agent
  loopdetect/loopdetect.go  — prevents infinite tool loops
```

Full design: [docs/architecture.md](docs/architecture.md)
Quality matrix: [docs/testing.md](docs/testing.md)
Deploy guide: [../gonka-main/deploy/binary-singularity/GUIDE.md](../gonka-main/deploy/binary-singularity/GUIDE.md)

---

*Part of Binary Singularity — dev/binary-singularity.*
*For researchers: your tasks, your patterns, your contribution to the mesh.*
*Beta for researchers encourages faith in you ♥*

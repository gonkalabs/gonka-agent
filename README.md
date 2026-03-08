# gonka-agent

AI coding agent built on the Gonka inference network.

> **Community project** — built and maintained by [A. Mayveskii](https://github.com/Mayveskii). Reviewed and supported by [GonkaLabs](https://gonkalabs.com).

```
gonka "add retry logic to the upload function"
```

---

## Quick start

```bash
cp .env.example .env
# fill in GONKA_API_KEY
go build -o bin/gonka ./cmd/gonka
./bin/gonka "your task"
```

Get an API key at [proxy.gonka.gg](https://proxy.gonka.gg).

---

## Configuration

All settings live in `.env` (copy from `.env.example`).

| Variable | Required | Description |
|---|---|---|
| `GONKA_API_KEY` | yes | Primary API key (`gnk_live_…`) |
| `GONKA_API_KEYS` | no | Comma-separated pool — parallel planning |
| `GONKA_SOURCE_URL` | no | Inference endpoint (default: `https://proxy.gonka.gg/api/public`) |
| `GONKA_DIRECT_URL` | no | Direct node URL — bypasses routing |
| `AGENT_MODEL` | no | Model for execute phase |
| `AGENT_PLAN_MODEL` | no | Smaller model for planning roles |
| `AGENT_WORKSPACE` | no | Directory to work in (default: `.`) |
| `AGENT_ALLOW_SHELL` | no | Enable `run_command` tool (default: `true`) |
| `AGENT_COMMAND_TIMEOUT` | no | Shell timeout (default: `60s`) |
| `AGENT_WEB_SEARCH_KEY` | no | Brave Search key for `web_search` |
| `SEMCACHE_MAX_ENTRIES` | no | Semantic cache size (default: 1000) |

---

## Modes

| Flag | Mode | When to use |
|---|---|---|
| *(none)* | simple | Short tasks, quick edits |
| `--hard` | phased | Refactors, architecture, multi-file |
| `--new` | — | Clear session, start fresh |
| `--clear-cache` | — | Invalidate context cache |

---

## How it works

```
user task
   │
   ▼
semcache lookup ──── FULL HIT ──► return cached answer (agent verifies)
   │ MISS/PARTIAL                 ▲
   ▼                              │
system prompt (+ partial context) │
   │                              │
   ▼                              │
6-phase Plan-First Chain          │
   Plan → Implement → Verify ─────┘
   │
   ▼
semcache.Store(task, steps, answer)
X-Inference-Feedback → opengnk → Gonka quality metrics
```

Each tool call is monitored by the loop detector — the agent is stopped
early if it repeats the same operation without progress.

---

## Tools

| Tool | Description |
|---|---|
| `read_file` | Read a file in the workspace |
| `write_file` | Write or overwrite a file |
| `run_command` | Execute a shell command |
| `web_search` | Search the web (Brave / SearXNG) |
| `web_fetch` | Fetch a URL |
| `semantic_search` | Find relevant files by meaning (uses Gonka `/v1/embeddings`) |
| `memory_write` | Persist agent notes across turns |
| `todo_write` / `todo_read` | Manage a structured task list |

---

## Session persistence

Completed conversations are saved to `.gonka-cache/session.json`.
The file is written atomically (rename) under a PID-checked write lock —
safe for multiple concurrent agent instances on the same workspace.

Run `gonka --new` to start a fresh session.

---

## Semantic cache

The agent stores each solved task locally in `.gonka-cache/semcache.json`
as an embedding + steps + answer.  On the next similar task:

- **score ≥ 0.95** — inject previous answer; agent verifies and adapts.
- **score 0.75–0.94** — inject previous steps as context; agent continues.
- **score < 0.75** — solve from scratch, store result.

Quality scores (0–1) are updated from outcome feedback (`resolved` / `unresolved`)
and fed back to Gonka opengnk as `X-Inference-Feedback` for GiP #860 metrics.

---

## Architecture

See [docs/architecture.md](docs/architecture.md) for the full system design
including the decentralised semantic cache, key pool, loop detection, and
opengnk L1 cache.

## Testing

See [docs/testing.md](docs/testing.md) for the GiP #860 / PR #859 test matrix.

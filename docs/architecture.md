# gonka-agent — Architecture

## Overview

```
┌─────────────────────────────────────────────────────┐
│                   gonka-agent                       │
│                                                     │
│  cmd/gonka ──► semcache.Lookup                      │
│                    │                                │
│                    ▼                                │
│             agent.RunWithHistory                    │
│             ┌─────────────────┐                    │
│             │   runLoop       │                    │
│             │  loopdetect.Detect                   │
│             │  Client.chat ───► opengnk             │
│             │  dispatchWithState                   │
│             └─────────────────┘                    │
│                    │                                │
│             semcache.Store                          │
│             feedback.json → next run                │
└─────────────────────────────────────────────────────┘
         │
         ▼
┌────────────────┐      ┌────────────────┐
│   opengnk      │ ───► │   Gonka DAPI   │
│  L1 semcache   │      │  (inference)   │
│  X-Cache: HIT  │      └────────────────┘
│  quality mdlw  │
└────────────────┘
```

---

## Components

### 1. Key pool (`internal/agent/failover.go`)

`Client` holds a `keyPool` of API keys, each with a `coolUntil` timestamp.

| Event | Action |
|---|---|
| HTTP 429 | Rotate to next key, cool for 60s |
| HTTP 401/403 | Cool for 24h (key invalid) |
| HTTP 5xx / EOF | Cool for 10s, rotate |
| All keys cooling | Sleep until earliest recovery |

`FailoverReason` enumerates all failure classes for typed handling.
No string matching in the critical path — reason is set once and
propagated as an integer.

### 2. Loop detection (`internal/loopdetect`)

Tracks a sliding window of 30 tool calls (name + SHA256(stableArgs)).

| Pattern | Warning | Critical | Action |
|---|---|---|---|
| `generic_repeat` | 10 identical calls | — | Inject warning message |
| `poll_no_progress` | 10 polls, same result | 20 | Stop; inject, then circuit-break |
| `ping_pong` | 10 alternating calls | 20 | Inject, then stop |
| `circuit_breaker` | — | 30 any pattern | Return `RunResult.Err` |

Warning: inject `LOOP WARNING: …` as a tool result so the model self-corrects
without breaking the session.  Critical: return error immediately.

### 3. Session write lock (`internal/agent/session_lock.go`)

`session.json.lock` is created with `O_CREATE|O_EXCL` (atomic on Linux).
Contents: `{"pid": N, "created_at": "..."}`.

Stale detection:
- `kill(pid, 0)` → `ESRCH`: process dead → stale
- Lock file older than 5 min → stale

After acquiring: session is written to a `.tmp` then `os.Rename` → atomic.

### 4. Embedding cache (`internal/tools/embedcache.go`)

In-process `sync.Mutex` map: `sha256(text)` → `[]float64`.

- TTL: 1 hour
- Max: 2000 entries (≈12MB vectors + overhead)
- Eviction: random when full (TTL handles natural churn)

**Why this matters at scale:**
Without caching, `SemanticSearch` re-embeds all workspace files on every call.
With caching, warm workloads reduce `/v1/embeddings` calls by >95%.

### 5. Semantic task cache (`internal/semcache`)

Local disk: `.gonka-cache/semcache.json`.

```
Lookup(task):
  embed(task) → vec
  for each entry: cosine(vec, entry.Vec)
  if max_score ≥ 0.95 → Full hit
  if max_score ≥ 0.75 → Partial hit (inject steps)
  else                 → Miss

Store(task, steps, answer):
  embed(task) → vec
  append entry; evict by quality×recency
  write .tmp → rename (atomic)
```

Quality scoring:
- Initial: 0.7
- `resolved` outcome: +0.1 (capped at 1.0)
- `unresolved` outcome: −0.2 (floored at 0.0)
- Entries with quality < 0.3 are excluded from lookup

The decentralised design: **each client stores their own solutions**.
`opengnk` is never involved in solution storage — it acts only as an
inference router.  Future P2P discovery layer would allow opt-in sharing
of embeddings + peer addresses; actual solutions stay with their owners.

### 6. X-Inference-Feedback (L4 signal for GiP #860)

After each run:
- `resolved` / `unresolved` is saved to `.gonka-cache/feedback.json`
- On the next `gonka` invocation, it is loaded, set on `Client`, and sent
  as `X-Inference-Feedback: {"outcome":"..."}` on the first `chat()` call
- `opengnk/internal/quality/middleware.go` reads this header and
  increments `feedbackOK` / `feedbackNo` counters → L4 metric

### 7. opengnk L1 semcache (`opengnk/internal/semcache`)

Exact-match response cache keyed by `SHA256(messagesJSON)`.

```
Request → hash(messages) → entries map
  HIT  → write X-Cache: HIT + cached body
  MISS → forward to DAPI → store response + X-Cache: MISS
```

`opengnk/internal/quality/middleware.go` counts `X-Cache: HIT` as L6
(cache reuse rate).

Storage: `data/semcache.json` (async write, atomic rename).
TTL: 24h. Max: 500 entries. Configurable via `OPENGNK_CACHE_DIR`.

---

## Data flow for GiP #860 / PR #859

```
Run N:
  gonka "task A" → MISS → solve → Store(task A) → feedback.json="resolved"

Run N+1:
  gonka "task A again"
    → semcache FULL HIT (score 0.97)
    → inject cached answer
    → agent verifies → returns in <2s
    → feedback.json="resolved"
    → X-Inference-Feedback sent on first chat() → opengnk L4 counter++

  opengnk:
    → request hits L1 → X-Cache: HIT → L6 counter++
    → latency < 10ms (no DAPI call) → L8 (CV) drops
    → L9 completion rate stays 1.0
```

All four GiP #860 axes (L4, L6, L8, L9) are now measurable in a single
local setup: one agent + one opengnk + Bookworm DAPI bridge.

# Semantic Cache — Working Rules & Core Team Validation Guide

## Expert Panel

| Role | Responsibility |
|---|---|
| Protocol Architect | Validates protocol alignment, governance param flow, epoch lifecycle |
| Security Reviewer | Validates ResponseHash integrity, data race safety |
| API/Backend Engineer | Validates embedder input, in-memory store, double-embed elimination |
| QA/Test Engineer | Runs the validation flow below, fills the Evidence Table |

---

## Rules for This Task

1. Read documentation first — never guess mlnode API or chain types from memory.
2. Find ALL problems before making ANY changes.
3. After all fixes are applied — one build, one vet, one test run. No iterative compile-fix loops.
4. Every explanation includes: what changes, in which file, why the core team will accept it.
5. BLSSignature field is reserved. Verification deferred to chain-side wiring; zero gRPC calls on the HIT path.
6. Embedder input = semantic text of messages only (not canonical JSON). PromptHash uses canonical JSON for on-chain integrity.
7. QualityReporter submits MsgSubmitCacheQualitySummary once per epoch. This closes the economic loop.
8. InMemoryCacheStore is the default backend — zero external dependencies. Same CacheStore interface.

---

## Architecture — Two-Level Cache

```
User Request
│
▼ handleTransferRequest (BEFORE MsgStartInference)
[L1: PromptHash exact-match] ──── HIT ──► return cached JSON immediately
│                                         No on-chain tx, no GPU, no fee.
│ MISS                                    SimilarityBps = 10000 (100%).
▼
MsgStartInference (user pays fee)
│
▼ handleExecutorRequest
[L2: cosine similarity search] ─── HIT ──► verify sha256(payload)==ResponseHash
│                                          return cached JSON, send MsgFinishInference
│ MISS
▼
GPU Inference → MsgFinishInference → StoreResult(embedding, payload)
```

**Guarantee per level:**
- L1: sha256(canonical_JSON) identical → 100% same result. Cryptographically certain.
- L2: cosine similarity ≥ threshold (default 9700 bps = 97%). Probabilistic, governance-controlled.

---

## Reproducible Validation Flow

> This is the exact procedure the core team runs to validate the feature.
> Backend: InMemoryCacheStore (no external deps). Embedder: ML-node /embed.
> Replace `$DAPI_URL` with the DAPI public endpoint.

### Prerequisites

```bash
DAPI_URL=http://<dapi-host>:8000
MODEL=<model-name-available-on-node>
AUTH_TOKEN=<bearer-token>
```

### Step 1 — Unit test matrix (no live stack required)

```bash
cd decentralized-api
go test ./semanticcache/... -v -count=1
# Expected: 14/14 PASS
```

Covers: L1 exact-match HIT, L1 wrong-hash MISS, L2 cosine HIT, L2 below-threshold MISS,
TTL eviction (both L1 and L2), model version invalidation, disabled cache, stats accuracy.

### Step 2 — MISS: first request, no cache entry yet

```bash
curl -s -X POST $DAPI_URL/v1/chat/completions \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"What is 2+2?"}]}' \
  -D - | grep -E "^X-Cache:|HTTP/"
# Expected: HTTP/... 200 — no X-Cache header (MISS → GPU inference)
```

### Step 3 — L1 HIT: identical request (same PromptHash)

```bash
curl -s -X POST $DAPI_URL/v1/chat/completions \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"What is 2+2?"}]}' \
  -D - | grep -E "^X-Cache:|X-Cache-Level:|HTTP/"
# Expected: X-Cache: HIT / X-Cache-Level: 1
# No MsgStartInference sent — check DAPI logs for "Semantic cache L1 HIT (PromptHash)"
```

### Step 4 — L2 HIT: semantically equivalent prompt (different wording)

```bash
curl -s -X POST $DAPI_URL/v1/chat/completions \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"what is 2 + 2"}]}' \
  -D - | grep -E "^X-Cache:|X-Cache-Level:|HTTP/"
# Expected: X-Cache: HIT / X-Cache-Level: 2 (different PromptHash, same semantic space)
```

### Step 5 — MISS: semantically different prompt

```bash
curl -s -X POST $DAPI_URL/v1/chat/completions \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"'"$MODEL"'","messages":[{"role":"user","content":"Explain quantum entanglement"}]}' \
  -D - | grep -E "^X-Cache:|HTTP/"
# Expected: HTTP 200, no X-Cache header (cosine below 9700 bps threshold → GPU)
```

### Step 6 — TTL invalidation: restart DAPI or wait MaxCacheAgeEpochs

```
With MaxCacheAgeEpochs=10, entries expire after 10 epochs (~30 min on testnet).
EvictExpired called every 30s; after epoch boundary all old entries purged from L1+L2.
```

```bash
# After eviction, same prompt → MISS again (cache rebuilt from new inference)
curl -s -X POST $DAPI_URL/v1/chat/completions ... | grep "X-Cache"
# Expected: no X-Cache header (MISS)
```

### Step 7 — Model version invalidation (governance)

```
Submit governance proposal: set CacheQualityParams.EmbeddingModelVersion = "v2"
Wait for proposal to pass (~1 epoch).
```

```bash
curl -s -X POST $DAPI_URL/v1/chat/completions \
  ... "What is 2+2?" ...
# Expected: MISS — sc.modelVersion changed to "v2", stored result has ModelVersion="v1"
# No manual cache flush needed. UpdateCacheParams called every 30s from governance.
```

### Step 8 — On-chain quality summary (requires live testnet)

```bash
# After epoch boundary
gonkad q inference cache-quality-summaries <participant-address>
# Expected: entry for completed epoch with cache_reuse_count >= 1
```

---

## Evidence Table — Validated on Debian bookworm / Go 1.22.8 / 2026-03-03

| Step | Check | Expected | Actual | Pass/Fail |
|---|---|---|---|---|
| 1a | `go build ./...` | clean (no errors) | clean | PASS |
| 1b | `go vet ./...` | clean (no warnings) | clean | PASS |
| 1c | `go test ./semanticcache/... -v` | 14/14 PASS | 14/14 PASS | PASS |
| 1d | TestMatrix_L1_ExactMatch | L1 HIT, SimilarityBps=10000 | SimilarityBps=10000 | PASS |
| 1e | TestMatrix_L1_WrongHash | L1 MISS | MISS | PASS |
| 1f | TestMatrix_L2_SemanticHit | L2 HIT, SimilarityBps≥9700 | SimilarityBps≥9700 | PASS |
| 1g | TestMatrix_L2_BelowThreshold | L2 MISS (orthogonal vector) | MISS | PASS |
| 1h | TestMatrix_TTL_Eviction | L1+L2 MISS after EvictExpired | MISS | PASS |
| 1i | TestMatrix_ModelVersion_Invalidation | MISS after v1→v2 upgrade | MISS | PASS |
| 2 | First request (MISS) | HTTP 200, no X-Cache | Requires live DAPI | PENDING |
| 3 | L1 HIT (same prompt) | X-Cache: HIT, X-Cache-Level: 1 | Requires live DAPI | PENDING |
| 4 | L2 HIT (reworded prompt) | X-Cache: HIT, X-Cache-Level: 2 | Requires live DAPI | PENDING |
| 5 | MISS (different topic) | HTTP 200, no X-Cache | Requires live DAPI | PENDING |
| 6 | TTL eviction at epoch boundary | MISS after MaxCacheAgeEpochs | Requires live DAPI | PENDING |
| 7 | Model version governance update | MISS after param change | Requires live DAPI | PENDING |
| 8 | MsgSubmitCacheQualitySummary | cache_reuse_count ≥ 1 | Requires testnet | PENDING |

> Steps 1a–1i: executed on remote host bookworm, 2026-03-03.
> Steps 2–8: require full node stack (DAPI + ML-node + chain). Reproducible on testnet.

---

## Known Limitations (documented for core team)

| Item | Description | Mitigation |
|---|---|---|
| L2 self-reported similarity | AvgSimilarityBps is computed by the DAPI operator's own ML-node embed. Cannot be verified on-chain. | Governance `MaxWeightFractionBps` caps the maximum bonus weight. Incentive to lie is bounded. |
| CacheReuseCount self-reported | Only the DAPI operator knows actual hit count. | Same cap applies. Future work: on-chain event per L1 HIT. |
| In-memory cache lost on restart | Rebuilds naturally from new inferences. | Acceptable for a cache. No data loss — original results always on-chain. |
| L2 requires live ML-node embed | ML-node must be running for semantic search. If ML-node down, L2 disabled, L1 still works. | ML-node is part of standard gonka node stack. |

---

## Build Verification (run before every PR update)

```bash
# On remote (Debian bookworm, Go 1.22.8)
cd decentralized-api
go build ./...          # must be clean
go vet ./...            # must be clean
go test ./semanticcache/... -v  # all 14 tests must pass
```

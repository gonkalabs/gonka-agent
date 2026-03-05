# Gonka.gg API Key

**Key:** `gnk_live_iR5P9uxvmHccVvmAvB0oRPRHYKmDkX8ochLs8GwLvFA`

**Base URL:** `https://gonka.gg/api/public`

**Auth header:**
```
Authorization: Bearer gnk_live_iR5P9uxvmHccVvmAvB0oRPRHYKmDkX8ochLs8GwLvFA
```

## Useful endpoints

| Endpoint | Description |
|---|---|
| `GET /stats/network-overview` | Top GPUs, datacenters, countries |
| `GET /stats/historical?limit=N` | Per-epoch stats (participants, inferences, blocks) |
| `GET /nodes` | Active node list |
| `GET /account` | Account info |

## Network state (verified 2026-03-03)

- Current epoch: **188**
- Avg participants per epoch (last 31): **163**
- Blocks per epoch: **15,391** (~7 days)
- Total inferences last 31 epochs: **2,325,522**
- Top GPU: NVIDIA H100 (3,450 units)

## Usage for semantic cache validation

The public API is for **inference consumers** — it runs the official DAPI binary,
not our branch. `X-Cache: HIT` cannot be observed through this endpoint.

Use this key for:
- Verifying live network inference responses (benchmark / sanity check)
- Checking epoch state for TTL validation planning

For full cache feature validation, use the integration test (see justrule.md)
which tests InMemoryCacheStore, TTL, model version, and response hash against
local in-memory cache on bookworm — no chain or GPU required.



sk-422820ece6e84605814a9c6f433ebb5fd10da35ba15ee918
# api key #2 
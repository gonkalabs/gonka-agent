Proof 860 — two participants, different environments 

| Metric | Participant A (workspace A) | Participant B (workspace B) |
|---|---|---|
| Workspace | `/tmp/gonka-r3-A` | `/tmp/gonka-r3-B` |
| Input file | `server.go` (Counter) | `cache.go` (Cache) |
| Cache at start | **empty** | **copied from A** |
| Semcache hit | **MISS** (cold) | **◈ partial hit (score 0.79)** |
| Elapsed | **758s** (12m37s) | **583s** (9m43s) |
| Time saved B vs A | — | **23%** |
| Tool calls (A) | 13 | — |
| Tool calls (B) | — | 12 |
| `go build -race .` | **OK** | **OK** |
| semcache entries after A | **1** | — |
| semcache entries after B | — | **2** |
| feedback.json (A) | `resolved` | — |
| feedback.json (B) | — | `resolved` |

**Exact numbers:** A elapsed 758s | B elapsed 583s | B partial hit score 0.79 | B tool calls 12 | Time saved (B vs A) 23%.

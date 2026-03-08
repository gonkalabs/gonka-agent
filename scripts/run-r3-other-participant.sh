#!/usr/bin/env bash
# R-3 Real other participant test: two workspaces, copy A cache to B, run B.
# Run on host (192.168.111.25) with .env and services up. Prints numbers for testing.md.

set -e
GONKA_BIN="${GONKA_BIN:-./bin/gonka}"
WORK_A="${WORK_A:-/tmp/gonka-r3-A}"
WORK_B="${WORK_B:-/tmp/gonka-r3-B}"

echo "=== R-3 Other participant test ==="
echo "Workspace A: $WORK_A  |  Workspace B: $WORK_B"
echo ""

# 1. Workspaces
mkdir -p "$WORK_A" "$WORK_B"
rm -rf "$WORK_A/.gonka-cache" "$WORK_B/.gonka-cache"

# 2. Participant A: server.go (Counter race — buggy: no lock in Increment/Get)
cat > "$WORK_A/server.go" << 'EOF'
package main

import "sync"

type Counter struct {
	mu    sync.Mutex
	value int
}

func (c *Counter) Increment() {
	c.value++
}

func (c *Counter) Get() int {
	return c.value
}
EOF
# Note: mu exists but is never used — race. Agent must add Lock/Unlock.

# 3. Run A (cold)
echo "--- Participant A (cold) ---"
START_A=$(date +%s)
AGENT_WORKSPACE="$WORK_A" "$GONKA_BIN" "There is a race condition in server.go — the Counter methods are not thread-safe. Fix the mutex usage so Increment and Get are both protected, then verify with go build -race" || { echo "A failed"; exit 1; }
END_A=$(date +%s)
ELAPSED_A=$((END_A - START_A))
echo "A elapsed: ${ELAPSED_A}s"

if [ ! -f "$WORK_A/.gonka-cache/semcache.json" ]; then
	echo "ERROR: A did not create semcache.json"
	exit 1
fi
ENTRIES_A=$(grep -c '"task"' "$WORK_A/.gonka-cache/semcache.json" 2>/dev/null || echo "1")
echo "A semcache entries: $ENTRIES_A"

# 4. Copy A cache to B
cp -r "$WORK_A/.gonka-cache" "$WORK_B/"

# 5. Participant B: cache.go (Cache race — no mutex)
cat > "$WORK_B/cache.go" << 'EOF'
package main

type Cache struct {
	data map[string]string
}

func (c *Cache) Set(key, val string) {
	c.data[key] = val
}

func (c *Cache) Get(key string) string {
	return c.data[key]
}

func (c *Cache) Delete(key string) {
	delete(c.data, key)
}
EOF

# 6. Run B (expect partial hit)
echo ""
echo "--- Participant B (with A cache) ---"
START_B=$(date +%s)
OUT_B=$(AGENT_WORKSPACE="$WORK_B" "$GONKA_BIN" "cache.go has a race condition — the Cache methods have no mutex protection. Fix it to be thread-safe and verify with go build -race" 2>&1) || { echo "B failed"; exit 1; }
END_B=$(date +%s)
ELAPSED_B=$((END_B - START_B))
echo "$OUT_B" | head -20
echo "B elapsed: ${ELAPSED_B}s"

# Extract partial hit score from output (e.g. "partial hit (score 0.79)")
HIT_LINE=$(echo "$OUT_B" | grep -oE 'partial hit \(score [0-9.]+\)' || true)
SCORE_B="${HIT_LINE#*score }"
SCORE_B="${SCORE_B%)}"
[ -z "$SCORE_B" ] && SCORE_B="(no partial hit line — check output)"

# 7. Verify B build
(cd "$WORK_B" && go build -race .) || { echo "B go build -race failed"; exit 1; }
echo "B go build -race: OK"

# 8. Time saved
if [ "$ELAPSED_A" -gt 0 ]; then
	SAVED=$(( (ELAPSED_A - ELAPSED_B) * 100 / ELAPSED_A ))
else
	SAVED=0
fi

echo ""
echo "=== R-3 Numbers for testing.md ==="
echo "| A elapsed (s)     | $ELAPSED_A |"
echo "| B elapsed (s)     | $ELAPSED_B |"
echo "| B partial score   | $SCORE_B |"
echo "| Time saved (B vs A) | ${SAVED}% |"

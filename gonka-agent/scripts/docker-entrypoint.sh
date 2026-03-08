#!/bin/bash
set -e

if [ -z "$GONKA_API_KEY" ]; then
  echo "ERROR: GONKA_API_KEY is required."
  echo "  docker run -e GONKA_API_KEY=gnk_live_... ..."
  exit 1
fi

# Start local embed server in background (fastembed all-MiniLM-L6-v2, CPU).
python3 /opt/embed-server.py --port 8001 &
EMBED_PID=$!

# Wait for embed server to load model (first start downloads ~60MB).
for i in $(seq 1 30); do
  if curl -s http://localhost:8001/health > /dev/null 2>&1; then
    break
  fi
  sleep 1
done

# Run the agent with all remaining arguments as the task.
# If no arguments: read task from stdin (interactive mode).
exec /usr/local/bin/gonka "$@"

#!/usr/bin/env python3
"""
Local OpenAI-compatible /v1/embeddings server using fastembed.
Model: sentence-transformers/all-MiniLM-L6-v2 (dim=384, no GPU needed).

Usage:
    python3 scripts/embed-server.py [--port 8001]

Sets AGENT_EMBED_URL=http://localhost:8001 in .env to point
gonka-agent's semcache and SemanticSearch at this server.
"""
import json
import sys
import argparse
from http.server import HTTPServer, BaseHTTPRequestHandler

try:
    from fastembed import TextEmbedding
except ImportError:
    print("fastembed not installed. Run: pip install fastembed", file=sys.stderr)
    sys.exit(1)

MODEL_NAME = "sentence-transformers/all-MiniLM-L6-v2"
print(f"Loading {MODEL_NAME}...", flush=True)
_model = TextEmbedding(model_name=MODEL_NAME, dimension=384)
print("Embedding model ready.", flush=True)


class EmbedHandler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass  # suppress access log

    def do_POST(self):
        if self.path not in ("/v1/embeddings", "/embeddings"):
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))

        inp = body.get("input", "")
        if isinstance(inp, str):
            texts = [inp]
        elif isinstance(inp, list):
            texts = inp
        else:
            self.send_error(400, "input must be string or array")
            return

        vecs = list(_model.embed(texts))
        data = [
            {"object": "embedding", "embedding": v.tolist(), "index": i}
            for i, v in enumerate(vecs)
        ]
        resp = json.dumps({
            "object": "list",
            "data": data,
            "model": MODEL_NAME,
            "usage": {"prompt_tokens": sum(len(t.split()) for t in texts), "total_tokens": sum(len(t.split()) for t in texts)},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        else:
            self.send_error(404)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8001)
    args = parser.parse_args()
    server = HTTPServer(("0.0.0.0", args.port), EmbedHandler)
    print(f"Embed server listening on port {args.port}", flush=True)
    server.serve_forever()

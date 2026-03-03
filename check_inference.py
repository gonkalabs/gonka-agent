#!/usr/bin/env python3
"""Проверка inference через Gonka по твоему ключу. Запуск: GONKA_PRIVATE_KEY=... python3 check_inference.py"""
import os
import sys

def main():
    pk = os.environ.get("GONKA_PRIVATE_KEY")
    if not pk:
        print("NO GONKA_PRIVATE_KEY in env. export GONKA_PRIVATE_KEY=<hex>", file=sys.stderr)
        sys.exit(1)
    try:
        from gonka_openai import GonkaOpenAI, Endpoint
    except ImportError:
        print("pip install gonka-openai", file=sys.stderr)
        sys.exit(1)
    endpoints = [
        Endpoint(url="http://node2.gonka.ai:8000/v1", address="gonka1dkl4mah5erqggvhqkpc8j3qs5tyuetgdy552cp"),
        Endpoint(url="http://node1.gonka.ai:8000/v1", address="gonka1y2a9p56kv044327uycmqdexl7zs82fs5ryv5le"),
        Endpoint(url="https://node3.gonka.ai/v1", address="gonka1kx9mca3xm8u8ypzfuhmxey66u0ufxhs7nm6wc5"),
    ]
    client = GonkaOpenAI(gonka_private_key=pk, endpoints=endpoints)
    r = client.chat.completions.create(
        model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
        messages=[{"role": "user", "content": "Reply with exactly: OK"}],
    )
    out = (r.choices[0].message.content or "").strip()
    print("INFERENCE OK:", out)
    if "OK" not in out.upper():
        print("Unexpected reply", file=sys.stderr)
        sys.exit(2)

if __name__ == "__main__":
    main()

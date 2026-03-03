#!/usr/bin/env python3
"""
Свой мост к Gonka — OpenAI-совместимый API без GonkaGate.
Запуск: GONKA_PRIVATE_KEY=... uvicorn bridge:app --host 0.0.0.0 --port 8000
Клиент: base_url="http://localhost:8000/v1", api_key="any" (или без ключа).
"""
import os
from fastapi import FastAPI, Request, HTTPException
from fastapi.responses import JSONResponse

app = FastAPI(title="Gonka Bridge")

_GONKA_CLIENT = None

def get_client():
    global _GONKA_CLIENT
    if _GONKA_CLIENT is not None:
        return _GONKA_CLIENT
    pk = os.environ.get("GONKA_PRIVATE_KEY")
    if not pk:
        raise RuntimeError("GONKA_PRIVATE_KEY not set")
    from gonka_openai import GonkaOpenAI, Endpoint
    endpoints = [
        Endpoint(url="http://node2.gonka.ai:8000/v1", address="gonka1dkl4mah5erqggvhqkpc8j3qs5tyuetgdy552cp"),
        Endpoint(url="http://node1.gonka.ai:8000/v1", address="gonka1y2a9p56kv044327uycmqdexl7zs82fs5ryv5le"),
        Endpoint(url="https://node3.gonka.ai/v1", address="gonka1kx9mca3xm8u8ypzfuhmxey66u0ufxhs7nm6wc5"),
    ]
    _GONKA_CLIENT = GonkaOpenAI(gonka_private_key=pk, endpoints=endpoints)
    return _GONKA_CLIENT


@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    body = await request.json()
    try:
        client = get_client()
    except RuntimeError as e:
        raise HTTPException(status_code=503, detail=str(e))
    try:
        r = client.chat.completions.create(**body)
        if hasattr(r, "model_dump"):
            return r.model_dump()
        if hasattr(r, "dict"):
            return r.dict()
        return dict(r)
    except Exception as e:
        return JSONResponse(status_code=502, content={"error": str(e)})


@app.get("/v1/models")
async def models():
    try:
        client = get_client()
        # список моделей с сети
        return {
            "object": "list",
            "data": [
                {"id": "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8", "object": "model"},
                {"id": "Qwen/Qwen3-32B-FP8", "object": "model"},
                {"id": "Qwen/QwQ-32B", "object": "model"},
            ],
        }
    except RuntimeError as e:
        raise HTTPException(status_code=503, detail=str(e))


@app.get("/")
async def root():
    return {"bridge": "gonka", "openai_compatible": "/v1/chat/completions", "models": "/v1/models"}

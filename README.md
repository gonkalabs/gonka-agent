# exit

Свой мост к Gonka (без GonkaGate) + проверка inference.

## Чек inference

```bash
export GONKA_PRIVATE_KEY=<твой hex ключ>
pip install -r requirements.txt
python3 check_inference.py
```

## Свой мост (OpenAI-совместимый API)

```bash
export GONKA_PRIVATE_KEY=<твой hex ключ>
uvicorn bridge:app --host 0.0.0.0 --port 8000
```

Клиент (LangChain, n8n, свой код):

```python
from openai import OpenAI
c = OpenAI(base_url="http://localhost:8000/v1", api_key="dummy")
r = c.chat.completions.create(model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8", messages=[{"role":"user","content":"Hi"}])
print(r.choices[0].message.content)
```

Ключ берётся из кошелька Gonka (inferenced / Keplr). GNK — bounty или Host.

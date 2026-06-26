#!/usr/bin/env python3
# latency.py — mede a latencia HOT-PATH (embed de 1 prompt) por modelo. E isso
# que afeta cada turno; o batch de corpus e warmup (background, nao conta no UX).
# O daemon capa o embed da query em 5s -> acima disso cai pro BM25.
import time
import io
import contextlib
import embed_bench as eb

QUERY = "como criar um profile novo do piebald sem reiniciar o app inserindo linha no sqlite app.db"
MODELS = [
    ("openai/text-embedding-3-small", False),
    ("nvidia/llama-nemotron-embed-vl-1b-v2:free", True),
    ("qwen/qwen3-embedding-8b", False),
]
N = 6
for model, free in MODELS:
    ts = []
    for i in range(N):
        t0 = time.time()
        with contextlib.redirect_stdout(io.StringIO()):  # silencia o print de chunk
            eb.embed(model, [QUERY], free)
        ts.append(time.time() - t0)
    warm = sorted(ts[1:])  # descarta o 1o (cold)
    med = warm[len(warm) // 2]
    print(f"{model}")
    print(f"   cold={ts[0]:.2f}s | warm min/med/max = {warm[0]:.2f}/{med:.2f}/{warm[-1]:.2f}s")

#!/usr/bin/env python3
# full_bench.py — benchmark COMPLETO p/ tunar cutoff: 1 query gerada por memoria
# (142), parafraseada (nao-circular), testada em 4 modelos, com SWEEP de cutoff
# (recall do gold x extras por query). Queries cacheadas em queries.json.
import json, os, time, urllib.request
import embed_bench as eb

KEY = eb.KEY
CHAT_URL = "https://openrouter.ai/api/v1/chat/completions"
GEN_MODEL = "openai/gpt-4o-mini"
HERE = os.path.dirname(os.path.abspath(__file__))
QCACHE = os.path.join(HERE, "queries.json")
_envm = os.environ.get("BENCH_MODELS", "").strip()
if _envm:
    MODELS = [(m, m.endswith(":free")) for m in _envm.split(",") if m]
else:
    MODELS = [
        ("baai/bge-m3", False),
        ("qwen/qwen3-embedding-8b", False),
        ("openai/text-embedding-3-small", False),
        ("nvidia/llama-nemotron-embed-vl-1b-v2:free", True),
    ]
OUT = os.environ.get("BENCH_OUT", "full_report.md")
CUTOFFS = [0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.58, 0.60, 0.62, 0.65]

def gen_query(text):
    prompt = ("Abaixo o conteudo de uma memoria pessoal de um dev. Escreva UMA pergunta/busca curta "
              "e natural (como o usuario digitaria no chat) cuja resposta esteja nesta memoria. "
              "Parafraseie com palavras DIFERENTES das do texto (isto testa busca SEMANTICA, nao "
              "match exato de keyword). Responda SO com a query, sem aspas.\n\n---\n" + text[:4000])
    body = json.dumps({"model": GEN_MODEL, "messages": [{"role": "user", "content": prompt}],
                       "max_tokens": 60, "temperature": 0.7}).encode()
    req = urllib.request.Request(CHAT_URL, data=body, method="POST")
    req.add_header("Authorization", "Bearer " + KEY); req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=60) as r:
        j = json.loads(r.read())
    return j["choices"][0]["message"]["content"].strip().strip('"').replace("\n", " ")

def build_queries(corpus):
    if os.path.exists(QCACHE):
        q = json.load(open(QCACHE, encoding="utf-8"))
        if len(q) == len(corpus):
            print(f"queries em cache ({len(q)})", flush=True); return q
    q = {}; t0 = time.time()
    for i, (fn, txt) in enumerate(corpus.items()):
        try:
            q[fn] = gen_query(txt)
        except Exception as e:
            q[fn] = fn.replace("_", " ").replace(".md", ""); print(f"  genfail {fn}: {e}", flush=True)
        if (i + 1) % 20 == 0:
            print(f"  queries geradas {i+1}/{len(corpus)} ({time.time()-t0:.0f}s)", flush=True)
    json.dump(q, open(QCACHE, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
    print(f"queries geradas e salvas ({len(q)})", flush=True)
    return q

def main():
    corpus = eb.load_corpus(); cids = list(corpus); ctexts = [corpus[k] for k in cids]
    queries = build_queries(corpus)
    qtexts = [queries[fn] for fn in cids]   # gold da query i = cids[i]
    n = len(cids)
    rep = [f"# Benchmark completo — {time.strftime('%Y-%m-%d %H:%M')} ({n} memorias, 1 query/mem, paraphrased)\n"]
    for model, free in MODELS:
        print(f"== {model} ==", flush=True)
        try:
            cv, cc = eb.embed(model, ctexts, free); qv, _ = eb.embed(model, qtexts, free)
        except Exception as e:
            rep.append(f"\n## {model}\n**FALHOU:** {e}\n"); print(f"  FALHOU {e}", flush=True); continue
        r1 = r5 = 0; gss = []
        recall_at = {c: 0 for c in CUTOFFS}; extras_at = {c: 0 for c in CUTOFFS}
        for i in range(n):
            scored = sorted(((cids[j], eb.cosine(qv[i], cv[j])) for j in range(n)), key=lambda x: -x[1])
            gold = cids[i]
            rank = next(k for k, (f, s) in enumerate(scored) if f == gold)
            gs = scored[rank][1]; gss.append(gs)
            if rank == 0: r1 += 1
            if rank < 5: r5 += 1
            for c in CUTOFFS:
                if gs >= c: recall_at[c] += 1
                extras_at[c] += sum(1 for f, s in scored if f != gold and s >= c)
        gs.sort if False else gss.sort()
        rep.append(f"\n## {model} (dims={len(cv[0])}, custo embed ${cc:.5f})")
        rep.append(f"recall@1={r1}/{n} ({r1/n:.0%}) · recall@5={r5}/{n} ({r5/n:.0%})")
        rep.append(f"score do gold: min={gss[0]:.3f} · p10={gss[n//10]:.3f} · mediana={gss[n//2]:.3f}\n")
        rep.append("| cutoff | recall (gold >= cut) | extras/query medio |")
        rep.append("|---|---|---|")
        for c in CUTOFFS:
            rep.append(f"| {c:.2f} | {recall_at[c]}/{n} ({recall_at[c]/n:.0%}) | {extras_at[c]/n:.1f} |")
        print(f"  recall@1={r1}/{n} recall@5={r5}/{n} min_gold={gss[0]:.3f} p10={gss[n//10]:.3f}", flush=True)
    open(os.path.join(HERE, OUT), "w", encoding="utf-8").write("\n".join(rep) + "\n")
    print(f"\nrelatorio -> bench/{OUT}", flush=True)

if __name__ == "__main__":
    main()

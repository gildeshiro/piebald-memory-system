#!/usr/bin/env python3
# w4_rerank.py — testa o GATE do reranker (W4): so integra se melhorar o top-5.
# Pega o top-10 do embedder (openai-3-small) por query, reranka com cohere e
# compara recall@1/@5 embedder-sozinho vs embedder+reranker no corpus real.
#
# Reranker so AJUDA quando o top-1 do embedder esta errado mas o certo esta no
# top-K. Se o embedder ja acerta o top-1, o reranker nao tem o que melhorar.

import json
import sys
import urllib.request
import embed_bench as eb

KEY = eb.KEY
RERANK_URL = "https://openrouter.ai/api/v1/rerank"
EMBED_MODEL = "openai/text-embedding-3-small"
RERANK_MODEL = "cohere/rerank-v3.5"

def rerank(query, docs):
    payload = {"model": RERANK_MODEL, "query": query, "documents": docs, "top_n": len(docs)}
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(RERANK_URL, data=data, method="POST")
    req.add_header("Authorization", "Bearer " + KEY)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=60) as r:
        j = json.loads(r.read())
    order = [res["index"] for res in j["results"]]
    cost = j.get("usage", {}).get("cost", 0.0) or 0.0
    return order, cost

def main():
    corpus = eb.load_corpus()
    cids = list(corpus.keys())
    ctexts = [corpus[k] for k in cids]
    cvecs, _ = eb.embed(EMBED_MODEL, ctexts, False)
    gvecs, _ = eb.embed(EMBED_MODEL, [q for q, _ in eb.QUERIES], False)

    e1 = e5 = r1 = r5 = 0
    total_cost = 0.0
    n = len(eb.QUERIES)
    for qi, (qtext, gold) in enumerate(eb.QUERIES):
        scored = sorted(((cids[j], eb.cosine(gvecs[qi], cvecs[j])) for j in range(len(cids))),
                        key=lambda x: -x[1])
        top10 = scored[:10]
        # embedder sozinho
        if top10[0][0] in gold: e1 += 1
        if any(g in [t[0] for t in top10[:5]] for g in gold): e5 += 1
        # reranker sobre os mesmos 10 (rerankando o texto embedado de cada candidato)
        docs = [corpus[fn] for fn, _ in top10]
        order, c = rerank(qtext, docs)
        total_cost += c
        reranked = [top10[i][0] for i in order]
        if reranked[0] in gold: r1 += 1
        if any(g in reranked[:5] for g in gold): r5 += 1
        moved = "=" if reranked[0] == top10[0][0] else f"{top10[0][0][:24]}->{reranked[0][:24]}"
        print(f"  {qtext[:38]:38} embed#1={'Y' if top10[0][0] in gold else 'n'} "
              f"rerank#1={'Y' if reranked[0] in gold else 'n'} top1 {moved}", flush=True)

    print(f"\nEMBEDDER sozinho:   recall@1={e1}/{n}  recall@5={e5}/{n}")
    print(f"EMBEDDER+RERANKER:  recall@1={r1}/{n}  recall@5={r5}/{n}")
    print(f"custo rerank (10 queries): ${total_cost:.4f}  (~${total_cost/n:.4f}/query em runtime)")
    verdict = "INTEGRAR" if (r1 > e1 or r5 > e5) else "NAO integrar (zero ganho; so adiciona custo+latencia)"
    print(f"GATE: {verdict}")

if __name__ == "__main__":
    main()

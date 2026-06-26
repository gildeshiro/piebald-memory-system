#!/usr/bin/env python3
# calibrate_full.py — recalibra o cutoff de VARIOS modelos sob FULL-CONTENT
# embedding (corpo integral). Reusa embed_bench (que ja foi atualizado p/ full).
# Para cada modelo: recall@1/@5 (sanidade) + menor-acerto vs maior-ruido -> cutoff.

import sys
import urllib.error
import embed_bench as eb

# (id, eh_free). Nemotron :free exige treino; tentamos tb a variante PAGA (deny).
MODELS = [
    ("openai/text-embedding-3-small", False),
    ("nvidia/llama-nemotron-embed-vl-1b-v2:free", True),
    ("nvidia/llama-nemotron-embed-vl-1b-v2", False),   # paga (se existir -> honra deny)
    ("qwen/qwen3-embedding-8b", False),
]

NONSENSE = [
    "receita de bolo de cenoura com cobertura de chocolate",
    "qual a capital da australia e a populacao atual",
    "letra da musica garota de ipanema",
    "como trocar o oleo do motor do carro",
    "previsao do tempo para amanha no rio de janeiro",
]

def main():
    corpus = eb.load_corpus()
    cids = list(corpus.keys())
    ctexts = [corpus[k] for k in cids]
    gold_q = [q for q, _ in eb.QUERIES]
    golds = [g for _, g in eb.QUERIES]
    avg = sum(len(t) for t in ctexts) / max(1, len(ctexts))
    print(f"corpus={len(cids)} | media={avg:.0f} chars/mem (FULL-CONTENT) | queries={len(gold_q)}\n", flush=True)

    for model, is_free in MODELS:
        print(f"== {model} ==", flush=True)
        try:
            cvecs, c1 = eb.embed(model, ctexts, is_free)
            gvecs, _ = eb.embed(model, gold_q, is_free)
            nvecs, _ = eb.embed(model, NONSENSE, is_free)
        except urllib.error.HTTPError as e:
            print(f"  FALHOU HTTP {e.code}: {e.read()[:120]}\n", flush=True)
            continue
        except Exception as e:
            print(f"  FALHOU: {e}\n", flush=True)
            continue
        dims = len(cvecs[0]) if cvecs else 0
        hit1 = hit5 = 0
        gold_scores = []
        for qi in range(len(gold_q)):
            scored = sorted(((cids[j], eb.cosine(gvecs[qi], cvecs[j])) for j in range(len(cids))), key=lambda x: -x[1])
            top5 = [s[0] for s in scored[:5]]
            if scored[0][0] in golds[qi]:
                hit1 += 1
            if any(g in top5 for g in golds[qi]):
                hit5 += 1
            gs = next((s for f, s in scored if f in golds[qi]), None)
            if gs is not None:
                gold_scores.append(gs)
        noise_top = [max(eb.cosine(nvecs[qi], cvecs[j]) for j in range(len(cids))) for qi in range(len(NONSENSE))]
        min_gold = min(gold_scores) if gold_scores else 0
        max_noise = max(noise_top) if noise_top else 0
        if min_gold > max_noise:
            cutoff = round(max_noise + (min_gold - max_noise) * 0.45, 2)
            folga = f"folga {min_gold-max_noise:.3f}"
        else:
            cutoff = round(min_gold - 0.02, 2)
            folga = "SEM folga (sobreposicao)"
        print(f"  dims={dims} recall@1={hit1}/{len(gold_q)} recall@5={hit5}/{len(gold_q)} "
              f"| menor-acerto={min_gold:.3f} maior-ruido={max_noise:.3f} ({folga})")
        print(f"  -> CUTOFF recomendado: {cutoff} | custo embed corpus ${c1:.5f}\n", flush=True)

if __name__ == "__main__":
    main()

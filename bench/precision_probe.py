#!/usr/bin/env python3
# precision_probe.py — com o modelo/cutoff ATIVOS, roda as queries-gabarito
# contra TODO o corpus e mostra, por query, o que passou do cutoff:
#   - gold (acerto esperado) vs EXTRA (frouxidao = injetado sem ser o alvo)
#   - se o gold ficou ABAIXO do cutoff (apertado = deveria ter pego e nao pegou)
import os
import embed_bench as eb

MODEL = open(os.path.expanduser("~/.openrouter-embed-model")).read().strip()
CUT = float(open(os.path.expanduser("~/.openrouter-embed-cutoff")).read().strip())
FREE = MODEL.endswith(":free")

corpus = eb.load_corpus(); cids = list(corpus); ctexts = [corpus[k] for k in cids]
cvecs, _ = eb.embed(MODEL, ctexts, FREE)
gq = [q for q, _ in eb.QUERIES]
gvecs, _ = eb.embed(MODEL, gq, FREE)

print(f"modelo={MODEL} cutoff={CUT} corpus={len(cids)}\n")
total_extra = 0; missed = 0; inj_counts = []
for qi, (q, gold) in enumerate(eb.QUERIES):
    scored = sorted(((cids[j], eb.cosine(gvecs[qi], cvecs[j])) for j in range(len(cids))), key=lambda x: -x[1])
    above = [(f, s) for f, s in scored if s >= CUT]
    inj_counts.append(len(above))
    gold_entry = next(((f, s) for f, s in scored if f in gold), None)
    gname, gscore = gold_entry
    gold_in = gscore >= CUT
    if not gold_in: missed += 1
    extras = [(f, s) for f, s in above if f not in gold]
    total_extra += len(extras)
    print(f"Q: {q[:50]}")
    print(f"   gold={gname[:42]} score={gscore:.3f} {'OK' if gold_in else '<<< ABAIXO DO CUTOFF (MISS)'}")
    if extras:
        for f, s in extras[:6]:
            print(f"   + extra {s:.3f} {f[:55]}")
    else:
        print(f"   (sem extras acima do cutoff)")
    print()
n = len(eb.QUERIES)
print(f"=== RESUMO ===")
print(f"queries: {n} | gold perdido (apertado): {missed} | extras totais (frouxo): {total_extra}")
print(f"injetados por query: min {min(inj_counts)} / media {sum(inj_counts)/n:.1f} / max {max(inj_counts)} (cap do daemon = 5)")

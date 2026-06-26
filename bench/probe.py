#!/usr/bin/env python3
# probe.py <model> — latencia hot-path + recall@1 + cutoff sob full-content.
import sys, time, io, contextlib
import embed_bench as eb
model = sys.argv[1]; free = model.endswith(":free")
NONSENSE = ["receita de bolo de cenoura com chocolate","capital da australia e populacao",
            "letra da musica garota de ipanema","como trocar o oleo do carro","previsao do tempo no rio amanha"]
QUERY = "como criar um profile novo do piebald sem reiniciar inserindo linha no sqlite app.db"
ts = []
for i in range(6):
    t0 = time.time()
    with contextlib.redirect_stdout(io.StringIO()): eb.embed(model, [QUERY], free)
    ts.append(time.time() - t0)
warm = sorted(ts[1:])
print(f"{model}")
print(f"  latency warm med={warm[len(warm)//2]:.2f}s (min {warm[0]:.2f}/max {warm[-1]:.2f})")
corpus = eb.load_corpus(); cids = list(corpus); ctexts = [corpus[k] for k in cids]
with contextlib.redirect_stdout(io.StringIO()):
    cvecs,_ = eb.embed(model, ctexts, free); gv,_ = eb.embed(model,[q for q,_ in eb.QUERIES],free); nv,_ = eb.embed(model,NONSENSE,free)
h1=0; gs=[]
for qi,(q,gold) in enumerate(eb.QUERIES):
    sc=sorted(((cids[j],eb.cosine(gv[qi],cvecs[j])) for j in range(len(cids))),key=lambda x:-x[1])
    if sc[0][0] in gold: h1+=1
    g=next((s for f,s in sc if f in gold),None)
    if g is not None: gs.append(g)
nt=[max(eb.cosine(nv[qi],cvecs[j]) for j in range(len(cids))) for qi in range(len(NONSENSE))]
mg=min(gs); mn=max(nt); cut=round(mn+(mg-mn)*0.45,2) if mg>mn else round(mg-0.02,2)
print(f"  dims={len(cvecs[0])} recall@1={h1}/{len(eb.QUERIES)} menor-acerto={mg:.3f} maior-ruido={mn:.3f} -> cutoff {cut}")

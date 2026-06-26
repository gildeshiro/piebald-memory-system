#!/usr/bin/env python3
# calibrate.py — calibra o cutoff (threshold) de UM modelo de embedding usando
# o corpus real. Reusa o harness do embed_bench.
#
# Ideia: o cutoff ideal fica ENTRE
#   - o MENOR score de um acerto real (gold) -> abaixo dele perdemos match real
#   - o MAIOR score de uma query OFF-TOPIC (ruido) -> acima dele injetamos lixo
# Se houver folga (menor_gold > maior_ruido), cutoff = ponto medio (com margem).
#
# Uso: OPENROUTER_API_KEY=... python calibrate.py [model_id]

import sys
import embed_bench as eb

MODEL = sys.argv[1] if len(sys.argv) > 1 else "openai/text-embedding-3-small"
IS_FREE = MODEL.endswith(":free")

# queries off-topic (nada a ver com as memorias) -> medem o teto do ruido
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

    print(f"modelo={MODEL} corpus={len(cids)}", flush=True)
    cvecs, _ = eb.embed(MODEL, ctexts, IS_FREE)
    gvecs, _ = eb.embed(MODEL, gold_q, IS_FREE)
    nvecs, cost = eb.embed(MODEL, NONSENSE, IS_FREE)

    # menor score de acerto real
    gold_scores = []
    for qi in range(len(gold_q)):
        s = max(eb.cosine(gvecs[qi], cvecs[j]) for j in range(len(cids)) if cids[j] in golds[qi])
        gold_scores.append(s)
    min_gold = min(gold_scores)

    # maior score de qualquer query off-topic (teto do ruido)
    noise_top = []
    for qi in range(len(NONSENSE)):
        best = max((eb.cosine(nvecs[qi], cvecs[j]) for j in range(len(cids))))
        noise_top.append(best)
    max_noise = max(noise_top)

    print(f"\nmenor score de ACERTO (gold): {min_gold:.3f}")
    print(f"maior score de RUIDO (off-topic): {max_noise:.3f}")
    print(f"todos golds: {[f'{x:.3f}' for x in sorted(gold_scores)]}")
    print(f"todos ruidos: {[f'{x:.3f}' for x in sorted(noise_top, reverse=True)]}")

    if min_gold > max_noise:
        mid = (min_gold + max_noise) / 2
        # margem: 1/3 do caminho do meio em direcao ao ruido (favorece NAO perder
        # match real; preferimos injetar de leve a perder memoria relevante).
        rec = round(max_noise + (min_gold - max_noise) * 0.45, 2)
        print(f"\nFOLGA de {min_gold - max_noise:.3f}. cutoff recomendado: {rec} "
              f"(ponto medio={mid:.3f}; fica acima do ruido e abaixo do menor acerto)")
    else:
        print(f"\nSEM folga (ruido >= acerto). cutoff dificil; sugiro {round(min_gold-0.02,2)} "
              f"e aceitar algum ruido, ou usar reranker.")
    print(f"custo desta calibracao: ${cost:.6f}")

if __name__ == "__main__":
    main()

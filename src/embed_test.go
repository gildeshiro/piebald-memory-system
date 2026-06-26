package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeEmbedServer: devolve vetores determinísticos (dim pequena p/ teste) e conta
// requests, pra provar batch + cache. Cada input vira um vetor "one-hot-ish"
// baseado no hash do texto, garantindo cossenos distinguíveis.
func fakeEmbedServer(t *testing.T, calls *int, gotInputs *[]int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		var in struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if gotInputs != nil {
			*gotInputs = append(*gotInputs, len(in.Input))
		}
		var data []map[string]interface{}
		for _, s := range in.Input {
			// vetor 4-dim derivado do texto (determinístico)
			v := []float32{0, 0, 0, 0}
			for i, c := range s {
				v[i%4] += float32(c%7) / 10.0
			}
			data = append(data, map[string]interface{}{"embedding": v})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
	}))
}

func withFakeKey(t *testing.T) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(f, []byte("sk-test-fake\n"), 0600); err != nil {
		t.Fatal(err)
	}
	orKeyPath = f
	embedModelOverride = "test-model" // hermético: não lê o config real do host
}

// TestCosine: identidade=1, ortogonal=0, oposto=-1, dim incompatível=0.
func TestCosine(t *testing.T) {
	a := []float32{1, 2, 3}
	if d := cosine(a, a); math.Abs(d-1) > 1e-6 {
		t.Fatalf("cosine(a,a)=%v, esperava 1", d)
	}
	if d := cosine([]float32{1, 0}, []float32{0, 1}); math.Abs(d) > 1e-6 {
		t.Fatalf("ortogonal=%v, esperava 0", d)
	}
	if d := cosine([]float32{1, 0}, []float32{-1, 0}); math.Abs(d+1) > 1e-6 {
		t.Fatalf("oposto=%v, esperava -1", d)
	}
	if d := cosine([]float32{1, 2}, []float32{1, 2, 3}); d != 0 {
		t.Fatalf("dim incompatível deveria dar 0, veio %v", d)
	}
}

// TestEncodeDecodeVec: roundtrip base64 float32 preserva os valores.
func TestEncodeDecodeVec(t *testing.T) {
	v := []float32{-0.018142, 0.009185, 0.062133, 0, 1e-7, -123.456}
	got := decodeVec(encodeVec(v))
	if len(got) != len(v) {
		t.Fatalf("len %d != %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Fatalf("pos %d: %v != %v", i, got[i], v[i])
		}
	}
	if decodeVec("!!!nao-base64") != nil {
		t.Fatalf("base64 inválido deveria dar nil")
	}
}

// TestVecHash: estável p/ mesmo texto, muda com o texto (=> dispara re-embed).
func TestVecHash(t *testing.T) {
	if vecHash("abc") != vecHash("abc") {
		t.Fatal("hash instável")
	}
	if vecHash("abc") == vecHash("abd") {
		t.Fatal("hash colidiu p/ textos distintos")
	}
}

// TestEmbedTextsBatch: 1 request, N vetores na ordem dos inputs.
func TestEmbedTextsBatch(t *testing.T) {
	withFakeKey(t)
	calls := 0
	srv := fakeEmbedServer(t, &calls, nil)
	defer srv.Close()
	embedURL = srv.URL

	vecs, err := embedTexts([]string{"alpha", "beta", "gamma"}, 5*time.Second)
	if err != nil {
		t.Fatalf("embedTexts: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("esperava 3 vetores, veio %d", len(vecs))
	}
	if calls != 1 {
		t.Fatalf("esperava 1 request (batch), veio %d", calls)
	}
}

// TestEmbedTextsNoKey: sem key => erro (caller cai no BM25).
func TestEmbedTextsNoKey(t *testing.T) {
	orKeyPath = filepath.Join(t.TempDir(), "inexistente")
	if _, err := embedTexts([]string{"x"}, time.Second); err == nil {
		t.Fatal("esperava erro sem key")
	}
}

// TestEnsureVectorsCacheAndStaleness: 1ª chamada embeda tudo; 2ª (mesmos textos)
// não chama a rede (cache); editar um texto re-embeda SÓ aquele.
func TestEnsureVectorsCacheAndStaleness(t *testing.T) {
	withFakeKey(t)
	calls := 0
	var batchSizes []int
	srv := fakeEmbedServer(t, &calls, &batchSizes)
	defer srv.Close()
	embedURL = srv.URL
	store := filepath.Join(t.TempDir(), embedVecFile)

	items := map[string]string{"a.md": "texto a", "b.md": "texto b", "c.md": "texto c"}
	got, err := ensureVectors(items, store, 5*time.Second)
	if err != nil || len(got) != 3 {
		t.Fatalf("1ª: got=%d err=%v", len(got), err)
	}
	if calls != 1 || batchSizes[0] != 3 {
		t.Fatalf("1ª deveria ser 1 batch de 3, veio calls=%d sizes=%v", calls, batchSizes)
	}

	// 2ª chamada idêntica: tudo em cache, ZERO requests novos
	got, _ = ensureVectors(items, store, 5*time.Second)
	if len(got) != 3 || calls != 1 {
		t.Fatalf("2ª deveria usar cache (calls=%d), got=%d", calls, len(got))
	}

	// edita só b.md -> re-embeda só 1
	items["b.md"] = "texto b EDITADO"
	got, _ = ensureVectors(items, store, 5*time.Second)
	if len(got) != 3 {
		t.Fatalf("3ª: got=%d", len(got))
	}
	if calls != 2 || batchSizes[1] != 1 {
		t.Fatalf("edição deveria re-embedar SÓ 1, veio calls=%d sizes=%v", calls, batchSizes)
	}

	// poda: remove c.md dos items -> some do store
	delete(items, "c.md")
	ensureVectors(items, store, 5*time.Second)
	persisted := loadVecStore(store)
	if _, ok := persisted["c.md"]; ok {
		t.Fatal("c.md órfã deveria ter sido podada do store")
	}
}

// TestTopCosine: ordena por similaridade, respeita cutoff e limite.
func TestTopCosine(t *testing.T) {
	q := []float32{1, 0, 0}
	vecs := map[string][]float32{
		"perto.md": {0.99, 0.1, 0}, // alto cosseno
		"medio.md": {0.6, 0.8, 0},  // médio
		"longe.md": {0, 0, 1},      // ortogonal -> 0, abaixo do cutoff
	}
	out := topCosine(q, vecs, 5, 0.5)
	if len(out) != 2 || out[0] != "perto.md" {
		t.Fatalf("esperava [perto, medio], veio %v", out)
	}
	// limite respeitado
	if out = topCosine(q, vecs, 1, 0.0); len(out) != 1 || out[0] != "perto.md" {
		t.Fatalf("limite 1 falhou: %v", out)
	}
}

// TestEnsureVectorsNetworkFailKeepsCache: erro de rede não apaga o que já existe.
func TestEnsureVectorsNetworkFailKeepsCache(t *testing.T) {
	withFakeKey(t)
	calls := 0
	srv := fakeEmbedServer(t, &calls, nil)
	embedURL = srv.URL
	store := filepath.Join(t.TempDir(), embedVecFile)
	items := map[string]string{"a.md": "txt"}
	ensureVectors(items, store, 5*time.Second) // popula cache
	srv.Close()                                // derruba a rede

	items["b.md"] = "novo" // b precisa embedar, mas a rede caiu
	got, err := ensureVectors(items, store, time.Second)
	if err == nil {
		t.Fatal("esperava erro de rede")
	}
	if _, ok := got["a.md"]; !ok {
		t.Fatal("a.md (cacheado) deveria sobreviver à falha de rede")
	}
}

func TestMemEmbedTextFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.md")
	os.WriteFile(p, []byte("---\nname: x\ndescription: desc aqui\n---\n\ncorpo da memoria preview\n"), 0644)
	et := memEmbedText("x.md", p)
	if !strings.HasPrefix(et, "x.md :: desc aqui") {
		t.Fatalf("formato inesperado: %q", et)
	}
}

// TestDenyByTier: modelo pago manda provider.data_collection=deny (privacidade);
// modelo ":free" NÃO manda (senão 404 — comprovado em runtime).
func TestDenyByTier(t *testing.T) {
	withFakeKey(t)
	var lastBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBody = map[string]interface{}{}
		json.NewDecoder(r.Body).Decode(&lastBody)
		json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]interface{}{{"embedding": []float32{1, 0}}}})
	}))
	defer srv.Close()
	embedURL = srv.URL

	embedModelOverride = "baai/bge-m3" // pago
	embedTexts([]string{"q"}, 5*time.Second)
	if _, ok := lastBody["provider"]; !ok {
		t.Fatalf("modelo pago deveria mandar provider(deny), body=%v", lastBody)
	}

	embedModelOverride = "nvidia/llama-nemotron-embed-vl-1b-v2:free" // free
	embedTexts([]string{"q"}, 5*time.Second)
	if _, ok := lastBody["provider"]; ok {
		t.Fatalf("modelo :free NÃO deveria mandar provider (causa 404), body=%v", lastBody)
	}
	if !modelAllowsDeny("baai/bge-m3") || modelAllowsDeny("x:free") {
		t.Fatal("modelAllowsDeny errado")
	}
}

// embed.go — backend semântico via OpenRouter (substitui o seletor Gemini que
// morre no cliff de 2026-06-18). Estágio-1: embedder dedicado (Nemotron Embed
// VL 1B v2, 2048 dims, grátis) — pré-computa vetor por memória no disco e, na
// query, embeda o prompt e ranqueia por cosseno. Fallback BM25 local intacto.
//
// Privacidade: embeda só "nome :: desc :: preview" (paridade com o que o
// seletor Gemini já recebia) e manda provider.data_collection=deny.
//
// Doc confirmada (openrouter.ai/docs/api/reference/embeddings, 2026-06):
//
//	POST /api/v1/embeddings  {model, input:[...], encoding_format:"float"}
//	-> {data:[{embedding:[...float...]}]}  (na ordem dos inputs; batch OK)
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultEmbedModel = "nvidia/llama-nemotron-embed-vl-1b-v2:free"
	embedVecFile      = ".piebald-vectors.json" // sidecar por dir de memória (gitignored)
)

var (
	// vars (não const) p/ os testes poderem apontar pra um httptest server / key fake.
	embedURL  = "https://openrouter.ai/api/v1/embeddings"
	rerankURL = "https://openrouter.ai/api/v1/rerank" // Wave 4
	orKeyPath = filepath.Join(home, ".openrouter-api-key")
	// Blindagem (2026-06-18): o daemon subido pelo hook SessionStart do Piebald
	// (cmd /C, env mínimo) resolveu home/USERPROFILE divergente -> orKeyPath sem
	// a key -> embed caía pro (agora removido) BM25 silenciosamente. openrouterKey
	// tenta este path E um absoluto conhecido como fallback. Loga 1x p/ sem spam.
	orKeyLogOnce sync.Once
	// modelo de embedding configurável SEM recompilar: edite o arquivo + restart.
	// Regra do deny: modelo ":free" exige permitir treino (não manda deny);
	// modelo pago manda provider.data_collection="deny" (privacidade).
	embedModelFile     = filepath.Join(home, ".openrouter-embed-model")
	embedModelOverride string // só p/ testes
	cutoffFile         = filepath.Join(home, ".openrouter-embed-cutoff")
	skillVecPath       = filepath.Join(home, ".claude", ".piebald-skill-vectors.json")
	// cosseno mínimo absoluto p/ injetar (decide "NONE"). Default conservador;
	// calibrado empiricamente na verificação da W1 + eval da W4.
	embedCutoff = 0.35
	// injeção de dependência p/ testes do cliente HTTP.
	embedDo = func(req *http.Request) (*http.Response, error) { return httpClient.Do(req) }
)

// ---- key -------------------------------------------------------------------
// openrouterKey lê a key, blindado contra env de launch quebrado. Tenta o path
// derivado do env (USERPROFILE) e, se vazio/erro, um path absoluto conhecido.
// Loga a resolução UMA vez por processo (sem spam por /select).
func openrouterKey() string {
	candidates := []string{orKeyPath}
	if up := os.Getenv("USERPROFILE"); up != "" {
		if absKey := filepath.Join(up, ".openrouter-api-key"); absKey != orKeyPath {
			candidates = append(candidates, absKey)
		}
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			if p != orKeyPath {
				orKeyLogOnce.Do(func() { logf("openrouterKey: key via fallback %s (primário %s falhou)", p, orKeyPath) })
			}
			return s
		}
	}
	orKeyLogOnce.Do(func() { logf("openrouterKey: VAZIA — paths tentados: %v", candidates) })
	return ""
}

// currentEmbedModel: lê o modelo do arquivo de config (default = Nemotron free).
// Trocar de modelo => vecEntry.Model diverge => re-embed automático no próximo scan.
func currentEmbedModel() string {
	if embedModelOverride != "" {
		return embedModelOverride
	}
	if b, err := os.ReadFile(embedModelFile); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return defaultEmbedModel
}

// modelAllowsDeny: modelos pagos honram data_collection=deny (privacidade);
// modelos ":free" exigem permitir treino (comprovado: deny => 404 no free).
func modelAllowsDeny(model string) bool { return !strings.HasSuffix(model, ":free") }

// currentCutoff: cosseno mínimo p/ injetar, configurável por arquivo (calibra-se
// por modelo sem recompilar). Default = embedCutoff. Nemotron ~0.35; qwen/bge
// têm outra faixa -> recalibrar na troca de modelo (W4 eval).
func currentCutoff() float64 {
	if b, err := os.ReadFile(cutoffFile); err == nil {
		if f, e := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); e == nil {
			return f
		}
	}
	return embedCutoff
}

// ---- vetor: (de)serialização base64 de float32 LE --------------------------
type vecEntry struct {
	Model string `json:"model"`
	Hash  string `json:"hash"` // hash do texto embedado (model-change ou edição => re-embed)
	Vec   string `json:"vec"`  // base64(float32 little-endian)
}

func encodeVec(v []float32) string {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func decodeVec(s string) []float32 {
	buf, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(buf)%4 != 0 {
		return nil
	}
	v := make([]float32, len(buf)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return v
}

func vecHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---- sidecar de vetores por dir --------------------------------------------
func loadVecStore(path string) map[string]vecEntry {
	m := map[string]vecEntry{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]vecEntry{}
	}
	return m
}

func saveVecStore(path string, m map[string]vecEntry) error {
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0644)
}

// ---- cliente OpenRouter (batch) --------------------------------------------
// embedTexts: devolve um vetor por input, na ordem. Erro => caller cai no BM25.
func embedTexts(texts []string, timeout time.Duration) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	key := openrouterKey()
	if key == "" {
		return nil, fmt.Errorf("sem openrouter key em %s", orKeyPath)
	}
	model := currentEmbedModel()
	payload := map[string]interface{}{
		"model":           model,
		"input":           texts,
		"encoding_format": "float",
	}
	if modelAllowsDeny(model) {
		// pago: nega coleta/treino (privacidade). free: omite (senão 404).
		payload["provider"] = map[string]interface{}{"data_collection": "deny"}
	}
	reqBody, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", embedURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := embedDo(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embed http %d: %.200s", resp.StatusCode, string(body))
	}
	var r struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if len(r.Data) != len(texts) {
		return nil, fmt.Errorf("embed: esperava %d vetores, veio %d", len(texts), len(r.Data))
	}
	out := make([][]float32, len(texts))
	for i := range r.Data {
		out[i] = r.Data[i].Embedding
	}
	return out, nil
}

// ---- garantia de vetores (lazy: re-embeda só faltante/stale) ---------------
// items: key -> texto a embedar.  storePath: sidecar onde persistir.
// Devolve key -> vetor. Erro de rede NÃO zera o que já estava em cache.
func ensureVectors(items map[string]string, storePath string, timeout time.Duration) (map[string][]float32, error) {
	out := map[string][]float32{}
	if len(items) == 0 {
		return out, nil
	}
	model := currentEmbedModel()
	store := loadVecStore(storePath)
	var needKeys, needTexts []string
	for k, txt := range items {
		h := vecHash(txt)
		if e, ok := store[k]; ok && e.Model == model && e.Hash == h {
			if v := decodeVec(e.Vec); len(v) > 0 {
				out[k] = v
				continue
			}
		}
		needKeys = append(needKeys, k)
		needTexts = append(needTexts, txt)
	}
	// poda: remove do store entradas órfãs (memória apagada)
	for k := range store {
		if _, ok := items[k]; !ok {
			delete(store, k)
		}
	}
	var rerr error
	if len(needTexts) > 0 {
		vecs, err := embedTexts(needTexts, timeout)
		if err != nil {
			rerr = err
		} else {
			for i, k := range needKeys {
				store[k] = vecEntry{Model: model, Hash: vecHash(needTexts[i]), Vec: encodeVec(vecs[i])}
				out[k] = vecs[i]
			}
		}
	}
	saveVecStore(storePath, store)
	return out, rerr
}

// topCosine: ranqueia vecs contra a query, aplica cutoff absoluto e limite.
func topCosine(q []float32, vecs map[string][]float32, limit int, cutoff float64) []string {
	type sc struct {
		k string
		s float64
	}
	var scored []sc
	for k, v := range vecs {
		scored = append(scored, sc{k, cosine(q, v)})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].s > scored[j].s })
	var out []string
	for i := 0; i < len(scored) && len(out) < limit; i++ {
		if scored[i].s < cutoff {
			break
		}
		out = append(out, scored[i].k)
		logf("embed: %s score=%.3f", scored[i].k, scored[i].s)
	}
	return out
}

// ---- entrada de alto nível: seleção combinada por embedding ----------------
// memEmbed: display -> texto a embedar (mesmo texto da linha do índice).
// memFiles: display -> path (p/ agrupar o sidecar por dir).
// skDescs:  nome de skill -> descrição.
// ok=false => backend falhou (sem key / rede / rate-limit) -> caller usa BM25.
func selectByEmbedding(prompt string, memFiles, memEmbed, skDescs map[string]string) (mems, skills []string, ok bool) {
	if openrouterKey() == "" {
		return nil, nil, false
	}
	// 1) garante vetores de memória, agrupando por dir físico (um sidecar por dir)
	memVecs := map[string][]float32{}
	byDir := map[string]map[string]string{} // dir -> (fname -> texto)
	dispOfFname := map[string]map[string]string{}
	for disp, path := range memFiles {
		dir := filepath.Dir(path)
		fname := filepath.Base(path)
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
			dispOfFname[dir] = map[string]string{}
		}
		byDir[dir][fname] = memEmbed[disp]
		dispOfFname[dir][fname] = disp
	}
	for dir, items := range byDir {
		got, _ := ensureVectors(items, filepath.Join(dir, embedVecFile), 4*time.Second)
		for fname, v := range got {
			memVecs[dispOfFname[dir][fname]] = v
		}
	}
	// 2) garante vetores de skills (sidecar único)
	skItems := map[string]string{}
	for name, d := range skDescs {
		skItems[name] = name + " :: " + d
	}
	skVecs, _ := ensureVectors(skItems, skillVecPath, 4*time.Second)

	if len(memVecs) == 0 && len(skVecs) == 0 {
		return nil, nil, false
	}
	// 3) embeda a query 1× (é a chamada que decide o cap de latência do /select)
	qv, err := embedTexts([]string{prompt}, 5*time.Second)
	if err != nil || len(qv) == 0 || len(qv[0]) == 0 {
		return nil, nil, false
	}
	q := qv[0]
	cutoff := currentCutoff()
	mems = topCosine(q, memVecs, maxMem, cutoff)
	skills = topCosine(q, skVecs, maxSkill, cutoff)
	return mems, skills, true
}

// ---- migração / warmup: re-embeda tudo (global + projetos + skills) ---------
// Roda em background no boot; não-fatal. Garante que a 1ª query real seja rápida
// e que os vetores das ~144 memórias existam (custo zero no tier free).
func migrateAllVectors() {
	if openrouterKey() == "" {
		logf("migrate: sem openrouter key, pulado (não-fatal)")
		return
	}
	dirs := []string{memDir}
	root := filepath.Join(home, ".claude", "projects")
	if es, err := os.ReadDir(root); err == nil {
		for _, e := range es {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name(), "memory"))
			}
		}
	}
	total := 0
	for _, dir := range dirs {
		items := dirEmbedItems(dir)
		if len(items) == 0 {
			continue
		}
		got, err := ensureVectors(items, filepath.Join(dir, embedVecFile), 30*time.Second)
		if err != nil {
			logf("migrate %s: %v", dir, err)
		}
		total += len(got)
	}
	// (skills não são mais embedadas — o Piebald injeta o catálogo nativo)
	logf("migrate: %d vetores de memória prontos", total)
}

// dirEmbedItems: escaneia um dir de memória e devolve fname -> texto a embedar
// (mesma composição "nome :: desc :: preview" do índice). Chave = filename, que
// é a chave usada no sidecar daquele dir.
func dirEmbedItems(dir string) map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "MEMORY.md" {
			continue
		}
		out[name] = memEmbedText(name, filepath.Join(dir, name))
	}
	return out
}

// extractBody: corpo INTEGRAL da memória (sem o frontmatter e sem as linhas de
// provenance "[host: ...]"). Mantém headings e todo o texto — é o conteúdo p/ a
// busca semântica DENTRO do corpo (não só título/desc/preview de 160 chars).
func extractBody(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	if strings.HasPrefix(strings.TrimSpace(s), "---") {
		if idx := strings.Index(s, "---"); idx >= 0 {
			if end := strings.Index(s[idx+3:], "---"); end >= 0 {
				s = s[idx+3+end+3:]
			}
		}
	}
	var parts []string
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "[host:") {
			continue
		}
		parts = append(parts, l)
	}
	return strings.Join(parts, "\n")
}

// memEmbedText: texto embedado por memória = "nome :: desc :: CORPO INTEGRAL".
// Conteúdo inteiro (não só preview) -> busca semântica DENTRO do corpo. Cap de
// guarda em 30000 chars (~7500 tokens, sob o limite 8191 do openai-3-small);
// a maior memória atual tem ~10KB, então nada trunca. Também alimenta a string
// de índice (que agora só é checada por != ""), inofensivo.
func memEmbedText(display, path string) string {
	s := display + " :: " + extractDesc(path)
	if body := extractBody(path); body != "" {
		s += " :: " + body
	}
	if len(s) > 30000 {
		s = s[:30000]
	}
	return s
}

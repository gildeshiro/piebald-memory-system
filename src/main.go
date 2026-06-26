// piebald-memory-daemon — seletor de memória quente para o Piebald.
// Injeta memórias + skills relevantes a cada turno (paridade com o Claude Code),
// via hook UserPromptSubmit, sem o overhead de spawn (bash+jq+TLS) por mensagem.
//
// Endpoint local: POST http://127.0.0.1:8099/select  (loopback dispensa token)
//
//	body  = JSON do hook UserPromptSubmit ({"prompt":...,"cwd":...})
//	resp  = JSON {"hookSpecificOutput":{...,"additionalContext":"..."}} ou vazio.
//
// Backend semântico (embed.go): embedder dedicado via OpenRouter (estágio-1),
// vetores pré-computados por memória + cosseno na query. Fallback BM25 local
// determinístico (zero-rede) quando o embedder falha (sem key/rede/rate-limit).
// Substituiu o seletor Gemini (cloudcode + OAuth-spoof), aposentado no cutover
// de 2026-06 — antes do cliff de 2026-06-18 que mataria o backend Gemini.
//
// Hardening (audit octo-fullstep 2026-05-29, veredito FRAGILE→corrigido):
//   - concorrência: escrita atômica (temp+rename) de MEMORY.md e do token.
//   - segurança: auth por token local (X-Daemon-Token) em /save e /select
//     (loopback 127.0.0.1/::1 dispensa o token — patch 2026-05-30);
//     path-jail em /save; escaping de tags </memories>/</memory> no conteúdo.
//   - resiliência: fallback BM25; timeout do select capado em 5s; saída asciiSafe.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	listenAddr  = "127.0.0.1:8099"
	maxMem      = 5
	maxSkill    = 3
	minPromptLn = 8
	maxLogBytes = 2 << 20 // 2 MiB: rotaciona logs acima disso (Wave 3: observability)
	skillTTL    = 5 * time.Minute
)

var (
	home             = os.Getenv("USERPROFILE")
	memDir           = filepath.Join(os.Getenv("USERPROFILE"), ".claude", "memory")
	logPath          = filepath.Join(os.Getenv("USERPROFILE"), ".claude", "piebald-memory-daemon.log")
	eventsPath       = filepath.Join(os.Getenv("USERPROFILE"), ".claude", "piebald-memory-daemon.events.jsonl")
	tokenPath        = filepath.Join(os.Getenv("USERPROFILE"), ".claude", ".piebald-daemon-token")
	pluginsCacheRoot = filepath.Join(os.Getenv("USERPROFILE"), ".claude", "plugins", "cache")
	// Cliente HTTP com keepalive (reusa conexão TLS entre requests = sem handshake).
	httpClient  = &http.Client{Timeout: 15 * time.Second}
	daemonToken string

	// Mutexes granulares (tráfego é ~1 req/mensagem; corretude > paralelismo).
	saveMu sync.Mutex // /save: arquivo de memória + append em MEMORY.md

	// cache do catálogo de skills (scan raro; TTL 5min). Wave 3: skill fold-in.
	skillMu         sync.Mutex
	skillIdxCache   string
	skillDescsCache map[string]string
	skillCacheAt    time.Time

	// neutraliza tags de fechamento que escapariam o bloco <memories> injetado.
	closeTagRe = regexp.MustCompile(`(?i)<\s*/\s*memor(y|ies)\s*>`)
)

func logf(format string, a ...interface{}) {
	rotateIfBig(logPath)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// rotateIfBig: rotação simples por tamanho (mantém .1 como histórico).
func rotateIfBig(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogBytes {
		os.Rename(path, path+".1")
	}
}

// selectEvent: registro estruturado por /select (Wave 3: observability).
type selectEvent struct {
	Ts        string   `json:"ts"`
	ReqID     string   `json:"req_id"`
	CwdHash   string   `json:"cwd_hash"`
	PromptLen int      `json:"prompt_len"`
	MemCand   int      `json:"mem_candidates"`
	SkillCand int      `json:"skill_candidates"`
	MemSel    []string `json:"mem_selected"`
	SkillSel  []string `json:"skill_selected"`
	Backend   string   `json:"backend"`
	LatencyMs int64    `json:"latency_ms"`
}

func logEvent(e selectEvent) {
	rotateIfBig(eventsPath)
	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(e); err == nil {
		f.Write(append(b, '\n'))
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b)
}

// shortHash: hash curto do cwd (privacidade no log — não grava o path real).
func shortHash(s string) string {
	if s == "" {
		return ""
	}
	h := fnv.New32a()
	h.Write([]byte(s))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// writeFileAtomic: escreve em arquivo temp no mesmo dir e faz rename atômico,
// evitando torn writes / corrupção sob escrita concorrente.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	os.Chmod(tmpName, perm)
	// Windows: o rename pode falhar com "Access is denied" se um leitor tem o
	// destino aberto naquele instante (sharing violation). Retry com backoff
	// até ~1s — em produção as leituras são esparsas (1/mensagem), então a
	// janela de colisão é mínima e o rename pega uma brecha rápido.
	var rerr error
	for i := 0; i < 100; i++ {
		if rerr = os.Rename(tmpName, path); rerr == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Remove(tmpName)
	return rerr
}

// ---- token local de auth (gera 0600 se não existir) ------------------------
func loadOrCreateToken() string {
	if b, err := os.ReadFile(tokenPath); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t
		}
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		logf("token: falha ao gerar (%v) — auth desabilitada", err)
		return ""
	}
	t := hex.EncodeToString(raw)
	if err := writeFileAtomic(tokenPath, []byte(t), 0600); err != nil {
		logf("token: falha ao gravar (%v) — auth desabilitada", err)
		return ""
	}
	logf("token de auth gerado em %s", tokenPath)
	return t
}

func authOK(r *http.Request) bool {
	if daemonToken == "" {
		return true // sem token: degrade gracioso, não trava o hook
	}
	if isLoopback(r.RemoteAddr) {
		// Chamadas locais (127.0.0.1/::1) dispensam o token: o daemon escuta
		// só em loopback (listenAddr), então o token só protegeria um bind
		// não-local. Mata a fricção do /save e /select no mesmo host — que era
		// 401 quando o caller não mandava o header. (patch 2026-05-30)
		return true
	}
	return r.Header.Get("X-Daemon-Token") == daemonToken
}

// isLoopback: true se o peer da conexão é loopback (127.0.0.0/8 ou ::1).
// RemoteAddr é preenchido pelo kernel a partir do socket — não é spoofável no
// nível HTTP (não confiamos em X-Forwarded-For aqui).
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// ---- índice de memórias (nome :: descrição) --------------------------------
// Devolve: índice textual, mapa nome->caminho, mapa nome->descrição (p/ BM25).
func memoryIndex(cwd string) (idx string, files, descs, embedStr map[string]string) {
	files = map[string]string{}
	descs = map[string]string{}
	embedStr = map[string]string{}
	var sb strings.Builder

	scan := func(dir, label string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "MEMORY.md" {
				continue
			}
			display := name
			if label != "" {
				display = label + ":" + name
			}
			path := filepath.Join(dir, name)
			et := memEmbedText(display, path) // fonte única: índice == texto embedado
			files[display] = path
			descs[display] = extractDesc(path) // BM25 usa só nome+desc
			embedStr[display] = et
			sb.WriteString("- " + et + "\n")
		}
	}

	scan(memDir, "")
	if cwd != "" {
		if pdir := projectMemDir(cwd, false); pdir != "" {
			scan(pdir, "proj")
		}
		scan(filepath.Join(cwd, ".claude", "memory"), "repo")
	}
	return sb.String(), files, descs, embedStr
}

// mangleCwd: replica o nome de dir do Claude Code — cada char não-alfanumérico
// vira '-'. Ex: C:\Projects\Foo -> C--Projects-Foo
func mangleCwd(cwd string) string {
	var sb strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('-')
		}
	}
	return sb.String()
}

// projectMemDir: resolve o dir de memória do projeto (store do CC).
func projectMemDir(cwd string, create bool) string {
	if cwd == "" {
		return ""
	}
	mangled := mangleCwd(cwd)
	projectsRoot := filepath.Join(home, ".claude", "projects")
	if entries, err := os.ReadDir(projectsRoot); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.EqualFold(e.Name(), mangled) {
				return filepath.Join(projectsRoot, e.Name(), "memory")
			}
		}
	}
	if !create {
		return ""
	}
	dir := filepath.Join(projectsRoot, mangled, "memory")
	if os.MkdirAll(dir, 0755) != nil {
		return ""
	}
	idx := filepath.Join(dir, "MEMORY.md")
	if _, err := os.Stat(idx); os.IsNotExist(err) {
		writeFileAtomic(idx, []byte("# Memory index\n\n"), 0644)
	}
	logf("projeto novo: dir de memória criado em %s", dir)
	return dir
}

func extractDesc(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "description:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "description:"))
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return ""
}

// extractName: lê o campo frontmatter `name:` (canônico p/ skills).
func extractName(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "name:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "name:")), `"'`)
		}
	}
	return ""
}

// extractPreview: primeiras ~160 chars do CORPO (após o frontmatter) — dá ao
// seletor sinal além da description (Wave 3: selection quality).
func extractPreview(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	// pula o frontmatter (entre o 1º e o 2º "---")
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
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "[host:") {
			continue
		}
		parts = append(parts, l)
		if len(strings.Join(parts, " ")) > 160 {
			break
		}
	}
	pv := strings.Join(parts, " ")
	if len(pv) > 160 {
		pv = pv[:160]
	}
	return pv
}

// skillIndex: catálogo de skills instaladas (cache com TTL). Dedup por nome.
// Wave 3: skill fold-in — surface dinâmica em vez da tabela estática só no prompt.
func skillIndex() (string, map[string]string) {
	skillMu.Lock()
	defer skillMu.Unlock()
	if skillIdxCache != "" && time.Since(skillCacheAt) < skillTTL {
		return skillIdxCache, skillDescsCache
	}
	descs := map[string]string{}
	matches, _ := filepath.Glob(filepath.Join(pluginsCacheRoot, "*", "*", "*", "skills", "*", "SKILL.md"))
	for _, p := range matches {
		name := extractName(p)
		if name == "" {
			name = filepath.Base(filepath.Dir(p))
		}
		if _, seen := descs[name]; seen {
			continue
		}
		descs[name] = extractDesc(p)
	}
	names := make([]string, 0, len(descs))
	for n := range descs {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		sb.WriteString("- " + n + " :: " + descs[n] + "\n")
	}
	skillIdxCache = sb.String()
	skillDescsCache = descs
	skillCacheAt = time.Now()
	return skillIdxCache, descs
}

// ---- fallback BM25 local (determinístico, zero-rede) -----------------------
// Usado quando o embedder falha (sem key/rede/rate-limit). Garante que a
// memória sobrevive a outages e que a busca nunca para — só degrada de
// semântica p/ lexical até a rede/quota voltar.
var stopWords = map[string]bool{
	"que": true, "com": true, "para": true, "por": true, "uma": true, "dos": true,
	"das": true, "como": true, "isso": true, "esse": true, "essa": true, "the": true,
	"and": true, "for": true, "you": true, "with": true, "não": true,
	"ele": true, "ela": true, "mas": true, "foi": true, "são": true, "este": true,
}

func tokenize(s string) []string {
	var toks []string
	var cur []rune
	flush := func() {
		if len(cur) >= 3 {
			t := string(cur)
			if !stopWords[t] {
				toks = append(toks, t)
			}
		}
		cur = cur[:0]
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

func localSelect(prompt string, descs map[string]string, limit int) []string {
	q := tokenize(prompt)
	if len(q) == 0 || len(descs) == 0 {
		return nil
	}
	docs := map[string][]string{}
	totalLen := 0
	for name, d := range descs {
		t := tokenize(name + " " + d)
		docs[name] = t
		totalLen += len(t)
	}
	n := len(docs)
	avg := float64(totalLen) / float64(n)
	if avg == 0 {
		avg = 1
	}
	df := map[string]int{}
	for _, toks := range docs {
		seen := map[string]bool{}
		for _, t := range toks {
			if !seen[t] {
				seen[t] = true
				df[t]++
			}
		}
	}
	const k1, b = 1.5, 0.75
	type sc struct {
		name string
		s    float64
	}
	var scored []sc
	for name, toks := range docs {
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		var s float64
		dl := float64(len(toks))
		for _, qt := range q {
			if tf[qt] == 0 {
				continue
			}
			idf := math.Log(1 + (float64(n)-float64(df[qt])+0.5)/(float64(df[qt])+0.5))
			f := float64(tf[qt])
			s += idf * (f * (k1 + 1)) / (f + k1*(1-b+b*dl/avg))
		}
		if s > 0 {
			scored = append(scored, sc{name, s})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].s > scored[j].s })
	if len(scored) == 0 {
		return nil
	}
	// cutoff relativo: descarta matches fracos (< 35% do topo) p/ não injetar ruído.
	cutoff := scored[0].s * 0.35
	var out []string
	for i := 0; i < len(scored) && i < limit; i++ {
		if scored[i].s < cutoff {
			break
		}
		out = append(out, scored[i].name)
	}
	return out
}

// sanitizeMemory: neutraliza tags de fechamento que escapariam o bloco injetado.
func sanitizeMemory(b []byte) []byte {
	return closeTagRe.ReplaceAll(b, []byte("[/memory]"))
}

// ---- handler ----------------------------------------------------------------
func handleSelect(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			logf("PANIC handleSelect: %v", r)
		}
	}()
	body, _ := io.ReadAll(req.Body)
	var in struct {
		Prompt         string  `json:"prompt"`
		TranscriptPath *string `json:"transcript_path"`
		Cwd            string  `json:"cwd"`
	}
	json.Unmarshal(body, &in)

	if in.TranscriptPath != nil && *in.TranscriptPath != "" {
		return // Claude Code tem @imports nativo
	}
	prompt := strings.TrimSpace(in.Prompt)
	if len([]rune(prompt)) < minPromptLn {
		return
	}

	start := time.Now()
	reqID := randHex(4)

	memIdx, files, memDescs, memEmbed := memoryIndex(in.Cwd)
	if memIdx == "" {
		return
	}

	// Skills NÃO são mais superficiadas aqui: o Piebald já injeta o catálogo
	// nativo (<available_agent_skills>) no início da sessão — o surfacing de
	// skill do daemon era REDUNDANTE (e gastava embedding por turno). Removido
	// 2026-06-15. O daemon foca só em memória, que é o gap real do Piebald.
	mems, _, ok := selectByEmbedding(prompt, files, memEmbed, nil)
	backend := "embed"
	if !ok {
		// Fallback BM25 DESATIVADO (decisão do dono, 2026-06-18): sem
		// degradação silenciosa. Se o backend de embedding (OpenRouter) falha,
		// o daemon NÃO serve memória de BM25 — falha visível, nenhuma injeção.
		// localSelect()/TestLocalSelectBM25 PERMANECEM no código (não removidos);
		// apenas não há mais canal que os use como fallback do /select.
		backend = "embed-failed"
		logf("embed FALHOU (fallback BM25 desativado) — nenhuma memória injetada: %.60s", prompt)
		mems = nil
	}

	// filtra p/ itens que existem de fato + aplica caps
	var memSel []string
	for _, fn := range mems {
		if _, exists := files[fn]; exists && len(memSel) < maxMem {
			memSel = append(memSel, fn)
		}
	}

	if len(memSel) == 0 {
		logf("no-match (%s): %.60s", backend, prompt)
		logEvent(selectEvent{
			Ts: time.Now().Format(time.RFC3339), ReqID: reqID, CwdHash: shortHash(in.Cwd),
			PromptLen: len([]rune(prompt)), Backend: backend, LatencyMs: time.Since(start).Milliseconds(),
		})
		return
	}

	var ctx strings.Builder
	if len(memSel) > 0 {
		ctx.WriteString(fmt.Sprintf("<memories source=\"piebald-memory-daemon\" note=\"Memórias relevantes do usuário selecionadas para esta mensagem. Considere-as como contexto persistente.\">\n<!-- piebald-selector backend=%s mem=%s -->\n", backend, strings.Join(memSel, ",")))
		for _, fn := range memSel {
			b, err := os.ReadFile(files[fn])
			if err != nil {
				continue
			}
			ctx.WriteString("\n<memory source=\"" + closeTagRe.ReplaceAllString(fn, "") + "\">\n")
			ctx.Write(sanitizeMemory(b))
			ctx.WriteString("\n</memory>\n")
			logf("injetou mem: %s", fn)
		}
		ctx.WriteString("\n</memories>")
	}

	logEvent(selectEvent{
		Ts: time.Now().Format(time.RFC3339), ReqID: reqID, CwdHash: shortHash(in.Cwd),
		PromptLen: len([]rune(prompt)), MemCand: len(memDescs),
		MemSel: memSel, Backend: backend, LatencyMs: time.Since(start).Milliseconds(),
	})

	out, _ := json.Marshal(map[string]interface{}{
		"hookSpecificOutput": map[string]string{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": ctx.String(),
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(asciiSafe(out))
}

// asciiSafe: escapa runes não-ASCII como \uXXXX num JSON já serializado, pra a
// saída ser PURA ASCII e sobreviver a qualquer codepage do Windows. Fix do bug
// reportado pelo @santosfaab: memórias saíam como "MemÃ³rias" quando o console/
// pipe decodificava o UTF-8 como ANSI/Latin-1. Com tudo em \uXXXX, qualquer
// codepage decodifica idêntico e o parser JSON reconstrói o Unicode correto.
func asciiSafe(b []byte) []byte {
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 128:
			sb.WriteRune(r)
		case r <= 0xFFFF:
			fmt.Fprintf(&sb, "\\u%04x", r)
		default: // fora do BMP (ex: emoji) -> surrogate pair
			r -= 0x10000
			fmt.Fprintf(&sb, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		}
	}
	return []byte(sb.String())
}

// warmup: re-embeda todas as memórias (global + projetos + skills) em background
// ANTES da 1ª mensagem, pra a 1ª query real ser rápida. NÃO-FATAL: qualquer
// falha apenas loga e segue (o daemon sobe e o /save funciona mesmo sem rede /
// sem key). Substitui o antigo warmup do Gemini (aposentado no cutover).
func warmup() {
	migrateAllVectors()
}

func slugify(title string) string {
	var sb strings.Builder
	prevUnd := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			prevUnd = false
		} else if !prevUnd {
			sb.WriteRune('_')
			prevUnd = true
		}
	}
	s := strings.Trim(sb.String(), "_")
	if s == "" {
		s = fmt.Sprintf("memory_%d", time.Now().Unix())
	}
	return s
}

// badEnc: true se a string carrega U+FFFD (RuneError) — assinatura de UTF-8
// destruído no transporte (argv passando por codepage não-UTF-8 no Windows).
// Já é tarde para recuperar os bytes originais; o chamador deve reenviar via
// stdin. Detecção barata, sem alocação.
func badEnc(s string) bool { return strings.ContainsRune(s, '\uFFFD') }

// upsertIndexLine: devolve o índice MEMORY.md com a linha de `fname` atualizada
// in-place, ou anexada se ausente. Casa pela âncora "](fname)" (slug é
// determinístico por título). Colapsa duplicatas legadas (mantém só a 1ª, que
// vira a linha nova). Preserva header/vizinhos e termina sempre com '\n'.
func upsertIndexLine(existing []byte, fname, title, desc string) []byte {
	desc = strings.ReplaceAll(desc, "\n", " ")
	newLine := "- [" + title + "](" + fname + ") — " + desc
	marker := "](" + fname + ")"
	var out []string
	replaced := false
	for _, ln := range strings.Split(string(existing), "\n") {
		if strings.Contains(ln, marker) && strings.HasPrefix(strings.TrimSpace(ln), "- [") {
			if replaced {
				continue // descarta duplicata legada
			}
			out = append(out, newLine)
			replaced = true
			continue
		}
		out = append(out, ln)
	}
	if !replaced {
		for len(out) > 0 && out[len(out)-1] == "" {
			out = out[:len(out)-1] // sem linha em branco antes do append
		}
		out = append(out, newLine)
	}
	res := strings.Join(out, "\n")
	if !strings.HasSuffix(res, "\n") {
		res += "\n"
	}
	return []byte(res)
}

// handleSave: grava uma memória nova no formato Claude Code (frontmatter + index).
// Serializado por saveMu; escrita atômica; path-jail sob ~/.claude.
func handleSave(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var in struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Content     string `json:"content"`
		Scope       string `json:"scope"`
		Cwd         string `json:"cwd"`
	}
	if json.Unmarshal(body, &in) != nil || in.Title == "" {
		http.Error(w, `{"error":"title obrigatorio"}`, 400)
		return
	}

	// Anti-mojibake (audit 2026-06-02): input com U+FFFD (RuneError) chegou
	// corrompido no transporte — tipicamente `curl -d` inline passando UTF-8
	// pelo argv no Windows (o codepage ANSI destrói '→' e acentos antes do
	// curl ler argv). U+FFFD é IRRECUPERÁVEL, então falhamos LOUD em vez de
	// persistir mojibake em silêncio. Fix do chamador: mandar o corpo por
	// stdin (--data-binary @-), que não passa pelo argv.
	if badEnc(in.Title) || badEnc(in.Description) || badEnc(in.Content) {
		http.Error(w, `{"error":"input corrompido (U+FFFD): nao passe UTF-8 pelo argv do shell. Use stdin: curl -s -X POST http://127.0.0.1:8099/save -H 'Content-Type: application/json' --data-binary @- <<'EOF' / {json} / EOF"}`, 400)
		logf("save REJEITADO (mojibake U+FFFD no input)")
		return
	}

	saveMu.Lock()
	defer saveMu.Unlock()

	var dir string
	if in.Scope == "project" {
		dir = projectMemDir(in.Cwd, true)
		if dir == "" {
			http.Error(w, `{"error":"sem cwd valido para escopo project"}`, 400)
			return
		}
	} else {
		dir = memDir
		os.MkdirAll(dir, 0755)
	}

	slug := slugify(in.Title) // já restringe a [a-z0-9_], sem traversal
	fname := slug + ".md"
	fpath := filepath.Join(dir, fname)

	// path-jail: o destino final TEM que ficar sob ~/.claude
	absPath, err1 := filepath.Abs(fpath)
	jailRoot, err2 := filepath.Abs(filepath.Join(home, ".claude"))
	if err1 != nil || err2 != nil || !strings.HasPrefix(absPath, jailRoot+string(os.PathSeparator)) {
		http.Error(w, `{"error":"path fora do jail"}`, 400)
		logf("save BLOQUEADO (jail): %s", fpath)
		return
	}

	host := os.Getenv("CLAUDE_HOST_TAG")
	if host == "" {
		if hn, err := os.Hostname(); err == nil {
			host = hn
		} else {
			host = "local"
		}
	}
	date := time.Now().Format("2006-01-02")
	var mb strings.Builder
	mb.WriteString("---\n")
	mb.WriteString("name: " + slug + "\n")
	mb.WriteString("description: " + strings.ReplaceAll(in.Description, "\n", " ") + "\n")
	mb.WriteString("metadata:\n  node_type: memory\n  type: note\n---\n\n")
	mb.WriteString(fmt.Sprintf("[host: %s, date: %s]\n\n", host, date))
	mb.WriteString(in.Content + "\n")
	if writeFileAtomic(fpath, []byte(mb.String()), 0644) != nil {
		http.Error(w, `{"error":"falha ao escrever"}`, 500)
		return
	}

	// índice MEMORY.md: read-modify-write atômico (sob saveMu, sem interleave).
	// Dedup (W2): substitui a linha do mesmo arquivo em vez de anexar cega —
	// re-salvar o mesmo título NÃO duplica mais a entrada (e conserta legado).
	idxPath := filepath.Join(dir, "MEMORY.md")
	existing, _ := os.ReadFile(idxPath)
	writeFileAtomic(idxPath, upsertIndexLine(existing, fname, in.Title, in.Description), 0644)

	logf("salvou (%s): %s", in.Scope, fpath)

	// re-embed on save (W2): atualiza o vetor desta memória já, sem esperar o
	// próximo /select/migrate. Best-effort em goroutine (não atrasa a resposta
	// nem segura saveMu numa chamada de rede). Passa o dir INTEIRO de propósito:
	// ensureVectors poda órfãos, então mandar só o item recém-salvo apagaria os
	// vetores das outras memórias do dir. Se rede/key falhar, o BM25 cobre e o
	// re-embed lazy pega na próxima varredura.
	go func(d string) {
		if openrouterKey() == "" {
			return
		}
		if _, err := ensureVectors(dirEmbedItems(d), filepath.Join(d, embedVecFile), 15*time.Second); err != nil {
			logf("reembed on save: %v", err)
		}
	}(dir)

	out, _ := json.Marshal(map[string]string{"status": "ok", "path": fpath})
	w.Header().Set("Content-Type", "application/json")
	w.Write(asciiSafe(out))
}

func main() {
	daemonToken = loadOrCreateToken()
	http.HandleFunc("/select", guard(handleSelect))
	http.HandleFunc("/save", guard(handleSave))
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	logf("daemon iniciado em %s (auth=%v)", listenAddr, daemonToken != "")
	go warmup() // aquece em background, sem bloquear o listen; não-fatal
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		logf("falha ao iniciar: %v", err)
		os.Exit(1)
	}
}

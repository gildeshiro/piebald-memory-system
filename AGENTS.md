# piebald-memory-system — AGENTS.md

> Doc autoritativo deste projeto (host **win-work**). O Piebald auto-carrega o
> `AGENTS.md` do diretório do projeto, então isto vira contexto vivo sempre que
> este repo está aberto. Mantenha-o curto e verdadeiro.

## O que é este repo
O daemon de memória local (Go, `127.0.0.1:8099`) que recria o comportamento de
memória do Claude Code dentro do Piebald: injeta memórias relevantes por mensagem
via hook `UserPromptSubmit` e grava no formato Claude-Code (frontmatter + `MEMORY.md`).
Fonte: `src/main.go`, `src/embed.go`.

## ⭐ Fonte canônica = ESTE repo (decidido 2026-06-27)
A verdade do source mora **aqui** (`C:\Projects\piebald-memory-system\src\`).
`~/bin` é **alvo de deploy** (recebe o `.exe` buildado), não fonte. O kit do Santos
é **downstream** (regenerado a partir daqui). Fluxo único, sem editar nos outros lugares:

```
edita em src/  →  go build  →  deploya .exe em ~/bin  →  (quando relevante) regenera o kit do Santos + handoff
```

Isto encerra o TODO antigo "repo vs ~/bin" e mata a armadilha de drift que já causou
a regressão de 2026-06-02 (rebuild a partir de cópia stale).

### Estado de reconciliação no momento da decisão (confirmado 2026-06-27, por `diff`)
- `src/` é a cópia **mais nova e completa** — está **à frente** do vivo, não atrás.
- Diferenças do repo vs `~/bin` (e vs kit, que é idêntico ao `~/bin`):
  - `main.go`: repo tem o **BM25 fallback DESATIVADO** (decisão 2026-06-18); `~/bin` vivo ainda tem BM25 ativo.
  - `embed.go`: repo tem o **hardening do `openrouterKey`** (resolução multi-path da key, 2026-06-18); o vivo tem a versão single-path.
- `main_test.go`, `embed_test.go`, `go.mod`: idênticos nas 3 cópias.
- ⚠️ O **`.exe` rodando é de 2026-06-03** — atrás do próprio source do `~/bin` (06-15) e do repo. O estado pretendido (BM25 cortado + key blindada) **só existe no repo, nunca foi deployado**. Rebuild+deploy é uma decisão consciente (muda comportamento vivo: desliga o BM25 fallback) — não fazer silenciosamente.

## Onde estão chats, sessões e memórias (mapa de dados)
**Chats/sessões do Piebald** → SQLite em
`C:\Users\carla\AppData\Roaming\Piebald\app.db` (~2,7 GB, vivo). Tabelas (confirmadas
por `sqlite_master`, 2026-06-27):
- **`chats`** — os chats em si; `chat_folders`, `chat_tags`, `chat_goals` — organização.
- **`messages`** + **`message_parts`** e `message_part_text` / `_image` / `_file` /
  `_audio` / `_tool_call` — conteúdo das mensagens. Busca full-text via
  `message_part_text_fts*`. (NÃO existe tabela `chat_message`.)
- `workos_sessions` = sessão de **auth** (WorkOS), não chat.
- System prompt do profile (separado): `profiles.config_id` → `generation_configs`
  → `base_gen_cfg_data.system_prompt`. Ler read-only: `file:app.db?mode=ro`.
- `sqlite3.exe` disponível em `~/scoop/apps/android-clt/current/platform-tools/`.

**Memórias deste sistema** (store do Claude Code, reusado):
- Global: `~/.claude/memory/*.md` + `MEMORY.md` (índice).
- Por projeto: `~/.claude/projects/<cwd-mangled>/memory/` (mangle replica o do CC).
- Sidecar de vetores por dir: `.piebald-vectors.json` (gitignored).

> Regra: **progresso/status de projeto vai em arquivo local** (ex.: `progress-log.md`),
> NÃO no daemon de memória.

## Deploy map (onde cada peça roda VIVO)
| Componente | Path vivo no host |
|---|---|
| Binário rodando | `~/bin/piebald-memory-daemon.exe` (porta 8099) |
| Source de deploy | `~/bin/piebald-memory-daemon/` (espelho de build; canônico = este repo) |
| Hook selector | `~/bin/piebald-memory-selector.cmd` (UserPromptSubmit → POST /select) |
| Hook starter | `~/bin/piebald-daemon-start.cmd` (SessionStart; idempotente; **pin de `USERPROFILE`/`HOME`** 2026-06-18) |
| Wiring dos hooks | `~/.claude/settings.json` (snapshot fiel em `docs/settings.json.snapshot`) |
| Memórias globais | `~/.claude/memory/*.md` + `MEMORY.md` |

## Build & deploy
```bash
cd src && go build -o "$USERPROFILE/bin/piebald-memory-daemon.exe" .
go test -race -count=1 ./...        # precisa gcc (mingw via scoop)
# restart do daemon vivo (TOCA O VIVO — confirmar antes):
taskkill //F //IM piebald-memory-daemon.exe ; \
  powershell -NoProfile -Command "Start-Process -FilePath '$env:USERPROFILE\bin\piebald-memory-daemon.exe' -WindowStyle Hidden"
```

## Relação com o kit do Santos (downstream)
`C:\Projects\SEXAGENARIOS\piebald-memory-kit\` (repo `gildeshiro/SEXAGENARIOS`) é o
**instalador portátil despersonalizado** do daemon para o @santosfaab — pacote
**downstream** deste repo, não uma fonte. Quando o canônico mudar e fizer sentido
propagar: **regenerar o kit a partir daqui** (não editar à mão lá) e avisar via
`SEXAGENARIOS/handoffs/`. Hoje o `daemon/` do kit está em 06-15 (igual ao `~/bin`),
atrás do repo.

## Segredos & runtime (host-only, NÃO versionado)
- `~/.openrouter-api-key` (0600) — key do embedder. `~/.openrouter-embed-model` /
  `~/.openrouter-embed-cutoff` — overrides de modelo/cutoff (config, sem recompilar).
- `~/.claude/.piebald-daemon-token` (0600) — token de auth local do daemon.
- `~/.claude/piebald-memory-daemon.log` / `.events.jsonl` — logs de runtime.

## Pendências
- **Tarefa 3 (limpar dead-code de skill-surfacing):** rodar AGORA que o repo é
  canônico — limpar no repo, `go build` + `go test -race`, depois deployar. Detalhe
  no `progress-log.md`.
- **Deploy do estado novo:** decidir rebuild+deploy do `.exe` (desliga BM25 fallback
  vivo + key hardening) — toca o vivo, confirmar antes.
- **Sync do kit do Santos:** regenerar a partir do canônico + handoff, quando propagar.

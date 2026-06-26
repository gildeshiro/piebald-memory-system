# piebald-memory-system

A **memory + automatic skill-surfacing system for Piebald**, replicating Claude
Code's "fluid" behavior (relevant memories and skills injected every turn) inside
Piebald, which is a "dumb" client in that regard.

Core: a **hot Go daemon** (`piebald-memory-daemon`) that ranks memories and skills
by **semantic embedding** (OpenRouter; local BM25 fallback), listens on
`127.0.0.1:8099`, and is driven by Piebald's **Claude-Code-compatible hooks**.

> ℹ️ **Semantic backend (current):** the selector migrated from Gemini Flash-Lite
> (cloudcode + OAuth-spoof, retired at the 2026-06-18 cliff) to a **dedicated
> embedder via OpenRouter** — vectors precomputed per memory + cosine against the
> query, with a deterministic BM25 fallback. **Embedding model in use:
> `baai/bge-m3`** (live config in `~/.openrouter-embed-model`); the built-in default
> is `nvidia/llama-nemotron-embed-vl-1b-v2:free`. Model and similarity cutoff are
> configurable without recompiling (`~/.openrouter-embed-model`,
> `~/.openrouter-embed-cutoff`; default cosine cutoff `0.35`) — changing the model
> auto-triggers a re-embed on the next scan. Paid models send
> `data_collection=deny` (privacy); `:free` models require allowing training. See
> `src/embed.go`.

> ⚠️ This repository is the **consolidated source / organizational copy**. The
> **running** system lives in the deploy paths below (`~/bin`, `~/.claude`). Editing
> here does **not** affect what's live until you rebuild/copy (see Deploy).

## Feature Request (angle for an upstream Piebald issue)

**Gap:** Claude Code injects relevant memories (`MEMORY.md`) and skills every turn
automatically. Piebald doesn't — it's a "dumb" client in that regard: no persistent
memory between chats, no automatic skill surfacing. The user loses context with each
new session.

**Proposal:** a selection + injection pipeline via a `UserPromptSubmit` hook (parity
with Claude Code's memory/skill behavior) — it selects up to 5 relevant memories + 3
skills and injects them as `additionalContext` for the turn, with a write endpoint in
the native format (frontmatter + `MEMORY.md`). This repository is the **reference
implementation** (local Go daemon, port 8099).

---

## What it does
- **Reading (automatic):** `UserPromptSubmit` hook → daemon selects up to 5 memories
  + 3 relevant skills (OpenRouter embedder — `baai/bge-m3`; deterministic local BM25
  fallback) and injects them as `additionalContext` (`<memories>` +
  `<relevant-skills>`), with a visible marker.
- **Writing:** `/save` endpoint writes a new memory in the Claude Code format
  (frontmatter + `MEMORY.md` index), with path-jail and atomic write.
- **Project-agnostic:** replicates CC's path mangling to find/create
  `~/.claude/projects/<mangled>/memory/`.
- **Resilience/Security (hardened in a multi-LLM audit):** mutex + atomic write
  (creds 0600), `X-Daemon-Token` auth, `</memories>` escaping, capped timeout,
  offline BM25 fallback, JSONL logs + rotation.

## Layout
```
src/        main.go, embed.go, main_test.go, embed_test.go, go.mod   ← daemon source (Go)
hooks/      piebald-memory-selector.cmd      ← UserPromptSubmit -> POST /select
            piebald-daemon-start.cmd         ← SessionStart -> starts the daemon (idempotent)
bench/      calibrate*.py, full_bench.py, latency.py, precision_probe.py, probe.py, w4_rerank.py
                                             ← embedding-model calibration / latency harness (bring your own corpus)
archived/   piebald-memory-selector.sh.deprecated  ← orphan bash version (DO NOT use; history)
docs/       settings.json.snapshot           ← snapshot of the hook wiring (~/.claude/settings.json)
```

## Deploy map (where each piece runs LIVE)
| Component | Active path on the host |
|---|---|
| Daemon source | `%USERPROFILE%\bin\piebald-memory-daemon\` (main.go etc.) |
| Compiled binary | `%USERPROFILE%\bin\piebald-memory-daemon.exe` (port 8099) |
| Selector hook | `%USERPROFILE%\bin\piebald-memory-selector.cmd` |
| Starter hook | `%USERPROFILE%\bin\piebald-daemon-start.cmd` |
| Hook wiring | `%USERPROFILE%\.claude\settings.json` (PreToolUse/SessionStart/UserPromptSubmit) |
| Global instructions | `%USERPROFILE%\.claude\piebald-global-agents.md` (pasted into the Piebald UI) |
| Global memories | `%USERPROFILE%\.claude\memory\*.md` + `MEMORY.md` |
| Project memories | `%USERPROFILE%\.claude\projects\<mangled>\memory\` |

## Secrets & runtime (NOT versioned — host-only)
- `~/.openrouter-api-key` — OpenRouter API key for embeddings (0600, per-host, not synced).
- `~/.openrouter-embed-model` / `~/.openrouter-embed-cutoff` — embedding model + cosine
  cutoff overrides (config, not secrets; editable without recompiling).
- `~/.claude/.piebald-daemon-token` — daemon's local auth token (0600).
- `~/.gemini/oauth_creds.json` — Gemini OAuth (legacy backend; access/refresh token).
- `~/.claude/.piebald-gemini-project` — cached GCP project (legacy, derived).
- `~/.claude/memory/` — user memory content (may be sensitive); per-dir vector cache
  `.piebald-vectors.json` sidecars (gitignored).
- `~/.claude/piebald-memory-daemon.log` / `.events.jsonl` — runtime logs.

> ⚠️ **Legacy (pre-OpenRouter):** the `geminiClientID`/`geminiSecret` in `src/main.go`
> are the Gemini CLI's **public** ones (extracted from the official bundle), not user
> secrets. The `v1internal` endpoint + User-Agent spoof hack expired at the
> **2026-06-18 cliff**; the embedder now runs on OpenRouter, and the local BM25
> fallback keeps memory alive regardless.

## Build & deploy
```bash
# build (from src/ or the active dir)
cd src && go build -o "$USERPROFILE/bin/piebald-memory-daemon.exe" .

# tests (needs gcc for -race; mingw via scoop)
go test -race -count=1 ./...

# restart the live daemon
taskkill //F //IM piebald-memory-daemon.exe ; \
  powershell -NoProfile -Command "Start-Process -FilePath '%USERPROFILE%\bin\piebald-memory-daemon.exe' -WindowStyle Hidden"
```
> Note: today's canonical source is still `~/bin/piebald-memory-daemon\`. If you want
> to make THIS repo the canonical source, move the source here and update build/deploy
> to copy the `.exe` into `~/bin` (pending decision — see TODO).

## Status
- ✅ Wave 1 (hardening: concurrency/atomic/auth/path-jail/escaping) — shipped + E2E.
- ✅ Wave 3 (observability JSONL + content-preview + BM25 cutoff + skill fold-in) — shipped + E2E.
- ✅ Loopback-bypass (2026-05-30): `/save` + `/select` skip the token on 127.0.0.1/::1.
- ✅ Anti-mojibake guard (2026-06-02): `/save` rejects input containing U+FFFD (400)
  instead of silently persisting corrupted UTF-8. **Recipe §4 fixed: send the JSON
  over STDIN (`--data-binary @-`), never `-d` inline** — on Windows argv goes through
  the ANSI codepage and destroys `→`/accents. Test: `TestBadEnc`. (suite 9/9)
- ⏳ Wave 2 (backend migration / Antigravity post-2026-06-18) — deferred; BM25 fallback is the net.

> ⚠️ **Canonical source = `~/bin/piebald-memory-daemon/` (main.go etc.).** On 2026-06-02
> this copy (`src/`) was STALE (missing the loopback-bypass) and a rebuild from it
> regressed the live daemon. `src/` is now a FAITHFUL copy of the canonical one.
> **Before rebuilding, confirm you're on the canonical one** — or the two diverge again.

## TODO
- [ ] Decide the canonical source (this repo vs `~/bin`) + deploy script.
- [ ] Capturing `interrupt`/`set_model` is not this project's concern (it's piebald-remote's).
- [ ] (Wave 2) Implement the Antigravity backend before 2026-06-18.

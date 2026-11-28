# Fleet Always-On Assistant — End-to-End Design

An always-on, sleep-wake personal assistant and second-brain coordinator built natively into `fleet` as a single `CGO_ENABLED=0` Go binary with zero external Go dependencies. Hosted on a Mac mini M1 (8GB) Nebula lighthouse and accessed from any admin Mac or phone over Mobile Nebula via the embedded Fleet Console PWA.

---

## 1. Design Principles & Architecture

1. **Sleep-wake-repeat (0 tokens at rest):** Inspired by Anthropic's *Building always-on agents*, the assistant sleeps almost all the time (~20–30 MB baseline RAM inside `fleet serve`) and wakes only for discrete runs triggered by chat messages, timers, heartbeat checks, or human approvals.
2. **Nebula mesh identity:** No public HTTP ports or extra auth stack. All `/v1/assistant/*` routes and `/assistant-widget` are admin-only and authenticated via `fleet.Identify` (Nebula source IP) plus `Host`/`Origin` anti-DNS-rebinding checks.
3. **Single binary, auto-synced:** Embedded PWA assets (`go:embed`) + pure-Go SQLite (`modernc.org/sqlite`), updated automatically on the lighthouse via `fleet sync`.
4. **System 1 + System 2 separation:** Cheap, non-generative Decision Models (`google/gemini-decision-1` / `typesafe/jev`) handle routing/triage; OpenRouter LLMs handle reasoning with `zdr: true` and explicit `cache_control` prefix caching.
5. **Zero-exposure secrets & scoped blast radius:** No shell/bash tool is exposed to the assistant. Secrets live in a scoped 1Password vault (`Fleet-Agent`) resolved in memory at runtime and scrubbed from responses.

```text
Phone (Mobile Nebula PWA) / Admin Mac Browser
        │  HTTPS/HTTP + SSE over Nebula (:7443)
        ▼
fleet serve  (internal/server/assistant.go + internal/assistant/)
  ├── Embedded PWA (#chat, #skills, /assistant-widget sandboxed iframe)
  ├── ~/.fleet/assistant.db        (SQLite WAL: channels, messages, inbox, timers, approvals, outbox)
  ├── ~/.fleet/assistant.json      (OpenRouter/ZDR config, model slots, MCP servers, credential aliases)
  ├── ~/.fleet/op-service-account.token  (0600, optional 1Password Service Account token)
  ├── Single-worker Inbox Loop     (concurrency = 1 per wake-up)
  ├── 30s Scheduler                (assistant_timers + _agent/HEARTBEAT.md)
  ├── System-1 Decision Router     (OpenRouter /decisions -> fast / balanced / frontier)
  ├── System-2 Streaming Loop      (OpenRouter /chat/completions + cache_control + ZDR)
  ├── In-process Tools             (fleet_*, skill_*, vault_*, timer_*, outbox_notify, render_*, credential_*)
  └── Stdio MCPManager             (external MCP servers: gcal, gmail, search, etc.)
        │
        ▼
Obsidian Vault (e.g. ~/vault, synced via `obsidian-headless` LaunchAgent)
  ├── _agent/SOUL.md               (static directives)
  ├── _agent/HEARTBEAT.md          (periodic autonomous checklist)
  ├── _agent/memory/REFLECTIONS.md (condensed durable context, cached prefix)
  ├── _agent/memory/OBSERVATIONS.md (append-only dated Observer log, cached prefix)
  ├── _skills/<name>/SKILL.md      (progressive-disclosure skill library)
  ├── Daily/ · Inbox/              (auto-writable capture folders)
  └── Projects/ · People/ · ...    (read anywhere; writes require HITL approval)
```

---

## 2. The Five Always-On Primitives (`~/.fleet/assistant.db`)

Managed in `internal/assistant/store.go`:

1. **Event Inbox (`assistant_inbox`):** Durable queue with `dedupe_key` (`UNIQUE`). Chat messages, timer ticks, heartbeat ticks, and approval resolutions enqueue rows; a single worker (`runWorker`) claims and processes one event at a time so concurrent runs never collide.
2. **Timers (`assistant_timers`) & Heartbeat:** `runScheduler` ticks every 30 seconds, enqueuing due one-shot (`once`) or recurring (`interval`) timers plus `_agent/HEARTBEAT.md` when enabled and non-empty.
3. **Memory (`assistant_messages` + Vault files):** Active tail stays in `(channel_id, context_epoch)` (`Reset Context` bumps `context_epoch` while preserving UI scrollback). Background Observational Memory distills unobserved messages into `_agent/memory/OBSERVATIONS.md` and condenses `_agent/memory/REFLECTIONS.md`.
4. **Permissions & Deferred HITL Approvals (`assistant_approvals`):** Sensitive actions (`fleet_slack_send`, `vault_write`/`vault_append` outside `auto_write_dirs`, `http_request_with_credential` when `require_approval=true`, and configured external MCP tools) pause the run cleanly, persist a pending approval row, emit an inline **Approve / Deny** card over SSE, and consume zero tokens while waiting. Approving via `#chat` or `fleet assistant approve <id>` enqueues an `approval` event that executes the tool and resumes the conversation.
5. **Outbox (`assistant_outbox`):** Stores proactive notifications emitted via `outbox_notify` and fans them out over SSE (`outbox` event).

---

## 3. End-to-End Flows

### A. Interactive Chat & Streaming (`#chat`)
1. Browser subscribes to `GET /v1/assistant/stream` (SSE with 25s `: keepalive` comments; `sw.js` bypasses cache for `/v1/assistant/stream`).
2. User submits a prompt via `POST /v1/assistant/messages` (optionally selecting a model in the picker or prefixing `/astra`, `/opus`, `/sonnet`, `/muse`, `/deepseek`, `/nano`, `/auto`).
3. `Engine.EnqueueUserMessage` persists the user message, broadcasts `message_created`, enqueues an `inbox` item, and wakes `runWorker`.
4. `Engine.resolveModel` applies the 5-level override hierarchy:
   1. Leading slash alias in message (`/astra ...`, etc. — stripped from prompt text)
   2. Explicit UI/CLI `model` override
   3. Matching skill `SKILL.md` frontmatter `model:`
   4. Channel default `model` (`general`, `fleet-ops`, `briefing`, or custom)
   5. System role default (`default_chat` or `heartbeat`); if resolved to `auto`, queries OpenRouter `/decisions` (`google/gemini-decision-1`) for `fast` | `balanced` | `frontier` (falling back safely to `default_chat` on error).
5. `openRouterClient.StreamChat` sends the cache-ordered prompt (`SOUL.md` + skill catalog + Fleet standing memory → `REFLECTIONS.md` + `OBSERVATIONS.md` with `cache_control: ephemeral` → active tail) with `zdr: true`, streaming `delta`, `tool_start`, `tool_done`, `approval`, and `message_done` SSE events.

### B. Generative UI & Sandboxed Widgets
- **Prompt Contract (`buildPromptMessages`):** Injects a compact Generative UI contract describing the exact `render_card` payload schemas, `data-fleet-action` callbacks, host theme CSS variables (`--text`, `--muted`, `--border`, `--accent`, `--surface`), and strict no-external-URL CSP constraints.
- **Typed Native Cards (`render_card`):** `agents`, `agenda`, `choices`, `key_value`, and `markdown` rendered directly via DOM helpers in `internal/server/web/chat.js` (e.g. tappable choice buttons send follow-up prompts on mobile).
- **Interactive HTML Widgets (`render_widget`):** Rendered inside `<iframe sandbox="allow-scripts" src="/assistant-widget">` (omitting `allow-same-origin` so scripts run in an opaque `null` origin). `/assistant-widget` sets `X-Frame-Options: SAMEORIGIN` and `Content-Security-Policy: default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'self'`, communicating `#root` height and `[data-fleet-action]` clicks back to `#chat` via `postMessage`, and supporting live theme updates via `{ type: "theme" }` without reloading widget state. `#chat` reconciles messages incrementally so mounted interactive widgets remain live across turns.

### C. Observational Memory & Obsidian Second Brain
- **File layout (`internal/assistant/vault.go`):** Strict path jailing rejects `..`, absolute paths, NUL bytes, and symlink escapes.
- **Two-tier cross-device memory split:**
  - **Fleet shared memory (`~/.fleet/memory.db`):** Standing `global/always` rules and repo/device notes shared with Claude Code, Codex, and Antigravity via `fleet_memory_*`.
  - **Primary Agent Vault (`~/agent-ob-vault`):** Assistant directives (`_agent/SOUL.md`, `HEARTBEAT.md`), skills (`_skills/*/SKILL.md`), daily notes (`Daily/`), captures (`Inbox/`), and Observational Memory (`_agent/memory/OBSERVATIONS.md` + `REFLECTIONS.md`).
  - **Mounted Extra Vaults (`extra_vaults`):** Optional named vaults (e.g. `personal` at `~/personal-vault`) opened without scaffolding (`_agent/` and `_skills/` are never created inside them). `vault_search` spans `"all"` or a specific vault, `vault_read` reads any mounted vault, and `vault_write`/`vault_append` enforce per-vault `read_only` and `auto_write_dirs` policies (pausing protected writes behind `#chat` Approve/Deny cards).
- **Observer & Reflector (`RunObserver` / `RunReflector`):** Automatically triggers when unobserved messages exceed `observer_threshold_tokens` (default `12000`), distilling dated `🔴`/`🟡`/`🟢` bullets into `OBSERVATIONS.md` and marking messages `observed=1`. When `OBSERVATIONS.md` exceeds `reflector_threshold_bytes` (default `24000`), `RunReflector` condenses `REFLECTIONS.md` and archives the raw block to `_agent/memory/archive/YYYY-MM.md`, auto-committing to local `.git` if present.
- **Headless Obsidian Sync:** Managed on the lighthouse via `fleet assistant sync-install`, which installs the per-user LaunchAgent `io.github.bdatdo0601.fleet.obsidian-sync` (or `io.github.bdatdo0601.fleet.obsidian-sync.<name>` for extra vaults) running `ob sync --continuous`.

### D. Zero-Exposure 1Password Credentials & External MCPs
- **Lighthouse Service Account:** `fleet assistant op-setup` delivers a scoped `OP_SERVICE_ACCOUNT_TOKEN` (limited to your `Fleet-Agent` vault) over Nebula into `~/.fleet/op-service-account.token` (`0600`).
- **OpenRouter key without plaintext on disk:** Passing `--api-key-op-ref "op://Fleet-Agent/OpenRouter/credential"` stores only the `op://` reference in `assistant.json` and resolves the key into memory (5-minute cache).
- **1Password HTTP proxy (`http_request_with_credential`):** Looks up a configured `CredentialPolicy` alias, validates URL host against `allowed_hosts`, injects the header in memory, and scrubs the secret (and URL-encoded variant) from any response or error before returning to the LLM.
- **Stdio MCP Manager (`internal/assistant/mcpclient.go`):** Spawns enabled servers from `mcp_servers` lazily, resolving `env_refs` (`op://...`) straight into the child process `Env`, and exposes tools as `mcp__<server>__<tool>` (with optional per-tool HITL approval gating via `require_approval_tools`).

---

## 4. Setup & Verification Runbook

1. **Update lighthouse & Macs:**
   ```bash
   fleet sync
   ```
2. **Configure 1Password headless auth (recommended on lighthouse + admin Mac):**
   - Install `op` on the lighthouse (`brew install 1password-cli`) and create a Service Account scoped strictly to your `Fleet-Agent` vault.
   - From any admin Mac:
     ```bash
     fleet assistant op-setup --op-ref "op://Private/Fleet-Agent Service Account/credential"
     ```
3. **Link Obsidian Sync on the lighthouse (optional, recommended):**
   ```bash
   npm install -g obsidian-headless
   ob login
   mkdir -p ~/agent-ob-vault && cd ~/agent-ob-vault
   ob sync-setup --vault "Agent Vault"
   fleet assistant sync-install --vault ~/agent-ob-vault

   # Optional: mount and sync an existing personal Obsidian vault without scaffolding:
   mkdir -p ~/personal-vault && cd ~/personal-vault
   ob sync-setup --vault "Personal"
   fleet assistant sync-install --name personal --vault ~/personal-vault
   ```
   Then add `extra_vaults` (e.g. `[{"name":"personal","path":"~/personal-vault","read_only":false,"auto_write_dirs":["Inbox"]}]`) in `#chat` → **Settings**.
4. **Configure the assistant & verify all subsystems:**
   ```bash
   fleet assistant setup \
     --api-key-op-ref "op://Fleet-Agent/OpenRouter/credential" \
     --vault '~/vault' \
     --enable

   fleet assistant doctor
   ```
5. **Open Fleet Console:** Visit `http://10.213.0.1:7443/#chat` and `#skills` on your phone (Mobile Nebula) or laptop.

---

## 5. Roadmap — What Is Next

### Immediate Setup (Day 1)
- Run `fleet sync` on the Mac mini, connect `obsidian-headless` + 1Password Service Account (`op-setup`, `sync-install`), run `fleet assistant setup` + `fleet assistant doctor`, and pin `http://10.213.0.1:7443/#chat` to your iPhone Home Screen.

### Phase 2 — Self-Service MCPs, Automations & Simple PWA Alerts
- **Agent self-setup for external & internal MCPs:** Extend `MCPServerConfig` / `MCPManager` with Streamable HTTP/SSE transport (`transport: "stdio" | "http"`, `url`, `header_refs`) alongside `stdio` (`env_refs`) so both SaaS MCPs (Google Calendar, Brave/Exa, etc.) and internal Nebula MCPs work cleanly. Add `mcp_list`, `mcp_test`, and HITL-approved `mcp_configure` / `mcp_remove` tools (direct user chat only) so the assistant can discover 1Password `op://` references via `onepassword_list_items` / `onepassword_get_item_meta`, store non-secret JSON config in `~/.fleet/assistant.json` (never raw secrets, never auto-executing vault-synced commands), and default new servers to `require_approval_tools: ["*"]` until narrowed.
- **Simple PWA notifications (desktop / open tab):** Wire `outbox` and `approval` SSE events globally in `app.js` to in-app banners/toasts, browser `Notification` API (when hidden over HTTPS via `fleet tls`), and `navigator.setAppBadge`. Exclude phone wake pings / VAPID Web Push here — mobile push is deferred to Phase 4.
- **Automated `HEARTBEAT.md` + morning briefing:** Enable daily weekday briefing timer in `#briefing` combining waiting Fleet agents, Slack threads, and calendar/notes.

### Phase 3 — Full Dots / Duet Delegation (Extending Fleet Spawn)
- **Unified Fleet spawn + assistant tools:** Extract the existing `POST /v1/agents/spawn` & `/kill` orchestration (`slacker-worker` + `session.Start`) into shared `internal/core` methods and expose `fleet_list_paths`, `fleet_spawn_agent` (approval-gated by default, surfacing `worker_online`), and `fleet_kill_agent` via `FleetDeps`.
- **Event-driven delegation lifecycle:** Track assistant-spawned tasks (`assistant_delegations` mapping `job_id` + `session_name` → `channel_id`) and push state transitions (launch failure, `waiting_permission`, `waiting_question`, `idle` after working, `exited`) into `assistant_inbox` so the assistant sleeps at 0 tokens while the coding agent runs, then wakes automatically to summarize blockers or PRs back in `#chat`.
- **Vault diff & revision browser:** Surface recent `.git` commits and diffs in `#skills` for one-click audit/rollback of agent self-edits, plus `sync_to_fleet: true` skill distribution via `fleet sync`.

### Phase 4 — Outbound-Only Discord Webhook Integration
- **Outbound-only Discord Webhook:** Add a pluggable `Notifier` interface implemented first for Discord Webhooks (`op://Fleet-Agent/...` webhook URL in 1Password) — requiring zero public inbound ports, zero gateway bots, and zero sidecar processes on the Mac mini.
- **Dedicated read-only notification channel(s):** Post rich colored Discord embeds (for `urgent`/`normal` `assistant_outbox` items, pending HITL approvals, briefings, and delegated-agent updates) into dedicated read-only Discord notification channel(s) with deep links back to `#chat/<channel>`, preserving clean, meaningful alert history without mirroring or spoofing interactive Discord conversations.
- **Privacy boundary:** Keep outbound payloads to concise summaries + Fleet Console deep links (never raw secrets, vault note contents, or sensitive tool arguments).

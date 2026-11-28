# Fleet Always-On Assistant — End-to-End Design

An always-on, sleep-wake personal assistant and second-brain coordinator built natively into `fleet` as a single `CGO_ENABLED=0` Go binary with zero external Go dependencies. Hosted on a Mac mini (16GB) Nebula lighthouse and accessed from any admin Mac or phone over Mobile Nebula via the embedded Fleet Console PWA.

---

## 1. Design Principles & Architecture

1. **Sleep-wake-repeat (0 tokens at rest):** Inspired by Anthropic's *Building always-on agents*, the assistant sleeps almost all the time (~20–30 MB baseline RAM inside `fleet serve`) and wakes only for discrete runs triggered by chat messages, timers, heartbeat checks, or human approvals.
2. **Nebula mesh identity:** No public HTTP ports or extra auth stack. All `/v1/assistant/*` routes and `/assistant-widget` are admin-only and authenticated via `fleet.Identify` (Nebula source IP) plus `Host`/`Origin` anti-DNS-rebinding checks.
3. **Single binary, auto-synced:** Embedded PWA assets (`go:embed`) + pure-Go SQLite (`modernc.org/sqlite`), updated automatically on the lighthouse via `fleet sync`.
4. **System 1 + System 2 separation:** Cheap, non-generative Decision Models (`typesafe/jev-1.14`, ZDR-eligible) handle routing/triage; OpenRouter LLMs handle reasoning with `zdr: true` and explicit `cache_control` prefix caching.
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
4. `Engine.resolveTurnRoute` applies the 5-level override hierarchy:
   1. Leading slash alias in message (`/astra ...`, etc. — stripped from prompt text)
   2. Explicit UI/CLI `model` override
   3. Matching skill `SKILL.md` frontmatter `model:`
   4. Channel default `model` (`general`, `fleet-ops`, `briefing`, or custom)
   5. System role default (`heartbeat` uses `models.heartbeat`; interactive `chat` defaults to **Hybrid** auto-routing via `routing.mode`: `hybrid` | `gate` | `auto` | `off`). When auto-routing runs, OpenRouter `/api/alpha/decisions` (`typesafe/jev-1.14` with a 2s timeout and `provider: {zdr, data_collection}` when on `openrouter.ai`) evaluates `needs_frontier` (and `same_topic` when a Topic Grant is active) over the prompt + recent tail:
      - `< borderline_prob` (`0.60`): runs immediately on `default_chat` (`claude-haiku-5.5`).
      - `borderline_prob .. gate_prob` (`0.60–0.80`, `hybrid` mode): runs immediately on `default_chat` and attaches a 1-tap `frontier_retry` bar (`Retry with <frontier>` / `Retry + unlock topic`).
      - `>= gate_prob` (`0.80`): auto-approves Frontier if a context-scoped **Topic Grant** is active (same topic, within `grant_max_turns`, `grant_max_cost_usd`, `grant_idle_minutes`, and `daily_soft_cap_usd`); otherwise pauses with an inline HITL escalation card (`Use Frontier (this topic)` / `Once` / `Use Haiku`, no Discord alert). In `auto` mode it escalates unattended; on Decisions API error/timeout it fails open to `default_chat`.
5. `openRouterClient.StreamChat` sends the cache-ordered prompt (`SOUL.md` + skill catalog + Fleet standing memory → `REFLECTIONS.md` + `OBSERVATIONS.md` with `cache_control: ephemeral` → active tail) with `zdr: true`, streaming `delta`, `tool_start`, `tool_done`, `approval`, and `message_done` SSE events.

### B. Generative UI & Sandboxed Widgets
- **Prompt Contract (`buildPromptMessages`):** Injects a compact Generative UI contract describing the exact `render_card` payload schemas, `data-fleet-action` callbacks, host theme CSS variables (`--text`, `--muted`, `--border`, `--accent`, `--surface`), and strict no-external-URL CSP constraints.
- **Typed Native Cards (`render_card`):** `agents`, `agenda`, `choices`, `key_value`, and `markdown` rendered directly via DOM helpers in `internal/server/web/chat.js` (e.g. tappable choice buttons send follow-up prompts on mobile).
- **Interactive HTML Widgets (`render_widget`):** Rendered inside `<iframe sandbox="allow-scripts" src="/assistant-widget">` (omitting `allow-same-origin` so scripts run in an opaque `null` origin). `/assistant-widget` omits `X-Frame-Options` (which conflicts with opaque sandbox origins in Safari/WebKit PWAs) and enforces `Content-Security-Policy: default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'self'`, communicating `#root` height and `[data-fleet-action]` clicks back to `#chat` via `postMessage`, and supporting live theme updates via `{ type: "theme" }` without reloading widget state. `#chat` reconciles messages incrementally so mounted interactive widgets remain live across turns.

### C. Observational Memory & Obsidian Second Brain
- **File layout (`internal/assistant/vault.go`):** Strict path jailing rejects `..`, absolute paths, NUL bytes, and symlink escapes.
- **Two-tier cross-device memory split:**
  - **Fleet shared memory (`~/.fleet/memory.db`):** Standing `global/always` rules and repo/device notes shared with Claude Code, Codex, and Antigravity via `fleet_memory_*`.
  - **Primary Agent Vault (`~/agent-ob-vault`):** Assistant directives (`_agent/SOUL.md`, `HEARTBEAT.md`), skills (`_skills/*/SKILL.md` + optional companion files and `.fleet-skill.json` provenance), daily notes (`Daily/`), captures (`Inbox/`), and Observational Memory (`_agent/memory/OBSERVATIONS.md` + `REFLECTIONS.md`).
  - **Self-Installing Skill Catalog (`internal/assistant/skill_registry.go`):** Pure-Go registry client parallel to `mcp_*` self-setup: `skill_search` (queries `skills.sh/api/search`), `skill_fetch` (previews `owner/repo@skill`, GitHub URLs, `skills.sh` URLs, or direct HTTPS `SKILL.md` bundles), `skill_install` (direct chat only, HITL-approved with commit-SHA pinning, SSRF public-only egress, `sync_to_fleet` stripping, non-executable `0644` writes, git auto-commit), `skill_read(name, file?)` for companion files, and `skill_remove` (HITL-approved). The `#skills` Web Console also provides **Install from Registry** search and 1-click installation.
  - **Mounted Extra Vaults (`extra_vaults`):** Optional named vaults (e.g. `personal` at `~/personal-vault`) opened without scaffolding (`_agent/` and `_skills/` are never created inside them). `vault_search` spans `"all"` or a specific vault, `vault_read` reads any mounted vault, and `vault_write`/`vault_append` enforce per-vault `read_only` and `auto_write_dirs` policies (pausing protected writes behind `#chat` Approve/Deny cards).
- **Obsidian Vault hybrid search (`internal/assistant/vault_index.go`):** `assistant_vault_chunks` + `assistant_vault_chunks_fts` incrementally chunks `.md` files across the primary and `extra_vaults` (H1/H2/H3 aware) on mtime/size changes and fuses FTS5 BM25 + optional cosine vector similarity (`embeddings`, e.g. Ollama `nomic-embed-text`) via Reciprocal Rank Fusion (`k=60`), degrading cleanly to pure FTS5 when offline. Shared `~/.fleet/memory.db` (`internal/memory/embed.go`) supports the same hybrid FTS5 + `memories.embedding` search with async backfill (`fleet memory embed`) and upgraded Ollama curator defaults (`qwen3:8b`, `num_ctx: 8192`).
- **Local LLM routing (`local/<model>`):** `local_llm` lets `/local` turns (plus private roles like `observer`, `reflector`, or `heartbeat`) route to a local Ollama/MLX OpenAI-compatible server (`keep_alive: "2m"` scale-to-zero, `num_ctx: 16384`, flattened system prompt without `cache_control`).
- **Local voice transcription (`internal/assistant/transcribe.go`):** Optional one-shot `ffmpeg` + `whisper-cli` auto-transcribes uploaded voice/audio attachments on arrival (replacing placeholder text before routing) and exposes the `transcribe_audio` tool for sandbox or vault audio files.
- **Observer & Reflector (`RunObserver` / `RunReflector`):** Automatically triggers in three situations: (1) when a channel's unobserved messages exceed `observer_threshold_tokens` (default `3000`), (2) via the background scheduler when a channel has substantive unobserved turns idle for `observer_idle_minutes` (default `30`, `0` disables), and (3) asynchronously when resetting/clearing a channel (`New Chat` / `/reset`) using a pre-delete snapshot so unobserved history is preserved. Distills dated `🔴`/`🟡`/`🟢` bullets into `OBSERVATIONS.md` and marks messages `observed=1`. When `OBSERVATIONS.md` exceeds `reflector_threshold_bytes` (default `24000`), `RunReflector` condenses `REFLECTIONS.md` and archives the raw block to `_agent/memory/archive/YYYY-MM.md`, auto-committing to local `.git` if present.
- **Headless Obsidian Sync:** Managed on the lighthouse via `fleet assistant sync-install`, which installs the per-user LaunchAgent `io.github.bdatdo0601.fleet.obsidian-sync` (or `io.github.bdatdo0601.fleet.obsidian-sync.<name>` for extra vaults) running `ob sync --continuous`.

### D. Zero-Exposure 1Password Credentials & External MCPs
- **Lighthouse Service Account:** `fleet assistant op-setup` delivers a scoped `OP_SERVICE_ACCOUNT_TOKEN` (limited to your `Fleet-Agent` vault) over Nebula into `~/.fleet/op-service-account.token` (`0600`).
- **OpenRouter key without plaintext on disk:** Passing `--api-key-op-ref "op://Fleet-Agent/OpenRouter/credential"` stores only the `op://` reference in `assistant.json` and resolves the key into memory (5-minute cache).
- **1Password HTTP proxy (`http_request_with_credential`):** Looks up a configured `CredentialPolicy` alias, validates URL host against `allowed_hosts`, injects the header in memory, and scrubs the secret (and URL-encoded variant) from any response or error before returning to the LLM.
- **Stdio & HTTP MCP Manager (`internal/assistant/mcpclient.go`, `mcp_oauth.go`):** Spawns stdio servers or connects to Streamable HTTP/SSE servers (`2025-03-26`, cursor-paginated `tools/list`) from `mcp_servers` lazily, resolving `env_refs`/`header_refs` (`op://...`) in memory and supporting OAuth 2.0/2.1 Authorization Code + PKCE (`S256`) with automatic token refresh + `401` retry (persisting tokens in `assistant_mcp_oauth_tokens` and scrubbing all secrets from errors). Exposes tools as `mcp__<server>__<tool>` (with optional per-tool HITL approval gating via `require_approval_tools`).

#### Google Workspace Official MCP Setup
Google's official hosted Workspace MCP servers (`https://workspacemcp.googleapis.com/mcp/v1self`, `https://gmailmcp.googleapis.com/mcp/v1`, `https://calendarmcp.googleapis.com/mcp/v1`, etc.; use `/mcp/v1self` when form elicitation is not needed) require a Google Cloud OAuth 2.0 Client ID with **"Use this client for an AI-powered agent"** enabled under **Google Auth Platform → Clients**:
1. Store your OAuth client secret in 1Password (e.g. `op://Fleet-Agent/Google-MCP/client_secret`).
2. Configure the server in `#chat` → **Settings** (or via `mcp_configure`), using `"provider": "google"` (auto-detected for `*.googleapis.com` when `auth_url`/`token_url` are omitted, defaulting `access_type=offline&prompt=consent`):
   ```json
   [
     {
       "name": "gworkspace",
       "transport": "http",
       "url": "https://workspacemcp.googleapis.com/mcp/v1self",
       "enabled": true,
       "oauth": {
         "enabled": true,
         "provider": "google",
         "client_id": "<YOUR_AGENT_CLIENT_ID>.apps.googleusercontent.com",
         "client_secret_op_ref": "op://Fleet-Agent/Google-MCP/client_secret",
         "scopes": [
           "https://www.googleapis.com/auth/gmail.modify",
           "https://www.googleapis.com/auth/calendar"
         ]
       },
       "require_approval_tools": ["*"]
     }
   ]
   ```
3. Complete the one-time consent flow:
   - **In-chat interactive card (recommended):** Ask the assistant to configure or connect the server (`mcp_configure` / `mcp_oauth_connect`); it attaches a native `mcp_oauth` Generative UI card directly in `#chat` supporting 1-click browser sign-in, automatic callback updates, and inline callback URL/code paste (for Desktop loopback clients) plus a **Verify & Continue** button.
   - **Desktop OAuth client (CLI):** run `fleet assistant mcp-auth gworkspace` on your Mac (uses a local `http://127.0.0.1:<port>/oauth2callback` redirect while exchanging tokens on the lighthouse).
   - **Web OAuth client (with `fleet tls` HTTPS):** add `https://<your-fleet-domain>/v1/assistant/mcp/oauth/callback` as an authorized redirect URI in Google Cloud Console and click **Connect with Google** in `#chat` (or **Connect OAuth** in `#chat` → **Settings**).

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
- **Lighthouse Host Worker (host self-configuration):** `fleet assistant worker-install` installs and starts `io.github.bdatdo0601.fleet.slacker-worker` inside the lighthouse's `gui/<uid>` session (defaulting to the Antigravity desktop app for `agy` and Rex/tmux for `claude`/`codex`, with optional `--use-rex` / `--use-tmux` overrides), while keeping the lighthouse device (`mini`) out of `slacker.json`'s `default_devices` so normal coding spawns still route to laptops/workstations. A scaffolded `_skills/lighthouse-self-config/SKILL.md` playbook teaches the assistant when and how to delegate host dependency installs (`uv`, `node`, `brew` formulae) or stdio MCP debugging via `fleet_spawn_agent(device="<lighthouse>", harness="agy", repo_path="~")` (lighthouse spawns default to `agy` unless another harness is specified). Lighthouse spawns are restricted to direct user chat, always require explicit HITL approval (never bypassed by `auto_approve`), `fleet session start` automatically pre-trusts the target folder in `~/.claude.json`, `~/.codex/config.toml`, and `~/.gemini/antigravity-cli/settings.json` so spawned sessions don't hang at workspace trust dialogs, and `fleet serve` / `MCPManager` merge `~/.local/bin` and common toolchain paths so newly installed binaries work immediately without restarting `fleet serve`.

### Phase 4 — Outbound-Only Discord Webhook & Two-Way Gateway Bot
- **Outbound-only Discord Webhook (`NotificationsConfig`):** Pluggable `Notifier` (`DiscordWebhookNotifier`, `op://Fleet-Agent/...` webhook URL in 1Password) posting concise summary embeds + deep links into dedicated read-only Discord alert channel(s) (`#fleet-alerts`) with zero public inbound ports.
- **Two-way Discord Gateway Bot (`DiscordConfig` — Option B):** Outbound `wss://gateway.discord.gg/?v=10` WebSocket connection using `internal/ws` (`BotTokenOPRef`, `AuthorizedUserID`, optional `GuildID`, `CategoryName`, `AutoSyncChannels`, 1-to-1 `Channels` map, `assistant_discord_state` startup catch-up cursor). `assistant_messages` (`source`, `external_id`) remains the single source of truth across Fleet Console `#chat` and Discord:
  - **Automatic channel management (`auto_sync_channels`, default `true`):** With `Manage Channels`, the bot auto-detects a single server (or uses `guild_id`), creates/adopts the **`Fleet Assistant`** category and per-channel text channels on startup (`READY`), live on Web `+ New Channel`, or via `fleet assistant discord-sync` / `CHANNEL_CREATE` inside that category (persisting `discord.channels` without restarting the Gateway session and never deleting channels).
  - Messages from the authorized Discord user enqueue into `assistant_inbox` and appear live in `#chat` (`via Discord` badge); assistant replies post back cleanly (`render_card` embeds + deep link for `render_widget`).
  - Web/timer turns mirror to Discord with non-spoofed `> **You (Fleet Web):**` blockquotes (never impersonating the user's Discord account).
  - `/new` in Discord (or `New Chat` in Web) starts a new context epoch with a divider while preserving Discord history.
  - Pending HITL approvals render interactive **Approve / Deny** Discord buttons (`INTERACTION_CREATE`), verified against `AuthorizedUserID`, and `outbox_notify` embeds also deliver directly to mapped Discord channels (making the separate outbound webhook optional when the bot is enabled).
- **Privacy boundary:** Keep outbound payloads to concise summaries + Fleet Console deep links (never raw secrets, vault note contents, or sensitive tool arguments).

---

## 6. Sandboxed Computer (Apple `container` Micro-VM)

For document reading, conversion, data analysis, and file generation ("computer use" rather than software repo engineering), the assistant runs an isolated per-channel Linux micro-VM via Apple's native `container` CLI (macOS 26+ on Apple Silicon):

- **Scale-to-zero warm LRU pool & capability profiles:** Keeps up to `max_active` (default `2`, max `3`) per-channel micro-VMs (`fleet-sb-<channel>`) warm (`4` CPUs, `4096 MB` RAM, `50 MB` max file, `300s` max timeout by default; image builds use `4` CPUs / `4096 MB`, or `6144 MB` when `browser`/`office` profiles are enabled) and evicts LRU containers on capacity overflow or after `idle_minutes` (default `20m`) via `Engine.runScheduler`. Opt-in `profiles` (`browser` for headless Chromium/Playwright, `office` for LibreOffice headless conversion, `media` for `ffmpeg`) bake heavier runtimes cleanly into the cached image.
- **Offline by default + approval-gated internet:** Containers attach to an internal `fleet-sandbox-offline` `vmnet` network with `--no-dns`, `--read-only` rootfs, `--cap-drop ALL`, `--user 1000:1000`, and only `/work` mounted. When `sandbox_exec(..., internet=true)` is approved via HITL, `fleet` spawns an ephemeral authenticating `EgressProxy` on the internal gateway IP blocking loopback, RFC1918, CGNAT, Nebula mesh (`10.213.0.0/16`), and non-80/443 ports.
- **End-to-end `#chat` file loop:**
  - `POST /v1/assistant/files` stages uploaded files into `/work/in/` and attaches them to user messages.
  - `sandbox_publish_file` copies generated artifacts from `/work/out/` into `~/.fleet/assistant-files/`, registers them in `assistant_files`, and renders downloadable/previewable `files` cards via `GET /v1/assistant/files/{id}`.
  - `sandbox_vault_bridge` imports vault files into `/work/in/` or exports text results through `execVaultWriteTool` (preserving vault write/HITL policies).
- **Pre-baked toolkit + approval-gated self-updating packages:** `fleet assistant sandbox-build` builds a single-stage OCI image (`fleet-sandbox:latest` from `ghcr.io/astral-sh/uv:python3.13-bookworm-slim` using `uv` and `Acquire::ForceIPv4=true` with BuildKit layer caching) containing Python 3, `pypdf`, `pdfplumber`, `pymupdf`, `weasyprint`, `reportlab`, `python-docx`, `openpyxl`, `python-pptx`, `pandas`, `duckdb`, `matplotlib`, `pillow`, `pandoc`, `poppler-utils`, `tesseract-ocr`, `sqlite3`, `jq`, `ripgrep`, and `uv`. Never use multi-stage `FROM <local-tag>` (which triggers `oci-layout://` HTTP 500 errors in `apple/container`'s `container-builder-shim`). If a task needs an extra apt or pip package, `sandbox_install_packages` requests one-time HITL approval, persists the validated package into `assistant.json`, and rebuilds `fleet-sandbox:latest` cleanly on top of the cached base layers.
- **macOS `gui/<uid>` LaunchAgent bridge & Apple `container` operational notes:**
  - `fleet serve` runs as a `system` LaunchDaemon, whereas Apple's `container` (`brew install container`, formula — not `--cask`) registers `com.apple.container.apiserver` inside the user's `gui/<uid>` LaunchAgent domain. `fleet assistant sandbox-install` installs `io.github.bdatdo0601.fleet.sandbox-agent` in `gui/<uid>` exposing a `0600` Unix-socket bridge (`~/.fleet/sandbox.sock`), and `fleet sync` automatically restarts it when the `fleet` binary updates. On headless Mac mini hosts, enable macOS **Automatic Login** so `container` and `sandbox-agent` start automatically after reboots.
  - Image builds pass `--dns 1.1.1.1 --dns 8.8.8.8` to both `container builder start` and `container build`: `fleet edge` binds mesh DNS explicitly to `127.0.0.1:53` and `10.213.0.1:53` (never `0.0.0.0:53`), and `apple/container` 1.7.1 resets `buildkit` DNS to `[]` if `--dns` is omitted on `container build` itself. Build failures preserve the log tail for clear diagnostics.

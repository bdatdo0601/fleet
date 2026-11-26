# fleet

A self-hosted [Nebula](https://github.com/slackhq/nebula) network for your Macs and phones, run from
an always-on Mac at home. You register each device once by pasting two short blocks of text. After that,
every device keeps the same mesh IP on any network, and your admin devices can SSH or screen-share
into it from anywhere. No third-party servers are involved.

```
                         internet: only UDP 443 is forwarded, to the lighthouse
                                         │
 laptop   (admin)   ─┐                   ▼
 office   (managed) ─┼── Nebula ──► home Mac (lighthouse) 10.213.0.1   relay + CA
 phone    (admin)   ─┘   tunnels   ├─ ~/.fleet/fleet.db  registry (SQLite): devices, IPs, certs, revocations
                                   ├─ ~/.fleet/bin/      cross-compiled Go binaries + manifest.json
                                   └─ fleet serve :7443  API reachable only from inside the mesh
 lighthouse: `fleet sync` pulls git, cross-compiles darwin/arm64 + darwin/amd64, verifies `fleet serve` health,
             and publishes binaries to ~/.fleet/bin/
 every Mac:  `fleet sync` (launchd, every 5 min) pulls its config, cert, revocations, and updated `fleet` binary
             over Nebula, and restarts nebula only if something changed
```

- **No IPs to choose and no device files.** The registry assigns the next free IP in `10.213.0.0/16`,
  signs the certificate, and keeps the revocation list.
- **Private keys never leave their device.** Join requests and responses contain only public keys,
  certificates and config. That makes it safe to pass them by clipboard, AirDrop or Messages.
- **Admin from anywhere.** Admin devices approve, revoke and list devices through the mesh. Signing
  needs the CA passphrase, which you type each time; it is never stored.
- **Central policy.** Firewall rules and revocations live on the lighthouse, and every Mac picks up
  changes within about 5 minutes.
- **Single Go binary.** Compiled with `CGO_ENABLED=0` (pure-Go SQLite, embedded templates) plus `nebula` / `nebula-cert`
  from Homebrew. Only the lighthouse builds from git (`brew install go`); other Macs self-update over the mesh.

## Contents

[Commands](#commands) · [Setup](#setup) · [Adding devices](#adding-devices) ·
[Groups and access](#groups-and-access) · [Day to day](#day-to-day) · [Security model](#security-model) ·
[FAQ](#faq) · [Development](#development)

## Commands

| Where | Command | What it does |
|---|---|---|
| Lighthouse, once | `fleet init --host H [--route53-zone Z] [--lan-ip IP]` | Creates the CA, the registry and the lighthouse's cert, and starts nebula, the API, sync and (optionally) DNS updates |
| Lighthouse | `fleet ddns setup\|status\|policy` | Keeps the public DNS name pointed at home (Route 53) |
| New Mac | `fleet join NAME` | Prints a join request, waits for you to paste the approval, then installs everything |
| Admin device | `fleet approve [--group admin\|managed] [--replace]` | Turns a pasted join request into the response to paste back |
| Admin device | `fleet approve-mobile NAME` | Signs a public key from the Mobile Nebula app |
| Admin device | `fleet ls` | Lists devices: IP, group, last check-in, cert expiry, client runtime, status |
| Admin device | `fleet revoke NAME` | Cuts a device off everywhere within about 5 minutes |
| Admin device | `fleet set NAME --group G` | Moves a device to another group (re-signs its cert, also used to renew one) |
| Any Mac | `fleet agents [--all] [--waiting]` | Lists live AI agent sessions (`claude`, `codex`, `antigravity`), including sessions blocked on permission or question prompts |
| Any Mac | `fleet session <setup\|claude\|agy\|codex> ...` | Manages persistent Rex/tmux agent sessions (`start`, `start-attach`, `start-task`, `resume`, `attach`, `list`, `kill`, `kill-all`, `upgrade`) |
| Lighthouse / Any Mac | `fleet slacker <manifest\|setup\|listen\|worker\|...>` | Lighthouse-hosted Slack Socket Mode reaction dispatcher and per-Mac pull workers |
| Any Mac / Admin | `fleet memory ...` | Manages shared agent memory (`ls`, `search`, `set`, `import`, `promote`, `review`, `curator setup`) |
| Any Mac | `fleet mcp status\|install\|uninstall` | Checks or manages MCP + lifecycle hook configs for installed agent harnesses |
| Any Mac | `fleet status` | Shows the services, whether the lighthouse is reachable, and this device's IP |
| Any Mac | `fleet version` | Prints the compiled binary version, git commit, and platform |

## Setup

### 1. Prerequisites (lighthouse)

- **An always-on Mac** with Homebrew, `git`, and Go (`brew install go`).
  In Energy settings, turn on "Prevent automatic sleeping" and "Start up automatically after a power failure".
- **Router:** reserve the Mac's LAN IP and forward **UDP 443** to it. Check for CGNAT: the router's WAN IP must equal
  `curl ifconfig.me`. If they differ, port forwarding can't work, and the lighthouse has to run somewhere with a public IP.
- **A DNS name** that points at your home connection, e.g. `lighthouse.example.com`. If your DNS is on Route 53, `fleet`
  can keep it updated itself ([step 3](#3-dns-updates-route-53-optional)). Otherwise, use your router's DDNS.

### 2. Install

On the lighthouse:
```bash
git clone https://github.com/bdatdo0601/fleet.git ~/fleet
cd ~/fleet
CGO_ENABLED=0 go build -o /tmp/fleet ./cmd/fleet
sudo install -d -m 755 /usr/local/libexec/fleet
sudo install -m 755 /tmp/fleet /usr/local/libexec/fleet/fleet
rm /tmp/fleet
ln -s /usr/local/libexec/fleet/fleet /opt/homebrew/bin/fleet
```
`fleet sync` on the lighthouse updates its clone with `git pull --ff-only`, builds `darwin/arm64` and `darwin/amd64`
into `~/.fleet/bin/` (use `sudo fleet sync --rebuild` to force a rebuild), verifies `fleet serve` health, and serves
verified binaries to other Macs over Nebula (`GET /v1/binary`). To bootstrap a new Mac without Go installed, copy
`~/.fleet/bin/fleet-darwin-<arch>` from the lighthouse to `/usr/local/libexec/fleet/fleet` and symlink `/opt/homebrew/bin/fleet`.

### 3. DNS updates (Route 53, optional)

`fleet ddns` checks every 5 minutes whether your public IP still matches what the zone's own nameservers answer.
Only when it differs does it update the A record (TTL 60). After your home IP changes, devices reconnect within
about 6 minutes, and connections that are already open keep working.

1. **Find the hosted zone ID.** In the console: Route 53 → Hosted zones → your domain → *Hosted zone details*. Or with the CLI:
   `aws route53 list-hosted-zones-by-name --dns-name example.com --query 'HostedZones[0].Id' --output text`.
2. **Create an IAM user** `fleet-ddns`: no console access and no managed policies. Give it one **inline policy**, which
   `fleet ddns policy --zone-id Z… --name lighthouse.example.com` prints for you. The policy allows only an
   UPSERT of that single A record, so a stolen key can't touch the rest of the zone.
3. **Create an access key** for that user on its *Security credentials* tab (use case: "Application running outside AWS").
4. **Configure the lighthouse** with the key:
   ```bash
   brew install awscli
   aws configure --profile fleet-ddns     # paste the key; region us-east-1
   ```

### 4. Create the fleet

```bash
fleet init --host lighthouse.example.com --route53-zone Z… --lan-ip 192.168.1.50
```
`init` first checks the AWS setup (when `--route53-zone` is given), so nothing is created if it's missing.
Then it:
1. asks you to set a **CA passphrase**, typed twice. Store it in your password manager; you'll need it for every approval.
2. asks for sudo.
3. creates the CA and registry in `~/.fleet`.
4. installs and starts the services.
5. points DNS at your public IP.
6. prints the **CA id**.

Back up `~/.fleet`. To confirm it worked:
```bash
fleet status        # nebula, sync, serve and ddns loaded; lighthouse reachable as 10.213.0.1
fleet ddns status   # public IP == DNS answer
fleet ls            # one device: mini
```

## Adding devices

### Your first admin device (at home)

```bash
# laptop
fleet join laptop                    # prints a FLEET JOIN REQUEST block and waits
# lighthouse
fleet approve --group admin          # paste it; check the request id matches; type the passphrase
# laptop: paste the FLEET JOIN RESPONSE block. Check the CA id matches what init printed.
```
To move the blocks between the two Macs, use Universal Clipboard, AirDrop or Messages. From now on the laptop can
run `fleet approve`, `fleet ls` and the rest from any network. You don't need to log in to the lighthouse again.

### More Macs, from anywhere

On the new Mac, run `fleet join office` and send the request block to an admin device. There, run `fleet approve`,
adding `--group admin` only if this Mac should be able to manage others, and paste the response back.
The approval travels through the mesh to the lighthouse, and the new Mac joins right away.

### Phones and tablets

1. In **Mobile Nebula** (Defined Networking), add a site, have it generate a key, and copy the **public key**.
2. On an admin Mac, run `fleet approve-mobile phone` and paste the key. It prints a certificate, the CA and the lighthouse address.
3. Paste the certificate and CA into the app. Add host `10.213.0.1`, marked as a **lighthouse**, with address `lighthouse.example.com:443`.
4. In an SSH client (Blink, Termius), create a key and authorize it on the Macs you want to reach.

### Making a Mac reachable

- **SSH:** turn on System Settings → General → Sharing → **Remote Login**, then run `~/fleet/bin/ssh-harden`.
  It allows key-only logins, accepted only from mesh addresses. Add your admin devices' public keys to
  `~/.ssh/authorized_keys`.
- **Screen sharing:** turn on **Screen Sharing**, limited to your user, and connect a VNC client to `<mesh IP>:5900`.
  Leave "VNC viewers may control screen with password" off.

## Groups and access

Each certificate carries one group. The CA signs it into the certificate, so a device can't claim a different one.

| From ↓ / To → | lighthouse | admin Mac | managed Mac | phone |
|---|---|---|---|---|
| **admin** (laptop, phone) | SSH, VNC, ping, API | SSH, VNC, ping | SSH, VNC, ping | ping¹ |
| **managed** | ping, API (sync only) | ping | ping | ping¹ |
| **lighthouse** | — | ping | ping | ping¹ |

¹ Phones use the Mobile Nebula app's own firewall settings.

- Through the API, **admin** devices (and the lighthouse itself) can approve, revoke, list and regroup devices.
  **managed** devices can only pull their own config.
- Every other port is dropped. Traffic relayed through the lighthouse is checked by the same rules at the destination.
- To change the rules, edit [`templates/`](templates/) and push. Every Mac applies the new rules on its next sync.
- To move a device to another group, run `fleet set NAME --group admin|managed`.

## Day to day

- `ssh you@10.213.0.2`, or connect a VNC client to `10.213.0.2:5900`. Run `fleet ls` to see every device's IP.
- **gcloud login from a phone:** add `source ~/fleet/shell/gauth.zsh` to the target Mac's `~/.zshrc`. Then, over SSH,
  run `gauth`: open the URL on the phone, sign in, and paste the code back. Running `gauth` again reattaches if the connection drops.
- **Lost a device:** run `fleet revoke NAME`, and remove its SSH key from `authorized_keys` wherever you added it.
- **Reinstalled a Mac:** run `fleet join NAME --force` on it, then `fleet approve --replace`. It keeps its IP.
- **Logs:** `/var/log/fleet-sync.log` (every Mac), `~/.fleet/serve.log` and `~/.fleet/ddns.log` (lighthouse),
  `/opt/homebrew/var/log/nebula.log`.

## Web UI

`fleet serve` embeds and serves a mesh-only management console at `http://10.213.0.1:7443/` (also reachable at `http://127.0.0.1:7443/` on the lighthouse and from Mobile Nebula browsers on phones/tablets).

- **Same Nebula identity model:** Access is verified by mesh source IP (`fleet.Identify`) with anti-CSRF/anti-DNS-rebinding `Host` and `Origin` checks. Strangers and revoked devices receive `403`.
- **Role-scoped UI:** `admin` devices (`laptop`, `phone`, `ipadpro`, `mini`) see the full console — **Overview**, **Devices & PKI** (approve Mac/mobile blocks, change groups/renew certs, revoke, inspect configs & blocklist), **Agents & Messages**, **Shared Memory & Curator**, **Slacker** (Slack reaction dispatcher config & job queue), and **Lighthouse System** (PKI, published binaries, Route 53 DDNS check/sync/IAM policy, and service logs). `managed` devices see **Overview**, **Agents & Messages**, and **Shared Memory**.
- **Single binary:** All HTML/CSS/JS assets are embedded directly via `go:embed` with zero external CDN or Node runtime dependencies, so `fleet sync` updates the UI automatically with `fleet serve`.

## Agent sessions (MCP)

`fleet serve` also brokers live session discovery, transcript/git inspection, and peer messaging in `~/.fleet/agents.db`
on the lighthouse across all active Macs (`admin` and `managed` alike).

- **Auto-configured everywhere:** `fleet init`, `fleet join`, and `fleet sync` automatically register `fleet` as a local
  `stdio` MCP server and lifecycle hook handler for whichever harnesses exist on each Mac (**Claude Code**, **Codex CLI**,
  and **Antigravity**). Run `fleet mcp status` or `fleet mcp install` to inspect or refresh manually. In Codex CLI, approve
  new hook entries once via `/hooks` when prompted.
- **Friendly session names & titles:** Sessions automatically pick up harness titles (Claude `customTitle`/`aiTitle`,
  Codex `thread_name`, Antigravity `conversation_summaries`) and derive short handles (`name`), or can be renamed anytime
  with `fleet_set_name` / `fleet agents rename`. Peer tools accept `name`, `device/name`, `title`, short ID, or `session_id`.
- **Permission & question tracking:** Sessions report `waiting_permission` or `waiting_question` alongside
  `state_changed_at`, `waiting_seconds`, and structured `pending_action` metadata (`kind`, `tool`, `detail`, `event`), and
  stay live for 60 minutes while waiting (vs. 5 minutes for normal activity). Claude Code and Codex detect both permission
  and user-input prompts via `PermissionRequest`, `PreToolUse`, and `Notification`; Antigravity tracks explicit
  `ask_question` and `ask_permission` calls via `PreToolUse` (Antigravity does not yet expose an implicit tool-approval
  hook).
- **Real-time delivery & availability:** Every session reports `listening` and a `delivery` status (`immediate`,
  `next_hook`, `waiting_user`, `queued_idle`, or `offline`), and `fleet_send_message` / `fleet_ask_agent` report delivery
  state immediately (`fleet_ask_agent` fails fast on `queued_idle` and `waiting_user` unless `wait_if_idle=true`). Codex
  listens automatically inside its MCP server via `codex queue`; Claude Code listens automatically inside its MCP server
  via Claude's local messaging socket (`CLAUDE_CODE_MESSAGING_SOCKET`, with `fleet mcp install` / `fleet sync` defaulting
  `crossSessionInbound` to `"accept"` when unset); and Antigravity App runs a single global `fleet`
  sidecar (`fleet agents relay --harness antigravity`) that pushes messages via `agentapi`, with lifecycle hooks and
  manual `fleet agents wait` as fallbacks.
- **Tools exposed to agents:** `fleet_list_agents` (supports `state="waiting"`), `fleet_inspect_agent`, `fleet_send_message`,
  `fleet_check_inbox`, `fleet_ask_agent`, `fleet_set_name`, and `fleet_listen`.
- **CLI inspection:** Run `fleet agents` (or `fleet agents --waiting` / `--state ...`) from any Mac to view active
  sessions, names, listener status, wait durations, pending prompts, titles, and repos; `fleet agents rename <target> <name>`
  to rename a session; or `fleet agents wait --session <id>` to block until unread messages arrive.

## Shared agent memory

`fleet serve` stores a shared, cross-harness agent memory space in `~/.fleet/memory.db` (SQLite + FTS5 + revision history) on the lighthouse, scoped by `global`, `repo:<name>` (resolved from `git origin` so checkouts and worktrees share one scope), and `device:<name>`.

- **Automatic `SessionStart` injection:** Every new `claude`, `codex`, or `antigravity` session receives standing `always` preferences and a compact index of recent `searchable` notes at `SessionStart`, cached locally in `~/.fleet-cache/` for offline fallback.
- **Cross-harness MCP tools:** All three harnesses expose `fleet_memory_search`, `fleet_memory_get`, `fleet_memory_set`, and `fleet_memory_delete`.
- **One-way local backup:** `fleet memory import` (and automatic `fleet sync`) harvests local `~/.claude/projects/*/memory/*.md` (and `~/.codex/memories/*.md`) up to the lighthouse without overwriting `human`-edited or `locked` entries.
- **Async LLM curator (optional):** Configure an OpenRouter or OpenAI-compatible curator on the lighthouse from an admin Mac using 1Password (`fleet memory curator setup --provider openrouter --model openai/gpt-6-luna --op-ref "op://Vault/OpenRouter/credential"`). It runs automatically in the background to filter noise, merge duplicates, and auto-apply `global` promotions by default (opt out of any rule with `fleet memory rm`, or pass `--review-global` during setup to stage `global` promotions into `fleet memory review` / `approve` / `reject`).
- **Human CLI curation:** `fleet memory ls|search|get|set|edit|rm|promote|history|revert|context`.

## Session management (`fleet session`)

`fleet session` replaces the chezmoi `claude-session` and `agy-session` shell scripts with native Go session management for **Claude Code** (`claude`), **Antigravity** (`agy` / `antigravity`), and **Codex CLI** (`codex`).

- **Prerequisite check & setup:**
  ```bash
  fleet session setup --check    # inspect tmux, rex, lazygit, CLIs, tmux extended-keys, and hooks
  fleet session setup            # install missing tmux/lazygit via brew and configure hooks
  ```
- **Interactive sessions (`agy` defaults to the Antigravity desktop app so sessions sync to desktop & phone, with `--rex` / `--tmux` optional; `claude` and `codex` default to Rex with `lazygit` pane in git repos, or `--tmux`):**
  ```bash
  fleet session agy start my-task ~/workspace/repo       # Antigravity app (syncs to desktop & phone)
  fleet session agy start --rex my-task ~/workspace/repo # Rex CLI session (optional)
  fleet session claude start my-task ~/workspace/repo
  fleet session codex start --tmux my-task ~/workspace/repo
  fleet session claude attach my-task
  fleet session claude list
  fleet session claude resume my-task ~/workspace/repo <SESSION_UUID>
  fleet session claude kill my-task
  fleet session claude kill-all
  fleet session claude upgrade   # stops sessions, upgrades CLI, resumes exact conversations in place
  ```
- **Task-seeded sessions (`start-task`):** Reads a multiline prompt from stdin and launches safely:
  - `claude`: `--permission-mode manual --remote-control <NAME>` (manual tool approvals)
  - `agy`: submits a native conversation to the Antigravity app by default (or `--sandbox --mode plan --remote-control --prompt-interactive <PROMPT>` with `--rex` / `--tmux`)
  - `codex`: `-s workspace-write -a on-request` (workspace-write sandbox, on-request approvals) and automatically starts `codex remote-control` (sharing `codex app-server daemon` with the mobile app; disable Remote Control in Codex Desktop Settings if Desktop holds the host's slot)

### Cutover from shell launchers (`claude-session` / `agy-session`)

`fleet session` preserves the exact tmux session prefixes (`claude-`, `agy-`), Rex labels, and pane metadata options (`@claude_session`, `@agy_session`, `@claude_exit_option`, `@agy_exit_option`, `@agy_managed`) used by the chezmoi scripts.

1. Upgrade `fleet` everywhere (`fleet sync` on the lighthouse and target Macs).
2. Run `fleet session setup` on each Mac to update `~/.gemini/config/hooks.json` (`agy-session` entry) to call `fleet session agy __hook`.
3. Replace `~/.customscripts/claude-session` and `~/.customscripts/agy-session` (or add shell aliases/shims):
   ```sh
   alias claude-session='fleet session claude'
   alias agy-session='fleet session agy'
   ```
   Once `fleet session` is verified, remove the old bash scripts from `~/.local/share/chezmoi/dot_customscripts/` and apply chezmoi.

## Slack reaction dispatcher (`fleet slacker`)

`fleet slacker` runs one supervised Socket Mode listener per configured Slack account on the lighthouse (`fleet slacker listen` via `io.github.bdatdo0601.fleet.slacker`) and pull-based workers on target Macs (`fleet slacker worker` via a per-user `Aqua` LaunchAgent `io.github.bdatdo0601.fleet.slacker-worker`) so launched sessions run as the logged-in user.

### Setup (where to run each step)

1. **Update `fleet` everywhere (lighthouse first, then each target Mac):**
   Run `fleet sync` on the **lighthouse**, then run `fleet sync` on **every other Mac** (`laptop`, `office`, etc.). This builds/installs the updated binary and refreshes each harness's MCP config with `fleet_slack_channels`, `fleet_slack_read`, and `fleet_slack_send`.
2. **Install session prerequisites (each Mac that runs sessions):**
   ```bash
   fleet session setup --check    # inspect tmux, rex, lazygit, claude/agy/codex CLIs, and hooks
   fleet session setup            # installs missing tmux/lazygit via Homebrew & updates hooks
   ```
3. **Create Slack app(s) and save tokens in 1Password (browser + 1Password):**
   Print the Slack App Manifest YAML from **any Mac**:
   ```bash
   fleet slacker manifest
   ```
   The manifest configures Socket Mode, `reaction_added`, and User OAuth scopes (`reactions:read`, `channels:history`, `groups:history`, `im:history`, `mpim:history`, `channels:read`, `groups:read`, `users:read`, `search:read`, `chat:write`). For each workspace/account you want to connect (e.g. `personal` and `work`):
   - Open `https://api.slack.com/apps` -> **Create New App** -> **From a manifest** -> pick the workspace -> paste the manifest -> create.
   - Under **Basic Information -> App-Level Tokens**, generate a token with `connections:write` (`xapp-...`).
   - Under **OAuth & Permissions**, install the app to the workspace and copy the **User OAuth Token** (`xoxp-...`) — and optionally a **Bot User OAuth Token** (`xoxb-...`) if you want agent-sent messages to post as the bot instead of your user.
   - Store `app_token`, `user_token` (and optional `bot_token`) in a 1Password item (for example `Local/Fleet Slacker` or `Work/Fleet Slacker`).
4. **Configure accounts, channels, and emojis (any admin Mac, lighthouse, or Web UI `#slacker`):**
   ```bash
   # First / personal account
   fleet slacker setup --account personal --op-item "Local/Fleet Slacker" \
     --channel C0123456789=laptop:~/workspace/fleet \
     --emoji claude=claude --emoji agy=agy --emoji codex=codex

   # Optional additional account(s)
   fleet slacker setup --account work --op-item "Work/Fleet Slacker" \
     --bot-token-op-ref "op://Work/Fleet Slacker/bot_token" \
     --channel C9876543210=office:~/workspace/work-repo

   # Add/update mappings or inspect accounts anytime
   fleet slacker map channel --account work C9876543210 office ~/workspace/work-repo
   fleet slacker map emoji codex codex
   fleet slacker accounts
   ```
   Pass `--allow-unmapped-send` during `setup` (or toggle it in the Web UI at `http://10.213.0.1:7443/#slacker`) on any account where agents should be allowed to post to unmapped channels.
5. **Install & start background services (run as your logged-in user, without `sudo`):**
   - **On the lighthouse:** `fleet slacker install` installs and starts `io.github.bdatdo0601.fleet.slacker` for all enabled accounts. Use `fleet slacker install --role both` if the lighthouse itself is also a target device in a channel mapping.
   - **On each target Mac:** `fleet slacker install` installs and starts the per-user `Aqua` LaunchAgent `io.github.bdatdo0601.fleet.slacker-worker` so sessions launch as your logged-in macOS user.
6. **Verify (from any admin Mac):**
   ```bash
   fleet slacker status
   fleet slacker logs
   fleet slacker read --account personal --channel C0123456789 --limit 5
   ```
   React to a message in a mapped channel with `:claude:`, `:agy:`, or `:codex:`, then inspect `fleet slacker jobs` (or `#slacker` in the Web UI) and `fleet session <claude|agy|codex> list` on that Mac.

### Agent & CLI Slack reading and messaging

- **MCP tools (all Nebula devices):** `fleet_slack_channels` (lists configured accounts/mapped channels or Slack `users.conversations` when `include_all=true`), `fleet_slack_read` (reads channel history, a full thread via `thread_ts`, or `search.messages` queries during investigation), and `fleet_slack_send` (posts messages or thread replies using `bot_token` if configured, else `user_token`; restricted to mapped channels unless `allow_unmapped_send` is enabled on the account).
- **CLI / HTTP API:** `fleet slacker read [--account ID] [--channel ID] [--thread TS] [--query Q]` (`GET /v1/slacker/messages`), `GET /v1/slacker/channels`, and `fleet slacker send --channel ID [--account ID] [--thread TS] --text "..."` (`POST /v1/slacker/send`).

### Reliability & security

- Only `reaction_added` events from each account's configured `authorized_user_id` on mapped `(account, channel)` pairs and configured emojis are accepted.
- Jobs are persisted in `~/.fleet/slacker.db` (deduplicated by `event_id`, scoped by `account`) **before** acknowledging the Socket Mode envelope.
- Exact messages (including thread replies via `conversations.replies`) are fetched and passed via `fleet session <harness> start-task` in safe mode (`manual` / `--sandbox --mode plan` / `workspace-write`).
- Sleeping Macs pull their own jobs over Nebula when awake; deterministic session names (`slack-<channel>-<ts>` for `default`, `slack-<account>-<channel>-<ts>` for named accounts) and local worker receipts prevent duplicate launches across restarts.

### Cleanup

```bash
fleet slacker cleanup --uninstall --prune-jobs
```
Cleanup removes slacker services, local receipts/locks, and terminal DB job records while preserving all active tmux/Rex sessions and user repositories.

## Security model

- **Internet exposure:** only UDP 443 on the lighthouse. Nebula drops any packet that isn't from a
  certificate this CA signed, so port scans see nothing. SSH and the API are never port-forwarded.
- **API:** answers only loopback and mesh addresses. Nebula ties a mesh source IP to the sender's
  certificate, which is how the API knows who is calling. A LAN host can't spoof a mesh address,
  because the replies go into the tunnel and never reach it.
- **CA key:** lives only on the lighthouse, encrypted. The passphrase travels inside the tunnel for
  each approval and is used only in memory.
- **Paste channels:** both ends show a **request id** and a **CA id**. Compare them to detect a block
  that was swapped in transit.
- **Code updates run as root.** `fleet sync` on the lighthouse builds from this repo (and managed Macs
  download SHA-256-verified binaries from the lighthouse and apply them as root), so anyone who can push
  to the repo can get root on every Mac that follows it. If you fork this repo, protect your account with 2FA,
  and review anything before merging.

## FAQ

**Do I have to re-register a device when it changes networks?**
No. A device's identity is its certificate, not its network. It keeps its mesh IP at home, at the office, on hotel
Wi-Fi or on a hotspot. After a switch, it reports its new address to the lighthouse within about a minute.

**Do certificates renew automatically?**
No. Signing needs the CA passphrase, and it is never stored.
- **Phone and tablet certs last 1 year.** Renew with `fleet set phone --group admin`, then paste the new cert into the app.
  The old cert is revoked immediately, so do both steps together.
- **Mac certs** last until the CA expires.
- **The CA lasts 5 years.** After that, re-run setup and re-join every device.

**Can devices in different groups reach each other?**
Admin devices can reach everything on SSH and VNC. Managed machines can only ping and sync. See [Groups and access](#groups-and-access).

**Does my traffic go through the lighthouse?**
Only when two devices can't connect directly, for example a phone on cellular reaching a laptop behind a strict NAT.
Even then it stays end-to-end encrypted, so the lighthouse can't read it. Relayed traffic is limited by your home upload speed.

**What if a network blocks UDP?**
Nebula only uses UDP. Port 443 gets through most networks, because QUIC/HTTP3 uses it, but a network that blocks all UDP
can't join. The device reconnects on its own once it's back on a normal network.

**What happens when the lighthouse or my home internet is down?**
Existing connections keep working. New connections, syncs and approvals wait until the lighthouse is back.
A Mac has to be awake to be reachable.

**How do I manage the lighthouse without exposing SSH to the internet?**
Approve one admin device while you're at home. After that, `fleet` commands run from that device over the mesh. If you
need a shell on the lighthouse, SSH to `10.213.0.1` from an admin device after running `ssh-harden` there.

**I mistyped the CA passphrase.**
`fleet approve` asks again, up to 3 times. To test the passphrase on the lighthouse without changing anything, sign a
throwaway key: `nebula-cert sign -ca-key ~/.fleet/ca.key -ca-crt ~/.fleet/ca.crt -name probe -networks 10.213.255.254/16 -in-pub <a .pub from nebula-cert keygen> -out-crt /tmp/probe.crt`.

**I lost the CA passphrase. How do I start over?**
It can't be recovered. On the lighthouse:
1. Stop the services: `for s in serve sync ddns; do sudo launchctl bootout system/io.github.bdatdo0601.fleet.$s; done`.
2. Move the old state aside: `mv ~/.fleet ~/.fleet.old`.
3. Run `fleet init` again.

Then re-join every device. Macs use `fleet join NAME --force`, which reuses any pending key.

**How many devices can it handle?**
The `/16` network has room for about 65,000. The lighthouse's own load is tiny. The real limit is relayed traffic
using your home upload, which doesn't matter for SSH but can slow screen sharing.

**Linux or Windows?**
Not yet. `fleet join` / `sync` assume macOS (Homebrew and launchd). Phones work through Mobile Nebula.

## Development

```bash
go test -race ./...                                     # uses the real nebula/nebula-cert binaries
go vet ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/fleet
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/fleet
shellcheck bin/ssh-harden
```

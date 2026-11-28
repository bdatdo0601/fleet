---
name: fleet-agents
description: >-
  Coordinate across AI agent sessions on the Fleet Nebula mesh: list or inspect peer
  agents (claude, codex, antigravity), send/receive/ask peer messages, rename
  sessions, check push-delivery status, and spawn or kill agent sessions across
  devices. Use when interacting with peer agents or managing remote/local agent
  sessions.
---

# Fleet Agents CLI

Use your session ID (`--session <id>`) from the `SessionStart` hook when calling messaging, inbox, rename, or listen commands. Pass `--json` whenever you want structured output.

## Discovery & Inspection

- **List active sessions:**
  `fleet agents [--device D] [--repo R] [--harness claude|codex|antigravity] [--state working|waiting|waiting_permission|waiting_question|idle|exited] [--waiting] [--all] [--json]`
- **Inspect a session (cwd, branch, pending prompt, recent tail):**
  `fleet agents inspect TARGET [--tail N] [--json]`
  (`TARGET` accepts friendly name, `device/name`, title, short ID, or full `session_id`.)
- **List known directories and repos across devices (captured from heartbeats, spawns, and Slacker):**
  `fleet agents paths [--device D] [--q TEXT] [--json]`

## Peer Messaging

- **Send a message:**
  `fleet agents send TO --session SID [--reply-to MSG_ID] [--thread THREAD_ID] (--message TEXT | --stdin) [--json]`
  (`TO` accepts a session target, `device:<name>`, `repo:<name>`, or `*`.)
  When replying to a peer message, pass `--reply-to <id>` and `--thread <thread>` so `fleet agents ask` waiters receive the reply immediately.
- **Check inbox:**
  `fleet agents inbox --session SID [--all] [--no-mark-read] [--json]`
- **Ask a peer and wait for reply:**
  `fleet agents ask TO --session SID [--timeout 30] [--wait-if-idle] (--question TEXT | --stdin) [--json]`
- **Rename this session (or another session):**
  `fleet agents rename NAME --session SID [--json]`
  `fleet agents rename TARGET NAME [--json]`
- **Check push-delivery listener status:**
  `fleet agents listen [--session SID] [--json]`

## Watching & Wakeups

- **Subscribe your session to push wakeups when a peer finishes, blocks on permission/input, or exits:**
  `fleet agents watch TARGET --session SID [--on idle,waiting,exited] [--once] [--device D] [--pending] [--json]`
  (When `TARGET` transitions to a matching state, Fleet pushes a `[Fleet Watch]` message with the state change, pending action, and last response snippet directly into your session.)
- **Block in a script/command until a target session transitions:**
  `fleet agents watch TARGET --wait [--on idle,waiting,exited] [--timeout S] [--include-current] [--device D] [--pending] [--json]`
- **List or cancel active watches:**
  `fleet agents watch ls [--session SID] [--all] [--json]`
  `fleet agents watch rm ID_OR_TARGET [--session SID] [--json]`

## Spawning & Killing Agent Sessions

- **Spawn a new agent session (remote via Slacker worker, or local fallback):**
  `fleet agents spawn --harness claude|agy|codex [--name NAME] [--device DEV] [--repo PATH] [--prompt TEXT | --prompt-file PATH | --stdin] [--policy auto|bypass|safe] [--model MODEL] [--tmux] [--local] [--watch] [--watch-on EVENTS] [--watch-once] [--session SID] [--json]`
  - `--repo` defaults to your current working directory when targeting the local device (or omitted).
  - `--model` applies to Antigravity app sessions (`flash`, `pro`, etc.).
  - `--watch` automatically subscribes `--session SID` to state-transition wakeups from the spawned session.
  - If the target is the local machine and `slacker-worker` is offline (or `--local` is passed), `fleet` launches the session locally via `session.Start` and records the receipt.
- **Kill a single agent session:**
  `fleet agents kill TARGET [--session SID] [--force] [--json]`
  (Refuses to kill your own session unless `--force` is passed.)
- **Kill all matching agent sessions (destructive — confirm intent first):**
  `fleet agents kill-all --yes [--harness claude|agy|codex|all] [--device DEV] [--json]`

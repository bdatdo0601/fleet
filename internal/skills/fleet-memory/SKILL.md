---
name: fleet-memory
description: >-
  Search, read, save, update, and archive shared Fleet memories synced across
  devices and harnesses (claude, codex, antigravity). Use when looking up deeper
  repo or fleet notes, saving standing user preferences, or recording durable
  repository conventions.
---

# Fleet Memory CLI

Pass `--session <id>` from `SessionStart` so repo scope resolves accurately even if your shell cwd differs from the session's workspace, and pass `--json` for structured output.

## Commands

- **Search memories (defaults to `global` + current repo):**
  `fleet memory search QUERY [--scope current|global|repo:<name>|device:<name>|all] [--kind preference|feedback|project|reference] [--tier always|searchable] [--session SID] [--limit 20] [--json]`
- **List recent memories:**
  `fleet memory ls [--scope current|global|repo:<name>|device:<name>|all] [--kind K] [--tier T] [--session SID] [--limit 30] [--json]`
- **Read full memory body (and optional history):**
  `fleet memory get TARGET [--scope S] [--history] [--json]`
  (`TARGET` can be `<scope>/<key>` or `<key>` with `--scope`.)
- **Create or update a memory:**
  `fleet memory set TARGET [BODY] [--scope S] [--kind preference|feedback|project|reference] [--tier always|searchable] [--summary S] [--source fleet] [--session SID] [--stdin] [--json]`
- **Archive (soft-delete) a memory:**
  `fleet memory rm TARGET [--scope S] [--session SID] [--json]`

## Rules

- Always pass `--source fleet` when saving agent-authored memories so the curator can index/refine them.
- Save standing cross-fleet user preferences with `--scope global --kind preference --tier always`.
- Save durable repo conventions/facts with default repo scope (pass `--session SID` or `--scope repo:<name>`).
- Never save ephemeral task state, in-progress TODOs, or facts easily derived from git; update existing keys instead of creating duplicates.

---
name: fleet-slack
description: >-
  List configured Slack accounts and channels, read Slack channel history,
  threads, or search results, and post Slack messages or thread replies through
  Fleet Slacker. Use when investigating Slack threads or replying on Slack.
---

# Fleet Slack CLI

Use `fleet slacker` commands with `--json` when inspecting or posting to Slack via the lighthouse Slacker integration.

## Commands

- **List configured accounts and mapped channels (or all Slack channels with `--all`):**
  `fleet slacker channels [--account ID] [--all] [--limit 100] [--json]`
- **Read channel history, a thread, or search messages:**
  `fleet slacker read [--account ID] [--channel CHANNEL_ID] [--thread TS] [--query QUERY] [--oldest TS] [--latest TS] [--limit 25] [--json]`
- **Send a Slack message or thread reply:**
  `fleet slacker send --channel CHANNEL_ID [--account ID] [--thread TS] (--text TEXT | -) [--json]`

## Notes

- `--account` is optional when the target channel is mapped in Slacker or only one account is configured.
- Sending to unmapped channels requires `allow_unmapped_send` to be enabled on that Slack account.

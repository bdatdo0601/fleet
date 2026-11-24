"""Tests for cross-node agent registry, symmetric HTTP API, MCP stdio, hooks, and config installer."""

from __future__ import annotations

import io
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest import mock

from fleetctl import cli, hooks, mcp, mcp_install, pki, waiter
from fleetctl.agents import AgentError, AgentHub
from fleetctl.client import ApiError, call
from fleetctl.core import Fleet
from fleetctl.server import make_server

PASSPHRASE = "correct horse battery staple"


def keypair(directory: Path, name: str) -> tuple[Path, str]:
    key, pub = directory / f"{name}.key", directory / f"{name}.pub"
    pki.keygen(key, pub)
    return key, pub.read_text()


class AgentHubTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.hub = AgentHub(self.tmp / "agents.db")
        self.addCleanup(self.hub.close)

    def test_heartbeat_placeholder_promotion_and_deregister(self) -> None:
        # 1. MCP stdio registers a pid placeholder
        hb1 = self.hub.heartbeat(
            device="laptop",
            harness="claude",
            raw_id="pid-4242",
            pid=4242,
            cwd="/Users/datdo/proj",
            repo="proj",
            branch="main",
        )
        self.assertEqual(hb1["session_id"], "laptop:claude:pid-4242")

        # Another agent messages the placeholder before a hook runs
        self.hub.heartbeat(device="office", harness="codex", raw_id="codex-1111", cwd="/tmp/o", repo="other")
        msg = self.hub.send_message(
            device="office",
            from_session="office:codex:codex-1111",
            to="laptop:claude:pid-4242",
            body="check api.py",
        )
        self.assertEqual(msg["to_session"], "laptop:claude:pid-4242")

        # 2. Hook arrives with real session UUID for same (device, harness, cwd)
        hb2 = self.hub.heartbeat(
            device="laptop",
            harness="claude",
            raw_id="c779894d-fe1c-41c0",
            cwd="/Users/datdo/proj",
            repo="proj",
            branch="main",
            summary="fix bug",
            tail=[{"role": "user", "text": "fix bug"}],
            drain_inbox=True,
        )
        self.assertEqual(hb2["session_id"], "laptop:claude:c779894d-fe1")
        self.assertEqual(len(hb2["unread"]), 1)
        self.assertEqual(hb2["unread"][0]["body"], "check api.py")

        # Placeholder row is gone and pid was preserved
        live = self.hub.list_sessions(device="laptop")
        self.assertEqual(len(live), 1)
        self.assertEqual(live[0]["session_id"], "laptop:claude:c779894d-fe1")
        self.assertEqual(live[0]["pid"], 4242)

        # Short suffix resolution works
        detail = self.hub.get_session("c779894d-fe1")
        self.assertEqual(detail["tail"], [{"role": "user", "text": "fix bug"}])

        # Deregister marks exited and hides from default list
        self.hub.deregister(device="laptop", harness="claude", pid=4242)
        self.assertEqual(self.hub.list_sessions(device="laptop"), [])
        self.assertEqual(len(self.hub.list_sessions(device="laptop", include_stale=True)), 1)

    def test_stale_filtering_and_lookup_errors(self) -> None:
        self.hub.heartbeat(device="mini", harness="claude", raw_id="aaa111", cwd="/a")
        self.hub.heartbeat(device="laptop", harness="codex", raw_id="aaa111", cwd="/b")
        with self.assertRaisesRegex(AgentError, "ambiguous"):
            self.hub.get_session("aaa111")
        with self.assertRaisesRegex(AgentError, "no agent session"):
            self.hub.get_session("does-not-exist")

        old_ts = (datetime.now(timezone.utc) - timedelta(minutes=10)).replace(microsecond=0).isoformat()
        self.hub.conn.execute(
            "UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?",
            (old_ts, "mini:claude:aaa111"),
        )
        live = self.hub.list_sessions()
        self.assertEqual([s["session_id"] for s in live], ["laptop:codex:aaa111"])
        self.assertEqual(len(self.hub.list_sessions(include_stale=True)), 2)

    def test_broadcast_independent_reads_and_ask_wakeup(self) -> None:
        self.hub.heartbeat(device="mini", harness="claude", raw_id="s1", repo="fleet", cwd="/f")
        self.hub.heartbeat(device="laptop", harness="codex", raw_id="s2", repo="fleet", cwd="/f")
        self.hub.heartbeat(device="office", harness="antigravity", raw_id="s3", repo="other", cwd="/o")

        # Repo broadcast reaches s2 (not s1 sender, not s3 other repo)
        self.hub.send_message(
            device="mini",
            from_session="mini:claude:s1",
            to="repo:fleet",
            body="rebasing main",
            known_devices={"mini", "laptop", "office"},
        )
        self.assertEqual(len(self.hub.fetch_inbox(device="laptop", session_id="laptop:codex:s2")), 1)
        self.assertEqual(self.hub.fetch_inbox(device="laptop", session_id="laptop:codex:s2"), [])
        self.assertEqual(self.hub.fetch_inbox(device="office", session_id="office:antigravity:s3"), [])

        # Cluster broadcast has independent per-session read state
        self.hub.send_message(
            device="mini",
            from_session="mini:claude:s1",
            to="*",
            body="deploying",
            known_devices={"mini", "laptop", "office"},
        )
        self.assertEqual(len(self.hub.fetch_inbox(device="laptop", session_id="laptop:codex:s2")), 1)
        self.assertEqual(len(self.hub.fetch_inbox(device="office", session_id="office:antigravity:s3")), 1)

        # Device name target routes to all sessions on that device
        self.hub.send_message(
            device="mini",
            from_session="mini:claude:s1",
            to="office",
            body="ping office",
            known_devices={"mini", "laptop", "office"},
        )
        self.assertEqual(len(self.hub.fetch_inbox(device="office", session_id="office:antigravity:s3")), 1)

        # Concurrent ask() wakes up on reply
        def responder() -> None:
            for _ in range(40):
                msgs = self.hub.fetch_inbox(device="laptop", session_id="laptop:codex:s2")
                if msgs:
                    self.hub.send_message(
                        device="laptop",
                        from_session="laptop:codex:s2",
                        to=msgs[0]["from_session"],
                        body="tests pass",
                        reply_to=msgs[0]["id"],
                    )
                    return
                threading.Event().wait(0.02)

        t = threading.Thread(target=responder, daemon=True)
        t.start()
        res = self.hub.ask(
            device="mini",
            from_session="mini:claude:s1",
            to="laptop:codex:s2",
            body="status?",
            timeout=3.0,
        )
        t.join(timeout=2.0)
        self.assertFalse(res["timed_out"])
        self.assertEqual(res["reply"]["body"], "tests pass")

        # Ask timeout fallback
        timeout_res = self.hub.ask(
            device="mini",
            from_session="mini:claude:s1",
            to="office:antigravity:s3",
            body="anyone there?",
            timeout=0.1,
        )
        self.assertTrue(timeout_res["timed_out"])
        self.assertIsNone(timeout_res["reply"])


class SymmetricApiAndMcpTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        _, mini_pub = keypair(self.tmp, "mini")
        self.fleet = Fleet.initialize(
            root=self.tmp / "state",
            host="home.example.com",
            port=443,
            lan_ip="192.168.1.50",
            passphrase=PASSPHRASE,
            lighthouse_pubkey=mini_pub,
        )
        self.addCleanup(self.fleet.close)
        _, laptop_pub = keypair(self.tmp, "laptop")
        self.fleet.approve(name="laptop", pubkey=laptop_pub, kind="mac", group="admin", passphrase=PASSPHRASE)
        _, office_pub = keypair(self.tmp, "office")
        self.fleet.approve(name="office", pubkey=office_pub, kind="mac", group="managed", passphrase=PASSPHRASE)

        httpd = make_server(self.fleet, "127.0.0.1", 0, quiet=True)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        self.addCleanup(httpd.server_close)
        self.addCleanup(httpd.shutdown)
        self.api = f"http://127.0.0.1:{httpd.server_address[1]}"

    def test_managed_mac_has_symmetric_agent_access_but_not_admin_routes(self) -> None:
        laptop = self.fleet.registry.get("laptop")
        office = self.fleet.registry.get("office")

        with mock.patch.object(self.fleet, "identify", return_value=laptop):
            call(
                self.api,
                "POST",
                "/v1/agents/heartbeat",
                {"harness": "claude", "raw_id": "lap1", "cwd": "/repo", "repo": "app", "summary": "working on auth"},
            )

        with mock.patch.object(self.fleet, "identify", return_value=office):
            call(
                self.api,
                "POST",
                "/v1/agents/heartbeat",
                {"harness": "codex", "raw_id": "off1", "cwd": "/repo", "repo": "app"},
            )
            agents = call(self.api, "GET", "/v1/agents")["agents"]
            self.assertEqual({a["device"] for a in agents}, {"laptop", "office"})
            inspected = call(self.api, "GET", "/v1/agents/laptop:claude:lap1")["agent"]
            self.assertEqual(inspected["summary"], "working on auth")
            sent = call(
                self.api,
                "POST",
                "/v1/agents/send",
                {"harness": "codex", "session_id": "office:codex:off1", "to": "lap1", "body": "hello"},
            )
            self.assertEqual(sent["message"]["to_session"], "laptop:claude:lap1")

            # Cannot spoof another device's session_id
            with self.assertRaisesRegex(ApiError, "HTTP 403"):
                call(
                    self.api,
                    "POST",
                    "/v1/agents/send",
                    {"harness": "claude", "session_id": "laptop:claude:lap1", "to": "off1", "body": "spoof"},
                )
            # Admin routes remain protected
            with self.assertRaisesRegex(ApiError, "HTTP 403"):
                call(self.api, "GET", "/v1/devices")

        with (
            mock.patch.object(self.fleet, "identify", return_value=None),
            self.assertRaisesRegex(ApiError, "HTTP 403"),
        ):
            call(self.api, "GET", "/v1/agents")

    def test_mcp_stdio_roundtrip_and_deregister_on_eof(self) -> None:
        self.fleet.agents.heartbeat(
            device="mini",
            harness="antigravity",
            raw_id="conv-9999",
            cwd="/srv",
            repo="infra",
            summary="checking logs",
            tail=[{"role": "user", "text": "check logs"}],
        )
        # Pre-register stdio placeholder and queue a message for it before serve_stdio runs
        ppid = os.getppid()
        self.fleet.agents.heartbeat(
            device="mini",
            harness="claude",
            raw_id=f"pid-{ppid}",
            pid=ppid,
            cwd=os.getcwd(),
        )
        self.fleet.agents.send_message(
            device="mini",
            from_session="mini:antigravity:conv-9999",
            to=f"mini:claude:pid-{ppid}",
            body="see line 12",
        )

        def ask_responder() -> None:
            for _ in range(50):
                msgs = self.fleet.agents.fetch_inbox(device="mini", session_id="mini:antigravity:conv-9999")
                q = [m for m in msgs if m["body"] == "which port?"]
                if q:
                    self.fleet.agents.send_message(
                        device="mini",
                        from_session="mini:antigravity:conv-9999",
                        to=q[0]["from_session"],
                        body="port 7443",
                        reply_to=q[0]["id"],
                    )
                    return
                threading.Event().wait(0.02)

        t = threading.Thread(target=ask_responder, daemon=True)
        t.start()
        requests = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-03-26"}},
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
            {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "fleet_list_agents", "arguments": {}}},
            {
                "jsonrpc": "2.0",
                "id": 4,
                "method": "tools/call",
                "params": {"name": "fleet_inspect_agent", "arguments": {"agent_id": "conv-9999"}},
            },
            {
                "jsonrpc": "2.0",
                "id": 5,
                "method": "tools/call",
                "params": {"name": "fleet_check_inbox", "arguments": {}},
            },
            {
                "jsonrpc": "2.0",
                "id": 6,
                "method": "tools/call",
                "params": {"name": "fleet_send_message", "arguments": {"to": "conv-9999", "message": "got it"}},
            },
            {
                "jsonrpc": "2.0",
                "id": 7,
                "method": "tools/call",
                "params": {
                    "name": "fleet_ask_agent",
                    "arguments": {"to": "conv-9999", "question": "which port?", "timeout_seconds": 3},
                },
            },
        ]
        stdin = io.StringIO("\n".join(json.dumps(r) for r in requests) + "\n")
        stdout = io.StringIO()
        rc = mcp.serve_stdio("claude", stdin=stdin, stdout=stdout, api_url=self.api, heartbeat_interval=60.0)
        t.join(timeout=2.0)
        self.assertEqual(rc, 0)

        responses = [json.loads(line) for line in stdout.getvalue().splitlines() if line.strip()]
        self.assertEqual([r["id"] for r in responses], [1, 2, 3, 4, 5, 6, 7])
        tool_names = [t["name"] for t in responses[1]["result"]["tools"]]
        self.assertEqual(
            tool_names,
            [
                "fleet_list_agents",
                "fleet_inspect_agent",
                "fleet_send_message",
                "fleet_check_inbox",
                "fleet_ask_agent",
                "fleet_set_name",
                "fleet_listen",
            ],
        )
        listed = json.loads(responses[2]["result"]["content"][0]["text"])["agents"]
        self.assertTrue(any(a["session_id"] == "mini:antigravity:conv-9999" for a in listed))
        inspected = json.loads(responses[3]["result"]["content"][0]["text"])["agent"]
        self.assertEqual(inspected["tail"], [{"role": "user", "text": "check logs"}])
        inbox = json.loads(responses[4]["result"]["content"][0]["text"])["messages"]
        self.assertEqual(inbox[0]["body"], "see line 12")
        asked = json.loads(responses[6]["result"]["content"][0]["text"])
        self.assertFalse(asked["timed_out"])
        self.assertEqual(asked["reply"]["body"], "port 7443")
        # After stdin EOF, stdio session was deregistered
        live_after = self.fleet.agents.list_sessions()
        self.assertEqual([a["session_id"] for a in live_after], ["mini:antigravity:conv-9999"])


class HooksAndTranscriptTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        _, mini_pub = keypair(self.tmp, "mini")
        self.fleet = Fleet.initialize(
            root=self.tmp / "state",
            host="home.example.com",
            port=443,
            lan_ip="192.168.1.50",
            passphrase=PASSPHRASE,
            lighthouse_pubkey=mini_pub,
        )
        self.addCleanup(self.fleet.close)
        httpd = make_server(self.fleet, "127.0.0.1", 0, quiet=True)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        self.addCleanup(httpd.server_close)
        self.addCleanup(httpd.shutdown)
        self.api = f"http://127.0.0.1:{httpd.server_address[1]}"

    def test_transcript_tail_parsers(self) -> None:
        claude_log = self.tmp / "claude.jsonl"
        claude_log.write_text(
            "\n".join([
                json.dumps({"type": "user", "isMeta": True, "message": {"role": "user", "content": "meta"}}),
                json.dumps({"type": "user", "message": {"role": "user", "content": "refactor auth"}}),
                json.dumps({
                    "type": "assistant",
                    "message": {
                        "role": "assistant",
                        "content": [
                            {"type": "thinking", "thinking": "secret"},
                            {"type": "tool_use", "name": "Edit", "input": {"file": "auth.py"}},
                            {"type": "text", "text": "Updated auth.py"},
                        ],
                    },
                }),
            ])
        )
        summary, tail = hooks.read_transcript_tail("claude", str(claude_log))
        self.assertEqual(summary, "refactor auth")
        self.assertEqual(
            tail,
            [
                {"role": "user", "text": "refactor auth"},
                {"role": "tool", "text": "Edit(file=auth.py)"},
                {"role": "assistant", "text": "Updated auth.py"},
            ],
        )

        codex_log = self.tmp / "codex.jsonl"
        codex_log.write_text(
            "\n".join([
                json.dumps({
                    "type": "response_item",
                    "payload": {
                        "type": "message",
                        "role": "user",
                        "content": [{"type": "input_text", "text": "<environment_context>skip</environment_context>"}],
                    },
                }),
                json.dumps({
                    "type": "response_item",
                    "payload": {
                        "type": "message",
                        "role": "user",
                        "content": [{"type": "input_text", "text": "run tests"}],
                    },
                }),
                json.dumps({
                    "type": "response_item",
                    "payload": {"type": "function_call", "name": "exec_command", "arguments": "pytest"},
                }),
            ])
        )
        c_sum, c_tail = hooks.read_transcript_tail("codex", str(codex_log))
        self.assertEqual(c_sum, "run tests")
        self.assertEqual(len(c_tail), 2)

        agy_log = self.tmp / "agy.jsonl"
        agy_log.write_text(
            "\n".join([
                json.dumps({"type": "USER_INPUT", "content": "<USER_REQUEST>\ncheck build\n</USER_REQUEST>"}),
                json.dumps({"type": "PLANNER_RESPONSE", "content": "Running build", "tool_calls": [{"name": "run"}]}),
            ])
        )
        a_sum, a_tail = hooks.read_transcript_tail("antigravity", str(agy_log))
        self.assertEqual(a_sum, "check build")
        self.assertEqual([t["role"] for t in a_tail], ["user", "assistant", "tool"])

    def test_hook_lifecycle_antigravity_and_claude(self) -> None:
        # Register sender
        self.fleet.agents.heartbeat(device="mini", harness="codex", raw_id="sender-1", cwd="/tmp")

        # 1. Antigravity SessionStart injects session ID
        out = io.StringIO()
        hooks.run_hook(
            "antigravity",
            stdin=io.StringIO(json.dumps({
                "hookEventName": "SessionStart",
                "conversationId": "40f94b03-1363",
                "cwd": str(self.tmp),
            })),
            stdout=out,
            api_url=self.api,
        )
        start_resp = json.loads(out.getvalue())
        self.assertIn("mini:antigravity:40f94b03-136", start_resp["injectSteps"][0]["systemMessage"]["systemMessage"])

        # Queue a message for Antigravity
        self.fleet.agents.send_message(
            device="mini",
            from_session="mini:codex:sender-1",
            to="mini:antigravity:40f94b03-136",
            body="please check schema",
        )

        # 2. Antigravity PostToolUse must return {} and NOT drain the message
        out = io.StringIO()
        hooks.run_hook(
            "antigravity",
            stdin=io.StringIO(json.dumps({
                "hookEventName": "PostToolUse",
                "conversationId": "40f94b03-1363",
                "cwd": str(self.tmp),
            })),
            stdout=out,
            api_url=self.api,
        )
        self.assertEqual(json.loads(out.getvalue()), {})

        # 3. PreInvocation picks up the queued message
        out = io.StringIO()
        hooks.run_hook(
            "antigravity",
            stdin=io.StringIO(json.dumps({
                "hookEventName": "PreInvocation",
                "conversationId": "40f94b03-1363",
                "cwd": str(self.tmp),
            })),
            stdout=out,
            api_url=self.api,
        )
        pre_resp = json.loads(out.getvalue())
        self.assertIn("please check schema", pre_resp["injectSteps"][0]["systemMessage"]["systemMessage"])

        # 4. Stop continues once if unread, then allows when continuationActive=True
        self.fleet.agents.send_message(
            device="mini",
            from_session="mini:codex:sender-1",
            to="mini:antigravity:40f94b03-136",
            body="one more question",
        )
        out = io.StringIO()
        hooks.run_hook(
            "antigravity",
            stdin=io.StringIO(json.dumps({
                "hookEventName": "Stop",
                "conversationId": "40f94b03-1363",
                "cwd": str(self.tmp),
                "continuationActive": False,
                "terminationReason": "NO_TOOL_CALL",
            })),
            stdout=out,
            api_url=self.api,
        )
        self.assertEqual(json.loads(out.getvalue())["decision"], "continue")

        self.fleet.agents.send_message(
            device="mini",
            from_session="mini:codex:sender-1",
            to="mini:antigravity:40f94b03-136",
            body="not injected on loop",
        )
        out = io.StringIO()
        hooks.run_hook(
            "antigravity",
            stdin=io.StringIO(json.dumps({
                "hookEventName": "Stop",
                "conversationId": "40f94b03-1363",
                "cwd": str(self.tmp),
                "continuationActive": True,
                "terminationReason": "NO_TOOL_CALL",
            })),
            stdout=out,
            api_url=self.api,
        )
        self.assertEqual(json.loads(out.getvalue()), {"decision": "allow"})

        # 5. Claude Stop blocks on unread unless stop_hook_active=True; offline fails open
        self.fleet.agents.heartbeat(device="mini", harness="claude", raw_id="claude-1", cwd=str(self.tmp))
        self.fleet.agents.send_message(
            device="mini",
            from_session="mini:codex:sender-1",
            to="mini:claude:claude-1",
            body="urgent",
        )
        out = io.StringIO()
        hooks.run_hook(
            "claude",
            stdin=io.StringIO(json.dumps({
                "hook_event_name": "Stop",
                "session_id": "claude-1",
                "cwd": str(self.tmp),
                "stop_hook_active": False,
            })),
            stdout=out,
            api_url=self.api,
        )
        self.assertEqual(json.loads(out.getvalue())["decision"], "block")

        out = io.StringIO()
        rc = hooks.run_hook(
            "claude",
            stdin=io.StringIO(json.dumps({"hook_event_name": "UserPromptSubmit", "session_id": "c1", "cwd": str(self.tmp)})),
            stdout=out,
            api_url="http://127.0.0.1:1",
        )
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out.getvalue()), {})


class McpInstallTest(unittest.TestCase):
    def setUp(self) -> None:
        self.home = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.home)

    def test_install_preserves_existing_configs_handles_empty_json_and_uninstalls(self) -> None:
        # Existing Claude hooks + MCP servers
        (self.home / ".claude.json").write_text(json.dumps({"mcpServers": {"other": {"command": "x"}}}))
        (self.home / ".claude").mkdir()
        (self.home / ".claude" / "settings.json").write_text(json.dumps({
            "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "existing.sh"}]}]}
        }))

        # Existing Codex hooks + TOML with trust hashes & projects
        (self.home / ".codex").mkdir()
        (self.home / ".codex" / "hooks.json").write_text(json.dumps({
            "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "first.sh"}]}]}
        }))
        (self.home / ".codex" / "config.toml").write_text(
            '[features]\njs_repl = false\n\n'
            '[hooks.state."/Users/x/.codex/hooks.json:session_start:0:0"]\n'
            'enabled = true\n'
            'trusted_hash = "sha256:abc"\n\n'
            '[projects."/Users/x/repo"]\n'
            'trust_level = "trusted"\n'
        )

        # Empty 0-byte Antigravity mcp_config.json
        (self.home / ".gemini" / "config").mkdir(parents=True)
        (self.home / ".gemini" / "config" / "mcp_config.json").write_text("")

        changed = mcp_install.install(self.home, bin_path="/opt/homebrew/bin/fleet")
        self.assertEqual(len(changed), 6)
        # Idempotent second run
        self.assertEqual(mcp_install.install(self.home, bin_path="/opt/homebrew/bin/fleet"), [])
        self.assertEqual(mcp_install.status(self.home), {"claude": True, "codex": True, "antigravity": True})

        # Existing Claude & Codex hooks stay at index 0
        claude_hooks = json.loads((self.home / ".claude" / "settings.json").read_text())["hooks"]["SessionStart"]
        self.assertEqual(claude_hooks[0]["hooks"][0]["command"], "existing.sh")
        codex_hooks = json.loads((self.home / ".codex" / "hooks.json").read_text())["hooks"]["SessionStart"]
        self.assertEqual(codex_hooks[0]["hooks"][0]["command"], "first.sh")

        toml_text = (self.home / ".codex" / "config.toml").read_text()
        self.assertIn("hooks = true", toml_text)
        self.assertIn("js_repl = false", toml_text)
        self.assertIn('trusted_hash = "sha256:abc"', toml_text)
        self.assertIn('[projects."/Users/x/repo"]', toml_text)
        self.assertIn("[mcp_servers.fleet]", toml_text)

        agy_hooks = json.loads((self.home / ".gemini" / "config" / "hooks.json").read_text())
        self.assertIn("PreInvocation", agy_hooks["fleet"])
        self.assertEqual(agy_hooks["fleet"]["PreToolUse"][0]["matcher"], "ask_question|ask_permission")

        # Verify newly added Claude & Codex hook events, matchers, and Codex Interrupt/SessionEnd timeouts
        claude_settings = json.loads((self.home / ".claude" / "settings.json").read_text())["hooks"]
        self.assertEqual(claude_settings["PreToolUse"][0]["matcher"], "AskUserQuestion|ExitPlanMode")
        self.assertEqual(claude_settings["PermissionRequest"][0]["matcher"], "*")
        self.assertEqual(claude_settings["PostToolUseFailure"][0]["matcher"], "*")
        self.assertEqual(claude_settings["PermissionDenied"][0]["matcher"], "*")
        self.assertIn("permission_prompt", claude_settings["Notification"][0]["matcher"])
        self.assertIn("elicitation_dialog", claude_settings["Notification"][0]["matcher"])

        codex_hooks_all = json.loads((self.home / ".codex" / "hooks.json").read_text())["hooks"]
        self.assertEqual(codex_hooks_all["PreToolUse"][0]["matcher"], "request_user_input")
        self.assertEqual(codex_hooks_all["PermissionRequest"][0]["matcher"], "*")
        self.assertEqual(codex_hooks_all["Interrupt"][0]["hooks"][0]["timeout"], 3)
        self.assertEqual(codex_hooks_all["SessionEnd"][0]["hooks"][0]["timeout"], 3)

        # Uninstall cleans fleet entries while keeping existing settings intact
        mcp_install.uninstall(self.home)
        self.assertEqual(mcp_install.status(self.home), {"claude": False, "codex": False, "antigravity": False})
        self.assertIn("other", json.loads((self.home / ".claude.json").read_text())["mcpServers"])
        self.assertIn('trusted_hash = "sha256:abc"', (self.home / ".codex" / "config.toml").read_text())
        self.assertNotIn("[mcp_servers.fleet]", (self.home / ".codex" / "config.toml").read_text())
        claude_after = json.loads((self.home / ".claude" / "settings.json").read_text())["hooks"]
        self.assertEqual([g["hooks"][0]["command"] for g in claude_after["SessionStart"]], ["existing.sh"])
        self.assertNotIn("PermissionRequest", claude_after)
        self.assertNotIn("Notification", claude_after)
        codex_after = json.loads((self.home / ".codex" / "hooks.json").read_text())["hooks"]
        self.assertEqual([g["hooks"][0]["command"] for g in codex_after["SessionStart"]], ["first.sh"])
        self.assertNotIn("PermissionRequest", codex_after)
        self.assertNotIn("Interrupt", codex_after)


class RealtimeAndFriendlyNamesTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        _, mini_pub = keypair(self.tmp, "mini")
        self.fleet = Fleet.initialize(
            root=self.tmp / "state",
            host="home.example.com",
            port=443,
            lan_ip="192.168.1.50",
            passphrase=PASSPHRASE,
            lighthouse_pubkey=mini_pub,
        )
        self.addCleanup(self.fleet.close)
        httpd = make_server(self.fleet, "127.0.0.1", 0, quiet=True)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        self.addCleanup(httpd.server_close)
        self.addCleanup(httpd.shutdown)
        self.api = f"http://127.0.0.1:{httpd.server_address[1]}"

    def test_legacy_db_migration(self) -> None:
        db_path = self.tmp / "legacy.db"
        conn = sqlite3.connect(str(db_path))
        conn.executescript(
            """
            CREATE TABLE agent_sessions (
              session_id TEXT PRIMARY KEY,
              device TEXT NOT NULL,
              harness TEXT NOT NULL,
              raw_id TEXT NOT NULL,
              pid INTEGER,
              cwd TEXT NOT NULL DEFAULT '',
              repo TEXT NOT NULL DEFAULT '',
              branch TEXT NOT NULL DEFAULT '',
              state TEXT NOT NULL DEFAULT 'working',
              summary TEXT NOT NULL DEFAULT '',
              git_status TEXT NOT NULL DEFAULT '',
              tail_json TEXT NOT NULL DEFAULT '[]',
              started_at TEXT NOT NULL,
              updated_at TEXT NOT NULL
            );
            CREATE TABLE agent_messages (
              id INTEGER PRIMARY KEY AUTOINCREMENT,
              thread_id TEXT NOT NULL,
              reply_to INTEGER,
              from_session TEXT NOT NULL,
              from_device TEXT NOT NULL,
              to_device TEXT,
              to_session TEXT,
              to_repo TEXT,
              body TEXT NOT NULL,
              created_at TEXT NOT NULL
            );
            CREATE TABLE agent_reads (
              session_id TEXT NOT NULL,
              message_id INTEGER NOT NULL,
              read_at TEXT NOT NULL,
              PRIMARY KEY (session_id, message_id)
            );
            """
        )
        now_iso = datetime.now(timezone.utc).replace(microsecond=0).isoformat()
        conn.execute(
            "INSERT INTO agent_sessions(session_id, device, harness, raw_id, cwd, repo, started_at, updated_at) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            ("mini:claude:leg1", "mini", "claude", "leg1", "/proj", "legacy-repo", now_iso, now_iso),
        )
        conn.commit()
        conn.close()

        hub = AgentHub(db_path)
        self.addCleanup(hub.close)
        cols = {r["name"] for r in hub.conn.execute("PRAGMA table_info(agent_sessions)").fetchall()}
        self.assertTrue(
            {"full_raw_id", "state_changed_at", "pending_action_json", "title", "name", "custom_name"}.issubset(cols)
        )
        listed = hub.list_sessions()
        self.assertEqual(len(listed), 1)
        self.assertEqual(listed[0]["session_id"], "mini:claude:leg1")
        self.assertEqual(listed[0]["delivery"], "next_hook")
        self.assertIsNone(listed[0]["pending_action"])
        self.assertIsNone(listed[0]["waiting_seconds"])
        self.assertEqual(listed[0]["state_changed_at"], now_iso)
        hb = hub.heartbeat(device="mini", harness="claude", raw_id="leg1-full", title="Migrated Session", cwd="/proj")
        self.assertEqual(hb["name"], "migrated-session")

    def test_pid_ghost_row_fix_preserves_idle_and_refreshes_updated_at(self) -> None:
        hub = self.fleet.agents
        hub.heartbeat(device="mini", harness="claude", raw_id="pid-4242", pid=4242, cwd="/proj", repo="app")
        hub.heartbeat(
            device="mini",
            harness="claude",
            raw_id="c779894d-fe1c-41c0-9999",
            pid=4242,
            cwd="/proj",
            repo="app",
            state="idle",
        )
        old_ts = (datetime.now(timezone.utc) - timedelta(minutes=3)).replace(microsecond=0).isoformat()
        hub.conn.execute(
            "UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?",
            (old_ts, "mini:claude:c779894d-fe1"),
        )

        hb = hub.heartbeat(
            device="mini",
            harness="claude",
            raw_id="pid-4242",
            pid=4242,
            cwd="/proj",
            repo="app",
            state="working",
        )
        self.assertEqual(hb["session_id"], "mini:claude:c779894d-fe1")
        self.assertEqual(hb["full_raw_id"], "c779894d-fe1c-41c0-9999")
        sessions = hub.list_sessions(device="mini", include_stale=True)
        self.assertEqual(len(sessions), 1)
        self.assertEqual(sessions[0]["state"], "idle")
        self.assertGreater(sessions[0]["updated_at"], old_ts)

    def test_title_extraction_for_all_three_harnesses(self) -> None:
        # 1. Claude: >256 KB transcript with custom-title > agent-name > ai-title priority
        claude_log = self.tmp / "claude_big.jsonl"
        filler = json.dumps({"type": "assistant", "message": {"role": "assistant", "content": "x" * 2048}})
        lines = [
            json.dumps({"type": "ai-title", "aiTitle": "Auto Generated Title"}),
            json.dumps({"type": "agent-name", "agentName": "Agent Alias"}),
            json.dumps({"type": "custom-title", "customTitle": "Justin"}),
        ] + [filler] * 140
        claude_log.write_text("\n".join(lines) + "\n")
        self.assertGreater(claude_log.stat().st_size, 256 * 1024)
        self.assertEqual(hooks.read_session_title("claude", "c1", str(claude_log)), ("Justin", True))

        claude_ai_only = self.tmp / "claude_ai.jsonl"
        claude_ai_only.write_text(json.dumps({"type": "ai-title", "aiTitle": "Debug OAuth Flow"}) + "\n")
        self.assertEqual(hooks.read_session_title("claude", "c2", str(claude_ai_only)), ("Debug OAuth Flow", False))

        # 2. Codex: session_index.jsonl picks latest thread_name for matching UUID
        codex_home = self.tmp / ".codex"
        codex_home.mkdir()
        uid = "01a1ece5-f00c-7860-a0dc-6c1897e73f30"
        (codex_home / "session_index.jsonl").write_text(
            "\n".join([
                json.dumps({"id": uid, "thread_name": "Initial Thread"}),
                json.dumps({"id": "other-uuid", "thread_name": "Other"}),
                json.dumps({"id": uid, "thread_name": "Renamed Codex Worker"}),
            ])
            + "\n"
        )
        self.assertEqual(
            hooks.read_session_title("codex", uid, None, codex_home=codex_home),
            ("Renamed Codex Worker", False),
        )

        # 3. Antigravity: read-only conversation_summaries.db via transcript path and gemini_home fallback
        gemini_home = self.tmp / ".gemini"
        agy_dir = gemini_home / "antigravity"
        transcript_dir = agy_dir / "brain" / "conv-abc" / ".system_generated" / "logs"
        transcript_dir.mkdir(parents=True)
        transcript_file = transcript_dir / "transcript.jsonl"
        transcript_file.write_text("{}\n")
        db_path = agy_dir / "conversation_summaries.db"
        db_conn = sqlite3.connect(str(db_path))
        db_conn.execute("CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT NOT NULL)")
        db_conn.execute(
            "INSERT INTO conversation_summaries(conversation_id, title) VALUES (?, ?)",
            ("conv-abc", "Implement Fleet Realtime Push"),
        )
        db_conn.commit()
        db_conn.close()
        self.assertEqual(
            hooks.read_session_title("antigravity", "conv-abc", str(transcript_file)),
            ("Implement Fleet Realtime Push", False),
        )
        self.assertEqual(
            hooks.read_session_title("antigravity", "conv-abc", None, gemini_home=gemini_home),
            ("Implement Fleet Realtime Push", False),
        )

    def test_friendly_naming_target_resolution_and_inbox_enrichment(self) -> None:
        hub = self.fleet.agents
        # Repo fallback -> title update -> collision suffix -2 -> custom rename preserved
        r1 = hub.heartbeat(device="mini", harness="claude", raw_id="id-1111", cwd="/a", repo="fleet")
        self.assertEqual(r1["name"], "fleet")
        r1b = hub.heartbeat(device="mini", harness="claude", raw_id="id-1111", title="Auth Bug Fix", cwd="/a", repo="fleet")
        self.assertEqual(r1b["name"], "auth-bug-fix")

        r2 = hub.heartbeat(device="mini", harness="codex", raw_id="id-2222", title="Auth Bug Fix", cwd="/b", repo="fleet")
        self.assertEqual(r2["name"], "auth-bug-fix-2")

        # Another device with same friendly name resolves via device/name and triggers ambiguity without device
        hub.heartbeat(device="laptop", harness="claude", raw_id="id-3333", title="Auth Bug Fix", cwd="/c", repo="fleet")
        with self.assertRaisesRegex(AgentError, "ambiguous session 'auth-bug-fix'"):
            hub.get_session("auth-bug-fix")
        self.assertEqual(hub.get_session("laptop/auth-bug-fix")["session_id"], "laptop:claude:id-3333")
        self.assertEqual(hub.get_session("auth-bug-fix-2")["session_id"], "mini:codex:id-2222")
        self.assertEqual(hub.get_session("id-2222")["session_id"], "mini:codex:id-2222")

        # Custom rename sticks across subsequent title updates
        renamed = hub.rename_session(device="mini", session_id="mini:claude:id-1111", new_name="Backend Lead")
        self.assertEqual(renamed["name"], "backend-lead")
        r1c = hub.heartbeat(device="mini", harness="claude", raw_id="id-1111", title="Completely Different Title", cwd="/a")
        self.assertEqual(r1c["name"], "backend-lead")
        self.assertEqual(hub.get_session("Completely Different Title")["session_id"], "mini:claude:id-1111")

        # Live session wins over stale session with same name
        hub.heartbeat(device="mini", harness="antigravity", raw_id="id-stale", cwd="/s")
        hub.rename_session(device="mini", session_id="mini:antigravity:id-stale", new_name="runner")
        old_ts = (datetime.now(timezone.utc) - timedelta(minutes=10)).replace(microsecond=0).isoformat()
        hub.conn.execute("UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?", (old_ts, "mini:antigravity:id-stale"))
        hub.heartbeat(device="mini", harness="antigravity", raw_id="id-live", cwd="/l")
        hub.rename_session(device="mini", session_id="mini:antigravity:id-live", new_name="runner")
        self.assertEqual(hub.get_session("runner")["session_id"], "mini:antigravity:id-live")

        # Sender friendly name/title enriched in inbox
        hub.send_message(device="mini", from_session="mini:claude:id-1111", to="runner", body="check metrics")
        inbox = hub.fetch_inbox(device="mini", session_id="mini:antigravity:id-live")
        self.assertEqual(inbox[0]["from_name"], "backend-lead")
        self.assertEqual(inbox[0]["from_title"], "Completely Different Title")

    def test_wait_presence_promotion_deregister_and_ack(self) -> None:
        hub = self.fleet.agents
        hub.heartbeat(device="mini", harness="claude", raw_id="sender-a", cwd="/s")
        hub.heartbeat(device="mini", harness="codex", raw_id="pid-8888", pid=8888, cwd="/w", state="idle")

        # Wait with require_full_id=True stays unready until promoted
        wait_res: dict = {}

        def do_wait() -> None:
            wait_res.update(
                hub.wait_for_messages(
                    device="mini",
                    session_id="mini:codex:pid-8888",
                    timeout=3.0,
                    mark_read=False,
                    require_full_id=True,
                )
            )

        t = threading.Thread(target=do_wait, daemon=True)
        t.start()
        time.sleep(0.08)
        pre = hub.get_session("mini:codex:pid-8888")
        self.assertFalse(pre["listening"])

        full_uuid = "01a1ece5-f00c-7860-a0dc-6c1897e73f30"
        hub.heartbeat(device="mini", harness="codex", raw_id=full_uuid, pid=8888, cwd="/w", state="idle")
        time.sleep(0.08)
        promoted = hub.get_session("01a1ece5-f00")
        self.assertTrue(promoted["listening"])
        self.assertEqual(promoted["delivery"], "immediate")

        report = hub.send_message_with_report(
            device="mini",
            from_session="mini:claude:sender-a",
            to="01a1ece5-f00",
            body="wake up",
        )
        self.assertEqual(report["delivery"], "immediate")
        self.assertEqual(report["woke_waiters"], 1)
        self.assertIsNone(report["warning"])

        t.join(timeout=2.0)
        self.assertFalse(wait_res["timed_out"])
        self.assertEqual(wait_res["session_id"], "mini:codex:01a1ece5-f00")
        self.assertEqual(wait_res["full_raw_id"], full_uuid)
        self.assertEqual(wait_res["messages"][0]["body"], "wake up")

        # Because mark_read=False, message stays unread until /v1/agents/ack
        self.assertEqual(len(hub.fetch_inbox(device="mini", session_id="mini:codex:01a1ece5-f00", mark_read=False)), 1)
        ack = call(
            self.api,
            "POST",
            "/v1/agents/ack",
            {"harness": "codex", "session_id": "mini:codex:01a1ece5-f00", "message_ids": [wait_res["messages"][0]["id"]]},
        )
        self.assertEqual(ack, {"ok": True, "acked": 1})
        self.assertEqual(hub.fetch_inbox(device="mini", session_id="mini:codex:01a1ece5-f00"), [])

        # Deregister wakes an active waiter with exited=True
        exit_res: dict = {}

        def wait_until_exit() -> None:
            exit_res.update(hub.wait_for_messages(device="mini", session_id="mini:codex:01a1ece5-f00", timeout=3.0))

        t2 = threading.Thread(target=wait_until_exit, daemon=True)
        t2.start()
        time.sleep(0.08)
        hub.deregister(device="mini", session_id="mini:codex:01a1ece5-f00")
        t2.join(timeout=2.0)
        self.assertTrue(exit_res["exited"])
        self.assertEqual(exit_res["messages"], [])

    def test_delivery_reports_and_fail_fast_ask(self) -> None:
        hub = self.fleet.agents
        hub.heartbeat(device="mini", harness="claude", raw_id="asker", cwd="/a")
        hub.heartbeat(device="mini", harness="claude", raw_id="idle-target", cwd="/i", state="idle")
        hub.heartbeat(device="mini", harness="claude", raw_id="busy-target", cwd="/b", state="working")
        hub.heartbeat(device="mini", harness="claude", raw_id="gone-target", cwd="/g", state="idle")
        hub.deregister(device="mini", session_id="mini:claude:gone-target")

        send_idle = call(
            self.api,
            "POST",
            "/v1/agents/send",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "idle-target", "body": "hi"},
        )
        self.assertEqual(send_idle["delivery"], "queued_idle")
        self.assertEqual(send_idle["woke_waiters"], 0)
        self.assertIn("idle with no active listener", send_idle["warning"])

        send_busy = call(
            self.api,
            "POST",
            "/v1/agents/send",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "busy-target", "body": "yo"},
        )
        self.assertEqual(send_busy["delivery"], "next_hook")
        self.assertIsNone(send_busy["warning"])

        send_off = call(
            self.api,
            "POST",
            "/v1/agents/send",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "gone-target", "body": "bye"},
        )
        self.assertEqual(send_off["delivery"], "offline")
        self.assertIn("offline/stale", send_off["warning"])

        send_bcast = call(
            self.api,
            "POST",
            "/v1/agents/send",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "*", "body": "all"},
        )
        self.assertEqual(send_bcast["delivery"], "broadcast")
        self.assertEqual(send_bcast["recipients"]["next_hook"], ["mini:claude:busy-target"])
        self.assertEqual(send_bcast["recipients"]["queued_idle"], ["mini:claude:idle-target"])

        # ask fails fast on queued_idle and offline without waiting
        t0 = time.monotonic()
        ask_idle = call(
            self.api,
            "POST",
            "/v1/agents/ask",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "idle-target", "body": "q?", "timeout": 10},
        )
        self.assertLess(time.monotonic() - t0, 1.0)
        self.assertFalse(ask_idle["delivered"])
        self.assertFalse(ask_idle["timed_out"])
        self.assertEqual(ask_idle["delivery"], "queued_idle")

        ask_off = call(
            self.api,
            "POST",
            "/v1/agents/ask",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "gone-target", "body": "q?", "timeout": 10},
        )
        self.assertFalse(ask_off["delivered"])
        self.assertEqual(ask_off["delivery"], "offline")

        # wait_if_idle=True waits and receives reply
        def reply_idle() -> None:
            for _ in range(40):
                msgs = hub.fetch_inbox(device="mini", session_id="mini:claude:idle-target")
                if msgs and msgs[-1]["body"] == "wait for me":
                    hub.send_message(
                        device="mini",
                        from_session="mini:claude:idle-target",
                        to="asker",
                        body="got it",
                        reply_to=msgs[-1]["id"],
                    )
                    return
                time.sleep(0.02)

        rt = threading.Thread(target=reply_idle, daemon=True)
        rt.start()
        ask_wait = call(
            self.api,
            "POST",
            "/v1/agents/ask",
            {
                "harness": "claude",
                "session_id": "mini:claude:asker",
                "to": "idle-target",
                "body": "wait for me",
                "timeout": 3.0,
                "wait_if_idle": True,
            },
        )
        rt.join(timeout=2.0)
        self.assertTrue(ask_wait["delivered"])
        self.assertFalse(ask_wait["timed_out"])
        self.assertEqual(ask_wait["reply"]["body"], "got it")

    def test_wait_for_inbox_cli_and_pid_lock_replacement(self) -> None:
        hub = self.fleet.agents
        hub.heartbeat(device="mini", harness="antigravity", raw_id="agy-wait", title="Deploy Helper", cwd="/d")
        hub.heartbeat(device="mini", harness="claude", raw_id="sender-w", title="Coordinator", cwd="/c")
        lock_dir = self.tmp / "locks"

        # Spawn a dummy old waiter process and place its PID in the lockfile
        dummy = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
        self.addCleanup(lambda: (dummy.kill(), dummy.wait()) if dummy.poll() is None else None)
        lock_path = waiter._pid_lock_path("mini:antigravity:agy-wait", lock_dir=lock_dir)
        lock_dir.mkdir(parents=True, exist_ok=True)
        lock_path.write_text(f"{dummy.pid}\n")

        out = io.StringIO()

        def send_soon() -> None:
            time.sleep(0.15)
            hub.send_message(
                device="mini",
                from_session="mini:claude:sender-w",
                to="deploy-helper",
                body="run migration",
            )

        st = threading.Thread(target=send_soon, daemon=True)
        st.start()
        rc = waiter.wait_for_inbox_cli(
            session_id="mini:antigravity:agy-wait",
            api_url=self.api,
            harness="antigravity",
            max_wait=5.0,
            poll_timeout=2.0,
            stdout=out,
            lock_dir=lock_dir,
            watch_parent=False,
        )
        st.join(timeout=2.0)
        self.assertEqual(rc, 0)
        self.assertIsNotNone(dummy.poll())  # old duplicate waiter was terminated
        self.assertFalse(lock_path.exists())
        text = out.getvalue()
        self.assertIn("coordinator (mini:claude:sender-w)", text)
        self.assertIn("run migration", text)
        self.assertIn("fleet agents wait --session mini:antigravity:agy-wait", text)

    def test_codex_queue_listener_success_and_failure_pause(self) -> None:
        hub = self.fleet.agents
        hub.heartbeat(device="mini", harness="claude", raw_id="peer-1", cwd="/p")
        hub.heartbeat(device="mini", harness="codex", raw_id="pid-7777", pid=7777, cwd="/codex", state="idle")

        recorded_cmds: list[list[str]] = []
        mode = {"rc": 0, "err": ""}

        def fake_queue(cmd: list[str]) -> tuple[int, str]:
            recorded_cmds.append(cmd)
            return mode["rc"], mode["err"]

        listener = waiter.CodexQueueListener(
            api_url=self.api,
            ppid=7777,
            cwd="/codex",
            codex_bin="/usr/local/bin/codex",
            poll_timeout=0.5,
            run_queue_fn=fake_queue,
        )
        self.addCleanup(listener.stop)
        listener.start()

        # Promote codex placeholder to full UUID
        full_uuid = "01a1ece5-f00c-7860-a0dc-6c1897e73f30"
        hub.heartbeat(device="mini", harness="codex", raw_id=full_uuid, pid=7777, cwd="/codex", state="idle")
        for _ in range(30):
            if hub.get_session("01a1ece5-f00")["listening"]:
                break
            time.sleep(0.03)
        self.assertTrue(hub.get_session("01a1ece5-f00")["listening"])

        # Send message -> invokes `codex queue` and acks
        hub.send_message(device="mini", from_session="mini:claude:peer-1", to="01a1ece5-f00", body="check diff")
        for _ in range(40):
            if listener.status()["delivered_count"] == 1:
                break
            time.sleep(0.03)
        self.assertEqual(listener.status()["delivered_count"], 1)
        self.assertEqual(recorded_cmds[0][:4], ["/usr/local/bin/codex", "queue", "--thread", full_uuid])
        self.assertIn("--resume", recorded_cmds[0])
        self.assertIn("check diff", recorded_cmds[0][recorded_cmds[0].index("--message") + 1])
        self.assertEqual(hub.fetch_inbox(device="mini", session_id="mini:codex:01a1ece5-f00"), [])

        # Failure on `codex queue` does NOT ack and pauses listener
        for _ in range(30):
            if hub.get_session("01a1ece5-f00")["listening"]:
                break
            time.sleep(0.03)
        mode["rc"] = 1
        mode["err"] = "daemon unavailable"
        hub.send_message(device="mini", from_session="mini:claude:peer-1", to="01a1ece5-f00", body="still unread")
        for _ in range(40):
            if listener.status()["last_error"] == "daemon unavailable":
                break
            time.sleep(0.03)
        st = listener.status()
        self.assertFalse(st["enabled"])
        self.assertEqual(st["last_error"], "daemon unavailable")
        self.assertFalse(hub.get_session("01a1ece5-f00")["listening"])
        unread = hub.fetch_inbox(device="mini", session_id="mini:codex:01a1ece5-f00")
        self.assertEqual([m["body"] for m in unread], ["still unread"])

    def test_mcp_listen_set_name_hooks_and_cli_commands(self) -> None:
        # 1. MCP fleet_set_name + fleet_listen on claude & codex
        ppid = os.getppid()
        self.fleet.agents.heartbeat(
            device="mini",
            harness="claude",
            raw_id="claude-mcp-1",
            pid=ppid,
            cwd=os.getcwd(),
        )
        reqs = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-03-26"}},
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "fleet_set_name", "arguments": {"name": "Planner Lead"}},
            },
            {
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {"name": "fleet_listen", "arguments": {"action": "status"}},
            },
        ]
        out = io.StringIO()
        mcp.serve_stdio(
            "claude",
            stdin=io.StringIO("\n".join(json.dumps(r) for r in reqs) + "\n"),
            stdout=out,
            api_url=self.api,
            heartbeat_interval=60.0,
        )
        resps = [json.loads(line) for line in out.getvalue().splitlines() if line.strip()]
        rename_data = json.loads(resps[1]["result"]["content"][0]["text"])
        self.assertEqual(rename_data["name"], "planner-lead")
        listen_data = json.loads(resps[2]["result"]["content"][0]["text"])
        self.assertEqual(listen_data["mode"], "background_command")
        self.assertEqual(listen_data["command"], "fleet agents wait --session mini:claude:claude-mcp-1")

        # Codex serve_stdio with auto_listen + mocked codex_run_queue_fn
        self.fleet.agents.heartbeat(
            device="mini",
            harness="codex",
            raw_id="01a1ece5-f00c-7860-a0dc-6c1897e73f31",
            pid=ppid,
            cwd=os.getcwd(),
        )
        codex_reqs = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-03-26"}},
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "fleet_listen", "arguments": {"action": "status"}},
            },
            {
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {"name": "fleet_listen", "arguments": {"action": "stop"}},
            },
            {
                "jsonrpc": "2.0",
                "id": 4,
                "method": "tools/call",
                "params": {"name": "fleet_listen", "arguments": {"action": "start"}},
            },
        ]
        codex_out = io.StringIO()
        mcp.serve_stdio(
            "codex",
            stdin=io.StringIO("\n".join(json.dumps(r) for r in codex_reqs) + "\n"),
            stdout=codex_out,
            api_url=self.api,
            heartbeat_interval=60.0,
            auto_listen=True,
            codex_run_queue_fn=lambda _cmd: (0, ""),
        )
        codex_resps = [json.loads(line) for line in codex_out.getvalue().splitlines() if line.strip()]
        st0 = json.loads(codex_resps[1]["result"]["content"][0]["text"])
        st1 = json.loads(codex_resps[2]["result"]["content"][0]["text"])
        st2 = json.loads(codex_resps[3]["result"]["content"][0]["text"])
        self.assertEqual(st0["mode"], "mcp_codex_queue")
        self.assertTrue(st0["enabled"])
        self.assertFalse(st1["enabled"])
        self.assertTrue(st2["enabled"])

        # 2. SessionStart hook instructions for all 3 harnesses
        hook_out_agy = io.StringIO()
        hooks.run_hook(
            "antigravity",
            stdin=io.StringIO(json.dumps({"hookEventName": "SessionStart", "conversationId": "agy-ss1", "cwd": str(self.tmp)})),
            stdout=hook_out_agy,
            api_url=self.api,
        )
        agy_msg = json.loads(hook_out_agy.getvalue())["injectSteps"][0]["systemMessage"]["systemMessage"]
        self.assertIn("fleet agents wait --session mini:antigravity:agy-ss1", agy_msg)
        self.assertIn("WaitMsBeforeAsync=500", agy_msg)

        hook_out_cl = io.StringIO()
        hooks.run_hook(
            "claude",
            stdin=io.StringIO(json.dumps({"hook_event_name": "SessionStart", "session_id": "cl-ss1", "cwd": str(self.tmp)})),
            stdout=hook_out_cl,
            api_url=self.api,
        )
        cl_msg = json.loads(hook_out_cl.getvalue())["hookSpecificOutput"]["additionalContext"]
        self.assertIn("fleet agents wait --session mini:claude:cl-ss1", cl_msg)
        self.assertIn("run_in_background=true", cl_msg)

        hook_out_cx = io.StringIO()
        hooks.run_hook(
            "codex",
            stdin=io.StringIO(json.dumps({"hook_event_name": "SessionStart", "session_id": "cx-ss1", "cwd": str(self.tmp)})),
            stdout=hook_out_cx,
            api_url=self.api,
        )
        cx_msg = json.loads(hook_out_cx.getvalue())["hookSpecificOutput"]["additionalContext"]
        self.assertIn("codex queue", cx_msg)

        # 3. CLI `fleet agents rename` and `fleet agents` table output
        parser = cli.build_parser()
        with mock.patch.object(cli, "api", return_value=self.api):
            buf = io.StringIO()
            with redirect_stdout(buf):
                ns = parser.parse_args(["agents", "rename", "agy-ss1", "ui-lead"])
                ns.func(ns)
            self.assertIn("mini:antigravity:agy-ss1 -> ui-lead", buf.getvalue())

            buf2 = io.StringIO()
            with redirect_stdout(buf2):
                ns2 = parser.parse_args(["agents", "--all"])
                ns2.func(ns2)
            table = buf2.getvalue()
            self.assertIn("NAME", table)
            self.assertIn("TITLE", table)
            self.assertIn("ui-lead", table)

    def test_waiting_state_lifecycle_retention_and_filtering(self) -> None:
        hub = self.fleet.agents
        hub.heartbeat(device="mini", harness="claude", raw_id="pid-5151", pid=5151, cwd="/wait")
        hub.heartbeat(
            device="mini",
            harness="claude",
            raw_id="cl-wait-1",
            pid=5151,
            cwd="/wait",
            state="waiting_permission",
            pending_action={"kind": "permission", "tool": "Bash", "detail": "rm -rf build", "event": "PermissionRequest"},
        )
        sess = hub.get_session("cl-wait-1")
        self.assertEqual(sess["state"], "waiting_permission")
        self.assertEqual(sess["delivery"], "waiting_user")
        self.assertEqual(sess["pending_action"]["tool"], "Bash")
        self.assertEqual(sess["pending_action"]["detail"], "rm -rf build")
        self.assertIsInstance(sess["waiting_seconds"], int)
        orig_changed_at = sess["state_changed_at"]

        # Delayed generic Notification(permission_prompt) without tool must NOT clobber rich PermissionRequest or reset timer
        time.sleep(0.02)
        hub.heartbeat(
            device="mini",
            harness="claude",
            raw_id="cl-wait-1",
            pid=5151,
            cwd="/wait",
            state="waiting_permission",
            pending_action={"kind": "permission", "detail": "Claude needs your permission", "event": "Notification:permission_prompt"},
        )
        sess2 = hub.get_session("cl-wait-1")
        self.assertEqual(sess2["pending_action"]["tool"], "Bash")
        self.assertEqual(sess2["pending_action"]["detail"], "rm -rf build")
        self.assertEqual(sess2["state_changed_at"], orig_changed_at)

        # Waiting sessions stay live past 5 min (up to 60 min) and pid keepalive preserves waiting state & pending_action
        ts_20m_ago = (datetime.now(timezone.utc) - timedelta(minutes=20)).replace(microsecond=0).isoformat()
        hub.conn.execute(
            "UPDATE agent_sessions SET updated_at = ?, state_changed_at = ? WHERE session_id = ?",
            (ts_20m_ago, ts_20m_ago, "mini:claude:cl-wait-1"),
        )
        live_20m = hub.list_sessions()
        self.assertEqual([s["session_id"] for s in live_20m], ["mini:claude:cl-wait-1"])
        self.assertGreaterEqual(live_20m[0]["waiting_seconds"], 1190)

        keepalive = hub.heartbeat(device="mini", harness="claude", raw_id="pid-5151", pid=5151, cwd="/wait", state="working")
        self.assertEqual(keepalive["session_id"], "mini:claude:cl-wait-1")
        after_ka = hub.get_session("cl-wait-1")
        self.assertEqual(after_ka["state"], "waiting_permission")
        self.assertEqual(after_ka["pending_action"]["tool"], "Bash")
        self.assertEqual(after_ka["state_changed_at"], ts_20m_ago)

        # After 65 min, waiting session is stale
        ts_65m_ago = (datetime.now(timezone.utc) - timedelta(minutes=65)).replace(microsecond=0).isoformat()
        hub.conn.execute("UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?", (ts_65m_ago, "mini:claude:cl-wait-1"))
        self.assertEqual(hub.list_sessions(), [])
        # Normal working/idle session is stale after 10 min
        hub.heartbeat(device="mini", harness="codex", raw_id="cx-work", cwd="/w2", state="working")
        hub.heartbeat(
            device="mini",
            harness="antigravity",
            raw_id="agy-q",
            cwd="/q",
            state="waiting_question",
            pending_action={"kind": "question", "tool": "ask_question", "detail": "Which DB?", "event": "PreToolUse"},
        )
        ts_10m_ago = (datetime.now(timezone.utc) - timedelta(minutes=10)).replace(microsecond=0).isoformat()
        hub.conn.execute("UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?", (ts_10m_ago, "mini:codex:cx-work"))
        # Re-touch cl-wait-1 so both waiting states are live
        hub.heartbeat(device="mini", harness="claude", raw_id="cl-wait-1", pid=5151, cwd="/wait", state="waiting_permission")
        hub.heartbeat(device="mini", harness="codex", raw_id="cx-live", cwd="/w3", state="working")

        self.assertEqual(
            {s["session_id"] for s in hub.list_sessions(state="waiting")},
            {"mini:claude:cl-wait-1", "mini:antigravity:agy-q"},
        )
        self.assertEqual(
            [s["session_id"] for s in hub.list_sessions(state="waiting_question")],
            ["mini:antigravity:agy-q"],
        )
        self.assertEqual(
            [s["session_id"] for s in hub.list_sessions(state="waiting_permission")],
            ["mini:claude:cl-wait-1"],
        )
        with self.assertRaisesRegex(AgentError, "invalid state filter"):
            hub.list_sessions(state="bogus")

        # Returning to working/idle/exited clears pending_action and waiting_seconds
        hub.heartbeat(device="mini", harness="claude", raw_id="cl-wait-1", pid=5151, cwd="/wait", state="working")
        resumed = hub.get_session("cl-wait-1")
        self.assertEqual(resumed["state"], "working")
        self.assertIsNone(resumed["pending_action"])
        self.assertIsNone(resumed["waiting_seconds"])

        hub.deregister(device="mini", session_id="mini:antigravity:agy-q")
        exited = hub.get_session("agy-q")
        self.assertEqual(exited["state"], "exited")
        self.assertIsNone(exited["pending_action"])
        self.assertIsNone(exited["waiting_seconds"])

    def test_waiting_user_delivery_reports_and_ask_fail_fast(self) -> None:
        hub = self.fleet.agents
        hub.heartbeat(device="mini", harness="claude", raw_id="asker", cwd="/a", state="working")
        hub.heartbeat(
            device="mini",
            harness="codex",
            raw_id="01a1ece5-f00c-7860-a0dc-6c1897e73f99",
            cwd="/w",
            state="waiting_permission",
            pending_action={"kind": "permission", "tool": "Bash", "detail": "git push", "event": "PermissionRequest"},
        )
        hub.heartbeat(device="mini", harness="claude", raw_id="idle-peer", cwd="/i", state="idle")

        # Even when listening=True, waiting state reports delivery="waiting_user"
        stop_wait = threading.Event()
        t = threading.Thread(
            target=lambda: hub.wait_for_messages(
                device="mini",
                session_id="mini:codex:01a1ece5-f00",
                timeout=2.0,
                mark_read=False,
            )
            if not stop_wait.is_set()
            else None,
            daemon=True,
        )
        t.start()
        for _ in range(30):
            if hub.get_session("01a1ece5-f00")["listening"]:
                break
            time.sleep(0.02)
        self.assertTrue(hub.get_session("01a1ece5-f00")["listening"])
        self.assertEqual(hub.get_session("01a1ece5-f00")["delivery"], "waiting_user")

        send_wait = call(
            self.api,
            "POST",
            "/v1/agents/send",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "01a1ece5-f00", "body": "ping"},
        )
        self.assertEqual(send_wait["delivery"], "waiting_user")
        self.assertIn("permission approval", send_wait["warning"])
        self.assertIn("Bash: git push", send_wait["warning"])
        t.join(timeout=2.0)
        stop_wait.set()

        # Broadcast populates recipients["waiting_user"]
        bcast = call(
            self.api,
            "POST",
            "/v1/agents/send",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "*", "body": "heads up"},
        )
        self.assertEqual(bcast["recipients"]["waiting_user"], ["mini:codex:01a1ece5-f00"])
        self.assertEqual(bcast["recipients"]["queued_idle"], ["mini:claude:idle-peer"])
        self.assertIn("waiting for user input or idle", bcast["warning"])

        # ask() fails fast on waiting_user unless wait_if_idle=True
        t0 = time.monotonic()
        ask_fast = call(
            self.api,
            "POST",
            "/v1/agents/ask",
            {"harness": "claude", "session_id": "mini:claude:asker", "to": "01a1ece5-f00", "body": "status?", "timeout": 10},
        )
        self.assertLess(time.monotonic() - t0, 1.0)
        self.assertFalse(ask_fast["delivered"])
        self.assertFalse(ask_fast["timed_out"])
        self.assertEqual(ask_fast["delivery"], "waiting_user")

        def answer_later() -> None:
            for _ in range(40):
                msgs = hub.fetch_inbox(device="mini", session_id="mini:codex:01a1ece5-f00")
                if msgs and msgs[-1]["body"] == "wait for approval":
                    hub.send_message(
                        device="mini",
                        from_session="mini:codex:01a1ece5-f00",
                        to="asker",
                        body="approved and done",
                        reply_to=msgs[-1]["id"],
                    )
                    return
                time.sleep(0.02)

        rt = threading.Thread(target=answer_later, daemon=True)
        rt.start()
        ask_wait = call(
            self.api,
            "POST",
            "/v1/agents/ask",
            {
                "harness": "claude",
                "session_id": "mini:claude:asker",
                "to": "01a1ece5-f00",
                "body": "wait for approval",
                "timeout": 3.0,
                "wait_if_idle": True,
            },
        )
        rt.join(timeout=2.0)
        self.assertTrue(ask_wait["delivered"])
        self.assertEqual(ask_wait["reply"]["body"], "approved and done")

    def test_waiting_hooks_across_all_three_harnesses_and_mcp_cli(self) -> None:
        hub = self.fleet.agents
        hub.heartbeat(device="mini", harness="claude", raw_id="sender-h", cwd="/s")

        def fire(harness: str, payload: dict) -> dict:
            out = io.StringIO()
            rc = hooks.run_hook(harness, stdin=io.StringIO(json.dumps(payload)), stdout=out, api_url=self.api)
            self.assertEqual(rc, 0)
            return json.loads(out.getvalue())

        # --- Claude Code ---
        fire("claude", {"hook_event_name": "SessionStart", "session_id": "cl-h1", "cwd": str(self.tmp)})
        hub.send_message(device="mini", from_session="mini:claude:sender-h", to="mini:claude:cl-h1", body="keep unread")

        # PermissionRequest on Bash -> waiting_permission, passive {}, does NOT drain inbox
        self.assertEqual(
            fire("claude", {
                "hook_event_name": "PermissionRequest",
                "session_id": "cl-h1",
                "cwd": str(self.tmp),
                "tool_name": "Bash",
                "tool_input": {"command": "npm test"},
            }),
            {},
        )
        s = hub.get_session("cl-h1")
        self.assertEqual(s["state"], "waiting_permission")
        self.assertEqual(s["pending_action"]["tool"], "Bash")
        self.assertEqual(s["pending_action"]["detail"], "npm test")
        self.assertEqual(len(hub.fetch_inbox(device="mini", session_id="mini:claude:cl-h1", mark_read=False)), 1)

        # Notification(permission_prompt) keeps richer tool info, unknown Notification is no-op
        fire("claude", {
            "hook_event_name": "Notification",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "notification_type": "permission_prompt",
            "message": "Claude needs your permission",
        })
        self.assertEqual(hub.get_session("cl-h1")["pending_action"]["tool"], "Bash")
        fire("claude", {
            "hook_event_name": "Notification",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "notification_type": "auth_success",
        })
        self.assertEqual(hub.get_session("cl-h1")["state"], "waiting_permission")

        # PostToolUseFailure clears waiting back to working without draining; PostToolUse clears & injects unread
        self.assertEqual(
            fire("claude", {
                "hook_event_name": "PostToolUseFailure",
                "session_id": "cl-h1",
                "cwd": str(self.tmp),
                "tool_name": "Bash",
            }),
            {},
        )
        self.assertEqual(hub.get_session("cl-h1")["state"], "working")
        self.assertIsNone(hub.get_session("cl-h1")["pending_action"])

        # PreToolUse AskUserQuestion & ExitPlanMode & PermissionRequest AskUserQuestion
        self.assertEqual(
            fire("claude", {
                "hook_event_name": "PreToolUse",
                "session_id": "cl-h1",
                "cwd": str(self.tmp),
                "tool_name": "AskUserQuestion",
                "tool_input": {"questions": [{"question": "Use SQLite or Postgres?"}]},
            }),
            {},
        )
        s = hub.get_session("cl-h1")
        self.assertEqual(s["state"], "waiting_question")
        self.assertEqual(s["pending_action"]["detail"], "Use SQLite or Postgres?")

        self.assertEqual(
            fire("claude", {
                "hook_event_name": "PermissionDenied",
                "session_id": "cl-h1",
                "cwd": str(self.tmp),
                "tool_name": "Bash",
            }),
            {},
        )
        self.assertEqual(hub.get_session("cl-h1")["state"], "working")

        fire("claude", {
            "hook_event_name": "PreToolUse",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "tool_name": "ExitPlanMode",
            "tool_input": {},
        })
        self.assertEqual(hub.get_session("cl-h1")["pending_action"]["detail"], "Waiting for plan approval")

        fire("claude", {
            "hook_event_name": "PermissionRequest",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "tool_name": "AskUserQuestion",
            "tool_input": {"questions": [{"question": "Confirm deploy?"}]},
        })
        self.assertEqual(hub.get_session("cl-h1")["state"], "waiting_question")
        self.assertEqual(hub.get_session("cl-h1")["pending_action"]["detail"], "Confirm deploy?")

        fire("claude", {
            "hook_event_name": "PostToolUseFailure",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "tool_name": "AskUserQuestion",
        })
        self.assertEqual(hub.get_session("cl-h1")["state"], "working")

        fire("claude", {
            "hook_event_name": "Notification",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "notification_type": "elicitation_dialog",
            "message": "Enter API key",
        })
        self.assertEqual(hub.get_session("cl-h1")["state"], "waiting_question")
        self.assertEqual(hub.get_session("cl-h1")["pending_action"]["detail"], "Enter API key")

        ups = fire("claude", {
            "hook_event_name": "UserPromptSubmit",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "prompt": "continue",
        })
        self.assertIn("keep unread", ups["hookSpecificOutput"]["additionalContext"])

        hub.send_message(device="mini", from_session="mini:claude:sender-h", to="mini:claude:cl-h1", body="keep unread 2")
        fire("claude", {
            "hook_event_name": "Notification",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "notification_type": "agent_needs_input",
            "message": "Agent needs confirmation",
        })
        self.assertEqual(hub.get_session("cl-h1")["state"], "waiting_question")
        self.assertEqual(hub.get_session("cl-h1")["pending_action"]["detail"], "Agent needs confirmation")
        self.assertEqual(len(hub.fetch_inbox(device="mini", session_id="mini:claude:cl-h1", mark_read=False)), 1)

        post_resp = fire("claude", {
            "hook_event_name": "PostToolUse",
            "session_id": "cl-h1",
            "cwd": str(self.tmp),
            "tool_name": "AskUserQuestion",
        })
        self.assertIn("keep unread 2", post_resp["hookSpecificOutput"]["additionalContext"])
        self.assertEqual(hub.get_session("cl-h1")["state"], "working")

        # --- Codex CLI ---
        fire("codex", {"hook_event_name": "SessionStart", "session_id": "cx-h1", "cwd": str(self.tmp)})
        hub.send_message(device="mini", from_session="mini:claude:sender-h", to="mini:codex:cx-h1", body="codex msg")
        self.assertEqual(
            fire("codex", {
                "hook_event_name": "PermissionRequest",
                "session_id": "cx-h1",
                "cwd": str(self.tmp),
                "tool_name": "Bash",
                "tool_input": {"command": "deploy.sh"},
            }),
            {},
        )
        self.assertEqual(hub.get_session("cx-h1")["state"], "waiting_permission")
        self.assertEqual(hub.get_session("cx-h1")["pending_action"]["detail"], "deploy.sh")
        self.assertEqual(len(hub.fetch_inbox(device="mini", session_id="mini:codex:cx-h1", mark_read=False)), 1)

        self.assertEqual(
            fire("codex", {
                "hook_event_name": "PreToolUse",
                "session_id": "cx-h1",
                "cwd": str(self.tmp),
                "tool_name": "request_user_input",
                "tool_input": {"questions": [{"question": "Select environment"}]},
            }),
            {},
        )
        self.assertEqual(hub.get_session("cx-h1")["state"], "waiting_question")
        self.assertEqual(hub.get_session("cx-h1")["pending_action"]["detail"], "Select environment")

        # Keep Codex waiting on question for MCP/CLI filter check below, test Interrupt/SessionEnd on cx-h2
        fire("codex", {"hook_event_name": "SessionStart", "session_id": "cx-h2", "cwd": str(self.tmp)})
        fire("codex", {"hook_event_name": "Interrupt", "session_id": "cx-h2", "cwd": str(self.tmp)})
        self.assertEqual(hub.get_session("cx-h2")["state"], "idle")
        fire("codex", {"hook_event_name": "SessionEnd", "session_id": "cx-h2", "cwd": str(self.tmp)})
        self.assertEqual(hub.get_session("cx-h2")["state"], "exited")

        # --- Antigravity ---
        fire("antigravity", {"hookEventName": "SessionStart", "conversationId": "agy-h1", "cwd": str(self.tmp)})
        hub.send_message(device="mini", from_session="mini:claude:sender-h", to="mini:antigravity:agy-h1", body="agy msg")

        pre_q = fire("antigravity", {
            "hookEventName": "PreToolUse",
            "conversationId": "agy-h1",
            "cwd": str(self.tmp),
            "toolCall": {"name": "ask_question", "args": {"questions": [{"question": "Keep both options?"}]}},
        })
        self.assertEqual(pre_q, {"decision": "allow"})
        self.assertEqual(hub.get_session("agy-h1")["state"], "waiting_question")
        self.assertEqual(hub.get_session("agy-h1")["pending_action"]["detail"], "Keep both options?")
        self.assertEqual(len(hub.fetch_inbox(device="mini", session_id="mini:antigravity:agy-h1", mark_read=False)), 1)

        self.assertEqual(
            fire("antigravity", {"hookEventName": "PostToolUse", "conversationId": "agy-h1", "cwd": str(self.tmp)}),
            {},
        )
        self.assertEqual(hub.get_session("agy-h1")["state"], "working")
        pre_inv = fire("antigravity", {"hookEventName": "PreInvocation", "conversationId": "agy-h1", "cwd": str(self.tmp)})
        self.assertIn("agy msg", pre_inv["injectSteps"][0]["systemMessage"]["systemMessage"])

        pre_p = fire("antigravity", {
            "hookEventName": "PreToolUse",
            "conversationId": "agy-h1",
            "cwd": str(self.tmp),
            "toolCall": {
                "name": "ask_permission",
                "args": {"Action": "Write", "Target": "/etc/hosts", "Reason": "Add local test alias"},
            },
        })
        self.assertEqual(pre_p, {"decision": "allow"})
        self.assertEqual(hub.get_session("agy-h1")["state"], "waiting_permission")
        self.assertIn("Write /etc/hosts", hub.get_session("agy-h1")["pending_action"]["detail"])

        # --- MCP fleet_list_agents(state="waiting") & fleet_inspect_agent & CLI `fleet agents --waiting` ---
        reqs = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-03-26"}},
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "fleet_list_agents", "arguments": {"state": "waiting"}},
            },
            {
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {"name": "fleet_inspect_agent", "arguments": {"agent_id": "agy-h1"}},
            },
        ]
        mcp_out = io.StringIO()
        mcp.serve_stdio(
            "antigravity",
            stdin=io.StringIO("\n".join(json.dumps(r) for r in reqs) + "\n"),
            stdout=mcp_out,
            api_url=self.api,
        )
        mcp_resps = [json.loads(line) for line in mcp_out.getvalue().splitlines() if line.strip()]
        waiting_list = json.loads(mcp_resps[1]["result"]["content"][0]["text"])["agents"]
        self.assertEqual({a["session_id"] for a in waiting_list}, {"mini:codex:cx-h1", "mini:antigravity:agy-h1"})
        inspected = json.loads(mcp_resps[2]["result"]["content"][0]["text"])["agent"]
        self.assertEqual(inspected["state"], "waiting_permission")
        self.assertEqual(inspected["pending_action"]["tool"], "ask_permission")

        parser = cli.build_parser()
        with mock.patch.object(cli, "api", return_value=self.api):
            buf = io.StringIO()
            with redirect_stdout(buf):
                ns = parser.parse_args(["agents", "--waiting"])
                ns.func(ns)
            table = buf.getvalue()
            self.assertIn("waiting_permission", table)
            self.assertIn("waiting_question", table)
            self.assertIn("ask_permission", table)
            self.assertNotIn("cl-h1", table)


if __name__ == "__main__":
    unittest.main()

"""Tests for cross-node agent registry, symmetric HTTP API, MCP stdio, hooks, and config installer."""

from __future__ import annotations

import io
import json
import os
import shutil
import tempfile
import threading
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest import mock

from fleetctl import hooks, mcp, mcp_install, pki
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
            ["fleet_list_agents", "fleet_inspect_agent", "fleet_send_message", "fleet_check_inbox", "fleet_ask_agent"],
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

        # Uninstall cleans fleet entries while keeping existing settings intact
        mcp_install.uninstall(self.home)
        self.assertEqual(mcp_install.status(self.home), {"claude": False, "codex": False, "antigravity": False})
        self.assertIn("other", json.loads((self.home / ".claude.json").read_text())["mcpServers"])
        self.assertIn('trusted_hash = "sha256:abc"', (self.home / ".codex" / "config.toml").read_text())
        self.assertNotIn("[mcp_servers.fleet]", (self.home / ".codex" / "config.toml").read_text())


if __name__ == "__main__":
    unittest.main()

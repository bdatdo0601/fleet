"""Stdlib-only MCP stdio server bridging local agent sessions to the lighthouse API."""

from __future__ import annotations

import json
import os
import sys
import threading
import urllib.parse
from typing import IO

from .client import ApiError, call
from .hooks import git_context, read_transcript_tail
from .paths import HARNESSES, node_file

HEARTBEAT_INTERVAL_SECONDS = 45.0

TOOLS = [
    {
        "name": "fleet_list_agents",
        "description": "List active AI agent sessions (claude, codex, antigravity) across the Nebula fleet.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "device": {"type": "string", "description": "Optional fleet device name filter (e.g. mini, laptop)."},
                "repo": {"type": "string", "description": "Optional repository name filter."},
                "harness": {"type": "string", "enum": list(HARNESSES), "description": "Optional harness filter."},
                "include_stale": {"type": "boolean", "description": "Include recently inactive or exited sessions."},
            },
        },
    },
    {
        "name": "fleet_inspect_agent",
        "description": "Inspect another agent session's cwd, git branch/status, and recent conversation/tool activity.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "agent_id": {
                    "type": "string",
                    "description": "Full session_id (e.g. laptop:claude:abc123) or unique short suffix.",
                },
                "tail_limit": {
                    "type": "integer",
                    "description": "Optional maximum number of recent tail entries to return.",
                },
            },
            "required": ["agent_id"],
        },
    },
    {
        "name": "fleet_send_message",
        "description": "Send a message to a peer agent session, device (device:name), repo (repo:name), or all (*).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "to": {
                    "type": "string",
                    "description": "Recipient session_id, short id, device name, device:<name>, repo:<name>, or '*'.",
                },
                "message": {"type": "string", "description": "Message text to deliver."},
                "session_id": {"type": "string", "description": "Caller session_id if explicitly provided."},
                "thread_id": {"type": "string", "description": "Optional thread_id to continue an existing thread."},
                "reply_to": {"type": "integer", "description": "Optional message id being replied to."},
            },
            "required": ["to", "message"],
        },
    },
    {
        "name": "fleet_check_inbox",
        "description": "Check incoming peer messages for this agent session.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "session_id": {"type": "string", "description": "Caller session_id if explicitly provided."},
                "unread_only": {"type": "boolean", "description": "Return only unread messages (default true)."},
            },
        },
    },
    {
        "name": "fleet_ask_agent",
        "description": "Send a question to another agent session and wait for its reply.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "to": {"type": "string", "description": "Target session_id or unique short id."},
                "question": {"type": "string", "description": "Question to ask the peer agent."},
                "session_id": {"type": "string", "description": "Caller session_id if explicitly provided."},
                "timeout_seconds": {
                    "type": "number",
                    "description": "Maximum seconds to wait for a reply (default 30, max 120).",
                },
            },
            "required": ["to", "question"],
        },
    },
]


def _resolve_api(api_url: str | None) -> str:
    if api_url:
        return api_url
    try:
        return json.loads(node_file().read_text())["api"]
    except FileNotFoundError as e:
        raise ApiError("this machine hasn't joined a fleet (fleet join NAME)") from e


def _send_heartbeat(api_url: str | None, harness: str, ppid: int, cwd: str) -> None:
    try:
        api = _resolve_api(api_url)
        repo, branch, git_status = git_context(cwd)
        summary, tail = read_transcript_tail(harness, None, cwd=cwd)
        payload: dict = {
            "harness": harness,
            "raw_id": f"pid-{ppid}",
            "pid": ppid,
            "cwd": cwd,
            "repo": repo,
            "branch": branch,
            "state": "working",
            "git_status": git_status,
            "drain_inbox": False,
        }
        if summary:
            payload["summary"] = summary
        if tail:
            payload["tail"] = tail
        call(api, "POST", "/v1/agents/heartbeat", payload, timeout=2.0)
    except (ApiError, OSError, ValueError, KeyError):
        pass


def _deregister(api_url: str | None, harness: str, ppid: int) -> None:
    try:
        api = _resolve_api(api_url)
        call(api, "POST", "/v1/agents/deregister", {"harness": harness, "pid": ppid}, timeout=2.0)
    except (ApiError, OSError, ValueError, KeyError):
        pass


def _call_tool(name: str, args: dict, *, api_url: str | None, harness: str, ppid: int, cwd: str) -> dict:
    api = _resolve_api(api_url)
    if name == "fleet_list_agents":
        params = {}
        for key in ("device", "repo", "harness"):
            if args.get(key):
                params[key] = str(args[key])
        if args.get("include_stale"):
            params["include_stale"] = "true"
        qs = f"?{urllib.parse.urlencode(params)}" if params else ""
        return call(api, "GET", f"/v1/agents{qs}")

    if name == "fleet_inspect_agent":
        agent_id = str(args.get("agent_id") or "").strip()
        if not agent_id:
            raise ValueError("agent_id is required")
        res = call(api, "GET", f"/v1/agents/{urllib.parse.quote(agent_id, safe='')}")
        if args.get("tail_limit") is not None and isinstance(res.get("agent"), dict):
            limit = max(0, int(args["tail_limit"]))
            tail = res["agent"].get("tail") or []
            res["agent"]["tail"] = tail[-limit:] if limit else []
        return res

    if name == "fleet_send_message":
        body = {
            "to": str(args.get("to") or ""),
            "body": str(args.get("message") or ""),
            "harness": harness,
            "cwd": cwd,
            "pid": ppid,
        }
        if args.get("session_id"):
            body["session_id"] = str(args["session_id"])
        if args.get("thread_id"):
            body["thread_id"] = str(args["thread_id"])
        if args.get("reply_to") is not None:
            body["reply_to"] = int(args["reply_to"])
        return call(api, "POST", "/v1/agents/send", body)

    if name == "fleet_check_inbox":
        body: dict = {
            "harness": harness,
            "cwd": cwd,
            "pid": ppid,
            "unread_only": bool(args.get("unread_only", True)),
            "mark_read": True,
        }
        if args.get("session_id"):
            body["session_id"] = str(args["session_id"])
        return call(api, "POST", "/v1/agents/inbox", body)

    if name == "fleet_ask_agent":
        timeout_sec = float(args.get("timeout_seconds", 30.0))
        body = {
            "to": str(args.get("to") or ""),
            "body": str(args.get("question") or ""),
            "harness": harness,
            "cwd": cwd,
            "pid": ppid,
            "timeout": timeout_sec,
        }
        if args.get("session_id"):
            body["session_id"] = str(args["session_id"])
        return call(api, "POST", "/v1/agents/ask", body, timeout=max(5.0, min(timeout_sec, 120.0) + 10.0))

    raise ValueError(f"unknown tool {name!r}")


def _jsonrpc_result(req_id: object, result: dict) -> dict:
    return {"jsonrpc": "2.0", "id": req_id, "result": result}


def _jsonrpc_error(req_id: object, code: int, message: str) -> dict:
    return {"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}}


def serve_stdio(
    harness: str,
    stdin: IO[str] | None = None,
    stdout: IO[str] | None = None,
    api_url: str | None = None,
    heartbeat_interval: float = HEARTBEAT_INTERVAL_SECONDS,
) -> int:
    """Run an MCP JSON-RPC 2.0 server over newline-delimited stdio."""
    if harness not in HARNESSES:
        raise ValueError(f"unknown harness {harness!r}")
    in_stream = stdin or sys.stdin
    out_stream = stdout or sys.stdout
    ppid = os.getppid()
    cwd = os.getcwd()
    stop_event = threading.Event()
    hb_thread: threading.Thread | None = None

    if harness in ("claude", "codex"):
        def heartbeat_loop() -> None:
            _send_heartbeat(api_url, harness, ppid, cwd)
            while not stop_event.wait(heartbeat_interval):
                _send_heartbeat(api_url, harness, ppid, cwd)

        hb_thread = threading.Thread(target=heartbeat_loop, daemon=True)
        hb_thread.start()

    try:
        for line in in_stream:
            line = line.strip()
            if not line:
                continue
            try:
                req = json.loads(line)
            except ValueError:
                out_stream.write(json.dumps(_jsonrpc_error(None, -32700, "parse error")) + "\n")
                out_stream.flush()
                continue
            if not isinstance(req, dict):
                continue
            req_id = req.get("id")
            method = req.get("method")
            params = req.get("params") if isinstance(req.get("params"), dict) else {}

            if req_id is None:
                # Notification (e.g. notifications/initialized): no response allowed in JSON-RPC 2.0.
                continue

            if method == "initialize":
                proto = params.get("protocolVersion") or "2024-11-05"
                resp = _jsonrpc_result(req_id, {
                    "protocolVersion": proto,
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": "fleet", "version": "0.1.0"},
                })
            elif method == "ping":
                resp = _jsonrpc_result(req_id, {})
            elif method == "tools/list":
                resp = _jsonrpc_result(req_id, {"tools": TOOLS})
            elif method == "tools/call":
                tool_name = str(params.get("name") or "")
                tool_args = params.get("arguments") if isinstance(params.get("arguments"), dict) else {}
                try:
                    data = _call_tool(
                        tool_name,
                        tool_args,
                        api_url=api_url,
                        harness=harness,
                        ppid=ppid,
                        cwd=cwd,
                    )
                    resp = _jsonrpc_result(req_id, {
                        "content": [{"type": "text", "text": json.dumps(data, indent=2)}],
                        "isError": False,
                    })
                except (ApiError, OSError, ValueError, KeyError) as e:
                    resp = _jsonrpc_result(req_id, {
                        "content": [{"type": "text", "text": str(e)}],
                        "isError": True,
                    })
            else:
                resp = _jsonrpc_error(req_id, -32601, f"method not found: {method}")

            out_stream.write(json.dumps(resp) + "\n")
            out_stream.flush()
    finally:
        stop_event.set()
        if hb_thread is not None:
            hb_thread.join(timeout=1.0)
            _deregister(api_url, harness, ppid)
    return 0

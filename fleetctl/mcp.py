"""Stdlib-only MCP stdio server bridging local agent sessions to the lighthouse API."""

from __future__ import annotations

import json
import os
import sys
import threading
import urllib.parse
from collections.abc import Callable
from typing import IO

from .client import ApiError, call
from .hooks import git_context, read_session_title, read_transcript_tail
from .paths import HARNESSES, node_file
from .waiter import CodexQueueListener

HEARTBEAT_INTERVAL_SECONDS = 45.0

TOOLS = [
    {
        "name": "fleet_list_agents",
        "description": (
            "List active AI agent sessions (claude, codex, antigravity) across the Nebula fleet, "
            "including friendly names, titles, listening status, and delivery modes."
        ),
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
                    "description": "Target session name (or device/name), title, short ID, or full session_id.",
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
                    "description": (
                        "Recipient session name (or device/name), title, short id, full session_id, "
                        "device name, device:<name>, repo:<name>, or '*'."
                    ),
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
                "to": {
                    "type": "string",
                    "description": "Target session name (or device/name), title, short id, or full session_id.",
                },
                "question": {"type": "string", "description": "Question to ask the peer agent."},
                "session_id": {"type": "string", "description": "Caller session_id if explicitly provided."},
                "timeout_seconds": {
                    "type": "number",
                    "description": "Maximum seconds to wait for a reply (default 30, max 120).",
                },
                "wait_if_idle": {
                    "type": "boolean",
                    "description": (
                        "Wait for a reply even if the target is currently idle with no listener (default false)."
                    ),
                },
            },
            "required": ["to", "question"],
        },
    },
    {
        "name": "fleet_set_name",
        "description": "Set a friendly name for this agent session so peers can address it easily.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "Friendly handle for this session (e.g. auth-worker)."},
                "session_id": {"type": "string", "description": "Caller session_id if explicitly provided."},
            },
            "required": ["name"],
        },
    },
    {
        "name": "fleet_listen",
        "description": "Check, start, or stop real-time background inbox listening for this session.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["status", "start", "stop"],
                    "description": "Listener action (default status).",
                },
                "session_id": {"type": "string", "description": "Caller session_id if explicitly provided."},
            },
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


def _send_heartbeat(
    api_url: str | None,
    harness: str,
    ppid: int,
    cwd: str,
    known_full_id: str = "",
) -> str:
    try:
        api = _resolve_api(api_url)
        repo, branch, git_status = git_context(cwd)
        summary, tail = read_transcript_tail(harness, None, cwd=cwd)
        title, is_explicit = read_session_title(harness, known_full_id, None)
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
        if title:
            payload["title"] = title
        if is_explicit and title:
            payload["name"] = title
            payload["explicit_name"] = True
        if summary:
            payload["summary"] = summary
        if tail:
            payload["tail"] = tail
        resp = call(api, "POST", "/v1/agents/heartbeat", payload, timeout=2.0)
        return str(resp.get("full_raw_id") or known_full_id)
    except (ApiError, OSError, ValueError, KeyError):
        return known_full_id


def _deregister(api_url: str | None, harness: str, ppid: int) -> None:
    try:
        api = _resolve_api(api_url)
        call(api, "POST", "/v1/agents/deregister", {"harness": harness, "pid": ppid}, timeout=2.0)
    except (ApiError, OSError, ValueError, KeyError):
        pass


def _resolve_sid_non_destructive(api: str, *, harness: str, cwd: str, ppid: int, explicit_sid: object) -> str:
    if explicit_sid:
        return str(explicit_sid)
    resp = call(
        api,
        "POST",
        "/v1/agents/inbox",
        {"harness": harness, "cwd": cwd, "pid": ppid, "unread_only": False, "mark_read": False},
    )
    return str(resp.get("session_id") or "")


def _call_tool(
    name: str,
    args: dict,
    *,
    api_url: str | None,
    harness: str,
    ppid: int,
    cwd: str,
    listener: CodexQueueListener | None = None,
) -> dict:
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
            "wait_if_idle": bool(args.get("wait_if_idle", False)),
        }
        if args.get("session_id"):
            body["session_id"] = str(args["session_id"])
        return call(api, "POST", "/v1/agents/ask", body, timeout=max(5.0, min(timeout_sec, 120.0) + 10.0))

    if name == "fleet_set_name":
        body = {
            "name": str(args.get("name") or ""),
            "harness": harness,
            "cwd": cwd,
            "pid": ppid,
        }
        if args.get("session_id"):
            body["session_id"] = str(args["session_id"])
        return call(api, "POST", "/v1/agents/rename", body)

    if name == "fleet_listen":
        action = str(args.get("action") or "status")
        if action not in ("status", "start", "stop"):
            raise ValueError(f"invalid action {action!r}")
        if harness == "codex":
            if listener is not None:
                if action == "start":
                    st = listener.start()
                elif action == "stop":
                    st = listener.pause()
                else:
                    st = listener.status()
            else:
                st = {
                    "running": False,
                    "enabled": False,
                    "session_id": str(args.get("session_id") or ""),
                    "full_raw_id": "",
                    "delivered_count": 0,
                    "last_error": None,
                }
            sid = str(args.get("session_id") or st.get("session_id") or "")
            if not sid:
                try:
                    sid = _resolve_sid_non_destructive(
                        api, harness=harness, cwd=cwd, ppid=ppid, explicit_sid=None
                    )
                except ApiError:
                    sid = ""
            sess: dict = {}
            if sid:
                try:
                    sess = call(api, "GET", f"/v1/agents/{urllib.parse.quote(sid, safe='')}").get("agent") or {}
                except ApiError:
                    sess = {}
            return {
                "harness": "codex",
                "mode": "mcp_codex_queue",
                "action": action,
                **st,
                "session_id": sid or st.get("session_id") or "",
                "name": sess.get("name", ""),
                "listening": bool(sess.get("listening", st["enabled"])),
                "delivery": sess.get("delivery", "immediate" if st["enabled"] else "queued_idle"),
            }

        sid = _resolve_sid_non_destructive(
            api, harness=harness, cwd=cwd, ppid=ppid, explicit_sid=args.get("session_id")
        )
        try:
            sess = call(api, "GET", f"/v1/agents/{urllib.parse.quote(sid, safe='')}").get("agent") or {}
        except ApiError:
            sess = {}
        cmd = f"fleet agents wait --session {sid}"
        if harness == "claude":
            hint = (
                "Run this command in the background with Bash(run_in_background=true), "
                "and re-run it after handling each incoming message."
            )
        else:
            hint = (
                "Run this command in the background with run_command(WaitMsBeforeAsync=500, IsDaemon=false), "
                "and re-run it after handling each incoming message."
            )
        return {
            "harness": harness,
            "mode": "background_command",
            "action": action,
            "session_id": sid,
            "name": sess.get("name", ""),
            "listening": bool(sess.get("listening", False)),
            "delivery": sess.get("delivery", "offline"),
            "command": cmd,
            "instruction": hint,
        }

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
    auto_listen: bool = True,
    codex_run_queue_fn: Callable[[list[str]], tuple[int, str]] | None = None,
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
    listener: CodexQueueListener | None = None

    if harness in ("claude", "codex"):
        def heartbeat_loop() -> None:
            known_full = _send_heartbeat(api_url, harness, ppid, cwd)
            while not stop_event.wait(heartbeat_interval):
                known_full = _send_heartbeat(api_url, harness, ppid, cwd, known_full)

        hb_thread = threading.Thread(target=heartbeat_loop, daemon=True)
        hb_thread.start()

    if harness == "codex" and auto_listen:
        listener = CodexQueueListener(
            api_url=api_url,
            ppid=ppid,
            cwd=cwd,
            run_queue_fn=codex_run_queue_fn,
        )
        listener.start()

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
                        listener=listener,
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
        if listener is not None:
            listener.stop()
        if hb_thread is not None:
            hb_thread.join(timeout=1.0)
            _deregister(api_url, harness, ppid)
    return 0

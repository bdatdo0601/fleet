"""`fleet serve`: the lighthouse's HTTP API.

Listens on all interfaces but answers only loopback (the lighthouse itself) and
mesh addresses. The port is never port-forwarded, so the internet can't reach it,
and a LAN host can't complete a TCP handshake with a spoofed mesh source address
because replies route into the Nebula tunnel.
"""

from __future__ import annotations

import json
import sys
import urllib.parse
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .agents import AgentError
from .core import Fleet, FleetError

MAX_BODY = 256 * 1024


def make_handler(fleet: Fleet, quiet: bool = False) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        server_version = "fleet/1"

        def log_message(self, fmt: str, *args: object) -> None:
            if not quiet:
                sys.stderr.write(f"{self.client_address[0]} {fmt % args}\n")

        def _send(self, status: int, body: dict) -> None:
            data = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _body(self) -> dict:
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                raise FleetError("request too large", 413)
            try:
                return json.loads(self.rfile.read(length) or b"{}")
            except json.JSONDecodeError as e:
                raise FleetError("invalid JSON") from e

        def _dispatch_agents(self, caller_name: str, method: str, path_only: str, query: dict[str, list[str]]) -> bool:
            def q(key: str) -> str | None:
                vals = query.get(key)
                return vals[0] if vals and vals[0] else None

            if (method, path_only) == ("GET", "/v1/agents"):
                self._send(200, {"agents": fleet.agents.list_sessions(
                    device=q("device"), repo=q("repo"), harness=q("harness"),
                    include_stale=(q("include_stale") or "").lower() in ("1", "true"),
                )})  # fmt: skip
                return True
            if method == "GET" and path_only.startswith("/v1/agents/"):
                target = urllib.parse.unquote(path_only[len("/v1/agents/"):])
                self._send(200, {"agent": fleet.agents.get_session(target)})
                return True
            if method != "POST":
                return False

            body = self._body()
            harness = str(body.get("harness", "claude"))
            cwd = str(body.get("cwd", ""))
            pid = int(body["pid"]) if body.get("pid") is not None else None

            if path_only == "/v1/agents/heartbeat":
                tail = body.get("tail")
                res = fleet.agents.heartbeat(
                    device=caller_name, harness=harness, raw_id=str(body.get("raw_id", "")),
                    pid=pid, cwd=cwd, repo=str(body.get("repo", "")), branch=str(body.get("branch", "")),
                    state=str(body.get("state", "working")),
                    summary=str(body["summary"]) if body.get("summary") is not None else None,
                    git_status=str(body["git_status"]) if body.get("git_status") is not None else None,
                    tail=tail if isinstance(tail, list) else None,
                    drain_inbox=bool(body.get("drain_inbox", False)),
                )  # fmt: skip
                self._send(200, res)
                return True
            if path_only == "/v1/agents/deregister":
                sid = str(body["session_id"]) if body.get("session_id") else None
                res = fleet.agents.deregister(
                    device=caller_name, session_id=sid,
                    harness=str(body["harness"]) if body.get("harness") else None, pid=pid,
                )  # fmt: skip
                self._send(200, res)
                return True
            if path_only == "/v1/agents/send":
                from_sid = fleet.agents.resolve_caller_session(
                    device=caller_name, harness=harness,
                    session_id=str(body["session_id"]) if body.get("session_id") else None, cwd=cwd, pid=pid,
                )
                reply_to = int(body["reply_to"]) if body.get("reply_to") is not None else None
                msg = fleet.agents.send_message(
                    device=caller_name, from_session=from_sid, to=str(body.get("to", "")),
                    body=str(body.get("body", "")),
                    thread_id=str(body["thread_id"]) if body.get("thread_id") else None,
                    reply_to=reply_to, known_devices=fleet.active_device_names(),
                )  # fmt: skip
                self._send(200, {"message": msg})
                return True
            if path_only == "/v1/agents/inbox":
                sid = fleet.agents.resolve_caller_session(
                    device=caller_name, harness=harness,
                    session_id=str(body["session_id"]) if body.get("session_id") else None, cwd=cwd, pid=pid,
                )
                msgs = fleet.agents.fetch_inbox(
                    device=caller_name, session_id=sid,
                    unread_only=bool(body.get("unread_only", True)),
                    mark_read=bool(body.get("mark_read", True)),
                )  # fmt: skip
                self._send(200, {"session_id": sid, "messages": msgs})
                return True
            if path_only == "/v1/agents/ask":
                from_sid = fleet.agents.resolve_caller_session(
                    device=caller_name, harness=harness,
                    session_id=str(body["session_id"]) if body.get("session_id") else None, cwd=cwd, pid=pid,
                )
                res = fleet.agents.ask(
                    device=caller_name, from_session=from_sid, to=str(body.get("to", "")),
                    body=str(body.get("body", "")), timeout=float(body.get("timeout", 30.0)),
                    known_devices=fleet.active_device_names(),
                )  # fmt: skip
                self._send(200, res)
                return True
            return False

        def _dispatch(self, method: str) -> None:
            try:
                caller = fleet.identify(self.client_address[0])
                if caller is None:
                    raise FleetError("not a member of this fleet", 403)
                path_only, _, query_str = self.path.partition("?")
                route = (method, path_only)
                if route == ("GET", "/v1/sync"):
                    return self._send(200, fleet.sync(caller))
                if path_only == "/v1/agents" or path_only.startswith("/v1/agents/"):
                    if self._dispatch_agents(caller.name, method, path_only, urllib.parse.parse_qs(query_str)):
                        return
                    raise FleetError("not found", 404)
                if not fleet.is_admin(caller):
                    raise FleetError(f"{caller.name} is not an admin device", 403)
                if route == ("GET", "/v1/devices"):
                    return self._send(200, {"devices": fleet.list(), "ca_id": fleet.ca_id()})
                body = self._body() if method == "POST" else {}
                if route == ("POST", "/v1/approve"):
                    return self._send(200, fleet.approve(
                        name=str(body.get("name", "")), pubkey=str(body.get("pubkey", "")),
                        kind=str(body.get("kind", "mac")), group=str(body.get("group", "managed")),
                        passphrase=str(body.get("passphrase", "")), replace=bool(body.get("replace")),
                    ))  # fmt: skip
                if route == ("POST", "/v1/revoke"):
                    fleet.revoke(str(body.get("name", "")))
                    return self._send(200, {"ok": True})
                if route == ("POST", "/v1/set-group"):
                    return self._send(200, fleet.set_group(
                        str(body.get("name", "")), str(body.get("group", "")), str(body.get("passphrase", ""))
                    ))  # fmt: skip
                raise FleetError("not found", 404)
            except (FleetError, AgentError) as e:
                self._send(e.status, {"error": str(e)})
            except Exception as e:  # noqa: BLE001 - report, keep serving
                self.log_message("internal error: %r", e)
                self._send(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": "internal error (see serve log)"})

        def do_GET(self) -> None:
            self._dispatch("GET")

        def do_POST(self) -> None:
            self._dispatch("POST")

    return Handler


def make_server(fleet: Fleet, host: str, port: int, quiet: bool = False) -> ThreadingHTTPServer:
    return ThreadingHTTPServer((host, port), make_handler(fleet, quiet))


def serve(port: int, host: str = "0.0.0.0") -> None:  # noqa: S104 - see module docstring
    fleet = Fleet()
    httpd = make_server(fleet, host, port)
    sys.stderr.write(f"fleet serve listening on {host}:{port}\n")
    httpd.serve_forever()

"""`fleet serve`: the lighthouse's HTTP API.

Listens on all interfaces but answers only loopback (the lighthouse itself) and
mesh addresses. The port is never port-forwarded, so the internet can't reach it,
and a LAN host can't complete a TCP handshake with a spoofed mesh source address
because replies route into the Nebula tunnel.
"""

from __future__ import annotations

import json
import sys
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from .core import Fleet, FleetError

MAX_BODY = 64 * 1024


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

        def _dispatch(self, method: str) -> None:
            try:
                caller = fleet.identify(self.client_address[0])
                if caller is None:
                    raise FleetError("not a member of this fleet", 403)
                route = (method, self.path.split("?")[0])
                if route == ("GET", "/v1/sync"):
                    return self._send(200, fleet.sync(caller))
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
            except FleetError as e:
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

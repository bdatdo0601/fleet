"""HTTP client for the lighthouse API (stdlib only)."""

from __future__ import annotations

import json
import urllib.error
import urllib.request


class ApiError(RuntimeError):
    pass


def call(api: str, method: str, path: str, body: dict | None = None, timeout: float = 30) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"{api}{path}", data=data, method=method)  # noqa: S310 - http mesh URL
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 - fixed http mesh URL
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        with e:
            try:
                msg = json.loads(e.read()).get("error", e.reason)
            except Exception:  # noqa: BLE001
                msg = e.reason
        raise ApiError(f"{msg} (HTTP {e.code})") from None
    except (urllib.error.URLError, OSError) as e:
        raise ApiError(f"can't reach the lighthouse at {api}: {getattr(e, 'reason', e)}") from e

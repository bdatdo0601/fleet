"""HTTP client for the lighthouse API (stdlib only)."""

from __future__ import annotations

import hashlib
import json
import urllib.error
import urllib.request
from pathlib import Path


class ApiError(RuntimeError):
    pass


def call(
    api: str,
    method: str,
    path: str,
    body: dict | None = None,
    timeout: float = 30,
    headers: dict[str, str] | None = None,
) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"{api}{path}", data=data, method=method)  # noqa: S310 - http mesh URL
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if headers:
        for k, v in headers.items():
            req.add_header(k, v)
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


def download(
    api: str,
    path: str,
    dest: str | Path,
    timeout: float = 60,
    headers: dict[str, str] | None = None,
) -> tuple[str, int]:
    """Stream `GET {api}{path}` to `dest`, returning `(sha256_hex, byte_size)`."""
    req = urllib.request.Request(f"{api}{path}", method="GET")  # noqa: S310 - http mesh URL
    if headers:
        for k, v in headers.items():
            req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 - fixed http mesh URL
            hasher = hashlib.sha256()
            total = 0
            dest_path = Path(dest)
            dest_path.parent.mkdir(parents=True, exist_ok=True)
            with dest_path.open("wb") as f:
                while True:
                    chunk = resp.read(65536)
                    if not chunk:
                        break
                    hasher.update(chunk)
                    f.write(chunk)
                    total += len(chunk)
            sha_hex = hasher.hexdigest()
            expected = (resp.headers.get("X-Fleet-SHA256") or "").strip()
            if expected and expected.lower() != sha_hex.lower():
                raise ApiError(f"downloaded binary SHA-256 mismatch: expected {expected}, got {sha_hex}")
            return sha_hex, total
    except urllib.error.HTTPError as e:
        with e:
            try:
                msg = json.loads(e.read()).get("error", e.reason)
            except Exception:  # noqa: BLE001
                msg = e.reason
        raise ApiError(f"{msg} (HTTP {e.code})") from None
    except (urllib.error.URLError, OSError) as e:
        raise ApiError(f"can't reach the lighthouse at {api}: {getattr(e, 'reason', e)}") from e

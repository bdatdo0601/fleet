"""Copy-paste blocks that carry join requests/responses between machines.

Blocks are PEM-style and wrapped at 64 columns: macOS terminals truncate a
single pasted line at 1024 bytes, and a response is ~2 KB.
"""

from __future__ import annotations

import base64
import hashlib
import json
import sys
import zlib
from typing import TextIO

REQUEST = "FLEET JOIN REQUEST"
RESPONSE = "FLEET JOIN RESPONSE"


class CodeError(ValueError):
    pass


def encode(label: str, payload: dict) -> str:
    raw = zlib.compress(json.dumps(payload, separators=(",", ":")).encode(), 9)
    b64 = base64.b64encode(raw).decode()
    lines = [b64[i : i + 64] for i in range(0, len(b64), 64)]
    return "\n".join([f"-----BEGIN {label}-----", *lines, f"-----END {label}-----"])


def decode(label: str, text: str) -> dict:
    begin, end = f"-----BEGIN {label}-----", f"-----END {label}-----"
    if begin not in text or end not in text:
        raise CodeError(f"expected a {label} block")
    body = text.split(begin, 1)[1].split(end, 1)[0]
    try:
        return json.loads(zlib.decompress(base64.b64decode("".join(body.split()), validate=True)))
    except Exception as e:
        raise CodeError(f"{label} block is damaged; copy it again ({e})") from e


def read_block(end_marker: str, stream: TextIO | None = None) -> str:
    """Read pasted lines up to and including the line containing end_marker."""
    lines = []
    for line in stream if stream is not None else sys.stdin:
        lines.append(line)
        if end_marker in line:
            break
    return "".join(lines)


def short_id(text: str) -> str:
    """Human-comparable id, shown on both ends of a paste to detect tampering."""
    h = hashlib.sha256(text.encode()).hexdigest()[:12]
    return "-".join(h[i : i + 4] for i in range(0, 12, 4))

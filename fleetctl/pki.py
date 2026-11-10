"""Thin wrappers around the `nebula-cert` binary."""

from __future__ import annotations

import base64
import json
import os
import re
import shutil
import subprocess
import tempfile
from datetime import datetime, timezone
from pathlib import Path

from .paths import CA_HOURS, MESH, MESH_PREFIX

PUBKEY_PEM = re.compile(
    r"-----BEGIN NEBULA X25519 PUBLIC KEY-----\s*([A-Za-z0-9+/=\s]+?)\s*-----END NEBULA X25519 PUBLIC KEY-----"
)


class PkiError(RuntimeError):
    pass


def _bin() -> str:
    path = shutil.which("nebula-cert")
    if not path:
        raise PkiError("nebula-cert not found (brew install nebula)")
    return path


def run(*args: str, passphrase: str | None = None) -> str:
    env = dict(os.environ)
    env.pop("NEBULA_CA_PASSPHRASE", None)
    if passphrase is not None:
        env["NEBULA_CA_PASSPHRASE"] = passphrase
    proc = subprocess.run(
        [_bin(), *args], env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True
    )
    if proc.returncode != 0:
        msg = (proc.stderr or proc.stdout).strip().removeprefix("Error: ")
        raise PkiError(msg or f"nebula-cert {args[0]} failed")
    return proc.stdout


def create_ca(key: Path, crt: Path, passphrase: str, name: str = "fleet") -> None:
    run(
        "ca", "-name", name, "-duration", f"{CA_HOURS}h", "-encrypt",
        "-networks", str(MESH), "-out-key", str(key), "-out-crt", str(crt),
        passphrase=passphrase,
    )  # fmt: skip


def keygen(key: Path, pub: Path) -> None:
    run("keygen", "-out-key", str(key), "-out-pub", str(pub))


def normalize_pubkey(text: str) -> str:
    """Extract a canonical Nebula public key PEM from pasted text."""
    m = PUBKEY_PEM.search(text)
    if not m:
        raise PkiError("not a Nebula public key (expected a NEBULA X25519 PUBLIC KEY block)")
    body = "".join(m.group(1).split())
    raw = base64.b64decode(body, validate=True)
    if len(raw) != 32:
        raise PkiError("public key has the wrong length")
    return f"-----BEGIN NEBULA X25519 PUBLIC KEY-----\n{body}\n-----END NEBULA X25519 PUBLIC KEY-----\n"


def pubkey_hex(pem: str) -> str:
    body = "".join(PUBKEY_PEM.search(pem).group(1).split())  # type: ignore[union-attr]
    return base64.b64decode(body).hex()


def sign(
    *, ca_key: Path, ca_crt: Path, pubkey: str, name: str, ip: str, group: str,
    passphrase: str, hours: int | None = None,
) -> str:  # fmt: skip
    with tempfile.TemporaryDirectory() as tmp:
        pub = Path(tmp, "host.pub")
        out = Path(tmp, "host.crt")
        pub.write_text(normalize_pubkey(pubkey))
        args = [
            "sign", "-ca-key", str(ca_key), "-ca-crt", str(ca_crt), "-name", name,
            "-networks", f"{ip}/{MESH_PREFIX}", "-groups", group,
            "-in-pub", str(pub), "-out-crt", str(out),
        ]  # fmt: skip
        if hours is not None:
            args += ["-duration", f"{hours}h"]
        run(*args, passphrase=passphrase)
        return out.read_text()


def info(cert_pem: str) -> dict:
    with tempfile.TemporaryDirectory() as tmp:
        p = Path(tmp, "c.crt")
        p.write_text(cert_pem)
        data = json.loads(run("print", "-json", "-path", str(p)))
    c = data[0] if isinstance(data, list) else data
    d = c["details"]
    return {
        "name": d["name"],
        "fingerprint": c["fingerprint"],
        "public_key": c["publicKey"],
        "groups": d.get("groups") or [],
        "networks": d.get("networks") or [],
        "not_after": d["notAfter"],
        "is_ca": d.get("isCa", False),
    }


def verify(ca_pem: str, cert_pem: str) -> None:
    with tempfile.TemporaryDirectory() as tmp:
        ca = Path(tmp, "ca.crt")
        crt = Path(tmp, "host.crt")
        ca.write_text(ca_pem)
        crt.write_text(cert_pem)
        run("verify", "-ca", str(ca), "-crt", str(crt))


def hours_until(not_after: str) -> int:
    end = datetime.fromisoformat(not_after)
    return int((end - datetime.now(timezone.utc)).total_seconds() // 3600)

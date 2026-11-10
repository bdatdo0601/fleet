"""SQLite registry of devices. Lives on the lighthouse only, owned by `fleet serve`."""

from __future__ import annotations

import ipaddress
import sqlite3
import threading
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from .paths import LIGHTHOUSE_IP, MESH

SCHEMA = """
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS devices (
  name        TEXT PRIMARY KEY,
  ip          TEXT NOT NULL UNIQUE,
  kind        TEXT NOT NULL CHECK (kind IN ('lighthouse', 'mac', 'mobile')),
  grp         TEXT NOT NULL,
  pubkey      TEXT NOT NULL,
  cert        TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  not_after   TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  revoked_at  TEXT,
  last_seen   TEXT
);
-- Every certificate ever replaced or revoked. Rendered into pki.blocklist everywhere.
CREATE TABLE IF NOT EXISTS revoked_certs (
  fingerprint TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  revoked_at  TEXT NOT NULL,
  reason      TEXT NOT NULL
);
"""


def now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat()


@dataclass(frozen=True)
class Device:
    name: str
    ip: str
    kind: str
    grp: str
    pubkey: str
    cert: str
    fingerprint: str
    not_after: str
    created_at: str
    revoked_at: str | None
    last_seen: str | None

    @property
    def active(self) -> bool:
        return self.revoked_at is None


class Registry:
    def __init__(self, path: Path):
        self.path = path
        self.lock = threading.RLock()
        self.conn = sqlite3.connect(str(path), check_same_thread=False, isolation_level=None)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA foreign_keys = ON")
        self.conn.executescript(SCHEMA)

    def close(self) -> None:
        self.conn.close()

    # settings -----------------------------------------------------------
    def setting(self, key: str) -> str | None:
        row = self.conn.execute("SELECT value FROM settings WHERE key = ?", (key,)).fetchone()
        return row["value"] if row else None

    def set_setting(self, key: str, value: str) -> None:
        with self.lock:
            self.conn.execute(
                "INSERT INTO settings(key, value) VALUES (?, ?) "
                "ON CONFLICT(key) DO UPDATE SET value = excluded.value",
                (key, value),
            )

    # devices ------------------------------------------------------------
    def _device(self, row: sqlite3.Row | None) -> Device | None:
        return Device(**dict(row)) if row else None

    def get(self, name: str) -> Device | None:
        return self._device(self.conn.execute("SELECT * FROM devices WHERE name = ?", (name,)).fetchone())

    def by_ip(self, ip: str) -> Device | None:
        return self._device(self.conn.execute("SELECT * FROM devices WHERE ip = ?", (ip,)).fetchone())

    def lighthouse(self) -> Device | None:
        return self._device(self.conn.execute("SELECT * FROM devices WHERE kind = 'lighthouse'").fetchone())

    def devices(self) -> list[Device]:
        rows = self.conn.execute("SELECT * FROM devices ORDER BY ip").fetchall()
        return sorted((Device(**dict(r)) for r in rows), key=lambda d: ipaddress.ip_address(d.ip))

    def next_ip(self) -> str:
        """Lowest free mesh address. Addresses of revoked devices are never reused."""
        used = {r["ip"] for r in self.conn.execute("SELECT ip FROM devices")}
        for host in MESH.hosts():
            ip = str(host)
            if ip != LIGHTHOUSE_IP and ip not in used:
                return ip
        raise RuntimeError("mesh address space exhausted")

    def add(self, *, name: str, ip: str, kind: str, grp: str, pubkey: str, cert: str,
            fingerprint: str, not_after: str) -> None:  # fmt: skip
        with self.lock:
            self.conn.execute(
                "INSERT INTO devices(name, ip, kind, grp, pubkey, cert, fingerprint, not_after, created_at) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (name, ip, kind, grp, pubkey, cert, fingerprint, not_after, now()),
            )

    def replace_cert(self, *, name: str, grp: str, pubkey: str, cert: str, fingerprint: str,
                     not_after: str, reason: str) -> None:  # fmt: skip
        """New cert for an existing device; the old one is blocklisted. Un-revokes the device."""
        with self.lock:
            old = self.get(name)
            assert old is not None
            self.conn.execute("BEGIN")
            try:
                self._blocklist(old.fingerprint, name, reason)
                self.conn.execute(
                    "UPDATE devices SET grp = ?, pubkey = ?, cert = ?, fingerprint = ?, not_after = ?, "
                    "revoked_at = NULL WHERE name = ?",
                    (grp, pubkey, cert, fingerprint, not_after, name),
                )
                self.conn.execute("COMMIT")
            except BaseException:
                self.conn.execute("ROLLBACK")
                raise

    def revoke(self, name: str) -> None:
        with self.lock:
            dev = self.get(name)
            assert dev is not None
            self.conn.execute("BEGIN")
            try:
                self._blocklist(dev.fingerprint, name, "revoked")
                self.conn.execute("UPDATE devices SET revoked_at = ? WHERE name = ?", (now(), name))
                self.conn.execute("COMMIT")
            except BaseException:
                self.conn.execute("ROLLBACK")
                raise

    def _blocklist(self, fingerprint: str, name: str, reason: str) -> None:
        self.conn.execute(
            "INSERT OR IGNORE INTO revoked_certs(fingerprint, name, revoked_at, reason) VALUES (?, ?, ?, ?)",
            (fingerprint, name, now(), reason),
        )

    def blocklist(self) -> list[str]:
        rows = self.conn.execute("SELECT fingerprint FROM revoked_certs ORDER BY fingerprint")
        return [r["fingerprint"] for r in rows]

    def touch(self, name: str) -> None:
        with self.lock:
            self.conn.execute("UPDATE devices SET last_seen = ? WHERE name = ?", (now(), name))

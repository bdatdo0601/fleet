"""SQLite agent session registry and cross-node message broker (`~/.fleet/agents.db`)."""

from __future__ import annotations

import json
import re
import sqlite3
import threading
import time
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

from .paths import AGENT_ACTIVE_SECONDS, AGENT_RETENTION_SECONDS, HARNESSES

SCHEMA = """
CREATE TABLE IF NOT EXISTS agent_sessions (
  session_id  TEXT PRIMARY KEY,
  device      TEXT NOT NULL,
  harness     TEXT NOT NULL,
  raw_id      TEXT NOT NULL,
  pid         INTEGER,
  cwd         TEXT NOT NULL DEFAULT '',
  repo        TEXT NOT NULL DEFAULT '',
  branch      TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL DEFAULT 'working',
  summary     TEXT NOT NULL DEFAULT '',
  git_status  TEXT NOT NULL DEFAULT '',
  tail_json   TEXT NOT NULL DEFAULT '[]',
  started_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_lookup
  ON agent_sessions(device, harness, cwd, updated_at);

CREATE TABLE IF NOT EXISTS agent_messages (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  thread_id    TEXT NOT NULL,
  reply_to     INTEGER,
  from_session TEXT NOT NULL,
  from_device  TEXT NOT NULL,
  to_device    TEXT,
  to_session   TEXT,
  to_repo      TEXT,
  body         TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_messages_inbox
  ON agent_messages(to_session, to_device, id);

CREATE TABLE IF NOT EXISTS agent_reads (
  session_id  TEXT NOT NULL,
  message_id  INTEGER NOT NULL,
  read_at     TEXT NOT NULL,
  PRIMARY KEY (session_id, message_id)
);
"""

VALID_STATES = ("working", "idle", "exited")
MAX_MESSAGE_BYTES = 32 * 1024


class AgentError(Exception):
    """Caller-level error with an HTTP status code."""

    def __init__(self, message: str, status: int = 400):
        super().__init__(message)
        self.status = status


def _now_dt() -> datetime:
    return datetime.now(timezone.utc).replace(microsecond=0)


def _now() -> str:
    return _now_dt().isoformat()


def normalize_raw_id(raw_id: str) -> str:
    cleaned = re.sub(r"[^a-zA-Z0-9_-]", "", raw_id)
    if not cleaned:
        raise AgentError("session raw_id must not be empty")
    return cleaned if cleaned.startswith("pid-") else cleaned[:12]


def make_session_id(device: str, harness: str, raw_id: str) -> tuple[str, str]:
    if harness not in HARNESSES:
        raise AgentError(f"unknown harness {harness!r}")
    short = normalize_raw_id(raw_id)
    return f"{device}:{harness}:{short}", short


def _row_to_message(row: sqlite3.Row) -> dict:
    return {
        "id": row["id"],
        "thread_id": row["thread_id"],
        "reply_to": row["reply_to"],
        "from_session": row["from_session"],
        "from_device": row["from_device"],
        "to_device": row["to_device"],
        "to_session": row["to_session"],
        "to_repo": row["to_repo"],
        "body": row["body"],
        "created_at": row["created_at"],
    }


def _row_to_session(row: sqlite3.Row, now_dt: datetime | None = None) -> dict:
    now_dt = now_dt or _now_dt()
    updated = datetime.fromisoformat(row["updated_at"])
    age = max(0, int((now_dt - updated).total_seconds()))
    live = row["state"] != "exited" and age <= AGENT_ACTIVE_SECONDS
    tail = json.loads(row["tail_json"] or "[]")
    return {
        "session_id": row["session_id"],
        "device": row["device"],
        "harness": row["harness"],
        "pid": row["pid"],
        "cwd": row["cwd"],
        "repo": row["repo"],
        "branch": row["branch"],
        "state": row["state"],
        "live": live,
        "age_seconds": age,
        "summary": row["summary"],
        "git_status": row["git_status"],
        "turn_count": len(tail),
        "started_at": row["started_at"],
        "updated_at": row["updated_at"],
    }


class AgentHub:
    def __init__(self, path: Path):
        self.path = path
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.lock = threading.RLock()
        self.cond = threading.Condition(self.lock)
        self._last_prune = 0.0
        self.conn = sqlite3.connect(str(path), check_same_thread=False, isolation_level=None)
        self.conn.row_factory = sqlite3.Row
        if path.exists():
            path.chmod(0o600)
        self.conn.execute("PRAGMA journal_mode = WAL")
        self.conn.execute("PRAGMA foreign_keys = ON")
        self.conn.executescript(SCHEMA)

    def close(self) -> None:
        with self.lock:
            self.conn.close()

    def prune(self, *, force: bool = False) -> None:
        now_mono = time.monotonic()
        if not force and now_mono - self._last_prune < 60.0:
            return
        self._last_prune = now_mono
        cutoff = (_now_dt() - timedelta(seconds=AGENT_RETENTION_SECONDS)).isoformat()
        with self.lock:
            self.conn.execute("BEGIN")
            try:
                self.conn.execute("DELETE FROM agent_sessions WHERE updated_at < ?", (cutoff,))
                self.conn.execute("DELETE FROM agent_messages WHERE created_at < ?", (cutoff,))
                self.conn.execute(
                    "DELETE FROM agent_reads WHERE read_at < ? "
                    "OR message_id NOT IN (SELECT id FROM agent_messages)",
                    (cutoff,),
                )
                self.conn.execute("COMMIT")
            except BaseException:
                self.conn.execute("ROLLBACK")
                raise

    def _promote_placeholder(self, *, device: str, harness: str, cwd: str, pid: int | None, target_id: str) -> tuple[int | None, str | None]:  # noqa: E501
        """If a stdio `pid-*` placeholder exists for this session, migrate its identity & messages."""
        if not cwd:
            return pid, None
        cutoff = (_now_dt() - timedelta(seconds=AGENT_ACTIVE_SECONDS)).isoformat()
        rows = self.conn.execute(
            "SELECT session_id, pid, started_at FROM agent_sessions "
            "WHERE device = ? AND harness = ? AND cwd = ? AND raw_id LIKE 'pid-%' "
            "AND state != 'exited' AND updated_at >= ? ORDER BY updated_at DESC",
            (device, harness, cwd, cutoff),
        ).fetchall()
        if not rows:
            return pid, None
        chosen = next((r for r in rows if pid is not None and r["pid"] == pid), rows[0])
        old_id = chosen["session_id"]
        if old_id == target_id:
            return pid, chosen["started_at"]
        self.conn.execute("UPDATE agent_messages SET to_session = ? WHERE to_session = ?", (target_id, old_id))
        self.conn.execute("UPDATE agent_messages SET from_session = ? WHERE from_session = ?", (target_id, old_id))
        self.conn.execute(
            "INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) "
            "SELECT ?, message_id, read_at FROM agent_reads WHERE session_id = ?",
            (target_id, old_id),
        )
        self.conn.execute("DELETE FROM agent_reads WHERE session_id = ?", (old_id,))
        self.conn.execute("DELETE FROM agent_sessions WHERE session_id = ?", (old_id,))
        return pid if pid is not None else chosen["pid"], chosen["started_at"]

    def heartbeat(
        self,
        *,
        device: str,
        harness: str,
        raw_id: str,
        pid: int | None = None,
        cwd: str = "",
        repo: str = "",
        branch: str = "",
        state: str = "working",
        summary: str | None = None,
        git_status: str | None = None,
        tail: list[dict] | None = None,
        drain_inbox: bool = False,
    ) -> dict:
        if state not in VALID_STATES:
            raise AgentError(f"invalid state {state!r}")
        session_id, short_id = make_session_id(device, harness, raw_id)
        now_iso = _now()
        with self.lock:
            self.prune()
            self.conn.execute("BEGIN")
            try:
                old_pid, old_started = (
                    self._promote_placeholder(device=device, harness=harness, cwd=cwd, pid=pid, target_id=session_id)
                    if not short_id.startswith("pid-")
                    else (pid, None)
                )
                existing = self.conn.execute(
                    "SELECT pid, cwd, repo, branch, summary, git_status, tail_json, started_at "
                    "FROM agent_sessions WHERE session_id = ?",
                    (session_id,),
                ).fetchone()
                if existing:
                    self.conn.execute(
                        "UPDATE agent_sessions SET pid = ?, cwd = ?, repo = ?, branch = ?, state = ?, "
                        "summary = ?, git_status = ?, tail_json = ?, started_at = ?, updated_at = ? "
                        "WHERE session_id = ?",
                        (
                            old_pid if old_pid is not None else existing["pid"],
                            cwd or existing["cwd"],
                            repo or existing["repo"],
                            branch or existing["branch"],
                            state,
                            existing["summary"] if summary is None else summary[:160],
                            existing["git_status"] if git_status is None else git_status[:1500],
                            existing["tail_json"] if tail is None else json.dumps(tail[-25:]),
                            old_started or existing["started_at"],
                            now_iso,
                            session_id,
                        ),
                    )
                else:
                    self.conn.execute(
                        "INSERT INTO agent_sessions(session_id, device, harness, raw_id, pid, cwd, repo, branch, "
                        "state, summary, git_status, tail_json, started_at, updated_at) "
                        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                        (
                            session_id,
                            device,
                            harness,
                            short_id,
                            old_pid,
                            cwd,
                            repo,
                            branch,
                            state,
                            (summary or "")[:160],
                            (git_status or "")[:1500],
                            json.dumps((tail or [])[-25:]),
                            old_started or now_iso,
                            now_iso,
                        ),
                    )
                self.conn.execute("COMMIT")
            except BaseException:
                self.conn.execute("ROLLBACK")
                raise
            unread = (
                self.fetch_inbox(device=device, session_id=session_id, unread_only=True, mark_read=True)
                if drain_inbox
                else []
            )
        return {"session_id": session_id, "unread": unread}

    def deregister(
        self,
        *,
        device: str,
        session_id: str | None = None,
        harness: str | None = None,
        pid: int | None = None,
    ) -> dict:
        if session_id is not None and not session_id.startswith(f"{device}:"):
            raise AgentError("session belongs to another device", 403)
        now_iso = _now()
        with self.lock:
            if session_id is not None:
                self.conn.execute(
                    "UPDATE agent_sessions SET state = 'exited', updated_at = ? WHERE device = ? AND session_id = ?",
                    (now_iso, device, session_id),
                )
            elif harness is not None and pid is not None:
                self.conn.execute(
                    "UPDATE agent_sessions SET state = 'exited', updated_at = ? "
                    "WHERE device = ? AND harness = ? AND pid = ?",
                    (now_iso, device, harness, pid),
                )
            else:
                raise AgentError("session_id or (harness, pid) is required")
        return {"ok": True}

    def resolve_caller_session(
        self,
        *,
        device: str,
        harness: str = "claude",
        session_id: str | None = None,
        cwd: str = "",
        pid: int | None = None,
    ) -> str:
        if session_id:
            if ":" in session_id:
                if not session_id.startswith(f"{device}:"):
                    raise AgentError("session belongs to another device", 403)
                return session_id
            resolved, _ = make_session_id(device, harness, session_id)
            return resolved
        if harness not in HARNESSES:
            raise AgentError(f"unknown harness {harness!r}")
        with self.lock:
            rows = self.conn.execute(
                "SELECT session_id, pid, cwd FROM agent_sessions "
                "WHERE device = ? AND harness = ? AND state != 'exited' ORDER BY updated_at DESC",
                (device, harness),
            ).fetchall()
            if pid is not None:
                for r in rows:
                    if r["pid"] == pid:
                        return r["session_id"]
            if cwd:
                for r in rows:
                    if r["cwd"] == cwd:
                        return r["session_id"]
            if rows:
                return rows[0]["session_id"]
            raw = f"pid-{pid}" if pid is not None else "default"
            return self.heartbeat(device=device, harness=harness, raw_id=raw, pid=pid, cwd=cwd)["session_id"]

    def list_sessions(
        self,
        *,
        device: str | None = None,
        repo: str | None = None,
        harness: str | None = None,
        include_stale: bool = False,
    ) -> list[dict]:
        with self.lock:
            rows = self.conn.execute(
                "SELECT * FROM agent_sessions "
                "WHERE (? IS NULL OR device = ?) AND (? IS NULL OR repo = ?) AND (? IS NULL OR harness = ?) "
                "ORDER BY updated_at DESC",
                (device, device, repo, repo, harness, harness),
            ).fetchall()
        now_dt = _now_dt()
        sessions = [_row_to_session(r, now_dt) for r in rows]
        return sessions if include_stale else [s for s in sessions if s["live"]]

    def get_session(self, target: str) -> dict:
        target = target.strip()
        if not target:
            raise AgentError("session target is required")
        with self.lock:
            exact = self.conn.execute("SELECT * FROM agent_sessions WHERE session_id = ?", (target,)).fetchone()
            if exact:
                info = _row_to_session(exact)
                info["tail"] = json.loads(exact["tail_json"] or "[]")
                return info
            rows = self.conn.execute("SELECT * FROM agent_sessions ORDER BY updated_at DESC").fetchall()
        suffix_matches = [r for r in rows if r["session_id"].endswith(f":{target}")]
        matches = suffix_matches or [r for r in rows if target in r["session_id"]]
        if not matches:
            raise AgentError(f"no agent session matching {target!r}", 404)
        if len(matches) > 1:
            ids = ", ".join(r["session_id"] for r in matches[:5])
            raise AgentError(f"ambiguous session {target!r}: {ids}", 400)
        row = matches[0]
        info = _row_to_session(row)
        info["tail"] = json.loads(row["tail_json"] or "[]")
        return info

    def send_message(
        self,
        *,
        device: str,
        from_session: str,
        to: str,
        body: str,
        thread_id: str | None = None,
        reply_to: int | None = None,
        known_devices: set[str] | frozenset[str] = frozenset(),
    ) -> dict:
        if not from_session.startswith(f"{device}:"):
            raise AgentError("session belongs to another device", 403)
        text = body.strip()
        if not text:
            raise AgentError("message body is required")
        if len(text.encode("utf-8")) > MAX_MESSAGE_BYTES:
            raise AgentError("message too large (max 32 KB)", 413)

        target = to.strip()
        if not target:
            raise AgentError("recipient `to` is required")
        to_device: str | None = None
        to_session: str | None = None
        to_repo: str | None = None
        if target in ("*", "all"):
            pass
        elif target.startswith("repo:"):
            to_repo = target[5:].strip()
            if not to_repo:
                raise AgentError("repo target cannot be empty")
        elif target.startswith("device:"):
            to_device = target[7:].strip()
            if not to_device:
                raise AgentError("device target cannot be empty")
        elif ":" not in target and target in known_devices:
            to_device = target
        else:
            resolved = self.get_session(target)
            to_session = resolved["session_id"]
            to_device = resolved["device"]

        now_iso = _now()
        with self.cond:
            resolved_thread = thread_id
            if not resolved_thread and reply_to is not None:
                parent = self.conn.execute("SELECT thread_id FROM agent_messages WHERE id = ?", (reply_to,)).fetchone()
                if parent:
                    resolved_thread = parent["thread_id"]
            resolved_thread = resolved_thread or uuid.uuid4().hex[:12]
            cur = self.conn.execute(
                "INSERT INTO agent_messages(thread_id, reply_to, from_session, from_device, "
                "to_device, to_session, to_repo, body, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (resolved_thread, reply_to, from_session, device, to_device, to_session, to_repo, text, now_iso),
            )
            row = self.conn.execute("SELECT * FROM agent_messages WHERE id = ?", (cur.lastrowid,)).fetchone()
            assert row is not None
            msg = _row_to_message(row)
            self.cond.notify_all()
            return msg

    def fetch_inbox(
        self,
        *,
        device: str,
        session_id: str,
        unread_only: bool = True,
        mark_read: bool = True,
        limit: int = 50,
    ) -> list[dict]:
        if not session_id.startswith(f"{device}:"):
            raise AgentError("session belongs to another device", 403)
        limit = max(1, min(int(limit), 200))
        with self.lock:
            sess = self.conn.execute(
                "SELECT repo, started_at FROM agent_sessions WHERE session_id = ?",
                (session_id,),
            ).fetchone()
            repo = sess["repo"] if sess else ""
            started_at = sess["started_at"] if sess else "1970-01-01T00:00:00+00:00"
            rows = self.conn.execute(
                "SELECT m.* FROM agent_messages m "
                "WHERE m.from_session != ? "
                "AND (m.to_session = ? OR (m.to_session IS NULL "
                "  AND (m.to_device IS NULL OR m.to_device = ?) "
                "  AND (m.to_repo IS NULL OR (m.to_repo = ? AND ? != '')) "
                "  AND m.created_at >= ?)) "
                "AND (? = 0 OR m.id NOT IN (SELECT message_id FROM agent_reads WHERE session_id = ?)) "
                "ORDER BY m.id ASC LIMIT ?",
                (
                    session_id,
                    session_id,
                    device,
                    repo,
                    repo,
                    started_at,
                    1 if unread_only else 0,
                    session_id,
                    limit,
                ),
            ).fetchall()
            messages = [_row_to_message(r) for r in rows]
            if mark_read and messages:
                read_at = _now()
                self.conn.execute("BEGIN")
                try:
                    for m in messages:
                        self.conn.execute(
                            "INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) VALUES (?, ?, ?)",
                            (session_id, m["id"], read_at),
                        )
                    self.conn.execute("COMMIT")
                except BaseException:
                    self.conn.execute("ROLLBACK")
                    raise
            return messages

    def ask(
        self,
        *,
        device: str,
        from_session: str,
        to: str,
        body: str,
        timeout: float = 30.0,
        known_devices: set[str] | frozenset[str] = frozenset(),
    ) -> dict:
        timeout = max(1.0, min(float(timeout), 120.0))
        sent = self.send_message(
            device=device,
            from_session=from_session,
            to=to,
            body=body,
            known_devices=known_devices,
        )
        deadline = time.monotonic() + timeout
        with self.cond:
            while True:
                row = self.conn.execute(
                    "SELECT * FROM agent_messages "
                    "WHERE thread_id = ? AND id > ? AND from_session != ? "
                    "AND (to_session = ? OR (to_session IS NULL AND (to_device IS NULL OR to_device = ?))) "
                    "ORDER BY id ASC LIMIT 1",
                    (sent["thread_id"], sent["id"], from_session, from_session, device),
                ).fetchone()
                if row is not None:
                    self.conn.execute(
                        "INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) VALUES (?, ?, ?)",
                        (from_session, row["id"], _now()),
                    )
                    return {"sent": sent, "reply": _row_to_message(row), "timed_out": False}
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return {"sent": sent, "reply": None, "timed_out": True}
                self.cond.wait(timeout=remaining)

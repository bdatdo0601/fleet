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

from .paths import AGENT_ACTIVE_SECONDS, AGENT_RETENTION_SECONDS, AGENT_WAITING_ACTIVE_SECONDS, HARNESSES

SCHEMA = """
CREATE TABLE IF NOT EXISTS agent_sessions (
  session_id          TEXT PRIMARY KEY,
  device              TEXT NOT NULL,
  harness             TEXT NOT NULL,
  raw_id              TEXT NOT NULL,
  full_raw_id         TEXT NOT NULL DEFAULT '',
  pid                 INTEGER,
  cwd                 TEXT NOT NULL DEFAULT '',
  repo                TEXT NOT NULL DEFAULT '',
  branch              TEXT NOT NULL DEFAULT '',
  state               TEXT NOT NULL DEFAULT 'working',
  state_changed_at    TEXT NOT NULL DEFAULT '',
  pending_action_json TEXT NOT NULL DEFAULT '',
  title               TEXT NOT NULL DEFAULT '',
  name                TEXT NOT NULL DEFAULT '',
  custom_name         INTEGER NOT NULL DEFAULT 0,
  summary             TEXT NOT NULL DEFAULT '',
  git_status          TEXT NOT NULL DEFAULT '',
  tail_json           TEXT NOT NULL DEFAULT '[]',
  started_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
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

SESSION_MIGRATIONS = (
    ("full_raw_id", "TEXT NOT NULL DEFAULT ''"),
    ("state_changed_at", "TEXT NOT NULL DEFAULT ''"),
    ("pending_action_json", "TEXT NOT NULL DEFAULT ''"),
    ("title", "TEXT NOT NULL DEFAULT ''"),
    ("name", "TEXT NOT NULL DEFAULT ''"),
    ("custom_name", "INTEGER NOT NULL DEFAULT 0"),
)

WAITING_STATES = ("waiting_permission", "waiting_question")
VALID_STATES = ("working", "waiting_permission", "waiting_question", "idle", "exited")
VALID_STATE_FILTERS = (*VALID_STATES, "waiting")
MAX_MESSAGE_BYTES = 32 * 1024
WAITER_REARM_GRACE_SECONDS = 20.0


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


def slugify_name(text: str, *, max_words: int = 5, max_len: int = 32) -> str:
    tokens = re.findall(r"[a-z0-9]+", text.lower())
    if not tokens:
        return ""
    return "-".join(tokens[:max_words])[:max_len].rstrip("-")


def _is_live_row(state: str, age: int) -> bool:
    if state == "exited":
        return False
    limit = AGENT_WAITING_ACTIVE_SECONDS if state in WAITING_STATES else AGENT_ACTIVE_SECONDS
    return age <= limit


def _normalize_pending_action(action: dict | None) -> dict | None:
    if not isinstance(action, dict):
        return None
    limits = {"kind": 32, "tool": 120, "detail": 1000, "event": 64}
    cleaned: dict[str, str] = {}
    for key, max_len in limits.items():
        val = action.get(key)
        if isinstance(val, str):
            text = val.strip()[:max_len]
            if text:
                cleaned[key] = text
    return cleaned or None


def _parse_pending_action(raw: str | None) -> dict | None:
    if not raw:
        return None
    try:
        obj = json.loads(raw)
    except ValueError:
        return None
    return obj if isinstance(obj, dict) and obj else None


def delivery_mode(*, live: bool, state: str, listening: bool) -> str:
    if not live or state == "exited":
        return "offline"
    if state in WAITING_STATES:
        return "waiting_user"
    if listening:
        return "immediate"
    if state == "working":
        return "next_hook"
    return "queued_idle"


def _row_to_message(row: sqlite3.Row) -> dict:
    keys = row.keys()
    return {
        "id": row["id"],
        "thread_id": row["thread_id"],
        "reply_to": row["reply_to"],
        "from_session": row["from_session"],
        "from_device": row["from_device"],
        "from_name": (row["from_name"] or "") if "from_name" in keys else "",
        "from_title": (row["from_title"] or "") if "from_title" in keys else "",
        "to_device": row["to_device"],
        "to_session": row["to_session"],
        "to_repo": row["to_repo"],
        "body": row["body"],
        "created_at": row["created_at"],
    }


def _row_to_session(row: sqlite3.Row, now_dt: datetime | None = None, *, listening: bool = False) -> dict:
    now_dt = now_dt or _now_dt()
    updated = datetime.fromisoformat(row["updated_at"])
    age = max(0, int((now_dt - updated).total_seconds()))
    state = row["state"]
    live = _is_live_row(state, age)
    tail = json.loads(row["tail_json"] or "[]")
    keys = row.keys()
    full_raw_id = (row["full_raw_id"] or "") if "full_raw_id" in keys else ""
    title = (row["title"] or "") if "title" in keys else ""
    name = (row["name"] or "") if "name" in keys else ""
    state_changed_at = (
        (row["state_changed_at"] or row["updated_at"]) if "state_changed_at" in keys else row["updated_at"]
    )
    pending_action = (
        _parse_pending_action(row["pending_action_json"])
        if ("pending_action_json" in keys and state in WAITING_STATES)
        else None
    )
    waiting_seconds = (
        max(0, int((now_dt - datetime.fromisoformat(state_changed_at)).total_seconds()))
        if state in WAITING_STATES
        else None
    )
    is_listening = bool(listening and live and state != "exited")
    return {
        "session_id": row["session_id"],
        "device": row["device"],
        "harness": row["harness"],
        "full_raw_id": full_raw_id,
        "pid": row["pid"],
        "cwd": row["cwd"],
        "repo": row["repo"],
        "branch": row["branch"],
        "state": state,
        "state_changed_at": state_changed_at,
        "waiting_seconds": waiting_seconds,
        "pending_action": pending_action,
        "live": live,
        "listening": is_listening,
        "delivery": delivery_mode(live=live, state=state, listening=is_listening),
        "age_seconds": age,
        "title": title,
        "name": name,
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
        self._waiters: dict[str, int] = {}
        self._wait_seen_at: dict[str, float] = {}
        self._promoted_to: dict[str, str] = {}
        self._closed = False
        self.conn = sqlite3.connect(str(path), check_same_thread=False, isolation_level=None)
        self.conn.row_factory = sqlite3.Row
        if path.exists():
            path.chmod(0o600)
        self.conn.execute("PRAGMA journal_mode = WAL")
        self.conn.execute("PRAGMA foreign_keys = ON")
        self.conn.executescript(SCHEMA)
        existing_cols = {r["name"] for r in self.conn.execute("PRAGMA table_info(agent_sessions)").fetchall()}
        for col, col_def in SESSION_MIGRATIONS:
            if col not in existing_cols:
                self.conn.execute(f"ALTER TABLE agent_sessions ADD COLUMN {col} {col_def}")

    def close(self) -> None:
        with self.cond:
            self._closed = True
            self.cond.notify_all()
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

    def _transfer_waiter(self, old_id: str, target_id: str) -> None:
        if old_id == target_id:
            return
        if old_id in self._waiters:
            self._waiters[target_id] = self._waiters.get(target_id, 0) + self._waiters.pop(old_id)
        if old_id in self._wait_seen_at:
            self._wait_seen_at[target_id] = max(
                self._wait_seen_at.get(target_id, 0.0),
                self._wait_seen_at.pop(old_id),
            )
        self._promoted_to[old_id] = target_id

    def _migrate_session_identity(self, *, old_id: str, target_id: str) -> None:
        if old_id == target_id:
            return
        self.conn.execute("UPDATE agent_messages SET to_session = ? WHERE to_session = ?", (target_id, old_id))
        self.conn.execute("UPDATE agent_messages SET from_session = ? WHERE from_session = ?", (target_id, old_id))
        self.conn.execute(
            "INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) "
            "SELECT ?, message_id, read_at FROM agent_reads WHERE session_id = ?",
            (target_id, old_id),
        )
        self.conn.execute("DELETE FROM agent_reads WHERE session_id = ?", (old_id,))
        self.conn.execute("DELETE FROM agent_sessions WHERE session_id = ?", (old_id,))
        self._transfer_waiter(old_id, target_id)

    def _is_listening(self, session_id: str, state: str, live: bool, now_mono: float | None = None) -> bool:
        if not live or state == "exited":
            return False
        if self._waiters.get(session_id, 0) > 0:
            return True
        now_mono = time.monotonic() if now_mono is None else now_mono
        return (
            state == "working"
            and (now_mono - self._wait_seen_at.get(session_id, -1e9)) <= WAITER_REARM_GRACE_SECONDS
        )

    def _serialize_session(
        self,
        row: sqlite3.Row,
        now_dt: datetime | None = None,
        now_mono: float | None = None,
    ) -> dict:
        now_dt = now_dt or _now_dt()
        updated = datetime.fromisoformat(row["updated_at"])
        age = max(0, int((now_dt - updated).total_seconds()))
        live = _is_live_row(row["state"], age)
        listening = self._is_listening(row["session_id"], row["state"], live, now_mono=now_mono)
        return _row_to_session(row, now_dt, listening=listening)

    def _unique_auto_name(self, base: str, *, device: str, exclude_sid: str) -> str:
        if not base:
            return ""
        now_dt = _now_dt()
        cutoff = (now_dt - timedelta(seconds=AGENT_ACTIVE_SECONDS)).isoformat()
        waiting_cutoff = (now_dt - timedelta(seconds=AGENT_WAITING_ACTIVE_SECONDS)).isoformat()
        rows = self.conn.execute(
            "SELECT LOWER(name) AS lname FROM agent_sessions "
            "WHERE device = ? AND state != 'exited' "
            "AND (updated_at >= ? OR (state IN ('waiting_permission', 'waiting_question') AND updated_at >= ?)) "
            "AND session_id != ? AND name != ''",
            (device, cutoff, waiting_cutoff, exclude_sid),
        ).fetchall()
        taken = {r["lname"] for r in rows}
        if base.lower() not in taken:
            return base
        n = 2
        while True:
            suffix = f"-{n}"
            candidate = f"{base[:max(1, 32 - len(suffix))].rstrip('-')}{suffix}"
            if candidate.lower() not in taken:
                return candidate
            n += 1

    def _resolve_title_and_name(
        self,
        *,
        device: str,
        session_id: str,
        repo: str,
        title: str | None,
        name: str | None,
        explicit_name: bool,
        base_title: str,
        base_name: str,
        base_custom: int,
    ) -> tuple[str, str, int]:
        eff_title = title.strip()[:160] if isinstance(title, str) and title.strip() else base_title
        if base_custom:
            return eff_title, base_name, 1
        if isinstance(name, str) and name.strip():
            slug = slugify_name(name)
            if slug:
                eff_name = (
                    slug
                    if explicit_name
                    else self._unique_auto_name(slug, device=device, exclude_sid=session_id)
                )
                return eff_title, eff_name, 0
        if eff_title and (eff_title != base_title or not base_name):
            slug = slugify_name(eff_title)
            if slug:
                return eff_title, self._unique_auto_name(slug, device=device, exclude_sid=session_id), 0
        if not base_name and repo:
            slug = slugify_name(repo)
            if slug:
                return eff_title, self._unique_auto_name(slug, device=device, exclude_sid=session_id), 0
        return eff_title, base_name, 0

    def _promote_placeholder(
        self,
        *,
        device: str,
        harness: str,
        cwd: str,
        pid: int | None,
        target_id: str,
    ) -> tuple[int | None, str | None, str, str, int]:
        """If a stdio `pid-*` placeholder exists for this session, migrate its identity & messages."""
        if not cwd:
            return pid, None, "", "", 0
        cutoff = (_now_dt() - timedelta(seconds=AGENT_ACTIVE_SECONDS)).isoformat()
        rows = self.conn.execute(
            "SELECT session_id, pid, started_at, title, name, custom_name FROM agent_sessions "
            "WHERE device = ? AND harness = ? AND cwd = ? AND raw_id LIKE 'pid-%' "
            "AND state != 'exited' AND updated_at >= ? ORDER BY updated_at DESC",
            (device, harness, cwd, cutoff),
        ).fetchall()
        if not rows:
            return pid, None, "", "", 0
        chosen = next((r for r in rows if pid is not None and r["pid"] == pid), rows[0])
        old_id = chosen["session_id"]
        self._migrate_session_identity(old_id=old_id, target_id=target_id)
        return (
            pid if pid is not None else chosen["pid"],
            chosen["started_at"],
            chosen["title"] or "",
            chosen["name"] or "",
            int(chosen["custom_name"] or 0),
        )

    def _find_promoted_session(self, *, device: str, harness: str, pid: int, cwd: str) -> sqlite3.Row | None:
        now_dt = _now_dt()
        cutoff = (now_dt - timedelta(seconds=AGENT_ACTIVE_SECONDS)).isoformat()
        waiting_cutoff = (now_dt - timedelta(seconds=AGENT_WAITING_ACTIVE_SECONDS)).isoformat()
        by_pid = self.conn.execute(
            "SELECT * FROM agent_sessions "
            "WHERE device = ? AND harness = ? AND pid = ? AND raw_id NOT LIKE 'pid-%' "
            "AND state != 'exited' "
            "AND (updated_at >= ? OR (state IN ('waiting_permission', 'waiting_question') AND updated_at >= ?)) "
            "ORDER BY updated_at DESC LIMIT 1",
            (device, harness, pid, cutoff, waiting_cutoff),
        ).fetchone()
        if by_pid is not None:
            return by_pid
        if cwd:
            by_cwd = self.conn.execute(
                "SELECT * FROM agent_sessions "
                "WHERE device = ? AND harness = ? AND cwd = ? AND pid IS NULL AND raw_id NOT LIKE 'pid-%' "
                "AND state != 'exited' "
                "AND (updated_at >= ? OR (state IN ('waiting_permission', 'waiting_question') AND updated_at >= ?)) "
                "ORDER BY updated_at DESC",
                (device, harness, cwd, cutoff, waiting_cutoff),
            ).fetchall()
            if len(by_cwd) == 1:
                return by_cwd[0]
        return None

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
        pending_action: dict | None = None,
        title: str | None = None,
        name: str | None = None,
        explicit_name: bool = False,
        summary: str | None = None,
        git_status: str | None = None,
        tail: list[dict] | None = None,
        drain_inbox: bool = False,
    ) -> dict:
        if state not in VALID_STATES:
            raise AgentError(f"invalid state {state!r}")
        session_id, short_id = make_session_id(device, harness, raw_id)
        raw_clean = raw_id.strip()[:128]
        now_iso = _now()
        with self.cond:
            self.prune()
            self.conn.execute("BEGIN")
            try:
                if short_id.startswith("pid-") and pid is not None:
                    promoted = self._find_promoted_session(device=device, harness=harness, pid=pid, cwd=cwd)
                    if promoted is not None:
                        target_id = promoted["session_id"]
                        self._migrate_session_identity(old_id=session_id, target_id=target_id)
                        eff_title, eff_name, eff_custom = self._resolve_title_and_name(
                            device=device,
                            session_id=target_id,
                            repo=repo or promoted["repo"],
                            title=title,
                            name=name,
                            explicit_name=explicit_name,
                            base_title=promoted["title"] or "",
                            base_name=promoted["name"] or "",
                            base_custom=int(promoted["custom_name"] or 0),
                        )
                        self.conn.execute(
                            "UPDATE agent_sessions SET pid = ?, cwd = ?, repo = ?, branch = ?, "
                            "title = ?, name = ?, custom_name = ?, summary = ?, git_status = ?, "
                            "tail_json = ?, updated_at = ? WHERE session_id = ?",
                            (
                                promoted["pid"] if promoted["pid"] is not None else pid,
                                cwd or promoted["cwd"],
                                repo or promoted["repo"],
                                branch or promoted["branch"],
                                eff_title,
                                eff_name,
                                eff_custom,
                                promoted["summary"] if summary is None else summary[:160],
                                promoted["git_status"] if git_status is None else git_status[:1500],
                                promoted["tail_json"] if tail is None else json.dumps(tail[-25:]),
                                now_iso,
                                target_id,
                            ),
                        )
                        self.conn.execute("COMMIT")
                        self.cond.notify_all()
                        unread = (
                            self.fetch_inbox(device=device, session_id=target_id, unread_only=True, mark_read=True)
                            if drain_inbox
                            else []
                        )
                        return {
                            "session_id": target_id,
                            "full_raw_id": promoted["full_raw_id"] or "",
                            "name": eff_name,
                            "title": eff_title,
                            "unread": unread,
                        }

                if not short_id.startswith("pid-"):
                    old_pid, old_started, old_title, old_name, old_custom = self._promote_placeholder(
                        device=device, harness=harness, cwd=cwd, pid=pid, target_id=session_id
                    )
                else:
                    old_pid, old_started, old_title, old_name, old_custom = pid, None, "", "", 0

                existing = self.conn.execute(
                    "SELECT full_raw_id, pid, cwd, repo, branch, state, state_changed_at, pending_action_json, "
                    "title, name, custom_name, summary, git_status, tail_json, started_at, updated_at "
                    "FROM agent_sessions WHERE session_id = ?",
                    (session_id,),
                ).fetchone()

                base_title = (existing["title"] if existing else "") or old_title
                if existing and existing["custom_name"]:
                    base_name = existing["name"] or ""
                    base_custom = 1
                elif old_custom:
                    base_name = old_name
                    base_custom = 1
                else:
                    base_name = (existing["name"] if existing else "") or old_name
                    base_custom = 0

                eff_repo = repo or (existing["repo"] if existing else "")
                eff_title, eff_name, eff_custom = self._resolve_title_and_name(
                    device=device,
                    session_id=session_id,
                    repo=eff_repo,
                    title=title,
                    name=name,
                    explicit_name=explicit_name,
                    base_title=base_title,
                    base_name=base_name,
                    base_custom=base_custom,
                )
                full_raw = raw_clean if not short_id.startswith("pid-") else (
                    existing["full_raw_id"] if existing else ""
                )

                if state in WAITING_STATES:
                    norm_action = _normalize_pending_action(pending_action)
                    existing_same_state = existing is not None and existing["state"] == state
                    existing_pending = (
                        _parse_pending_action(existing["pending_action_json"]) if existing_same_state else None
                    )
                    if norm_action is None:
                        new_pending_json = (existing["pending_action_json"] or "") if existing_same_state else ""
                    elif (
                        existing_same_state
                        and existing_pending
                        and existing_pending.get("tool")
                        and not norm_action.get("tool")
                    ):
                        new_pending_json = existing["pending_action_json"] or ""
                    else:
                        new_pending_json = json.dumps(norm_action)
                else:
                    new_pending_json = ""

                if (
                    existing is not None
                    and existing["state"] == state
                    and (state not in WAITING_STATES or new_pending_json == (existing["pending_action_json"] or ""))
                ):
                    new_state_changed_at = existing["state_changed_at"] or existing["updated_at"]
                else:
                    new_state_changed_at = now_iso

                if existing:
                    self.conn.execute(
                        "UPDATE agent_sessions SET full_raw_id = ?, pid = ?, cwd = ?, repo = ?, branch = ?, "
                        "state = ?, state_changed_at = ?, pending_action_json = ?, title = ?, name = ?, "
                        "custom_name = ?, summary = ?, git_status = ?, tail_json = ?, started_at = ?, "
                        "updated_at = ? WHERE session_id = ?",
                        (
                            full_raw or existing["full_raw_id"],
                            old_pid if old_pid is not None else existing["pid"],
                            cwd or existing["cwd"],
                            eff_repo,
                            branch or existing["branch"],
                            state,
                            new_state_changed_at,
                            new_pending_json,
                            eff_title,
                            eff_name,
                            eff_custom,
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
                        "INSERT INTO agent_sessions(session_id, device, harness, raw_id, full_raw_id, pid, cwd, repo, "
                        "branch, state, state_changed_at, pending_action_json, title, name, custom_name, summary, "
                        "git_status, tail_json, started_at, updated_at) "
                        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                        (
                            session_id,
                            device,
                            harness,
                            short_id,
                            full_raw,
                            old_pid,
                            cwd,
                            repo,
                            branch,
                            state,
                            new_state_changed_at,
                            new_pending_json,
                            eff_title,
                            eff_name,
                            eff_custom,
                            (summary or "")[:160],
                            (git_status or "")[:1500],
                            json.dumps((tail or [])[-25:]),
                            old_started or now_iso,
                            now_iso,
                        ),
                    )
                self.conn.execute("COMMIT")
                self.cond.notify_all()
            except BaseException:
                self.conn.execute("ROLLBACK")
                raise
            unread = (
                self.fetch_inbox(device=device, session_id=session_id, unread_only=True, mark_read=True)
                if drain_inbox
                else []
            )
        return {
            "session_id": session_id,
            "full_raw_id": full_raw,
            "name": eff_name,
            "title": eff_title,
            "unread": unread,
        }

    def rename_session(self, *, device: str, session_id: str, new_name: str) -> dict:
        if not session_id.startswith(f"{device}:"):
            raise AgentError("session belongs to another device", 403)
        raw = new_name.strip().lower()
        if raw.startswith(("repo:", "device:")):
            raise AgentError("invalid session name")
        slug = slugify_name(new_name, max_words=6, max_len=40)
        if not slug or slug in ("all",):
            raise AgentError("session name must contain letters or digits and cannot be reserved")
        now_iso = _now()
        with self.lock:
            cur = self.conn.execute(
                "UPDATE agent_sessions SET name = ?, custom_name = 1, updated_at = ? "
                "WHERE device = ? AND session_id = ?",
                (slug, now_iso, device, session_id),
            )
            if cur.rowcount == 0:
                raise AgentError(f"no agent session {session_id!r}", 404)
        return {"session_id": session_id, "name": slug}

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
        with self.cond:
            if session_id is not None:
                self.conn.execute(
                    "UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, "
                    "pending_action_json = '', updated_at = ? WHERE device = ? AND session_id = ?",
                    (now_iso, now_iso, device, session_id),
                )
            elif harness is not None and pid is not None:
                self.conn.execute(
                    "UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, "
                    "pending_action_json = '', updated_at = ? WHERE device = ? AND harness = ? AND pid = ?",
                    (now_iso, now_iso, device, harness, pid),
                )
            else:
                raise AgentError("session_id or (harness, pid) is required")
            self.cond.notify_all()
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
                with self.lock:
                    cur = session_id
                    while cur in self._promoted_to:
                        cur = self._promoted_to[cur]
                    return cur
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
        state: str | None = None,
        include_stale: bool = False,
    ) -> list[dict]:
        if state is not None and state not in VALID_STATE_FILTERS:
            raise AgentError(f"invalid state filter {state!r}")
        with self.lock:
            rows = self.conn.execute(
                "SELECT * FROM agent_sessions "
                "WHERE (? IS NULL OR device = ?) AND (? IS NULL OR repo = ?) AND (? IS NULL OR harness = ?) "
                "ORDER BY updated_at DESC",
                (device, device, repo, repo, harness, harness),
            ).fetchall()
            now_dt = _now_dt()
            now_mono = time.monotonic()
            sessions = [self._serialize_session(r, now_dt=now_dt, now_mono=now_mono) for r in rows]
        filtered = sessions if include_stale else [s for s in sessions if s["live"]]
        if state is not None:
            if state == "waiting":
                filtered = [s for s in filtered if s["state"] in WAITING_STATES]
            else:
                filtered = [s for s in filtered if s["state"] == state]
        return filtered

    def get_session(self, target: str) -> dict:
        target = target.strip()
        if not target:
            raise AgentError("session target is required")
        with self.lock:
            rows = self.conn.execute("SELECT * FROM agent_sessions ORDER BY updated_at DESC").fetchall()
            now_dt = _now_dt()
            now_mono = time.monotonic()

            def is_live(r: sqlite3.Row) -> bool:
                updated = datetime.fromisoformat(r["updated_at"])
                age = max(0, int((now_dt - updated).total_seconds()))
                return _is_live_row(r["state"], age)

            def pick(matches: list[sqlite3.Row]) -> dict | None:
                if not matches:
                    return None
                live_matches = [r for r in matches if is_live(r)]
                pool = live_matches or matches
                if len(pool) == 1:
                    info = self._serialize_session(pool[0], now_dt=now_dt, now_mono=now_mono)
                    info["tail"] = json.loads(pool[0]["tail_json"] or "[]")
                    return info
                cands = ", ".join(
                    f"{r['name']} ({r['session_id']})" if r["name"] else r["session_id"] for r in pool[:5]
                )
                raise AgentError(f"ambiguous session {target!r}: {cands}", 400)

            target_lower = target.lower()
            tiers: list[list[sqlite3.Row]] = [
                [r for r in rows if r["session_id"] == target],
            ]
            if "/" in target and ":" not in target:
                dev_part, key_part = target.split("/", 1)
                key_lower = key_part.lower()
                dev_rows = [r for r in rows if r["device"] == dev_part]
                if key_part:
                    tiers.extend([
                        [r for r in dev_rows if (r["name"] or "").lower() == key_lower],
                        [r for r in dev_rows if (r["title"] or "").lower() == key_lower],
                        [r for r in dev_rows if r["session_id"].endswith(f":{key_part}")],
                        [
                            r
                            for r in dev_rows
                            if key_lower in (r["name"] or "").lower()
                            or key_lower in (r["title"] or "").lower()
                            or key_part in r["session_id"]
                        ],
                    ])
            tiers.extend([
                [r for r in rows if r["name"] and r["name"].lower() == target_lower],
                [r for r in rows if r["title"] and r["title"].lower() == target_lower],
                [r for r in rows if r["session_id"].endswith(f":{target}")],
                [
                    r
                    for r in rows
                    if target_lower in (r["name"] or "").lower()
                    or target_lower in (r["title"] or "").lower()
                    or target in r["session_id"]
                ],
            ])
            for matches in tiers:
                found = pick(matches)
                if found is not None:
                    return found
        raise AgentError(f"no agent session matching {target!r}", 404)

    def _delivery_report_locked(self, msg: dict) -> dict:
        now_dt = _now_dt()
        now_mono = time.monotonic()
        if msg["to_session"]:
            sid = msg["to_session"]
            row = self.conn.execute("SELECT * FROM agent_sessions WHERE session_id = ?", (sid,)).fetchone()
            sess: dict | None = None
            if row is None:
                delivery = "offline"
                label = sid
            else:
                sess = self._serialize_session(row, now_dt=now_dt, now_mono=now_mono)
                delivery = sess["delivery"]
                label = f"{sess['name']} ({sess['session_id']})" if sess["name"] else sess["session_id"]
            woke = 1 if self._waiters.get(sid, 0) > 0 else 0
            warning: str | None = None
            if delivery == "waiting_user" and sess is not None:
                kind_label = "permission approval" if sess["state"] == "waiting_permission" else "user input"
                pending = sess.get("pending_action") or {}
                tool = str(pending.get("tool") or "").strip()
                detail = str(pending.get("detail") or "").strip()
                if tool and detail:
                    wait_desc = f"{kind_label} ({tool}: {detail[:120]})"
                elif tool or detail:
                    wait_desc = f"{kind_label} ({(tool or detail)[:120]})"
                else:
                    wait_desc = kind_label
                warning = (
                    f"Recipient {label} is waiting for {wait_desc}; "
                    "message saved in inbox until the user responds."
                )
            elif delivery == "queued_idle":
                warning = (
                    f"Recipient {label} is idle with no active listener; "
                    "message queued until its next hook or user turn."
                )
            elif delivery == "offline":
                warning = f"Recipient {label} is offline/stale; message saved in inbox but will not trigger action."
            return {"delivery": delivery, "woke_waiters": woke, "recipients": None, "warning": warning}

        rows = self.conn.execute(
            "SELECT * FROM agent_sessions WHERE session_id != ? AND state != 'exited' ORDER BY updated_at DESC",
            (msg["from_session"],),
        ).fetchall()
        recipients: dict[str, list[str]] = {
            "immediate": [],
            "next_hook": [],
            "waiting_user": [],
            "queued_idle": [],
        }
        for r in rows:
            if msg["to_device"] is not None and r["device"] != msg["to_device"]:
                continue
            if msg["to_repo"] is not None and (not r["repo"] or r["repo"] != msg["to_repo"]):
                continue
            sess = self._serialize_session(r, now_dt=now_dt, now_mono=now_mono)
            if not sess["live"]:
                continue
            bucket = sess["delivery"]
            if bucket in recipients:
                recipients[bucket].append(sess["session_id"])
        woke = len(recipients["immediate"])
        warning = None
        if not recipients["immediate"] and not recipients["next_hook"]:
            if recipients["waiting_user"] and recipients["queued_idle"]:
                warning = (
                    "All matching recipients are waiting for user input or idle with no active listener; "
                    "message saved in inbox."
                )
            elif recipients["waiting_user"]:
                warning = (
                    "All matching recipients are waiting for user permission/input; "
                    "message saved in inbox until the user responds."
                )
            elif recipients["queued_idle"]:
                warning = (
                    "All matching recipients are idle with no active listener; "
                    "message queued until next hook or user turn."
                )
            else:
                warning = "No live matching recipients found; message saved in inbox."
        return {"delivery": "broadcast", "woke_waiters": woke, "recipients": recipients, "warning": warning}

    def delivery_report(self, msg: dict) -> dict:
        with self.lock:
            return self._delivery_report_locked(msg)

    def _send_message_and_report(
        self,
        *,
        device: str,
        from_session: str,
        to: str,
        body: str,
        thread_id: str | None = None,
        reply_to: int | None = None,
        known_devices: set[str] | frozenset[str] = frozenset(),
    ) -> tuple[dict, dict]:
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
        elif ":" not in target and "/" not in target and target in known_devices:
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
            row = self.conn.execute(
                "SELECT m.*, COALESCE(s.name, '') AS from_name, COALESCE(s.title, '') AS from_title "
                "FROM agent_messages m LEFT JOIN agent_sessions s ON s.session_id = m.from_session "
                "WHERE m.id = ?",
                (cur.lastrowid,),
            ).fetchone()
            assert row is not None
            msg = _row_to_message(row)
            report = self._delivery_report_locked(msg)
            self.cond.notify_all()
            return msg, report

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
        msg, _ = self._send_message_and_report(
            device=device,
            from_session=from_session,
            to=to,
            body=body,
            thread_id=thread_id,
            reply_to=reply_to,
            known_devices=known_devices,
        )
        return msg

    def send_message_with_report(
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
        msg, report = self._send_message_and_report(
            device=device,
            from_session=from_session,
            to=to,
            body=body,
            thread_id=thread_id,
            reply_to=reply_to,
            known_devices=known_devices,
        )
        return {"message": msg, **report}

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
                "SELECT m.*, COALESCE(s.name, '') AS from_name, COALESCE(s.title, '') AS from_title "
                "FROM agent_messages m "
                "LEFT JOIN agent_sessions s ON s.session_id = m.from_session "
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

    def wait_for_messages(
        self,
        *,
        device: str,
        session_id: str,
        timeout: float = 55.0,
        mark_read: bool = True,
        limit: int = 50,
        require_full_id: bool = False,
    ) -> dict:
        if not session_id.startswith(f"{device}:"):
            raise AgentError("session belongs to another device", 403)
        timeout = max(0.05, min(float(timeout), 60.0))
        deadline = time.monotonic() + timeout
        current_sid = session_id
        registered_sid: str | None = None
        with self.cond:
            try:
                while True:
                    if self._closed:
                        return {
                            "session_id": current_sid,
                            "full_raw_id": "",
                            "name": "",
                            "title": "",
                            "messages": [],
                            "timed_out": True,
                            "exited": False,
                        }
                    while current_sid in self._promoted_to:
                        current_sid = self._promoted_to[current_sid]
                    if registered_sid is not None and registered_sid != current_sid:
                        self._transfer_waiter(registered_sid, current_sid)
                        registered_sid = current_sid

                    row = self.conn.execute(
                        "SELECT state, full_raw_id, name, title FROM agent_sessions WHERE session_id = ?",
                        (current_sid,),
                    ).fetchone()
                    if row is not None and row["state"] == "exited":
                        return {
                            "session_id": current_sid,
                            "full_raw_id": row["full_raw_id"] or "",
                            "name": row["name"] or "",
                            "title": row["title"] or "",
                            "messages": [],
                            "timed_out": False,
                            "exited": True,
                        }
                    if row is not None:
                        self.conn.execute(
                            "UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?",
                            (_now(), current_sid),
                        )
                    full_raw_id = (row["full_raw_id"] or "") if row else ""
                    sess_name = (row["name"] or "") if row else ""
                    sess_title = (row["title"] or "") if row else ""
                    ready = (not require_full_id) or (bool(full_raw_id) and not full_raw_id.startswith("pid-"))
                    if ready:
                        if registered_sid != current_sid:
                            self._waiters[current_sid] = self._waiters.get(current_sid, 0) + 1
                            registered_sid = current_sid
                        msgs = self.fetch_inbox(
                            device=device,
                            session_id=current_sid,
                            unread_only=True,
                            mark_read=mark_read,
                            limit=limit,
                        )
                        if msgs:
                            return {
                                "session_id": current_sid,
                                "full_raw_id": full_raw_id,
                                "name": sess_name,
                                "title": sess_title,
                                "messages": msgs,
                                "timed_out": False,
                                "exited": False,
                            }
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        return {
                            "session_id": current_sid,
                            "full_raw_id": full_raw_id,
                            "name": sess_name,
                            "title": sess_title,
                            "messages": [],
                            "timed_out": True,
                            "exited": False,
                        }
                    self.cond.wait(timeout=remaining)
            finally:
                if registered_sid is not None:
                    count = self._waiters.get(registered_sid, 0) - 1
                    if count <= 0:
                        self._waiters.pop(registered_sid, None)
                    else:
                        self._waiters[registered_sid] = count
                    self._wait_seen_at[registered_sid] = time.monotonic()

    def ack_messages(self, *, device: str, session_id: str, message_ids: list[int]) -> dict:
        if not session_id.startswith(f"{device}:"):
            raise AgentError("session belongs to another device", 403)
        ids = [int(mid) for mid in message_ids]
        if not ids:
            return {"ok": True, "acked": 0}
        read_at = _now()
        with self.lock:
            self.conn.execute("BEGIN")
            try:
                for mid in ids:
                    self.conn.execute(
                        "INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) VALUES (?, ?, ?)",
                        (session_id, mid, read_at),
                    )
                self.conn.execute("COMMIT")
            except BaseException:
                self.conn.execute("ROLLBACK")
                raise
        return {"ok": True, "acked": len(ids)}

    def ask(
        self,
        *,
        device: str,
        from_session: str,
        to: str,
        body: str,
        timeout: float = 30.0,
        wait_if_idle: bool = False,
        known_devices: set[str] | frozenset[str] = frozenset(),
    ) -> dict:
        timeout = max(0.1, min(float(timeout), 120.0))
        with self.cond:
            sent, report = self._send_message_and_report(
                device=device,
                from_session=from_session,
                to=to,
                body=body,
                known_devices=known_devices,
            )
            if sent["to_session"]:
                if report["delivery"] == "offline":
                    return {
                        "sent": sent,
                        "reply": None,
                        "timed_out": False,
                        "delivered": False,
                        "delivery": "offline",
                        "warning": report["warning"],
                    }
                if report["delivery"] in ("queued_idle", "waiting_user") and not wait_if_idle:
                    return {
                        "sent": sent,
                        "reply": None,
                        "timed_out": False,
                        "delivered": False,
                        "delivery": report["delivery"],
                        "warning": report["warning"],
                    }
            deadline = time.monotonic() + timeout
            while True:
                if self._closed:
                    return {
                        "sent": sent,
                        "reply": None,
                        "timed_out": True,
                        "delivered": False,
                        "delivery": report["delivery"],
                        "warning": report["warning"],
                    }
                row = self.conn.execute(
                    "SELECT m.*, COALESCE(s.name, '') AS from_name, COALESCE(s.title, '') AS from_title "
                    "FROM agent_messages m LEFT JOIN agent_sessions s ON s.session_id = m.from_session "
                    "WHERE m.thread_id = ? AND m.id > ? AND m.from_session != ? "
                    "AND (m.to_session = ? OR (m.to_session IS NULL AND (m.to_device IS NULL OR m.to_device = ?))) "
                    "ORDER BY m.id ASC LIMIT 1",
                    (sent["thread_id"], sent["id"], from_session, from_session, device),
                ).fetchone()
                if row is not None:
                    self.conn.execute(
                        "INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) VALUES (?, ?, ?)",
                        (from_session, row["id"], _now()),
                    )
                    return {
                        "sent": sent,
                        "reply": _row_to_message(row),
                        "timed_out": False,
                        "delivered": True,
                        "delivery": report["delivery"],
                        "warning": report["warning"],
                    }
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return {
                        "sent": sent,
                        "reply": None,
                        "timed_out": True,
                        "delivered": False,
                        "delivery": report["delivery"],
                        "warning": report["warning"],
                    }
                self.cond.wait(timeout=remaining)

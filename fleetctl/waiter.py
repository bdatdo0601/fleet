"""Real-time inbox waiter for CLI background watchers and Codex MCP queue delivery."""

from __future__ import annotations

import contextlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from collections.abc import Callable
from pathlib import Path
from typing import IO

from .client import ApiError, call
from .hooks import _format_unread
from .paths import HARNESSES, node_file


def resolve_api(api_url: str | None) -> str:
    if api_url:
        return api_url
    try:
        return json.loads(node_file().read_text())["api"]
    except FileNotFoundError as e:
        raise ApiError("this machine hasn't joined a fleet (fleet join NAME)") from e


def find_codex_binary() -> str | None:
    if found := shutil.which("codex"):
        return found
    for candidate in (
        Path.home() / ".local" / "bin" / "codex",
        Path("/opt/homebrew/bin/codex"),
        Path("/usr/local/bin/codex"),
    ):
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return str(candidate)
    return None


def format_wait_output(
    session_id: str,
    session_name: str,
    messages: list[dict],
    *,
    rearm_cmd: str | None = None,
) -> str:
    rearm_hint = f"Re-arm listener after handling: run `{rearm_cmd}` in the background." if rearm_cmd else ""
    return _format_unread(session_id, messages, session_name=session_name, rearm_hint=rearm_hint)


def _pid_lock_path(session_id: str, lock_dir: Path | None = None) -> Path:
    base = lock_dir or Path(tempfile.gettempdir())
    safe_sid = re.sub(r"[^a-zA-Z0-9._-]", "_", session_id)
    return base / f"fleet-wait-{os.getuid()}-{safe_sid}.pid"


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def _acquire_pid_lock(session_id: str, lock_dir: Path | None = None) -> Path:
    path = _pid_lock_path(session_id, lock_dir=lock_dir)
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        try:
            old_pid = int(path.read_text().strip())
        except (OSError, ValueError):
            old_pid = 0
        if old_pid > 0 and old_pid != os.getpid():
            with contextlib.suppress(OSError):
                os.kill(old_pid, signal.SIGTERM)
            deadline = time.monotonic() + 0.3
            while time.monotonic() < deadline and _pid_alive(old_pid):
                time.sleep(0.05)

    tmp = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    tmp.write_text(f"{os.getpid()}\n")
    tmp.chmod(0o600)
    os.replace(tmp, path)
    return path


def _release_pid_lock(path: Path) -> None:
    try:
        if path.exists() and path.read_text().strip() == str(os.getpid()):
            path.unlink(missing_ok=True)
    except OSError:
        pass


def _parent_gone(initial_ppid: int) -> bool:
    if initial_ppid <= 1:
        return False
    if os.getppid() != initial_ppid:
        return True
    return not _pid_alive(initial_ppid)


def _infer_harness(session_id: str, harness: str | None) -> str:
    if harness in HARNESSES:
        return harness
    parts = session_id.split(":", 2)
    if len(parts) >= 2 and parts[1] in HARNESSES:
        return parts[1]
    return "antigravity"


def wait_for_inbox_cli(
    *,
    session_id: str,
    api_url: str | None = None,
    harness: str | None = None,
    max_wait: float = 0.0,
    poll_timeout: float = 50.0,
    stdout: IO[str] | None = None,
    lock_dir: Path | None = None,
    watch_parent: bool = True,
) -> int:
    """Block on `/v1/agents/wait` until unread messages arrive, then print and exit 0."""
    out_stream = stdout or sys.stdout
    api = resolve_api(api_url)
    resolved_harness = _infer_harness(session_id, harness)
    initial_ppid = os.getppid()
    deadline = (time.monotonic() + max_wait) if max_wait > 0 else None
    current_sid = session_id
    backoff = 2.0

    lock_path = _acquire_pid_lock(session_id, lock_dir=lock_dir)
    try:
        while True:
            if watch_parent and _parent_gone(initial_ppid):
                return 0
            if deadline is not None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    out_stream.write(
                        "[Fleet listener idle timeout; do not auto-restart unless still expecting peer messages]\n"
                    )
                    out_stream.flush()
                    return 0
                req_timeout = max(0.05, min(float(poll_timeout), remaining, 60.0))
            else:
                req_timeout = max(0.05, min(float(poll_timeout), 60.0))

            try:
                resp = call(
                    api,
                    "POST",
                    "/v1/agents/wait",
                    {
                        "session_id": current_sid,
                        "harness": resolved_harness,
                        "timeout": req_timeout,
                        "mark_read": True,
                    },
                    timeout=req_timeout + 15.0,
                )
                backoff = 2.0
            except (ApiError, OSError):
                if watch_parent and _parent_gone(initial_ppid):
                    return 0
                sleep_for = backoff
                backoff = 5.0 if backoff < 5.0 else 10.0
                if deadline is not None:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        out_stream.write(
                            "[Fleet listener idle timeout; do not auto-restart unless still expecting peer messages]\n"
                        )
                        out_stream.flush()
                        return 0
                    sleep_for = min(sleep_for, remaining)
                time.sleep(sleep_for)
                continue

            current_sid = str(resp.get("session_id") or current_sid)
            if resp.get("exited"):
                return 0
            messages = resp.get("messages") or []
            if messages:
                text = format_wait_output(
                    current_sid,
                    str(resp.get("name") or ""),
                    messages,
                    rearm_cmd=f"fleet agents wait --session {current_sid}",
                )
                out_stream.write(text + "\n")
                out_stream.flush()
                return 0
    finally:
        _release_pid_lock(lock_path)


class CodexQueueListener:
    """Background listener inside `fleet mcp serve --harness codex` pushing inbox messages via `codex queue`."""

    def __init__(
        self,
        *,
        api_url: str | None,
        ppid: int,
        cwd: str,
        codex_bin: str | None = None,
        poll_timeout: float = 50.0,
        run_queue_fn: Callable[[list[str]], tuple[int, str]] | None = None,
    ):
        self.api_url = api_url
        self.ppid = ppid
        self.cwd = cwd
        self.poll_timeout = max(0.05, min(float(poll_timeout), 60.0))
        self._run_queue_fn = run_queue_fn
        if codex_bin is None:
            self.codex_bin: str | None = find_codex_binary() if run_queue_fn is None else "codex"
        else:
            self.codex_bin = codex_bin or None

        self._lock = threading.Lock()
        self._enabled_event = threading.Event()
        self._stop_event = threading.Event()
        self._thread: threading.Thread | None = None
        self._session_id = ""
        self._full_raw_id = ""
        self._delivered_count = 0
        self._last_error: str | None = None

    def start(self) -> dict:
        if not self.codex_bin:
            with self._lock:
                self._last_error = "codex binary not found"
            self._enabled_event.clear()
            return self.status()
        with self._lock:
            self._last_error = None
            self._enabled_event.set()
            if self._thread is None or not self._thread.is_alive():
                self._stop_event.clear()
                self._thread = threading.Thread(target=self._loop, daemon=True)
                self._thread.start()
        return self.status()

    def pause(self) -> dict:
        self._enabled_event.clear()
        return self.status()

    def stop(self) -> dict:
        self._stop_event.set()
        self._enabled_event.clear()
        thread = self._thread
        if thread is not None:
            thread.join(timeout=1.0)
        return self.status()

    def status(self) -> dict:
        with self._lock:
            running = bool(
                self._thread is not None and self._thread.is_alive() and not self._stop_event.is_set()
            )
            return {
                "running": running,
                "enabled": self._enabled_event.is_set(),
                "session_id": self._session_id,
                "full_raw_id": self._full_raw_id,
                "delivered_count": self._delivered_count,
                "last_error": self._last_error,
            }

    def _invoke_queue(self, cmd: list[str]) -> tuple[int, str]:
        if self._run_queue_fn is not None:
            return self._run_queue_fn(cmd)
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True, timeout=15.0)
            detail = (proc.stderr or proc.stdout or "").strip()
            return proc.returncode, detail
        except (OSError, subprocess.SubprocessError) as e:
            return 1, str(e)

    def _loop(self) -> None:
        backoff = 2.0
        while not self._stop_event.is_set():
            if not self._enabled_event.is_set():
                self._stop_event.wait(0.25)
                continue
            if not self.codex_bin:
                with self._lock:
                    self._last_error = "codex binary not found"
                self._enabled_event.clear()
                break
            try:
                api = resolve_api(self.api_url)
                with self._lock:
                    known_sid = self._session_id
                payload: dict = {
                    "harness": "codex",
                    "pid": self.ppid,
                    "cwd": self.cwd,
                    "timeout": self.poll_timeout,
                    "mark_read": False,
                    "require_full_id": True,
                }
                if known_sid:
                    payload["session_id"] = known_sid
                resp = call(api, "POST", "/v1/agents/wait", payload, timeout=self.poll_timeout + 15.0)
                backoff = 2.0
            except (ApiError, OSError, ValueError) as e:
                with self._lock:
                    self._last_error = str(e)
                sleep_for = backoff
                backoff = 5.0 if backoff < 5.0 else 10.0
                self._stop_event.wait(sleep_for)
                continue

            if self._stop_event.is_set():
                break
            if resp.get("exited"):
                self._enabled_event.clear()
                break

            sid = str(resp.get("session_id") or "")
            full_raw = str(resp.get("full_raw_id") or "")
            name = str(resp.get("name") or "")
            with self._lock:
                if sid:
                    self._session_id = sid
                if full_raw:
                    self._full_raw_id = full_raw

            if not self._enabled_event.is_set():
                continue

            messages = resp.get("messages") or []
            if not messages or not full_raw:
                continue

            text = format_wait_output(sid, name, messages, rearm_cmd=None)
            cmd = [self.codex_bin, "queue", "--thread", full_raw, "--message", text, "--resume"]
            rc, err = self._invoke_queue(cmd)
            if rc == 0:
                with contextlib.suppress(ApiError, OSError):
                    call(
                        api,
                        "POST",
                        "/v1/agents/ack",
                        {
                            "harness": "codex",
                            "session_id": sid,
                            "message_ids": [int(m["id"]) for m in messages],
                        },
                        timeout=5.0,
                    )
                with self._lock:
                    self._delivered_count += len(messages)
                    self._last_error = None
            else:
                with self._lock:
                    self._last_error = err or f"codex queue exited with status {rc}"
                self._enabled_event.clear()

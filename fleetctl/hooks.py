"""Local transcript/git context extraction and lifecycle hooks for claude, codex, and antigravity."""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path
from typing import IO

from .client import ApiError, call
from .paths import HARNESSES, node_file

MAX_TRANSCRIPT_BYTES = 256 * 1024
USER_REQUEST_RE = re.compile(r"<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>", re.DOTALL)


def git_context(cwd: str) -> tuple[str, str, str]:
    """Return `(repo_name, branch, short_status)` for `cwd` without blocking."""
    if not cwd:
        return "", "", ""
    path = Path(cwd)
    if not path.is_dir():
        return path.name, "", ""
    repo = path.name
    branch = ""
    git_status = ""
    try:
        top = subprocess.run(
            ["git", "-C", cwd, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD"],
            capture_output=True,
            text=True,
            timeout=1.5,
        )
        if top.returncode == 0:
            lines = [line.strip() for line in top.stdout.splitlines() if line.strip()]
            if lines:
                repo = Path(lines[0]).name
            if len(lines) > 1:
                branch = lines[1]
            st = subprocess.run(
                ["git", "-C", cwd, "status", "--short"],
                capture_output=True,
                text=True,
                timeout=1.5,
            )
            if st.returncode == 0 and st.stdout.strip():
                git_status = "\n".join(st.stdout.splitlines()[:25])[:1500]
    except (OSError, subprocess.SubprocessError):
        pass
    return repo, branch, git_status


def find_codex_transcript(cwd: str, codex_home: Path | None = None) -> str | None:
    """Locate the most recent Codex rollout JSONL matching `cwd`."""
    if not cwd:
        return None
    sessions_dir = (codex_home or Path.home() / ".codex") / "sessions"
    if not sessions_dir.is_dir():
        return None
    cutoff = time.time() - 7200
    candidates: list[tuple[float, Path]] = []
    try:
        for p in sessions_dir.rglob("*.jsonl"):
            try:
                mtime = p.stat().st_mtime
            except OSError:
                continue
            if mtime >= cutoff:
                candidates.append((mtime, p))
    except OSError:
        return None
    candidates.sort(key=lambda item: item[0], reverse=True)
    for _, path in candidates[:10]:
        try:
            with path.open("r", encoding="utf-8", errors="replace") as f:
                first_line = f.readline().strip()
            if not first_line:
                continue
            obj = json.loads(first_line)
            payload = obj.get("payload")
            if isinstance(payload, dict) and payload.get("cwd") == cwd:
                return str(path)
        except (OSError, ValueError):
            continue
    return None


def _extract_text_blocks(content: object) -> str:
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        parts: list[str] = []
        for item in content:
            if isinstance(item, dict) and item.get("type") in ("text", "input_text", "output_text"):
                txt = item.get("text")
                if isinstance(txt, str) and txt.strip():
                    parts.append(txt.strip())
        return "\n".join(parts).strip()
    return ""


def _fmt_tool(name: object, args: object) -> str:
    tool_name = str(name or "tool")
    if isinstance(args, dict):
        preview = ", ".join(f"{k}={str(v)[:60]}" for k, v in list(args.items())[:3])
        return f"{tool_name}({preview})" if preview else tool_name
    if isinstance(args, str) and args.strip():
        return f"{tool_name}({args.strip()[:100]})"
    return tool_name


def _parse_claude_line(obj: dict) -> list[dict]:
    kind = obj.get("type")
    msg = obj.get("message")
    if not isinstance(msg, dict):
        return []
    if kind == "user" and not obj.get("isMeta"):
        text = _extract_text_blocks(msg.get("content"))
        return [{"role": "user", "text": text[:400]}] if text else []
    if kind == "assistant":
        content = msg.get("content")
        if isinstance(content, str) and content.strip():
            return [{"role": "assistant", "text": content.strip()[:400]}]
        if isinstance(content, list):
            items: list[dict] = []
            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "text":
                    txt = str(block.get("text") or "").strip()
                    if txt:
                        items.append({"role": "assistant", "text": txt[:400]})
                elif btype == "tool_use":
                    items.append({"role": "tool", "text": _fmt_tool(block.get("name"), block.get("input"))[:400]})
            return items
    return []


def _parse_codex_line(obj: dict) -> list[dict]:
    if obj.get("type") != "response_item":
        return []
    payload = obj.get("payload")
    if not isinstance(payload, dict):
        return []
    ptype = payload.get("type")
    if ptype == "message" and payload.get("role") in ("user", "assistant"):
        text = _extract_text_blocks(payload.get("content"))
        if not text or text.startswith("<environment_context>") or text.startswith("<permissions"):
            return []
        return [{"role": payload["role"], "text": text[:400]}]
    if ptype in ("function_call", "custom_tool_call", "local_shell_call"):
        label = _fmt_tool(payload.get("name") or ptype, payload.get("arguments") or payload.get("input"))
        return [{"role": "tool", "text": label[:400]}]
    return []


def _parse_antigravity_line(obj: dict) -> list[dict]:
    kind = obj.get("type")
    if kind == "USER_INPUT":
        raw = obj.get("content")
        if not isinstance(raw, str):
            return []
        m = USER_REQUEST_RE.search(raw)
        text = (m.group(1) if m else raw).strip()
        return [{"role": "user", "text": text[:400]}] if text else []
    if kind in ("PLANNER_RESPONSE", "EXECUTOR_RESPONSE", "MODEL_RESPONSE") or "tool_calls" in obj:
        items: list[dict] = []
        raw = obj.get("content")
        if isinstance(raw, str) and raw.strip():
            items.append({"role": "assistant", "text": raw.strip()[:400]})
        calls = obj.get("tool_calls")
        if isinstance(calls, list) and calls:
            names = [str(c.get("name") or c.get("tool") or "tool") for c in calls if isinstance(c, dict)]
            if names:
                items.append({"role": "tool", "text": ", ".join(names)[:400]})
        return items
    return []


def read_transcript_tail(
    harness: str,
    transcript_path: str | None,
    cwd: str = "",
    max_items: int = 20,
) -> tuple[str, list[dict]]:
    """Read the last `max_items` normalized events from a harness transcript JSONL."""
    resolved = transcript_path
    if (not resolved or not Path(resolved).is_file()) and harness == "codex" and cwd:
        resolved = find_codex_transcript(cwd)
    if not resolved:
        return "", []
    path = Path(resolved)
    if not path.is_file():
        return "", []

    parser = {
        "claude": _parse_claude_line,
        "codex": _parse_codex_line,
        "antigravity": _parse_antigravity_line,
    }.get(harness)
    if parser is None:
        return "", []

    items: list[dict] = []
    try:
        size = path.stat().st_size
        start = max(0, size - MAX_TRANSCRIPT_BYTES)
        with path.open("rb") as f:
            if start > 0:
                f.seek(start)
                f.readline()  # discard partial first line
            chunk = f.read().decode("utf-8", errors="replace")
    except OSError:
        return "", []

    for line in chunk.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if isinstance(obj, dict):
            items.extend(parser(obj))

    tail = items[-max_items:]
    summary = ""
    for item in reversed(items):
        if item.get("role") == "user" and item.get("text"):
            summary = item["text"][:160]
            break
    return summary, tail


def _format_unread(session_id: str, messages: list[dict]) -> str:
    lines = [
        f"[Fleet Peer Messages for {session_id}]",
        "Reply using `fleet_send_message` (include `reply_to` / `thread_id` and `session_id` below):",
    ]
    for m in messages:
        lines.append(
            f"- id={m['id']} thread={m['thread_id']} from={m['from_session']}: {m['body']}"
        )
    return "\n".join(lines)


def _format_hook_output(
    *,
    harness: str,
    event: str,
    session_id: str,
    unread: list[dict],
    can_inject: bool,
) -> dict:
    unread_text = _format_unread(session_id, unread) if unread else ""
    if harness == "antigravity":
        if event == "PostToolUse":
            return {}
        if event == "SessionStart":
            if not session_id and not unread_text:
                return {}
            parts = []
            if session_id:
                parts.append(
                    f"Your fleet agent session ID is `{session_id}`. "
                    "Pass this as `session_id` when calling `fleet_send_message`, `fleet_check_inbox`, "
                    "or `fleet_ask_agent`."
                )
            if unread_text:
                parts.append(unread_text)
            return {"injectSteps": [{"systemMessage": {"systemMessage": "\n\n".join(parts)}}]}
        if event == "PreInvocation":
            return {"injectSteps": [{"systemMessage": {"systemMessage": unread_text}}]} if unread_text else {}
        if event == "Stop":
            if unread_text and can_inject:
                return {"decision": "continue", "reason": unread_text}
            return {"decision": "allow"}
        return {}

    if event == "SessionStart":
        parts = []
        if session_id:
            parts.append(f"Your fleet agent session ID is `{session_id}`.")
        if unread_text:
            parts.append(unread_text)
        if not parts:
            return {}
        return {"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": "\n\n".join(parts)}}
    if event in ("UserPromptSubmit", "PostToolUse"):
        if not unread_text:
            return {}
        return {"hookSpecificOutput": {"hookEventName": event, "additionalContext": unread_text}}
    if event == "Stop":
        if unread_text and can_inject:
            return {"decision": "block", "reason": unread_text}
        return {}
    return {}


def _resolve_api(api_url: str | None) -> str:
    if api_url:
        return api_url
    return json.loads(node_file().read_text())["api"]


def run_hook(
    harness: str,
    stdin: IO[str] | None = None,
    stdout: IO[str] | None = None,
    api_url: str | None = None,
) -> int:
    """Process a single lifecycle hook payload on stdin and emit harness JSON on stdout."""
    if harness not in HARNESSES:
        raise ValueError(f"unknown harness {harness!r}")
    in_stream = stdin or sys.stdin
    out_stream = stdout or sys.stdout
    raw_input = in_stream.read().strip()
    try:
        payload = json.loads(raw_input) if raw_input else {}
    except ValueError:
        payload = {}
    if not isinstance(payload, dict):
        payload = {}

    if harness == "antigravity":
        event = str(payload.get("hookEventName") or "")
        raw_id = str(payload.get("conversationId") or "agy")
        ws = payload.get("workspacePaths")
        fallback_ws = ws[0] if isinstance(ws, list) and ws else ""
        cwd = str(payload.get("cwd") or fallback_ws or os.getcwd())
        transcript_path = payload.get("transcriptPath")
        prompt = payload.get("lastUserInput")
        cont_active = bool(payload.get("continuationActive"))
        term_reason = str(payload.get("terminationReason") or "NO_TOOL_CALL")
    else:
        event = str(payload.get("hook_event_name") or "")
        raw_id = str(payload.get("session_id") or f"pid-{os.getppid()}")
        cwd = str(payload.get("cwd") or os.getcwd())
        transcript_path = payload.get("transcript_path")
        prompt = payload.get("prompt")
        cont_active = bool(payload.get("stop_hook_active"))
        term_reason = "NO_TOOL_CALL"

    state = "idle" if event == "Stop" else "working"
    can_inject = not (harness == "antigravity" and event == "PostToolUse") and not (
        event == "Stop" and (cont_active or term_reason != "NO_TOOL_CALL")
    )
    repo, branch, git_status = git_context(cwd)
    tail_summary, tail = read_transcript_tail(
        harness,
        str(transcript_path) if transcript_path else None,
        cwd=cwd,
    )
    summary = prompt.strip()[:160] if isinstance(prompt, str) and prompt.strip() else tail_summary

    session_id = ""
    unread: list[dict] = []
    try:
        api = _resolve_api(api_url)
        resp = call(
            api,
            "POST",
            "/v1/agents/heartbeat",
            {
                "harness": harness,
                "raw_id": raw_id,
                "pid": None if harness == "antigravity" else os.getppid(),
                "cwd": cwd,
                "repo": repo,
                "branch": branch,
                "state": state,
                "summary": summary or None,
                "git_status": git_status,
                "tail": tail,
                "drain_inbox": can_inject,
            },
            timeout=2.0,
        )
        session_id = str(resp.get("session_id") or "")
        raw_unread = resp.get("unread")
        if isinstance(raw_unread, list):
            unread = raw_unread
    except (ApiError, OSError, ValueError, KeyError):
        pass

    output = _format_hook_output(
        harness=harness,
        event=event,
        session_id=session_id,
        unread=unread,
        can_inject=can_inject,
    )
    out_stream.write(json.dumps(output) + "\n")
    out_stream.flush()
    return 0

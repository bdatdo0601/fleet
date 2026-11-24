"""Local transcript/git context extraction and lifecycle hooks for claude, codex, and antigravity."""

from __future__ import annotations

import json
import os
import re
import sqlite3
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


def _read_claude_title(transcript_path: str | None) -> tuple[str, bool]:
    if not transcript_path:
        return "", False
    path = Path(transcript_path)
    if not path.is_file():
        return "", False
    custom_title = ""
    agent_name = ""
    ai_title = ""
    try:
        with path.open("rb") as f:
            for raw_line in f:
                if (
                    b'"custom-title"' not in raw_line
                    and b'"agent-name"' not in raw_line
                    and b'"ai-title"' not in raw_line
                ):
                    continue
                try:
                    obj = json.loads(raw_line.decode("utf-8", errors="replace"))
                except ValueError:
                    continue
                if not isinstance(obj, dict):
                    continue
                kind = obj.get("type")
                if kind == "custom-title" and isinstance(obj.get("customTitle"), str) and obj["customTitle"].strip():
                    custom_title = obj["customTitle"].strip()
                elif kind == "agent-name" and isinstance(obj.get("agentName"), str) and obj["agentName"].strip():
                    agent_name = obj["agentName"].strip()
                elif kind == "ai-title" and isinstance(obj.get("aiTitle"), str) and obj["aiTitle"].strip():
                    ai_title = obj["aiTitle"].strip()
    except OSError:
        return "", False
    if custom_title:
        return custom_title, True
    if agent_name:
        return agent_name, True
    if ai_title:
        return ai_title, False
    return "", False


def _read_codex_title(full_raw_id: str, codex_home: Path | None = None) -> tuple[str, bool]:
    if not full_raw_id or full_raw_id.startswith("pid-"):
        return "", False
    index_path = (codex_home or Path.home() / ".codex") / "session_index.jsonl"
    if not index_path.is_file():
        return "", False
    latest = ""
    try:
        with index_path.open("r", encoding="utf-8", errors="replace") as f:
            for line in f:
                if full_raw_id not in line:
                    continue
                try:
                    obj = json.loads(line)
                except ValueError:
                    continue
                if (
                    isinstance(obj, dict)
                    and obj.get("id") == full_raw_id
                    and isinstance(obj.get("thread_name"), str)
                    and obj["thread_name"].strip()
                ):
                    latest = obj["thread_name"].strip()
    except OSError:
        return "", False
    return latest, False


def _read_antigravity_title(
    full_raw_id: str,
    transcript_path: str | None,
    gemini_home: Path | None = None,
) -> tuple[str, bool]:
    if not full_raw_id or full_raw_id.startswith("pid-"):
        return "", False
    candidates: list[Path] = []
    if transcript_path:
        for parent in Path(transcript_path).expanduser().parents:
            db = parent / "conversation_summaries.db"
            if db.is_file():
                candidates.append(db)
    base = gemini_home or Path.home() / ".gemini"
    for subdir in ("antigravity", "antigravity-cli", "antigravity-ide"):
        db = base / subdir / "conversation_summaries.db"
        if db.is_file() and db not in candidates:
            candidates.append(db)

    for db in candidates:
        try:
            conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=0.5)
            try:
                row = conn.execute(
                    "SELECT title FROM conversation_summaries WHERE conversation_id = ?",
                    (full_raw_id,),
                ).fetchone()
            finally:
                conn.close()
            if row and isinstance(row[0], str) and row[0].strip():
                return row[0].strip(), False
        except (sqlite3.Error, OSError):
            continue
    return "", False


def read_session_title(
    harness: str,
    full_raw_id: str,
    transcript_path: str | None,
    *,
    codex_home: Path | None = None,
    gemini_home: Path | None = None,
) -> tuple[str, bool]:
    """Return `(title, is_explicit_name)` discovered from the local harness state."""
    if harness == "claude":
        return _read_claude_title(transcript_path)
    if harness == "codex":
        return _read_codex_title(full_raw_id, codex_home=codex_home)
    if harness == "antigravity":
        return _read_antigravity_title(full_raw_id, transcript_path, gemini_home=gemini_home)
    return "", False


def _format_unread(
    session_id: str,
    messages: list[dict],
    *,
    session_name: str = "",
    rearm_hint: str = "",
) -> str:
    label = f"{session_name} ({session_id})" if session_name else session_id
    lines = [
        f"[Fleet Peer Messages for {label}]",
        "Reply using `fleet_send_message` (include `reply_to` / `thread_id` and `session_id` below):",
    ]
    for m in messages:
        sender = f"{m['from_name']} ({m['from_session']})" if m.get("from_name") else m["from_session"]
        lines.append(
            f"- id={m['id']} thread={m['thread_id']} from={sender}: {m['body']}"
        )
    if rearm_hint:
        lines.append("")
        lines.append(rearm_hint)
    return "\n".join(lines)


def _session_start_message(harness: str, session_id: str, session_name: str = "") -> str:
    id_label = f"`{session_id}` (name `{session_name}`)" if session_name else f"`{session_id}`"
    lines = [
        f"Your fleet agent session ID is {id_label}. "
        f"Pass `{session_id}` as `session_id` when calling `fleet_send_message`, `fleet_check_inbox`, "
        "`fleet_ask_agent`, `fleet_set_name`, or `fleet_listen`.",
    ]
    if harness == "antigravity":
        lines.append(
            f"To receive peer messages immediately while idle, on your first turn run "
            f"`fleet agents wait --session {session_id}` in the background via `run_command` "
            "(`WaitMsBeforeAsync=500`, `IsDaemon=false`), and re-run it after handling each incoming fleet message."
        )
    elif harness == "claude":
        lines.append(
            f"To receive peer messages immediately while idle, on your first turn run "
            f"`fleet agents wait --session {session_id}` in the background via Bash "
            "(`run_in_background=true`), and re-run it after handling each incoming fleet message."
        )
    elif harness == "codex":
        lines.append(
            "The fleet MCP server automatically listens in the background and delivers incoming peer "
            "messages to this session via `codex queue` (use `fleet_listen` to check status or pause/resume)."
        )
    return "\n".join(lines)


def _summarize_questions(tool_name: str, args: object) -> str:
    if isinstance(args, dict):
        questions = args.get("questions")
        if isinstance(questions, list) and questions:
            parts = []
            for q in questions:
                if isinstance(q, dict):
                    txt = str(q.get("question") or q.get("header") or "").strip()
                    if txt:
                        parts.append(txt)
            if parts:
                return " | ".join(parts)[:400]
        for key in ("question", "prompt"):
            val = args.get(key)
            if isinstance(val, str) and val.strip():
                return val.strip()[:400]
    return _fmt_tool(tool_name, args)[:400]


def _summarize_tool_detail(tool_name: str, tool_input: object) -> str:
    if isinstance(tool_input, dict):
        if tool_name == "ask_permission" or any(k in tool_input for k in ("Action", "Target", "Reason")):
            action = str(tool_input.get("Action") or "").strip()
            target = str(tool_input.get("Target") or "").strip()
            reason = str(tool_input.get("Reason") or "").strip()
            head = f"{action} {target}".strip()
            if head and reason:
                return f"{head} — {reason}"[:400]
            if head or reason:
                return (head or reason)[:400]
        for key in ("command", "CommandLine", "cmd"):
            val = tool_input.get(key)
            if isinstance(val, list):
                joined = " ".join(str(x) for x in val if x is not None).strip()
                if joined:
                    return joined[:400]
            elif isinstance(val, str) and val.strip():
                return val.strip()[:400]
        for key in ("file_path", "TargetFile", "path", "AbsolutePath"):
            val = tool_input.get(key)
            if isinstance(val, str) and val.strip():
                return val.strip()[:400]
        desc = tool_input.get("description")
        if isinstance(desc, str) and desc.strip():
            return desc.strip()[:400]
        return _fmt_tool(tool_name, tool_input)[:400]
    if isinstance(tool_input, str) and tool_input.strip():
        return tool_input.strip()[:400]
    return ""


def _classify_hook_event(
    harness: str,
    event: str,
    payload: dict,
    *,
    cont_active: bool,
    term_reason: str,
) -> tuple[str | None, dict | None, bool]:
    if harness == "antigravity":
        if event == "Stop":
            return "idle", None, not (cont_active or term_reason != "NO_TOOL_CALL")
        if event == "PreToolUse":
            tool_call = payload.get("toolCall") if isinstance(payload.get("toolCall"), dict) else {}
            tool_name = str(tool_call.get("name") or "")
            tool_args = tool_call.get("args")
            if tool_name == "ask_question":
                return (
                    "waiting_question",
                    {
                        "kind": "question",
                        "tool": tool_name,
                        "detail": _summarize_questions(tool_name, tool_args),
                        "event": event,
                    },
                    False,
                )
            if tool_name == "ask_permission":
                return (
                    "waiting_permission",
                    {
                        "kind": "permission",
                        "tool": tool_name,
                        "detail": _summarize_tool_detail(tool_name, tool_args),
                        "event": event,
                    },
                    False,
                )
            return "working", None, False
        if event in ("SessionStart", "PreInvocation"):
            return "working", None, True
        return "working", None, False

    tool_name = str(payload.get("tool_name") or "")
    tool_input = payload.get("tool_input")

    if harness == "claude":
        if event == "PermissionRequest":
            if tool_name == "AskUserQuestion":
                return (
                    "waiting_question",
                    {
                        "kind": "question",
                        "tool": tool_name,
                        "detail": _summarize_questions(tool_name, tool_input),
                        "event": event,
                    },
                    False,
                )
            if tool_name == "ExitPlanMode":
                return (
                    "waiting_question",
                    {
                        "kind": "question",
                        "tool": tool_name,
                        "detail": "Waiting for plan approval",
                        "event": event,
                    },
                    False,
                )
            return (
                "waiting_permission",
                {
                    "kind": "permission",
                    "tool": tool_name,
                    "detail": _summarize_tool_detail(tool_name, tool_input),
                    "event": event,
                },
                False,
            )
        if event == "PreToolUse":
            if tool_name == "AskUserQuestion":
                return (
                    "waiting_question",
                    {
                        "kind": "question",
                        "tool": tool_name,
                        "detail": _summarize_questions(tool_name, tool_input),
                        "event": event,
                    },
                    False,
                )
            if tool_name == "ExitPlanMode":
                return (
                    "waiting_question",
                    {
                        "kind": "question",
                        "tool": tool_name,
                        "detail": "Waiting for plan approval",
                        "event": event,
                    },
                    False,
                )
            return "working", None, False
        if event == "Notification":
            notif_type = str(payload.get("notification_type") or "")
            notif_msg = str(payload.get("message") or payload.get("title") or "").strip()[:400]
            if notif_type == "permission_prompt":
                return (
                    "waiting_permission",
                    {
                        "kind": "permission",
                        "detail": notif_msg or "Waiting for permission approval",
                        "event": f"Notification:{notif_type}",
                    },
                    False,
                )
            if notif_type in ("elicitation_dialog", "elicitation_url_dialog", "agent_needs_input"):
                return (
                    "waiting_question",
                    {
                        "kind": "question",
                        "detail": notif_msg or f"Waiting for user input ({notif_type})",
                        "event": f"Notification:{notif_type}",
                    },
                    False,
                )
            if notif_type == "idle_prompt":
                return "idle", None, False
            return None, None, False
        if event == "Stop":
            return "idle", None, not cont_active
        if event == "SessionEnd":
            return "exited", None, False
        if event in ("SessionStart", "UserPromptSubmit", "PostToolUse"):
            return "working", None, True
        return "working", None, False

    if harness == "codex":
        if event == "PermissionRequest":
            return (
                "waiting_permission",
                {
                    "kind": "permission",
                    "tool": tool_name,
                    "detail": _summarize_tool_detail(tool_name, tool_input),
                    "event": event,
                },
                False,
            )
        if event == "PreToolUse":
            if tool_name == "request_user_input":
                return (
                    "waiting_question",
                    {
                        "kind": "question",
                        "tool": tool_name,
                        "detail": _summarize_questions(tool_name, tool_input),
                        "event": event,
                    },
                    False,
                )
            return "working", None, False
        if event == "Stop":
            return "idle", None, not cont_active
        if event == "Interrupt":
            return "idle", None, False
        if event == "SessionEnd":
            return "exited", None, False
        if event in ("SessionStart", "UserPromptSubmit", "PostToolUse"):
            return "working", None, True
        return "working", None, False

    return "working", None, False


def _format_hook_output(
    *,
    harness: str,
    event: str,
    session_id: str,
    unread: list[dict],
    can_inject: bool,
    session_name: str = "",
) -> dict:
    unread_text = _format_unread(session_id, unread, session_name=session_name) if unread else ""
    if harness == "antigravity":
        if event == "PreToolUse":
            return {"decision": "allow"}
        if event == "PostToolUse":
            return {}
        if event == "SessionStart":
            if not session_id and not unread_text:
                return {}
            parts = []
            if session_id:
                parts.append(_session_start_message(harness, session_id, session_name))
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
            parts.append(_session_start_message(harness, session_id, session_name))
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

    state, pending_action, can_inject = _classify_hook_event(
        harness,
        event,
        payload,
        cont_active=cont_active,
        term_reason=term_reason,
    )
    if state is None:
        out_stream.write("{}\n")
        out_stream.flush()
        return 0

    repo, branch, git_status = git_context(cwd)
    transcript_str = str(transcript_path) if transcript_path else None
    tail_summary, tail = read_transcript_tail(harness, transcript_str, cwd=cwd)
    title, is_explicit_name = read_session_title(harness, raw_id, transcript_str)
    summary = prompt.strip()[:160] if isinstance(prompt, str) and prompt.strip() else tail_summary

    session_id = ""
    session_name = ""
    unread: list[dict] = []
    try:
        api = _resolve_api(api_url)
        hb_payload: dict = {
            "harness": harness,
            "raw_id": raw_id,
            "pid": None if harness == "antigravity" else os.getppid(),
            "cwd": cwd,
            "repo": repo,
            "branch": branch,
            "state": state,
            "title": title or None,
            "name": title if is_explicit_name else None,
            "explicit_name": is_explicit_name,
            "summary": summary or None,
            "git_status": git_status,
            "tail": tail,
            "drain_inbox": can_inject,
        }
        if pending_action is not None:
            hb_payload["pending_action"] = pending_action
        resp = call(api, "POST", "/v1/agents/heartbeat", hb_payload, timeout=2.0)
        session_id = str(resp.get("session_id") or "")
        session_name = str(resp.get("name") or "")
        raw_unread = resp.get("unread")
        if isinstance(raw_unread, list):
            unread = raw_unread
    except (ApiError, OSError, ValueError, KeyError):
        pass

    output = _format_hook_output(
        harness=harness,
        event=event,
        session_id=session_id,
        session_name=session_name,
        unread=unread,
        can_inject=can_inject,
    )
    out_stream.write(json.dumps(output) + "\n")
    out_stream.flush()
    return 0

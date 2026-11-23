"""Idempotent MCP and lifecycle hook installation for claude, codex, and antigravity."""

from __future__ import annotations

import json
import os
import pwd
import re
import shutil
from pathlib import Path

from . import system
from .paths import HARNESSES, launcher_path

HOOK_MARKER = "agent-hook --harness "
TOML_HEADER_RE = re.compile(r"^\s*\[[^\]]+\]\s*$")


def _load_json(path: Path) -> dict:
    if not path.exists():
        return {}
    raw = path.read_text().strip()
    if not raw:
        return {}
    data = json.loads(raw)
    if not isinstance(data, dict):
        raise ValueError(f"{path} must contain a JSON object")
    return data


def _ensure_dir(path: Path, uid: int | None, gid: int | None) -> None:
    missing: list[Path] = []
    cur = path
    while not cur.exists():
        missing.append(cur)
        if cur.parent == cur:
            break
        cur = cur.parent
    path.mkdir(parents=True, exist_ok=True)
    if uid is not None and gid is not None and os.geteuid() == 0:
        for d in reversed(missing):
            os.chown(d, uid, gid)


def _write_user_file(path: Path, content: str, *, uid: int | None = None, gid: int | None = None) -> bool:
    try:
        if path.exists() and path.read_text() == content:
            return False
    except OSError:
        pass
    _ensure_dir(path.parent, uid, gid)
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o600
    tmp = path.with_name(f".{path.name}.fleet-tmp")
    tmp.write_text(content)
    tmp.chmod(mode)
    if uid is not None and gid is not None and os.geteuid() == 0:
        os.chown(tmp, uid, gid)
    os.replace(tmp, path)
    return True


def _has_binary(name: str, home: Path) -> bool:
    if shutil.which(name):
        return True
    for candidate_dir in (home / ".local" / "bin", Path("/opt/homebrew/bin"), Path("/usr/local/bin")):
        if (candidate_dir / name).exists():
            return True
    return False


def detect_harnesses(home: Path, force_all: bool = False) -> list[str]:
    if force_all:
        return list(HARNESSES)
    found: list[str] = []
    if (home / ".claude").exists() or (home / ".claude.json").exists() or _has_binary("claude", home):
        found.append("claude")
    if (home / ".codex").exists() or _has_binary("codex", home):
        found.append("codex")
    if (home / ".gemini").exists() or _has_binary("agy", home):
        found.append("antigravity")
    return found


def _is_fleet_hook_group(group: object, harness: str) -> bool:
    if not isinstance(group, dict):
        return False
    handlers = group.get("hooks")
    if not isinstance(handlers, list):
        return False
    needle = f"{HOOK_MARKER}{harness}"
    return any(isinstance(h, dict) and needle in str(h.get("command") or "") for h in handlers)


def _upsert_claude_style_hooks(hooks_root: dict, *, harness: str, cmd_bin: str) -> None:
    hook_cmd = f"{cmd_bin} agent-hook --harness {harness}"
    events: tuple[tuple[str, str | None], ...] = (
        ("SessionStart", None),
        ("UserPromptSubmit", None),
        ("PostToolUse", "*"),
        ("Stop", None),
    )
    for event, matcher in events:
        entries = hooks_root.get(event)
        if not isinstance(entries, list):
            entries = []
            hooks_root[event] = entries
        handler = {"type": "command", "command": hook_cmd, "timeout": 5}
        desired = {"matcher": matcher, "hooks": [handler]} if matcher is not None else {"hooks": [handler]}
        idx = next((i for i, g in enumerate(entries) if _is_fleet_hook_group(g, harness)), None)
        if idx is not None:
            entries[idx] = desired
        else:
            # Append only, never reorder earlier items (preserves Codex trust-hash indices).
            entries.append(desired)


def _remove_claude_style_hooks(hooks_root: dict, harness: str) -> bool:
    changed = False
    for event in ("SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"):
        entries = hooks_root.get(event)
        if not isinstance(entries, list):
            continue
        filtered = [g for g in entries if not _is_fleet_hook_group(g, harness)]
        if len(filtered) != len(entries):
            changed = True
            if filtered:
                hooks_root[event] = filtered
            else:
                del hooks_root[event]
    return changed


def _update_codex_toml(raw: str, *, cmd_bin: str | None, remove: bool = False) -> str:
    lines = raw.splitlines()
    # Strip existing [mcp_servers.fleet] table up to next section header.
    cleaned: list[str] = []
    skipping = False
    for line in lines:
        stripped = line.strip()
        if stripped == "[mcp_servers.fleet]":
            skipping = True
            continue
        if skipping and TOML_HEADER_RE.match(stripped):
            skipping = False
        if not skipping:
            cleaned.append(line)

    if remove:
        while cleaned and not cleaned[-1].strip():
            cleaned.pop()
        return ("\n".join(cleaned) + "\n") if cleaned else ""

    # Ensure [features] has hooks = true.
    feat_idx = next((i for i, ln in enumerate(cleaned) if ln.strip() == "[features]"), None)
    if feat_idx is None:
        if cleaned and cleaned[-1].strip():
            cleaned.append("")
        cleaned.extend(["[features]", "hooks = true"])
    else:
        end_idx = len(cleaned)
        for i in range(feat_idx + 1, len(cleaned)):
            if TOML_HEADER_RE.match(cleaned[i].strip()):
                end_idx = i
                break
        hooks_idx = next(
            (i for i in range(feat_idx + 1, end_idx) if re.match(r"^\s*hooks\s*=", cleaned[i])),
            None,
        )
        if hooks_idx is not None:
            cleaned[hooks_idx] = "hooks = true"
        else:
            cleaned.insert(feat_idx + 1, "hooks = true")

    if cleaned and cleaned[-1].strip():
        cleaned.append("")
    assert cmd_bin is not None
    cleaned.extend([
        "[mcp_servers.fleet]",
        f"command = {json.dumps(cmd_bin)}",
        'args = ["mcp", "serve", "--harness", "codex"]',
    ])
    return "\n".join(cleaned) + "\n"


def install(
    home: Path | None = None,
    *,
    uid: int | None = None,
    gid: int | None = None,
    bin_path: str | None = None,
    force_all: bool = False,
) -> list[Path]:
    """Install or refresh MCP + lifecycle hook configs for detected harnesses."""
    target_home = home or Path.home()
    cmd_bin = bin_path or launcher_path()
    harnesses = detect_harnesses(target_home, force_all=force_all)
    changed: list[Path] = []

    if "claude" in harnesses:
        claude_json = target_home / ".claude.json"
        data = _load_json(claude_json)
        servers = data.setdefault("mcpServers", {})
        if isinstance(servers, dict):
            servers["fleet"] = {"type": "stdio", "command": cmd_bin, "args": ["mcp", "serve", "--harness", "claude"]}
            if _write_user_file(claude_json, json.dumps(data, indent=2) + "\n", uid=uid, gid=gid):
                changed.append(claude_json)

        settings_json = target_home / ".claude" / "settings.json"
        settings = _load_json(settings_json)
        hooks_root = settings.setdefault("hooks", {})
        if isinstance(hooks_root, dict):
            _upsert_claude_style_hooks(hooks_root, harness="claude", cmd_bin=cmd_bin)
            if _write_user_file(settings_json, json.dumps(settings, indent=2) + "\n", uid=uid, gid=gid):
                changed.append(settings_json)

    if "codex" in harnesses:
        codex_toml = target_home / ".codex" / "config.toml"
        raw_toml = codex_toml.read_text() if codex_toml.exists() else ""
        updated_toml = _update_codex_toml(raw_toml, cmd_bin=cmd_bin, remove=False)
        if _write_user_file(codex_toml, updated_toml, uid=uid, gid=gid):
            changed.append(codex_toml)

        codex_hooks = target_home / ".codex" / "hooks.json"
        hooks_data = _load_json(codex_hooks)
        hooks_root = hooks_data.setdefault("hooks", {})
        if isinstance(hooks_root, dict):
            _upsert_claude_style_hooks(hooks_root, harness="codex", cmd_bin=cmd_bin)
            if _write_user_file(codex_hooks, json.dumps(hooks_data, indent=2) + "\n", uid=uid, gid=gid):
                changed.append(codex_hooks)

    if "antigravity" in harnesses:
        agy_mcp = target_home / ".gemini" / "config" / "mcp_config.json"
        mcp_data = _load_json(agy_mcp)
        servers = mcp_data.setdefault("mcpServers", {})
        if isinstance(servers, dict):
            servers["fleet"] = {"command": cmd_bin, "args": ["mcp", "serve", "--harness", "antigravity"]}
            if _write_user_file(agy_mcp, json.dumps(mcp_data, indent=2) + "\n", uid=uid, gid=gid):
                changed.append(agy_mcp)

        agy_hooks = target_home / ".gemini" / "config" / "hooks.json"
        agy_hooks_data = _load_json(agy_hooks)
        hook_cmd = f"{cmd_bin} agent-hook --harness antigravity"
        handler = {"type": "command", "command": hook_cmd, "timeout": 5}
        agy_hooks_data["fleet"] = {
            "SessionStart": [handler],
            "PreInvocation": [handler],
            "PostToolUse": [{"matcher": "*", "hooks": [handler]}],
            "Stop": [handler],
        }
        if _write_user_file(agy_hooks, json.dumps(agy_hooks_data, indent=2) + "\n", uid=uid, gid=gid):
            changed.append(agy_hooks)

    return changed


def uninstall(home: Path | None = None) -> list[Path]:
    """Remove fleet MCP and hook entries from all harness configs."""
    target_home = home or Path.home()
    changed: list[Path] = []

    claude_json = target_home / ".claude.json"
    if claude_json.exists():
        data = _load_json(claude_json)
        servers = data.get("mcpServers")
        if isinstance(servers, dict) and "fleet" in servers:
            del servers["fleet"]
            if _write_user_file(claude_json, json.dumps(data, indent=2) + "\n"):
                changed.append(claude_json)

    settings_json = target_home / ".claude" / "settings.json"
    if settings_json.exists():
        settings = _load_json(settings_json)
        hooks_root = settings.get("hooks")
        if (
            isinstance(hooks_root, dict)
            and _remove_claude_style_hooks(hooks_root, "claude")
            and _write_user_file(settings_json, json.dumps(settings, indent=2) + "\n")
        ):
            changed.append(settings_json)

    codex_toml = target_home / ".codex" / "config.toml"
    if codex_toml.exists():
        updated = _update_codex_toml(codex_toml.read_text(), cmd_bin=None, remove=True)
        if _write_user_file(codex_toml, updated):
            changed.append(codex_toml)

    codex_hooks = target_home / ".codex" / "hooks.json"
    if codex_hooks.exists():
        hooks_data = _load_json(codex_hooks)
        hooks_root = hooks_data.get("hooks")
        if (
            isinstance(hooks_root, dict)
            and _remove_claude_style_hooks(hooks_root, "codex")
            and _write_user_file(codex_hooks, json.dumps(hooks_data, indent=2) + "\n")
        ):
            changed.append(codex_hooks)

    agy_mcp = target_home / ".gemini" / "config" / "mcp_config.json"
    if agy_mcp.exists():
        mcp_data = _load_json(agy_mcp)
        servers = mcp_data.get("mcpServers")
        if isinstance(servers, dict) and "fleet" in servers:
            del servers["fleet"]
            if _write_user_file(agy_mcp, json.dumps(mcp_data, indent=2) + "\n"):
                changed.append(agy_mcp)

    agy_hooks = target_home / ".gemini" / "config" / "hooks.json"
    if agy_hooks.exists():
        agy_hooks_data = _load_json(agy_hooks)
        if "fleet" in agy_hooks_data:
            del agy_hooks_data["fleet"]
            if _write_user_file(agy_hooks, json.dumps(agy_hooks_data, indent=2) + "\n"):
                changed.append(agy_hooks)

    return changed


def status(home: Path | None = None) -> dict[str, bool]:
    """Return `{harness: is_configured}` for all supported harnesses."""
    target_home = home or Path.home()
    claude_mcp = _load_json(target_home / ".claude.json").get("mcpServers", {})
    claude_hooks = _load_json(target_home / ".claude" / "settings.json").get("hooks", {})
    claude_ok = (
        isinstance(claude_mcp, dict)
        and "fleet" in claude_mcp
        and isinstance(claude_hooks, dict)
        and any(_is_fleet_hook_group(g, "claude") for g in claude_hooks.get("SessionStart", []))
    )

    codex_toml_path = target_home / ".codex" / "config.toml"
    codex_toml = codex_toml_path.read_text() if codex_toml_path.exists() else ""
    codex_hooks = _load_json(target_home / ".codex" / "hooks.json").get("hooks", {})
    codex_ok = (
        "[mcp_servers.fleet]" in codex_toml
        and isinstance(codex_hooks, dict)
        and any(_is_fleet_hook_group(g, "codex") for g in codex_hooks.get("SessionStart", []))
    )

    agy_mcp = _load_json(target_home / ".gemini" / "config" / "mcp_config.json").get("mcpServers", {})
    agy_hooks = _load_json(target_home / ".gemini" / "config" / "hooks.json")
    agy_ok = (
        isinstance(agy_mcp, dict)
        and "fleet" in agy_mcp
        and isinstance(agy_hooks, dict)
        and "fleet" in agy_hooks
    )

    return {"claude": claude_ok, "codex": codex_ok, "antigravity": agy_ok}


def sync_owner_configs() -> list[Path]:
    """Called by `fleet sync` (running as root) to keep the repo owner's harness configs updated."""
    owner = system.repo_owner()
    if owner == "root":
        return []
    pw = pwd.getpwnam(owner)
    return install(Path(pw.pw_dir), uid=pw.pw_uid, gid=pw.pw_gid)

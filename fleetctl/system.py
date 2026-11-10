"""macOS side effects: root-owned files, launchd daemons, the nebula service."""

from __future__ import annotations

import os
import plistlib
import pwd
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

from .paths import (
    DDNS_LABEL,
    NEBULA_LABEL,
    REPO,
    SERVE_LABEL,
    SYNC_INTERVAL_SECONDS,
    SYNC_LABEL,
    state_dir,
)  # fmt: skip

LAUNCH_DAEMONS = Path("/Library/LaunchDaemons")


def is_root() -> bool:
    return os.geteuid() == 0


def sudo(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    cmd = list(args) if is_root() else ["sudo", *args]
    return subprocess.run(cmd, check=check, text=True, capture_output=not check)


def write_root_file(path: Path, content: str, mode: int = 0o644) -> bool:
    """Write a root-owned file; returns True if the content changed."""
    try:
        if path.read_text() == content:
            return False
    except (FileNotFoundError, PermissionError):
        pass
    if is_root():
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_name(f".{path.name}.tmp")
        tmp.write_text(content)
        tmp.chmod(mode)
        os.replace(tmp, path)
        return True
    with tempfile.NamedTemporaryFile("w", delete=False) as f:
        f.write(content)
    try:
        sudo("mkdir", "-p", str(path.parent))
        sudo("install", "-m", f"{mode:o}", "-o", "root", "-g", "wheel", f.name, str(path))
    finally:
        os.unlink(f.name)
    return True


def ensure_nebula() -> None:
    if not shutil.which("nebula") or not shutil.which("nebula-cert"):
        print("==> installing nebula (brew)")
        subprocess.run(["brew", "install", "nebula"], check=True)


ALF = "/usr/libexec/ApplicationFirewall/socketfilterfw"


def firewall_allow(*binaries: str | None) -> None:
    """If the macOS application firewall is on, let these binaries accept incoming
    traffic. Paths are resolved because Homebrew's Cellar path changes on upgrade,
    so callers re-run this (sync does, every run)."""
    state = subprocess.run([ALF, "--getglobalstate"], capture_output=True, text=True).stdout
    if "enabled" not in state:
        return
    for binary in binaries:
        if binary:
            real = os.path.realpath(binary)
            sudo(ALF, "--add", real, check=False)
            sudo(ALF, "--unblockapp", real, check=False)


def start_nebula() -> None:
    """Load the Homebrew root service (first time) or restart it."""
    firewall_allow(shutil.which("nebula"))
    if launchd_loaded(NEBULA_LABEL):
        restart(NEBULA_LABEL)
    else:
        sudo("brew", "services", "start", "nebula")


def launchd_loaded(label: str) -> bool:
    return sudo("launchctl", "print", f"system/{label}", check=False).returncode == 0


def restart(label: str) -> None:
    sudo("launchctl", "kickstart", "-k", f"system/{label}", check=False)


def install_daemon(label: str, args: list[str], *, user: str | None = None, interval: int | None = None,
                   keepalive: bool = False, log: Path) -> None:  # fmt: skip
    plist: dict = {
        "Label": label,
        "ProgramArguments": args,
        "WorkingDirectory": str(REPO),
        "EnvironmentVariables": {"PYTHONPATH": str(REPO), "PATH": "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"},
        "RunAtLoad": True,
        "StandardOutPath": str(log),
        "StandardErrorPath": str(log),
    }
    if user:
        plist["UserName"] = user
        home = pwd.getpwnam(user).pw_dir
        plist["EnvironmentVariables"]["HOME"] = home
    if interval:
        plist["StartInterval"] = interval
    if keepalive:
        plist["KeepAlive"] = True
    path = LAUNCH_DAEMONS / f"{label}.plist"
    write_root_file(path, plistlib.dumps(plist).decode())
    sudo("launchctl", "bootout", f"system/{label}", check=False)
    sudo("launchctl", "bootstrap", "system", str(path))


# Daemons go through bin/fleet (python3 from PATH) rather than sys.executable,
# whose versioned Homebrew path disappears on the next major Python upgrade.
LAUNCHER = str(REPO / "bin" / "fleet")


def install_sync_daemon() -> None:
    install_daemon(SYNC_LABEL, [LAUNCHER, "sync"], interval=SYNC_INTERVAL_SECONDS,
                   log=Path("/var/log/fleet-sync.log"))  # fmt: skip


def install_serve_daemon(user: str) -> None:
    install_daemon(SERVE_LABEL, [LAUNCHER, "serve"], user=user, keepalive=True,
                   log=state_dir() / "serve.log")  # fmt: skip


def install_ddns_daemon(user: str) -> None:
    install_daemon(DDNS_LABEL, [LAUNCHER, "ddns", "run"], user=user, interval=SYNC_INTERVAL_SECONDS,
                   log=state_dir() / "ddns.log")  # fmt: skip


def repo_owner() -> str:
    return pwd.getpwuid(REPO.stat().st_uid).pw_name


def self_update() -> bool:
    """`git pull --ff-only` as the repo owner (never as root). Best effort; True if HEAD moved."""
    if not (REPO / ".git").exists():
        return False
    owner = repo_owner()

    def git(*args: str) -> subprocess.CompletedProcess:
        cmd = ["git", "-C", str(REPO), *args]
        if is_root() and owner != "root":
            cmd = ["sudo", "-u", owner, "-H", *cmd]
        env = dict(os.environ, GIT_TERMINAL_PROMPT="0", GIT_SSH_COMMAND="ssh -o BatchMode=yes")
        return subprocess.run(cmd, capture_output=True, text=True, timeout=60, env=env)

    before = git("rev-parse", "HEAD").stdout.strip()
    pulled = git("pull", "--ff-only", "-q")
    if pulled.returncode != 0:
        print(f"self-update skipped: {pulled.stderr.strip() or 'git pull failed'}", file=sys.stderr)
        return False
    return git("rev-parse", "HEAD").stdout.strip() != before

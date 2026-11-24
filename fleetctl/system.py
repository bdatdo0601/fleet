"""macOS side effects: root-owned files, launchd daemons, nebula, and Go migration helpers."""

from __future__ import annotations

import contextlib
import hashlib
import json
import os
import platform
import plistlib
import pwd
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

from .paths import (
    DDNS_LABEL,
    NEBULA_LABELS,
    REPO,
    SERVE_LABEL,
    SYNC_INTERVAL_SECONDS,
    SYNC_LABEL,
    api_port,
    installed_binary,
    state_dir,
    state_dir_for_owner,
    symlink_path,
)  # fmt: skip

LAUNCH_DAEMONS = Path("/Library/LaunchDaemons")
STANDARD_PATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"


def is_root() -> bool:
    return os.geteuid() == 0


def sudo(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    cmd = list(args) if is_root() else ["sudo", *args]
    return subprocess.run(cmd, check=check, text=True, capture_output=not check)


def write_root_file(path: Path, content: str, mode: int = 0o644) -> bool:
    """Write a root-owned file; returns True if the content changed."""
    with contextlib.suppress(FileNotFoundError, PermissionError):
        if path.read_text() == content:
            return False
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


def nebula_label(daemons: Path = LAUNCH_DAEMONS) -> str | None:
    """The launchd label Homebrew gave the nebula service, or None if not installed."""
    for label in NEBULA_LABELS:
        if (daemons / f"{label}.plist").exists():
            return label
    return None


def start_nebula() -> None:
    """Load the Homebrew root service (first time) or restart it."""
    firewall_allow(shutil.which("nebula"))
    label = nebula_label()
    if label and launchd_loaded(label):
        restart(label)
    else:
        sudo("brew", "services", "start", "nebula")


def launchd_loaded(label: str) -> bool:
    return sudo("launchctl", "print", f"system/{label}", check=False).returncode == 0


def restart(label: str) -> None:
    sudo("launchctl", "kickstart", "-k", f"system/{label}", check=False)


def _reload_daemon_detached(label: str, plist_path: Path) -> None:
    """Reload a LaunchDaemon after a 1s delay in a detached session so `fleet sync` won't SIGTERM itself."""
    script = (
        'sleep 1; launchctl bootout "system/$1" >/dev/null 2>&1 || true; '
        'launchctl bootstrap system "$2" >/dev/null 2>&1 || true'
    )
    cmd = ["sh", "-c", script, "_", label, str(plist_path)]
    if not is_root():
        cmd = ["sudo", *cmd]
    subprocess.Popen(
        cmd,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )


def install_daemon(
    label: str,
    args: list[str],
    *,
    user: str | None = None,
    interval: int | None = None,
    keepalive: bool = False,
    log: Path,
    workdir: Path | None = None,
    include_pythonpath: bool = True,
    reload_detached: bool = False,
) -> None:
    env: dict[str, str] = {"PATH": STANDARD_PATH}
    if include_pythonpath:
        env["PYTHONPATH"] = str(REPO)
    plist: dict = {
        "Label": label,
        "ProgramArguments": args,
        "WorkingDirectory": str(workdir or REPO),
        "EnvironmentVariables": env,
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
    if reload_detached:
        _reload_daemon_detached(label, path)
    else:
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


# Go migration bridge helpers ------------------------------------------------


def go_arch() -> str:
    m = platform.machine().lower()
    if m in ("arm64", "aarch64"):
        return "arm64"
    if m in ("x86_64", "amd64"):
        return "amd64"
    return m


def git_head_sha(repo: Path = REPO) -> str:
    if not (repo / ".git").exists():
        return ""
    proc = subprocess.run(["git", "-C", str(repo), "rev-parse", "HEAD"], capture_output=True, text=True, timeout=10)
    return proc.stdout.strip() if proc.returncode == 0 else ""


def python_client_id(repo: Path = REPO) -> str:
    sha = git_head_sha(repo)
    return f"python/{sha[:7]}" if sha else "python/dev"


def find_go_binary() -> str | None:
    for cand in (Path("/opt/homebrew/bin/go"), Path("/usr/local/bin/go")):
        if cand.is_file() and os.access(cand, os.X_OK):
            return str(cand)
    return shutil.which("go")


def file_sha256(path: Path) -> tuple[str, int] | None:
    if not path.is_file():
        return None
    h = hashlib.sha256()
    total = 0
    with path.open("rb") as f:
        while True:
            chunk = f.read(65536)
            if not chunk:
                break
            h.update(chunk)
            total += len(chunk)
    return h.hexdigest(), total


def smoke_test_binary(bin_path: Path) -> None:
    proc = subprocess.run([str(bin_path), "version"], capture_output=True, text=True, timeout=10)
    if proc.returncode != 0 or "fleet " not in (proc.stdout + proc.stderr):
        detail = (proc.stderr or proc.stdout).strip()
        raise RuntimeError(f"smoke test `{bin_path} version` failed: {detail}")


def ensure_symlink(target: Path, link: Path) -> None:
    if link.is_symlink():
        with contextlib.suppress(OSError):
            if Path(os.readlink(link)) == target:
                return
    try:
        link.parent.mkdir(parents=True, exist_ok=True)
        tmp = link.with_name(f".{link.name}.{os.getpid()}.sym")
        tmp.unlink(missing_ok=True)
        os.symlink(target, tmp)
        os.replace(tmp, link)
    except PermissionError:
        sudo("mkdir", "-p", str(link.parent))
        sudo("ln", "-sfn", str(target), str(link))


def install_binary_atomic(src: Path, target: Path, link: Path | None = None) -> Path | None:
    """Smoke-test `src`, preserve `.prev` backup if `target` exists, atomically install, symlink, and allowlist."""
    smoke_test_binary(src)
    prev_path: Path | None = None
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.exists():
        cand = target.with_name(target.name + ".prev")
        prev_tmp = target.with_name(f".{target.name}.prev.{os.getpid()}.tmp")
        shutil.copy2(target, prev_tmp)
        prev_tmp.chmod(0o755)
        os.replace(prev_tmp, cand)
        prev_path = cand
    tmp = target.with_name(f".{target.name}.{os.getpid()}.tmp")
    shutil.copy2(src, tmp)
    tmp.chmod(0o755)
    if os.geteuid() == 0:
        os.chown(tmp, 0, 0)
    os.replace(tmp, target)
    if link is not None:
        ensure_symlink(target, link)
    firewall_allow(str(target))
    return prev_path


def rollback_binary(target: Path, prev_path: Path | None) -> None:
    if prev_path is not None and prev_path.exists():
        os.replace(prev_path, target)
        firewall_allow(str(target))


def install_go_sync_daemon(bin_path: Path, workdir: Path | None = None, *, reload_detached: bool = True) -> None:
    install_daemon(
        SYNC_LABEL,
        [str(bin_path), "sync"],
        interval=SYNC_INTERVAL_SECONDS,
        log=Path("/var/log/fleet-sync.log"),
        workdir=workdir or bin_path.parent,
        include_pythonpath=False,
        reload_detached=reload_detached,
    )


def install_go_serve_daemon(bin_path: Path, user: str, st_dir: Path, workdir: Path | None = None) -> None:
    install_daemon(
        SERVE_LABEL,
        [str(bin_path), "serve"],
        user=user,
        keepalive=True,
        log=st_dir / "serve.log",
        workdir=workdir or st_dir,
        include_pythonpath=False,
    )


def install_go_ddns_daemon(bin_path: Path, user: str, st_dir: Path, workdir: Path | None = None) -> None:
    install_daemon(
        DDNS_LABEL,
        [str(bin_path), "ddns", "run"],
        user=user,
        interval=SYNC_INTERVAL_SECONDS,
        log=st_dir / "ddns.log",
        workdir=workdir or st_dir,
        include_pythonpath=False,
    )


def wait_serve_healthy(api_url: str, timeout: float = 10.0) -> bool:
    deadline = time.monotonic() + timeout
    url = api_url.rstrip("/") + "/v1/sync"
    while True:
        try:
            with urllib.request.urlopen(url, timeout=2.0) as resp:  # noqa: S310
                if resp.status == 200:
                    return True
        except (urllib.error.URLError, OSError):
            pass
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.25)


def _run_as_owner(
    owner: str,
    workdir: Path,
    extra_env: dict[str, str],
    timeout: float,
    *cmd: str,
) -> subprocess.CompletedProcess:
    env_map = {"PATH": STANDARD_PATH, "GOTOOLCHAIN": "auto", **extra_env}
    if owner and owner != "root":
        with contextlib.suppress(KeyError):
            env_map["HOME"] = pwd.getpwnam(owner).pw_dir
    if is_root() and owner and owner != "root":
        kv = [f"{k}={v}" for k, v in sorted(env_map.items())]
        full_cmd = ["sudo", "-u", owner, "-H", "env", *kv, *cmd]
        return subprocess.run(full_cmd, cwd=str(workdir), capture_output=True, text=True, timeout=timeout)
    merged_env = dict(os.environ, **env_map)
    return subprocess.run(list(cmd), cwd=str(workdir), env=merged_env, capture_output=True, text=True, timeout=timeout)


def lighthouse_build_and_migrate(
    *,
    repo: Path = REPO,
    st_dir: Path | None = None,
    owner: str = "",
    target_bin: Path | None = None,
    link_path: Path | None = None,
    local_api: str | None = None,
    rebuild: bool = False,
    daemons_dir: Path = LAUNCH_DAEMONS,
    healthcheck_fn: object | None = None,
) -> tuple[str, str]:
    """Build Go binaries on the lighthouse, install & healthcheck `serve`, and publish manifest.json.

    Returns `(status, detail)` where status is one of:
    `no_go_mod`, `missing_go`, `up_to_date`, `updated`, `failed`.
    """
    if not (repo / "go.mod").is_file():
        return "no_go_mod", ""
    eff_owner = owner or repo_owner()
    eff_state = st_dir or state_dir_for_owner(eff_owner)
    eff_target = target_bin or installed_binary()
    eff_link = link_path or symlink_path()
    eff_api = local_api or f"http://127.0.0.1:{api_port()}"

    head_sha = git_head_sha(repo)
    bin_dir = eff_state / "bin"
    manifest_path = bin_dir / "manifest.json"
    manifest_ok = False
    existing_commit = ""
    if manifest_path.is_file():
        with contextlib.suppress(OSError, ValueError):
            m = json.loads(manifest_path.read_text())
            if isinstance(m, dict) and (m.get("binaries") or {}):
                manifest_ok = True
                existing_commit = str(m.get("commit") or "")

    need_build = rebuild or (not eff_target.exists()) or (not manifest_ok) or (
        bool(head_sha) and existing_commit != head_sha
    )
    if not need_build:
        return "up_to_date", head_sha

    go_bin = find_go_binary()
    if not go_bin:
        return "missing_go", "go not found on lighthouse (install with: brew install go); staying on Python"

    bin_dir.mkdir(parents=True, exist_ok=True)
    if is_root() and eff_owner and eff_owner != "root":
        with contextlib.suppress(KeyError):
            pw = pwd.getpwnam(eff_owner)
            os.chown(eff_state, pw.pw_uid, pw.pw_gid)
            os.chown(bin_dir, pw.pw_uid, pw.pw_gid)

    built_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    commit_val = head_sha or "dev"
    ldflags = (
        f"-s -w -X github.com/bdatdo0601/fleet/internal/version.Commit={commit_val} "
        f"-X github.com/bdatdo0601/fleet/internal/version.BuiltAt={built_at}"
    )

    tmp_bins: dict[str, Path] = {}
    try:
        for arch in ("arm64", "amd64"):
            tmp_out = bin_dir / f".fleet-darwin-{arch}.tmp"
            tmp_bins[arch] = tmp_out
            proc = _run_as_owner(
                eff_owner,
                repo,
                {"CGO_ENABLED": "0", "GOOS": "darwin", "GOARCH": arch},
                240.0,
                go_bin, "build", "-ldflags", ldflags, "-o", str(tmp_out), "./cmd/fleet",
            )
            if proc.returncode != 0:
                detail = (proc.stderr or proc.stdout).strip()
                raise RuntimeError(f"go build darwin/{arch} failed: {detail}")

        host_arch = go_arch()
        if host_arch not in tmp_bins:
            raise RuntimeError(f"unsupported host arch {host_arch}")

        serve_plist = daemons_dir / f"{SERVE_LABEL}.plist"
        prev_serve_plist = serve_plist.read_text() if serve_plist.exists() else None
        ddns_plist = daemons_dir / f"{DDNS_LABEL}.plist"
        prev_ddns_plist = ddns_plist.read_text() if ddns_plist.exists() else None
        ddns_was_loaded = launchd_loaded(DDNS_LABEL)
        prev_link_target: Path | None = None
        if eff_link.is_symlink():
            with contextlib.suppress(OSError):
                prev_link_target = Path(os.readlink(eff_link))

        prev_bin = install_binary_atomic(tmp_bins[host_arch], eff_target, eff_link)
        install_go_serve_daemon(eff_target, eff_owner, eff_state, repo)
        if ddns_was_loaded:
            install_go_ddns_daemon(eff_target, eff_owner, eff_state, repo)

        checker = healthcheck_fn if callable(healthcheck_fn) else wait_serve_healthy
        if not checker(eff_api):
            if prev_bin is not None:
                rollback_binary(eff_target, prev_bin)
            else:
                eff_target.unlink(missing_ok=True)
                if prev_link_target is not None:
                    ensure_symlink(prev_link_target, eff_link)
            if prev_serve_plist is not None:
                write_root_file(serve_plist, prev_serve_plist)
                sudo("launchctl", "bootout", f"system/{SERVE_LABEL}", check=False)
                sudo("launchctl", "bootstrap", "system", str(serve_plist), check=False)
            if ddns_was_loaded and prev_ddns_plist is not None:
                write_root_file(ddns_plist, prev_ddns_plist)
                sudo("launchctl", "bootout", f"system/{DDNS_LABEL}", check=False)
                sudo("launchctl", "bootstrap", "system", str(ddns_plist), check=False)
            raise RuntimeError("new fleet serve failed health check; rolled back")

        binaries_meta: dict[str, dict] = {}
        for arch in ("arm64", "amd64"):
            final_name = f"fleet-darwin-{arch}"
            final_path = bin_dir / final_name
            os.replace(tmp_bins[arch], final_path)
            final_path.chmod(0o755)
            if is_root() and eff_owner and eff_owner != "root":
                with contextlib.suppress(KeyError):
                    pw = pwd.getpwnam(eff_owner)
                    os.chown(final_path, pw.pw_uid, pw.pw_gid)
            digest_info = file_sha256(final_path)
            assert digest_info is not None
            sha_hex, sz = digest_info
            binaries_meta[f"darwin/{arch}"] = {"file": final_name, "sha256": sha_hex, "size": sz}

        manifest = {
            "version": "0.1.0",
            "commit": head_sha,
            "built_at": built_at,
            "binaries": binaries_meta,
        }
        tmp_manifest = bin_dir / f".manifest.{os.getpid()}.tmp"
        tmp_manifest.write_text(json.dumps(manifest, indent=2) + "\n")
        tmp_manifest.chmod(0o644)
        if is_root() and eff_owner and eff_owner != "root":
            with contextlib.suppress(KeyError):
                pw = pwd.getpwnam(eff_owner)
                os.chown(tmp_manifest, pw.pw_uid, pw.pw_gid)
        os.replace(tmp_manifest, manifest_path)

        install_go_sync_daemon(eff_target, repo, reload_detached=True)
        return "updated", commit_val
    except Exception as e:  # noqa: BLE001 - clean up staging and report cleanly
        for p in tmp_bins.values():
            p.unlink(missing_ok=True)
        return "failed", str(e)

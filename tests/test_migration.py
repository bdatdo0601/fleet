"""Tests for Python-to-Go zero-touch migration bridge and cross-language interoperability."""

from __future__ import annotations

import hashlib
import http.server
import json
import os
import plistlib
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

from fleetctl import cli, codes, pki, system
from fleetctl.client import call
from fleetctl.core import Fleet
from fleetctl.paths import REPO, SERVE_LABEL, SYNC_LABEL

PASSPHRASE = "correct horse battery staple"


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def keypair(directory: Path, name: str) -> tuple[Path, str]:
    key, pub = directory / f"{name}.key", directory / f"{name}.pub"
    pki.keygen(key, pub)
    return key, pub.read_text()


class MigrationBridgeTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)

    def test_bin_fleet_respects_force_python_and_delegates_to_go_bin(self) -> None:
        fake_go = self.tmp / "fleet-go"
        fake_go.write_text("#!/bin/sh\necho 'DELEGATED_TO_GO'\n")
        fake_go.chmod(0o755)

        env_go = dict(os.environ, FLEET_INSTALL_BIN=str(fake_go))
        env_go.pop("FLEET_FORCE_PYTHON", None)
        p1 = subprocess.run([str(REPO / "bin" / "fleet"), "version"], env=env_go, capture_output=True, text=True)
        self.assertEqual(p1.returncode, 0)
        self.assertEqual(p1.stdout.strip(), "DELEGATED_TO_GO")

        env_py = dict(os.environ, FLEET_INSTALL_BIN=str(fake_go), FLEET_FORCE_PYTHON="1")
        p2 = subprocess.run([str(REPO / "bin" / "fleet"), "--help"], env=env_py, capture_output=True, text=True)
        self.assertEqual(p2.returncode, 0)
        self.assertIn("Self-hosted Nebula fleet", p2.stdout)

    def test_client_bridge_downloads_verifies_and_migrates_plist(self) -> None:
        good_bin = self.tmp / "good-go"
        good_bin.write_text("#!/bin/sh\necho 'fleet 0.1.0 (test)'\n")
        good_bytes = good_bin.read_bytes()
        good_sha = hashlib.sha256(good_bytes).hexdigest()

        etc_dir = self.tmp / "etc"
        etc_dir.mkdir()
        target_bin = self.tmp / "libexec" / "fleet"
        sym_bin = self.tmp / "brew" / "fleet"
        daemons_dir = self.tmp / "LaunchDaemons"
        daemons_dir.mkdir()

        state = {"sha": good_sha, "payload": good_bytes}

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_: object) -> None:
                pass

            def do_GET(self) -> None:
                if self.path.startswith("/v1/sync"):
                    body = json.dumps({
                        "name": "laptop",
                        "ip": "10.213.0.2",
                        "kind": "mac",
                        "ca": "ca-pem",
                        "cert": "cert-pem",
                        "config": "config-yml",
                        "binary_sha256": state["sha"],
                        "binary_version": "go/abc1234",
                    }).encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    return
                if self.path.startswith("/v1/binary"):
                    data = state["payload"]
                    self.send_response(200)
                    self.send_header("Content-Type", "application/octet-stream")
                    self.send_header("Content-Length", str(len(data)))
                    self.send_header("X-Fleet-SHA256", hashlib.sha256(data).hexdigest())
                    self.end_headers()
                    self.wfile.write(data)
                    return
                self.send_response(404)
                self.end_headers()

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        # Use localhost (not 127.0.0.1) so cmd_sync treats this as a roaming client rather than lighthouse.
        api_url = f"http://localhost:{srv.server_address[1]}"

        (etc_dir / "fleet.json").write_text(json.dumps({"name": "laptop", "api": api_url, "owner": "datdo"}))

        reloaded: list[tuple[str, Path]] = []
        execed: list[tuple[str, list[str]]] = []

        class ExecCalled(BaseException):
            pass

        def fake_execv(prog: str, argv: list[str]) -> None:
            execed.append((prog, argv))
            raise ExecCalled()

        with (
            mock.patch.dict(os.environ, {
                "FLEET_NEBULA_ETC": str(etc_dir),
                "FLEET_INSTALL_BIN": str(target_bin),
                "FLEET_BIN_SYMLINK": str(sym_bin),
            }),
            mock.patch.object(system, "is_root", return_value=True),
            mock.patch.object(system, "self_update", return_value=False),
            mock.patch.object(system, "firewall_allow"),
            mock.patch.object(system, "LAUNCH_DAEMONS", daemons_dir),
            mock.patch.object(system, "_reload_daemon_detached", side_effect=lambda lbl, p: reloaded.append((lbl, p))),
            mock.patch.object(cli.mcp_install, "sync_owner_configs", return_value=[]),
            mock.patch.object(cli.os, "execv", side_effect=fake_execv),
        ):
            # 1. Corrupt SHA rejected, stays on Python cleanly
            state["sha"] = "0" * 64
            ns = cli.build_parser().parse_args(["sync"])
            ns.func(ns)
            self.assertFalse(target_bin.exists())
            self.assertEqual(execed, [])

            # 2. Valid SHA downloads, smoke tests, installs, updates sync plist, and re-execs
            state["sha"] = good_sha
            with self.assertRaises(ExecCalled):
                ns.func(ns)
            self.assertTrue(target_bin.is_file())
            self.assertEqual(Path(os.readlink(sym_bin)), target_bin)
            self.assertEqual(execed[0], (str(target_bin), [str(target_bin), "sync", "--no-update"]))
            self.assertEqual(reloaded[0][0], SYNC_LABEL)
            plist = plistlib.loads((daemons_dir / f"{SYNC_LABEL}.plist").read_bytes())
            self.assertEqual(plist["ProgramArguments"], [str(target_bin), "sync"])
            self.assertNotIn("PYTHONPATH", plist["EnvironmentVariables"])

    def test_lighthouse_bridge_missing_go_and_rollback(self) -> None:
        state_dir = self.tmp / "state"
        state_dir.mkdir()
        daemons_dir = self.tmp / "LaunchDaemons"
        daemons_dir.mkdir()
        target_bin = self.tmp / "libexec" / "fleet"
        sym_bin = self.tmp / "brew" / "fleet"
        orig_link_target = self.tmp / "orig-bin" / "fleet"
        orig_link_target.parent.mkdir()
        orig_link_target.write_text("#!/bin/sh\necho orig\n")
        sym_bin.parent.mkdir()
        os.symlink(orig_link_target, sym_bin)

        old_serve_plist = plistlib.dumps({"Label": SERVE_LABEL, "ProgramArguments": ["/old/bin/fleet", "serve"]}).decode()
        (daemons_dir / f"{SERVE_LABEL}.plist").write_text(old_serve_plist)

        with mock.patch.object(system, "find_go_binary", return_value=None):
            status, msg = system.lighthouse_build_and_migrate(
                repo=REPO, st_dir=state_dir, owner="datdo", target_bin=target_bin, link_path=sym_bin, daemons_dir=daemons_dir
            )
            self.assertEqual(status, "missing_go")
            self.assertIn("brew install go", msg)

        if not shutil.which("go"):
            self.skipTest("go toolchain not installed")

        # Unhealthy new serve must rollback binary, plist, symlink, and NOT write manifest.json
        with (
            mock.patch.object(system, "is_root", return_value=True),
            mock.patch.object(system, "firewall_allow"),
            mock.patch.object(system, "sudo"),
            mock.patch.object(system, "launchd_loaded", return_value=False),
            mock.patch.object(system, "LAUNCH_DAEMONS", daemons_dir),
            mock.patch.object(system, "write_root_file", side_effect=lambda p, c, m=0o644: p.write_text(c) is not None),
        ):
            status, detail = system.lighthouse_build_and_migrate(
                repo=REPO,
                st_dir=state_dir,
                owner=os.environ.get("USER") or "datdo",
                target_bin=target_bin,
                link_path=sym_bin,
                daemons_dir=daemons_dir,
                healthcheck_fn=lambda _api: False,
            )
            self.assertEqual(status, "failed")
            self.assertIn("rolled back", detail)
            self.assertFalse(target_bin.exists())
            self.assertEqual(Path(os.readlink(sym_bin)), orig_link_target)
            self.assertEqual((daemons_dir / f"{SERVE_LABEL}.plist").read_text(), old_serve_plist)
            self.assertFalse((state_dir / "bin" / "manifest.json").exists())

            # Healthy new serve publishes manifest.json for both arches, writes valid serve.plist, and triggers detached sync reload
            reloaded_sync: list[tuple[str, Path]] = []
            with mock.patch.object(system, "_reload_daemon_detached", side_effect=lambda lbl, p: reloaded_sync.append((lbl, p))):
                status, _ = system.lighthouse_build_and_migrate(
                    repo=REPO,
                    st_dir=state_dir,
                    owner=os.environ.get("USER") or "datdo",
                    target_bin=target_bin,
                    link_path=sym_bin,
                    daemons_dir=daemons_dir,
                    healthcheck_fn=lambda _api: True,
                )
            self.assertEqual(status, "updated")
            self.assertTrue(target_bin.is_file())
            manifest = json.loads((state_dir / "bin" / "manifest.json").read_text())
            self.assertEqual(sorted(manifest["binaries"].keys()), ["darwin/amd64", "darwin/arm64"])
            self.assertEqual(reloaded_sync[0][0], SYNC_LABEL)
            serve_plist = plistlib.loads((daemons_dir / f"{SERVE_LABEL}.plist").read_bytes())
            self.assertEqual(serve_plist["ProgramArguments"], [str(target_bin), "serve"])
            self.assertEqual(serve_plist["StandardOutPath"], str(state_dir / "serve.log"))
            if shutil.which("plutil"):
                lint = subprocess.run(["plutil", "-lint", str(daemons_dir / f"{SERVE_LABEL}.plist")], capture_output=True, text=True)
                self.assertEqual(lint.returncode, 0, lint.stdout + lint.stderr)

    @unittest.skipUnless(bool(shutil.which("go") and shutil.which("nebula-cert")), "go and nebula-cert required")
    def test_python_and_go_serve_interop_over_shared_sqlite(self) -> None:
        go_bin = self.tmp / "fleet-go"
        build = subprocess.run(["go", "build", "-o", str(go_bin), "./cmd/fleet"], cwd=str(REPO), capture_output=True, text=True)
        self.assertEqual(build.returncode, 0, build.stderr)

        state_dir = self.tmp / "state"
        pki_dir = self.tmp / "etc"
        pki_dir.mkdir()
        _, mini_pub = keypair(self.tmp, "mini")
        fleet = Fleet.initialize(
            root=state_dir,
            host="home.example.com",
            port=443,
            lan_ip="192.168.1.50",
            passphrase=PASSPHRASE,
            lighthouse_pubkey=mini_pub,
        )
        _, lap_pub = keypair(self.tmp, "laptop")
        fleet.approve(name="laptop", pubkey=lap_pub, kind="mac", group="admin", passphrase=PASSPHRASE)
        fleet.agents.heartbeat(device="mini", harness="claude", raw_id="py-sess-1", cwd="/proj", repo="fleet", title="Py Session")
        fleet.agents.heartbeat(device="mini", harness="codex", raw_id="py-sess-2", cwd="/proj", repo="fleet")
        fleet.agents.send_message(device="mini", from_session="mini:claude:py-sess-1", to="mini:codex:py-sess-2", body="from python")
        fleet.close()

        port = free_port()
        env = dict(os.environ, FLEET_STATE_DIR=str(state_dir), FLEET_NEBULA_ETC=str(pki_dir), FLEET_API_PORT=str(port))
        api_url = f"http://127.0.0.1:{port}"
        (pki_dir / "fleet.json").write_text(json.dumps({"name": "mini", "api": api_url}))

        proc = subprocess.Popen([str(go_bin), "serve"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

        def cleanup_proc() -> None:
            if proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=5)
            if proc.stdout:
                proc.stdout.close()
            if proc.stderr:
                proc.stderr.close()

        self.addCleanup(cleanup_proc)

        # Wait for Go server to accept HTTP requests
        ready = False
        for _ in range(50):
            try:
                sync_resp = call(api_url, "GET", "/v1/sync?os=darwin&arch=arm64", headers={"X-Fleet-Client": "python/test123"})
                ready = True
                break
            except Exception:  # noqa: BLE001
                time.sleep(0.1)
        self.assertTrue(ready, "Go fleet serve did not start in time")
        self.assertEqual(sync_resp["name"], "mini")

        # Python client reads devices & agent inbox created in Python DB and served by Go
        devs = call(api_url, "GET", "/v1/devices")["devices"]
        self.assertEqual([d["name"] for d in devs], ["mini", "laptop"])
        self.assertEqual(devs[0]["client_version"], "python/test123")

        inbox = call(api_url, "POST", "/v1/agents/inbox", {"harness": "codex", "session_id": "mini:codex:py-sess-2"})
        self.assertEqual(len(inbox["messages"]), 1)
        self.assertEqual(inbox["messages"][0]["body"], "from python")

        # Go CLI `fleet ls` & `fleet agents` work against Go serve
        ls_proc = subprocess.run([str(go_bin), "ls"], env=env, capture_output=True, text=True)
        self.assertEqual(ls_proc.returncode, 0, ls_proc.stderr)
        self.assertIn("python/test123", ls_proc.stdout)

        ag_proc = subprocess.run([str(go_bin), "agents"], env=env, capture_output=True, text=True)
        self.assertEqual(ag_proc.returncode, 0, ag_proc.stderr)
        self.assertIn("py-session", ag_proc.stdout)

        # Python codes <-> Go binary compatibility: Go `approve` decodes Python `codes.REQUEST`, Python decodes Go `codes.RESPONSE`
        _, off_pub = keypair(self.tmp, "office")
        req_block = codes.encode(codes.REQUEST, {"v": 1, "name": "office", "pubkey": off_pub})
        app_proc = subprocess.run(
            [str(go_bin), "approve", "--group", "managed"],
            env=env,
            input=f"{req_block}\ny\n{PASSPHRASE}\n",
            capture_output=True,
            text=True,
        )
        self.assertEqual(app_proc.returncode, 0, app_proc.stderr)
        decoded = codes.decode(codes.RESPONSE, app_proc.stdout)
        self.assertEqual(decoded["name"], "office")
        self.assertEqual(decoded["ip"], "10.213.0.3")


if __name__ == "__main__":
    unittest.main()

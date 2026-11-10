"""End-to-end tests against the real nebula / nebula-cert binaries (brew install nebula).

Run: python3 -m unittest discover -s tests
"""

from __future__ import annotations

import io
import json
import os
import shutil
import subprocess
import tempfile
import threading
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest import mock

from fleetctl import cli, codes, pki
from fleetctl.client import ApiError, call
from fleetctl.core import Fleet, FleetError
from fleetctl.render import blocklist_yaml, render
from fleetctl.server import make_server

HAVE_NEBULA = bool(shutil.which("nebula") and shutil.which("nebula-cert"))
PASSPHRASE = "correct horse battery staple"


def keypair(directory: Path, name: str) -> tuple[Path, str]:
    key, pub = directory / f"{name}.key", directory / f"{name}.pub"
    pki.keygen(key, pub)
    return key, pub.read_text()


def nebula_test(config: str, pki_dir: Path, ca: str, cert: str, key: Path) -> subprocess.CompletedProcess:
    (pki_dir / "ca.crt").write_text(ca)
    (pki_dir / "host.crt").write_text(cert)
    shutil.copy(key, pki_dir / "host.key")
    (pki_dir / "config.yml").write_text(config)
    return subprocess.run(["nebula", "-test", "-config", str(pki_dir / "config.yml")], capture_output=True, text=True)


class CodesTest(unittest.TestCase):
    def test_roundtrip_wraps_lines_for_terminal_paste(self) -> None:
        payload = {"blob": os.urandom(3000).hex()}
        block = codes.encode(codes.RESPONSE, payload)
        self.assertTrue(all(len(line) <= 64 for line in block.splitlines()[1:-1]))
        noisy = f"some prompt text\n  {block.replace(chr(10), chr(10) + '  ')}  \ntrailing"
        self.assertEqual(codes.decode(codes.RESPONSE, noisy), payload)

    def test_damaged_or_wrong_block(self) -> None:
        block = codes.encode(codes.REQUEST, {"a": 1})
        with self.assertRaises(codes.CodeError):
            codes.decode(codes.RESPONSE, block)
        lines = block.splitlines()
        lines[1] = lines[1][:-4] + "AAAA"
        with self.assertRaises(codes.CodeError):
            codes.decode(codes.REQUEST, "\n".join(lines))

    def test_read_block_stops_at_end_marker(self) -> None:
        stream = io.StringIO("x\n-----END FLEET JOIN REQUEST-----\nnot read\n")
        self.assertNotIn("not read", codes.read_block("END FLEET JOIN REQUEST", stream))


class RenderTest(unittest.TestCase):
    def test_blocklist_entries_are_quoted(self) -> None:
        # An all-digit fingerprint would otherwise become a YAML integer and nebula rejects it.
        self.assertIn('- "0123"', blocklist_yaml(["0123"]))
        self.assertEqual(blocklist_yaml([]).strip(), "blocklist: []")

    def test_all_placeholders_rendered(self) -> None:
        s = {"lighthouse_host": "home.example.com", "lighthouse_port": "443"}
        for kind in ("lighthouse", "mac"):
            self.assertNotRegex(render(kind, s, []), r"@[A-Z_]+@")
        self.assertIn("home.example.com:443", render("mac", s, []))


@unittest.skipUnless(HAVE_NEBULA, "nebula not installed")
class FleetTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.pki_dir = self.tmp / "etc"
        self.pki_dir.mkdir()
        env = mock.patch.dict(os.environ, {"FLEET_NEBULA_ETC": str(self.pki_dir), "FLEET_API_PORT": "7443"})
        env.start()
        self.addCleanup(env.stop)
        self.mini_key, mini_pub = keypair(self.tmp, "mini")
        self.fleet = Fleet.initialize(root=self.tmp / "state", host="home.example.com", port=443,
                                      lan_ip="192.168.1.50", passphrase=PASSPHRASE, lighthouse_pubkey=mini_pub)  # fmt: skip
        self.addCleanup(self.fleet.registry.close)

    def approve(self, name: str, group: str = "admin", kind: str = "mac", replace: bool = False) -> tuple[dict, Path]:
        key, pub = keypair(self.tmp, f"{name}-{os.urandom(2).hex()}")
        resp = self.fleet.approve(name=name, pubkey=pub, kind=kind, group=group, passphrase=PASSPHRASE, replace=replace)
        return resp, key

    def test_initialize_registers_lighthouse_and_refuses_twice(self) -> None:
        mini = self.fleet.registry.lighthouse()
        assert mini is not None
        self.assertEqual((mini.name, mini.ip, mini.grp), ("mini", "10.213.0.1", "lighthouse"))
        self.assertEqual(oct((self.tmp / "state").stat().st_mode & 0o777), "0o700")
        self.assertEqual(oct((self.tmp / "state" / "ca.key").stat().st_mode & 0o777), "0o600")
        self.assertIn("ENCRYPTED", (self.tmp / "state" / "ca.key").read_text())
        with self.assertRaises(FleetError):
            Fleet.initialize(root=self.tmp / "state", host="x", port=1, lan_ip=None, passphrase=PASSPHRASE,
                             lighthouse_pubkey=mini.pubkey)  # fmt: skip

    def test_lighthouse_config_is_valid(self) -> None:
        mini = self.fleet.registry.lighthouse()
        assert mini is not None
        out = nebula_test(self.fleet.config_for(mini) or "", self.pki_dir, self.fleet.ca_pem(), mini.cert, self.mini_key)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("port: 7443", self.fleet.config_for(mini) or "")

    def test_approve_mac_allocates_ip_and_yields_valid_config(self) -> None:
        resp, key = self.approve("laptop")
        self.assertEqual(resp["ip"], "10.213.0.2")
        meta = pki.info(resp["cert"])
        self.assertEqual((meta["groups"], meta["networks"]), (["admin"], ["10.213.0.2/16"]))
        pki.verify(resp["ca"], resp["cert"])
        self.assertEqual(resp["api"], "http://10.213.0.1:7443")
        out = nebula_test(resp["config"], self.pki_dir, resp["ca"], resp["cert"], key)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertEqual(self.approve("office", group="managed")[0]["ip"], "10.213.0.3")

    def test_duplicate_name_needs_replace_which_keeps_ip_and_blocklists_old_cert(self) -> None:
        first, _ = self.approve("laptop")
        with self.assertRaises(FleetError) as ctx:
            self.approve("laptop")
        self.assertEqual(ctx.exception.status, 409)
        second, _ = self.approve("laptop", replace=True)
        self.assertEqual(second["ip"], first["ip"])
        self.assertIn(pki.info(first["cert"])["fingerprint"], self.fleet.registry.blocklist())

    def test_wrong_passphrase_and_bad_input_are_rejected(self) -> None:
        _, pub = keypair(self.tmp, "x")
        with self.assertRaisesRegex(FleetError, "signing failed"):
            self.fleet.approve(name="x", pubkey=pub, kind="mac", group="admin", passphrase="nope")
        self.assertIsNone(self.fleet.registry.get("x"), "failed signing must not register the device")
        for bad in ({"name": "Bad Name"}, {"group": "root"}, {"kind": "server"}, {"pubkey": "garbage"}):
            args = {"name": "ok", "pubkey": pub, "kind": "mac", "group": "admin", "passphrase": PASSPHRASE, **bad}
            with self.assertRaises(FleetError):
                self.fleet.approve(**args)

    def test_mobile_cert_is_one_year_and_has_no_config(self) -> None:
        resp, _ = self.approve("phone", kind="mobile")
        self.assertNotIn("config", resp)
        self.assertEqual(resp["lighthouse"], {"ip": "10.213.0.1", "address": "home.example.com:443"})
        self.assertIn(pki.hours_until(resp["not_after"]), range(8750, 8761))

    def test_identify(self) -> None:
        self.approve("laptop", group="admin")
        self.approve("office", group="managed")
        reg = self.fleet.registry
        self.assertEqual(self.fleet.identify("127.0.0.1").name, "mini")  # type: ignore[union-attr]
        self.assertEqual(self.fleet.identify("10.213.0.2").name, "laptop")  # type: ignore[union-attr]
        self.assertTrue(self.fleet.is_admin(self.fleet.identify("10.213.0.2")))
        self.assertFalse(self.fleet.is_admin(self.fleet.identify("10.213.0.3")))
        self.assertIsNone(self.fleet.identify("10.213.9.9"))  # unregistered mesh address
        self.assertIsNone(self.fleet.identify("192.168.1.20"))  # LAN, not mesh
        self.fleet.revoke("office")
        self.assertIsNone(self.fleet.identify("10.213.0.3"))
        self.assertFalse(reg.get("office").active)  # type: ignore[union-attr]

    def test_revoke_reaches_every_rendered_config_and_ip_is_not_reused(self) -> None:
        resp, _ = self.approve("phone", kind="mobile")
        self.fleet.revoke("phone")
        fp = pki.info(resp["cert"])["fingerprint"]
        mini = self.fleet.registry.lighthouse()
        self.assertIn(f'"{fp}"', self.fleet.config_for(mini) or "")  # type: ignore[arg-type]
        with self.assertRaises(FleetError):
            self.fleet.revoke("phone")
        with self.assertRaises(FleetError):
            self.fleet.revoke("mini")
        self.assertEqual(self.approve("tablet", kind="mobile")[0]["ip"], "10.213.0.3")

    def test_set_group_resigns_and_blocklists_old_cert(self) -> None:
        first, _ = self.approve("laptop", group="admin")
        resp = self.fleet.set_group("laptop", "managed", PASSPHRASE)
        self.assertEqual(pki.info(resp["cert"])["groups"], ["managed"])
        self.assertEqual(resp["ip"], first["ip"])
        self.assertIn(pki.info(first["cert"])["fingerprint"], self.fleet.registry.blocklist())

    # HTTP API --------------------------------------------------------------
    def serve(self) -> str:
        httpd = make_server(self.fleet, "127.0.0.1", 0, quiet=True)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        self.addCleanup(httpd.server_close)
        self.addCleanup(httpd.shutdown)
        return f"http://127.0.0.1:{httpd.server_address[1]}"

    def test_api_flow_over_loopback(self) -> None:
        api = self.serve()
        sync = call(api, "GET", "/v1/sync")
        self.assertEqual((sync["name"], sync["ip"]), ("mini", "10.213.0.1"))
        _, pub = keypair(self.tmp, "laptop")
        resp = call(api, "POST", "/v1/approve",
                    {"name": "laptop", "pubkey": pub, "group": "admin", "passphrase": PASSPHRASE})  # fmt: skip
        self.assertEqual(resp["ip"], "10.213.0.2")
        with self.assertRaisesRegex(ApiError, "HTTP 409"):
            call(api, "POST", "/v1/approve", {"name": "laptop", "pubkey": pub, "passphrase": PASSPHRASE})
        with self.assertRaisesRegex(ApiError, "signing failed.*HTTP 400"):
            call(api, "POST", "/v1/approve", {"name": "other", "pubkey": pub, "passphrase": "nope"})
        names = [d["name"] for d in call(api, "GET", "/v1/devices")["devices"]]
        self.assertEqual(names, ["mini", "laptop"])
        self.assertEqual(call(api, "POST", "/v1/revoke", {"name": "laptop"}), {"ok": True})
        with self.assertRaisesRegex(ApiError, "HTTP 404"):
            call(api, "GET", "/v1/nope")
        self.assertIsNotNone(self.fleet.registry.get("mini").last_seen)  # type: ignore[union-attr]

    def test_api_rejects_non_admins_and_strangers(self) -> None:
        api = self.serve()
        self.approve("office", group="managed")
        office = self.fleet.registry.get("office")
        with mock.patch.object(self.fleet, "identify", return_value=office):
            self.assertEqual(call(api, "GET", "/v1/sync")["name"], "office")  # managed may sync
            with self.assertRaisesRegex(ApiError, "not an admin.*HTTP 403"):
                call(api, "GET", "/v1/devices")
            with self.assertRaisesRegex(ApiError, "HTTP 403"):
                call(api, "POST", "/v1/revoke", {"name": "mini"})
        with mock.patch.object(self.fleet, "identify", return_value=None), \
                self.assertRaisesRegex(ApiError, "not a member.*HTTP 403"):  # fmt: skip
            call(api, "GET", "/v1/sync")

    # CLI ---------------------------------------------------------------------
    def test_cli_approve_turns_request_into_response_for_join(self) -> None:
        api = self.serve()
        (self.pki_dir / "fleet.json").write_text(json.dumps({"name": "mini", "api": api}))
        _, pub = keypair(self.tmp, "laptop")
        request = codes.encode(codes.REQUEST, {"v": 1, "name": "laptop", "pubkey": pub})
        out = io.StringIO()
        with mock.patch("sys.stdin", io.StringIO(request + "\n")), mock.patch("builtins.input", return_value="y"), \
                mock.patch.object(cli, "ask_passphrase", return_value=PASSPHRASE), redirect_stdout(out):  # fmt: skip
            self.assertEqual(cli.main(["approve", "--group", "admin"]), 0)
        resp = codes.decode(codes.RESPONSE, out.getvalue())
        # The checks `fleet join` makes before installing anything:
        self.assertEqual(pki.info(resp["cert"])["public_key"], pki.pubkey_hex(pub))
        pki.verify(resp["ca"], resp["cert"])
        self.assertEqual((resp["name"], resp["ip"], resp["kind"]), ("laptop", "10.213.0.2", "mac"))
        self.assertEqual(resp["ca_id"], self.fleet.ca_id())

    def test_cli_reports_errors_without_traceback(self) -> None:
        err = io.StringIO()
        with mock.patch("sys.stderr", err):
            self.assertEqual(cli.main(["ls"]), 1)  # no fleet.json in the temp nebula dir
        self.assertIn("hasn't joined a fleet", err.getvalue())


if __name__ == "__main__":
    unittest.main()

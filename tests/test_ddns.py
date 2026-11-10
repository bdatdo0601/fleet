"""DDNS logic with fake network/DNS/aws calls (no real AWS or DNS traffic)."""

from __future__ import annotations

import json
import subprocess
import unittest
from unittest import mock

from fleetctl import ddns

CFG = ddns.Route53(name="lighthouse.datbdo.com", zone_id="Z0123ABC", profile="fleet-ddns",
                   nameservers=("ns-1.awsdns.com", "ns-2.awsdns.net"))  # fmt: skip


def fetcher(*answers: str | Exception):
    replies = list(answers)

    def fetch(_url: str) -> str:
        r = replies.pop(0)
        if isinstance(r, Exception):
            raise r
        return r

    return fetch


class FakeAws:
    def __init__(self, returncode: int = 0, stderr: str = ""):
        self.calls: list[list[str]] = []
        self.returncode, self.stderr = returncode, stderr

    def __call__(self, cmd: list[str], **_: object) -> subprocess.CompletedProcess:
        self.calls.append(cmd)
        out = json.dumps({"ChangeInfo": {"Id": "/change/C1", "Status": "PENDING"}})
        return subprocess.CompletedProcess(cmd, self.returncode, out, self.stderr)


class PublicIpTest(unittest.TestCase):
    def test_returns_first_valid_answer_and_falls_back(self) -> None:
        self.assertEqual(ddns.public_ipv4(fetcher("9.9.9.9\n")), "9.9.9.9")
        self.assertEqual(ddns.public_ipv4(fetcher(OSError("down"), "1.1.1.1")), "1.1.1.1")
        self.assertEqual(ddns.public_ipv4(fetcher("<html>", "8.8.8.8")), "8.8.8.8")

    def test_rejects_cgnat_and_private_addresses(self) -> None:
        for addr in ("100.64.1.2", "192.168.1.5", "10.0.0.1"):
            with self.assertRaisesRegex(ddns.DdnsError, "not internet-routable"):
                ddns.public_ipv4(fetcher(addr))

    def test_all_services_failing_is_an_error(self) -> None:
        with self.assertRaisesRegex(ddns.DdnsError, "couldn't determine"):
            ddns.public_ipv4(fetcher(OSError("a"), OSError("b")))


class DnsTest(unittest.TestCase):
    def test_find_nameservers_walks_up_to_the_zone(self) -> None:
        seen = []

        def dig(*args: str) -> list[str]:
            seen.append(args)
            return ["ns-2.awsdns.net.", "ns-1.awsdns.com."] if args[0] == "datbdo.com" else []

        self.assertEqual(ddns.find_nameservers("lighthouse.datbdo.com", dig), ("ns-1.awsdns.com", "ns-2.awsdns.net"))
        self.assertEqual([a[0] for a in seen], ["lighthouse.datbdo.com", "datbdo.com"])

    def test_current_record_asks_authoritative_servers(self) -> None:
        calls = []

        def dig(*args: str) -> list[str]:
            calls.append(args)
            if args[-1] == "@ns-1.awsdns.com":
                raise ddns.DdnsError("timeout")
            return ["9.9.9.9"]

        self.assertEqual(ddns.current_record(CFG, dig), "9.9.9.9")
        self.assertEqual(calls[-1], ("lighthouse.datbdo.com", "A", "@ns-2.awsdns.net"))
        self.assertIsNone(ddns.current_record(CFG, lambda *a: []))
        with self.assertRaisesRegex(ddns.DdnsError, "no authoritative"):
            ddns.current_record(CFG, mock.Mock(side_effect=ddns.DdnsError("x")))


class SyncTest(unittest.TestCase):
    def setUp(self) -> None:
        patcher = mock.patch("shutil.which", return_value="/opt/homebrew/bin/aws")
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_no_update_when_dns_already_matches(self) -> None:
        aws = FakeAws()
        result = ddns.sync(CFG, fetch=fetcher("9.9.9.9"), dig=lambda *a: ["9.9.9.9"], run=aws)
        self.assertEqual(result, ("9.9.9.9", "9.9.9.9", False))
        self.assertEqual(aws.calls, [])

    def test_upserts_one_a_record_when_ip_changed(self) -> None:
        aws = FakeAws()
        result = ddns.sync(CFG, fetch=fetcher("1.1.1.9"), dig=lambda *a: [], run=aws)
        self.assertEqual(result, (None, "1.1.1.9", True))
        cmd = aws.calls[0]
        self.assertEqual(cmd[1:3], ["route53", "change-resource-record-sets"])
        self.assertEqual(cmd[cmd.index("--profile") + 1], "fleet-ddns")
        self.assertEqual(cmd[cmd.index("--hosted-zone-id") + 1], "Z0123ABC")
        batch = json.loads(cmd[cmd.index("--change-batch") + 1])
        self.assertEqual(batch["Changes"], [{"Action": "UPSERT", "ResourceRecordSet": {
            "Name": "lighthouse.datbdo.com", "Type": "A", "TTL": 60,
            "ResourceRecords": [{"Value": "1.1.1.9"}]}}])  # fmt: skip

    def test_aws_failure_is_reported(self) -> None:
        with self.assertRaisesRegex(ddns.DdnsError, "AccessDenied"):
            ddns.sync(CFG, fetch=fetcher("1.1.1.9"), dig=lambda *a: [], run=FakeAws(255, "AccessDenied"))


class PolicyTest(unittest.TestCase):
    def test_policy_is_scoped_to_one_record(self) -> None:
        p = ddns.policy("/hostedzone/Z0123ABC", "Lighthouse.datbdo.com.")
        stmt = p["Statement"][0]
        self.assertEqual(stmt["Resource"], "arn:aws:route53:::hostedzone/Z0123ABC")
        cond = stmt["Condition"]["ForAllValues:StringEquals"]
        self.assertEqual(cond["route53:ChangeResourceRecordSetsNormalizedRecordNames"], ["lighthouse.datbdo.com"])
        self.assertEqual(cond["route53:ChangeResourceRecordSetsRecordTypes"], ["A"])

    def test_zone_id_validation(self) -> None:
        self.assertEqual(ddns.normalize_zone_id(" /hostedzone/Z0123ABC "), "Z0123ABC")
        with self.assertRaises(ddns.DdnsError):
            ddns.normalize_zone_id("datbdo.com")


if __name__ == "__main__":
    unittest.main()

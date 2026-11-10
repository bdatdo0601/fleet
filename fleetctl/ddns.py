"""Keep the lighthouse's DNS name (e.g. lighthouse.example.com) pointed at the home
connection's public IPv4, via an AWS Route 53 A record.

Runs as the admin user (launchd, every 5 minutes). Reads the current answer
straight from the zone's authoritative nameservers (no resolver cache) and only
calls Route 53 when the address actually changed. Needs `aws` (brew install
awscli) and a profile whose IAM policy is `fleet ddns policy`.
"""

from __future__ import annotations

import ipaddress
import json
import shutil
import subprocess
import urllib.request
from dataclasses import dataclass
from typing import Callable

CHECKIP_URLS = ("https://checkip.amazonaws.com", "https://api.ipify.org")
TTL = 60

Dig = Callable[..., "list[str]"]
Fetch = Callable[[str], str]


class DdnsError(RuntimeError):
    pass


@dataclass(frozen=True)
class Route53:
    name: str
    zone_id: str
    profile: str
    nameservers: tuple[str, ...]


def normalize_zone_id(zone_id: str) -> str:
    zid = zone_id.strip().rsplit("/", 1)[-1]
    if not zid.startswith("Z") or not zid.isalnum():
        raise DdnsError(f"{zone_id!r} doesn't look like a Route 53 hosted zone id (Z…)")
    return zid


def policy(zone_id: str, name: str) -> dict:
    """Least-privilege IAM policy: UPSERT this one A record, nothing else in the zone."""
    return {
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Action": "route53:ChangeResourceRecordSets",
                "Resource": f"arn:aws:route53:::hostedzone/{normalize_zone_id(zone_id)}",
                "Condition": {
                    "ForAllValues:StringEquals": {
                        "route53:ChangeResourceRecordSetsNormalizedRecordNames": [name.rstrip(".").lower()],
                        "route53:ChangeResourceRecordSetsRecordTypes": ["A"],
                        "route53:ChangeResourceRecordSetsActions": ["UPSERT"],
                    }
                },
            },
            {"Effect": "Allow", "Action": "route53:GetChange", "Resource": "arn:aws:route53:::change/*"},
        ],
    }


# lookups ---------------------------------------------------------------------


def _fetch(url: str) -> str:
    with urllib.request.urlopen(url, timeout=10) as resp:  # noqa: S310 - fixed https URLs
        return resp.read().decode()


def public_ipv4(fetch: Fetch = _fetch) -> str:
    errors = []
    for url in CHECKIP_URLS:
        try:
            ip = ipaddress.IPv4Address(fetch(url).strip())
        except Exception as e:  # noqa: BLE001 - try the next service
            errors.append(f"{url}: {e}")
            continue
        if not ip.is_global:
            # e.g. 100.64.0.0/10: the ISP's CGNAT. Port forwarding can't work behind it.
            raise DdnsError(f"public address {ip} is not internet-routable (CGNAT?); the lighthouse can't be reached")
        return str(ip)
    raise DdnsError("couldn't determine the public IP: " + "; ".join(errors))


def dig(*args: str) -> list[str]:
    if not shutil.which("dig"):
        raise DdnsError("dig not found")
    proc = subprocess.run(["dig", "+short", "+time=3", "+tries=2", *args], capture_output=True, text=True)
    if proc.returncode != 0:
        raise DdnsError(f"dig {' '.join(args)} failed: {(proc.stdout + proc.stderr).strip()}")
    return [line.strip() for line in proc.stdout.splitlines() if line.strip() and not line.startswith(";")]


def find_nameservers(name: str, dig: Dig = dig) -> tuple[str, ...]:
    """Authoritative nameservers of the zone containing `name` (walks up the labels)."""
    labels = name.rstrip(".").split(".")
    for i in range(len(labels) - 1):
        ns = dig(".".join(labels[i:]), "NS")
        if ns:
            return tuple(sorted(n.rstrip(".") for n in ns))
    raise DdnsError(f"no nameservers found for {name}")


def current_record(cfg: Route53, dig: Dig = dig) -> str | None:
    """The A record as the authoritative servers see it now (None if it doesn't exist)."""
    errors = []
    for ns in cfg.nameservers:
        try:
            answers = dig(cfg.name, "A", f"@{ns}")
        except DdnsError as e:
            errors.append(str(e))
            continue
        for a in answers:
            try:
                return str(ipaddress.IPv4Address(a))
            except ValueError:
                continue  # e.g. a CNAME target
        return None
    raise DdnsError("no authoritative nameserver answered: " + "; ".join(errors))


# update ----------------------------------------------------------------------


def change_batch(name: str, ip: str) -> dict:
    return {
        "Comment": "fleet ddns",
        "Changes": [{
            "Action": "UPSERT",
            "ResourceRecordSet": {"Name": name, "Type": "A", "TTL": TTL, "ResourceRecords": [{"Value": ip}]},
        }],
    }  # fmt: skip


def upsert(cfg: Route53, ip: str, run: Callable[..., subprocess.CompletedProcess] = subprocess.run) -> str:
    aws = shutil.which("aws")
    if not aws:
        raise DdnsError("aws CLI not found (brew install awscli)")
    proc = run(
        [aws, "route53", "change-resource-record-sets", "--profile", cfg.profile, "--hosted-zone-id", cfg.zone_id,
         "--change-batch", json.dumps(change_batch(cfg.name, ip)), "--output", "json"],
        capture_output=True, text=True,
    )  # fmt: skip
    if proc.returncode != 0:
        raise DdnsError(f"Route 53 update failed: {proc.stderr.strip() or proc.stdout.strip()}")
    return json.loads(proc.stdout)["ChangeInfo"]["Id"]


def sync(cfg: Route53, *, fetch: Fetch = _fetch, dig: Dig = dig,
         run: Callable[..., subprocess.CompletedProcess] = subprocess.run) -> tuple[str | None, str, bool]:  # fmt: skip
    """Returns (previous DNS answer, public IP, whether an update was sent)."""
    ip = public_ipv4(fetch)
    before = current_record(cfg, dig)
    if before == ip:
        return before, ip, False
    upsert(cfg, ip, run)
    return before, ip, True

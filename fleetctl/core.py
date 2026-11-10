"""Lighthouse-side operations on the registry. Used by `fleet init` and `fleet serve`."""

from __future__ import annotations

import ipaddress
import re
from pathlib import Path

from . import ddns, pki
from .codes import short_id
from .paths import (
    GROUPS,
    LIGHTHOUSE_IP,
    MESH,
    MOBILE_CERT_HOURS,
    NAME_PATTERN,
    api_port,
    state_dir,
)  # fmt: skip
from .registry import Device, Registry
from .render import render

WRONG_PASSPHRASE = "wrong CA passphrase (the one set during `fleet init`)"  # noqa: S105 - a message


class FleetError(Exception):
    """A request the caller got wrong. `status` is the HTTP status to report."""

    def __init__(self, message: str, status: int = 400):
        super().__init__(message)
        self.status = status


class Fleet:
    def __init__(self, root: Path | None = None):
        self.root = root or state_dir()
        self.ca_key = self.root / "ca.key"
        self.ca_crt = self.root / "ca.crt"
        self.registry = Registry(self.root / "fleet.db")

    # setup ---------------------------------------------------------------
    @classmethod
    def initialize(cls, *, root: Path, host: str, port: int, lan_ip: str | None,
                   passphrase: str, lighthouse_pubkey: str) -> Fleet:  # fmt: skip
        """Create the CA, registry and the lighthouse's own entry."""
        if (root / "fleet.db").exists():
            raise FleetError(f"{root}/fleet.db already exists; this fleet is initialised")
        root.mkdir(mode=0o700, parents=True, exist_ok=True)
        root.chmod(0o700)
        pki.create_ca(root / "ca.key", root / "ca.crt", passphrase)
        (root / "ca.key").chmod(0o600)
        fleet = cls(root)
        (root / "fleet.db").chmod(0o600)
        reg = fleet.registry
        reg.set_setting("lighthouse_host", host)
        reg.set_setting("lighthouse_port", str(port))
        if lan_ip:
            reg.set_setting("lighthouse_lan_ip", lan_ip)
        cert = fleet._sign(lighthouse_pubkey, "mini", LIGHTHOUSE_IP, "lighthouse", passphrase, None)
        meta = pki.info(cert)
        reg.add(name="mini", ip=LIGHTHOUSE_IP, kind="lighthouse", grp="lighthouse", pubkey=lighthouse_pubkey,
                cert=cert, fingerprint=meta["fingerprint"], not_after=meta["not_after"])  # fmt: skip
        return fleet

    # helpers -------------------------------------------------------------
    def settings(self) -> dict[str, str]:
        keys = ("lighthouse_host", "lighthouse_port", "lighthouse_lan_ip")
        return {k: v for k in keys if (v := self.registry.setting(k)) is not None}

    def ddns_config(self) -> ddns.Route53 | None:
        zone = self.registry.setting("ddns_zone_id")
        if not zone:
            return None
        return ddns.Route53(
            name=self.registry.setting("ddns_name") or "",
            zone_id=zone,
            profile=self.registry.setting("ddns_profile") or "",
            nameservers=tuple((self.registry.setting("ddns_nameservers") or "").split(",")),
        )

    def set_ddns(self, cfg: ddns.Route53) -> None:
        self.registry.set_setting("ddns_name", cfg.name)
        self.registry.set_setting("ddns_zone_id", cfg.zone_id)
        self.registry.set_setting("ddns_profile", cfg.profile)
        self.registry.set_setting("ddns_nameservers", ",".join(cfg.nameservers))

    def ca_pem(self) -> str:
        return self.ca_crt.read_text()

    def ca_id(self) -> str:
        return short_id(pki.info(self.ca_pem())["fingerprint"])

    def config_for(self, dev: Device) -> str | None:
        if dev.kind == "mobile":
            return None
        return render(dev.kind, self.settings(), self.registry.blocklist())

    def _sign(self, pubkey: str, name: str, ip: str, group: str, passphrase: str, hours: int | None) -> str:
        if not passphrase:
            raise FleetError("CA passphrase required")
        try:
            return pki.sign(ca_key=self.ca_key, ca_crt=self.ca_crt, pubkey=pubkey, name=name, ip=ip,
                            group=group, passphrase=passphrase, hours=hours)  # fmt: skip
        except pki.PkiError as e:
            if "invalid passphrase" in str(e):
                raise FleetError(WRONG_PASSPHRASE, 401) from e
            raise FleetError(f"signing failed: {e}") from e

    def _mobile_hours(self) -> int:
        # A cert can't outlive its CA; nebula-cert refuses instead of clamping.
        return max(1, min(MOBILE_CERT_HOURS, pki.hours_until(pki.info(self.ca_pem())["not_after"]) - 1))

    # identity ------------------------------------------------------------
    def identify(self, peer_ip: str) -> Device | None:
        """Who is calling the API. Nebula guarantees a mesh source IP matches the
        sender's certificate; loopback is the lighthouse itself."""
        try:
            addr = ipaddress.ip_address(peer_ip)
        except ValueError:
            return None
        if addr.is_loopback:
            return self.registry.lighthouse()
        if addr in MESH:
            dev = self.registry.by_ip(str(addr))
            return dev if dev and dev.active else None
        return None

    @staticmethod
    def is_admin(dev: Device | None) -> bool:
        return dev is not None and dev.grp in ("admin", "lighthouse")

    # operations ----------------------------------------------------------
    def sync(self, dev: Device) -> dict:
        self.registry.touch(dev.name)
        return {"name": dev.name, "ip": dev.ip, "kind": dev.kind, "ca": self.ca_pem(),
                "cert": dev.cert, "config": self.config_for(dev)}  # fmt: skip

    def approve(self, *, name: str, pubkey: str, kind: str, group: str, passphrase: str,
                replace: bool = False) -> dict:  # fmt: skip
        if not re.match(NAME_PATTERN, name):
            raise FleetError("name must be lowercase letters, digits and dashes (max 32)")
        if kind not in ("mac", "mobile"):
            raise FleetError(f"unknown kind {kind!r}")
        if group not in GROUPS:
            raise FleetError(f"group must be one of {', '.join(GROUPS)}")
        try:
            pubkey = pki.normalize_pubkey(pubkey)
        except pki.PkiError as e:
            raise FleetError(str(e)) from e

        with self.registry.lock:
            existing = self.registry.get(name)
            if existing and existing.kind == "lighthouse":
                raise FleetError("the lighthouse can't be re-approved", 409)
            if existing and not replace:
                raise FleetError(f"{name} already exists ({existing.ip}); pass --replace to re-key it", 409)
            ip = existing.ip if existing else self.registry.next_ip()
            hours = self._mobile_hours() if kind == "mobile" else None
            cert = self._sign(pubkey, name, ip, group, passphrase, hours)
            meta = pki.info(cert)
            if existing:
                self.registry.replace_cert(name=name, grp=group, pubkey=pubkey, cert=cert,
                                           fingerprint=meta["fingerprint"], not_after=meta["not_after"],
                                           reason="replaced")  # fmt: skip
            else:
                self.registry.add(name=name, ip=ip, kind=kind, grp=group, pubkey=pubkey, cert=cert,
                                  fingerprint=meta["fingerprint"], not_after=meta["not_after"])  # fmt: skip
            dev = self.registry.get(name)
            assert dev is not None

        result = {"name": name, "ip": ip, "kind": kind, "group": group, "ca": self.ca_pem(), "cert": cert,
                  "ca_id": self.ca_id(), "not_after": meta["not_after"]}  # fmt: skip
        if kind == "mac":
            result["config"] = self.config_for(dev)
            result["api"] = f"http://{LIGHTHOUSE_IP}:{api_port()}"
        else:
            s = self.settings()
            result["lighthouse"] = {"ip": LIGHTHOUSE_IP, "address": f"{s['lighthouse_host']}:{s['lighthouse_port']}"}
        return result

    def set_group(self, name: str, group: str, passphrase: str) -> dict:
        dev = self._existing(name)
        return self.approve(name=name, pubkey=dev.pubkey, kind=dev.kind, group=group,
                            passphrase=passphrase, replace=True)  # fmt: skip

    def revoke(self, name: str) -> None:
        dev = self._existing(name)
        if dev.kind == "lighthouse":
            raise FleetError("the lighthouse can't be revoked", 409)
        if not dev.active:
            raise FleetError(f"{name} is already revoked", 409)
        self.registry.revoke(name)

    def list(self) -> list[dict]:
        return [{"name": d.name, "ip": d.ip, "kind": d.kind, "group": d.grp, "not_after": d.not_after,
                 "last_seen": d.last_seen, "revoked_at": d.revoked_at} for d in self.registry.devices()]  # fmt: skip

    def _existing(self, name: str) -> Device:
        dev = self.registry.get(name)
        if dev is None:
            raise FleetError(f"no device named {name}", 404)
        return dev

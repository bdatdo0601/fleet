"""`fleet` command line."""

from __future__ import annotations

import argparse
import getpass
import json
import os
import pwd
import shutil
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path

from . import codes, ddns, pki, system
from .client import ApiError, call
from .core import Fleet, FleetError
from .paths import (
    DDNS_LABEL,
    LIGHTHOUSE_IP,
    NEBULA_LABEL,
    SERVE_LABEL,
    SYNC_LABEL,
    api_port,
    nebula_etc,
    node_file,
    pending_dir,
    state_dir,
)  # fmt: skip


class UserError(Exception):
    pass


def info(msg: str) -> None:
    print(f"==> {msg}")


def current_user() -> str:
    return pwd.getpwuid(os.getuid()).pw_name


def require_not_root(cmd: str) -> None:
    if system.is_root():
        raise UserError(f"run `fleet {cmd}` as your normal user (it uses sudo where needed)")


def load_node() -> dict:
    try:
        return json.loads(node_file().read_text())
    except FileNotFoundError:
        raise UserError("this machine hasn't joined a fleet (fleet join NAME)") from None


def api() -> str:
    return load_node()["api"]


def confirm(question: str) -> None:
    if input(f"{question} [y/N] ").strip().lower() not in ("y", "yes"):
        raise UserError("cancelled")


def ask_passphrase(new: bool = False) -> str:
    pw = getpass.getpass("CA passphrase: ")
    if new:
        if len(pw) < 12:
            raise UserError("use at least 12 characters")
        if getpass.getpass("Repeat passphrase: ") != pw:
            raise UserError("passphrases don't match")
    return pw


def paste(prompt: str, end_marker: str) -> str:
    if sys.stdin.isatty():
        print(prompt)
    text = codes.read_block(end_marker)
    if end_marker not in text:
        raise UserError("input ended before the end of the block")
    return text


def install_node_files(*, key_pem: str | None, ca: str, cert: str, config: str, node: dict) -> None:
    etc = nebula_etc()
    if key_pem is not None:
        system.write_root_file(etc / "host.key", key_pem, 0o600)
    system.write_root_file(etc / "ca.crt", ca)
    system.write_root_file(etc / "host.crt", cert)
    system.write_root_file(etc / "config.yml", config)
    system.write_root_file(node_file(), json.dumps(node, indent=2) + "\n")


def check_lighthouse() -> None:
    info(f"checking the lighthouse ({LIGHTHOUSE_IP}) ...")
    ok = subprocess.run(["ping", "-c", "3", "-t", "15", LIGHTHOUSE_IP], capture_output=True).returncode == 0
    print("    reachable" if ok else "    not reachable yet; see `fleet status` and /opt/homebrew/var/log/nebula.log")


# commands -------------------------------------------------------------------


def cmd_init(a: argparse.Namespace) -> None:
    require_not_root("init")
    root = state_dir()
    if (root / "fleet.db").exists():
        raise UserError(f"{root}/fleet.db exists: this machine is already the lighthouse")
    system.ensure_nebula()
    dns = ddns_preflight(a.host, a.route53_zone, a.aws_profile) if a.route53_zone else None
    info("choose the CA passphrase. You'll type it whenever you approve a device. Store it in your password manager.")
    passphrase = ask_passphrase(new=True)

    with tempfile.TemporaryDirectory() as tmp:
        key, pub = Path(tmp, "host.key"), Path(tmp, "host.pub")
        pki.keygen(key, pub)
        fleet = Fleet.initialize(root=root, host=a.host, port=a.port, lan_ip=a.lan_ip, passphrase=passphrase,
                                 lighthouse_pubkey=pub.read_text())  # fmt: skip
        info("CA, registry and lighthouse certificate created in " + str(root))
        mini = fleet.registry.lighthouse()
        assert mini is not None
        install_node_files(key_pem=key.read_text(), ca=fleet.ca_pem(), cert=mini.cert,
                           config=fleet.config_for(mini) or "",
                           node={"name": mini.name, "api": f"http://127.0.0.1:{api_port()}"})  # fmt: skip

    info("installing launchd services (fleet serve, fleet sync) ...")
    system.firewall_allow(sys.executable)  # fleet serve accepts mesh connections on the API port
    system.install_serve_daemon(current_user())
    system.install_sync_daemon()
    info("starting nebula ...")
    system.start_nebula()
    if dns:
        try:
            ddns_activate(fleet, dns)
        except (ddns.DdnsError, UserError) as e:
            print(f"warning: DNS not set up: {e}\n         fix it, then run: fleet ddns setup --zone-id {dns.zone_id}")
    print(f"""
Lighthouse ready.
  CA id:        {fleet.ca_id()}   (join codes show this id; check that it matches)
  Public entry: {a.host}:{a.port}/udp  -> forward this UDP port on the router to this Mac
  Back up:      {root}/ca.key and the passphrase (e.g. 1Password)

Next, register your laptop. On the laptop:   fleet join laptop
                       Then on this Mac:     fleet approve --group admin""")


def cmd_join(a: argparse.Namespace) -> None:
    require_not_root("join")
    if node_file().exists() and not a.force:
        raise UserError(f"already joined as {load_node()['name']} (use --force to re-join)")
    system.ensure_nebula()

    pend = pending_dir()
    pend.mkdir(mode=0o700, parents=True, exist_ok=True)
    key, pub = pend / f"{a.name}.key", pend / f"{a.name}.pub"
    if not key.exists():
        pki.keygen(key, pub)
        key.chmod(0o600)
    pubkey = pki.normalize_pubkey(pub.read_text())
    request = codes.encode(codes.REQUEST, {"v": 1, "name": a.name, "pubkey": pubkey})

    print(f"\n{request}\n")
    info(f"request id: {codes.short_id(pubkey)}")
    print("    On an admin device (or the lighthouse) run `fleet approve` and paste the block above.")
    print("    Re-running `fleet join` prints the same request until it's approved.\n")

    resp = codes.decode(codes.RESPONSE, paste("Paste the FLEET JOIN RESPONSE block:", f"END {codes.RESPONSE}"))
    if resp.get("kind") != "mac":
        raise UserError("that response is for a mobile device")
    cert_info = pki.info(resp["cert"])
    if cert_info["public_key"] != pki.pubkey_hex(pubkey):
        raise UserError("that response is for a different request (public key mismatch)")
    pki.verify(resp["ca"], resp["cert"])
    info(f"approved as {resp['name']} ({resp['ip']}), CA id {resp['ca_id']}")

    install_node_files(key_pem=key.read_text(), ca=resp["ca"], cert=resp["cert"], config=resp["config"],
                       node={"name": resp["name"], "api": resp["api"]})  # fmt: skip
    system.install_sync_daemon()
    system.start_nebula()
    key.unlink()
    pub.unlink(missing_ok=True)
    check_lighthouse()
    print("\nJoined. Config and revocations now sync automatically every 5 minutes.")


def _approve(payload: dict) -> dict:
    payload["passphrase"] = ask_passphrase()
    return call(api(), "POST", "/v1/approve", payload)


def cmd_approve(a: argparse.Namespace) -> None:
    req = codes.decode(codes.REQUEST, paste("Paste the FLEET JOIN REQUEST block:", f"END {codes.REQUEST}"))
    name = a.name or req["name"]
    print(f"\n  device:     {name}\n  request id: {codes.short_id(req['pubkey'])}  (must match the joining machine)")
    print(f"  group:      {a.group}{'  (replacing existing cert)' if a.replace else ''}")
    confirm("Approve?")
    resp = _approve({"name": name, "pubkey": req["pubkey"], "kind": "mac", "group": a.group, "replace": a.replace})
    fields = ("name", "ip", "kind", "ca", "cert", "config", "api", "ca_id")
    block = codes.encode(codes.RESPONSE, {k: resp[k] for k in fields})
    print(f"\n{block}\n")
    info(f"{name} -> {resp['ip']}. Paste the block above into the waiting `fleet join`.")


def cmd_approve_mobile(a: argparse.Namespace) -> None:
    pem = paste("Paste the public key from the Mobile Nebula app:", "END NEBULA X25519 PUBLIC KEY")
    pubkey = pki.normalize_pubkey(pem)
    print(f"\n  device: {a.name} (mobile)\n  group:  {a.group}\n  key id: {codes.short_id(pubkey)}")
    confirm("Approve?")
    resp = _approve({"name": a.name, "pubkey": pubkey, "kind": "mobile", "group": a.group, "replace": a.replace})
    print_mobile(resp)


def print_mobile(resp: dict) -> None:
    print(f"\n--- Certificate (paste into the app) ---\n{resp['cert'].strip()}\n")
    print(f"--- CA (paste into the app) ---\n{resp['ca'].strip()}\n")
    lh = resp["lighthouse"]
    print(f"--- Hosts ---\n  {lh['ip']}  lighthouse: yes  address: {lh['address']}\n")
    info(f"{resp['name']} -> {resp['ip']}, cert expires {resp['not_after']}")


def cmd_ls(_: argparse.Namespace) -> None:
    data = call(api(), "GET", "/v1/devices")
    rows = [("NAME", "IP", "KIND", "GROUP", "LAST SEEN", "EXPIRES", "STATUS")]
    for d in data["devices"]:
        rows.append((d["name"], d["ip"], d["kind"], d["group"], ago(d["last_seen"]), d["not_after"][:10],
                     "revoked" if d["revoked_at"] else "active"))  # fmt: skip
    widths = [max(len(r[i]) for r in rows) for i in range(len(rows[0]))]
    for r in rows:
        print("  ".join(c.ljust(w) for c, w in zip(r, widths)).rstrip())
    print(f"\nCA id {data['ca_id']}")


def ago(ts: str | None) -> str:
    if not ts:
        return "never"
    secs = int((datetime.now(timezone.utc) - datetime.fromisoformat(ts)).total_seconds())
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60)):
        if secs >= size:
            return f"{secs // size}{unit} ago"
    return "just now"


def cmd_revoke(a: argparse.Namespace) -> None:
    confirm(f"Revoke {a.name}? It loses access everywhere within ~5 minutes.")
    call(api(), "POST", "/v1/revoke", {"name": a.name})
    info(f"revoked {a.name}. Also remove its SSH key from authorized_keys on machines it could reach.")


def cmd_set(a: argparse.Namespace) -> None:
    resp = call(api(), "POST", "/v1/set-group", {"name": a.name, "group": a.group, "passphrase": ask_passphrase()})
    if resp["kind"] == "mobile":
        print_mobile(resp)
        info("replace the certificate in the app; the old one is now revoked")
    else:
        info(f"{a.name} is now {a.group}; it picks up its new certificate on its next sync (<= 5 min)")


def cmd_sync(a: argparse.Namespace) -> None:
    if not system.is_root():
        raise UserError("fleet sync runs as root (launchd does this every 5 minutes; or: sudo fleet sync)")
    node = load_node()
    stamp = datetime.now().isoformat(timespec="seconds")
    if not a.no_update and system.self_update():
        print(f"{stamp} code updated from git")
        if node["api"].startswith("http://127.0.0.1"):
            system.restart(SERVE_LABEL)
            time.sleep(2)  # let the restarted server bind before we call it
    deadline = time.monotonic() + (a.wait if a.wait else 0)
    while True:
        try:
            payload = call(node["api"], "GET", "/v1/sync", timeout=15)
            break
        except ApiError as e:
            if "HTTP 403" in str(e):
                raise UserError(f"the lighthouse refused this device: {e}") from e
            if time.monotonic() >= deadline:
                print(f"{stamp} lighthouse unreachable, keeping current config: {e}")
                return
            time.sleep(1)
    changed = [p.name for p, content in (
        (nebula_etc() / "ca.crt", payload["ca"]),
        (nebula_etc() / "host.crt", payload["cert"]),
        (nebula_etc() / "config.yml", payload["config"]),
    ) if system.write_root_file(p, content)]  # fmt: skip
    system.firewall_allow(shutil.which("nebula"))  # re-apply: brew upgrades move the binary
    if changed and system.launchd_loaded(NEBULA_LABEL):
        system.restart(NEBULA_LABEL)
    print(f"{stamp} synced {payload['name']}: {'updated ' + ', '.join(changed) if changed else 'no changes'}")


def cmd_serve(_: argparse.Namespace) -> None:
    from .server import serve

    serve(api_port())


def cmd_status(_: argparse.Namespace) -> None:
    node = load_node()
    print(f"device:      {node['name']}")
    print(f"api:         {node['api']}")
    for label in (NEBULA_LABEL, SYNC_LABEL, SERVE_LABEL, DDNS_LABEL):
        if label in (SERVE_LABEL, DDNS_LABEL) and not node["api"].startswith("http://127.0.0.1"):
            continue
        print(f"{label + ':':<44} {'loaded' if system.launchd_loaded(label) else 'NOT loaded'}")
    try:
        me = call(node["api"], "GET", "/v1/sync", timeout=10)
        print(f"lighthouse:  reachable, this device is {me['ip']}")
    except ApiError as e:
        print(f"lighthouse:  {e}")


# ddns -----------------------------------------------------------------------------


def ddns_preflight(name: str, zone_id: str, profile: str) -> ddns.Route53:
    """Fail before anything is created if the AWS side isn't ready."""
    zid = ddns.normalize_zone_id(zone_id)
    if not shutil.which("aws"):
        raise UserError("aws CLI not found: brew install awscli")
    profiles = subprocess.run(["aws", "configure", "list-profiles"], capture_output=True, text=True).stdout.split()
    if profile not in profiles:
        raise UserError(
            f"no AWS profile {profile!r}. Create an IAM user whose only policy is the output of\n"
            f"  fleet ddns policy --zone-id {zid} --name {name}\n"
            f"then store its access key with: aws configure --profile {profile}"
        )
    return ddns.Route53(name=name.rstrip("."), zone_id=zid, profile=profile, nameservers=ddns.find_nameservers(name))


def ddns_activate(fleet: Fleet, cfg: ddns.Route53) -> None:
    """Prove the credentials work with a real update, then save + schedule."""
    before, ip, changed = ddns.sync(cfg)
    fleet.set_ddns(cfg)
    system.install_ddns_daemon(current_user())
    state = f"updated {before or 'none'} -> {ip}" if changed else f"already {ip}"
    info(f"DNS {cfg.name}: {state}; re-checked every 5 minutes")


def cmd_ddns_setup(a: argparse.Namespace) -> None:
    require_not_root("ddns setup")
    fleet = Fleet()
    host = fleet.settings().get("lighthouse_host")
    name = a.name or host
    if not name:
        raise UserError("run on the lighthouse after `fleet init`, or pass --name")
    if host and name.rstrip(".") != host:
        print(f"warning: devices connect to {host}, not {name}")
    ddns_activate(fleet, ddns_preflight(name, a.zone_id, a.profile))


def cmd_ddns_run(_: argparse.Namespace) -> None:
    cfg = Fleet().ddns_config()
    if cfg is None:
        raise UserError("DNS updates aren't configured (fleet ddns setup)")
    stamp = datetime.now().isoformat(timespec="seconds")
    try:
        before, ip, changed = ddns.sync(cfg)
    except ddns.DdnsError as e:
        raise UserError(f"{stamp} {e}") from e
    if changed:
        print(f"{stamp} {cfg.name}: {before or 'none'} -> {ip}")


def cmd_ddns_status(_: argparse.Namespace) -> None:
    cfg = Fleet().ddns_config()
    if cfg is None:
        raise UserError("DNS updates aren't configured (fleet ddns setup)")
    print(f"record:      {cfg.name} (zone {cfg.zone_id}, profile {cfg.profile})")
    print(f"public IP:   {ddns.public_ipv4()}")
    print(f"DNS answer:  {ddns.current_record(cfg) or 'no record'}  (from {cfg.nameservers[0]})")
    print(f"{DDNS_LABEL + ':':<44} {'loaded' if system.launchd_loaded(DDNS_LABEL) else 'NOT loaded'}")


def cmd_ddns_policy(a: argparse.Namespace) -> None:
    print(json.dumps(ddns.policy(a.zone_id, a.name), indent=2))


# entry point -------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="fleet", description="Self-hosted Nebula fleet")
    sub = p.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("init", help="(lighthouse, once) create CA + registry, start services")
    s.add_argument("--host", required=True, help="public DDNS name of the home connection")
    s.add_argument("--port", type=int, default=443, help="UDP port forwarded to this Mac (default 443)")
    s.add_argument("--lan-ip", help="this Mac's reserved LAN IP (fallback for routers without NAT loopback)")
    s.add_argument("--route53-zone", metavar="ZONE_ID", help="keep --host pointed at this network via Route 53")
    s.add_argument("--aws-profile", default="fleet-ddns", help="AWS profile for DNS updates (default fleet-ddns)")
    s.set_defaults(func=cmd_init)

    s = sub.add_parser("join", help="(new Mac) request to join, then paste the approval")
    s.add_argument("name")
    s.add_argument("--force", action="store_true", help="re-join even if already joined")
    s.set_defaults(func=cmd_join)

    s = sub.add_parser("approve", help="(admin) approve a pasted join request")
    s.add_argument("--group", choices=("admin", "managed"), default="managed")
    s.add_argument("--name", help="override the requested name")
    s.add_argument("--replace", action="store_true", help="re-key an existing device, keeping its IP")
    s.set_defaults(func=cmd_approve)

    s = sub.add_parser("approve-mobile", help="(admin) sign a phone/tablet key from Mobile Nebula")
    s.add_argument("name")
    s.add_argument("--group", choices=("admin", "managed"), default="admin")
    s.add_argument("--replace", action="store_true")
    s.set_defaults(func=cmd_approve_mobile)

    sub.add_parser("ls", help="(admin) list devices").set_defaults(func=cmd_ls)

    s = sub.add_parser("revoke", help="(admin) revoke a device")
    s.add_argument("name")
    s.set_defaults(func=cmd_revoke)

    s = sub.add_parser("set", help="(admin) change a device's group (re-signs its cert)")
    s.add_argument("name")
    s.add_argument("--group", choices=("admin", "managed"), required=True)
    s.set_defaults(func=cmd_set)

    s = sub.add_parser("sync", help="(root, launchd) pull config/certs/revocations and code")
    s.add_argument("--no-update", action="store_true", help="skip git self-update")
    s.add_argument("--wait", type=int, default=0, metavar="SECONDS", help="retry until the lighthouse answers")
    s.set_defaults(func=cmd_sync)

    sub.add_parser("serve", help="(lighthouse, launchd) run the API").set_defaults(func=cmd_serve)

    d = sub.add_parser("ddns", help="(lighthouse) keep the public DNS name pointed at home").add_subparsers(
        dest="ddns_cmd", required=True)
    s = d.add_parser("setup", help="verify AWS access, update the record now, schedule every 5 minutes")
    s.add_argument("--zone-id", required=True, help="Route 53 hosted zone id (Z…)")
    s.add_argument("--profile", default="fleet-ddns", help="AWS profile (default fleet-ddns)")
    s.add_argument("--name", help="record to manage (default: the lighthouse host from fleet init)")
    s.set_defaults(func=cmd_ddns_setup)
    d.add_parser("run", help="(launchd) update the record if the public IP changed").set_defaults(func=cmd_ddns_run)
    d.add_parser("status", help="public IP vs. what DNS answers").set_defaults(func=cmd_ddns_status)
    s = d.add_parser("policy", help="print the least-privilege IAM policy for the DNS updater")
    s.add_argument("--zone-id", required=True)
    s.add_argument("--name", required=True, help="e.g. lighthouse.datbdo.com")
    s.set_defaults(func=cmd_ddns_policy)
    sub.add_parser("status", help="show this machine's fleet status").set_defaults(func=cmd_status)
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        args.func(args)
    except (UserError, FleetError, ApiError, pki.PkiError, codes.CodeError, ddns.DdnsError) as e:
        print(f"error: {e}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("\ncancelled", file=sys.stderr)
        return 130
    except subprocess.CalledProcessError as e:
        print(f"error: command failed: {' '.join(map(str, e.cmd))}", file=sys.stderr)
        return 1
    return 0

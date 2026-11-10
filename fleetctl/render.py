"""Render a device's Nebula config from templates/ plus registry settings."""

from __future__ import annotations

import re
from pathlib import Path

from .paths import LIGHTHOUSE_IP, TEMPLATES, api_port, nebula_etc

TEMPLATE_FOR_KIND = {"lighthouse": "lighthouse.yml.tmpl", "mac": "node.yml.tmpl"}
PLACEHOLDER = re.compile(r"@[A-Z_]+@")


def blocklist_yaml(fingerprints: list[str]) -> str:
    if not fingerprints:
        return "  blocklist: []"
    # Quoted: an all-digit fingerprint would otherwise parse as a YAML integer.
    return "\n".join(["  blocklist:", *(f'    - "{fp}"' for fp in fingerprints)])


def render(kind: str, settings: dict[str, str], blocklist: list[str], pki_dir: Path | None = None) -> str:
    tmpl = TEMPLATE_FOR_KIND.get(kind)
    if tmpl is None:
        raise ValueError(f"no Nebula config for kind {kind!r}")
    text = (TEMPLATES / tmpl).read_text()
    values = {
        "NEBULA_ETC": str(pki_dir or nebula_etc()),
        "LIGHTHOUSE_IP": LIGHTHOUSE_IP,
        "LIGHTHOUSE_HOST": settings["lighthouse_host"],
        "LIGHTHOUSE_PORT": settings["lighthouse_port"],
        "LIGHTHOUSE_LAN_IP": settings.get("lighthouse_lan_ip") or settings["lighthouse_host"],
        "API_PORT": str(api_port()),
        "BLOCKLIST": blocklist_yaml(blocklist),
    }
    for key, value in values.items():
        text = text.replace(f"@{key}@", value)
    leftovers = sorted(set(PLACEHOLDER.findall(text)))
    if leftovers:
        raise ValueError(f"unrendered placeholders in {tmpl}: {leftovers}")
    return text

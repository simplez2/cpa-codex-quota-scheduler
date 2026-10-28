"""Fail CI when published metadata or a pinned host patch is inconsistent."""

import hashlib
import json
import os
import re
from pathlib import Path


root = Path(__file__).resolve().parents[2]
version = re.search(r'pluginVersion\s*=\s*"([0-9]+\.[0-9]+\.[0-9]+)"',
                    (root / "main.go").read_text(encoding="utf-8")).group(1)
if os.environ.get("RELEASE_TAG", "") not in ("", "v" + version):
    raise SystemExit("release tag does not match pluginVersion")
registry = json.loads((root / "registry.json").read_text(encoding="utf-8"))
entry = next(p for p in registry["plugins"] if p["id"] == "codex-quota-scheduler")
if entry["version"] != version:
    raise SystemExit("registry version does not match pluginVersion")
if f"Source version: **{version}**" not in (root / "README.md").read_text(encoding="utf-8"):
    raise SystemExit("README source version does not match pluginVersion")
if not (root / "SERIAL_CONFIG.example.yaml").read_text(encoding="utf-8").startswith(
        f"# Standalone CPA quota scheduler v{version}."):
    raise SystemExit("example config version does not match pluginVersion")
for name in ("host-patch.json", "continuity-host-patch.json", "continuity-host-patch-v8.0.44.json"):
    manifest = json.loads((root / "integration" / name).read_text(encoding="utf-8"))
    patch = root / "integration" / manifest.get(
        "patch_file", "CLIProxyAPI-v7.2.152-quota-failover.patch")
    if hashlib.sha256(patch.read_bytes()).hexdigest() != manifest["patch_sha256"]:
        raise SystemExit(f"host patch checksum mismatch: {name}")
print(f"Release metadata and three host patch checksums verified: {version}")

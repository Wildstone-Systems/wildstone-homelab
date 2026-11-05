#!/usr/bin/env python3
"""Generate bounded management metadata; never infer upgrade compatibility."""
import argparse
import hashlib
import json
from pathlib import Path
import re

REPOSITORY = "Wildstone-Systems/wildstone-homelab"
IMAGE = "ghcr.io/wildstone-systems/wildstone-management"
VERSION = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?")


def version(value):
    match = VERSION.fullmatch(value)
    if not match or any(p.isdigit() and len(p) > 1 and p[0] == "0" for p in (match[4] or "").split(".")):
        raise ValueError("release must be a canonical v-prefixed SemVer without build metadata")
    return match


def generate(root, tag, commit, digest):
    target = version(tag)
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("full source commit required")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError("immutable image index digest required")
    policy = json.loads((root / "release/management-policy.json").read_text())
    if set(policy) != {"upgradeFrom", "wardstoneCompatibility", "recovery"}:
        raise ValueError("unknown or missing policy field")
    predecessors = policy["upgradeFrom"]
    # Initial publication is discovery-only. Enabling upgrades requires a
    # separate reviewed compatibility/evidence contract, not a config switch.
    if predecessors != [] or policy["wardstoneCompatibility"] != "unverified":
        raise ValueError("upgrade eligibility is not implemented in manifest v1")
    if policy["recovery"] != {"backupRequired": True, "imageRollbackSupported": False}:
        raise ValueError("unproven recovery policy")
    chart = root / "charts/wildstone-management"
    chart_version = re.search(r"^version: ([0-9]+\.[0-9]+\.[0-9]+)$", (chart / "Chart.yaml").read_text(), re.M)[1]
    # Bind every chart source file without non-deterministic archive timestamps.
    chart_files = {p.relative_to(root).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
                   for p in sorted(chart.rglob("*")) if p.is_file()}
    migrations = {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                  for p in sorted((root / "keystone/runtimedb/migrations").glob("*.sql"))}
    if not chart_files or not migrations:
        raise ValueError("chart and schema inventory required")
    return {
        "schemaVersion": 1,
        "product": "wildstone-management",
        "release": tag,
        "channel": "preview" if target[4] else "stable",
        "source": {"repository": REPOSITORY, "commit": commit},
        "image": {"repository": IMAGE, "digest": digest, "platforms": ["linux/amd64", "linux/arm64"]},
        "chart": {"path": "charts/wildstone-management", "version": chart_version, "files": chart_files},
        "database": {"migrationFiles": migrations},
        "compatibility": policy,
        "upgradeEnabled": False,
        "releaseNotes": f"https://github.com/{REPOSITORY}/releases/tag/{tag}",
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--digest", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        manifest = generate(Path(__file__).resolve().parent.parent, args.tag, args.commit, args.digest)
    except (ValueError, KeyError, TypeError) as error:
        parser.error(str(error))
    args.output.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")

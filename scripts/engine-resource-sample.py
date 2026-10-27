#!/usr/bin/env python3
"""Read-only idle resource sample for W050 #159 and #129.

Run once on a Docker-only baseline and once after k3s/Longhorn settles on
an otherwise comparable VM. Output contains no addresses or credentials.
"""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import platform
import statistics
import subprocess
import time


def command(*args):
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=15, check=False)
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return ""
    return result.stdout.strip() if result.returncode == 0 else ""


def meminfo():
    values = {}
    for line in Path("/proc/meminfo").read_text().splitlines():
        key, _, value = line.partition(":")
        if key in ("MemTotal", "MemAvailable"):
            values[key] = int(value.split()[0]) * 1024
    return values


def cpu_ticks():
    fields = Path("/proc/stat").read_text().splitlines()[0].split()
    ticks = [int(value) for value in fields[1:9]]
    return sum(ticks), ticks[3] + ticks[4]


def tree_bytes(path):
    if not Path(path).exists():
        return None
    out = command("du", "-sb", path)
    return int(out.split()[0]) if out else None


def service_memory(name):
    raw = command("systemctl", "show", name, "-p", "MemoryCurrent", "--value")
    return int(raw) if raw.isdecimal() else None


def count_lines(*args):
    out = command(*args)
    return len(out.splitlines()) if out else 0


def sample(seconds, interval):
    before_total, before_idle = cpu_ticks()
    available = []
    deadlines = [time.monotonic() + seconds * i / interval for i in range(1, interval + 1)]
    for deadline in deadlines:
        time.sleep(max(0, deadline - time.monotonic()))
        available.append(meminfo()["MemAvailable"])
    after_total, after_idle = cpu_ticks()
    elapsed_ticks = after_total - before_total
    if elapsed_ticks <= 0:
        raise RuntimeError("CPU counters did not advance")
    return round(100 * (1 - (after_idle - before_idle) / elapsed_ticks), 2), int(statistics.median(available))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--label", choices=("docker-baseline", "engine-idle"), required=True)
    parser.add_argument("--seconds", type=int, default=120)
    parser.add_argument("--samples", type=int, default=10)
    args = parser.parse_args()
    if not 1 <= args.samples <= args.seconds <= 3600:
        parser.error("require 1 <= samples <= seconds <= 3600")
    if args.label == "docker-baseline" and command("k3s", "--version"):
        parser.error("Docker baseline must be collected before k3s is installed")
    if args.label == "engine-idle" and not command("k3s", "--version"):
        parser.error("engine-idle needs an installed k3s")

    initial = meminfo()
    root_before = os.statvfs("/")
    used_before = (root_before.f_blocks - root_before.f_bfree) * root_before.f_frsize
    busy, available = sample(args.seconds, args.samples)
    root_after = os.statvfs("/")
    used_after = (root_after.f_blocks - root_after.f_bfree) * root_after.f_frsize
    k3s_version = command("k3s", "--version").splitlines()
    result = {
        "label": args.label,
        "sampledAtUtc": datetime.now(timezone.utc).isoformat(),
        "sampleSeconds": args.seconds,
        "sampleCount": args.samples,
        "osVersion": next((line.split("=", 1)[1].strip('"') for line in Path("/etc/os-release").read_text().splitlines() if line.startswith("PRETTY_NAME=")), None),
        "kernelVersion": platform.release(),
        "cpuCount": os.cpu_count(),
        "hostCpuBusyPercent": busy,
        "memTotalBytes": initial["MemTotal"],
        "memAvailableMedianBytes": available,
        "memUsedMedianBytes": initial["MemTotal"] - available,
        "rootFsUsedStartBytes": used_before,
        "rootFsUsedEndBytes": used_after,
        "rootFsTotalBytes": root_after.f_blocks * root_after.f_frsize,
        "dockerDataBytes": tree_bytes("/var/lib/docker"),
        "k3sDataBytes": tree_bytes("/var/lib/rancher"),
        "longhornDataBytes": tree_bytes("/var/lib/longhorn"),
        "dockerServiceMemoryBytes": service_memory("docker"),
        "k3sServiceMemoryBytes": service_memory("k3s"),
        "dockerRunningContainers": count_lines("docker", "ps", "-q"),
        "k3sVersion": k3s_version[0] if k3s_version else None,
        "ansibleVersion": (command("ansible", "--version").splitlines() or [None])[0],
    }
    print(json.dumps(result, sort_keys=True, indent=2))


if __name__ == "__main__":
    main()

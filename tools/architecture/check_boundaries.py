#!/usr/bin/env python3
"""Check the new-architecture boundary contract (docs/architecture/README.md, #350).

Three checks, all offline and standard-library only:

1. Nothing under a new-code root (bootstrap/, platform/, catalog/,
   recovery/) names, imports or reaches into legacy code: Keystone,
   Wardstone, the old Console, the Statefile, the management chart, or
   legacy paths such as deploy/ansible. Markdown is exempt so a README may
   explain what it replaces.
2. go.work never lists a module under a new root, so new Go code builds in
   plain module mode (GOWORK=off) and cannot resolve a legacy module.
3. docs/architecture/legacy-inventory.md classifies every top-level entry of
   the repository, and every path it lists still exists. Adding a top-level
   directory, or deleting a listed path, therefore needs an inventory edit.

Usage: python3 tools/architecture/check_boundaries.py [repo-root]
Exit status: 0 clean, 1 violations found, 2 usage or configuration error.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

RULES_FILE = Path(__file__).with_name("boundaries.json")
SKIP_DIRS = {".git", "node_modules", "__pycache__", ".venv", "dist"}
# A row of the inventory table whose first cell is one backticked path.
INVENTORY_ROW = re.compile(r"^\|\s*`([^`]+)`\s*\|")


@dataclass(frozen=True)
class Violation:
    path: str
    line: int
    reason: str

    def __str__(self) -> str:
        where = f"{self.path}:{self.line}" if self.line else self.path
        return f"{where}: {self.reason}"


@dataclass(frozen=True)
class Rule:
    regex: re.Pattern[str]
    reason: str


def load_rules(path: Path = RULES_FILE) -> dict:
    with path.open(encoding="utf-8") as handle:
        rules = json.load(handle)
    for key in ("new_roots", "forbidden_terms", "forbidden_paths", "inventory"):
        if key not in rules:
            raise ValueError(f"{path}: missing {key!r}")
    return rules


def compile_rules(rules: dict) -> list[Rule]:
    compiled = [Rule(re.compile(r["pattern"], re.IGNORECASE), r["reason"]) for r in rules["forbidden_terms"]]
    compiled += [Rule(re.compile(r["pattern"]), r["reason"]) for r in rules["forbidden_paths"]]
    return compiled


def read_text(path: Path) -> str | None:
    """The file as text, or None for anything binary."""
    data = path.read_bytes()
    if b"\0" in data:
        return None
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError:
        return None


def iter_files(root: Path):
    for directory, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        for name in sorted(filenames):
            yield Path(directory) / name


def check_new_roots(repo: Path, rules: dict) -> list[Violation]:
    compiled = compile_rules(rules)
    exempt = tuple(rules.get("exempt_suffixes", []))
    found: list[Violation] = []
    for root_name in rules["new_roots"]:
        root = repo / root_name
        if not root.is_dir():
            continue
        for file in iter_files(root):
            relative = file.relative_to(repo).as_posix()
            # The file's own path counts too: platform/keystone-shim.yaml is
            # as much a dependency as a line that names Keystone.
            for rule in compiled:
                if rule.regex.search(relative.split("/", 1)[1]):
                    found.append(Violation(relative, 0, f"path: {rule.reason}"))
            if file.name.endswith(exempt):
                continue
            text = read_text(file)
            if text is None:
                continue
            for number, line in enumerate(text.splitlines(), start=1):
                for rule in compiled:
                    if rule.regex.search(line):
                        found.append(Violation(relative, number, rule.reason))
    return found


def check_go_work(repo: Path, rules: dict) -> list[Violation]:
    work = repo / "go.work"
    if not work.is_file():
        return []
    roots = tuple(rules["new_roots"])
    found = []
    for number, line in enumerate(work.read_text(encoding="utf-8").splitlines(), start=1):
        entry = line.split("//", 1)[0].strip().strip("()").strip()
        if entry.startswith("use "):
            entry = entry[4:].strip()
        entry = entry.removeprefix("./").rstrip("/")
        if entry and entry.split("/", 1)[0] in roots:
            found.append(Violation("go.work", number, f"{entry} must build with GOWORK=off; do not add it to the legacy workspace"))
    return found


def top_level_entries(repo: Path) -> set[str]:
    """Top-level names git tracks; every entry on disk when git is unavailable."""
    try:
        out = subprocess.run(
            ["git", "-C", str(repo), "ls-files", "-z"],
            check=True, capture_output=True,
        ).stdout.decode("utf-8")
        names = {p.split("/", 1)[0] for p in out.split("\0") if p}
        if names:
            return names
    except (OSError, subprocess.CalledProcessError):
        pass
    return {p.name for p in repo.iterdir() if p.name not in SKIP_DIRS}


def inventory_paths(text: str) -> list[tuple[int, str]]:
    rows = []
    for number, line in enumerate(text.splitlines(), start=1):
        match = INVENTORY_ROW.match(line)
        if match:
            rows.append((number, match.group(1).strip().rstrip("/")))
    return rows


def check_inventory(repo: Path, rules: dict, entries: set[str] | None = None) -> list[Violation]:
    inventory = rules["inventory"]
    path = repo / inventory
    if not path.is_file():
        return [Violation(inventory, 0, "the retain/adapt/retire inventory is missing")]
    rows = inventory_paths(path.read_text(encoding="utf-8"))
    listed = {p for _, p in rows}
    found = [
        Violation(inventory, number, f"`{p}` is listed but does not exist; update the inventory with the deletion")
        for number, p in rows
        if not (repo / p).exists()
    ]
    for entry in sorted(top_level_entries(repo) if entries is None else entries):
        if entry not in listed:
            found.append(Violation(inventory, 0, f"top-level `{entry}` has no retain/adapt/retire row"))
    return found


def main(argv: list[str]) -> int:
    if len(argv) > 2:
        print(__doc__.strip().splitlines()[-2], file=sys.stderr)
        return 2
    repo = Path(argv[1] if len(argv) == 2 else Path(__file__).resolve().parents[2])
    try:
        rules = load_rules()
    except (OSError, ValueError, json.JSONDecodeError, re.error) as error:
        print(f"check_boundaries: {error}", file=sys.stderr)
        return 2
    violations = check_new_roots(repo, rules) + check_go_work(repo, rules) + check_inventory(repo, rules)
    for violation in violations:
        print(violation)
    present = [r for r in rules["new_roots"] if (repo / r).is_dir()]
    print(
        f"check_boundaries: {len(violations)} violation(s); "
        f"new roots present: {', '.join(present) or 'none yet'}",
        file=sys.stderr,
    )
    return 1 if violations else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

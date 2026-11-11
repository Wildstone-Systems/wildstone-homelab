"""Tests for check_boundaries.py: each rule fires on a violation and stays quiet on clean code.

Run: python3 -m unittest discover -s tools/architecture -p 'test_*.py'
"""

from __future__ import annotations

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import check_boundaries as cb  # noqa: E402

REPO = Path(__file__).resolve().parents[2]
INVENTORY_HEADER = "| Path | Disposition |\n|---|---|\n"


class Fixture:
    """A throwaway repository tree with the real rules and a valid inventory."""

    def __init__(self, test: unittest.TestCase):
        self._dir = tempfile.TemporaryDirectory()
        test.addCleanup(self._dir.cleanup)
        self.root = Path(self._dir.name)
        self.rules = cb.load_rules()
        self.write("docs/architecture/legacy-inventory.md", INVENTORY_HEADER + "| `docs` | Retain |\n")

    def write(self, relative: str, text: str | bytes) -> Path:
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        if isinstance(text, bytes):
            path.write_bytes(text)
        else:
            path.write_text(text, encoding="utf-8")
        return path

    def roots(self) -> list[str]:
        return [str(v) for v in cb.check_new_roots(self.root, self.rules)]

    def go_work(self) -> list[str]:
        return [str(v) for v in cb.check_go_work(self.root, self.rules)]

    def inventory(self) -> list[str]:
        entries = {p.name for p in self.root.iterdir()}
        return [str(v) for v in cb.check_inventory(self.root, self.rules, entries)]


class NewRootTests(unittest.TestCase):
    def test_clean_tree_passes(self):
        f = Fixture(self)
        f.write("platform/k3s/config.yaml", "write-kubeconfig-mode: \"0600\"\n# Longhorn, Forgejo and Flux only.\n")
        f.write("bootstrap/create.sh", "#!/usr/bin/env bash\nset -euo pipefail\n. \"$(dirname \"$0\")/lib/preflight.sh\"\n")
        self.assertEqual(f.roots(), [])

    def test_absent_roots_are_not_an_error(self):
        self.assertEqual(Fixture(self).roots(), [])

    def test_legacy_component_names_are_refused(self):
        f = Fixture(self)
        f.write("bootstrap/a.sh", "systemctl start wardstoned\n")
        f.write("platform/b.yaml", "url: https://Keystone.wildstone-system.svc\n")
        f.write("catalog/c.json", '{"chart": "wildstone-management"}\n')
        f.write("recovery/d.sh", "wild-playbooks verify\ncp statefile.db /tmp\n")
        found = f.roots()
        self.assertTrue(any(v.startswith("bootstrap/a.sh:1:") for v in found), found)
        self.assertTrue(any(v.startswith("platform/b.yaml:1:") for v in found), found)
        self.assertTrue(any(v.startswith("catalog/c.json:1:") for v in found), found)
        self.assertEqual(sum(v.startswith("recovery/d.sh:") for v in found), 2, found)

    def test_legacy_go_modules_are_refused(self):
        f = Fixture(self)
        f.write("bootstrap/go.mod", "module example.invalid/bootstrap\n\nrequire github.com/Wildstone-Systems/wildstone-homelab/proto v0.0.0\n")
        f.write("bootstrap/main.go", 'package main\n\nimport _ "github.com/Wildstone-Systems/wildstone-homelab/cli/cmd/wild"\n')
        found = f.roots()
        self.assertTrue(any(v.startswith("bootstrap/go.mod:3:") for v in found), found)
        self.assertTrue(any(v.startswith("bootstrap/main.go:3:") for v in found), found)

    def test_legacy_paths_are_refused(self):
        f = Fixture(self)
        f.write("platform/longhorn/install.sh", "ansible-playbook ../../deploy/ansible/engine-storage.yml\n")
        f.write("bootstrap/vendor.sh", "cp ../console/dist/index.html .\n")
        f.write("recovery/drill.sh", "bash scripts/install.sh\n")
        found = f.roots()
        self.assertTrue(any(v.startswith("platform/longhorn/install.sh:1:") for v in found), found)
        self.assertTrue(any(v.startswith("bootstrap/vendor.sh:1:") for v in found), found)
        self.assertTrue(any(v.startswith("recovery/drill.sh:1:") for v in found), found)

    def test_file_names_count_as_references(self):
        f = Fixture(self)
        f.write("platform/keystone-shim.yaml", "kind: ConfigMap\n")
        self.assertTrue(any(v.startswith("platform/keystone-shim.yaml: path:") for v in f.roots()))

    def test_markdown_may_explain_what_it_replaces(self):
        f = Fixture(self)
        f.write("bootstrap/README.md", "Replaces the legacy Keystone installer.\n")
        self.assertEqual(f.roots(), [])

    def test_binary_files_are_skipped(self):
        f = Fixture(self)
        f.write("platform/blob.bin", b"\0keystone\0")
        self.assertEqual(f.roots(), [])

    def test_symlink_into_legacy_is_refused(self):
        f = Fixture(self)
        f.write("deploy/ansible/prereqs.yml", "- hosts: localhost\n")
        (f.root / "platform").mkdir()
        (f.root / "platform/k3s").symlink_to("../deploy/ansible")
        (f.root / "platform/prereqs.yml").symlink_to("../deploy/ansible/prereqs.yml")
        found = f.roots()
        self.assertTrue(any(v.startswith("platform/k3s: symlink") for v in found), found)
        self.assertTrue(any(v.startswith("platform/prereqs.yml: symlink") for v in found), found)

    def test_symlink_root_is_refused(self):
        f = Fixture(self)
        f.write("deploy/ansible/prereqs.yml", "- hosts: localhost\n")
        (f.root / "platform").symlink_to("deploy/ansible")
        self.assertTrue(any(v.startswith("platform: a new root") for v in f.roots()))

    def test_symlink_within_new_roots_is_allowed(self):
        f = Fixture(self)
        f.write("platform/k3s/config.yaml", "a: 1\n")
        (f.root / "bootstrap").mkdir()
        (f.root / "bootstrap/k3s").symlink_to("../platform/k3s")
        self.assertEqual(f.roots(), [])

    def test_similar_words_are_not_flagged(self):
        f = Fixture(self)
        f.write("platform/notes.yaml", "keystones: 2\nconsole: serial\nscripts/check.sh\n../scripts/x.sh\n")
        self.assertEqual(f.roots(), [])


class GoWorkTests(unittest.TestCase):
    def test_legacy_workspace_is_allowed(self):
        f = Fixture(self)
        f.write("go.work", "go 1.26.0\n\nuse (\n\t./cli\n\t./keystone\n)\n")
        self.assertEqual(f.go_work(), [])

    def test_new_module_in_workspace_is_refused(self):
        f = Fixture(self)
        f.write("go.work", "go 1.26.0\n\nuse (\n\t./keystone\n\t./bootstrap/cli // new\n)\nuse ./platform\n")
        found = f.go_work()
        self.assertEqual(len(found), 2, found)
        self.assertTrue(found[0].startswith("go.work:5:"), found)

    def test_quoted_and_inline_entries_are_refused(self):
        f = Fixture(self)
        f.write("go.work", 'go 1.26.0\n\nuse "./bootstrap/cli"\nuse `./catalog`\nuse ( ./keystone "recovery/drill" )\nuse ./platform/../platform/x\n')
        found = f.go_work()
        self.assertEqual([v.split(":")[1] for v in found], ["3", "4", "5", "6"], found)


class InventoryTests(unittest.TestCase):
    def test_every_entry_listed_passes(self):
        self.assertEqual(Fixture(self).inventory(), [])

    def test_unlisted_top_level_entry_fails(self):
        f = Fixture(self)
        f.write("newthing/x.txt", "x\n")
        self.assertIn("top-level `newthing`", " ".join(f.inventory()))

    def test_listed_path_that_was_deleted_fails(self):
        f = Fixture(self)
        f.write("docs/architecture/legacy-inventory.md", INVENTORY_HEADER + "| `docs` | Retain |\n| `docs/gone.md` | Retire |\n")
        self.assertIn("`docs/gone.md` is listed but does not exist", " ".join(f.inventory()))

    def test_missing_inventory_fails(self):
        f = Fixture(self)
        (f.root / "docs/architecture/legacy-inventory.md").unlink()
        self.assertIn("inventory is missing", " ".join(f.inventory()))


class RepositoryTests(unittest.TestCase):
    """The real repository satisfies its own contract."""

    def test_repository_is_clean(self):
        result = subprocess.run(
            [sys.executable, str(Path(cb.__file__)), str(REPO)],
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()

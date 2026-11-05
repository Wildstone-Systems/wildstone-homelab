"""Release boundary checks, without publishing or signing credentials."""
import importlib.util
import json
import os
import subprocess
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("release", Path(__file__).with_name("management-release.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
ROOT = Path(__file__).resolve().parent.parent


class ManifestTests(unittest.TestCase):
    def manifest(self, tag="v1.2.3"):
        return release.generate(ROOT, tag, "a" * 40, "sha256:" + "b" * 64)

    def test_identity_and_no_unproven_upgrade(self):
        m = self.manifest()
        self.assertEqual(m["channel"], "stable")
        self.assertFalse(m["upgradeEnabled"])
        self.assertEqual(m["compatibility"]["upgradeFrom"], [])
        self.assertTrue(m["database"]["migrationFiles"])
        self.assertIn("charts/wildstone-management/templates/deployment.yaml", m["chart"]["files"])
        self.assertEqual(m, self.manifest())

    def test_all_prereleases_are_preview(self):
        for suffix in ("alpha.1", "beta.2", "rc.1"):
            self.assertEqual(self.manifest("v1.2.3-" + suffix)["channel"], "preview")

    def test_bad_identity_refused(self):
        for tag in ("latest", "v01.2.3", "v1.2.3-01", "v1.2.3+other", "v1.2.3;id"):
            with self.assertRaises(ValueError):
                self.manifest(tag)
        with self.assertRaises(ValueError):
            release.generate(ROOT, "v1.2.3", "main", "sha256:" + "b" * 64)
        with self.assertRaises(ValueError):
            release.generate(ROOT, "v1.2.3", "a" * 40, "latest")

    def test_policy_cannot_enable_unproven_install(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "release").mkdir()
            policy = json.loads((ROOT / "release/management-policy.json").read_text())
            policy["upgradeFrom"] = ["v1.2.2"]
            (root / "release/management-policy.json").write_text(json.dumps(policy))
            with self.assertRaises(ValueError):
                release.generate(root, "v1.2.3", "a" * 40, "sha256:" + "b" * 64)


    def test_verifier_rejects_signing_failure_and_wrong_release(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = root / "manifest.json"
            manifest.write_text(json.dumps(self.manifest()))
            bundle = root / "bundle.json"
            bundle.write_text("{}")
            cosign = root / "cosign"
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"])
            command = [str(ROOT / "scripts/verify-management-release.sh"), "v1.2.3", str(manifest), str(bundle)]
            cosign.write_text("#!/bin/sh\nexit 1\n")
            cosign.chmod(0o755)
            self.assertNotEqual(subprocess.run(command, env=env, capture_output=True).returncode, 0)
            # Test wrapper policy only, not cryptography: a fake successful
            # Cosign still cannot make a different manifest tag acceptable.
            cosign.write_text("#!/bin/sh\nexit 0\n")
            self.assertEqual(subprocess.run(command, env=env, capture_output=True).returncode, 0)
            m = self.manifest()
            m["release"] = "v1.2.4"
            manifest.write_text(json.dumps(m))
            self.assertNotEqual(subprocess.run(command, env=env, capture_output=True).returncode, 0)


if __name__ == "__main__":
    unittest.main()

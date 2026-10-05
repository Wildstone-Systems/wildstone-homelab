"""Build-helper contract fixtures; never call Docker or a registry."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / 'scripts' / 'build-ddgs-worker.sh'
DIGEST = 'sha256:' + hashlib.sha256(b'release-fixture').hexdigest()


class ReleaseTests(unittest.TestCase):
    def run_helper(self, base=None, docker_exit=0, malformed=False, existing=False):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            # Only these fixtures replace git/Docker. Production uses the real
            # checkout cleanliness check and publisher's authenticated builder.
            (tmp / 'git').write_text('#!/bin/sh\ncase "$*" in *rev-parse*) echo fixture-revision;; *show*) echo 1;; esac\n')
            (tmp / 'docker').write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
assert '--push' in args
assert args[args.index('--platform') + 1] == 'linux/amd64,linux/arm64'
if int(os.environ['FIXTURE_EXIT']): sys.exit(int(os.environ['FIXTURE_EXIT']))
path = pathlib.Path(args[args.index('--metadata-file') + 1])
path.write_text(json.dumps({'containerimage.digest': os.environ['FIXTURE_DIGEST']}))
''')
            for name in ('git', 'docker'):
                (tmp / name).chmod(0o755)
            out = tmp / 'evidence'
            if existing:
                out.mkdir()
                (out / 'image.json').write_text('old evidence')
            env = dict(os.environ, PATH=str(tmp) + os.pathsep + os.environ['PATH'],
                       FIXTURE_EXIT=str(docker_exit), FIXTURE_DIGEST='invalid' if malformed else DIGEST)
            result = subprocess.run(['bash', str(SCRIPT), base or 'python:3.12.14-slim-bookworm@' + DIGEST,
                                     'registry.example.internal/ddgs:fixture', str(out)],
                                    env=env, capture_output=True, text=True)
            artifact = (out / 'image.json').read_text() if (out / 'image.json').exists() else None
            return result, artifact

    def test_requires_pinned_base(self):
        result, artifact = self.run_helper(base='python:3.12.14-slim-bookworm')
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(artifact)

    def test_publisher_failure_cannot_claim_image(self):
        result, artifact = self.run_helper(docker_exit=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(artifact)

    def test_invalid_metadata_cannot_claim_image(self):
        result, artifact = self.run_helper(malformed=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(artifact)

    def test_existing_evidence_is_not_overwritten(self):
        result, artifact = self.run_helper(existing=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(artifact, 'old evidence')

    def test_records_builder_digest_without_claiming_smoke(self):
        result, artifact = self.run_helper()
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads(artifact)
        self.assertEqual(evidence['image'], 'registry.example.internal/ddgs@' + DIGEST)
        self.assertEqual(evidence['smokeTest'], 'not run')
        self.assertEqual(evidence['liveConformance'], 'not run')
        self.assertEqual(evidence['sourceRevision'], 'fixture-revision')

#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import importlib.util
import os
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

root = Path(sys.argv[1])
sys.argv = [sys.argv[0]]
spec = importlib.util.spec_from_file_location("acceptance", root / "dev/release-acceptance.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class TestAcceptance(unittest.TestCase):
    def test_prepare_uses_plain_sources_and_retains_only_needed_artifacts(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            checkout = directory / "checkout"
            for name in ("dev/agent-e2e.sh", "scripts/lib/runtime.sh", "tests/helpers/source-files.sh"):
                target = checkout / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(root / name, target)
            (checkout / ".gitignore").write_text("/.build/\n/private/\n/.subyard-acceptance/\n")
            (checkout / "input.txt").write_text("current uncommitted input\n")
            (checkout / "private").mkdir()
            (checkout / "private/excluded.txt").write_text("excluded fixture\n")
            package = checkout / "dev/package-engine.sh"
            package.write_text('''#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --output-dir ] && [ "$3" = --version ]
printf 'packaged candidate\\n' > "$2/subyard-$4-linux-ARCH.tar.gz"
'''.replace("ARCH", {"x86_64": "amd64", "aarch64": "arm64"}[m.platform.machine()]))
            output = checkout / ".build/acceptance"
            inventory = {"sample": {"kind": "runner", "path": "sample-acceptance.sh"}}
            check_output = m.subprocess.check_output
            def tool_output(command, **kwargs):
                if command == ["go", "version"]:
                    return "go version fixture"
                return check_output(command, **kwargs)
            with patch.object(m, "ROOT", checkout), \
                 patch.object(m, "profile_inventory", return_value=inventory), \
                 patch.object(m, "external_inventory", return_value={}), \
                 patch.object(m.subprocess, "check_output", side_effect=tool_output):
                m.prepare(output, "test")
                source, receipt = m.load_bound(output)
                self.assertEqual(source, output / "source")
                self.assertEqual((source / "input.txt").read_text(), "current uncommitted input\n")
                self.assertEqual({path.name for path in output.iterdir()},
                                 {"source", "candidate-bundle.tar.gz", "receipt.json", receipt["checks"]["package"]["log"]})
                self.assertFalse(list(source.rglob(".git")))
                self.assertFalse((source / "private").exists())
                with tarfile.open(output / "candidate-bundle.tar.gz") as archive:
                    names = set(archive.getnames())
                    self.assertIn("input.txt", names)
                    self.assertIn(".subyard-acceptance/candidate.json", names)
                    self.assertIn(".subyard-acceptance/release/" + receipt["runtime_file"], names)
                    self.assertFalse(any(".git" in Path(name).parts for name in names))
                    self.assertNotIn(".subyard-e2e-index", names)
                (source / "input.txt").write_text("changed after preparation\n")
                with self.assertRaisesRegex(ValueError, "frozen source fingerprint mismatch"):
                    m.load_bound(output)
                (source / "input.txt").write_text("current uncommitted input\n")
                runtime = source / ".subyard-acceptance/release" / receipt["runtime_file"]
                runtime.write_text("changed runtime\n")
                with self.assertRaisesRegex(ValueError, "candidate release assets changed"):
                    m.load_bound(output)

    def test_complete_status_requires_every_obligation(self):
        for check, external, expected in (
            ("pending", "passed", "incomplete"),
            ("passed", "not-run", "incomplete"),
            ("failed", "passed", "failed"),
            ("passed", "passed", "passed"),
        ):
            receipt = {"checks": {"verify": {"status": check}},
                       "external_obligations": {"x": {"status": external}}}
            m.update_result(receipt)
            self.assertEqual(receipt["result"], expected)

    def test_transport_override_is_scoped_to_physical_controllers(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            root = output / "source"
            receipt = {"version": "test", "source_fingerprint": "a" * 64, "profiles": {},
                       "candidate_bundle_sha256": "b" * 64, "external_obligations": {},
                       "checks": {name: {"status": "pending"} for name in ("verify", "profile:sample")}}
            observed = {}
            def execute(name, command, root, output, environment, slot=None):
                observed[name] = dict(environment)
                return {"status": "passed", "exit_code": 0}
            with patch.object(m, "load_bound", return_value=(root, receipt)), \
                 patch.object(m, "commands", return_value={"verify": ["true"], "profile:sample": ["true"]}), \
                 patch.object(m, "source_fingerprint", return_value="a" * 64), \
                 patch.object(m, "execute", side_effect=execute), \
                 patch.dict(os.environ, {"SUBYARD_E2E_CANDIDATE_BUNDLE": "ambient",
                                         "SUBYARD_E2E_CONTROLLER_WORKSPACE": "ambient"}):
                self.assertEqual(m.run_checks(output, [1], [], False), 0)
            self.assertNotIn("SUBYARD_E2E_CANDIDATE_BUNDLE", observed["verify"])
            self.assertNotIn("SUBYARD_E2E_CONTROLLER_WORKSPACE", observed["verify"])
            self.assertEqual(observed["profile:sample"]["SUBYARD_E2E_CONTROLLER_WORKSPACE"], str(m.ROOT))
            self.assertEqual(observed["profile:sample"]["SUBYARD_E2E_CANDIDATE_BUNDLE"],
                             str(output / "candidate-bundle.tar.gz"))


unittest.main()
PY

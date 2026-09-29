#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PYTHONDONTWRITEBYTECODE=1 python3 -B - "$ROOT" <<'PY'
import importlib.util
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

root = Path(sys.argv[1])
sys.argv = [sys.argv[0]]
spec = importlib.util.spec_from_file_location("acceptance", root / "dev/release-acceptance.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class TestAcceptance(unittest.TestCase):
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

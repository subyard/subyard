#!/usr/bin/env python3
"""Focused collector controls, including detached orphan CPU and paired resampling."""
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace

SCRIPT = Path(__file__).resolve().parents[1] / "measure-release-owner.py"
spec = importlib.util.spec_from_file_location("owner_measure", SCRIPT)
measure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(measure)


def main():
    with tempfile.TemporaryDirectory(prefix="subyard-owner-measure-control-", dir="/tmp") as temporary:
        root = Path(temporary)
        control = root / "control.py"
        control.write_text('''import os, sys, time
mode = sys.argv[1]
child = os.fork()
if child == 0:
    os.setsid()
    if mode == "double-orphan":
        if os.fork(): os._exit(0)
    deadline = time.process_time() + 0.15
    while time.process_time() < deadline: pass
    os._exit(0)
if mode == "waited": os.waitpid(child, 0)
os._exit(0)
''')
        results = {}
        for mode in ("waited", "orphan", "double-orphan"):
            result = measure.command_measure([sys.executable, str(control), mode],
                {"PATH": "/usr/bin:/bin"}, root / mode, 0.002, timeout=5)
            assert result["exit"] == 0 and result["drained"] and result["cpu_complete"], result
            assert 0.14 <= result["cpu_seconds"] <= 0.40, result["cpu_seconds"]
            assert result["wall_seconds"] >= 0.14
            assert len(result["reaped"]) >= (1 if mode == "waited" else 2)
            assert not any(entry["alive_at_end"] for entry in result["process_history"])
            results[mode] = {key: result[key] for key in ("cpu_seconds", "wall_seconds", "reaped")}
        timeout = measure.command_measure([sys.executable, "-c", "import time;time.sleep(10)"],
            {"PATH": "/usr/bin:/bin"}, root / "timeout", 0.002, timeout=0.02)
        assert timeout["timed_out"] and timeout["drained"] and not timeout["cpu_complete"]
        assert timeout["exit"] == -9
        native = measure.command_measure(["/bin/sleep", "0.08"], {"PATH": "/usr/bin:/bin"},
                                         root / "native-small", 0.002, timeout=5)
        assert native["exec_confirmed"] and native["preexec_rss_excluded"] and native["exit"] == 0
        assert 0 < native["peak_sampled_tree_rss_bytes"] < 8 * 1024 * 1024, native["peak_sampled_tree_rss_bytes"]
        results["native-small"] = {"peak_sampled_tree_rss_bytes": native["peak_sampled_tree_rss_bytes"]}
        pairs = [{owner: {"wall_seconds": value, "cpu_seconds": value,
                           "peak_sampled_tree_rss_bytes": 10 * 1024 * 1024, "cpu_complete": True}
                  for owner, value in (("go", 1), ("rust", 1))} for _ in range(100)]
        report = measure.comparison(pairs, 1, replicates=100)
        assert report["metrics"]["cpu_seconds"]["status"] == "PASS"
        assert report["status"] == "INCONCLUSIVE"  # No physical I/O or complete helper RSS census.
        for pair in pairs:
            pair["rust"]["cpu_seconds"] = 1.2
        assert measure.comparison(pairs, 1, replicates=100)["status"] == "FAIL"
        assert measure.comparison(pairs[:1], 1, replicates=100)["status"] == "INCONCLUSIVE"
        fixture_root = root / "fixture"
        fixture_root.mkdir()
        data = fixture_root / "data"
        runtime = data / "runtime"
        runtime.mkdir(parents=True)
        original = fixture_root / "state"
        original.write_bytes(b"original")
        original.chmod(0o600)
        original_directory = fixture_root / "directory"
        original_directory.mkdir(mode=0o700)
        state = measure.FixtureReset(SimpleNamespace(root=fixture_root, runtime=runtime, data=data), root)
        pending = fixture_root / "pending"
        pending.write_bytes(b"changed")
        pending.replace(original)
        extra = fixture_root / "extra"
        extra.mkdir()
        (extra / "file").write_bytes(b"new")
        reset = state.restore()
        assert original.read_bytes() == b"original" and not extra.exists()
        assert "fixture/state" in reset["changed_file_identities"] and not reset["full_readset_identity_equal"]
        original_directory.rename(fixture_root / "moved-directory")
        original_directory.mkdir(mode=0o700)
        try:
            state.restore()
            raise AssertionError("changed root directory identity was accepted")
        except RuntimeError as error:
            assert "directory identity" in str(error)
        # A diagnostic exit0 is not readiness and must not trigger dependent
        # execution. Preserve the refusal before the disposable root disappears.
        declined = root / "declined"
        config = declined / "config"
        config.mkdir(parents=True)
        runtime = declined / "runtime"
        runtime.mkdir()
        retained = config / "settings.env"
        retained.write_text("# synthetic settings\n")
        project = declined / "project"
        project.write_text("project data\n")
        (declined / "incus-observation.json").write_text('{"migrationHistory":null}')
        fixture = SimpleNamespace(root=declined, config=config, runtime=runtime, env={},
            journal=config / "journal.json", retained=retained, retained_hash=measure.sha(retained), project_marker=project)
        verify = measure.module("refusal_fixture", measure.ROOT / "dev/verify-release-upgrades.py")
        calls = []
        saved = measure.command_measure
        def refusal(command, env, directory, interval):
            calls.append(command[0])
            directory.mkdir()
            (directory / "stdout").write_text('{"outcome":{"status":"operator-action-required"}}')
            return {"exit": 0}
        measure.command_measure = refusal
        output = root / "refusal-output"
        output.mkdir()
        try:
            measure.run_prepared(verify, fixture, [("inspect-recovery", ["inspect"]), ("resume", ["execute"])],
                                 "checkpoint-history-recovery", declined, output, 0.002, False)
            raise AssertionError("semantic refusal continued")
        except RuntimeError as error:
            assert "dependent commands stopped" in str(error)
        finally:
            measure.command_measure = saved
        assert calls == ["inspect"] and (output / "inspect-recovery/refused-config").is_dir()
        print(json.dumps({"status": "PASS", "controls": results}, sort_keys=True))


if __name__ == "__main__":
    main()
